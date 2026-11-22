use dh_tms_format::{
    write_checked_bytes, write_checked_pod_slice, CheckedMmap, FileKind, FormatError, Pod,
};
use std::path::Path;
use xxhash_rust::xxh3::xxh3_64_with_seed;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum InfoHash {
    V1([u8; 20]),
    V2([u8; 32]),
}

impl InfoHash {
    pub fn to_hex(&self) -> String {
        match self {
            Self::V1(b) => encode_hex(b),
            Self::V2(b) => encode_hex(b),
        }
    }

    pub fn to_magnet_xt(&self) -> String {
        match self {
            Self::V1(b) => format!("urn:btih:{}", encode_hex(b)),
            Self::V2(b) => format!("urn:btmh:1220{}", encode_hex(b)),
        }
    }
}

pub fn encode_hex(bytes: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut out = String::with_capacity(bytes.len() * 2);
    for &b in bytes {
        out.push(HEX[(b >> 4) as usize] as char);
        out.push(HEX[(b & 0x0F) as usize] as char);
    }
    out
}

#[inline]
fn hex_nibble(c: u8) -> Option<u8> {
    match c {
        b'0'..=b'9' => Some(c - b'0'),
        b'a'..=b'f' => Some(c - b'a' + 10),
        b'A'..=b'F' => Some(c - b'A' + 10),
        _ => None,
    }
}

/// Exact-length hex parser: strictly accepts 40 hex chars (v1) or 64 hex chars (v2).
/// Rejects truncated, overlong, or malformed inputs (no silent truncation!).
pub fn parse_info_hash_hex(raw: &str) -> Option<InfoHash> {
    let hex = raw
        .strip_prefix("\\\\x")
        .or_else(|| raw.strip_prefix("\\x"))
        .or_else(|| raw.strip_prefix("0x"))
        .unwrap_or(raw);
    let b = hex.as_bytes();
    match b.len() {
        40 => {
            let mut out = [0u8; 20];
            for i in 0..20 {
                out[i] = (hex_nibble(b[2 * i])? << 4) | hex_nibble(b[2 * i + 1])?;
            }
            Some(InfoHash::V1(out))
        }
        64 => {
            let mut out = [0u8; 32];
            for i in 0..32 {
                out[i] = (hex_nibble(b[2 * i])? << 4) | hex_nibble(b[2 * i + 1])?;
            }
            Some(InfoHash::V2(out))
        }
        _ => None,
    }
}

#[inline]
pub fn parse_v1_hex_exact(raw: &str) -> Option<[u8; 20]> {
    match parse_info_hash_hex(raw)? {
        InfoHash::V1(h) => Some(h),
        InfoHash::V2(_) => None,
    }
}

#[inline]
pub fn parse_v2_hex_exact(raw: &str) -> Option<[u8; 32]> {
    match parse_info_hash_hex(raw)? {
        InfoHash::V2(h) => Some(h),
        InfoHash::V1(_) => None,
    }
}

#[inline]
fn fastrange(h: u64, n: usize) -> usize {
    (((h as u128).wrapping_mul(n as u128)) >> 64) as usize
}

/// 8-byte compact fingerprint slot:
/// `doc_id_plus_1 == 0` => empty slot; otherwise verifies full 20B equality against dense `hash_rev_v1`.
/// Saves ~10 B/doc net while providing both O(1) forward lookup AND full reverse magnet emission!
#[repr(C)]
#[derive(Debug, Clone, Copy, Default)]
struct FpSlot {
    fp: u32,
    doc_id_plus_1: u32,
}

unsafe impl Pod for FpSlot {}

/// 36-byte sorted BEP-52 v2 directory entry (`[u8; 32]` + `u32` doc_id).
#[repr(C)]
#[derive(Debug, Clone, Copy, Default)]
struct V2DirEntry {
    hash_v2: [u8; 32],
    doc_id: u32,
}

unsafe impl Pod for V2DirEntry {}

/// 36-byte sorted reverse v2 entry (`doc_id` -> `hash_v2`) for hybrid & v2-only magnet emission.
#[repr(C)]
#[derive(Debug, Clone, Copy, Default)]
struct V2RevEntry {
    doc_id: u32,
    hash_v2: [u8; 32],
}

unsafe impl Pod for V2RevEntry {}

/// Cache-line (512-bit = 8×u64) split-block Bloom filter: 1 cache-line touch per negative lookup!
const BLOOM_BLOCK_WORDS: usize = 8;

