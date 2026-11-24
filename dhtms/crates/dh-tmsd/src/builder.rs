use ahash::{AHashMap, AHashSet};
use dh_tms_core::{MetaRow, StreamingCoreWriter};
use dh_tms_format::catalog::{ContentRecord, MmapContentCatalog};
use dh_tms_format::{publish_generation, stage_generation_dir, CurrentManifest, FormatError};
use dh_tms_grammar::{
    evaluate_serving_adult, extract_trigrams, parse_release_name, tokenize_alnum, EpisodeSpec,
    NamePolicyMatcher, CT_UNKNOWN, CT_XXX,
};
use dh_tms_hashdir::{build_and_save_hash_dir, parse_info_hash_hex, InfoHash};
use dh_tms_policy::PolicyState;
use dh_tms_search::{trigram_key, CustomSearchBuilder};
use dh_tms_swarm::SwarmPlane;
use serde::Deserialize;
use std::cmp::Ordering;
use std::collections::BinaryHeap;
use std::fs::File;
use std::io::{BufRead, BufReader, BufWriter, Read, Write};
use std::path::{Path, PathBuf};
use std::time::{SystemTime, UNIX_EPOCH};

const MAX_FILE_TOKENS_PER_DOC: usize = 2048;
const MAX_GENERATIONS_TO_KEEP: usize = 2;
const DEFAULT_SORT_CHUNK_DOCS: usize = 200_000;
const DEFAULT_SORT_CHUNK_BYTES: usize = 128 * 1024 * 1024;

#[derive(Debug, Clone, Deserialize)]
pub struct BuildFileInput {
    pub path: String,
    #[serde(default)]
    pub size: u64,
    #[serde(default)]
    pub extension: String,
}

#[derive(Debug, Clone, Deserialize)]
pub struct BuildCatalogRecord {
    pub entity_id: u32,
    pub content_type: u8,
    pub source: String,
    pub id: String,
    pub title: String,
    #[serde(default)]
    pub original_title: String,
    #[serde(default)]
    pub year: u16,
    #[serde(default)]
    pub adult: bool,
    #[serde(default)]
    pub alt_identifiers: Vec<(String, String)>,
}

#[derive(Debug, Clone, Deserialize)]
pub struct BuildDocRecord {
    /// 40-hex v1 info_hash (or empty for pure-v2 torrents when info_hash_v2 is set).
    #[serde(default)]
    pub info_hash_v1: String,
    /// Optional 64-hex BEP-52 v2 info_hash (hybrid or pure-v2).
    #[serde(default)]
    pub info_hash_v2: String,
    pub name: String,
    #[serde(default)]
    pub size_bytes: u64,
    #[serde(default = "default_ct")]
    pub content_type: u8,
    #[serde(default)]
    pub entity_id: u32,
    #[serde(default)]
    pub year: u16,
    #[serde(default)]
    pub published_epoch: u32,
    #[serde(default)]
    pub created_epoch: u32,
    #[serde(default)]
    pub resolution: u8,
    #[serde(default)]
    pub is_anime: bool,
    #[serde(default)]
    pub is_3d: bool,
    #[serde(default)]
    pub video_3d: u8,
    #[serde(default)]
    pub video_source: u8,
    #[serde(default)]
    pub video_codec: u8,
    #[serde(default)]
    pub files: Vec<BuildFileInput>,
    #[serde(default)]
    pub is_private: bool,
    #[serde(default)]
    pub quarantined: bool,
    #[serde(default)]
    pub privacy_tainted: bool,
    #[serde(default)]
    pub excluded: bool,
    #[serde(default)]
    pub held: bool,
    /// When authoritative_policy is true, adult and name_blocked are taken verbatim from
    /// Go's serving/namepolicy evaluation (S2-2: parity by construction).
    #[serde(default)]
    pub authoritative_policy: bool,
    #[serde(default)]
    pub adult: bool,
    #[serde(default)]
    pub name_blocked: bool,
    #[serde(default)]
    pub pub_approved: bool,
    #[serde(default)]
    pub payload_expires_epoch: u32,
    #[serde(default = "default_neg1")]
    pub seeders: i32,
    #[serde(default = "default_neg1")]
    pub leechers: i32,
    #[serde(default)]
    pub checked_epoch_min: u32,
}

fn default_ct() -> u8 {
    CT_UNKNOWN
}

fn default_neg1() -> i32 {
    -1
}

fn default_true() -> bool {
    true
}

#[derive(Debug, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
enum NdjsonLine {
    Header {
        #[serde(default = "default_lsn")]
        pg_lsn: u64,
        #[serde(default = "default_lease_ttl")]
        lease_ttl_ms: u64,
        #[serde(default)]
        pg_lsn_text: String,
        #[serde(default = "default_true")]
        index_trigrams: bool,
        #[serde(default)]
        unique_hashes: bool,
        #[serde(default)]
        estimated_docs: usize,
    },
    Catalog(BuildCatalogRecord),
    Doc(BuildDocRecord),
}

fn default_lsn() -> u64 {
    1
}

fn default_lease_ttl() -> u64 {
    60_000
}

#[derive(Debug, Clone)]
pub struct BuildOutcome {
    pub manifest: CurrentManifest,
    pub pg_lsn: u64,
    pub lease_ttl_ms: u64,
}

fn now_sec() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(1_800_000_000)
}

