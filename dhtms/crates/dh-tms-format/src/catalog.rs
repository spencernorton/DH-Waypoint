use crate::{
    read_varint_u32, write_varint_u32, CheckedFileWriter, CheckedMmap, FileKind, FormatError, Pod,
};
use std::path::Path;
use xxhash_rust::xxh3::xxh3_64_with_seed;

const CATALOG_KEY_SEED: u64 = 0x517C_C1B7_2722_0A95;

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ContentRecord {
    pub entity_id: u32,
    pub content_type: u8,
    pub year: u16,
    pub adult: bool,
    pub source: String,
    pub id: String,
    pub title: String,
    pub original_title: String,
    pub strict_title_hashes: Vec<u64>,
    pub family_hashes: Vec<u64>,
    pub alt_identifiers: Vec<(String, String)>, // e.g., ("imdb", "tt0944947"), ("tvdb", "121361")
}

#[repr(C)]
#[derive(Debug, Clone, Copy, Default)]
struct LookupEntry {
    key_hash: u64,
    entity_id: u32,
    _pad: u32,
}

unsafe impl Pod for LookupEntry {}

#[inline]
pub fn catalog_key_hash(ct: u8, source: &str, id: &str) -> u64 {
    let mut buf = Vec::with_capacity(source.len() + id.len() + 3);
    buf.push(ct);
    buf.push(0);
    buf.extend_from_slice(source.as_bytes());
    buf.push(0);
    buf.extend_from_slice(id.as_bytes());
    xxh3_64_with_seed(&buf, CATALOG_KEY_SEED)
}

#[inline]
pub fn alt_id_hash(ct: u8, attr_source: &str, attr_val: &str) -> u64 {
    let mut buf = Vec::with_capacity(attr_source.len() + attr_val.len() + 4);
    buf.push(0xFF);
    buf.push(ct);
    buf.extend_from_slice(attr_source.as_bytes());
    buf.push(0);
    buf.extend_from_slice(attr_val.as_bytes());
    xxh3_64_with_seed(&buf, CATALOG_KEY_SEED)
}

/// Zero-JSON binary mmap content catalog (<2 ms startup vs ~2.05 s JSON parse in v0.2).
/// Supports O(1) lookup by `entity_id` and O(log N) lookup by exact `(ct, source, id)` or external
/// identifier `(ct, "imdb"|"tvdb", value)` with full collision verification.
pub struct MmapContentCatalog {
    cm: CheckedMmap,
    num_entities: usize,
    num_lookups: usize,
    lookup_start: usize,
    offsets_start: usize,
    data_start: usize,
}

impl MmapContentCatalog {
    pub fn build_and_save(
        path: &Path,
        generation_id: u64,
        records: &[ContentRecord],
    ) -> Result<u64, FormatError> {
        let max_eid = records.iter().map(|r| r.entity_id).max().unwrap_or(0) as usize;
        let num_slots = if records.is_empty() { 0 } else { max_eid + 1 };
        let mut by_slot: Vec<Option<&ContentRecord>> = vec![None; num_slots];
        for rec in records {
            by_slot[rec.entity_id as usize] = Some(rec);
        }

        let mut lookups: Vec<LookupEntry> = Vec::with_capacity(records.len() * 2);
        let mut offsets: Vec<u64> = Vec::with_capacity(num_slots + 1);
        let mut data: Vec<u8> = Vec::with_capacity(records.len() * 96);

        for (idx, slot) in by_slot.into_iter().enumerate() {
            let eid = idx as u32;
            offsets.push(data.len() as u64);
            if let Some(rec) = slot {
                if eid > 0 {
                    lookups.push(LookupEntry {
                        key_hash: catalog_key_hash(rec.content_type, &rec.source, &rec.id),
                        entity_id: eid,
                        _pad: 0,
                    });
                    for (asrc, aval) in &rec.alt_identifiers {
                        lookups.push(LookupEntry {
                            key_hash: alt_id_hash(rec.content_type, asrc, aval),
                            entity_id: eid,
                            _pad: 0,
                        });
                    }
                }
                encode_record(rec, &mut data);
            }
        }
        offsets.push(data.len() as u64);
        lookups.sort_unstable_by_key(|e| (e.key_hash, e.entity_id));
        lookups.dedup_by_key(|e| (e.key_hash, e.entity_id));

        let mut w = CheckedFileWriter::create(path, FileKind::Catalog, generation_id)?;
        let l_bytes = unsafe {
            std::slice::from_raw_parts(
                lookups.as_ptr() as *const u8,
                std::mem::size_of_val(&lookups[..]),
            )
        };
        let o_bytes = unsafe {
            std::slice::from_raw_parts(
                offsets.as_ptr() as *const u8,
                std::mem::size_of_val(&offsets[..]),
            )
        };
        w.write_payload(l_bytes)?;
        w.write_payload(o_bytes)?;
        w.write_payload(&data)?;
        w.finish(num_slots as u64, lookups.len() as u64, 0)
    }