#[inline]
fn split_block_mask(h: u64) -> [u64; BLOOM_BLOCK_WORDS] {
    let mut mask = [0u64; BLOOM_BLOCK_WORDS];
    // 8 independent bit positions inside one 512-bit cache line (using high + mixed bits of keyed XXH3)
    let mut x = h ^ (h >> 33);
    for word in &mut mask {
        x = x.wrapping_mul(0x9E37_79B9_7F4A_7C15).rotate_left(23);
        let bit = (x & 63) as u32;
        *word = 1u64 << bit;
    }
    mask
}

pub struct HashDirBuildStats {
    pub rev_v1_bytes: u64,
    pub dir_v1_bytes: u64,
    pub v2_bytes: u64,
    pub max_probe_dist: u32,
}

/// Build and save:
/// 1. `hash_rev_v1.bin`: dense 20B/doc reverse column (`doc_id -> [u8; 20]`).
/// 2. `hash_dir_v1.bin`: cache-line split-block Bloom filter + 8B fingerprint Robin-Hood table (keyed XXH3).
/// 3. `hash_v2_dir.bin` + `hash_v2_rev.bin`: BEP-52 32B v2 & hybrid alias tables sharing `doc_id`.
///
/// Caller MUST pass deduplicated `v1_hashes` indexed by `doc_id` (`[0u8; 20]` allowed only for pure v2-only docs).
pub fn build_and_save_hash_dir(
    dir: &Path,
    generation_id: u64,
    hash_seed: u64,
    v1_hashes: &[[u8; 20]],
    v2_aliases: &[(u32, [u8; 32])],
) -> Result<HashDirBuildStats, FormatError> {
    let n_docs = v1_hashes.len();
    // 1. Write dense 20B reverse v1 column
    let rev_v1_bytes = write_checked_pod_slice(
        &dir.join("hash_rev_v1.bin"),
        FileKind::HashRevV1,
        generation_id,
        hash_seed,
        0,
        v1_hashes,
    )?;

    // 2. Build split-block Bloom + 75%-load 8B fingerprint Robin-Hood table
    let non_zero_v1 = v1_hashes.iter().filter(|h| **h != [0u8; 20]).count().max(1);
    let cap = ((non_zero_v1 as f64 / 0.75).ceil() as usize).max(16);
    let num_bloom_blocks = (non_zero_v1 * 10).div_ceil(512).max(1);
    let mut bloom = vec![0u64; num_bloom_blocks * BLOOM_BLOCK_WORDS];
    let mut slots = vec![FpSlot::default(); cap];
    let mut psls = vec![0u32; cap];
    let mut max_probe_dist = 0u32;

    for (doc_idx, hash) in v1_hashes.iter().enumerate() {
        if *hash == [0u8; 20] {
            continue;
        }
        let doc_id = doc_idx as u32;
        if doc_id == u32::MAX {
            return Err(FormatError::CorruptPayload(
                "doc_id == u32::MAX reserved".into(),
            ));
        }
        let h = xxh3_64_with_seed(hash, hash_seed);
        // Insert into cache-line split-block Bloom filter
        let blk = fastrange(h, num_bloom_blocks);
        let mask = split_block_mask(h);
        let base = blk * BLOOM_BLOCK_WORDS;
        for w in 0..BLOOM_BLOCK_WORDS {
            bloom[base + w] |= mask[w];
        }

        let fp = (h >> 32) as u32;
        let mut cur = FpSlot {
            fp,
            doc_id_plus_1: doc_id + 1,
        };
        let mut idx = fastrange(h, cap);
        let mut dist = 0u32;

        loop {
            let ex = slots[idx];
            if ex.doc_id_plus_1 == 0 {
                slots[idx] = cur;
                psls[idx] = dist;
                if dist > max_probe_dist {
                    max_probe_dist = dist;
                }
                break;
            }
            // Enforce canonical deduplication (reject accidental duplicate v1 hashes)
            if ex.fp == cur.fp
                && v1_hashes[(ex.doc_id_plus_1 - 1) as usize]
                    == v1_hashes[(cur.doc_id_plus_1 - 1) as usize]
            {
                return Err(FormatError::CorruptPayload(format!(
                    "duplicate v1 info_hash detected at doc_id {doc_id} and {}",
                    ex.doc_id_plus_1 - 1
                )));
            }
            if dist > psls[idx] {
                std::mem::swap(&mut slots[idx], &mut cur);
                std::mem::swap(&mut psls[idx], &mut dist);
            }
            idx += 1;
            if idx == cap {
                idx = 0;
            }
            dist += 1;
            if dist > max_probe_dist {
                max_probe_dist = dist;
            }
        }
    }

    // Serialize hash_dir_v1.bin payload:
    // [cap: u64][num_bloom_blocks: u64][max_probe_dist: u32][pad: u32][bloom: u64*][slots: FpSlot*]
    let mut payload = Vec::with_capacity(24 + bloom.len() * 8 + slots.len() * 8);
    payload.extend_from_slice(&(cap as u64).to_le_bytes());
    payload.extend_from_slice(&(num_bloom_blocks as u64).to_le_bytes());
    payload.extend_from_slice(&max_probe_dist.to_le_bytes());
    payload.extend_from_slice(&0u32.to_le_bytes());
    let bloom_bytes =
        unsafe { std::slice::from_raw_parts(bloom.as_ptr() as *const u8, bloom.len() * 8) };
    let slot_bytes = unsafe {
        std::slice::from_raw_parts(
            slots.as_ptr() as *const u8,
            slots.len() * std::mem::size_of::<FpSlot>(),
        )
    };
    payload.extend_from_slice(bloom_bytes);
    payload.extend_from_slice(slot_bytes);

    let dir_v1_bytes = write_checked_bytes(
        &dir.join("hash_dir_v1.bin"),
        FileKind::HashDirV1,
        generation_id,
        n_docs as u64,
        hash_seed,
        0,
        &payload,
    )?;

    // 3. Build BEP-52 32-byte v2 forward & reverse tables
    let mut v2_dir: Vec<V2DirEntry> = v2_aliases
        .iter()
        .map(|&(doc_id, hash_v2)| V2DirEntry { hash_v2, doc_id })
        .collect();
    v2_dir.sort_unstable_by_key(|e| e.hash_v2);
    for w in v2_dir.windows(2) {
        if w[0].hash_v2 == w[1].hash_v2 && w[0].doc_id != w[1].doc_id {
            return Err(FormatError::CorruptPayload(format!(
                "duplicate v2 info_hash mapped to two doc_ids: {} and {}",
                w[0].doc_id, w[1].doc_id
            )));
        }
    }
    v2_dir.dedup_by_key(|e| e.hash_v2);

    let mut v2_rev: Vec<V2RevEntry> = v2_dir
        .iter()
        .map(|e| V2RevEntry {
            doc_id: e.doc_id,
            hash_v2: e.hash_v2,
        })
        .collect();
    v2_rev.sort_unstable_by_key(|e| e.doc_id);

    let v2_bytes = write_checked_pod_slice(
        &dir.join("hash_v2_dir.bin"),
        FileKind::HashV2Dir,
        generation_id,
        hash_seed,
        0,
        &v2_dir,
    )? + write_checked_pod_slice(
        &dir.join("hash_v2_rev.bin"),
        FileKind::HashV2Rev,
        generation_id,
        hash_seed,
        0,
        &v2_rev,
    )?;

    Ok(HashDirBuildStats {
        rev_v1_bytes,
        dir_v1_bytes,
        v2_bytes,
        max_probe_dist,
    })
}

