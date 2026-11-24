use ahash::{AHashMap, AHashSet};
use crc32fast::Hasher as Crc32;
use dh_tms_core::MetaRow;
use dh_tms_grammar::{parse_release_name, tokenize_alnum, EpisodeSpec};
use parking_lot::RwLock;
use std::fs::{File, OpenOptions};
use std::io::{self, Read, Write};
use std::path::{Path, PathBuf};
use xxhash_rust::xxh3::xxh3_64_with_seed;

const L0_WAL_MAGIC: [u8; 4] = *b"L0WL";
const L0_EXT_V2_MAGIC: [u8; 4] = *b"L0V2";

#[derive(Debug, Clone)]
pub struct L0Doc {
    pub doc_id: u32,
    pub name: String,
    pub content_type: u8,
    pub resolution: u8,
    pub is_anime: bool,
    pub video_3d: u8,
    pub video_source: u8,
    pub video_codec: u8,
    pub entity_id: u32,
    pub year: u16,
    pub published_epoch: u32,
    pub created_epoch: u32,
    pub episodes: Vec<EpisodeSpec>,
    pub strict_title_hashes: Vec<u64>,
    pub family_hashes: Vec<u64>,
    pub terms: Vec<(u64, u16)>,
    pub info_hash_v1: Option<[u8; 20]>,
    pub info_hash_v2: Option<[u8; 32]>,
    pub size_bytes: u64,
    pub files_count: u32,
    pub files: Vec<(String, u64, String)>,
}

struct L0Inner {
    docs: AHashMap<u32, L0Doc>,
    /// Superseded or deleted base-segment doc_ids (masked out during query merge).
    superseded_base: AHashSet<u32>,
    inv: AHashMap<u64, Vec<(u32, u16)>>,
    entity_idx: AHashMap<u32, Vec<u32>>,
    strict_title_idx: AHashMap<u64, Vec<u32>>,
    family_idx: AHashMap<u64, Vec<u32>>,
    v1_to_doc: AHashMap<[u8; 20], u32>,
    v2_to_doc: AHashMap<[u8; 32], u32>,
    next_dynamic_doc_id: u32,
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
            family_idx: AHashMap::new(),
            v1_to_doc: AHashMap::new(),
            v2_to_doc: AHashMap::new(),
            next_dynamic_doc_id: 0,
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
                if doc.doc_id >= inner.next_dynamic_doc_id {
                    inner.next_dynamic_doc_id = doc.doc_id.saturating_add(1);
                }
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

    /// Idempotently resolve or allocate a dynamic `doc_id` in `[base_num_docs, max_doc_id)`
    /// for a newly discovered torrent hash.
    pub fn allocate_or_get_doc_id(
        &self,
        base_num_docs: u32,
        max_doc_id: u32,
        v1: Option<[u8; 20]>,
        v2: Option<[u8; 32]>,
    ) -> Result<u32, io::Error> {
        let mut g = self.inner.write();
        if let Some(h1) = v1 {
            if let Some(&existing) = g.v1_to_doc.get(&h1) {
                return Ok(existing);
            }
        }
        if let Some(h2) = v2 {
            if let Some(&existing) = g.v2_to_doc.get(&h2) {
                return Ok(existing);
            }
        }
        if g.next_dynamic_doc_id < base_num_docs {
            g.next_dynamic_doc_id = base_num_docs;
        }
        let assigned = g.next_dynamic_doc_id;
        if assigned >= max_doc_id {
            return Err(io::Error::new(
                io::ErrorKind::OutOfMemory,
                format!("dynamic L0 capacity exhausted (max_doc_id={max_doc_id})"),
            ));
        }
        g.next_dynamic_doc_id = assigned.saturating_add(1);
        if let Some(h1) = v1 {
            g.v1_to_doc.insert(h1, assigned);
        }
        if let Some(h2) = v2 {
            g.v2_to_doc.insert(h2, assigned);
        }
        Ok(assigned)
    }

    pub fn lookup_hash_v1(&self, h: &[u8; 20]) -> Option<u32> {
        self.inner.read().v1_to_doc.get(h).copied()
    }

    pub fn lookup_hash_v2(&self, h: &[u8; 32]) -> Option<u32> {
        self.inner.read().v2_to_doc.get(h).copied()
    }

    /// Durable WAL-before-mutate upsert / insert / reclassification for live writes.
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
            if doc.doc_id >= g.next_dynamic_doc_id {
                g.next_dynamic_doc_id = doc.doc_id.saturating_add(1);
            }
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