/// Initialize a valid empty generation (`generation_id=0`, `num_docs=0`) so `dh-tmsd serve`
/// boots cleanly on a brand-new volume without manual fixture initialization.
pub fn build_empty_generation(out_root: &Path) -> Result<CurrentManifest, FormatError> {
    let gen_id = 0u64;
    let hash_seed = 0xD470_5EED_0000_0000u64;
    let term_seed = 0xD470_7E20_0000_0000u64;
    let now = now_sec();

    std::fs::create_dir_all(out_root)?;
    let staged_dir = stage_generation_dir(out_root, gen_id)?;
    MmapContentCatalog::build_and_save(&staged_dir.join("catalog.bin"), gen_id, &[])?;

    let core_w = StreamingCoreWriter::new(&staged_dir, gen_id, 0, Vec::new())?;
    core_w.finish(&[], &[String::new()])?;
    build_and_save_hash_dir(&staged_dir, gen_id, hash_seed, &[], &[])?;
    PolicyState::new(0).save(&staged_dir, gen_id)?;
    SwarmPlane::create(&staged_dir, gen_id, &[])
        .map_err(|e| FormatError::CorruptPayload(e.to_string()))?;
    let sb = CustomSearchBuilder::new(gen_id, term_seed, 0, &staged_dir, true);
    sb.build_and_save(&staged_dir, 0)?;

    let manifest = CurrentManifest {
        generation_id: gen_id,
        engine_kind: "custom-v0.3".into(),
        num_docs: 0,
        pg_lsn: "0/0".into(),
        created_epoch_sec: now,
        max_created_epoch_sec: now,
        hash_seed,
    };
    publish_generation(out_root, &staged_dir, &manifest)?;
    Ok(manifest)
}

struct ParsedDoc {
    doc: BuildDocRecord,
    v1: [u8; 20],
    has_v1: bool,
    v2: Option<[u8; 32]>,
    seq: u64,
}

impl ParsedDoc {
    fn est_bytes(&self) -> usize {
        let fbytes: usize = self
            .doc
            .files
            .iter()
            .map(|f| 32 + f.path.len() + f.extension.len())
            .sum();
        192 + self.doc.name.len() + fbytes
    }

    fn cmp_physical(&self, other: &Self) -> Ordering {
        self.doc
            .content_type
            .min(11)
            .cmp(&other.doc.content_type.min(11))
            .then_with(|| other.doc.published_epoch.cmp(&self.doc.published_epoch))
            .then_with(|| self.v1.cmp(&other.v1))
            .then_with(|| self.v2.cmp(&other.v2))
            .then_with(|| other.seq.cmp(&self.seq))
    }
}

fn parse_doc_record(mut d: BuildDocRecord, seq: u64) -> Result<ParsedDoc, FormatError> {
    let v1_opt = if d.info_hash_v1.is_empty() {
        None
    } else {
        match parse_info_hash_hex(&d.info_hash_v1) {
            Some(InfoHash::V1(b)) => Some(b),
            _ => {
                return Err(FormatError::CorruptPayload(format!(
                    "invalid info_hash_v1: {}",
                    d.info_hash_v1
                )))
            }
        }
    };
    let v2_opt = if d.info_hash_v2.is_empty() {
        None
    } else {
        match parse_info_hash_hex(&d.info_hash_v2) {
            Some(InfoHash::V2(b)) => Some(b),
            _ => {
                return Err(FormatError::CorruptPayload(format!(
                    "invalid info_hash_v2: {}",
                    d.info_hash_v2
                )))
            }
        }
    };
    if v1_opt.is_none() && v2_opt.is_none() {
        return Err(FormatError::CorruptPayload(
            "doc missing both info_hash_v1 and info_hash_v2".into(),
        ));
    }
    // Release raw hex strings immediately once parsed into fixed-size arrays.
    d.info_hash_v1 = String::new();
    d.info_hash_v2 = String::new();
    Ok(ParsedDoc {
        doc: d,
        v1: v1_opt.unwrap_or([0u8; 20]),
        has_v1: v1_opt.is_some(),
        v2: v2_opt,
        seq,
    })
}

fn write_u8<W: Write>(w: &mut W, v: u8) -> Result<(), FormatError> {
    w.write_all(&[v])?;
    Ok(())
}
fn write_u16<W: Write>(w: &mut W, v: u16) -> Result<(), FormatError> {
    w.write_all(&v.to_le_bytes())?;
    Ok(())
}
fn write_u32<W: Write>(w: &mut W, v: u32) -> Result<(), FormatError> {
    w.write_all(&v.to_le_bytes())?;
    Ok(())
}
fn write_i32<W: Write>(w: &mut W, v: i32) -> Result<(), FormatError> {
    w.write_all(&v.to_le_bytes())?;
    Ok(())
}
fn write_u64<W: Write>(w: &mut W, v: u64) -> Result<(), FormatError> {
    w.write_all(&v.to_le_bytes())?;
    Ok(())
}
fn write_str<W: Write>(w: &mut W, s: &str) -> Result<(), FormatError> {
    let b = s.as_bytes();
    write_u32(w, b.len() as u32)?;
    w.write_all(b)?;
    Ok(())
}

fn read_exact_or_eof<R: Read>(r: &mut R, buf: &mut [u8]) -> Result<bool, FormatError> {
    let mut off = 0usize;
    while off < buf.len() {
        let n = r.read(&mut buf[off..])?;
        if n == 0 {
            if off == 0 {
                return Ok(false);
            }
            return Err(FormatError::CorruptPayload(
                "truncated external sort run record".into(),
            ));
        }
        off += n;
    }
    Ok(true)
}

fn read_u8<R: Read>(r: &mut R) -> Result<u8, FormatError> {
    let mut b = [0u8; 1];
    r.read_exact(&mut b)?;
    Ok(b[0])
}
fn read_u16<R: Read>(r: &mut R) -> Result<u16, FormatError> {
    let mut b = [0u8; 2];
    r.read_exact(&mut b)?;
    Ok(u16::from_le_bytes(b))
}
fn read_u32<R: Read>(r: &mut R) -> Result<u32, FormatError> {
    let mut b = [0u8; 4];
    r.read_exact(&mut b)?;
    Ok(u32::from_le_bytes(b))
}
fn read_i32<R: Read>(r: &mut R) -> Result<i32, FormatError> {
    let mut b = [0u8; 4];
    r.read_exact(&mut b)?;
    Ok(i32::from_le_bytes(b))
}
fn read_u64<R: Read>(r: &mut R) -> Result<u64, FormatError> {
    let mut b = [0u8; 8];
    r.read_exact(&mut b)?;
    Ok(u64::from_le_bytes(b))
}
fn read_str<R: Read>(r: &mut R) -> Result<String, FormatError> {
    let len = read_u32(r)? as usize;
    let mut buf = vec![0u8; len];
    r.read_exact(&mut buf)?;
    String::from_utf8(buf).map_err(|e| FormatError::CorruptPayload(format!("utf8 sort run: {e}")))
}

