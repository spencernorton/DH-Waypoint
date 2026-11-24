pub mod l0;

use ahash::AHashMap;
use byteorder::{LittleEndian, WriteBytesExt};
use dh_tms_core::{CorePlane, MetaRow};
use dh_tms_format::blob_map::MmapBlobMap;
use dh_tms_format::{
    read_varint_u32, write_checked_bytes, write_varint_u32, CheckedFileWriter, CheckedMmap,
    FileKind, FormatError, Pod,
};
use dh_tms_grammar::{
    ep_key_entity_any_season, ep_key_entity_episode, ep_key_entity_whole_season,
    ep_key_global_any_season, ep_key_global_episode, ep_key_global_whole_season,
    extract_substring_query_trigrams, fold_accents_lowercase, parse_release_name,
    title_hashes_from_str, tokenize_alnum, unlinked_year_compatible, EpisodeSpec, CT_MOVIE,
    CT_TV_SHOW, CT_UNKNOWN, NUM_CONTENT_TYPES,
};
use dh_tms_policy::{PolicyError, PolicyPlane, PolicyTier};
use l0::L0Overlay;
use roaring::RoaringBitmap;
use std::cmp::Ordering as CmpOrd;
use std::collections::{BinaryHeap, HashSet};
use std::fs::File;
use std::io::{BufReader, BufWriter, Read, Write};
use std::path::{Path, PathBuf};
use xxhash_rust::xxh3::xxh3_64_with_seed;

pub const WAND_BLOCK_SIZE: usize = 128;
pub const DENSE_TERM_DF_THRESHOLD: usize = 50_000;
pub const MAX_VIDEO_SOURCE: u8 = 9;
pub const MAX_VIDEO_CODEC: u8 = 9;
const POSTING_FLUSH_THRESHOLD: usize = 15_000_000;
const TERM_FLAG_INLINE_SINGLE: u32 = 1 << 0;
const FLOAT_BOUND_EPS: f32 = 1e-4;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum QueryMode {
    And,
    Or,
    Phrase,
}

/// Structured conjunctive filter clause evaluated as a disjunction (`any_clauses`)
/// across base static facet RoaringBitmaps and live L0 overlay documents.
#[derive(Debug, Clone, Default)]
pub struct SearchFilterClause {
    pub content_type: Option<u8>,
    pub resolutions: Vec<u8>,
    pub exclude_resolutions: Vec<u8>,
    pub require_anime: bool,
    pub require_3d: bool,
    pub video_source: Option<u8>,
    pub video_codec: Option<u8>,
}

impl SearchFilterClause {
    pub fn matches_l0(&self, doc: &l0::L0Doc) -> bool {
        if let Some(ct) = self.content_type {
            if doc.content_type != ct {
                return false;
            }
        }
        if !self.resolutions.is_empty() && !self.resolutions.contains(&doc.resolution) {
            return false;
        }
        if !self.exclude_resolutions.is_empty()
            && self.exclude_resolutions.contains(&doc.resolution)
        {
            return false;
        }
        if self.require_anime && !doc.is_anime {
            return false;
        }
        if self.require_3d && doc.video_3d == 0 {
            return false;
        }
        if let Some(src) = self.video_source {
            if doc.video_source != src {
                return false;
            }
        }
        if let Some(codec) = self.video_codec {
            if doc.video_codec != codec {
                return false;
            }
        }
        true
    }
}

/// Deterministic top-K score comparator: higher score wins; ties break by SMALLER `doc_id`
/// (which corresponds to NEWER `published_epoch` under `(ct, published_epoch DESC)` physical ordering!).
#[derive(Debug, Clone, Copy)]
pub struct ScoredDoc {
    pub doc_id: u32,
    pub score: f32,
}

impl PartialEq for ScoredDoc {
    fn eq(&self, other: &Self) -> bool {
        self.doc_id == other.doc_id && self.score.to_bits() == other.score.to_bits()
    }
}
impl Eq for ScoredDoc {}

/// Ordering for a min-heap keeping top-K: WORST item sits at top of BinaryHeap!
/// So `Worst` compares GREATER than `Better` inside `HeapItem`.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct MinHeapItem(ScoredDoc);

impl Ord for MinHeapItem {
    fn cmp(&self, other: &Self) -> CmpOrd {
        // Better = higher score, or equal score & smaller doc_id.
        // BinaryHeap is a max-heap, so return Greater when `self` is WORSE than `other`!
        other
            .0
            .score
            .total_cmp(&self.0.score)
            .then_with(|| self.0.doc_id.cmp(&other.0.doc_id))
    }
}
impl PartialOrd for MinHeapItem {
    fn partial_cmp(&self, other: &Self) -> Option<CmpOrd> {
        Some(self.cmp(other))
    }
}

#[inline]
fn is_strictly_better(cand: ScoredDoc, worst: ScoredDoc) -> bool {
    match cand.score.total_cmp(&worst.score) {
        CmpOrd::Greater => true,
        CmpOrd::Equal => cand.doc_id < worst.doc_id,
        CmpOrd::Less => false,
    }
}

#[repr(C)]
#[derive(Debug, Clone, Copy, Default)]
struct TermDirEntry {
    term_hash: u64,
    /// When `flags & TERM_FLAG_INLINE_SINGLE != 0`:
    ///   low 32 bits = doc_id, bits 32..48 = u16 impact (0 extra block/posting bytes!).
    /// Otherwise: byte offset into block_dir section.
    block_dir_offset: u64,
    df: u32,
    idf_bits: u32,
    num_blocks: u32,
    flags_max_imp: u32, // low 16 bits = term global max_impact (u16), high 16 bits = flags
}

unsafe impl Pod for TermDirEntry {}

impl TermDirEntry {
    #[inline]
    fn idf(&self) -> f32 {
        f32::from_bits(self.idf_bits)
    }

    #[inline]
    fn is_inline_single(&self) -> bool {
        ((self.flags_max_imp >> 16) & TERM_FLAG_INLINE_SINGLE) != 0
    }

    #[inline]
    fn inline_doc_imp(&self) -> (u32, u16) {
        (
            self.block_dir_offset as u32,
            (self.block_dir_offset >> 32) as u16,
        )
    }

    #[inline]
    fn global_max_impact(&self) -> u16 {
        (self.flags_max_imp & 0xFFFF) as u16
    }
}

#[repr(C)]
#[derive(Debug, Clone, Copy, Default)]
struct BlockMeta {
    first_doc: u32,
    last_doc: u32,
    max_impact: u16,
    count: u16,
    data_len: u32,
    data_offset: u64,
}

unsafe impl Pod for BlockMeta {}

#[derive(Debug)]
pub enum SearchError {
    Format(FormatError),
    Policy(PolicyError),
    TrigramsDisabled,
    TooManyCandidates,
}

impl std::fmt::Display for SearchError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Format(e) => write!(f, "search format error: {e}"),
            Self::Policy(e) => write!(f, "search policy error: {e}"),
            Self::TrigramsDisabled => {
                write!(f, "trigram substring index is disabled for this generation")
            }
            Self::TooManyCandidates => {
                write!(f, "substring candidate set exceeds bounded hash-order scan cap")
            }
        }
    }
}
impl std::error::Error for SearchError {}
impl From<FormatError> for SearchError {
    fn from(e: FormatError) -> Self {
        Self::Format(e)
    }
}
impl From<PolicyError> for SearchError {
    fn from(e: PolicyError) -> Self {
        Self::Policy(e)
    }
}

struct RunReader {
    r: BufReader<File>,
    cur_term: Option<(u64, Vec<(u32, u16)>)>,
}

impl RunReader {
    fn open(path: &Path) -> Result<Self, FormatError> {
        let mut rr = Self {
            r: BufReader::with_capacity(2 * 1024 * 1024, File::open(path)?),
            cur_term: None,
        };
        rr.advance()?;
        Ok(rr)
    }

    fn advance(&mut self) -> Result<(), FormatError> {
        let mut hdr = [0u8; 12];
        let mut read = 0usize;
        while read < 12 {
            let n = self.r.read(&mut hdr[read..])?;
            if n == 0 {
                if read == 0 {
                    self.cur_term = None;
                    return Ok(());
                } else {
                    return Err(FormatError::CorruptPayload(
                        "truncated posting run header".into(),
                    ));
                }
            }
            read += n;
        }
        let th = u64::from_le_bytes(hdr[0..8].try_into().unwrap());
        let n = u32::from_le_bytes(hdr[8..12].try_into().unwrap()) as usize;
        let mut raw = vec![0u8; n * 6];
        self.r.read_exact(&mut raw)?;
        let mut vec = Vec::with_capacity(n);
        for chunk in raw.as_chunks::<6>().0 {
            let d = u32::from_le_bytes(chunk[0..4].try_into().unwrap());
            let imp = u16::from_le_bytes(chunk[4..6].try_into().unwrap());
            vec.push((d, imp));
        }
        self.cur_term = Some((th, vec));
        Ok(())
    }
}

pub struct CustomSearchBuildStats {
    pub inverted_bytes: u64,
    pub dense_terms_bytes: u64,
    pub struct_bms_bytes: u64,
    pub trigrams_bytes: u64,
    pub num_terms: usize,
    pub num_dense_terms: usize,
}

#[inline]
pub fn term_hash(tok: &str, seed: u64) -> u64 {
    xxh3_64_with_seed(tok.as_bytes(), seed)
}

#[inline]
pub fn trigram_key(tri: [u8; 3]) -> u64 {
    (tri[0] as u64) | ((tri[1] as u64) << 8) | ((tri[2] as u64) << 16)
}

/// Hardened v0.3 SearchPlane Builder:
/// - Unsaturated u16 impacts + inline df=1 terms + dense-term Roaring overlay (df >= 50K)
/// - No `time_desc.bin` (relies on `(ct, published_epoch DESC)` doc_id ordering + `ct_ranges`)
/// - Per-entity episode keys + rolling daily `created_day_bms` + `strict_title_bms` + `family_title_bms`
/// - Optional trigram substring index (`pg_trgm` parity)
pub struct CustomSearchBuilder {
    generation_id: u64,
    term_seed: u64,
    num_docs: u32,
    postings: AHashMap<u64, Vec<(u32, u16)>>,
    buffered_postings: usize,
    run_paths: Vec<PathBuf>,
    run_dir: PathBuf,
    pub ct_ranges: [(u32, u32); NUM_CONTENT_TYPES],
    pub entity_to_docs: AHashMap<u32, RoaringBitmap>,
    pub ep_to_docs: AHashMap<u64, RoaringBitmap>,
    pub strict_title_to_docs: AHashMap<u64, RoaringBitmap>,
    pub family_title_to_docs: AHashMap<u64, RoaringBitmap>,
    pub created_day_to_docs: AHashMap<u32, RoaringBitmap>,
    pub trigram_to_docs: AHashMap<u64, RoaringBitmap>,
    pub facet_ct: [RoaringBitmap; NUM_CONTENT_TYPES],
    pub facet_res: [RoaringBitmap; 10],
    pub facet_codec: [RoaringBitmap; 10],
    pub facet_src: [RoaringBitmap; 10],
    pub facet_anime: RoaringBitmap,
    pub facet_3d: RoaringBitmap,
    pub facet_eng_audio: RoaringBitmap,
    pub facet_linked: RoaringBitmap,
    pub index_trigrams: bool,
}

impl CustomSearchBuilder {
    pub fn new(
        generation_id: u64,
        term_seed: u64,
        num_docs: u32,
        run_dir: &Path,
        index_trigrams: bool,
    ) -> Self {
        Self {
            generation_id,
            term_seed,
            num_docs,
            postings: AHashMap::with_capacity(200_000),
            buffered_postings: 0,
            run_paths: Vec::new(),
            run_dir: run_dir.to_path_buf(),
            ct_ranges: [(0, 0); NUM_CONTENT_TYPES],
            entity_to_docs: AHashMap::with_capacity(64_000),
            ep_to_docs: AHashMap::with_capacity(64_000),
            strict_title_to_docs: AHashMap::with_capacity(200_000),
            family_title_to_docs: AHashMap::with_capacity(200_000),
            created_day_to_docs: AHashMap::with_capacity(4096),
            trigram_to_docs: AHashMap::with_capacity(if index_trigrams { 65_536 } else { 0 }),
            facet_ct: std::array::from_fn(|_| RoaringBitmap::new()),
            facet_res: std::array::from_fn(|_| RoaringBitmap::new()),
            facet_codec: std::array::from_fn(|_| RoaringBitmap::new()),
            facet_src: std::array::from_fn(|_| RoaringBitmap::new()),
            facet_anime: RoaringBitmap::new(),
            facet_3d: RoaringBitmap::new(),
            facet_eng_audio: RoaringBitmap::new(),
            facet_linked: RoaringBitmap::new(),
            index_trigrams,
        }
    }

    pub fn flush_run_if_needed(&mut self) -> Result<(), FormatError> {
        if self.buffered_postings < POSTING_FLUSH_THRESHOLD {
            return Ok(());
        }
        self.flush_posting_run()
    }

    fn flush_posting_run(&mut self) -> Result<(), FormatError> {
        if self.postings.is_empty() {
            return Ok(());
        }
        std::fs::create_dir_all(&self.run_dir)?;
        let run_path = self
            .run_dir
            .join(format!("_post_run_{}.bin", self.run_paths.len()));
        let mut keys: Vec<u64> = self.postings.keys().copied().collect();
        keys.sort_unstable();
        let mut w = BufWriter::with_capacity(4 * 1024 * 1024, File::create(&run_path)?);
        for th in keys {
            if let Some(list) = self.postings.remove(&th) {
                w.write_u64::<LittleEndian>(th)?;
                w.write_u32::<LittleEndian>(list.len() as u32)?;
                for (d, imp) in list {
                    w.write_u32::<LittleEndian>(d)?;
                    w.write_u16::<LittleEndian>(imp)?;
                }
            }
        }
        w.flush()?;
        self.postings.clear();
        self.postings.shrink_to(100_000);
        self.buffered_postings = 0;
        self.run_paths.push(run_path);
        Ok(())
    }