    pub fn lookup_family(&self, fam_hash: u64) -> Vec<u32> {
        let g = self.inner.read();
        g.family_idx.get(&fam_hash).cloned().unwrap_or_default()
    }

    /// Return `(doc_id, published_epoch)` for L0 docs matching an optional `content_type`,
    /// sorted newest-first (`published_epoch DESC, doc_id ASC`).
    pub fn recent_docs(&self, ct_filter: Option<u8>) -> Vec<(u32, u32)> {
        self.recent_docs_filtered(ct_filter, None)
    }

    /// Return `(doc_id, published_epoch)` for L0 docs matching optional `content_type`
    /// and an optional structured predicate, sorted newest-first.
    pub fn recent_docs_filtered(
        &self,
        ct_filter: Option<u8>,
        pred: Option<&dyn Fn(&L0Doc) -> bool>,
    ) -> Vec<(u32, u32)> {
        let g = self.inner.read();
        if g.docs.is_empty() {
            return Vec::new();
        }
        let mut out: Vec<(u32, u32)> = g
            .docs
            .values()
            .filter(|d| {
                ct_filter.is_none_or(|ct| d.content_type == ct) && pred.is_none_or(|p| p(d))
            })
            .map(|d| (d.doc_id, d.published_epoch))
            .collect();
        out.sort_unstable_by(|a, b| b.1.cmp(&a.1).then_with(|| a.0.cmp(&b.0)));
        out
    }

    /// Return L0 doc_ids matching `(season, episode)` and allowed content types.
    pub fn matching_episode_docs(&self, season: u16, episode: Option<u16>) -> Vec<u32> {
        let g = self.inner.read();
        if g.docs.is_empty() {
            return Vec::new();
        }
        g.docs
            .values()
            .filter(|d| doc_matches_episode(&d.episodes, season, episode))
            .map(|d| d.doc_id)
            .collect()
    }

    /// Return `(doc_id, content_type, entity_id)` for L0 docs with `created_epoch >= cutoff`.
    pub fn docs_since(&self, cutoff_epoch: u32) -> Vec<(u32, u8, u32)> {
        let g = self.inner.read();
        if g.docs.is_empty() {
            return Vec::new();
        }
        g.docs
            .values()
            .filter(|d| d.created_epoch >= cutoff_epoch)
            .map(|d| (d.doc_id, d.content_type, d.entity_id))
            .collect()
    }

    pub fn wal_path(&self) -> &Path {
        &self.wal_path
    }
}

pub fn doc_matches_episode(episodes: &[EpisodeSpec], season: u16, episode: Option<u16>) -> bool {
    match episode {
        Some(ep) => episodes.iter().any(|e| match *e {
            EpisodeSpec::Episode(s, e_num) => s == season && e_num == ep,
            EpisodeSpec::WholeSeason(s) => s == season,
        }),
        None => episodes.iter().any(|e| match *e {
            EpisodeSpec::Episode(s, _) | EpisodeSpec::WholeSeason(s) => s == season,
        }),
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
    // Remove old postings, entity_idx, strict_title_idx, and family_idx entries if re-updating in L0.
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
        for fh in &old.family_hashes {
            if let Some(list) = inner.family_idx.get_mut(fh) {
                list.retain(|&d| d != doc_id);
            }
        }
    }
    // Deduplicate terms per doc_id keeping max impact so AND-mode term counts are exact.
    let mut best_terms: AHashMap<u64, u16> = AHashMap::with_capacity(doc.terms.len());
    for &(th, imp) in &doc.terms {
        best_terms
            .entry(th)
            .and_modify(|v| *v = (*v).max(imp))
            .or_insert(imp);
    }
    let mut doc = doc;
    doc.terms = best_terms.into_iter().collect();
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
    for &fh in &doc.family_hashes {
        inner.family_idx.entry(fh).or_default().push(doc_id);
    }
    if let Some(h1) = doc.info_hash_v1 {
        inner.v1_to_doc.insert(h1, doc_id);
    }
    if let Some(h2) = doc.info_hash_v2 {
        inner.v2_to_doc.insert(h2, doc_id);
    }
    inner.docs.insert(doc_id, doc);
}

