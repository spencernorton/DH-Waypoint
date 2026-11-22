use crate::{CheckedFileWriter, CheckedMmap, FileKind, FormatError, Pod};
use roaring::RoaringBitmap;
use std::path::Path;

const INLINE_SINGLETON_FLAG: u64 = 1u64 << 63;
const OFFSET_MASK: u64 = (1u64 << 39) - 1; // up to 512 GiB data region
const LEN_SHIFT: u32 = 39;
const LEN_MASK: u64 = (1u64 << 24) - 1; // up to 16 MiB per blob

#[repr(C)]
#[derive(Debug, Clone, Copy, Default)]
pub struct BlobIndexEntry {
    pub key: u64,
    /// If bit 63 is set: low 32 bits hold an inline singleton u32 doc_id (0 bytes in data region!).
    /// Otherwise: bits 0..39 = byte offset in data region, bits 39..63 = byte length.
    pub meta: u64,
}

unsafe impl Pod for BlobIndexEntry {}

impl BlobIndexEntry {
    #[inline]
    pub fn inline_singleton(key: u64, doc_id: u32) -> Self {
        Self {
            key,
            meta: INLINE_SINGLETON_FLAG | (doc_id as u64),
        }
    }

    #[inline]
    pub fn blob_ref(key: u64, offset: u64, len: u32) -> Result<Self, FormatError> {
        if offset > OFFSET_MASK || (len as u64) > LEN_MASK {
            return Err(FormatError::CorruptPayload(format!(
                "blob ref overflow: offset={offset} len={len}"
            )));
        }
        Ok(Self {
            key,
            meta: (offset & OFFSET_MASK) | (((len as u64) & LEN_MASK) << LEN_SHIFT),
        })
    }

    #[inline]
    pub fn is_inline(&self) -> bool {
        (self.meta & INLINE_SINGLETON_FLAG) != 0
    }

    #[inline]
    pub fn inline_doc_id(&self) -> u32 {
        self.meta as u32
    }

    #[inline]
    pub fn offset_len(&self) -> (usize, usize) {
        let off = (self.meta & OFFSET_MASK) as usize;
        let len = ((self.meta >> LEN_SHIFT) & LEN_MASK) as usize;
        (off, len)
    }
}

pub enum BlobValue {
    InlineSingleton(u32),
    Bytes(Vec<u8>),
}

/// Checked, zero-heap mmap sorted KV store for RoaringBitmaps and small binary blobs.
/// Supports inline singleton bitmaps (0 data bytes for df=1 keys) and propagates all format errors.
pub struct MmapBlobMap {
    cm: CheckedMmap,
    n_keys: usize,
    data_start: usize,
}

impl MmapBlobMap {
    pub fn build_from_bitmaps(
        path: &Path,
        generation_id: u64,
        mut items: Vec<(u64, RoaringBitmap)>,
    ) -> Result<u64, FormatError> {
        items.sort_unstable_by_key(|(k, _)| *k);
        items.dedup_by_key(|(k, _)| *k);

        let mut entries = Vec::with_capacity(items.len());
        let mut data = Vec::new();
        let mut ser_buf = Vec::new();
        for (k, bm) in items {
            if bm.is_empty() {
                continue;
            }
            if bm.len() == 1 {
                if let Some(d) = bm.min() {
                    entries.push(BlobIndexEntry::inline_singleton(k, d));
                    continue;
                }
            }
            ser_buf.clear();
            bm.serialize_into(&mut ser_buf)?;
            let off = data.len() as u64;
            let len = ser_buf.len() as u32;
            data.extend_from_slice(&ser_buf);
            entries.push(BlobIndexEntry::blob_ref(k, off, len)?);
        }
        Self::write_entries_and_data(path, generation_id, &entries, &data)
    }

    pub fn build_from_bytes(
        path: &Path,
        generation_id: u64,
        mut items: Vec<(u64, Vec<u8>)>,
    ) -> Result<u64, FormatError> {
        items.sort_unstable_by_key(|(k, _)| *k);
        items.dedup_by_key(|(k, _)| *k);
        let mut entries = Vec::with_capacity(items.len());
        let mut data = Vec::new();
        for (k, bytes) in items {
            let off = data.len() as u64;
            let len = bytes.len() as u32;
            data.extend_from_slice(&bytes);
            entries.push(BlobIndexEntry::blob_ref(k, off, len)?);
        }
        Self::write_entries_and_data(path, generation_id, &entries, &data)
    }

    fn write_entries_and_data(
        path: &Path,
        generation_id: u64,
        entries: &[BlobIndexEntry],
        data: &[u8],
    ) -> Result<u64, FormatError> {
        let mut w = CheckedFileWriter::create(path, FileKind::BlobMap, generation_id)?;
        let idx_bytes = unsafe {
            std::slice::from_raw_parts(
                entries.as_ptr() as *const u8,
                std::mem::size_of_val(entries),
            )
        };
        w.write_payload(idx_bytes)?;
        w.write_payload(data)?;
        w.finish(entries.len() as u64, data.len() as u64, 0)
    }