    pub fn open(path: &Path, expected_gen: Option<u64>) -> Result<Self, FormatError> {
        let cm = CheckedMmap::open(path, FileKind::Catalog, expected_gen, false)?;
        let num_entities = cm.header().record_count as usize;
        let num_lookups = cm.header().aux_u64 as usize;
        let lookup_bytes = num_lookups
            .checked_mul(std::mem::size_of::<LookupEntry>())
            .ok_or_else(|| FormatError::CorruptPayload("catalog lookup size overflow".into()))?;
        let offset_bytes = num_entities
            .checked_add(1)
            .and_then(|n| n.checked_mul(8))
            .ok_or_else(|| FormatError::CorruptPayload("catalog offset size overflow".into()))?;
        let data_start = lookup_bytes
            .checked_add(offset_bytes)
            .ok_or_else(|| FormatError::CorruptPayload("catalog header overflow".into()))?;
        if (data_start as u64) > cm.header().payload_len {
            return Err(FormatError::CorruptPayload(
                "catalog truncated before data section".into(),
            ));
        }
        Ok(Self {
            cm,
            num_entities,
            num_lookups,
            lookup_start: 0,
            offsets_start: lookup_bytes,
            data_start,
        })
    }

    #[inline]
    pub fn len(&self) -> usize {
        self.num_entities
    }

    #[inline]
    pub fn is_empty(&self) -> bool {
        self.num_entities == 0
    }

    fn lookups(&self) -> &[LookupEntry] {
        if self.num_lookups == 0 {
            return &[];
        }
        unsafe {
            std::slice::from_raw_parts(
                self.cm.payload()[self.lookup_start..].as_ptr() as *const LookupEntry,
                self.num_lookups,
            )
        }
    }

    fn offsets(&self) -> &[u64] {
        unsafe {
            std::slice::from_raw_parts(
                self.cm.payload()[self.offsets_start..].as_ptr() as *const u64,
                self.num_entities + 1,
            )
        }
    }

    pub fn get_by_entity_id(&self, entity_id: u32) -> Result<Option<ContentRecord>, FormatError> {
        let idx = entity_id as usize;
        if idx >= self.num_entities {
            return Ok(None);
        }
        let offs = self.offsets();
        let start = offs[idx] as usize;
        let end = offs[idx + 1] as usize;
        let data = &self.cm.payload()[self.data_start..];
        if start > end || end > data.len() {
            return Err(FormatError::CorruptPayload(
                "catalog record offset out of bounds".into(),
            ));
        }
        if start == end {
            return Ok(None);
        }
        decode_record(entity_id, &data[start..end]).map(Some)
    }

    pub fn find_by_key(
        &self,
        ct: u8,
        source: &str,
        id: &str,
    ) -> Result<Option<ContentRecord>, FormatError> {
        let kh = catalog_key_hash(ct, source, id);
        let ls = self.lookups();
        let mut pos = ls.partition_point(|e| e.key_hash < kh);
        while pos < ls.len() && ls[pos].key_hash == kh {
            if let Some(rec) = self.get_by_entity_id(ls[pos].entity_id)? {
                if rec.content_type == ct && rec.source == source && rec.id == id {
                    return Ok(Some(rec));
                }
            }
            pos += 1;
        }
        Ok(None)
    }

    pub fn find_by_alt_id(
        &self,
        ct: u8,
        attr_source: &str,
        attr_val: &str,
    ) -> Result<Vec<ContentRecord>, FormatError> {
        let kh = alt_id_hash(ct, attr_source, attr_val);
        let ls = self.lookups();
        let mut pos = ls.partition_point(|e| e.key_hash < kh);
        let mut out = Vec::new();
        while pos < ls.len() && ls[pos].key_hash == kh {
            if let Some(rec) = self.get_by_entity_id(ls[pos].entity_id)? {
                if rec.content_type == ct
                    && rec
                        .alt_identifiers
                        .iter()
                        .any(|(s, v)| s == attr_source && v == attr_val)
                {
                    out.push(rec);
                }
            }
            pos += 1;
        }
        Ok(out)
    }
}

fn write_str(out: &mut Vec<u8>, s: &str) {
    write_varint_u32(out, s.len() as u32);
    out.extend_from_slice(s.as_bytes());
}