    /// Unsaturated BM25F-weighted u16 impact indexing (range 1..=65535, zero 255 saturation!).
    pub fn index_doc_terms(
        &mut self,
        doc_id: u32,
        title_tokens: &[String],
        entity_tokens: &[String],
        raw_tokens: &[String],
        file_tokens: &[String],
    ) {
        let mut local: AHashMap<u64, u32> = AHashMap::with_capacity(24);
        let title_len = title_tokens.len().max(1) as f32;
        let norm = 1.0 / (0.35 + 0.65 * (title_len / 4.0).min(4.0));

        for t in title_tokens {
            if t.is_empty() {
                continue;
            }
            let h = term_hash(t, self.term_seed);
            let e = local.entry(h).or_insert(0);
            *e = e.saturating_add((300.0 * norm) as u32);
        }
        for t in entity_tokens {
            if t.is_empty() {
                continue;
            }
            let h = term_hash(t, self.term_seed);
            let e = local.entry(h).or_insert(0);
            *e = e.saturating_add(250);
        }
        for t in raw_tokens {
            if t.is_empty() {
                continue;
            }
            let h = term_hash(t, self.term_seed);
            let e = local.entry(h).or_insert(0);
            *e = e.saturating_add(100);
        }
        for t in file_tokens {
            if t.is_empty() {
                continue;
            }
            let h = term_hash(t, self.term_seed);
            let e = local.entry(h).or_insert(0);
            *e = e.saturating_add(40);
        }

        // BM25-style TF saturation mapped smoothly across u16 (1..=60000) — never saturates at 255!
        let k1 = 180.0f32;
        for (th, raw_imp) in local {
            let tf = raw_imp as f32;
            let imp = ((tf * (k1 + 1.0)) / (tf + k1) * (60_000.0 / (k1 + 1.0))).clamp(1.0, 60_000.0)
                as u16;
            self.postings.entry(th).or_default().push((doc_id, imp));
            self.buffered_postings += 1;
        }
        if doc_id + 1 > self.num_docs {
            self.num_docs = doc_id + 1;
        }
    }

    pub fn index_doc_episodes(&mut self, doc_id: u32, entity_id: u32, episodes: &[EpisodeSpec]) {
        for &ep in episodes {
            match ep {
                EpisodeSpec::WholeSeason(s) => {
                    self.ep_to_docs
                        .entry(ep_key_global_whole_season(s))
                        .or_default()
                        .insert(doc_id);
                    self.ep_to_docs
                        .entry(ep_key_global_any_season(s))
                        .or_default()
                        .insert(doc_id);
                    if entity_id != 0 {
                        self.ep_to_docs
                            .entry(ep_key_entity_whole_season(entity_id, s))
                            .or_default()
                            .insert(doc_id);
                        self.ep_to_docs
                            .entry(ep_key_entity_any_season(entity_id, s))
                            .or_default()
                            .insert(doc_id);
                    }
                }
                EpisodeSpec::Episode(s, e) => {
                    self.ep_to_docs
                        .entry(ep_key_global_episode(s, e))
                        .or_default()
                        .insert(doc_id);
                    self.ep_to_docs
                        .entry(ep_key_global_any_season(s))
                        .or_default()
                        .insert(doc_id);
                    if entity_id != 0 {
                        self.ep_to_docs
                            .entry(ep_key_entity_episode(entity_id, s, e))
                            .or_default()
                            .insert(doc_id);
                        self.ep_to_docs
                            .entry(ep_key_entity_any_season(entity_id, s))
                            .or_default()
                            .insert(doc_id);
                    }
                }
            }
        }
    }

    pub fn build_and_save(
        mut self,
        dir: &Path,
        num_docs: u32,
    ) -> Result<CustomSearchBuildStats, FormatError> {
        self.num_docs = num_docs;
        let n_docs_f = num_docs.max(1) as f32;

        // 1. Build inverted.bin + dense_terms.mmap (df >= DENSE_TERM_DF_THRESHOLD)
        let mut dense_terms: Vec<(u64, RoaringBitmap)> = Vec::new();
        let bdir_tmp = dir.join("_inv_bdir.tmp");
        let data_tmp = dir.join("_inv_data.tmp");
        let mut bw = BufWriter::with_capacity(4 * 1024 * 1024, File::create(&bdir_tmp)?);
        let mut dw = BufWriter::with_capacity(4 * 1024 * 1024, File::create(&data_tmp)?);
        let mut bdir_off: u64 = 0;
        let mut data_off: u64 = 0;
        let mut blk_buf: Vec<u8> = Vec::with_capacity(1024);
        let mut terms: Vec<TermDirEntry> = Vec::new();

        if self.run_paths.is_empty() {
            let mut keys: Vec<u64> = self.postings.keys().copied().collect();
            keys.sort_unstable();
            terms.reserve(keys.len());
            for th in keys {
                let list = self.postings.remove(&th).unwrap();
                if list.is_empty() {
                    continue;
                }
                Self::emit_term(
                    th,
                    &list,
                    n_docs_f,
                    &mut terms,
                    &mut dense_terms,
                    &mut bw,
                    &mut dw,
                    &mut bdir_off,
                    &mut data_off,
                    &mut blk_buf,
                )?;
            }
        } else {
            self.flush_posting_run()?;
            let mut readers: Vec<RunReader> = Vec::with_capacity(self.run_paths.len());
            for rp in &self.run_paths {
                readers.push(RunReader::open(rp)?);
            }
            let mut merged: Vec<(u32, u16)> = Vec::new();
            loop {
                let mut min_th: Option<u64> = None;
                for rr in &readers {
                    if let Some((th, _)) = rr.cur_term {
                        min_th = Some(match min_th {
                            Some(m) => m.min(th),
                            None => th,
                        });
                    }
                }
                let Some(th) = min_th else { break };
                merged.clear();
                for rr in &mut readers {
                    if let Some((rth, _)) = rr.cur_term {
                        if rth == th {
                            let (_, v) = rr.cur_term.take().unwrap();
                            merged.extend_from_slice(&v);
                            rr.advance()?;
                        }
                    }
                }
                if !merged.is_empty() {
                    Self::emit_term(
                        th,
                        &merged,
                        n_docs_f,
                        &mut terms,
                        &mut dense_terms,
                        &mut bw,
                        &mut dw,
                        &mut bdir_off,
                        &mut data_off,
                        &mut blk_buf,
                    )?;
                }
            }
            drop(readers);
            for rp in &self.run_paths {
                let _ = std::fs::remove_file(rp);
            }
        }
        bw.flush()?;
        dw.flush()?;
        drop(bw);
        drop(dw);

        let num_terms = terms.len();
        let num_dense_terms = dense_terms.len();
        let inverted_bytes = Self::assemble_inverted(
            dir,
            self.generation_id,
            self.term_seed,
            &terms,
            &bdir_tmp,
            &data_tmp,
            bdir_off,
            data_off,
        )?;
        let dense_terms_bytes = MmapBlobMap::build_from_bitmaps(
            &dir.join("dense_terms.mmap"),
            self.generation_id,
            dense_terms,
        )?;

        // 2. Persist static facets + ct_ranges (NO time_desc.bin!)
        let mut fbuf = Vec::with_capacity(2 * 1024 * 1024);
        for &(s, e) in &self.ct_ranges {
            fbuf.extend_from_slice(&s.to_le_bytes());
            fbuf.extend_from_slice(&e.to_le_bytes());
        }
        let mut tmp_bm = Vec::new();
        let push_bm =
            |out: &mut Vec<u8>, tmp: &mut Vec<u8>, bm: &RoaringBitmap| -> Result<(), FormatError> {
                tmp.clear();
                bm.serialize_into(&mut *tmp)?;
                write_varint_u32(out, tmp.len() as u32);
                out.extend_from_slice(tmp);
                Ok(())
            };
        for bm in &self.facet_ct {
            push_bm(&mut fbuf, &mut tmp_bm, bm)?;
        }
        for bm in &self.facet_res {
            push_bm(&mut fbuf, &mut tmp_bm, bm)?;
        }
        for bm in &self.facet_codec[..8] {
            push_bm(&mut fbuf, &mut tmp_bm, bm)?;
        }
        for bm in &self.facet_src[..8] {
            push_bm(&mut fbuf, &mut tmp_bm, bm)?;
        }
        push_bm(&mut fbuf, &mut tmp_bm, &self.facet_anime)?;
        push_bm(&mut fbuf, &mut tmp_bm, &self.facet_eng_audio)?;
        push_bm(&mut fbuf, &mut tmp_bm, &self.facet_linked)?;
        // Backward-compatible trailing extension for facet_3d + extended codec/source codes (8..10).
        fbuf.extend_from_slice(b"FEX2");
        push_bm(&mut fbuf, &mut tmp_bm, &self.facet_3d)?;
        push_bm(&mut fbuf, &mut tmp_bm, &self.facet_codec[8])?;
        push_bm(&mut fbuf, &mut tmp_bm, &self.facet_codec[9])?;
        push_bm(&mut fbuf, &mut tmp_bm, &self.facet_src[8])?;
        push_bm(&mut fbuf, &mut tmp_bm, &self.facet_src[9])?;
        let mut struct_bms_bytes = write_checked_bytes(
            &dir.join("facets_static.bin"),
            FileKind::FacetsStatic,
            self.generation_id,
            self.num_docs as u64,
            self.term_seed,
            0,
            &fbuf,
        )?;

        // 3. Persist MmapBlobMaps with inline singleton optimization
        let ent_items: Vec<(u64, RoaringBitmap)> = self
            .entity_to_docs
            .into_iter()
            .map(|(k, bm)| (k as u64, bm))
            .collect();
        struct_bms_bytes += MmapBlobMap::build_from_bitmaps(
            &dir.join("entity_bms.mmap"),
            self.generation_id,
            ent_items,
        )?;

        let ep_items: Vec<(u64, RoaringBitmap)> = self.ep_to_docs.into_iter().collect();
        struct_bms_bytes += MmapBlobMap::build_from_bitmaps(
            &dir.join("ep_bms.mmap"),
            self.generation_id,
            ep_items,
        )?;

        let sth_items: Vec<(u64, RoaringBitmap)> = self.strict_title_to_docs.into_iter().collect();
        struct_bms_bytes += MmapBlobMap::build_from_bitmaps(
            &dir.join("strict_title_bms.mmap"),
            self.generation_id,
            sth_items,
        )?;

        let fam_items: Vec<(u64, RoaringBitmap)> = self.family_title_to_docs.into_iter().collect();
        struct_bms_bytes += MmapBlobMap::build_from_bitmaps(
            &dir.join("family_title_bms.mmap"),
            self.generation_id,
            fam_items,
        )?;

        let day_items: Vec<(u64, RoaringBitmap)> = self
            .created_day_to_docs
            .into_iter()
            .map(|(d, bm)| (d as u64, bm))
            .collect();
        struct_bms_bytes += MmapBlobMap::build_from_bitmaps(
            &dir.join("created_day_bms.mmap"),
            self.generation_id,
            day_items,
        )?;

        let tri_items: Vec<(u64, RoaringBitmap)> = self.trigram_to_docs.into_iter().collect();
        let trigrams_bytes = MmapBlobMap::build_from_bitmaps(
            &dir.join("trigrams.mmap"),
            self.generation_id,
            tri_items,
        )?;

        Ok(CustomSearchBuildStats {
            inverted_bytes,
            dense_terms_bytes,
            struct_bms_bytes,
            trigrams_bytes,
            num_terms,
            num_dense_terms,
        })
    }

    #[allow(clippy::too_many_arguments)]
    fn emit_term(
        th: u64,
        list: &[(u32, u16)],
        n_docs_f: f32,
        terms: &mut Vec<TermDirEntry>,
        dense_terms: &mut Vec<(u64, RoaringBitmap)>,
        bw: &mut BufWriter<File>,
        dw: &mut BufWriter<File>,
        bdir_off: &mut u64,
        data_off: &mut u64,
        blk_buf: &mut Vec<u8>,
    ) -> Result<(), FormatError> {
        let df = list.len() as u32;
        let idf = ((n_docs_f - df as f32 + 0.5) / (df as f32 + 0.5) + 1.0)
            .ln()
            .max(0.01);

        // Inline singleton term (df == 1): 0 extra bytes in block_dir or data!
        if list.len() == 1 {
            let (doc_id, imp) = list[0];
            terms.push(TermDirEntry {
                term_hash: th,
                block_dir_offset: (doc_id as u64) | ((imp as u64) << 32),
                df: 1,
                idf_bits: idf.to_bits(),
                num_blocks: 0,
                flags_max_imp: (imp as u32) | (TERM_FLAG_INLINE_SINGLE << 16),
            });
            return Ok(());
        }

        // Dense-term Roaring overlay for ultra-fast cold facet_counts!
        if list.len() >= DENSE_TERM_DF_THRESHOLD {
            let mut bm = RoaringBitmap::new();
            for &(d, _) in list {
                bm.insert(d);
            }
            dense_terms.push((th, bm));
        }

        let start_bdir_off = *bdir_off;
        let mut num_blocks = 0u32;
        let mut term_max_imp = 0u16;

        for chunk in list.chunks(WAND_BLOCK_SIZE) {
            let first_doc = chunk[0].0;
            let last_doc = chunk[chunk.len() - 1].0;
            let mut max_imp = 0u16;
            let blk_data_off = *data_off;
            blk_buf.clear();
            let mut prev = 0u32;
            for &(doc_id, imp) in chunk {
                if imp > max_imp {
                    max_imp = imp;
                }
                let delta = doc_id.saturating_sub(prev);
                prev = doc_id;
                write_varint_u32(blk_buf, delta);
                write_varint_u32(blk_buf, imp as u32);
            }
            if max_imp > term_max_imp {
                term_max_imp = max_imp;
            }
            dw.write_all(blk_buf)?;
            let data_len = blk_buf.len() as u32;
            *data_off += data_len as u64;

            let bm = BlockMeta {
                first_doc,
                last_doc,
                max_impact: max_imp,
                count: chunk.len() as u16,
                data_len,
                data_offset: blk_data_off,
            };
            let bm_bytes = unsafe {
                std::slice::from_raw_parts(
                    &bm as *const BlockMeta as *const u8,
                    std::mem::size_of::<BlockMeta>(),
                )
            };
            bw.write_all(bm_bytes)?;
            *bdir_off += std::mem::size_of::<BlockMeta>() as u64;
            num_blocks += 1;
        }
        terms.push(TermDirEntry {
            term_hash: th,
            block_dir_offset: start_bdir_off,
            df,
            idf_bits: idf.to_bits(),
            num_blocks,
            flags_max_imp: term_max_imp as u32,
        });
        Ok(())
    }

    #[allow(clippy::too_many_arguments)]
    fn assemble_inverted(
        dir: &Path,
        generation_id: u64,
        term_seed: u64,
        terms: &[TermDirEntry],
        bdir_tmp: &Path,
        data_tmp: &Path,
        bdir_len: u64,
        data_len: u64,
    ) -> Result<u64, FormatError> {
        let mut w = CheckedFileWriter::create(
            &dir.join("inverted.bin"),
            FileKind::Inverted,
            generation_id,
        )?;
        // Sub-header: 24 bytes (aligned to 8; total offset 96+24 = 120, multiple of 8 for TermDirEntry/BlockMeta!)
        let mut sub = [0u8; 24];
        sub[0..8].copy_from_slice(&(terms.len() as u64).to_le_bytes());
        sub[8..16].copy_from_slice(&bdir_len.to_le_bytes());
        sub[16..24].copy_from_slice(&data_len.to_le_bytes());
        w.write_payload(&sub)?;
        let tbytes = unsafe {
            std::slice::from_raw_parts(terms.as_ptr() as *const u8, std::mem::size_of_val(terms))
        };
        w.write_payload(tbytes)?;

        // Copy bdir_tmp and data_tmp in streaming 4 MiB chunks
        let mut copy_buf = vec![0u8; 4 * 1024 * 1024];
        for tmp in [bdir_tmp, data_tmp] {
            let mut f = File::open(tmp)?;
            loop {
                let n = f.read(&mut copy_buf)?;
                if n == 0 {
                    break;
                }
                w.write_payload(&copy_buf[..n])?;
            }
            let _ = std::fs::remove_file(tmp);
        }
        w.finish(terms.len() as u64, term_seed, 0)
    }
}

