use ahash::{AHashMap, AHashSet};
use crc32fast::Hasher as Crc32;
use dh_tms_grammar::{parse_release_name, EpisodeSpec};
use parking_lot::RwLock;
use std::fs::{File, OpenOptions};
use std::io::{self, Read, Write};
use std::path::{Path, PathBuf};
use xxhash_rust::xxh3::xxh3_64_with_seed;

const L0_WAL_MAGIC: [u8; 4] = *b"L0WL";

#[derive(Debug, Clone)]
pub struct L0Doc {
    pub doc_id: u32,
    pub name: String,
    pub content_type: u8,
    pub resolution: u8,
    pub entity_id: u32,
    pub year: u16,
    pub published_epoch: u32,
    pub created_epoch: u32,
    pub episodes: Vec<EpisodeSpec>,
    pub strict_title_hashes: Vec<u64>,
    pub family_hashes: Vec<u64>,
    pub terms: Vec<(u64, u16)>,
}

struct L0Inner {
    docs: AHashMap<u32, L0Doc>,
    /// Superseded or deleted base-segment doc_ids (masked out during query merge).
    superseded_base: AHashSet<u32>,
    inv: AHashMap<u64, Vec<(u32, u16)>>,
    entity_idx: AHashMap<u32, Vec<u32>>,
    strict_title_idx: AHashMap<u64, Vec<u32>>,
    wal_file: File,
    next_seq: u64,
}

/// Append-only mutable L0 delta segment for live inserts, reclassifications, relinks, and deletes,
/// backed by a CRC32-checksummed, generation-tagged WAL.
pub struct L0Overlay {
    generation_id: u64,
    _term_seed: u64,
    wal_path: PathBuf,
    inner: RwLock<L0Inner>,
}

impl L0Overlay {
    pub fn open(wal_path: &Path, generation_id: u64, term_seed: u64) -> io::Result<Self> {
        let mut inner = L0Inner {
            docs: AHashMap::new(),
            superseded_base: AHashSet::new(),
            inv: AHashMap::new(),
            entity_idx: AHashMap::new(),
            strict_title_idx: AHashMap::new(),
            wal_file: OpenOptions::new()
                .create(true)
                .append(true)
                .open(wal_path)?,
            next_seq: 1,
        };
        if wal_path.exists() {
            let mut raw = Vec::new();
            File::open(wal_path)?.read_to_end(&mut raw)?;
            let mut pos = 0usize;
            while pos < raw.len() {
                if pos + 28 > raw.len() {
                    // Genuine torn trailing header at EOF from crash -> truncate tail.
                    truncate_wal(wal_path, pos as u64)?;
                    break;
                }
                if raw[pos..pos + 4] != L0_WAL_MAGIC {
                    return Err(io::Error::new(
                        io::ErrorKind::InvalidData,
                        format!("bad L0 WAL magic at byte {pos}"),
                    ));
                }
                let gid = u64::from_le_bytes(raw[pos + 4..pos + 12].try_into().unwrap());
                if gid != generation_id {
                    return Err(io::Error::new(
                        io::ErrorKind::InvalidData,
                        format!("L0 WAL generation mismatch: expected {generation_id}, got {gid}"),
                    ));
                }
                let seq = u64::from_le_bytes(raw[pos + 12..pos + 20].try_into().unwrap());
                let plen = u32::from_le_bytes(raw[pos + 20..pos + 24].try_into().unwrap()) as usize;
                let stored_crc = u32::from_le_bytes(raw[pos + 24..pos + 28].try_into().unwrap());
                let Some(end) = pos.checked_add(28).and_then(|x| x.checked_add(plen)) else {
                    return Err(io::Error::new(
                        io::ErrorKind::InvalidData,
                        format!("L0 WAL frame offset overflow at byte {pos}"),
                    ));
                };
                if end > raw.len() {
                    // Genuine torn trailing payload at EOF from crash -> truncate tail.
                    truncate_wal(wal_path, pos as u64)?;
                    break;
                }
                let payload = &raw[pos + 28..end];
                let mut crc = Crc32::new();
                crc.update(&raw[pos + 4..pos + 24]);
                crc.update(payload);
                if crc.finalize() != stored_crc {
                    if end == raw.len() {
                        truncate_wal(wal_path, pos as u64)?;
                        break;
                    }
                    return Err(io::Error::new(
                        io::ErrorKind::InvalidData,
                        format!(
                            "mid-file L0 WAL CRC mismatch at byte {pos} (end={end} < len={})",
                            raw.len()
                        ),
                    ));
                }
                let doc = decode_l0_payload(payload, term_seed).ok_or_else(|| {
                    io::Error::new(
                        io::ErrorKind::InvalidData,
                        format!("corrupt L0 WAL payload at byte {pos}"),
                    )
                })?;
                apply_upsert_mem(&mut inner, doc);
                inner.next_seq = seq + 1;
                pos = end;
            }
            inner.wal_file = OpenOptions::new()
                .create(true)
                .append(true)
                .open(wal_path)?;
        }
        Ok(Self {
            generation_id,
            _term_seed: term_seed,
            wal_path: wal_path.to_path_buf(),
            inner: RwLock::new(inner),
        })
    }