fn encode_l0_payload(doc: &L0Doc) -> Vec<u8> {
    let nb = doc.name.as_bytes();
    let ep_cnt = doc.episodes.len().min(u16::MAX as usize);
    let sth_cnt = doc.strict_title_hashes.len().min(u16::MAX as usize);
    let fam_cnt = doc.family_hashes.len().min(u16::MAX as usize);
    let file_cnt = doc.files.len().min(256);
    let mut out =
        Vec::with_capacity(64 + nb.len() + ep_cnt * 5 + sth_cnt * 8 + fam_cnt * 8 + file_cnt * 48);
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
    // Trailing v2 extension: hashes, size_bytes, files_count, capped file list, and packed meta flags
    out.extend_from_slice(&L0_EXT_V2_MAGIC);
    let mut flags = 4u8; // bit 2: trailing 2-byte packed MetaRow flags (source, codec, is_anime, video_3d)
    if doc.info_hash_v1.is_some() {
        flags |= 1;
    }
    if doc.info_hash_v2.is_some() {
        flags |= 2;
    }
    out.push(flags);
    if let Some(h1) = doc.info_hash_v1 {
        out.extend_from_slice(&h1);
    }
    if let Some(h2) = doc.info_hash_v2 {
        out.extend_from_slice(&h2);
    }
    out.extend_from_slice(&doc.size_bytes.to_le_bytes());
    out.extend_from_slice(&doc.files_count.to_le_bytes());
    out.extend_from_slice(&(file_cnt as u16).to_le_bytes());
    for (fp, sz, ext) in doc.files.iter().take(file_cnt) {
        let fpb = fp.as_bytes();
        let exb = ext.as_bytes();
        let fplen = fpb.len().min(u16::MAX as usize);
        let exlen = exb.len().min(u8::MAX as usize);
        out.extend_from_slice(&(fplen as u16).to_le_bytes());
        out.extend_from_slice(&fpb[..fplen]);
        out.extend_from_slice(&sz.to_le_bytes());
        out.push(exlen as u8);
        out.extend_from_slice(&exb[..exlen]);
    }
    let meta_bits = MetaRow::pack_flags_v2(
        doc.video_source,
        doc.video_codec,
        doc.is_anime,
        doc.video_3d,
        false,
        false,
        false,
        false,
    );
    out.extend_from_slice(&meta_bits.to_le_bytes());
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

    let mut info_hash_v1 = None;
    let mut info_hash_v2 = None;
    let mut size_bytes = 0u64;
    let mut files_count = 0u32;
    let mut files: Vec<(String, u64, String)> = Vec::new();
    let mut is_anime = false;
    let mut video_3d = 0u8;
    let mut video_source = 0u8;
    let mut video_codec = 0u8;

    if off + 5 <= buf.len() && buf[off..off + 4] == L0_EXT_V2_MAGIC {
        off += 4;
        let flags = buf[off];
        off += 1;
        if (flags & 1) != 0 {
            if off + 20 > buf.len() {
                return None;
            }
            info_hash_v1 = Some(buf[off..off + 20].try_into().ok()?);
            off += 20;
        }
        if (flags & 2) != 0 {
            if off + 32 > buf.len() {
                return None;
            }
            info_hash_v2 = Some(buf[off..off + 32].try_into().ok()?);
            off += 32;
        }
        if off + 14 > buf.len() {
            return None;
        }
        size_bytes = u64::from_le_bytes(buf[off..off + 8].try_into().ok()?);
        off += 8;
        files_count = u32::from_le_bytes(buf[off..off + 4].try_into().ok()?);
        off += 4;
        let fcnt = u16::from_le_bytes(buf[off..off + 2].try_into().ok()?) as usize;
        off += 2;
        files.reserve(fcnt);
        for _ in 0..fcnt {
            if off + 2 > buf.len() {
                return None;
            }
            let fplen = u16::from_le_bytes(buf[off..off + 2].try_into().ok()?) as usize;
            off += 2;
            if off + fplen + 9 > buf.len() {
                return None;
            }
            let fp = String::from_utf8(buf[off..off + fplen].to_vec()).ok()?;
            off += fplen;
            let sz = u64::from_le_bytes(buf[off..off + 8].try_into().ok()?);
            off += 8;
            let exlen = buf[off] as usize;
            off += 1;
            if off + exlen > buf.len() {
                return None;
            }
            let ext = String::from_utf8(buf[off..off + exlen].to_vec()).ok()?;
            off += exlen;
            files.push((fp, sz, ext));
        }
        if (flags & 4) != 0 {
            if off + 2 > buf.len() {
                return None;
            }
            let meta_bits = u16::from_le_bytes(buf[off..off + 2].try_into().ok()?);
            let tmp = MetaRow {
                flags_src: meta_bits,
                ..Default::default()
            };
            video_source = tmp.source();
            video_codec = tmp.codec();
            is_anime = tmp.is_anime();
            video_3d = tmp.video_3d();
        }
    }

    let mut terms = Vec::with_capacity(pr.title_tokens.len() + pr.raw_tokens.len());
    for t in &pr.title_tokens {
        terms.push((xxh3_64_with_seed(t.as_bytes(), term_seed), 1500u16));
    }
    for t in &pr.raw_tokens {
        terms.push((xxh3_64_with_seed(t.as_bytes(), term_seed), 600u16));
    }
    for (fp, _, _) in files.iter().take(64) {
        for t in tokenize_alnum(fp) {
            terms.push((xxh3_64_with_seed(t.as_bytes(), term_seed), 250u16));
        }
    }

    Some(L0Doc {
        doc_id,
        name,
        content_type,
        resolution,
        is_anime,
        video_3d,
        video_source,
        video_codec,
        entity_id,
        year,
        published_epoch,
        created_epoch,
        episodes,
        strict_title_hashes,
        family_hashes,
        terms,
        info_hash_v1,
        info_hash_v2,
        size_bytes,
        files_count,
        files,
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
        let h1 = [0xABu8; 20];

        {
            let l0 = L0Overlay::open(&wal, 7, seed).unwrap();
            l0.upsert_batch(
                &[L0Doc {
                    doc_id: 5,
                    name: "Suits.S01E02-E04.1080p.WEB-DL".into(),
                    content_type: 2,
                    resolution: 3,
                    is_anime: false,
                    video_3d: 0,
                    video_source: 0,
                    video_codec: 0,
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
                    info_hash_v1: None,
                    info_hash_v2: None,
                    size_bytes: 0,
                    files_count: 0,
                    files: vec![],
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
                    is_anime: false,
                    video_3d: 0,
                    video_source: 0,
                    video_codec: 0,
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
                    info_hash_v1: None,
                    info_hash_v2: None,
                    size_bytes: 0,
                    files_count: 0,
                    files: vec![],
                }],
                true,
            )
            .unwrap();
            assert!(l0.lookup_strict_title(old_sth).is_empty());
            assert_eq!(l0.lookup_strict_title(new_sth), vec![5]);

            // Dynamic new doc above base_num_docs=100 with hash + file metadata!
            let dyn_id = l0
                .allocate_or_get_doc_id(100, 1000, Some(h1), None)
                .unwrap();
            assert_eq!(dyn_id, 100);
            assert_eq!(
                l0.allocate_or_get_doc_id(100, 1000, Some(h1), None)
                    .unwrap(),
                100
            );
            l0.upsert_batch(
                &[L0Doc {
                    doc_id: dyn_id,
                    name: "Suits.S01E02-E04.1080p.WEB-DL".into(),
                    content_type: 2,
                    resolution: 3,
                    is_anime: false,
                    video_3d: 0,
                    video_source: 0,
                    video_codec: 0,
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
                    info_hash_v1: Some(h1),
                    info_hash_v2: None,
                    size_bytes: 4_200_000_000,
                    files_count: 1,
                    files: vec![("Suits.S01E02.mkv".into(), 4_200_000_000, "mkv".into())],
                }],
                true,
            )
            .unwrap();
        }

        // Reopen and verify structured fields + dynamic hash/files survived replay!
        {
            let l0 = L0Overlay::open(&wal, 7, seed).unwrap();
            assert_eq!(l0.len(), 2);
            assert_eq!(l0.lookup_hash_v1(&h1), Some(100));
            let d100 = l0.get_doc(100).unwrap();
            assert_eq!(
                d100.episodes.len(),
                3,
                "S01E02-E04 must survive L0 WAL replay"
            );
            assert_eq!(d100.size_bytes, 4_200_000_000);
            assert_eq!(d100.files.len(), 1);
            assert_eq!(d100.strict_title_hashes, pr1.strict_title_hashes);
            assert_eq!(d100.family_hashes, pr1.family_hashes);
            assert_eq!(l0.lookup_strict_title(new_sth), vec![5]);
            assert_eq!(l0.lookup_strict_title(old_sth), vec![100]);
            // Next allocation skips past replayed dynamic id 100 -> 101!
            let next_id = l0
                .allocate_or_get_doc_id(100, 1000, Some([0xCDu8; 20]), None)
                .unwrap();
            assert_eq!(next_id, 101);
        }

        // Corrupt a byte in frame 1 (mid-file) -> MUST fail closed!
        let mut raw = std::fs::read(&wal).unwrap();
        raw[32] ^= 0xAA;
        std::fs::write(&wal, &raw).unwrap();
        assert!(L0Overlay::open(&wal, 7, seed).is_err());

        let _ = std::fs::remove_dir_all(&dir);
    }
}