/// Hardened Candidate B (`DH-TMS Custom v0.3`) Search Engine.
pub struct CustomSearchPlane {
    pub generation_id: u64,
    pub term_seed: u64,
    inv_cm: CheckedMmap,
    n_terms: usize,
    block_dir_base: usize,
    block_dir_len: usize,
    data_base: usize,
    pub ct_ranges: [(u32, u32); NUM_CONTENT_TYPES],
    pub dense_terms: MmapBlobMap,
    pub entity_bms: MmapBlobMap,
    pub ep_bms: MmapBlobMap,
    pub strict_title_bms: MmapBlobMap,
    pub family_title_bms: MmapBlobMap,
    pub created_day_bms: MmapBlobMap,
    pub trigrams: MmapBlobMap,
    pub facet_ct: [RoaringBitmap; NUM_CONTENT_TYPES],
    pub facet_res: [RoaringBitmap; 10],
    pub facet_codec: [RoaringBitmap; 10],
    pub facet_src: [RoaringBitmap; 10],
    pub facet_anime: RoaringBitmap,
    pub facet_3d: RoaringBitmap,
    pub facet_eng_audio: RoaringBitmap,
    pub facet_linked: RoaringBitmap,
    pub l0: L0Overlay,
}

impl CustomSearchPlane {
    pub fn open(
        gen_dir: &Path,
        l0_wal_path: &Path,
        expected_gen: Option<u64>,
    ) -> Result<Self, FormatError> {
        let inv_cm = CheckedMmap::open(
            &gen_dir.join("inverted.bin"),
            FileKind::Inverted,
            expected_gen,
            false,
        )?;
        let generation_id = inv_cm.header().generation_id;
        let term_seed = inv_cm.header().aux_u64;
        let p = inv_cm.payload();
        if p.len() < 24 {
            return Err(FormatError::CorruptPayload(
                "inverted.bin sub-header truncated".into(),
            ));
        }
        let n_terms = u64::from_le_bytes(p[0..8].try_into().unwrap()) as usize;
        let block_dir_len = u64::from_le_bytes(p[8..16].try_into().unwrap()) as usize;
        let data_len = u64::from_le_bytes(p[16..24].try_into().unwrap()) as usize;
        let terms_bytes = n_terms
            .checked_mul(std::mem::size_of::<TermDirEntry>())
            .ok_or_else(|| FormatError::CorruptPayload("n_terms overflow".into()))?;
        let block_dir_base = 24usize
            .checked_add(terms_bytes)
            .ok_or_else(|| FormatError::CorruptPayload("block_dir_base overflow".into()))?;
        let data_base = block_dir_base
            .checked_add(block_dir_len)
            .ok_or_else(|| FormatError::CorruptPayload("data_base overflow".into()))?;
        let total = data_base
            .checked_add(data_len)
            .ok_or_else(|| FormatError::CorruptPayload("inverted total overflow".into()))?;
        if total != p.len() {
            return Err(FormatError::CorruptPayload(format!(
                "inverted.bin payload len mismatch: declared {total}, actual {}",
                p.len()
            )));
        }

        let dense_terms =
            MmapBlobMap::open(&gen_dir.join("dense_terms.mmap"), Some(generation_id))?;
        let entity_bms = MmapBlobMap::open(&gen_dir.join("entity_bms.mmap"), Some(generation_id))?;
        let ep_bms = MmapBlobMap::open(&gen_dir.join("ep_bms.mmap"), Some(generation_id))?;
        let strict_title_bms =
            MmapBlobMap::open(&gen_dir.join("strict_title_bms.mmap"), Some(generation_id))?;
        let family_title_bms =
            MmapBlobMap::open(&gen_dir.join("family_title_bms.mmap"), Some(generation_id))?;
        let created_day_bms =
            MmapBlobMap::open(&gen_dir.join("created_day_bms.mmap"), Some(generation_id))?;
        let trigrams = MmapBlobMap::open(&gen_dir.join("trigrams.mmap"), Some(generation_id))?;

        let fcm = CheckedMmap::open(
            &gen_dir.join("facets_static.bin"),
            FileKind::FacetsStatic,
            Some(generation_id),
            true,
        )?;
        let fp = fcm.payload();
        let mut pos = 0usize;
        let mut ct_ranges = [(0u32, 0u32); NUM_CONTENT_TYPES];
        if fp.len() < NUM_CONTENT_TYPES * 8 {
            return Err(FormatError::CorruptPayload(
                "facets_static ct_ranges truncated".into(),
            ));
        }
        for r in &mut ct_ranges {
            let s = u32::from_le_bytes(fp[pos..pos + 4].try_into().unwrap());
            let e = u32::from_le_bytes(fp[pos + 4..pos + 8].try_into().unwrap());
            *r = (s, e);
            pos += 8;
        }
        let read_bm = |cursor: &mut usize| -> Result<RoaringBitmap, FormatError> {
            let len = read_varint_u32(fp, cursor)? as usize;
            let end = cursor
                .checked_add(len)
                .ok_or_else(|| FormatError::CorruptPayload("facet bm overflow".into()))?;
            if end > fp.len() {
                return Err(FormatError::CorruptPayload("facet bm truncated".into()));
            }
            let bm = RoaringBitmap::deserialize_from(&fp[*cursor..end])
                .map_err(|e| FormatError::CorruptPayload(format!("corrupt facet bm: {e}")))?;
            *cursor = end;
            Ok(bm)
        };
        let mut facet_ct: [RoaringBitmap; NUM_CONTENT_TYPES] =
            std::array::from_fn(|_| RoaringBitmap::new());
        for b in &mut facet_ct {
            *b = read_bm(&mut pos)?;
        }
        let mut facet_res: [RoaringBitmap; 10] = std::array::from_fn(|_| RoaringBitmap::new());
        for b in &mut facet_res {
            *b = read_bm(&mut pos)?;
        }
        let mut facet_codec: [RoaringBitmap; 10] = std::array::from_fn(|_| RoaringBitmap::new());
        for b in &mut facet_codec[..8] {
            *b = read_bm(&mut pos)?;
        }
        let mut facet_src: [RoaringBitmap; 10] = std::array::from_fn(|_| RoaringBitmap::new());
        for b in &mut facet_src[..8] {
            *b = read_bm(&mut pos)?;
        }
        let facet_anime = read_bm(&mut pos)?;
        let facet_eng_audio = read_bm(&mut pos)?;
        let facet_linked = read_bm(&mut pos)?;
        let mut facet_3d = RoaringBitmap::new();
        if pos + 4 <= fp.len() && &fp[pos..pos + 4] == b"FEX2" {
            pos += 4;
            facet_3d = read_bm(&mut pos)?;
            facet_codec[8] = read_bm(&mut pos)?;
            facet_codec[9] = read_bm(&mut pos)?;
            facet_src[8] = read_bm(&mut pos)?;
            facet_src[9] = read_bm(&mut pos)?;
        }

        let l0 = L0Overlay::open(l0_wal_path, generation_id, term_seed)?;

        Ok(Self {
            generation_id,
            term_seed,
            inv_cm,
            n_terms,
            block_dir_base,
            block_dir_len,
            data_base,
            ct_ranges,
            dense_terms,
            entity_bms,
            ep_bms,
            strict_title_bms,
            family_title_bms,
            created_day_bms,
            trigrams,
            facet_ct,
            facet_res,
            facet_codec,
            facet_src,
            facet_anime,
            facet_3d,
            facet_eng_audio,
            facet_linked,
            l0,
        })
    }

    #[inline]
    fn term_entries(&self) -> &[TermDirEntry] {
        if self.n_terms == 0 {
            return &[];
        }
        // SAFETY: Validated length & 8-byte alignment (96 + 24 = 120 % 8 == 0) in open().
        unsafe {
            std::slice::from_raw_parts(
                self.inv_cm.payload()[24..].as_ptr() as *const TermDirEntry,
                self.n_terms,
            )
        }
    }

    #[inline]
    fn find_term(&self, th: u64) -> Option<TermDirEntry> {
        let ents = self.term_entries();
        let idx = ents.binary_search_by_key(&th, |e| e.term_hash).ok()?;
        Some(ents[idx])
    }

    #[inline]
    fn read_block_meta(
        &self,
        entry: &TermDirEntry,
        blk_idx: u32,
    ) -> Result<BlockMeta, FormatError> {
        let off =
            entry.block_dir_offset as usize + (blk_idx as usize) * std::mem::size_of::<BlockMeta>();
        let end = off + std::mem::size_of::<BlockMeta>();
        if end > self.block_dir_len {
            return Err(FormatError::CorruptPayload(
                "block_meta out of bounds".into(),
            ));
        }
        let s = &self.inv_cm.payload()[self.block_dir_base + off..self.block_dir_base + end];
        Ok(BlockMeta {
            first_doc: u32::from_le_bytes(s[0..4].try_into().unwrap()),
            last_doc: u32::from_le_bytes(s[4..8].try_into().unwrap()),
            max_impact: u16::from_le_bytes(s[8..10].try_into().unwrap()),
            count: u16::from_le_bytes(s[10..12].try_into().unwrap()),
            data_len: u32::from_le_bytes(s[12..16].try_into().unwrap()),
            data_offset: u64::from_le_bytes(s[16..24].try_into().unwrap()),
        })
    }

    fn decode_block(&self, bm: &BlockMeta, out: &mut Vec<(u32, u16)>) -> Result<(), FormatError> {
        out.clear();
        let start = self.data_base + bm.data_offset as usize;
        let end = start
            .checked_add(bm.data_len as usize)
            .ok_or_else(|| FormatError::CorruptPayload("posting block len overflow".into()))?;
        let payload = self.inv_cm.payload();
        if end > payload.len() {
            return Err(FormatError::CorruptPayload(
                "posting block out of bounds".into(),
            ));
        }
        let slice = &payload[start..end];
        let mut pos = 0usize;
        let mut doc = 0u32;
        for _ in 0..bm.count {
            let delta = read_varint_u32(slice, &mut pos)?;
            doc = doc
                .checked_add(delta)
                .ok_or_else(|| FormatError::CorruptPayload("posting doc_id overflow".into()))?;
            let imp = read_varint_u32(slice, &mut pos)? as u16;
            out.push((doc, imp));
        }
        Ok(())
    }

    fn find_block_ge(
        &self,
        entry: &TermDirEntry,
        start_blk: u32,
        target: u32,
    ) -> Result<u32, FormatError> {
        let mut lo = start_blk;
        let mut hi = entry.num_blocks;
        while lo < hi {
            let mid = lo + (hi - lo) / 2;
            let bm = self.read_block_meta(entry, mid)?;
            if bm.last_doc < target {
                lo = mid + 1;
            } else {
                hi = mid;
            }
        }
        Ok(lo)
    }

    /// Evaluate a disjunction of structured `SearchFilterClause` filters against base static facet bitmaps.
    pub fn build_filter_bitmap(
        &self,
        clauses: &[SearchFilterClause],
        base_mask: &RoaringBitmap,
    ) -> RoaringBitmap {
        let mut union = RoaringBitmap::new();
        for c in clauses {
            let mut bm = if let Some(ct) = c.content_type {
                if (ct as usize) < NUM_CONTENT_TYPES {
                    base_mask & &self.facet_ct[ct as usize]
                } else {
                    continue;
                }
            } else {
                base_mask.clone()
            };
            if bm.is_empty() {
                continue;
            }
            if !c.resolutions.is_empty() {
                let mut r_union = RoaringBitmap::new();
                for &r in &c.resolutions {
                    if (r as usize) < self.facet_res.len() {
                        r_union |= &self.facet_res[r as usize];
                    }
                }
                bm &= &r_union;
                if bm.is_empty() {
                    continue;
                }
            }
            if !c.exclude_resolutions.is_empty() {
                for &r in &c.exclude_resolutions {
                    if (r as usize) < self.facet_res.len() {
                        bm -= &self.facet_res[r as usize];
                    }
                }
                if bm.is_empty() {
                    continue;
                }
            }
            if c.require_anime {
                bm &= &self.facet_anime;
                if bm.is_empty() {
                    continue;
                }
            }
            if c.require_3d {
                bm &= &self.facet_3d;
                if bm.is_empty() {
                    continue;
                }
            }
            if let Some(src) = c.video_source {
                if src == 0 || (src as usize) >= self.facet_src.len() {
                    continue;
                }
                bm &= &self.facet_src[src as usize];
                if bm.is_empty() {
                    continue;
                }
            }
            if let Some(codec) = c.video_codec {
                if codec == 0 || (codec as usize) >= self.facet_codec.len() {
                    continue;
                }
                bm &= &self.facet_codec[codec as usize];
                if bm.is_empty() {
                    continue;
                }
            }
            union |= bm;
        }
        union
    }

    /// Unified search supporting `QueryMode::And`, `QueryMode::Or` (true Block-Max WAND pivot advancement!),
    /// and `QueryMode::Phrase`, with mandatory `PolicyPlane` egress enforcement on every returned row!
    #[allow(clippy::too_many_arguments)]
    pub fn search_text(
        &self,
        query_str: &str,
        mode: QueryMode,
        extra_filter: Option<&RoaringBitmap>,
        policy: &PolicyPlane,
        tier: PolicyTier,
        core: &CorePlane,
        top_k: usize,
        now_epoch_sec: u32,
        now_ms: u64,
    ) -> Result<Vec<ScoredDoc>, SearchError> {
        self.search_text_filtered(
            query_str,
            mode,
            extra_filter,
            None,
            None,
            policy,
            tier,
            core,
            top_k,
            now_epoch_sec,
            now_ms,
        )
    }