fn write_parsed_doc<W: Write>(w: &mut W, p: &ParsedDoc) -> Result<(), FormatError> {
    write_u64(w, p.seq)?;
    let mut flags: u16 = 0;
    if p.has_v1 {
        flags |= 1 << 0;
    }
    if p.v2.is_some() {
        flags |= 1 << 1;
    }
    if p.doc.is_private {
        flags |= 1 << 2;
    }
    if p.doc.quarantined {
        flags |= 1 << 3;
    }
    if p.doc.privacy_tainted {
        flags |= 1 << 4;
    }
    if p.doc.excluded {
        flags |= 1 << 5;
    }
    if p.doc.held {
        flags |= 1 << 6;
    }
    if p.doc.authoritative_policy {
        flags |= 1 << 7;
    }
    if p.doc.adult {
        flags |= 1 << 8;
    }
    if p.doc.name_blocked {
        flags |= 1 << 9;
    }
    if p.doc.pub_approved {
        flags |= 1 << 10;
    }
    if p.doc.is_anime {
        flags |= 1 << 11;
    }
    if p.doc.is_3d {
        flags |= 1 << 12;
    }
    write_u16(w, flags)?;
    w.write_all(&p.v1)?;
    if let Some(v2) = p.v2 {
        w.write_all(&v2)?;
    }
    write_u8(w, p.doc.content_type)?;
    write_u8(w, p.doc.resolution)?;
    write_u8(w, p.doc.video_3d)?;
    write_u8(w, p.doc.video_source)?;
    write_u8(w, p.doc.video_codec)?;
    write_u16(w, p.doc.year)?;
    write_u32(w, p.doc.entity_id)?;
    write_u32(w, p.doc.published_epoch)?;
    write_u32(w, p.doc.created_epoch)?;
    write_u64(w, p.doc.size_bytes)?;
    write_u32(w, p.doc.payload_expires_epoch)?;
    write_i32(w, p.doc.seeders)?;
    write_i32(w, p.doc.leechers)?;
    write_u32(w, p.doc.checked_epoch_min)?;
    write_str(w, &p.doc.name)?;
    write_u32(w, p.doc.files.len() as u32)?;
    for f in &p.doc.files {
        write_str(w, &f.path)?;
        write_u64(w, f.size)?;
        write_str(w, &f.extension)?;
    }
    Ok(())
}

fn read_parsed_doc<R: Read>(r: &mut R) -> Result<Option<ParsedDoc>, FormatError> {
    let mut seq_b = [0u8; 8];
    if !read_exact_or_eof(r, &mut seq_b)? {
        return Ok(None);
    }
    let seq = u64::from_le_bytes(seq_b);
    let flags = read_u16(r)?;
    let has_v1 = (flags & (1 << 0)) != 0;
    let has_v2 = (flags & (1 << 1)) != 0;
    let mut v1 = [0u8; 20];
    r.read_exact(&mut v1)?;
    let v2 = if has_v2 {
        let mut b = [0u8; 32];
        r.read_exact(&mut b)?;
        Some(b)
    } else {
        None
    };
    let content_type = read_u8(r)?;
    let resolution = read_u8(r)?;
    let video_3d = read_u8(r)?;
    let video_source = read_u8(r)?;
    let video_codec = read_u8(r)?;
    let year = read_u16(r)?;
    let entity_id = read_u32(r)?;
    let published_epoch = read_u32(r)?;
    let created_epoch = read_u32(r)?;
    let size_bytes = read_u64(r)?;
    let payload_expires_epoch = read_u32(r)?;
    let seeders = read_i32(r)?;
    let leechers = read_i32(r)?;
    let checked_epoch_min = read_u32(r)?;
    let name = read_str(r)?;
    let fcnt = read_u32(r)? as usize;
    let mut files = Vec::with_capacity(fcnt.min(1024));
    for _ in 0..fcnt {
        let path = read_str(r)?;
        let size = read_u64(r)?;
        let extension = read_str(r)?;
        files.push(BuildFileInput {
            path,
            size,
            extension,
        });
    }
    Ok(Some(ParsedDoc {
        doc: BuildDocRecord {
            info_hash_v1: String::new(),
            info_hash_v2: String::new(),
            name,
            size_bytes,
            content_type,
            entity_id,
            year,
            published_epoch,
            created_epoch,
            resolution,
            is_anime: (flags & (1 << 11)) != 0,
            is_3d: (flags & (1 << 12)) != 0,
            video_3d,
            video_source,
            video_codec,
            files,
            is_private: (flags & (1 << 2)) != 0,
            quarantined: (flags & (1 << 3)) != 0,
            privacy_tainted: (flags & (1 << 4)) != 0,
            excluded: (flags & (1 << 5)) != 0,
            held: (flags & (1 << 6)) != 0,
            authoritative_policy: (flags & (1 << 7)) != 0,
            adult: (flags & (1 << 8)) != 0,
            name_blocked: (flags & (1 << 9)) != 0,
            pub_approved: (flags & (1 << 10)) != 0,
            payload_expires_epoch,
            seeders,
            leechers,
            checked_epoch_min,
        },
        v1,
        has_v1,
        v2,
        seq,
    }))
}

struct TempRunsGuard {
    paths: Vec<PathBuf>,
}

impl Drop for TempRunsGuard {
    fn drop(&mut self) {
        for p in &self.paths {
            let _ = std::fs::remove_file(p);
        }
    }
}

struct MergeEntry {
    doc: ParsedDoc,
    run_idx: usize,
}

impl PartialEq for MergeEntry {
    fn eq(&self, other: &Self) -> bool {
        self.doc.cmp_physical(&other.doc) == Ordering::Equal && self.run_idx == other.run_idx
    }
}
impl Eq for MergeEntry {}
impl PartialOrd for MergeEntry {
    fn partial_cmp(&self, other: &Self) -> Option<Ordering> {
        Some(self.cmp(other))
    }
}
impl Ord for MergeEntry {
    fn cmp(&self, other: &Self) -> Ordering {
        // Min-heap on physical order via BinaryHeap (max-heap): reverse comparator!
        other
            .doc
            .cmp_physical(&self.doc)
            .then_with(|| other.run_idx.cmp(&self.run_idx))
    }
}