/// Checked zero-heap mmap HashDir supporting keyed v1 lookup, BEP-52 v2 & hybrid alias lookup,
/// and O(1) reverse `doc_id -> InfoHash` magnet URI resolution.
pub struct HashDir {
    rev_v1: CheckedMmap,
    dir_v1: CheckedMmap,
    v2_dir: CheckedMmap,
    v2_rev: CheckedMmap,
    hash_seed: u64,
    cap: usize,
    num_bloom_blocks: usize,
    max_probe_dist: usize,
    bloom_off: usize,
    slots_off: usize,
}

impl HashDir {
    pub fn open(dir: &Path, expected_gen: Option<u64>) -> Result<Self, FormatError> {
        let rev_v1 = CheckedMmap::open(
            &dir.join("hash_rev_v1.bin"),
            FileKind::HashRevV1,
            expected_gen,
            false,
        )?;
        let dir_v1 = CheckedMmap::open(
            &dir.join("hash_dir_v1.bin"),
            FileKind::HashDirV1,
            expected_gen,
            false,
        )?;
        let v2_dir = CheckedMmap::open(
            &dir.join("hash_v2_dir.bin"),
            FileKind::HashV2Dir,
            expected_gen,
            false,
        )?;
        let v2_rev = CheckedMmap::open(
            &dir.join("hash_v2_rev.bin"),
            FileKind::HashV2Rev,
            expected_gen,
            false,
        )?;

        // Validate typed slices up front so getters are panic-free
        let _ = rev_v1.typed_slice::<[u8; 20]>()?;
        let _ = v2_dir.typed_slice::<V2DirEntry>()?;
        let _ = v2_rev.typed_slice::<V2RevEntry>()?;

        let hash_seed = dir_v1.header().aux_u64;
        let p = dir_v1.payload();
        if p.len() < 24 {
            return Err(FormatError::CorruptPayload(
                "hash_dir_v1 header too short".into(),
            ));
        }
        let cap = u64::from_le_bytes(p[0..8].try_into().unwrap()) as usize;
        let num_bloom_blocks = u64::from_le_bytes(p[8..16].try_into().unwrap()) as usize;
        let max_probe_dist = u32::from_le_bytes(p[16..20].try_into().unwrap()) as usize;
        let bloom_off = 24usize;
        let bloom_bytes = num_bloom_blocks
            .checked_mul(BLOOM_BLOCK_WORDS * 8)
            .ok_or_else(|| FormatError::CorruptPayload("bloom size overflow".into()))?;
        let slots_off = bloom_off
            .checked_add(bloom_bytes)
            .ok_or_else(|| FormatError::CorruptPayload("slots_off overflow".into()))?;
        let slots_bytes = cap
            .checked_mul(std::mem::size_of::<FpSlot>())
            .ok_or_else(|| FormatError::CorruptPayload("slots size overflow".into()))?;
        let expected_len = slots_off
            .checked_add(slots_bytes)
            .ok_or_else(|| FormatError::CorruptPayload("hash_dir total overflow".into()))?;
        if expected_len != p.len() {
            return Err(FormatError::CorruptPayload(format!(
                "hash_dir_v1 payload length mismatch: expected {expected_len}, got {}",
                p.len()
            )));
        }
        Ok(Self {
            rev_v1,
            dir_v1,
            v2_dir,
            v2_rev,
            hash_seed,
            cap,
            num_bloom_blocks,
            max_probe_dist,
            bloom_off,
            slots_off,
        })
    }