    #[allow(clippy::too_many_arguments)]
    pub fn search_text_filtered(
        &self,
        query_str: &str,
        mode: QueryMode,
        extra_filter: Option<&RoaringBitmap>,
        l0_pred: Option<&dyn Fn(&l0::L0Doc) -> bool>,
        doc_pred: Option<&dyn Fn(u32) -> bool>,
        policy: &PolicyPlane,
        tier: PolicyTier,
        core: &CorePlane,
        top_k: usize,
        now_epoch_sec: u32,
        now_ms: u64,
    ) -> Result<Vec<ScoredDoc>, SearchError> {
        policy.lease().check(now_ms)?;
        let raw_toks = tokenize_alnum(query_str);
        if raw_toks.is_empty() || top_k == 0 {
            return Ok(Vec::new());
        }
        let mut dedup_toks = raw_toks.clone();
        dedup_toks.sort();
        dedup_toks.dedup();

        let snap = policy.snapshot();
        let base_mask = snap.bitmap_for(tier);
        let effective_mask_owned;
        let mask: &RoaringBitmap = match extra_filter {
            Some(f) => {
                effective_mask_owned = base_mask & f;
                &effective_mask_owned
            }
            None => base_mask,
        };
        if mask.is_empty() && !self.l0.has_entries() {
            return Ok(Vec::new());
        }

        let mut term_entries: Vec<TermDirEntry> = Vec::with_capacity(dedup_toks.len());
        let mut term_hashes: Vec<u64> = Vec::with_capacity(dedup_toks.len());
        for t in &dedup_toks {
            let th = term_hash(t, self.term_seed);
            term_hashes.push(th);
            match self.find_term(th) {
                Some(e) => term_entries.push(e),
                None => {
                    if matches!(mode, QueryMode::And | QueryMode::Phrase) {
                        // Still check L0 in case a brand-new live doc introduced this term
                        if !self.l0.has_entries() {
                            return Ok(Vec::new());
                        }
                    }
                }
            }
        }

        // For Phrase mode, collect slightly wider conjunctive candidates then verify exact ordered phrase on Name column
        let internal_k = if mode == QueryMode::Phrase {
            (top_k * 8).max(256)
        } else {
            top_k
        };

        let mut res = match mode {
            QueryMode::And | QueryMode::Phrase => {
                if term_entries.len() == dedup_toks.len() && !mask.is_empty() {
                    self.execute_and_bmw(
                        &term_entries,
                        mask,
                        doc_pred,
                        policy,
                        tier,
                        now_epoch_sec,
                        internal_k,
                    )?
                } else {
                    Vec::new()
                }
            }
            QueryMode::Or => {
                if !term_entries.is_empty() && !mask.is_empty() {
                    self.execute_or_bmw(
                        &term_entries,
                        mask,
                        doc_pred,
                        policy,
                        tier,
                        now_epoch_sec,
                        internal_k,
                    )?
                } else {
                    Vec::new()
                }
            }
        };

        // Merge L0 live delta segment (deduplicating superseded base docs!)
        if self.l0.has_entries() {
            res.retain(|sd| !self.l0.is_superseded(sd.doc_id));
            let l0_hits = self.l0.search_terms(&term_hashes, mode != QueryMode::Or);
            if !l0_hits.is_empty() {
                let mut seen: HashSet<u32> = res.iter().map(|s| s.doc_id).collect();
                for (doc_id, score) in l0_hits {
                    let l0_ok = if let Some(pred) = l0_pred {
                        self.l0.get_doc(doc_id).is_some_and(|ov| pred(&ov))
                    } else {
                        extra_filter.is_none_or(|f| f.contains(doc_id))
                    };
                    if seen.insert(doc_id)
                        && policy.authorize_egress_one(doc_id, tier, now_epoch_sec, now_ms)?
                        && l0_ok
                        && doc_pred.is_none_or(|p| p(doc_id))
                    {
                        res.push(ScoredDoc { doc_id, score });
                    }
                }
                res.sort_unstable_by(|a, b| {
                    b.score
                        .total_cmp(&a.score)
                        .then_with(|| a.doc_id.cmp(&b.doc_id))
                });
                res.truncate(internal_k);
            }
        }

        // Exact ordered phrase verification against seekable Name column (ZERO tree decompression!)
        if mode == QueryMode::Phrase && raw_toks.len() >= 2 {
            let mut filtered = Vec::with_capacity(top_k);
            for sd in res {
                let name_owned = if let Some(ov) = self.l0.get_doc(sd.doc_id) {
                    ov.name
                } else {
                    core.get_name(sd.doc_id)?.to_owned()
                };
                let ntoks = tokenize_alnum(&name_owned);
                if contains_subslice(&ntoks, &raw_toks) {
                    filtered.push(sd);
                    if filtered.len() >= top_k {
                        break;
                    }
                }
            }
            res = filtered;
        }

        // Mandatory final response egress gate (catches any revocation that occurred mid-search!)
        policy.lease().check(now_ms)?;
        res.retain(|sd| {
            policy
                .authorize_egress_one(sd.doc_id, tier, now_epoch_sec, now_ms)
                .unwrap_or(false)
        });
        res.truncate(top_k);
        Ok(res)
    }

    /// Conjunctive AND block-max pruned scan with exact tie-breaking (`score DESC, doc_id ASC`).
    #[allow(clippy::too_many_arguments)]
    fn execute_and_bmw(
        &self,
        entries: &[TermDirEntry],
        mask: &RoaringBitmap,
        doc_pred: Option<&dyn Fn(u32) -> bool>,
        policy: &PolicyPlane,
        tier: PolicyTier,
        now_epoch_sec: u32,
        top_k: usize,
    ) -> Result<Vec<ScoredDoc>, FormatError> {
        let mut sorted = entries.to_vec();
        sorted.sort_unstable_by_key(|e| e.df);
        let n_terms = sorted.len();
        let drive = sorted[0];

        // Handle inline single-posting drive term cleanly
        if drive.is_inline_single() {
            let (doc_id, d_imp) = drive.inline_doc_imp();
            if !mask.contains(doc_id)
                || policy.is_denied_atomic(doc_id)
                || (tier == PolicyTier::Payload
                    && policy.payload_expires_at(doc_id) <= now_epoch_sec)
                || doc_pred.is_some_and(|p| !p(doc_id))
            {
                return Ok(Vec::new());
            }
            let mut score_f64 = (drive.idf() as f64) * (d_imp as f64);
            let mut buf = Vec::with_capacity(WAND_BLOCK_SIZE);
            for oe in &sorted[1..] {
                match self.lookup_term_doc_impact(oe, doc_id, &mut buf)? {
                    Some(imp) => score_f64 += (oe.idf() as f64) * (imp as f64),
                    None => return Ok(Vec::new()),
                }
            }
            return Ok(vec![ScoredDoc {
                doc_id,
                score: score_f64 as f32,
            }]);
        }

        let mut heap: BinaryHeap<MinHeapItem> = BinaryHeap::with_capacity(top_k + 1);
        let global_other_max: f32 = sorted[1..]
            .iter()
            .map(|e| e.idf() * (e.global_max_impact() as f32))
            .sum();

        let mut drive_buf = Vec::with_capacity(WAND_BLOCK_SIZE);
        let mut other_bufs: Vec<Vec<(u32, u16)>> = (1..n_terms)
            .map(|_| Vec::with_capacity(WAND_BLOCK_SIZE))
            .collect();
        let mut other_blk_idx: Vec<u32> = vec![0; n_terms - 1];
        let mut other_loaded_blk: Vec<u32> = vec![u32::MAX; n_terms - 1];

        for blk_i in 0..drive.num_blocks {
            let dbm = self.read_block_meta(&drive, blk_i)?;
            if mask.range_cardinality(dbm.first_doc..=dbm.last_doc) == 0 {
                continue;
            }
            let drive_blk_max = drive.idf() * (dbm.max_impact as f32);
            if heap.len() >= top_k {
                let worst = heap.peek().unwrap().0;
                if drive_blk_max + global_other_max + FLOAT_BOUND_EPS < worst.score {
                    continue;
                }
            }

            let mut tight_max = drive_blk_max;
            let mut possible = true;
            for oi in 0..n_terms - 1 {
                let oe = &sorted[oi + 1];
                if oe.is_inline_single() {
                    let (od, oimp) = oe.inline_doc_imp();
                    if od < dbm.first_doc || od > dbm.last_doc {
                        possible = false;
                        break;
                    }
                    tight_max += oe.idf() * (oimp as f32);
                    continue;
                }
                let nb = self.find_block_ge(oe, other_blk_idx[oi], dbm.first_doc)?;
                if nb >= oe.num_blocks {
                    possible = false;
                    break;
                }
                other_blk_idx[oi] = nb;
                let obm = self.read_block_meta(oe, nb)?;
                if obm.first_doc > dbm.last_doc {
                    possible = false;
                    break;
                }
                if obm.last_doc >= dbm.last_doc {
                    tight_max += oe.idf() * (obm.max_impact as f32);
                } else {
                    tight_max += oe.idf() * (oe.global_max_impact() as f32);
                }
            }
            if !possible {
                continue;
            }
            if heap.len() >= top_k {
                let worst = heap.peek().unwrap().0;
                // Strict conservative check: NEVER prune if tight_max could tie worst.score!
                if tight_max + FLOAT_BOUND_EPS < worst.score {
                    continue;
                }
            }

            self.decode_block(&dbm, &mut drive_buf)?;
            for &(doc_id, d_imp) in &drive_buf {
                if !mask.contains(doc_id)
                    || policy.is_denied_atomic(doc_id)
                    || (tier == PolicyTier::Payload
                        && policy.payload_expires_at(doc_id) <= now_epoch_sec)
                    || doc_pred.is_some_and(|p| !p(doc_id))
                {
                    continue;
                }
                let mut score_f64 = (drive.idf() as f64) * (d_imp as f64);
                let mut matched = true;
                for oi in 0..n_terms - 1 {
                    let oe = &sorted[oi + 1];
                    if oe.is_inline_single() {
                        let (od, oimp) = oe.inline_doc_imp();
                        if od == doc_id {
                            score_f64 += (oe.idf() as f64) * (oimp as f64);
                            continue;
                        } else {
                            matched = false;
                            break;
                        }
                    }
                    let nb = self.find_block_ge(oe, other_blk_idx[oi], doc_id)?;
                    if nb >= oe.num_blocks {
                        matched = false;
                        break;
                    }
                    other_blk_idx[oi] = nb;
                    if other_loaded_blk[oi] != nb {
                        let obm = self.read_block_meta(oe, nb)?;
                        if obm.first_doc > doc_id {
                            matched = false;
                            break;
                        }
                        self.decode_block(&obm, &mut other_bufs[oi])?;
                        other_loaded_blk[oi] = nb;
                    }
                    match other_bufs[oi].binary_search_by_key(&doc_id, |&(d, _)| d) {
                        Ok(pos) => score_f64 += (oe.idf() as f64) * (other_bufs[oi][pos].1 as f64),
                        Err(_) => {
                            matched = false;
                            break;
                        }
                    }
                }
                if matched {
                    let cand = ScoredDoc {
                        doc_id,
                        score: score_f64 as f32,
                    };
                    if heap.len() < top_k {
                        heap.push(MinHeapItem(cand));
                    } else if is_strictly_better(cand, heap.peek().unwrap().0) {
                        heap.pop();
                        heap.push(MinHeapItem(cand));
                    }
                }
            }
        }

        let mut res: Vec<ScoredDoc> = heap.into_iter().map(|h| h.0).collect();
        res.sort_unstable_by(|a, b| {
            b.score
                .total_cmp(&a.score)
                .then_with(|| a.doc_id.cmp(&b.doc_id))
        });
        Ok(res)
    }