fn spill_sort_run(
    staged_dir: &Path,
    run_idx: usize,
    chunk: &mut Vec<ParsedDoc>,
) -> Result<PathBuf, FormatError> {
    chunk.sort_unstable_by(|a, b| a.cmp_physical(b));
    let path = staged_dir.join(format!("_sort_run_{run_idx:05}.bin"));
    let mut w = BufWriter::with_capacity(4 * 1024 * 1024, File::create(&path)?);
    for p in chunk.iter() {
        write_parsed_doc(&mut w, p)?;
    }
    w.flush()?;
    chunk.clear();
    Ok(path)
}

/// Build and atomically publish a generation from an NDJSON stream of header, catalog, and doc lines
/// using bounded-RAM external disk-spilled chunk sorting and streaming posting flushes.
pub fn build_generation_from_ndjson<R: BufRead>(
    reader: R,
    out_root: &Path,
    next_gen_id: u64,
) -> Result<BuildOutcome, FormatError> {
    build_generation_from_ndjson_with_limits(
        reader,
        out_root,
        next_gen_id,
        DEFAULT_SORT_CHUNK_DOCS,
        DEFAULT_SORT_CHUNK_BYTES,
    )
}

pub(crate) fn build_generation_from_ndjson_with_limits<R: BufRead>(
    reader: R,
    out_root: &Path,
    next_gen_id: u64,
    chunk_max_docs: usize,
    chunk_max_bytes: usize,
) -> Result<BuildOutcome, FormatError> {
    let mut pg_lsn = 1u64;
    let mut lease_ttl_ms = 60_000u64;
    let mut pg_lsn_text = String::new();
    let mut index_trigrams = true;
    let mut unique_hashes = false;
    let mut estimated_docs = 0usize;
    let mut catalog_recs: Vec<ContentRecord> = Vec::new();

    std::fs::create_dir_all(out_root)?;
    let staged_dir = stage_generation_dir(out_root, next_gen_id)?;
    let mut temp_runs = TempRunsGuard { paths: Vec::new() };
    let mut chunk: Vec<ParsedDoc> = Vec::new();
    let mut chunk_bytes = 0usize;
    let mut seq = 0u64;

    for (line_no, line_res) in reader.lines().enumerate() {
        let line = line_res?;
        let trimmed = line.trim();
        if trimmed.is_empty() {
            continue;
        }
        let parsed: NdjsonLine = serde_json::from_str(trimmed).map_err(|e| {
            FormatError::CorruptPayload(format!("ndjson line {}: {e}", line_no + 1))
        })?;
        match parsed {
            NdjsonLine::Header {
                pg_lsn: lsn,
                lease_ttl_ms: ttl,
                pg_lsn_text: txt,
                index_trigrams: idx_tri,
                unique_hashes: uniq,
                estimated_docs: est,
            } => {
                pg_lsn = lsn;
                lease_ttl_ms = ttl.max(1_000);
                pg_lsn_text = txt;
                index_trigrams = idx_tri;
                unique_hashes = uniq;
                estimated_docs = est;
            }
            NdjsonLine::Catalog(c) => {
                catalog_recs.push(ContentRecord {
                    entity_id: c.entity_id,
                    content_type: c.content_type,
                    source: c.source,
                    id: c.id,
                    title: c.title,
                    original_title: c.original_title,
                    year: c.year,
                    adult: c.adult,
                    strict_title_hashes: vec![],
                    family_hashes: vec![],
                    alt_identifiers: c.alt_identifiers,
                });
            }
            NdjsonLine::Doc(d) => {
                seq += 1;
                let pd = parse_doc_record(d, seq)?;
                chunk_bytes = chunk_bytes.saturating_add(pd.est_bytes());
                chunk.push(pd);
                if chunk.len() >= chunk_max_docs || chunk_bytes >= chunk_max_bytes {
                    let run_path = spill_sort_run(&staged_dir, temp_runs.paths.len(), &mut chunk)?;
                    temp_runs.paths.push(run_path);
                    chunk_bytes = 0;
                }
            }
        }
    }

    let total_seen = seq as usize;
    let cap_hint = if estimated_docs > 0 {
        estimated_docs
    } else {
        total_seen
    };
    let final_lsn_text = if pg_lsn_text.is_empty() {
        format!("0/{pg_lsn:X}")
    } else {
        pg_lsn_text
    };

    let manifest = if temp_runs.paths.is_empty() {
        // Fast in-memory path for <= chunk_max_docs (zero temp run files).
        if !unique_hashes {
            // Preserve exact last-write-wins dedup before physical sort.
            let mut seen_v1: AHashSet<[u8; 20]> = AHashSet::with_capacity(chunk.len());
            let mut seen_v2: AHashSet<[u8; 32]> = AHashSet::new();
            let mut deduped: Vec<ParsedDoc> = Vec::with_capacity(chunk.len());
            for pd in chunk.drain(..).rev() {
                if pd.has_v1 && !seen_v1.insert(pd.v1) {
                    continue;
                }
                if let Some(v2) = pd.v2 {
                    if !seen_v2.insert(v2) {
                        continue;
                    }
                }
                deduped.push(pd);
            }
            deduped.sort_unstable_by(|a, b| a.cmp_physical(b));
            build_generation_streaming(
                out_root,
                &staged_dir,
                next_gen_id,
                final_lsn_text,
                &catalog_recs,
                deduped.len(),
                index_trigrams,
                true,
                deduped.into_iter().map(Ok),
                temp_runs,
            )?
        } else {
            chunk.sort_unstable_by(|a, b| a.cmp_physical(b));
            let n = chunk.len();
            build_generation_streaming(
                out_root,
                &staged_dir,
                next_gen_id,
                final_lsn_text,
                &catalog_recs,
                n,
                index_trigrams,
                true,
                chunk.into_iter().map(Ok),
                temp_runs,
            )?
        }
    } else {
        if !chunk.is_empty() {
            let run_path = spill_sort_run(&staged_dir, temp_runs.paths.len(), &mut chunk)?;
            temp_runs.paths.push(run_path);
        }
        drop(chunk);

        let mut readers: Vec<BufReader<File>> = Vec::with_capacity(temp_runs.paths.len());
        for rp in &temp_runs.paths {
            readers.push(BufReader::with_capacity(256 * 1024, File::open(rp)?));
        }
        let mut heap: BinaryHeap<MergeEntry> = BinaryHeap::with_capacity(readers.len());
        for (ri, r) in readers.iter_mut().enumerate() {
            if let Some(doc) = read_parsed_doc(r)? {
                heap.push(MergeEntry { doc, run_idx: ri });
            }
        }
        let merge_iter = std::iter::from_fn(move || {
            let entry = heap.pop()?;
            match read_parsed_doc(&mut readers[entry.run_idx]) {
                Ok(Some(next_doc)) => heap.push(MergeEntry {
                    doc: next_doc,
                    run_idx: entry.run_idx,
                }),
                Ok(None) => {}
                Err(e) => return Some(Err(e)),
            }
            Some(Ok(entry.doc))
        });

        build_generation_streaming(
            out_root,
            &staged_dir,
            next_gen_id,
            final_lsn_text,
            &catalog_recs,
            cap_hint,
            index_trigrams,
            unique_hashes,
            merge_iter,
            temp_runs,
        )?
    };

    Ok(BuildOutcome {
        manifest,
        pg_lsn,
        lease_ttl_ms,
    })
}