    /// Durable WAL-before-mutate upsert / reclassification for live writes.
    pub fn upsert_batch(&self, docs: &[L0Doc], sync_wal: bool) -> io::Result<usize> {
        if docs.is_empty() {
            return Ok(0);
        }
        let mut g = self.inner.write();
        for doc in docs {
            let payload = encode_l0_payload(doc);
            let seq = g.next_seq;
            let mut crc = Crc32::new();
            crc.update(&self.generation_id.to_le_bytes());
            crc.update(&seq.to_le_bytes());
            crc.update(&(payload.len() as u32).to_le_bytes());
            crc.update(&payload);
            let checksum = crc.finalize();

            let mut frame = Vec::with_capacity(28 + payload.len());
            frame.extend_from_slice(&L0_WAL_MAGIC);
            frame.extend_from_slice(&self.generation_id.to_le_bytes());
            frame.extend_from_slice(&seq.to_le_bytes());
            frame.extend_from_slice(&(payload.len() as u32).to_le_bytes());
            frame.extend_from_slice(&checksum.to_le_bytes());
            frame.extend_from_slice(&payload);
            g.wal_file.write_all(&frame)?;
            g.next_seq += 1;
        }
        g.wal_file.flush()?;
        if sync_wal {
            g.wal_file.sync_data()?;
        }
        for doc in docs {
            apply_upsert_mem(&mut g, doc.clone());
        }
        Ok(docs.len())
    }

    pub fn is_superseded(&self, doc_id: u32) -> bool {
        let g = self.inner.read();
        !g.superseded_base.is_empty() && g.superseded_base.contains(&doc_id)
    }

    pub fn has_entries(&self) -> bool {
        !self.inner.read().docs.is_empty()
    }

    pub fn len(&self) -> usize {
        self.inner.read().docs.len()
    }

    pub fn is_empty(&self) -> bool {
        self.inner.read().docs.is_empty()
    }

    pub fn get_doc(&self, doc_id: u32) -> Option<L0Doc> {
        self.inner.read().docs.get(&doc_id).cloned()
    }

    pub fn search_terms(&self, term_hashes: &[u64], and_mode: bool) -> Vec<(u32, f32)> {
        let g = self.inner.read();
        if g.docs.is_empty() || term_hashes.is_empty() {
            return Vec::new();
        }
        let mut acc: AHashMap<u32, (f32, usize)> = AHashMap::new();
        for &th in term_hashes {
            if let Some(list) = g.inv.get(&th) {
                for &(doc_id, imp) in list {
                    let e = acc.entry(doc_id).or_insert((0.0, 0));
                    e.0 += imp as f32;
                    e.1 += 1;
                }
            } else if and_mode {
                return Vec::new();
            }
        }
        let need = term_hashes.len();
        acc.into_iter()
            .filter(|&(_, (_, cnt))| !and_mode || cnt == need)
            .map(|(d, (sc, _))| (d, sc))
            .collect()
    }