    /// True Disjunctive OR Block-Max WAND with genuine pivot selection & conservative upper bounds!
    /// Property-tested 100% identical to an exhaustive brute-force reference scorer!
    #[allow(clippy::too_many_arguments)]
    fn execute_or_bmw(
        &self,
        entries: &[TermDirEntry],
        mask: &RoaringBitmap,
        doc_pred: Option<&dyn Fn(u32) -> bool>,
        policy: &PolicyPlane,
        tier: PolicyTier,
        now_epoch_sec: u32,
        top_k: usize,
    ) -> Result<Vec<ScoredDoc>, FormatError> {
        struct PostingCursor {
            entry: TermDirEntry,
            idf: f32,
            global_max: f32,
            cur_blk: u32,
            cur_blk_meta: Option<BlockMeta>,
            loaded_blk: u32,
            buf: Vec<(u32, u16)>,
            pos_in_blk: usize,
            cur_doc: u32,
            cur_imp: u16,
            exhausted: bool,
        }

        impl PostingCursor {
            fn new(plane: &CustomSearchPlane, entry: TermDirEntry) -> Result<Self, FormatError> {
                let idf = entry.idf();
                let global_max = idf * (entry.global_max_impact() as f32);
                if entry.is_inline_single() {
                    let (d, imp) = entry.inline_doc_imp();
                    return Ok(Self {
                        entry,
                        idf,
                        global_max,
                        cur_blk: 0,
                        cur_blk_meta: None,
                        loaded_blk: 0,
                        buf: vec![(d, imp)],
                        pos_in_blk: 0,
                        cur_doc: d,
                        cur_imp: imp,
                        exhausted: false,
                    });
                }
                if entry.num_blocks == 0 {
                    return Ok(Self {
                        entry,
                        idf,
                        global_max,
                        cur_blk: 0,
                        cur_blk_meta: None,
                        loaded_blk: u32::MAX,
                        buf: Vec::new(),
                        pos_in_blk: 0,
                        cur_doc: u32::MAX,
                        cur_imp: 0,
                        exhausted: true,
                    });
                }
                let bm = plane.read_block_meta(&entry, 0)?;
                let mut buf = Vec::with_capacity(WAND_BLOCK_SIZE);
                plane.decode_block(&bm, &mut buf)?;
                let (d, imp) = buf[0];
                Ok(Self {
                    entry,
                    idf,
                    global_max,
                    cur_blk: 0,
                    cur_blk_meta: Some(bm),
                    loaded_blk: 0,
                    buf,
                    pos_in_blk: 0,
                    cur_doc: d,
                    cur_imp: imp,
                    exhausted: false,
                })
            }

            /// Advance cursor to the smallest doc_id >= `target`.
            fn seek(&mut self, plane: &CustomSearchPlane, target: u32) -> Result<(), FormatError> {
                if self.exhausted || self.cur_doc >= target {
                    return Ok(());
                }
                if self.entry.is_inline_single() {
                    if self.cur_doc < target {
                        self.exhausted = true;
                        self.cur_doc = u32::MAX;
                    }
                    return Ok(());
                }
                // Check if target is within currently loaded block
                if let Some(bm) = self.cur_blk_meta {
                    if target <= bm.last_doc && self.loaded_blk == self.cur_blk {
                        let rest = &self.buf[self.pos_in_blk..];
                        let off = rest.partition_point(|&(d, _)| d < target);
                        self.pos_in_blk += off;
                        if self.pos_in_blk < self.buf.len() {
                            let (d, imp) = self.buf[self.pos_in_blk];
                            self.cur_doc = d;
                            self.cur_imp = imp;
                            return Ok(());
                        }
                    }
                }
                // Jump via block directory binary search
                let nb = plane.find_block_ge(&self.entry, self.cur_blk, target)?;
                if nb >= self.entry.num_blocks {
                    self.exhausted = true;
                    self.cur_doc = u32::MAX;
                    return Ok(());
                }
                self.cur_blk = nb;
                let bm = plane.read_block_meta(&self.entry, nb)?;
                self.cur_blk_meta = Some(bm);
                if self.loaded_blk != nb {
                    plane.decode_block(&bm, &mut self.buf)?;
                    self.loaded_blk = nb;
                }
                let idx = self.buf.partition_point(|&(d, _)| d < target);
                if idx < self.buf.len() {
                    self.pos_in_blk = idx;
                    let (d, imp) = self.buf[idx];
                    self.cur_doc = d;
                    self.cur_imp = imp;
                } else {
                    self.exhausted = true;
                    self.cur_doc = u32::MAX;
                }
                Ok(())
            }

            #[inline]
            fn current_block_max(&self, target_doc: u32) -> f32 {
                if self.entry.is_inline_single() {
                    return self.global_max;
                }
                if let Some(bm) = self.cur_blk_meta {
                    if target_doc <= bm.last_doc {
                        return self.idf * (bm.max_impact as f32);
                    }
                }
                self.global_max
            }

            #[inline]
            fn current_block_last(&self) -> u32 {
                if self.entry.is_inline_single() {
                    return self.cur_doc;
                }
                self.cur_blk_meta
                    .map(|b| b.last_doc)
                    .unwrap_or(self.cur_doc)
            }
        }

        let mut cursors: Vec<PostingCursor> = Vec::with_capacity(entries.len());
        for &e in entries {
            let c = PostingCursor::new(self, e)?;
            if !c.exhausted {
                cursors.push(c);
            }
        }
        if cursors.is_empty() {
            return Ok(Vec::new());
        }

        let mut heap: BinaryHeap<MinHeapItem> = BinaryHeap::with_capacity(top_k + 1);

        loop {
            // Drop exhausted cursors
            cursors.retain(|c| !c.exhausted);
            if cursors.is_empty() {
                break;
            }
            // Sort cursors by current doc_id ASC
            cursors.sort_unstable_by_key(|c| c.cur_doc);

            let threshold = if heap.len() >= top_k {
                heap.peek().unwrap().0.score
            } else {
                0.0
            };

            // WAND pivot selection: find smallest pivot index where cumulative global_max >= threshold - EPS
            let mut sum_ub = 0.0f32;
            let mut pivot_idx = None;
            for (i, c) in cursors.iter().enumerate() {
                sum_ub += c.global_max;
                if heap.len() < top_k || sum_ub + FLOAT_BOUND_EPS >= threshold {
                    pivot_idx = Some(i);
                    break;
                }
            }
            let Some(p_idx) = pivot_idx else {
                break; // No combination of remaining cursors can reach threshold!
            };
            let pivot_doc = cursors[p_idx].cur_doc;
            if pivot_doc == u32::MAX {
                break;
            }

            // Include all cursors tied at pivot_doc
            let mut p_end = p_idx;
            while p_end + 1 < cursors.len() && cursors[p_end + 1].cur_doc == pivot_doc {
                p_end += 1;
            }

            // Block-Max upper bound check across cursors[0..=p_end] at pivot_doc!
            if heap.len() >= top_k {
                let mut blk_ub = 0.0f32;
                let mut min_blk_end = u32::MAX;
                for c in &cursors[0..=p_end] {
                    blk_ub += c.current_block_max(pivot_doc);
                    min_blk_end = min_blk_end.min(c.current_block_last());
                }
                if blk_ub + FLOAT_BOUND_EPS < threshold {
                    // Shallow skip past the shallowest block boundary (or next cursor doc)!
                    let next_target = if p_end + 1 < cursors.len() {
                        min_blk_end
                            .saturating_add(1)
                            .min(cursors[p_end + 1].cur_doc)
                    } else {
                        min_blk_end.saturating_add(1)
                    };
                    let target = next_target.max(pivot_doc.saturating_add(1));
                    for c in &mut cursors[0..=p_end] {
                        c.seek(self, target)?;
                    }
                    continue;
                }
            }

            if cursors[0].cur_doc == pivot_doc {
                // Evaluate pivot_doc across ALL matching cursors[0..=p_end]
                if mask.contains(pivot_doc)
                    && !policy.is_denied_atomic(pivot_doc)
                    && (tier != PolicyTier::Payload
                        || policy.payload_expires_at(pivot_doc) > now_epoch_sec)
                    && doc_pred.is_none_or(|p| p(pivot_doc))
                {
                    let mut score_f64 = 0.0f64;
                    for c in &cursors[0..=p_end] {
                        score_f64 += (c.idf as f64) * (c.cur_imp as f64);
                    }
                    let score = score_f64 as f32;
                    let cand = ScoredDoc {
                        doc_id: pivot_doc,
                        score,
                    };
                    if heap.len() < top_k {
                        heap.push(MinHeapItem(cand));
                    } else if is_strictly_better(cand, heap.peek().unwrap().0) {
                        heap.pop();
                        heap.push(MinHeapItem(cand));
                    }
                }
                let next_doc = pivot_doc.saturating_add(1);
                for c in &mut cursors[0..=p_end] {
                    c.seek(self, next_doc)?;
                }
            } else {
                // Advance a cursor before pivot_doc up to pivot_doc
                cursors[0].seek(self, pivot_doc)?;
            }
        }

        let mut res: Vec<ScoredDoc> = heap.into_iter().map(|h| h.0).collect();
        res.sort_unstable_by(|a, b| {
            b.score
                .total_cmp(&a.score)
                .then_with(|| a.doc_id.cmp(&b.doc_id))
        });
        Ok(res)
    }

    fn lookup_term_doc_impact(
        &self,
        entry: &TermDirEntry,
        doc_id: u32,
        buf: &mut Vec<(u32, u16)>,
    ) -> Result<Option<u16>, FormatError> {
        if entry.is_inline_single() {
            let (d, imp) = entry.inline_doc_imp();
            return Ok(if d == doc_id { Some(imp) } else { None });
        }
        let nb = self.find_block_ge(entry, 0, doc_id)?;
        if nb >= entry.num_blocks {
            return Ok(None);
        }
        let bm = self.read_block_meta(entry, nb)?;
        if bm.first_doc > doc_id {
            return Ok(None);
        }
        self.decode_block(&bm, buf)?;
        Ok(buf
            .binary_search_by_key(&doc_id, |&(d, _)| d)
            .ok()
            .map(|p| buf[p].1))
    }

    /// O(limit) `browse_newest` WITHOUT `time_desc.bin`!
    /// Uses `(ct, published_epoch DESC)` physical doc_id ordering (`ct_ranges`).
    #[allow(clippy::too_many_arguments)]
    pub fn browse_newest(
        &self,
        ct_filter: Option<u8>,
        policy: &PolicyPlane,
        tier: PolicyTier,
        metas: &[MetaRow],
        limit: usize,
        offset: usize,
        now_epoch_sec: u32,
        now_ms: u64,
    ) -> Result<Vec<u32>, SearchError> {
        self.browse_newest_filtered(
            ct_filter,
            None,
            None,
            None,
            policy,
            tier,
            metas,
            limit,
            offset,
            now_epoch_sec,
            now_ms,
        )
    }

    #[allow(clippy::too_many_arguments)]
    pub fn browse_newest_filtered(
        &self,
        ct_filter: Option<u8>,
        extra_filter: Option<&RoaringBitmap>,
        l0_pred: Option<&dyn Fn(&l0::L0Doc) -> bool>,
        doc_pred: Option<&dyn Fn(u32) -> bool>,
        policy: &PolicyPlane,
        tier: PolicyTier,
        metas: &[MetaRow],
        limit: usize,
        offset: usize,
        now_epoch_sec: u32,
        now_ms: u64,
    ) -> Result<Vec<u32>, SearchError> {
        policy.lease().check(now_ms)?;
        if limit == 0 {
            return Ok(Vec::new());
        }
        let snap = policy.snapshot();
        let base_mask = snap.bitmap_for(tier);
        let effective_mask_owned;
        let mask: &RoaringBitmap = match extra_filter {
            Some(f) => {
                effective_mask_owned = base_mask & f;
                &effective_mask_owned
            }
            None => base_mask,
        };
        let mut out = Vec::with_capacity(limit);

        let has_l0 = self.l0.has_entries();
        let l0_recent: Vec<(u32, u32)> = if has_l0 {
            let mut v = Vec::new();
            for (d, ep) in self.l0.recent_docs_filtered(ct_filter, l0_pred) {
                if policy.authorize_egress_one(d, tier, now_epoch_sec, now_ms)?
                    && doc_pred.is_none_or(|p| p(d))
                {
                    v.push((d, ep));
                }
            }
            v
        } else {
            Vec::new()
        };
        let mut l0_idx = 0usize;
        let need_total = offset.saturating_add(limit);

        if let Some(ct) = ct_filter {
            let ci = ct as usize;
            if ci >= NUM_CONTENT_TYPES {
                return Ok(Vec::new());
            }
            let (start, end) = self.ct_ranges[ci];
            let mut merged: Vec<(u32, u32)> = Vec::with_capacity(need_total.min(1024));
            if start < end {
                let mut base_it = mask.range(start..end);
                while merged.len() < need_total {
                    let mut next_base: Option<(u32, u32)> = None;
                    for doc_id in base_it.by_ref() {
                        if (has_l0 && self.l0.is_superseded(doc_id))
                            || policy.is_denied_atomic(doc_id)
                            || (tier == PolicyTier::Payload
                                && policy.payload_expires_at(doc_id) <= now_epoch_sec)
                            || doc_pred.is_some_and(|p| !p(doc_id))
                        {
                            continue;
                        }
                        next_base = Some((doc_id, metas[doc_id as usize].published_epoch));
                        break;
                    }
                    let Some((bd, bep)) = next_base else {
                        break;
                    };
                    while l0_idx < l0_recent.len() && merged.len() < need_total {
                        let (ld, lep) = l0_recent[l0_idx];
                        if lep > bep || (lep == bep && ld < bd) {
                            merged.push((ld, lep));
                            l0_idx += 1;
                        } else {
                            break;
                        }
                    }
                    if merged.len() < need_total {
                        merged.push((bd, bep));
                    }
                }
            }
            while l0_idx < l0_recent.len() && merged.len() < need_total {
                merged.push(l0_recent[l0_idx]);
                l0_idx += 1;
            }
            out.extend(merged.into_iter().skip(offset).take(limit).map(|(d, _)| d));
            policy.filter_egress_docs(&mut out, tier, now_epoch_sec, now_ms)?;
            return Ok(out);
        }

        // Global newest across all 11 content_types + L0 stream: O((offset + limit) log 12) k-way merge!
        let mut iters: Vec<_> = (0..NUM_CONTENT_TYPES)
            .map(|ci| {
                let (s, e) = self.ct_ranges[ci];
                mask.range(s..e)
            })
            .collect();
        let l0_stream_idx = NUM_CONTENT_TYPES;
        let mut heap: BinaryHeap<(u32, std::cmp::Reverse<u32>, usize)> =
            BinaryHeap::with_capacity(NUM_CONTENT_TYPES + 1);
        for (ci, it) in iters.iter_mut().enumerate() {
            for d in it.by_ref() {
                if (!has_l0 || !self.l0.is_superseded(d))
                    && !policy.is_denied_atomic(d)
                    && (tier != PolicyTier::Payload || policy.payload_expires_at(d) > now_epoch_sec)
                    && doc_pred.is_none_or(|p| p(d))
                {
                    heap.push((metas[d as usize].published_epoch, std::cmp::Reverse(d), ci));
                    break;
                }
            }
        }
        if l0_idx < l0_recent.len() {
            let (ld, lep) = l0_recent[l0_idx];
            l0_idx += 1;
            heap.push((lep, std::cmp::Reverse(ld), l0_stream_idx));
        }
        let mut skipped = 0usize;
        while let Some((_, std::cmp::Reverse(doc_id), ci)) = heap.pop() {
            if skipped < offset {
                skipped += 1;
            } else {
                out.push(doc_id);
                if out.len() >= limit {
                    break;
                }
            }
            if ci == l0_stream_idx {
                if l0_idx < l0_recent.len() {
                    let (ld, lep) = l0_recent[l0_idx];
                    l0_idx += 1;
                    heap.push((lep, std::cmp::Reverse(ld), l0_stream_idx));
                }
            } else {
                for d in iters[ci].by_ref() {
                    if (!has_l0 || !self.l0.is_superseded(d))
                        && !policy.is_denied_atomic(d)
                        && (tier != PolicyTier::Payload
                            || policy.payload_expires_at(d) > now_epoch_sec)
                        && doc_pred.is_none_or(|p| p(d))
                    {
                        heap.push((metas[d as usize].published_epoch, std::cmp::Reverse(d), ci));
                        break;
                    }
                }
            }
        }
        policy.filter_egress_docs(&mut out, tier, now_epoch_sec, now_ms)?;
        Ok(out)
    }

    /// Exact policy-gated document count for a filtered browse/RSS query across base + L0.
    #[allow(clippy::too_many_arguments)]
    pub fn count_browse_filtered(
        &self,
        ct_filter: Option<u8>,
        extra_filter: Option<&RoaringBitmap>,
        l0_pred: Option<&dyn Fn(&l0::L0Doc) -> bool>,
        doc_pred: Option<&dyn Fn(u32) -> bool>,
        policy: &PolicyPlane,
        tier: PolicyTier,
        now_epoch_sec: u32,
        now_ms: u64,
    ) -> Result<u64, SearchError> {
        policy.lease().check(now_ms)?;
        let snap = policy.snapshot();
        let mut mask = snap.bitmap_for(tier).clone();
        if let Some(f) = extra_filter {
            mask &= f;
        }
        if let Some(ct) = ct_filter {
            if (ct as usize) >= NUM_CONTENT_TYPES {
                return Ok(0);
            }
            mask &= &self.facet_ct[ct as usize];
        }
        let has_l0 = self.l0.has_entries();
        let mut total = 0u64;
        for d in mask.iter() {
            if (!has_l0 || !self.l0.is_superseded(d))
                && !policy.is_denied_atomic(d)
                && (tier != PolicyTier::Payload || policy.payload_expires_at(d) > now_epoch_sec)
                && doc_pred.is_none_or(|p| p(d))
            {
                total += 1;
            }
        }
        if has_l0 {
            for (d, _) in self.l0.recent_docs_filtered(ct_filter, l0_pred) {
                if policy.authorize_egress_one(d, tier, now_epoch_sec, now_ms)?
                    && doc_pred.is_none_or(|p| p(d))
                {
                    total += 1;
                }
            }
        }
        Ok(total)
    }