    #[inline]
    pub fn num_docs(&self) -> usize {
        self.rev_v1.header().record_count as usize
    }

    #[inline]
    pub fn rev_v1_slice(&self) -> &[[u8; 20]] {
        // SAFETY: validated in open().
        self.rev_v1.typed_slice::<[u8; 20]>().unwrap()
    }

    #[inline]
    fn bloom_words(&self) -> &[u64] {
        let p = &self.dir_v1.payload()[self.bloom_off..self.slots_off];
        unsafe {
            std::slice::from_raw_parts(
                p.as_ptr() as *const u64,
                self.num_bloom_blocks * BLOOM_BLOCK_WORDS,
            )
        }
    }

    #[inline]
    fn fp_slots(&self) -> &[FpSlot] {
        let p = &self.dir_v1.payload()[self.slots_off..];
        unsafe { std::slice::from_raw_parts(p.as_ptr() as *const FpSlot, self.cap) }
    }

    #[inline]
    pub fn maybe_contains_v1(&self, hash: &[u8; 20]) -> bool {
        let h = xxh3_64_with_seed(hash, self.hash_seed);
        let blk = fastrange(h, self.num_bloom_blocks);
        let mask = split_block_mask(h);
        let bloom = self.bloom_words();
        let base = blk * BLOOM_BLOCK_WORDS;
        for w in 0..BLOOM_BLOCK_WORDS {
            if (bloom[base + w] & mask[w]) == 0 {
                return false;
            }
        }
        true
    }

    /// O(1) keyed v1 lookup with single-hash Bloom check + Robin-Hood bounded probe + full 20B verification.
    #[inline]
    pub fn get_v1(&self, hash: &[u8; 20]) -> Option<u32> {
        let h = xxh3_64_with_seed(hash, self.hash_seed);
        let blk = fastrange(h, self.num_bloom_blocks);
        let mask = split_block_mask(h);
        let bloom = self.bloom_words();
        let base = blk * BLOOM_BLOCK_WORDS;
        for w in 0..BLOOM_BLOCK_WORDS {
            if (bloom[base + w] & mask[w]) == 0 {
                return None;
            }
        }

        let fp = (h >> 32) as u32;
        let slots = self.fp_slots();
        let rev = self.rev_v1_slice();
        let mut idx = fastrange(h, self.cap);
        let max_steps = (self.max_probe_dist + 1).min(self.cap);
        for _ in 0..max_steps {
            let s = slots[idx];
            if s.doc_id_plus_1 == 0 {
                return None;
            }
            if s.fp == fp {
                let doc_id = s.doc_id_plus_1 - 1;
                if let Some(stored) = rev.get(doc_id as usize) {
                    if stored == hash {
                        return Some(doc_id);
                    }
                }
            }
            idx += 1;
            if idx == self.cap {
                idx = 0;
            }
        }
        None
    }