    pub fn lookup_entity(&self, entity_id: u32) -> Vec<u32> {
        let g = self.inner.read();
        g.entity_idx.get(&entity_id).cloned().unwrap_or_default()
    }

    pub fn lookup_strict_title(&self, strict_hash: u64) -> Vec<u32> {
        let g = self.inner.read();
        g.strict_title_idx
            .get(&strict_hash)
            .cloned()
            .unwrap_or_default()
    }

    pub fn wal_path(&self) -> &Path {
        &self.wal_path
    }
}

fn truncate_wal(wal_path: &Path, valid_len: u64) -> io::Result<()> {
    let f = OpenOptions::new().write(true).open(wal_path)?;
    f.set_len(valid_len)?;
    f.sync_data()
}

fn apply_upsert_mem(inner: &mut L0Inner, doc: L0Doc) {
    let doc_id = doc.doc_id;
    inner.superseded_base.insert(doc_id);
    // Remove old postings, entity_idx, and strict_title_idx entries if re-updating in L0.
    if let Some(old) = inner.docs.remove(&doc_id) {
        for (th, _) in &old.terms {
            if let Some(list) = inner.inv.get_mut(th) {
                list.retain(|&(d, _)| d != doc_id);
            }
        }
        if old.entity_id != 0 {
            if let Some(list) = inner.entity_idx.get_mut(&old.entity_id) {
                list.retain(|&d| d != doc_id);
            }
        }
        for sth in &old.strict_title_hashes {
            if let Some(list) = inner.strict_title_idx.get_mut(sth) {
                list.retain(|&d| d != doc_id);
            }
        }
    }
    for &(th, imp) in &doc.terms {
        inner.inv.entry(th).or_default().push((doc_id, imp));
    }
    if doc.entity_id != 0 {
        inner
            .entity_idx
            .entry(doc.entity_id)
            .or_default()
            .push(doc_id);
    }
    for &sth in &doc.strict_title_hashes {
        inner.strict_title_idx.entry(sth).or_default().push(doc_id);
    }
    inner.docs.insert(doc_id, doc);
}

fn encode_l0_payload(doc: &L0Doc) -> Vec<u8> {
    let nb = doc.name.as_bytes();
    let ep_cnt = doc.episodes.len().min(u16::MAX as usize);
    let sth_cnt = doc.strict_title_hashes.len().min(u16::MAX as usize);
    let fam_cnt = doc.family_hashes.len().min(u16::MAX as usize);
    let mut out = Vec::with_capacity(30 + nb.len() + ep_cnt * 5 + sth_cnt * 8 + fam_cnt * 8);
    out.extend_from_slice(&doc.doc_id.to_le_bytes());
    out.push(doc.content_type);
    out.push(doc.resolution);
    out.extend_from_slice(&doc.year.to_le_bytes());
    out.extend_from_slice(&doc.entity_id.to_le_bytes());
    out.extend_from_slice(&doc.published_epoch.to_le_bytes());
    out.extend_from_slice(&doc.created_epoch.to_le_bytes());
    out.extend_from_slice(&(nb.len() as u32).to_le_bytes());
    out.extend_from_slice(nb);
    out.extend_from_slice(&(ep_cnt as u16).to_le_bytes());
    for ep in doc.episodes.iter().take(ep_cnt) {
        match *ep {
            EpisodeSpec::WholeSeason(season) => {
                out.extend_from_slice(&season.to_le_bytes());
                out.extend_from_slice(&0u16.to_le_bytes());
                out.push(1u8);
            }
            EpisodeSpec::Episode(season, episode) => {
                out.extend_from_slice(&season.to_le_bytes());
                out.extend_from_slice(&episode.to_le_bytes());
                out.push(0u8);
            }
        }
    }
    out.extend_from_slice(&(sth_cnt as u16).to_le_bytes());
    for &h in doc.strict_title_hashes.iter().take(sth_cnt) {
        out.extend_from_slice(&h.to_le_bytes());
    }
    out.extend_from_slice(&(fam_cnt as u16).to_le_bytes());
    for &h in doc.family_hashes.iter().take(fam_cnt) {
        out.extend_from_slice(&h.to_le_bytes());
    }
    out
}