    /// Hardened Torznab ID + Fixed Two-Tier Unlinked Fallback:
    /// - Per-entity episode keys (no 0..=200 cap, no 0xFFFF sentinel collision!)
    /// - Tier 1: exact normalized `strict_title_hashes` + multi-word `family_hashes` (allowing `CT_UNKNOWN`!)
    /// - Tier 2: bounded token-conjunctive backstop on unlinked docs verified via seekable Name column
    /// - Per-candidate `unlinked_year_compatible` check (NO whole-corpus year bitmap unions!)
    #[allow(clippy::too_many_arguments)]
    pub fn lookup_entity_with_fallback(
        &self,
        entity_id: u32,
        titles_and_aliases: &[String],
        ct_filter: u8,
        canonical_year: u16,
        season_ep: Option<(u16, Option<u16>)>,
        policy: &PolicyPlane,
        tier: PolicyTier,
        core: &CorePlane,
        limit: usize,
        now_epoch_sec: u32,
        now_ms: u64,
    ) -> Result<(Vec<u32>, Vec<u32>), SearchError> {
        self.lookup_entity_with_fallback_filtered(
            entity_id,
            titles_and_aliases,
            ct_filter,
            canonical_year,
            season_ep,
            None,
            policy,
            tier,
            core,
            limit,
            now_epoch_sec,
            now_ms,
        )
    }

    #[allow(clippy::too_many_arguments)]
    pub fn lookup_entity_with_fallback_filtered(
        &self,
        entity_id: u32,
        titles_and_aliases: &[String],
        ct_filter: u8,
        canonical_year: u16,
        season_ep: Option<(u16, Option<u16>)>,
        doc_pred: Option<&dyn Fn(u32) -> bool>,
        policy: &PolicyPlane,
        tier: PolicyTier,
        core: &CorePlane,
        limit: usize,
        now_epoch_sec: u32,
        now_ms: u64,
    ) -> Result<(Vec<u32>, Vec<u32>), SearchError> {
        policy.lease().check(now_ms)?;
        if limit == 0 {
            return Ok((Vec::new(), Vec::new()));
        }
        let snap = policy.snapshot();
        let mask = snap.bitmap_for(tier);
        let metas = core.metas();

        // 1. Linked lookup using per-entity episode keys!
        let mut linked_bm = if entity_id != 0 {
            if let Some((s, ep_opt)) = season_ep {
                match ep_opt {
                    Some(ep) => {
                        let mut bm = self
                            .ep_bms
                            .get_bitmap(ep_key_entity_episode(entity_id, s, ep))?
                            .unwrap_or_default();
                        if let Some(wb) = self
                            .ep_bms
                            .get_bitmap(ep_key_entity_whole_season(entity_id, s))?
                        {
                            bm |= wb;
                        }
                        bm
                    }
                    None => self
                        .ep_bms
                        .get_bitmap(ep_key_entity_any_season(entity_id, s))?
                        .unwrap_or_default(),
                }
            } else {
                self.entity_bms
                    .get_bitmap(entity_id as u64)?
                    .unwrap_or_default()
            }
        } else {
            RoaringBitmap::new()
        };
        linked_bm &= mask;

        let has_l0 = self.l0.has_entries();
        let doc_pub_epoch = |d: u32| -> u32 {
            if let Some(ov) = self.l0.get_doc(d) {
                ov.published_epoch
            } else {
                metas
                    .get(d as usize)
                    .map(|m| m.published_epoch)
                    .unwrap_or(0)
            }
        };
        let doc_ct_year = |d: u32| -> (u8, u16) {
            if let Some(ov) = self.l0.get_doc(d) {
                (ov.content_type, ov.year)
            } else if let Some(m) = metas.get(d as usize) {
                (m.content_type, m.year)
            } else {
                (CT_UNKNOWN, 0)
            }
        };

        // Already ordered newest-first within each content_type! Collect and sort by (published_epoch DESC, doc_id ASC)
        let mut linked: Vec<u32> = linked_bm
            .iter()
            .filter(|&d| {
                (!has_l0 || !self.l0.is_superseded(d))
                    && !policy.is_denied_atomic(d)
                    && (tier != PolicyTier::Payload || policy.payload_expires_at(d) > now_epoch_sec)
                    && doc_pred.is_none_or(|p| p(d))
            })
            .collect();
        if has_l0 && entity_id != 0 {
            let mut seen_linked: HashSet<u32> = linked.iter().copied().collect();
            for d in self.l0.lookup_entity(entity_id) {
                if !seen_linked.insert(d) {
                    continue;
                }
                if doc_pred.is_some_and(|p| !p(d)) {
                    continue;
                }
                if !policy.authorize_egress_one(d, tier, now_epoch_sec, now_ms)? {
                    continue;
                }
                if let Some((s, ep_opt)) = season_ep {
                    let ok = self
                        .l0
                        .get_doc(d)
                        .is_some_and(|ov| l0::doc_matches_episode(&ov.episodes, s, ep_opt));
                    if !ok {
                        continue;
                    }
                }
                linked.push(d);
            }
        }
        linked.sort_unstable_by(|&a, &b| {
            doc_pub_epoch(b)
                .cmp(&doc_pub_epoch(a))
                .then_with(|| a.cmp(&b))
        });
        linked.truncate(limit);

        if linked.len() >= limit || titles_and_aliases.is_empty() {
            policy.filter_egress_docs(&mut linked, tier, now_epoch_sec, now_ms)?;
            return Ok((linked, Vec::new()));
        }

        // 2. Fixed Two-Tier Unlinked Fallback!
        let needed = limit - linked.len();
        let mut strict_set: HashSet<u64> = HashSet::new();
        let mut multi_family_set: HashSet<u64> = HashSet::new();
        let mut query_token_sets: Vec<Vec<String>> = Vec::new();
        for raw_t in titles_and_aliases {
            let trimmed = raw_t.trim();
            if trimmed.is_empty() {
                continue;
            }
            let toks = tokenize_alnum(trimmed);
            if toks.is_empty() {
                continue;
            }
            let (sh, fh) = title_hashes_from_str(trimmed);
            if sh != 0 {
                strict_set.insert(sh);
            }
            if fh != 0 && toks.len() >= 2 {
                multi_family_set.insert(fh);
            }
            query_token_sets.push(toks);
        }

        // Unlinked episode filter (global episode keys, NO 0..=200 cap!)
        let global_ep_mask: Option<RoaringBitmap> = match season_ep {
            Some((s, Some(ep))) => {
                let mut bm = self
                    .ep_bms
                    .get_bitmap(ep_key_global_episode(s, ep))?
                    .unwrap_or_default();
                if let Some(wb) = self.ep_bms.get_bitmap(ep_key_global_whole_season(s))? {
                    bm |= wb;
                }
                Some(bm)
            }
            Some((s, None)) => Some(
                self.ep_bms
                    .get_bitmap(ep_key_global_any_season(s))?
                    .unwrap_or_default(),
            ),
            None => None,
        };

        // Allowed content_types: requested ct_filter OR unclassified CT_UNKNOWN!
        let mut unlinked_cands = RoaringBitmap::new();
        for &sh in &strict_set {
            if let Some(bm) = self.strict_title_bms.get_bitmap(sh)? {
                unlinked_cands |= bm;
            }
        }
        for &fh in &multi_family_set {
            if let Some(bm) = self.family_title_bms.get_bitmap(fh)? {
                unlinked_cands |= bm;
            }
        }
        unlinked_cands &= mask;
        unlinked_cands -= &self.facet_linked;
        if let Some(ref em) = global_ep_mask {
            unlinked_cands &= em;
        }

        let mut seen_unlinked: HashSet<u32> = linked.iter().copied().collect();
        let mut unlinked: Vec<u32> = Vec::with_capacity(needed);

        // Tier 1 candidates: check CT (requested or CT_UNKNOWN) and per-candidate year compatibility!
        let mut t1_docs: Vec<u32> = unlinked_cands
            .iter()
            .filter(|&d| {
                if (has_l0 && self.l0.is_superseded(d))
                    || seen_unlinked.contains(&d)
                    || policy.is_denied_atomic(d)
                    || (tier == PolicyTier::Payload
                        && policy.payload_expires_at(d) <= now_epoch_sec)
                    || doc_pred.is_some_and(|p| !p(d))
                {
                    return false;
                }
                let (dct, dyr) = doc_ct_year(d);
                if ct_filter != CT_UNKNOWN && dct != ct_filter && dct != CT_UNKNOWN {
                    return false;
                }
                // Per-candidate year check (ZERO whole-corpus year bitmap unions!)
                if canonical_year > 0 && dyr > 0 {
                    let diff = (dyr as i32) - (canonical_year as i32);
                    if ct_filter == CT_MOVIE && !(-1..=1).contains(&diff) {
                        return false;
                    }
                    if ct_filter == CT_TV_SHOW && diff < -1 {
                        return false;
                    }
                }
                true
            })
            .collect();
        if has_l0 {
            let mut seen_t1: HashSet<u32> = t1_docs.iter().copied().collect();
            let mut l0_cands: Vec<u32> = Vec::new();
            for &sh in &strict_set {
                l0_cands.extend(self.l0.lookup_strict_title(sh));
            }
            for &fh in &multi_family_set {
                l0_cands.extend(self.l0.lookup_family(fh));
            }
            for d in l0_cands {
                if seen_unlinked.contains(&d) || !seen_t1.insert(d) {
                    continue;
                }
                let Some(ov) = self.l0.get_doc(d) else {
                    continue;
                };
                if ov.entity_id != 0 {
                    continue;
                }
                if doc_pred.is_some_and(|p| !p(d)) {
                    continue;
                }
                if !policy.authorize_egress_one(d, tier, now_epoch_sec, now_ms)? {
                    continue;
                }
                if let Some((s, ep_opt)) = season_ep {
                    if !l0::doc_matches_episode(&ov.episodes, s, ep_opt) {
                        continue;
                    }
                }
                if ct_filter != CT_UNKNOWN
                    && ov.content_type != ct_filter
                    && ov.content_type != CT_UNKNOWN
                {
                    continue;
                }
                if canonical_year > 0 && ov.year > 0 {
                    let diff = (ov.year as i32) - (canonical_year as i32);
                    if ct_filter == CT_MOVIE && !(-1..=1).contains(&diff) {
                        continue;
                    }
                    if ct_filter == CT_TV_SHOW && diff < -1 {
                        continue;
                    }
                }
                t1_docs.push(d);
            }
        }
        t1_docs.sort_unstable_by(|&a, &b| {
            let (cta, _) = doc_ct_year(a);
            let (ctb, _) = doc_ct_year(b);
            let ta = (ct_filter != CT_UNKNOWN && cta == ct_filter) as u8;
            let tb = (ct_filter != CT_UNKNOWN && ctb == ct_filter) as u8;
            tb.cmp(&ta)
                .then_with(|| doc_pub_epoch(b).cmp(&doc_pub_epoch(a)))
                .then_with(|| a.cmp(&b))
        });

        for d in t1_docs {
            if unlinked.len() >= needed {
                break;
            }
            let (_, dyr) = doc_ct_year(d);
            // Refine TV S1-only year rule if needed via fast name parse
            if ct_filter == CT_TV_SHOW && canonical_year > 0 && dyr > 0 {
                let name_opt = if let Some(ov) = self.l0.get_doc(d) {
                    Some(ov.name)
                } else {
                    core.get_name(d).ok().map(str::to_owned)
                };
                if let Some(name) = name_opt {
                    let pr = parse_release_name(&name);
                    if !unlinked_year_compatible(ct_filter, canonical_year, pr.year, &pr.episodes) {
                        continue;
                    }
                }
            }
            seen_unlinked.insert(d);
            unlinked.push(d);
        }

        // Tier 2 bounded token-conjunctive backstop: prioritize typed ct_filter pass BEFORE CT_UNKNOWN pass!
        if unlinked.len() < needed {
            let mut base_unlinked_mask = mask - &self.facet_linked;
            if let Some(ref em) = global_ep_mask {
                base_unlinked_mask &= em;
            }
            let ct_passes: Vec<Option<u8>> =
                if ct_filter != CT_UNKNOWN && (ct_filter as usize) < NUM_CONTENT_TYPES {
                    vec![Some(ct_filter), Some(CT_UNKNOWN)]
                } else {
                    vec![None]
                };
            for ct_pass in ct_passes {
                if unlinked.len() >= needed {
                    break;
                }
                let pass_mask = match ct_pass {
                    Some(ct) => &base_unlinked_mask & &self.facet_ct[ct as usize],
                    None => base_unlinked_mask.clone(),
                };
                if pass_mask.is_empty() {
                    continue;
                }
                let scan_cap = ((needed - unlinked.len()) * 6).clamp(128, 1000);
                for toks in &query_token_sets {
                    if unlinked.len() >= needed {
                        break;
                    }
                    let mut t_entries = Vec::with_capacity(toks.len());
                    let mut all_found = true;
                    let mut dedup = toks.clone();
                    dedup.sort();
                    dedup.dedup();
                    for t in &dedup {
                        if let Some(e) = self.find_term(term_hash(t, self.term_seed)) {
                            t_entries.push(e);
                        } else {
                            all_found = false;
                            break;
                        }
                    }
                    if !all_found || t_entries.is_empty() {
                        continue;
                    }
                    let cands = self.execute_and_bmw(
                        &t_entries,
                        &pass_mask,
                        None,
                        policy,
                        tier,
                        now_epoch_sec,
                        scan_cap,
                    )?;
                    for sd in cands {
                        if unlinked.len() >= needed {
                            break;
                        }
                        if seen_unlinked.contains(&sd.doc_id)
                            || (has_l0 && self.l0.is_superseded(sd.doc_id))
                            || doc_pred.is_some_and(|p| !p(sd.doc_id))
                        {
                            continue;
                        }
                        let name = core.get_name(sd.doc_id)?;
                        let pr = parse_release_name(name);
                        let title_match = pr
                            .strict_title_hashes
                            .iter()
                            .any(|h| strict_set.contains(h))
                            || pr
                                .family_hashes
                                .iter()
                                .any(|h| multi_family_set.contains(h))
                            || contains_subslice(&pr.raw_tokens, toks);
                        if !title_match {
                            continue;
                        }
                        if !unlinked_year_compatible(
                            ct_filter,
                            canonical_year,
                            pr.year,
                            &pr.episodes,
                        ) {
                            continue;
                        }
                        seen_unlinked.insert(sd.doc_id);
                        unlinked.push(sd.doc_id);
                    }
                }
            }
        }

        policy.filter_egress_docs(&mut linked, tier, now_epoch_sec, now_ms)?;
        policy.filter_egress_docs(&mut unlinked, tier, now_epoch_sec, now_ms)?;
        Ok((linked, unlinked))
    }

    /// Torznab TV text + season + optional episode search (supports bare season AND specific episode + packs!).
    #[allow(clippy::too_many_arguments)]
    pub fn tvsearch_text(
        &self,
        title: &str,
        season: u16,
        episode: Option<u16>,
        policy: &PolicyPlane,
        tier: PolicyTier,
        core: &CorePlane,
        top_k: usize,
        now_epoch_sec: u32,
        now_ms: u64,
    ) -> Result<Vec<ScoredDoc>, SearchError> {
        self.tvsearch_text_filtered(
            title,
            season,
            episode,
            None,
            None,
            None,
            policy,
            tier,
            core,
            top_k,
            now_epoch_sec,
            now_ms,
        )
    }