    pub fn open(path: &Path, expected_gen: Option<u64>) -> Result<Self, FormatError> {
        let cm = CheckedMmap::open(path, FileKind::BlobMap, expected_gen, false)?;
        let n_keys = cm.header().record_count as usize;
        let data_len = cm.header().aux_u64 as usize;
        let idx_bytes = n_keys
            .checked_mul(std::mem::size_of::<BlobIndexEntry>())
            .ok_or_else(|| FormatError::CorruptPayload("blob_map index size overflow".into()))?;
        let total = idx_bytes
            .checked_add(data_len)
            .ok_or_else(|| FormatError::CorruptPayload("blob_map total size overflow".into()))?;
        if total as u64 != cm.header().payload_len {
            return Err(FormatError::CorruptPayload(format!(
                "blob_map payload mismatch: idx({idx_bytes}) + data({data_len}) != {}",
                cm.header().payload_len
            )));
        }
        let p = cm.payload();
        if n_keys > 0
            && !(p.as_ptr() as usize).is_multiple_of(std::mem::align_of::<BlobIndexEntry>())
        {
            return Err(FormatError::AlignmentViolation {
                align: std::mem::align_of::<BlobIndexEntry>(),
                addr: p.as_ptr() as usize,
            });
        }
        Ok(Self {
            cm,
            n_keys,
            data_start: idx_bytes,
        })
    }

    #[inline]
    pub fn len(&self) -> usize {
        self.n_keys
    }

    #[inline]
    pub fn is_empty(&self) -> bool {
        self.n_keys == 0
    }

    #[inline]
    fn entries(&self) -> &[BlobIndexEntry] {
        if self.n_keys == 0 {
            return &[];
        }
        // SAFETY: Validated length and alignment in open().
        unsafe {
            std::slice::from_raw_parts(
                self.cm.payload().as_ptr() as *const BlobIndexEntry,
                self.n_keys,
            )
        }
    }

    #[inline]
    pub fn lookup_entry(&self, key: u64) -> Option<BlobIndexEntry> {
        let ents = self.entries();
        let idx = ents.binary_search_by_key(&key, |e| e.key).ok()?;
        Some(ents[idx])
    }

    #[inline]
    pub fn get_slice(&self, key: u64) -> Result<Option<&[u8]>, FormatError> {
        let Some(e) = self.lookup_entry(key) else {
            return Ok(None);
        };
        if e.is_inline() {
            return Err(FormatError::CorruptPayload(
                "get_slice called on inline singleton entry".into(),
            ));
        }
        let (off, len) = e.offset_len();
        let data = &self.cm.payload()[self.data_start..];
        let end = off
            .checked_add(len)
            .ok_or_else(|| FormatError::CorruptPayload("blob slice overflow".into()))?;
        if end > data.len() {
            return Err(FormatError::CorruptPayload(format!(
                "blob slice out of bounds: {off}..{end} > {}",
                data.len()
            )));
        }
        Ok(Some(&data[off..end]))
    }

    pub fn get_bitmap(&self, key: u64) -> Result<Option<RoaringBitmap>, FormatError> {
        let Some(e) = self.lookup_entry(key) else {
            return Ok(None);
        };
        if e.is_inline() {
            let mut bm = RoaringBitmap::new();
            bm.insert(e.inline_doc_id());
            return Ok(Some(bm));
        }
        let (off, len) = e.offset_len();
        let data = &self.cm.payload()[self.data_start..];
        let end = off
            .checked_add(len)
            .ok_or_else(|| FormatError::CorruptPayload("bitmap blob overflow".into()))?;
        if end > data.len() {
            return Err(FormatError::CorruptPayload(format!(
                "bitmap blob out of bounds: {off}..{end} > {}",
                data.len()
            )));
        }
        let bm = RoaringBitmap::deserialize_from(&data[off..end])
            .map_err(|err| FormatError::CorruptPayload(format!("corrupt roaring bitmap: {err}")))?;
        Ok(Some(bm))
    }

    /// Iterate all keys and bitmaps (used during compaction or verification).
    pub fn iter_bitmaps(
        &self,
    ) -> impl Iterator<Item = Result<(u64, RoaringBitmap), FormatError>> + '_ {
        let ents = self.entries();
        let data = &self.cm.payload()[self.data_start..];
        ents.iter().map(move |e| {
            if e.is_inline() {
                let mut bm = RoaringBitmap::new();
                bm.insert(e.inline_doc_id());
                Ok((e.key, bm))
            } else {
                let (off, len) = e.offset_len();
                let end = off
                    .checked_add(len)
                    .ok_or_else(|| FormatError::CorruptPayload("bitmap blob overflow".into()))?;
                if end > data.len() {
                    return Err(FormatError::CorruptPayload(
                        "bitmap blob out of bounds".into(),
                    ));
                }
                let bm = RoaringBitmap::deserialize_from(&data[off..end]).map_err(|err| {
                    FormatError::CorruptPayload(format!("corrupt roaring bitmap: {err}"))
                })?;
                Ok((e.key, bm))
            }
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_blob_map_inline_and_multi_roundtrip() {
        let dir = std::env::temp_dir().join(format!("dhtms_blob_test_{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let p = dir.join("bms.mmap");

        let mut bm1 = RoaringBitmap::new();
        bm1.insert(777);
        let mut bm2 = RoaringBitmap::new();
        bm2.insert(1);
        bm2.insert(2);
        bm2.insert(99999);

        MmapBlobMap::build_from_bitmaps(&p, 7, vec![(20, bm2.clone()), (10, bm1.clone())]).unwrap();
        let m = MmapBlobMap::open(&p, Some(7)).unwrap();
        assert_eq!(m.len(), 2);
        assert!(m.lookup_entry(10).unwrap().is_inline());
        assert!(!m.lookup_entry(20).unwrap().is_inline());
        assert_eq!(m.get_bitmap(10).unwrap().unwrap(), bm1);
        assert_eq!(m.get_bitmap(20).unwrap().unwrap(), bm2);
        assert!(m.get_bitmap(30).unwrap().is_none());

        let _ = std::fs::remove_dir_all(&dir);
    }
}