#[allow(clippy::too_many_arguments)]
fn build_generation_streaming<I>(
    out_root: &Path,
    staged_dir: &Path,
    gen_id: u64,
    pg_lsn_text: String,
    catalog_recs: &[ContentRecord],
    cap_hint: usize,
    index_trigrams: bool,
    unique_hashes: bool,
    docs_iter: I,
    temp_runs: TempRunsGuard,
) -> Result<CurrentManifest, FormatError>
where
    I: Iterator<Item = Result<ParsedDoc, FormatError>>,
{
    let now = now_sec();
    let hash_seed = 0xD470_5EED_0000_0000u64 ^ gen_id.wrapping_mul(0x9E37_79B9_7F4A_7C15);
    let term_seed = 0xD470_7E20_0000_0000u64 ^ gen_id.wrapping_mul(0xBF58_476D_1CE4_E5B9);

    MmapContentCatalog::build_and_save(&staged_dir.join("catalog.bin"), gen_id, catalog_recs)?;

    let mut entity_tokens: AHashMap<u32, Vec<String>> = AHashMap::with_capacity(catalog_recs.len());
    for c in catalog_recs {
        let mut t = tokenize_alnum(&c.title);
        if !c.original_title.is_empty() {
            t.extend(tokenize_alnum(&c.original_title));
        }
        entity_tokens.insert(c.entity_id, t);
    }

    let mut ext_vocab: Vec<String> = vec![String::new()];
    let mut ext_map: AHashMap<String, u16> = AHashMap::new();
    ext_map.insert(String::new(), 0);

    let mut core_w = StreamingCoreWriter::new(staged_dir, gen_id, cap_hint, Vec::new())?;
    let mut sb = CustomSearchBuilder::new(
        gen_id,
        term_seed,
        cap_hint as u32,
        staged_dir,
        index_trigrams,
    );
    let mut ps = PolicyState::new(0);
    ps.payload_expires.reserve(cap_hint);
    let mut v1_hashes: Vec<[u8; 20]> = Vec::with_capacity(cap_hint);
    let mut v2_aliases: Vec<(u32, [u8; 32])> = Vec::new();
    let mut metas: Vec<MetaRow> = Vec::with_capacity(cap_hint);
    let mut swarm_init: Vec<(i32, i32, u32)> = Vec::with_capacity(cap_hint);
    let mut max_created = 0u64;

    let mut seen_v1: AHashSet<[u8; 20]> = if unique_hashes {
        AHashSet::new()
    } else {
        AHashSet::with_capacity(cap_hint)
    };
    let mut seen_v2: AHashSet<[u8; 32]> = AHashSet::new();
    let mut last_key: Option<(bool, [u8; 20], Option<[u8; 32]>)> = None;

    let np = NamePolicyMatcher::global();
    let mut file_tuples: Vec<(String, u64, String)> = Vec::new();
    let mut file_toks: Vec<String> = Vec::new();
    let mut cur_ct: Option<u8> = None;
    let mut doc_idx: usize = 0;

    for pd_res in docs_iter {
        let pd = pd_res?;
        if unique_hashes {
            let key = (pd.has_v1, pd.v1, pd.v2);
            if last_key.as_ref() == Some(&key) {
                continue;
            }
            last_key = Some(key);
        } else {
            if pd.has_v1 && !seen_v1.insert(pd.v1) {
                continue;
            }
            if let Some(v2) = pd.v2 {
                if !seen_v2.insert(v2) {
                    continue;
                }
            }
        }

        let d = doc_idx as u32;
        let doc = pd.doc;
        let ct = doc.content_type.min(11);
        if cur_ct != Some(ct) {
            if let Some(prev) = cur_ct {
                sb.ct_ranges[prev as usize].1 = d;
            }
            sb.ct_ranges[ct as usize].0 = d;
            cur_ct = Some(ct);
        }

        v1_hashes.push(pd.v1);
        if let Some(v2) = pd.v2 {
            v2_aliases.push((d, v2));
        }

        file_tuples.clear();
        file_tuples.reserve(doc.files.len());
        let mut files_sum = 0u64;
        for f in &doc.files {
            files_sum = files_sum.saturating_add(f.size);
            let ext = f.extension.to_ascii_lowercase();
            if !ext_map.contains_key(&ext) && ext_vocab.len() < u16::MAX as usize {
                let id = ext_vocab.len() as u16;
                ext_vocab.push(ext.clone());
                ext_map.insert(ext.clone(), id);
            }
            file_tuples.push((f.path.clone(), f.size, ext));
        }
        let total_size = if doc.size_bytes > 0 {
            doc.size_bytes
        } else {
            files_sum
        };

        let parsed = parse_release_name(&doc.name);
        let (is_adult, is_name_blocked) = if doc.authoritative_policy {
            (doc.adult, doc.name_blocked)
        } else {
            let ad = evaluate_serving_adult(
                &doc.name,
                total_size,
                "",
                &file_tuples,
                ct == CT_XXX,
                false,
            );
            let nb = np.is_blocked(&doc.name);
            (ad || doc.adult, nb || doc.name_blocked)
        };

        if !doc.is_private {
            ps.public.insert(d);
        }
        if doc.quarantined {
            ps.quarantined.insert(d);
        }
        if doc.privacy_tainted {
            ps.privacy_tainted.insert(d);
        }
        if doc.excluded {
            ps.excluded.insert(d);
        }
        if doc.held {
            ps.held.insert(d);
        }
        if is_adult {
            ps.adult.insert(d);
        }
        if is_name_blocked {
            ps.name_blocked.insert(d);
        }
        if doc.pub_approved {
            ps.pub_approved.insert(d);
        }
        ps.payload_expires.push(doc.payload_expires_epoch);

        let created_ep = if doc.created_epoch > 0 {
            doc.created_epoch
        } else {
            doc.published_epoch
        };
        max_created = max_created.max(created_ep as u64);

        let res = if doc.resolution != 0 {
            doc.resolution
        } else {
            parsed.resolution
        };
        let v3d = if doc.video_3d != 0 {
            doc.video_3d.min(3)
        } else if doc.is_3d {
            1
        } else {
            0
        };
        let (src_code, codec_code, is_anime) = if doc.authoritative_policy {
            (
                doc.video_source.min(15),
                doc.video_codec.min(15),
                doc.is_anime,
            )
        } else {
            (
                if doc.video_source != 0 {
                    doc.video_source.min(15)
                } else {
                    parsed.source.min(15)
                },
                if doc.video_codec != 0 {
                    doc.video_codec.min(15)
                } else {
                    parsed.codec.min(15)
                },
                doc.is_anime || parsed.is_anime_cue,
            )
        };
        let meta = MetaRow {
            size_bytes: total_size,
            entity_id: doc.entity_id,
            published_epoch: doc.published_epoch,
            created_epoch: created_ep,
            files_count: file_tuples.len() as u32,
            year: if doc.year != 0 { doc.year } else { parsed.year },
            flags_src: MetaRow::pack_flags_v2(
                src_code,
                codec_code,
                is_anime,
                v3d,
                true,
                false,
                parsed.hdr_flags != 0,
                !file_tuples.is_empty(),
            ),
            single_ext_id: 0,
            content_type: ct,
            resolution: res,
        };
        metas.push(meta);
        swarm_init.push((doc.seeders, doc.leechers, doc.checked_epoch_min));
        core_w.push_doc(&doc.name, &file_tuples, &ext_map)?;

        sb.facet_ct[ct as usize].insert(d);
        if (res as usize) < 10 {
            sb.facet_res[res as usize].insert(d);
        }
        if is_anime {
            sb.facet_anime.insert(d);
        }
        if v3d != 0 {
            sb.facet_3d.insert(d);
        }
        if src_code != 0 && (src_code as usize) < sb.facet_src.len() {
            sb.facet_src[src_code as usize].insert(d);
        }
        if codec_code != 0 && (codec_code as usize) < sb.facet_codec.len() {
            sb.facet_codec[codec_code as usize].insert(d);
        }
        if doc.entity_id != 0 {
            sb.facet_linked.insert(d);
            sb.entity_to_docs
                .entry(doc.entity_id)
                .or_default()
                .insert(d);
        }
        if created_ep > 0 {
            sb.created_day_to_docs
                .entry(created_ep / 86_400)
                .or_default()
                .insert(d);
        }
        for &sh in &parsed.strict_title_hashes {
            sb.strict_title_to_docs.entry(sh).or_default().insert(d);
        }
        for &fh in &parsed.family_hashes {
            sb.family_title_to_docs.entry(fh).or_default().insert(d);
        }
        let eps: Vec<EpisodeSpec> = parsed.episodes.clone();
        sb.index_doc_episodes(d, doc.entity_id, &eps);

        let ent_toks: &[String] = entity_tokens
            .get(&doc.entity_id)
            .map(Vec::as_slice)
            .unwrap_or(&[]);
        // Index all file tokens up to a generous per-doc cap so multi-file packs (>12 files)
        // achieve full PG tsv text recall parity without mega-torrent memory blowup.
        file_toks.clear();
        for (fp, _, _) in &file_tuples {
            if file_toks.len() >= MAX_FILE_TOKENS_PER_DOC {
                break;
            }
            for t in tokenize_alnum(fp) {
                file_toks.push(t);
                if file_toks.len() >= MAX_FILE_TOKENS_PER_DOC {
                    break;
                }
            }
        }
        sb.index_doc_terms(
            d,
            &parsed.title_tokens,
            ent_toks,
            &parsed.raw_tokens,
            &file_toks,
        );
        // Spill posting runs periodically so peak RAM stays bounded at 30M+ torrents.
        sb.flush_run_if_needed()?;

        if index_trigrams {
            for tri in extract_trigrams(&doc.name) {
                sb.trigram_to_docs
                    .entry(trigram_key(tri))
                    .or_default()
                    .insert(d);
            }
        }
        doc_idx += 1;
    }

    if let Some(prev) = cur_ct {
        sb.ct_ranges[prev as usize].1 = doc_idx as u32;
    }
    ps.num_docs = doc_idx;

    // Release temporary sort files and dedup/entity maps before disk finalization.
    drop(temp_runs);
    drop(seen_v1);
    drop(seen_v2);
    drop(entity_tokens);

    // Save and immediately drop each large structure sequentially to minimize peak RSS.
    core_w.finish(&metas, &ext_vocab)?;
    drop(metas);
    drop(ext_vocab);
    drop(ext_map);

    build_and_save_hash_dir(staged_dir, gen_id, hash_seed, &v1_hashes, &v2_aliases)?;
    drop(v1_hashes);
    drop(v2_aliases);

    ps.save(staged_dir, gen_id)?;
    drop(ps);

    SwarmPlane::create(staged_dir, gen_id, &swarm_init)
        .map_err(|e| FormatError::CorruptPayload(e.to_string()))?;
    drop(swarm_init);

    sb.build_and_save(staged_dir, doc_idx as u32)?;

    let manifest = CurrentManifest {
        generation_id: gen_id,
        engine_kind: "custom-v0.3".into(),
        num_docs: doc_idx as u64,
        pg_lsn: pg_lsn_text,
        created_epoch_sec: now,
        max_created_epoch_sec: if max_created > 0 { max_created } else { now },
        hash_seed,
    };
    publish_generation(out_root, staged_dir, &manifest)?;
    prune_old_generations(out_root, gen_id);
    Ok(manifest)
}