    #[allow(clippy::too_many_arguments)]
    pub fn tvsearch_text_filtered(
        &self,
        title: &str,
        season: u16,
        episode: Option<u16>,
        extra_filter: Option<&RoaringBitmap>,
        l0_pred: Option<&dyn Fn(&l0::L0Doc) -> bool>,
        doc_pred: Option<&dyn Fn(u32) -> bool>,
        policy: &PolicyPlane,
        tier: PolicyTier,
        core: &CorePlane,
        top_k: usize,
        now_epoch_sec: u32,
        now_ms: u64,
    ) -> Result<Vec<ScoredDoc>, SearchError> {
        let mut ep_bm = match episode {
            Some(ep) => {
                let mut bm = self
                    .ep_bms
                    .get_bitmap(ep_key_global_episode(season, ep))?
                    .unwrap_or_default();
                if let Some(wb) = self.ep_bms.get_bitmap(ep_key_global_whole_season(season))? {
                    bm |= wb;
                }
                bm
            }
            None => self
                .ep_bms
                .get_bitmap(ep_key_global_any_season(season))?
                .unwrap_or_default(),
        };
        // Allow TV_SHOW OR CT_UNKNOWN so unclassified new TV episodes aren't hidden!
        let mut ct_ok = self.facet_ct[CT_TV_SHOW as usize].clone();
        ct_ok |= &self.facet_ct[CT_UNKNOWN as usize];
        ep_bm &= &ct_ok;
        if let Some(f) = extra_filter {
            ep_bm &= f;
        }
        let mut l0_ep_docs: HashSet<u32> = HashSet::new();
        if self.l0.has_entries() {
            for d in self.l0.matching_episode_docs(season, episode) {
                if let Some(ov) = self.l0.get_doc(d) {
                    if (ov.content_type == CT_TV_SHOW || ov.content_type == CT_UNKNOWN)
                        && l0_pred.is_none_or(|p| p(&ov))
                    {
                        ep_bm.insert(d);
                        l0_ep_docs.insert(d);
                    }
                }
            }
        }
        if ep_bm.is_empty() {
            return Ok(Vec::new());
        }
        let combined_l0 = |ov: &l0::L0Doc| l0_ep_docs.contains(&ov.doc_id);
        self.search_text_filtered(
            title,
            QueryMode::And,
            Some(&ep_bm),
            Some(&combined_l0),
            doc_pred,
            policy,
            tier,
            core,
            top_k,
            now_epoch_sec,
            now_ms,
        )
    }

    /// Trigram / Substring search (`pg_trgm` ILIKE '%sub%' parity) with exact verification against seekable Name column!
    #[allow(clippy::too_many_arguments)]
    pub fn search_substring(
        &self,
        substring: &str,
        policy: &PolicyPlane,
        tier: PolicyTier,
        core: &CorePlane,
        limit: usize,
        now_epoch_sec: u32,
        now_ms: u64,
    ) -> Result<Vec<u32>, SearchError> {
        policy.lease().check(now_ms)?;
        if self.trigrams.is_empty() && core.num_docs > 0 {
            return Err(SearchError::TrigramsDisabled);
        }
        let needle = fold_accents_lowercase(substring);
        if needle.is_empty() || limit == 0 {
            return Ok(Vec::new());
        }
        let tris = extract_substring_query_trigrams(&needle);
        if tris.is_empty() {
            return Ok(Vec::new());
        }
        let has_l0 = self.l0.has_entries();
        let snap = policy.snapshot();
        let mut cands = snap.bitmap_for(tier).clone();
        let mut base_possible = !cands.is_empty();
        if base_possible {
            for tri in tris {
                if let Some(tb) = self.trigrams.get_bitmap(trigram_key(tri))? {
                    cands &= &tb;
                    if cands.is_empty() {
                        base_possible = false;
                        break;
                    }
                } else {
                    base_possible = false;
                    break;
                }
            }
        }
        let mut out = Vec::with_capacity(limit);
        if base_possible {
            for doc_id in cands.iter() {
                if (has_l0 && self.l0.is_superseded(doc_id))
                    || policy.is_denied_atomic(doc_id)
                    || (tier == PolicyTier::Payload
                        && policy.payload_expires_at(doc_id) <= now_epoch_sec)
                {
                    continue;
                }
                let name = core.get_name(doc_id)?;
                if fold_accents_lowercase(name).contains(&needle) {
                    out.push(doc_id);
                    if out.len() >= limit {
                        break;
                    }
                }
            }
        }
        if has_l0 && out.len() < limit {
            let mut l0_hits = self.l0.matching_substring(&needle);
            l0_hits.sort_unstable();
            for doc_id in l0_hits {
                if out.len() >= limit {
                    break;
                }
                if policy.authorize_egress_one(doc_id, tier, now_epoch_sec, now_ms)? {
                    out.push(doc_id);
                }
            }
        }
        policy.filter_egress_docs(&mut out, tier, now_epoch_sec, now_ms)?;
        Ok(out)
    }

    /// Hash-ordered substring search (PostgreSQL `torrents.info_hash ASC` + `> after_hash` parity)
    /// merging immutable base trigrams and live L0 overlay names. Returns `TooManyCandidates` when
    /// the eligible candidate set exceeds `max_scan` so callers transparently fall back to PostgreSQL.
    #[allow(clippy::too_many_arguments)]
    pub fn search_substring_hash_ordered<F>(
        &self,
        substring: &str,
        after_hash: Option<&[u8]>,
        hash_lookup: F,
        max_scan: usize,
        policy: &PolicyPlane,
        tier: PolicyTier,
        core: &CorePlane,
        limit: usize,
        now_epoch_sec: u32,
        now_ms: u64,
    ) -> Result<Vec<u32>, SearchError>
    where
        F: Fn(u32) -> Option<Vec<u8>>,
    {
        policy.lease().check(now_ms)?;
        if self.trigrams.is_empty() && core.num_docs > 0 {
            return Err(SearchError::TrigramsDisabled);
        }
        let needle = fold_accents_lowercase(substring);
        if needle.is_empty() || limit == 0 {
            return Ok(Vec::new());
        }
        let tris = extract_substring_query_trigrams(&needle);
        if tris.is_empty() {
            return Ok(Vec::new());
        }
        let has_l0 = self.l0.has_entries();
        let snap = policy.snapshot();
        let mut cands = snap.bitmap_for(tier).clone();
        let mut base_possible = !cands.is_empty();
        if base_possible {
            for tri in tris {
                if let Some(tb) = self.trigrams.get_bitmap(trigram_key(tri))? {
                    cands &= &tb;
                    if cands.is_empty() {
                        base_possible = false;
                        break;
                    }
                } else {
                    base_possible = false;
                    break;
                }
            }
        }
        if base_possible && (cands.len() as usize) > max_scan {
            return Err(SearchError::TooManyCandidates);
        }
        let mut heap: BinaryHeap<(Vec<u8>, u32)> = BinaryHeap::with_capacity(limit + 1);
        let mut consider = |doc_id: u32| {
            let Some(key) = hash_lookup(doc_id) else {
                return;
            };
            if let Some(after) = after_hash {
                if key.as_slice() <= after {
                    return;
                }
            }
            if heap.len() < limit {
                heap.push((key, doc_id));
            } else if let Some((top_key, top_id)) = heap.peek() {
                if key < *top_key || (key == *top_key && doc_id < *top_id) {
                    heap.pop();
                    heap.push((key, doc_id));
                }
            }
        };
        if base_possible {
            for doc_id in cands.iter() {
                if (has_l0 && self.l0.is_superseded(doc_id))
                    || policy.is_denied_atomic(doc_id)
                    || (tier == PolicyTier::Payload
                        && policy.payload_expires_at(doc_id) <= now_epoch_sec)
                {
                    continue;
                }
                let name = core.get_name(doc_id)?;
                if fold_accents_lowercase(name).contains(&needle) {
                    consider(doc_id);
                }
            }
        }
        if has_l0 {
            for doc_id in self.l0.matching_substring(&needle) {
                if policy.authorize_egress_one(doc_id, tier, now_epoch_sec, now_ms)? {
                    consider(doc_id);
                }
            }
        }
        let mut sorted: Vec<(Vec<u8>, u32)> = heap.into_vec();
        sorted.sort_unstable_by(|a, b| a.0.cmp(&b.0).then_with(|| a.1.cmp(&b.1)));
        let mut out: Vec<u32> = sorted.into_iter().map(|(_, d)| d).collect();
        policy.filter_egress_docs(&mut out, tier, now_epoch_sec, now_ms)?;
        Ok(out)
    }

    /// Rolling 30-day `dashstats` (NEVER frozen at build time — dynamically unions daily bitmaps for `[now-30d, now]`).
    pub fn dashstats_30d(
        &self,
        now_epoch_sec: u32,
        policy: &PolicyPlane,
        tier: PolicyTier,
        metas: &[MetaRow],
        now_ms: u64,
    ) -> Result<(u64, u64, u64, u64), SearchError> {
        policy.lease().check(now_ms)?;
        let cutoff_sec = now_epoch_sec.saturating_sub(30 * 86_400);
        let start_day = cutoff_sec / 86_400;
        let end_day = (now_epoch_sec / 86_400).saturating_add(365);

        let snap = policy.snapshot();
        let mask = snap.bitmap_for(tier);
        let mut recent = RoaringBitmap::new();
        for day in start_day..=end_day {
            if let Some(dbm) = self.created_day_bms.get_bitmap(day as u64)? {
                if day == start_day {
                    // Exact second boundary refinement on the oldest day
                    for d in (&dbm & mask).iter() {
                        if metas[d as usize].created_epoch >= cutoff_sec {
                            recent.insert(d);
                        }
                    }
                } else {
                    recent |= dbm;
                }
            }
        }
        recent &= mask;
        let has_l0 = self.l0.has_entries();
        // Exact movie & TV counts (excluding any atomically denied or L0-superseded base docs!)
        let movies = &recent & &self.facet_ct[CT_MOVIE as usize];
        let mut movies_30d = 0u64;
        let mut movies_linked_30d = 0u64;
        for d in movies.iter() {
            if (!has_l0 || !self.l0.is_superseded(d))
                && !policy.is_denied_atomic(d)
                && (tier != PolicyTier::Payload || policy.payload_expires_at(d) > now_epoch_sec)
            {
                movies_30d += 1;
                if self.facet_linked.contains(d) {
                    movies_linked_30d += 1;
                }
            }
        }
        let tv = &recent & &self.facet_ct[CT_TV_SHOW as usize];
        let mut tv_30d = 0u64;
        let mut tv_linked_30d = 0u64;
        for d in tv.iter() {
            if (!has_l0 || !self.l0.is_superseded(d))
                && !policy.is_denied_atomic(d)
                && (tier != PolicyTier::Payload || policy.payload_expires_at(d) > now_epoch_sec)
            {
                tv_30d += 1;
                if self.facet_linked.contains(d) {
                    tv_linked_30d += 1;
                }
            }
        }
        if has_l0 {
            for (d, ct, eid) in self.l0.docs_since(cutoff_sec) {
                if !policy.authorize_egress_one(d, tier, now_epoch_sec, now_ms)? {
                    continue;
                }
                if ct == CT_MOVIE {
                    movies_30d += 1;
                    if eid != 0 {
                        movies_linked_30d += 1;
                    }
                } else if ct == CT_TV_SHOW {
                    tv_30d += 1;
                    if eid != 0 {
                        tv_linked_30d += 1;
                    }
                }
            }
        }
        Ok((movies_30d, movies_linked_30d, tv_30d, tv_linked_30d))
    }

    /// Ultra-fast exact policy-gated facet counts:
    /// 1. When all query terms are dense (`df >= 50K`, stored in `dense_terms.mmap`): pure SIMD Roaring intersection!
    /// 2. Otherwise: scans postings and reads 1-byte `facet_col[doc_id]` (32x less memory bandwidth than MetaRow!).
    pub fn text_facet_counts(
        &self,
        query: &str,
        policy: &PolicyPlane,
        tier: PolicyTier,
        facet_bytes: &[u8],
        now_epoch_sec: u32,
        now_ms: u64,
    ) -> Result<([u64; NUM_CONTENT_TYPES], [u64; 10], u64), SearchError> {
        self.text_facet_counts_filtered(
            query,
            None,
            None,
            None,
            policy,
            tier,
            facet_bytes,
            now_epoch_sec,
            now_ms,
        )
    }