fn decode_l0_payload(buf: &[u8], term_seed: u64) -> Option<L0Doc> {
    if buf.len() < 24 {
        return None;
    }
    let doc_id = u32::from_le_bytes(buf[0..4].try_into().ok()?);
    let content_type = buf[4];
    let resolution = buf[5];
    let year = u16::from_le_bytes(buf[6..8].try_into().ok()?);
    let entity_id = u32::from_le_bytes(buf[8..12].try_into().ok()?);
    let published_epoch = u32::from_le_bytes(buf[12..16].try_into().ok()?);
    let created_epoch = u32::from_le_bytes(buf[16..20].try_into().ok()?);
    let nlen = u32::from_le_bytes(buf[20..24].try_into().ok()?) as usize;
    let mut off = 24usize.checked_add(nlen)?;
    if off > buf.len() {
        return None;
    }
    let name = String::from_utf8(buf[24..off].to_vec()).ok()?;
    let pr = parse_release_name(&name);

    let (episodes, strict_title_hashes, family_hashes) = if off == buf.len() {
        // Legacy v0 payload without trailing structured fields: reconstruct from parser.
        (pr.episodes, pr.strict_title_hashes, pr.family_hashes)
    } else {
        if off + 2 > buf.len() {
            return None;
        }
        let ep_cnt = u16::from_le_bytes(buf[off..off + 2].try_into().ok()?) as usize;
        off += 2;
        let ep_bytes = ep_cnt.checked_mul(5)?;
        if off.checked_add(ep_bytes)? > buf.len() {
            return None;
        }
        let mut episodes = Vec::with_capacity(ep_cnt);
        for _ in 0..ep_cnt {
            let season = u16::from_le_bytes(buf[off..off + 2].try_into().ok()?);
            let episode = u16::from_le_bytes(buf[off + 2..off + 4].try_into().ok()?);
            let is_whole_season = buf[off + 4] != 0;
            off += 5;
            if is_whole_season {
                episodes.push(EpisodeSpec::WholeSeason(season));
            } else {
                episodes.push(EpisodeSpec::Episode(season, episode));
            }
        }

        if off + 2 > buf.len() {
            return None;
        }
        let sth_cnt = u16::from_le_bytes(buf[off..off + 2].try_into().ok()?) as usize;
        off += 2;
        let sth_bytes = sth_cnt.checked_mul(8)?;
        if off.checked_add(sth_bytes)? > buf.len() {
            return None;
        }
        let mut strict_title_hashes = Vec::with_capacity(sth_cnt);
        for _ in 0..sth_cnt {
            strict_title_hashes.push(u64::from_le_bytes(buf[off..off + 8].try_into().ok()?));
            off += 8;
        }

        if off + 2 > buf.len() {
            return None;
        }
        let fam_cnt = u16::from_le_bytes(buf[off..off + 2].try_into().ok()?) as usize;
        off += 2;
        let fam_bytes = fam_cnt.checked_mul(8)?;
        if off.checked_add(fam_bytes)? > buf.len() {
            return None;
        }
        let mut family_hashes = Vec::with_capacity(fam_cnt);
        for _ in 0..fam_cnt {
            family_hashes.push(u64::from_le_bytes(buf[off..off + 8].try_into().ok()?));
            off += 8;
        }
        (episodes, strict_title_hashes, family_hashes)
    };

    let mut terms = Vec::with_capacity(pr.title_tokens.len() + pr.raw_tokens.len());
    for t in &pr.title_tokens {
        terms.push((xxh3_64_with_seed(t.as_bytes(), term_seed), 1500u16));
    }
    for t in &pr.raw_tokens {
        terms.push((xxh3_64_with_seed(t.as_bytes(), term_seed), 600u16));
    }

    Some(L0Doc {
        doc_id,
        name,
        content_type,
        resolution,
        entity_id,
        year,
        published_epoch,
        created_epoch,
        episodes,
        strict_title_hashes,
        family_hashes,
        terms,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_l0_wal_roundtrip_strict_title_cleanup_and_midfile_corruption() {
        let dir = std::env::temp_dir().join(format!("dhtms_l0_test_{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let wal = dir.join("l0.wal");
        let seed = 0x1234_5678u64;

        let pr1 = parse_release_name("Suits.S01E02-E04.1080p.WEB-DL");
        let pr2 = parse_release_name("Amelie.2001.1080p.BluRay");
        let old_sth = pr1.strict_title_hashes[0];
        let new_sth = pr2.strict_title_hashes[0];
        assert_ne!(old_sth, new_sth);

        {
            let l0 = L0Overlay::open(&wal, 7, seed).unwrap();
            l0.upsert_batch(
                &[L0Doc {
                    doc_id: 5,
                    name: "Suits.S01E02-E04.1080p.WEB-DL".into(),
                    content_type: 2,
                    resolution: 3,
                    entity_id: 201,
                    year: 2011,
                    published_epoch: 100,
                    created_epoch: 100,
                    episodes: pr1.episodes.clone(),
                    strict_title_hashes: pr1.strict_title_hashes.clone(),
                    family_hashes: pr1.family_hashes.clone(),
                    terms: pr1
                        .title_tokens
                        .iter()
                        .map(|t| (xxh3_64_with_seed(t.as_bytes(), seed), 1500))
                        .collect(),
                }],
                true,
            )
            .unwrap();
            assert_eq!(l0.lookup_strict_title(old_sth), vec![5]);

            // Re-upsert doc 5 with a different title -> old strict_title_idx entry MUST be cleaned!
            l0.upsert_batch(
                &[L0Doc {
                    doc_id: 5,
                    name: "Amelie.2001.1080p.BluRay".into(),
                    content_type: 1,
                    resolution: 3,
                    entity_id: 102,
                    year: 2001,
                    published_epoch: 101,
                    created_epoch: 101,
                    episodes: vec![],
                    strict_title_hashes: pr2.strict_title_hashes.clone(),
                    family_hashes: pr2.family_hashes.clone(),
                    terms: pr2
                        .title_tokens
                        .iter()
                        .map(|t| (xxh3_64_with_seed(t.as_bytes(), seed), 1500))
                        .collect(),
                }],
                true,
            )
            .unwrap();
            assert!(l0.lookup_strict_title(old_sth).is_empty());
            assert_eq!(l0.lookup_strict_title(new_sth), vec![5]);

            // Second doc with episodes to verify WAL replay preserves episodes!
            l0.upsert_batch(
                &[L0Doc {
                    doc_id: 6,
                    name: "Suits.S01E02-E04.1080p.WEB-DL".into(),
                    content_type: 2,
                    resolution: 3,
                    entity_id: 201,
                    year: 2011,
                    published_epoch: 102,
                    created_epoch: 102,
                    episodes: pr1.episodes.clone(),
                    strict_title_hashes: pr1.strict_title_hashes.clone(),
                    family_hashes: pr1.family_hashes.clone(),
                    terms: pr1
                        .title_tokens
                        .iter()
                        .map(|t| (xxh3_64_with_seed(t.as_bytes(), seed), 1500))
                        .collect(),
                }],
                true,
            )
            .unwrap();
        }

        // Reopen and verify structured fields survived replay!
        {
            let l0 = L0Overlay::open(&wal, 7, seed).unwrap();
            assert_eq!(l0.len(), 2);
            let d6 = l0.get_doc(6).unwrap();
            assert_eq!(
                d6.episodes.len(),
                3,
                "S01E02-E04 must survive L0 WAL replay"
            );
            assert_eq!(d6.strict_title_hashes, pr1.strict_title_hashes);
            assert_eq!(d6.family_hashes, pr1.family_hashes);
            assert_eq!(l0.lookup_strict_title(new_sth), vec![5]);
            assert_eq!(l0.lookup_strict_title(old_sth), vec![6]);
        }

        // Corrupt a byte in frame 1 (mid-file) -> MUST fail closed!
        let mut raw = std::fs::read(&wal).unwrap();
        raw[32] ^= 0xAA;
        std::fs::write(&wal, &raw).unwrap();
        assert!(L0Overlay::open(&wal, 7, seed).is_err());

        let _ = std::fs::remove_dir_all(&dir);
    }
}