    /// O(log N) exact BEP-52 32-byte v2 / hybrid lookup.
    #[inline]
    pub fn get_v2(&self, hash: &[u8; 32]) -> Option<u32> {
        let ents = self.v2_dir.typed_slice::<V2DirEntry>().ok()?;
        let idx = ents.binary_search_by_key(hash, |e| e.hash_v2).ok()?;
        Some(ents[idx].doc_id)
    }

    #[inline]
    pub fn get(&self, hash: &InfoHash) -> Option<u32> {
        match hash {
            InfoHash::V1(h) => self.get_v1(h),
            InfoHash::V2(h) => self.get_v2(h),
        }
    }

    /// O(1) reverse lookup returning the canonical v1 hash (and optional BEP-52 v2 hash) for magnet URI emission.
    #[inline]
    pub fn get_hashes_for_doc(&self, doc_id: u32) -> (Option<[u8; 20]>, Option<[u8; 32]>) {
        let v1 = self
            .rev_v1_slice()
            .get(doc_id as usize)
            .copied()
            .filter(|h| *h != [0u8; 20]);
        let v2 = self
            .v2_rev
            .typed_slice::<V2RevEntry>()
            .ok()
            .and_then(|ents| {
                ents.binary_search_by_key(&doc_id, |e| e.doc_id)
                    .ok()
                    .map(|i| ents[i].hash_v2)
            });
        (v1, v2)
    }

    #[inline]
    pub fn get_v1_for_doc(&self, doc_id: u32) -> Option<[u8; 20]> {
        self.rev_v1_slice()
            .get(doc_id as usize)
            .copied()
            .filter(|h| *h != [0u8; 20])
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_exact_hex_parsing_no_truncation() {
        let h40 = "0123456789abcdef0123456789abcdef01234567";
        let h64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";
        assert!(matches!(parse_info_hash_hex(h40), Some(InfoHash::V1(_))));
        assert!(matches!(parse_info_hash_hex(h64), Some(InfoHash::V2(_))));
        assert!(parse_v1_hex_exact(h64).is_none()); // MUST NOT truncate 64-hex v2 into v1!
        assert!(parse_info_hash_hex("0123456789abcdef0123456789abcdef0123456789").is_none()); // 42 hex rejected
        assert!(parse_info_hash_hex("0123").is_none());
    }

    #[test]
    fn test_hashdir_v1_v2_hybrid_and_dedup() {
        let dir = std::env::temp_dir().join(format!("dhtms_hdir_test_{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();

        let mut v1s = Vec::new();
        for i in 0u32..500 {
            let mut h = [0u8; 20];
            h[0..4].copy_from_slice(&i.to_le_bytes());
            h[19] = ((i % 250) + 1) as u8;
            v1s.push(h);
        }
        // Pure v2-only doc at doc_id = 500
        v1s.push([0u8; 20]);

        let v2_hybrid = [0xABu8; 32];
        let v2_only = [0xCDu8; 32];
        build_and_save_hash_dir(
            &dir,
            11,
            0xCAFE_BABE_1234_5678,
            &v1s,
            &[(42, v2_hybrid), (500, v2_only)],
        )
        .unwrap();

        let hd = HashDir::open(&dir, Some(11)).unwrap();
        for i in 0u32..500 {
            assert_eq!(hd.get_v1(&v1s[i as usize]), Some(i));
            assert_eq!(hd.get_v1_for_doc(i), Some(v1s[i as usize]));
        }
        assert_eq!(hd.get_v2(&v2_hybrid), Some(42));
        assert_eq!(hd.get_v2(&v2_only), Some(500));
        assert_eq!(hd.get_hashes_for_doc(42), (Some(v1s[42]), Some(v2_hybrid)));
        assert_eq!(hd.get_hashes_for_doc(500), (None, Some(v2_only)));
        assert_eq!(hd.get_v1(&[0xFFu8; 20]), None);

        // Duplicate v1 hash MUST be rejected
        let mut dup = v1s.clone();
        dup.push(v1s[0]);
        assert!(build_and_save_hash_dir(&dir, 12, 1, &dup, &[]).is_err());

        let _ = std::fs::remove_dir_all(&dir);
    }
}