    #[allow(clippy::too_many_arguments)]
    pub fn text_facet_counts_filtered(
        &self,
        query: &str,
        extra_filter: Option<&RoaringBitmap>,
        l0_pred: Option<&dyn Fn(&l0::L0Doc) -> bool>,
        doc_pred: Option<&dyn Fn(u32) -> bool>,
        policy: &PolicyPlane,
        tier: PolicyTier,
        facet_bytes: &[u8],
        now_epoch_sec: u32,
        now_ms: u64,
    ) -> Result<([u64; NUM_CONTENT_TYPES], [u64; 10], u64), SearchError> {
        policy.lease().check(now_ms)?;
        let mut ct_counts = [0u64; NUM_CONTENT_TYPES];
        let mut res_counts = [0u64; 10];
        let mut total = 0u64;

        let mut toks = tokenize_alnum(query);
        toks.sort();
        toks.dedup();
        if toks.is_empty() {
            return Ok((ct_counts, res_counts, 0));
        }

        let snap = policy.snapshot();
        let base_mask = snap.bitmap_for(tier);
        let effective_mask_owned;
        let mask: &RoaringBitmap = match extra_filter {
            Some(f) => {
                effective_mask_owned = base_mask & f;
                &effective_mask_owned
            }
            None => base_mask,
        };
        let has_l0 = self.l0.has_entries();
        if mask.is_empty() && !has_l0 {
            return Ok((ct_counts, res_counts, 0));
        }

        let mut term_entries = Vec::with_capacity(toks.len());
        let mut term_hashes = Vec::with_capacity(toks.len());
        let mut all_dense = true;
        let mut dense_bms: Vec<RoaringBitmap> = Vec::with_capacity(toks.len());
        for t in &toks {
            let th = term_hash(t, self.term_seed);
            term_hashes.push(th);
            match self.find_term(th) {
                Some(e) => {
                    term_entries.push(e);
                    if all_dense {
                        if let Some(dbm) = self.dense_terms.get_bitmap(th)? {
                            dense_bms.push(dbm);
                        } else {
                            all_dense = false;
                        }
                    }
                }
                None => {
                    if !has_l0 {
                        return Ok((ct_counts, res_counts, 0));
                    }
                }
            }
        }

        // Fast Path A: Pure SIMD Roaring bitmap intersection for dense terms (zero posting decode, zero page faults!)
        if all_dense
            && term_entries.len() == toks.len()
            && !dense_bms.is_empty()
            && tier != PolicyTier::Payload
            && !has_l0
            && doc_pred.is_none()
        {
            dense_bms.sort_by_key(|b| b.len());
            let mut matched = &dense_bms[0] & mask;
            for b in &dense_bms[1..] {
                matched &= b;
            }
            for (i, slot) in ct_counts.iter_mut().enumerate().take(NUM_CONTENT_TYPES) {
                *slot = matched.intersection_len(&self.facet_ct[i]);
            }
            for (i, slot) in res_counts.iter_mut().enumerate() {
                *slot = matched.intersection_len(&self.facet_res[i]);
            }
            total = matched.len();
            return Ok((ct_counts, res_counts, total));
        }

        // Fast Path B: Postings intersection + 1-byte `facet_col.bin` lookup!
        if term_entries.len() == toks.len() && !mask.is_empty() {
            term_entries.sort_unstable_by_key(|e| e.df);
            let n_terms = term_entries.len();
            let drive = term_entries[0];

            if drive.is_inline_single() {
                let (doc_id, _) = drive.inline_doc_imp();
                if mask.contains(doc_id)
                    && (!has_l0 || !self.l0.is_superseded(doc_id))
                    && !policy.is_denied_atomic(doc_id)
                    && (tier != PolicyTier::Payload
                        || policy.payload_expires_at(doc_id) > now_epoch_sec)
                    && doc_pred.is_none_or(|p| p(doc_id))
                {
                    let mut buf = Vec::with_capacity(WAND_BLOCK_SIZE);
                    let mut ok = true;
                    for oe in &term_entries[1..] {
                        if self.lookup_term_doc_impact(oe, doc_id, &mut buf)?.is_none() {
                            ok = false;
                            break;
                        }
                    }
                    if ok {
                        let fb = facet_bytes[doc_id as usize];
                        let ct = (fb & 0x0F) as usize;
                        let res = ((fb >> 4) & 0x0F) as usize;
                        if ct < NUM_CONTENT_TYPES {
                            ct_counts[ct] += 1;
                        }
                        if res < 10 {
                            res_counts[res] += 1;
                        }
                        total = 1;
                    }
                }
            } else {
                let mut drive_buf = Vec::with_capacity(WAND_BLOCK_SIZE);
                let mut other_bufs: Vec<Vec<(u32, u16)>> = (1..n_terms)
                    .map(|_| Vec::with_capacity(WAND_BLOCK_SIZE))
                    .collect();
                let mut other_blk_idx: Vec<u32> = vec![0; n_terms - 1];
                let mut other_loaded_blk: Vec<u32> = vec![u32::MAX; n_terms - 1];

                for blk_i in 0..drive.num_blocks {
                    let dbm = self.read_block_meta(&drive, blk_i)?;
                    if mask.range_cardinality(dbm.first_doc..=dbm.last_doc) == 0 {
                        continue;
                    }
                    let mut possible = true;
                    for oi in 0..n_terms - 1 {
                        let oe = &term_entries[oi + 1];
                        if oe.is_inline_single() {
                            let (od, _) = oe.inline_doc_imp();
                            if od < dbm.first_doc || od > dbm.last_doc {
                                possible = false;
                                break;
                            }
                            continue;
                        }
                        let nb = self.find_block_ge(oe, other_blk_idx[oi], dbm.first_doc)?;
                        if nb >= oe.num_blocks {
                            possible = false;
                            break;
                        }
                        other_blk_idx[oi] = nb;
                        let obm = self.read_block_meta(oe, nb)?;
                        if obm.first_doc > dbm.last_doc {
                            possible = false;
                            break;
                        }
                    }
                    if !possible {
                        continue;
                    }
                    self.decode_block(&dbm, &mut drive_buf)?;
                    for &(doc_id, _) in &drive_buf {
                        if !mask.contains(doc_id)
                            || (has_l0 && self.l0.is_superseded(doc_id))
                            || policy.is_denied_atomic(doc_id)
                            || (tier == PolicyTier::Payload
                                && policy.payload_expires_at(doc_id) <= now_epoch_sec)
                            || doc_pred.is_some_and(|p| !p(doc_id))
                        {
                            continue;
                        }
                        let mut matched = true;
                        for oi in 0..n_terms - 1 {
                            let oe = &term_entries[oi + 1];
                            if oe.is_inline_single() {
                                if oe.inline_doc_imp().0 != doc_id {
                                    matched = false;
                                    break;
                                }
                                continue;
                            }
                            let nb = self.find_block_ge(oe, other_blk_idx[oi], doc_id)?;
                            if nb >= oe.num_blocks {
                                matched = false;
                                break;
                            }
                            other_blk_idx[oi] = nb;
                            if other_loaded_blk[oi] != nb {
                                let obm = self.read_block_meta(oe, nb)?;
                                if obm.first_doc > doc_id {
                                    matched = false;
                                    break;
                                }
                                self.decode_block(&obm, &mut other_bufs[oi])?;
                                other_loaded_blk[oi] = nb;
                            }
                            if other_bufs[oi]
                                .binary_search_by_key(&doc_id, |&(d, _)| d)
                                .is_err()
                            {
                                matched = false;
                                break;
                            }
                        }
                        if matched {
                            let fb = facet_bytes[doc_id as usize];
                            let ct = (fb & 0x0F) as usize;
                            let res = ((fb >> 4) & 0x0F) as usize;
                            if ct < NUM_CONTENT_TYPES {
                                ct_counts[ct] += 1;
                            }
                            if res < 10 {
                                res_counts[res] += 1;
                            }
                            total += 1;
                        }
                    }
                }
            }
        }
        if has_l0 {
            for (doc_id, _) in self.l0.search_terms(&term_hashes, true) {
                if !policy.authorize_egress_one(doc_id, tier, now_epoch_sec, now_ms)? {
                    continue;
                }
                if doc_pred.is_some_and(|p| !p(doc_id)) {
                    continue;
                }
                let Some(ov) = self.l0.get_doc(doc_id) else {
                    continue;
                };
                if let Some(pred) = l0_pred {
                    if !pred(&ov) {
                        continue;
                    }
                } else if extra_filter.is_some_and(|f| !f.contains(doc_id)) {
                    continue;
                }
                if (ov.content_type as usize) < NUM_CONTENT_TYPES {
                    ct_counts[ov.content_type as usize] += 1;
                }
                if (ov.resolution as usize) < 10 {
                    res_counts[ov.resolution as usize] += 1;
                }
                total += 1;
            }
        }
        Ok((ct_counts, res_counts, total))
    }

    /// Exhaustive brute-force reference scorer used in property tests to verify 100% exact top-K parity
    /// of both `execute_and_bmw` and `execute_or_bmw` (including exact tie-breaking!).
    #[cfg(test)]
    fn reference_exhaustive_search(
        &self,
        query_str: &str,
        and_mode: bool,
        mask: &RoaringBitmap,
        top_k: usize,
    ) -> Vec<ScoredDoc> {
        let mut toks = tokenize_alnum(query_str);
        toks.sort();
        toks.dedup();
        let mut acc: AHashMap<u32, (f64, usize)> = AHashMap::new();
        let mut found_terms = 0usize;
        for t in &toks {
            let th = term_hash(t, self.term_seed);
            let Some(e) = self.find_term(th) else {
                if and_mode {
                    return Vec::new();
                }
                continue;
            };
            found_terms += 1;
            if e.is_inline_single() {
                let (d, imp) = e.inline_doc_imp();
                if mask.contains(d) {
                    let x = acc.entry(d).or_insert((0.0, 0));
                    x.0 += (e.idf() as f64) * (imp as f64);
                    x.1 += 1;
                }
            } else {
                let mut buf = Vec::new();
                for b in 0..e.num_blocks {
                    let bm = self.read_block_meta(&e, b).unwrap();
                    self.decode_block(&bm, &mut buf).unwrap();
                    for &(d, imp) in &buf {
                        if mask.contains(d) {
                            let x = acc.entry(d).or_insert((0.0, 0));
                            x.0 += (e.idf() as f64) * (imp as f64);
                            x.1 += 1;
                        }
                    }
                }
            }
        }
        let mut res: Vec<ScoredDoc> = acc
            .into_iter()
            .filter(|&(_, (_, cnt))| !and_mode || cnt == found_terms)
            .map(|(doc_id, (score, _))| ScoredDoc {
                doc_id,
                score: score as f32,
            })
            .collect();
        res.sort_unstable_by(|a, b| {
            b.score
                .total_cmp(&a.score)
                .then_with(|| a.doc_id.cmp(&b.doc_id))
        });
        res.truncate(top_k);
        res
    }
}

#[inline]
fn contains_subslice(haystack: &[String], needle: &[String]) -> bool {
    if needle.is_empty() || needle.len() > haystack.len() {
        return false;
    }
    haystack.windows(needle.len()).any(|w| w == needle)
}

#[cfg(test)]
mod tests {
    use super::*;
    use dh_tms_core::StreamingCoreWriter;
    use dh_tms_policy::PolicyState;
    use proptest::prelude::*;

    #[test]
    fn test_bmw_and_or_exact_parity_with_exhaustive_reference_including_ties() {
        let dir = std::env::temp_dir().join(format!("dhtms_bmw_prop_{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();

        let vocab = ["alpha", "beta", "gamma", "delta", "epsilon", "zeta"];
        let num_docs = 600u32;
        let mut sb = CustomSearchBuilder::new(5, 0x1234, num_docs, &dir, true);
        let mut ps = PolicyState::new(num_docs as usize);
        let ext_map = AHashMap::new();
        let mut cw = StreamingCoreWriter::new(&dir, 5, num_docs as usize, Vec::new()).unwrap();
        let mut metas = Vec::new();

        for d in 0..num_docs {
            // Deliberately create many identical-title ties across multiple 128-doc blocks!
            let mut title = Vec::new();
            if d % 2 == 0 {
                title.push(vocab[0].to_string());
            }
            if d % 3 == 0 {
                title.push(vocab[1].to_string());
            }
            if d % 5 == 0 {
                title.push(vocab[2].to_string());
            }
            if d % 7 == 0 {
                title.push(vocab[3].to_string());
            }
            if d == 444 {
                title.push(vocab[4].to_string());
            } // singleton df=1 term!
            let name = title.join(" ");
            sb.index_doc_terms(d, &title, &[], &title, &[]);
            if d % 4 != 0 {
                ps.public.insert(d);
            }
            cw.push_doc(&name, &[], &ext_map).unwrap();
            metas.push(MetaRow {
                published_epoch: 1_000_000 - d,
                created_epoch: 1_000_000 - d,
                content_type: CT_MOVIE,
                ..Default::default()
            });
        }
        sb.ct_ranges[CT_MOVIE as usize] = (0, num_docs);
        cw.finish(&metas, &[String::new()]).unwrap();
        ps.save(&dir, 5).unwrap();
        sb.build_and_save(&dir, num_docs).unwrap();

        let core = CorePlane::open(&dir, Some(5)).unwrap();
        let pp = PolicyPlane::open(&dir, &dir.join("pol.wal"), Some(5), 100, 1, 999_999).unwrap();
        let sp = CustomSearchPlane::open(&dir, &dir.join("l0.wal"), Some(5)).unwrap();
        let mask = pp.snapshot().base_eligible.clone();

        for q in [
            "alpha beta",
            "alpha beta gamma",
            "beta delta",
            "alpha epsilon",
            "gamma delta zeta",
        ] {
            for k in [1usize, 5, 25, 100] {
                let got_and = sp
                    .search_text(
                        q,
                        QueryMode::And,
                        None,
                        &pp,
                        PolicyTier::Base,
                        &core,
                        k,
                        100,
                        100,
                    )
                    .unwrap();
                let exp_and = sp.reference_exhaustive_search(q, true, &mask, k);
                assert_eq!(got_and, exp_and, "AND mismatch on q={q} k={k}");

                let got_or = sp
                    .search_text(
                        q,
                        QueryMode::Or,
                        None,
                        &pp,
                        PolicyTier::Base,
                        &core,
                        k,
                        100,
                        100,
                    )
                    .unwrap();
                let exp_or = sp.reference_exhaustive_search(q, false, &mask, k);
                assert_eq!(got_or, exp_or, "OR BMW mismatch on q={q} k={k}");
            }
        }

        let _ = std::fs::remove_dir_all(&dir);
    }

    proptest! {
        #[test]
        fn prop_or_and_tiebreak_never_loses_top_k(seed in 0u64..1000, k in 1usize..20) {
            let dir = std::env::temp_dir().join(format!("dhtms_prop_{}_{}", std::process::id(), seed));
            let _ = std::fs::remove_dir_all(&dir);
            std::fs::create_dir_all(&dir).unwrap();
            let num_docs = 300u32;
            let mut sb = CustomSearchBuilder::new(1, 77, num_docs, &dir, false);
            let mut ps = PolicyState::new(num_docs as usize);
            let ext_map = AHashMap::new();
            let mut cw = StreamingCoreWriter::new(&dir, 1, num_docs as usize, Vec::new()).unwrap();
            let mut metas = Vec::new();
            let words = ["w0", "w1", "w2", "w3"];
            let mut x = seed ^ 0x9E37_79B9;
            for d in 0..num_docs {
                x = x.wrapping_mul(6364136223846793005).wrapping_add(1);
                let mut toks = Vec::new();
                for (i, w) in words.iter().enumerate() {
                    if ((x >> (i * 4)) & 3) == 0 {
                        toks.push((*w).to_string());
                    }
                }
                sb.index_doc_terms(d, &toks, &[], &toks, &[]);
                if (x & 16) != 0 { ps.public.insert(d); }
                cw.push_doc(&toks.join(" "), &[], &ext_map).unwrap();
                metas.push(MetaRow::default());
            }
            cw.finish(&metas, &[String::new()]).unwrap();
            ps.save(&dir, 1).unwrap();
            sb.build_and_save(&dir, num_docs).unwrap();
            let core = CorePlane::open(&dir, Some(1)).unwrap();
            let pp = PolicyPlane::open(&dir, &dir.join("pol.wal"), Some(1), 10, 1, 999_999).unwrap();
            let sp = CustomSearchPlane::open(&dir, &dir.join("l0.wal"), Some(1)).unwrap();
            let mask = pp.snapshot().base_eligible.clone();

            let got_or = sp.search_text("w0 w1 w2", QueryMode::Or, None, &pp, PolicyTier::Base, &core, k, 10, 10).unwrap();
            let exp_or = sp.reference_exhaustive_search("w0 w1 w2", false, &mask, k);
            prop_assert_eq!(got_or, exp_or);

            let got_and = sp.search_text("w0 w1", QueryMode::And, None, &pp, PolicyTier::Base, &core, k, 10, 10).unwrap();
            let exp_and = sp.reference_exhaustive_search("w0 w1", true, &mask, k);
            prop_assert_eq!(got_and, exp_and);

            let _ = std::fs::remove_dir_all(&dir);
        }
    }
}