fn read_str(buf: &[u8], pos: &mut usize) -> Result<String, FormatError> {
    let len = read_varint_u32(buf, pos)? as usize;
    let end = pos
        .checked_add(len)
        .ok_or_else(|| FormatError::CorruptPayload("string length overflow".into()))?;
    if end > buf.len() {
        return Err(FormatError::CorruptPayload("string truncated".into()));
    }
    let s = String::from_utf8(buf[*pos..end].to_vec())
        .map_err(|_| FormatError::CorruptPayload("invalid UTF-8 in catalog string".into()))?;
    *pos = end;
    Ok(s)
}

fn encode_record(rec: &ContentRecord, out: &mut Vec<u8>) {
    out.push(rec.content_type);
    out.push(if rec.adult { 1 } else { 0 });
    out.extend_from_slice(&rec.year.to_le_bytes());
    write_str(out, &rec.source);
    write_str(out, &rec.id);
    write_str(out, &rec.title);
    write_str(out, &rec.original_title);
    write_varint_u32(out, rec.strict_title_hashes.len() as u32);
    for &h in &rec.strict_title_hashes {
        out.extend_from_slice(&h.to_le_bytes());
    }
    write_varint_u32(out, rec.family_hashes.len() as u32);
    for &h in &rec.family_hashes {
        out.extend_from_slice(&h.to_le_bytes());
    }
    write_varint_u32(out, rec.alt_identifiers.len() as u32);
    for (s, v) in &rec.alt_identifiers {
        write_str(out, s);
        write_str(out, v);
    }
}

fn decode_record(entity_id: u32, buf: &[u8]) -> Result<ContentRecord, FormatError> {
    if buf.len() < 4 {
        return Err(FormatError::CorruptPayload(
            "catalog record too short".into(),
        ));
    }
    let content_type = buf[0];
    let adult = buf[1] != 0;
    let year = u16::from_le_bytes([buf[2], buf[3]]);
    let mut pos = 4usize;
    let source = read_str(buf, &mut pos)?;
    let id = read_str(buf, &mut pos)?;
    let title = read_str(buf, &mut pos)?;
    let original_title = read_str(buf, &mut pos)?;

    let n_sths = read_varint_u32(buf, &mut pos)? as usize;
    let mut strict_title_hashes = Vec::with_capacity(n_sths.min(64));
    for _ in 0..n_sths {
        if pos + 8 > buf.len() {
            return Err(FormatError::CorruptPayload("catalog sth truncated".into()));
        }
        strict_title_hashes.push(u64::from_le_bytes(buf[pos..pos + 8].try_into().unwrap()));
        pos += 8;
    }

    let n_fhs = read_varint_u32(buf, &mut pos)? as usize;
    let mut family_hashes = Vec::with_capacity(n_fhs.min(64));
    for _ in 0..n_fhs {
        if pos + 8 > buf.len() {
            return Err(FormatError::CorruptPayload("catalog fh truncated".into()));
        }
        family_hashes.push(u64::from_le_bytes(buf[pos..pos + 8].try_into().unwrap()));
        pos += 8;
    }

    let n_alts = read_varint_u32(buf, &mut pos)? as usize;
    let mut alt_identifiers = Vec::with_capacity(n_alts.min(32));
    for _ in 0..n_alts {
        let s = read_str(buf, &mut pos)?;
        let v = read_str(buf, &mut pos)?;
        alt_identifiers.push((s, v));
    }

    Ok(ContentRecord {
        entity_id,
        content_type,
        year,
        adult,
        source,
        id,
        title,
        original_title,
        strict_title_hashes,
        family_hashes,
        alt_identifiers,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_catalog_roundtrip_and_alt_lookup() {
        let dir = std::env::temp_dir().join(format!("dhtms_cat_test_{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let p = dir.join("catalog.bin");

        let recs = vec![
            ContentRecord::default(),
            ContentRecord {
                entity_id: 1,
                content_type: 1,
                year: 1995,
                adult: false,
                source: "tmdb".into(),
                id: "629".into(),
                title: "The Usual Suspects".into(),
                original_title: "The Usual Suspects".into(),
                strict_title_hashes: vec![12345],
                family_hashes: vec![67890],
                alt_identifiers: vec![("imdb".into(), "tt0114814".into())],
            },
        ];
        MmapContentCatalog::build_and_save(&p, 3, &recs).unwrap();
        let cat = MmapContentCatalog::open(&p, Some(3)).unwrap();
        let found = cat.find_by_key(1, "tmdb", "629").unwrap().unwrap();
        assert_eq!(found.title, "The Usual Suspects");
        let by_imdb = cat.find_by_alt_id(1, "imdb", "tt0114814").unwrap();
        assert_eq!(by_imdb.len(), 1);
        assert_eq!(by_imdb[0].entity_id, 1);

        let _ = std::fs::remove_dir_all(&dir);
    }
}