fn prune_old_generations(out_root: &Path, current_gen: u64) {
    let gens_dir = out_root.join("generations");
    let Ok(entries) = std::fs::read_dir(&gens_dir) else {
        return;
    };
    let mut dirs: Vec<(u64, std::path::PathBuf)> = Vec::new();
    for ent in entries.flatten() {
        let name = ent.file_name();
        let Some(s) = name.to_str() else { continue };
        if let Some(hex_id) = s.strip_prefix("gen-") {
            if let Ok(gid) = u64::from_str_radix(hex_id, 16) {
                dirs.push((gid, ent.path()));
            }
        }
    }
    dirs.sort_by_key(|(gid, _)| *gid);
    if dirs.len() <= MAX_GENERATIONS_TO_KEEP {
        return;
    }
    let to_remove = dirs.len() - MAX_GENERATIONS_TO_KEEP;
    for (gid, path) in dirs.into_iter().take(to_remove) {
        if gid != current_gen {
            let _ = std::fs::remove_dir_all(path);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::engine::DaemonEngine;
    use std::io::Cursor;

    #[test]
    fn test_external_sort_spill_matches_in_memory_and_cleans_runs() {
        let base = std::env::temp_dir().join(format!("dhtms_spill_test_{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&base);
        let dir_mem = base.join("mem");
        let dir_spill = base.join("spill");
        let wal_spill = base.join("wal_spill");

        let mut ndjson = String::new();
        ndjson.push_str(
            r#"{"kind":"header","pg_lsn":42,"lease_ttl_ms":60000,"pg_lsn_text":"0/2A","index_trigrams":false,"unique_hashes":true}"#,
        );
        ndjson.push('\n');
        for i in 0u32..25 {
            let ct = (i % 3) + 1;
            let pub_ep = 1_700_000_000 + ((i * 37) % 1000);
            let h1 = format!("{:040x}", i + 1);
            ndjson.push_str(&format!(
                r#"{{"kind":"doc","info_hash_v1":"{h1}","name":"Spill.Title.{i}.1080p","size_bytes":1000,"content_type":{ct},"published_epoch":{pub_ep},"created_epoch":{pub_ep},"files":[{{"path":"video_{i}.mkv","size":1000,"extension":"mkv"}}]}}"#
            ));
            ndjson.push('\n');
        }

        let out_mem = build_generation_from_ndjson_with_limits(
            Cursor::new(&ndjson),
            &dir_mem,
            1,
            1000,
            10_000_000,
        )
        .unwrap();
        let out_spill = build_generation_from_ndjson_with_limits(
            Cursor::new(&ndjson),
            &dir_spill,
            1,
            4,
            10_000_000,
        )
        .unwrap();

        assert_eq!(out_mem.manifest.num_docs, 25);
        assert_eq!(out_spill.manifest.num_docs, 25);

        // Verify zero leftover _sort_run_* temp files in published generation dir.
        let gen_dir = dir_spill.join("generations/gen-0000000000000001");
        for ent in std::fs::read_dir(&gen_dir).unwrap() {
            let n = ent.unwrap().file_name().to_string_lossy().to_string();
            assert!(!n.starts_with("_sort_run_"), "leftover temp run file: {n}");
        }

        // Open spilled generation and verify search + trigrams_disabled behavior.
        let eng = DaemonEngine::open(&dir_spill, &wal_spill, 42, 60_000).unwrap();
        let sr = eng
            .handle_search(crate::engine::SearchRequest {
                query: "Spill Title 17".into(),
                mode: "and".into(),
                tier: "base".into(),
                limit: 10,
                tv_season: None,
                tv_episode: None,
                include_facets: false,
                include_files: true,
                any_clauses: vec![],
                min_seeders: None,
                now_epoch: None,
            })
            .unwrap();
        assert_eq!(sr.docs.len(), 1);
        assert_eq!(sr.docs[0].name, "Spill.Title.17.1080p");

        let sub_err = eng.handle_substring(crate::engine::SubstringRequest {
            substring: "Title".into(),
            tier: "base".into(),
            limit: 5,
            include_files: false,
            now_epoch: None,
        });
        assert!(matches!(
            sub_err,
            Err(crate::engine::DaemonError::TrigramsDisabled)
        ));

        let _ = std::fs::remove_dir_all(&base);
    }

    #[test]
    fn test_structured_filter_clauses_3d_anime_source_codec_and_l0_wal_roundtrip() {
        use crate::engine::{
            FilterClause, L0UpsertItem, L0UpsertRequest, RssRequest, SearchRequest,
        };

        let base =
            std::env::temp_dir().join(format!("dhtms_facets_v2_test_{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&base);
        let db = base.join("db");
        let wal = base.join("wal");

        let now = 1_800_000_000u32;
        let h_3d = format!("{:040x}", 1);
        let h_anime = format!("{:040x}", 2);
        let h_plain = format!("{:040x}", 3);
        let h_l0_anime = format!("{:040x}", 4);

        let ndjson = format!(
            concat!(
                r#"{{"kind":"header","pg_lsn":10,"lease_ttl_ms":60000,"pg_lsn_text":"0/A","index_trigrams":false,"unique_hashes":true}}"#,
                "
",
                r#"{{"kind":"doc","info_hash_v1":"{h_3d}","name":"Avatar.2009.1080p.3D.SBS.BluRay.x265","size_bytes":10000,"content_type":1,"resolution":6,"video_3d":2,"video_source":9,"video_codec":3,"published_epoch":{t1},"created_epoch":{t1},"authoritative_policy":true,"pub_approved":true,"seeders":25,"leechers":3}}"#,
                "
",
                r#"{{"kind":"doc","info_hash_v1":"{h_anime}","name":"[SubsPlease] Frieren - 01 (1080p) [AV1]","size_bytes":2000,"content_type":2,"resolution":6,"is_anime":true,"video_source":7,"video_codec":9,"published_epoch":{t2},"created_epoch":{t2},"authoritative_policy":true,"pub_approved":true,"seeders":50,"leechers":2}}"#,
                "
",
                r#"{{"kind":"doc","info_hash_v1":"{h_plain}","name":"Plain.Show.S01E01.720p.WEBRip.H264","size_bytes":1500,"content_type":2,"resolution":5,"is_anime":false,"video_source":8,"video_codec":1,"published_epoch":{t3},"created_epoch":{t3},"authoritative_policy":true,"pub_approved":true,"seeders":2,"leechers":1}}"#,
                "
"
            ),
            h_3d = h_3d,
            h_anime = h_anime,
            h_plain = h_plain,
            t1 = now - 30,
            t2 = now - 20,
            t3 = now - 10,
        );

        build_generation_from_ndjson_with_limits(Cursor::new(&ndjson), &db, 1, 2, 10_000_000)
            .unwrap();

        {
            let eng = DaemonEngine::open(&db, &wal, 10, 60_000).unwrap();

            // 1. 2060 (Movies 3D) filter returns only the 3D movie with exact video_3d=2, source=9, codec=3.
            let r3d = eng
                .handle_rss(RssRequest {
                    content_type: None,
                    tier: "base".into(),
                    limit: 10,
                    offset: 0,
                    include_files: false,
                    include_total: true,
                    any_clauses: vec![FilterClause {
                        content_type: Some(1),
                        require_3d: true,
                        ..Default::default()
                    }],
                    min_seeders: None,
                    now_epoch: None,
                })
                .unwrap();
            assert_eq!(r3d.docs.len(), 1);
            assert_eq!(r3d.total_count, Some(1));
            assert_eq!(r3d.docs[0].video_3d, 2);
            assert_eq!(r3d.docs[0].video_source, 9);
            assert_eq!(r3d.docs[0].video_codec, 3);

            // 2. 5070 (TV Anime) filter returns only the anime TV release with is_anime=true, codec=9 (AV1).
            let ranime = eng
                .handle_rss(RssRequest {
                    content_type: None,
                    tier: "base".into(),
                    limit: 10,
                    offset: 0,
                    include_files: false,
                    include_total: true,
                    any_clauses: vec![FilterClause {
                        content_type: Some(2),
                        require_anime: true,
                        ..Default::default()
                    }],
                    min_seeders: None,
                    now_epoch: None,
                })
                .unwrap();
            assert_eq!(ranime.docs.len(), 1);
            assert_eq!(ranime.total_count, Some(1));
            assert!(ranime.docs[0].is_anime);
            assert_eq!(ranime.docs[0].video_codec, 9);

            // 3. Combined video_source + video_codec + min_seeders on SearchRequest.
            let sr = eng
                .handle_search(SearchRequest {
                    query: "1080p".into(),
                    mode: "and".into(),
                    tier: "base".into(),
                    limit: 10,
                    tv_season: None,
                    tv_episode: None,
                    include_facets: true,
                    include_files: false,
                    any_clauses: vec![FilterClause {
                        video_source: Some(9),
                        video_codec: Some(3),
                        ..Default::default()
                    }],
                    min_seeders: Some(20),
                    now_epoch: None,
                })
                .unwrap();
            assert_eq!(sr.docs.len(), 1);
            assert_eq!(sr.total_facets, Some(1));
            assert_eq!(sr.docs[0].info_hash_v1.as_deref(), Some(h_3d.as_str()));

            // 4. Live L0 dynamic anime insert persists flags through WAL replay.
            let up = eng
                .handle_l0_upsert(L0UpsertRequest {
                    docs: vec![L0UpsertItem {
                        doc_id: None,
                        info_hash_v1: Some(h_l0_anime.clone()),
                        info_hash_v2: None,
                        name: "[Erai-raws] Dandadan - 02 [1080p][HEVC]".into(),
                        size_bytes: 1800,
                        content_type: 2,
                        resolution: 6,
                        is_anime: true,
                        is_3d: false,
                        video_3d: 0,
                        video_source: 7,
                        video_codec: 8,
                        entity_id: 0,
                        year: 2026,
                        published_epoch: now,
                        created_epoch: now,
                        files_count: 1,
                        file_paths: vec![],
                        files: vec![],
                    }],
                    sync: true,
                })
                .unwrap();
            let dyn_id = up.doc_ids[0];
            let grant = serde_json::json!({
                "ops": [{"op": "grant_base", "doc_id": dyn_id}],
                "pg_lsn": 11,
                "sync": true,
                "rebuild_snapshot": true
            });
            eng.handle_policy_batch(&serde_json::to_vec(&grant).unwrap())
                .unwrap();
        }

        // Reopen from WAL and verify dynamic L0 anime doc matches 5070 filter and preserves flags!
        let eng2 = DaemonEngine::open(&db, &wal, 11, 60_000).unwrap();
        let ranime2 = eng2
            .handle_rss(RssRequest {
                content_type: None,
                tier: "base".into(),
                limit: 10,
                offset: 0,
                include_files: false,
                include_total: true,
                any_clauses: vec![FilterClause {
                    content_type: Some(2),
                    require_anime: true,
                    ..Default::default()
                }],
                min_seeders: None,
                now_epoch: None,
            })
            .unwrap();
        assert_eq!(ranime2.docs.len(), 2);
        assert_eq!(ranime2.total_count, Some(2));
        assert_eq!(
            ranime2.docs[0].info_hash_v1.as_deref(),
            Some(h_l0_anime.as_str())
        );
        assert!(ranime2.docs[0].is_anime);
        assert_eq!(ranime2.docs[0].video_codec, 8);

        let _ = std::fs::remove_dir_all(&base);
    }
}
