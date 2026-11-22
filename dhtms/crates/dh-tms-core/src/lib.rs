use ahash::AHashMap;
use crc32fast::Hasher as Crc32;
use dh_tms_format::{
    read_varint_u32, read_varint_u64, write_checked_bytes, write_checked_pod_slice,
    write_varint_u32, write_varint_u64, CheckedFileWriter, CheckedMmap, FileKind, FormatError, Pod,
};
use rayon::prelude::*;
use std::cell::RefCell;
use std::path::{Path, PathBuf};

pub const MAX_RECORDS_PER_TREE_BLOCK: usize = 16;
pub const MAX_RAW_TREE_BLOCK_BYTES: usize = 256 * 1024; // 256 KiB byte-cap per multi-record block
pub const MAX_SAFE_DECOMPRESS_BYTES: usize = 128 * 1024 * 1024; // 128 MiB guard against corrupt headers
pub const NO_TREE_LOC: u32 = u32::MAX;

const BLK_FLAG_USES_DICT: u32 = 1 << 0;
const BLK_FLAG_OVERFLOW_SINGLE: u32 = 1 << 1;

/// Compact 32-byte cache-line-aligned hot metadata row (2 rows per 64B cache line, zero padding).
#[repr(C)]
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct MetaRow {
    pub size_bytes: u64,
    pub entity_id: u32,
    pub published_epoch: u32,
    pub created_epoch: u32,
    pub files_count: u32,
    pub year: u16,
    /// bits 0..4: source, 4..8: codec, 8: is_anime, 9: eng_audio, 10: has_exec, 11: hdr, 12: has_tree
    pub flags_src: u16,
    pub single_ext_id: u16,
    pub content_type: u8,
    pub resolution: u8,
}

unsafe impl Pod for MetaRow {}

impl MetaRow {
    #[inline]
    pub fn pack_flags(
        source: u8,
        codec: u8,
        is_anime: bool,
        eng_audio: bool,
        has_exec: bool,
        has_hdr: bool,
        has_tree: bool,
    ) -> u16 {
        ((source as u16) & 0x0F)
            | (((codec as u16) & 0x0F) << 4)
            | (if is_anime { 1 << 8 } else { 0 })
            | (if eng_audio { 1 << 9 } else { 0 })
            | (if has_exec { 1 << 10 } else { 0 })
            | (if has_hdr { 1 << 11 } else { 0 })
            | (if has_tree { 1 << 12 } else { 0 })
    }

    #[inline]
    pub fn source(&self) -> u8 {
        (self.flags_src & 0x0F) as u8
    }

    #[inline]
    pub fn codec(&self) -> u8 {
        ((self.flags_src >> 4) & 0x0F) as u8
    }

    #[inline]
    pub fn is_anime(&self) -> bool {
        (self.flags_src & (1 << 8)) != 0
    }

    #[inline]
    pub fn eng_audio(&self) -> bool {
        (self.flags_src & (1 << 9)) != 0
    }

    #[inline]
    pub fn has_exec(&self) -> bool {
        (self.flags_src & (1 << 10)) != 0
    }

    #[inline]
    pub fn has_hdr(&self) -> bool {
        (self.flags_src & (1 << 11)) != 0
    }

    #[inline]
    pub fn has_tree(&self) -> bool {
        (self.flags_src & (1 << 12)) != 0
    }

    #[inline]
    pub fn facet_byte(&self) -> u8 {
        (self.content_type & 0x0F) | ((self.resolution & 0x0F) << 4)
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct FileEntry {
    pub path: String,
    pub size: u64,
    pub ext: String,
}

#[derive(Debug, Clone)]
pub struct HydratedHeader {
    pub doc_id: u32,
    pub name: String,
    pub meta: MetaRow,
}

#[derive(Debug, Clone)]
pub struct HydratedTorrent {
    pub doc_id: u32,
    pub name: String,
    pub files: Vec<FileEntry>,
}

fn longest_dir_prefix(files: &[(String, u64, String)]) -> &str {
    if files.len() < 2 {
        return "";
    }
    let first = files[0].0.as_str();
    let Some(mut end) = first.rfind('/') else {
        return "";
    };
    end += 1;
    let mut prefix = &first[..end];
    for (p, _, _) in &files[1..] {
        while !p.starts_with(prefix) {
            if prefix.len() <= 1 {
                return "";
            }
            let cut = &prefix[..prefix.len() - 1];
            match cut.rfind('/') {
                Some(idx) => prefix = &prefix[..idx + 1],
                None => return "",
            }
        }
    }
    prefix
}

/// Encode a single torrent's file tree (prefix-stripped, varint sizes, u16 extension IDs).
pub fn encode_single_tree(
    files: &[(String, u64, String)],
    ext_map: &AHashMap<String, u16>,
) -> Vec<u8> {
    let mut out = Vec::with_capacity(files.len() * 28 + 16);
    write_varint_u32(&mut out, files.len() as u32);
    if !files.is_empty() {
        let prefix = longest_dir_prefix(files);
        let pb = prefix.as_bytes();
        write_varint_u32(&mut out, pb.len() as u32);
        out.extend_from_slice(pb);
        for (path, sz, ext) in files {
            let rel = if path.len() >= pb.len() {
                &path.as_bytes()[pb.len()..]
            } else {
                path.as_bytes()
            };
            write_varint_u32(&mut out, rel.len() as u32);
            out.extend_from_slice(rel);
            write_varint_u64(&mut out, *sz);
            let ext_id = *ext_map.get(ext).unwrap_or(&0);
            write_varint_u32(&mut out, ext_id as u32);
        }
    }
    out
}

/// Pack 1..=16 pre-encoded torrent tree records into one block with an O(1) per-record offset table:
/// `[count: u16][offsets: (u32, u32) * count][record_bytes...]`
fn pack_tree_block(records: &[Vec<u8>]) -> Vec<u8> {
    let count = records.len();
    let header_len = 2 + count * 8;
    let data_len: usize = records.iter().map(Vec::len).sum();
    let mut out = Vec::with_capacity(header_len + data_len);
    out.extend_from_slice(&(count as u16).to_le_bytes());
    let mut off = header_len as u32;
    for rec in records {
        let len = rec.len() as u32;
        out.extend_from_slice(&off.to_le_bytes());
        out.extend_from_slice(&len.to_le_bytes());
        off = off.checked_add(len).expect("tree block offset overflow");
    }
    for rec in records {
        out.extend_from_slice(rec);
    }
    out
}

fn decode_single_tree_checked(
    buf: &[u8],
    ext_vocab: &[String],
) -> Result<Vec<FileEntry>, FormatError> {
    let mut pos = 0usize;
    let fcount = read_varint_u32(buf, &mut pos)? as usize;
    if fcount == 0 {
        return Ok(Vec::new());
    }
    let plen = read_varint_u32(buf, &mut pos)? as usize;
    let pend = pos
        .checked_add(plen)
        .ok_or_else(|| FormatError::CorruptPayload("tree prefix len overflow".into()))?;
    if pend > buf.len() {
        return Err(FormatError::CorruptPayload("tree prefix truncated".into()));
    }
    let prefix = std::str::from_utf8(&buf[pos..pend])
        .map_err(|_| FormatError::CorruptPayload("tree prefix invalid UTF-8".into()))?;
    pos = pend;

    let mut files = Vec::with_capacity(fcount.min(4096));
    for _ in 0..fcount {
        let rlen = read_varint_u32(buf, &mut pos)? as usize;
        let rend = pos
            .checked_add(rlen)
            .ok_or_else(|| FormatError::CorruptPayload("tree relpath len overflow".into()))?;
        if rend > buf.len() {
            return Err(FormatError::CorruptPayload("tree relpath truncated".into()));
        }
        let rel = std::str::from_utf8(&buf[pos..rend])
            .map_err(|_| FormatError::CorruptPayload("tree relpath invalid UTF-8".into()))?;
        pos = rend;
        let sz = read_varint_u64(buf, &mut pos)?;
        let ext_id = read_varint_u32(buf, &mut pos)? as usize;
        let ext = ext_vocab.get(ext_id).cloned().unwrap_or_default();
        let mut full = String::with_capacity(prefix.len() + rel.len());
        full.push_str(prefix);
        full.push_str(rel);
        files.push(FileEntry {
            path: full,
            size: sz,
            ext,
        });
    }
    Ok(files)
}

fn decode_record_from_block_checked(
    raw: &[u8],
    in_blk: usize,
    ext_vocab: &[String],
) -> Result<Vec<FileEntry>, FormatError> {
    if raw.len() < 2 {
        return Err(FormatError::CorruptPayload("tree block too short".into()));
    }
    let count = u16::from_le_bytes([raw[0], raw[1]]) as usize;
    if in_blk >= count {
        return Err(FormatError::CorruptPayload(format!(
            "tree record index {in_blk} >= block count {count}"
        )));
    }
    let entry_off = 2 + in_blk * 8;
    let entry_end = entry_off + 8;
    if entry_end > raw.len() {
        return Err(FormatError::CorruptPayload(
            "tree block directory truncated".into(),
        ));
    }
    let rec_off = u32::from_le_bytes(raw[entry_off..entry_off + 4].try_into().unwrap()) as usize;
    let rec_len =
        u32::from_le_bytes(raw[entry_off + 4..entry_off + 8].try_into().unwrap()) as usize;
    let rec_end = rec_off
        .checked_add(rec_len)
        .ok_or_else(|| FormatError::CorruptPayload("tree record slice overflow".into()))?;
    if rec_end > raw.len() {
        return Err(FormatError::CorruptPayload(
            "tree record slice out of bounds".into(),
        ));
    }
    decode_single_tree_checked(&raw[rec_off..rec_end], ext_vocab)
}

/// Train a 112 KiB Zstd dictionary from a corpus-wide sample of encoded file-tree blocks.
pub fn train_tree_dictionary(sample_blocks: &[Vec<u8>]) -> Vec<u8> {
    let refs: Vec<&[u8]> = sample_blocks.iter().map(Vec::as_slice).collect();
    let total: usize = refs.iter().map(|s| s.len()).sum();
    if refs.len() >= 16 && total >= 32 * 1024 {
        zstd::dict::from_samples(&refs, 112 * 1024).unwrap_or_default()
    } else {
        Vec::new()
    }
}

pub struct CorePlaneBuildStats {
    pub meta_bytes: u64,
    pub facet_bytes: u64,
    pub names_bytes: u64,
    pub tree_blocks_bytes: u64,
    pub tree_offsets_bytes: u64,
    pub tree_locs_bytes: u64,
    pub tree_raw_bytes: u64,
    pub dict_bytes: u64,
    pub num_tree_blocks: u64,
    pub num_overflow_blocks: u64,
}

/// Streaming O(1)-RAM writer for the split Name column + byte-capped 16-record Zstd-dictionary
/// File-Tree store (with explicit overflow blocks for mega-torrents > 256 KiB and per-block CRC32).
pub struct StreamingCoreWriter {
    dir: PathBuf,
    generation_id: u64,
    names_data_w: CheckedFileWriter,
    names_offsets: Vec<u64>,
    names_cur_off: u64,

    tree_blocks_w: CheckedFileWriter,
    tree_offsets: Vec<u64>,
    tree_doc_locs: Vec<u32>,
    tree_cur_off: u64,
    cur_block_recs: Vec<Vec<u8>>,
    cur_block_bytes: usize,
    pending_blocks: Vec<(Vec<u8>, bool)>, // (raw_bytes, is_overflow)
    next_block_idx: u32,
    num_overflow_blocks: u64,
    total_tree_raw_bytes: u64,

    zstd_dict: Vec<u8>,
    edict: Option<zstd::dict::EncoderDictionary<'static>>,
    dict_ready: bool,
}

impl StreamingCoreWriter {
    /// Pass an optional pre-trained corpus-wide Zstd dictionary (recommended for corpus-wide coverage).
    /// If `pretrained_dict` is empty, automatically trains from the first 512 tree blocks.
    pub fn new(
        dir: &Path,
        generation_id: u64,
        estimated_docs: usize,
        pretrained_dict: Vec<u8>,
    ) -> Result<Self, FormatError> {
        let dict_ready = !pretrained_dict.is_empty();
        let edict = if dict_ready {
            Some(zstd::dict::EncoderDictionary::copy(&pretrained_dict, 5))
        } else {
            None
        };
        Ok(Self {
            dir: dir.to_path_buf(),
            generation_id,
            names_data_w: CheckedFileWriter::create(
                &dir.join("names_data.bin"),
                FileKind::NameData,
                generation_id,
            )?,
            names_offsets: Vec::with_capacity(estimated_docs + 1),
            names_cur_off: 0,
            tree_blocks_w: CheckedFileWriter::create(
                &dir.join("tree_blocks.bin"),
                FileKind::TreeBlocks,
                generation_id,
            )?,
            tree_offsets: Vec::with_capacity(estimated_docs / 16 + 64),
            tree_doc_locs: Vec::with_capacity(estimated_docs),
            tree_cur_off: 0,
            cur_block_recs: Vec::with_capacity(MAX_RECORDS_PER_TREE_BLOCK),
            cur_block_bytes: 0,
            pending_blocks: Vec::with_capacity(512),
            next_block_idx: 0,
            num_overflow_blocks: 0,
            total_tree_raw_bytes: 0,
            zstd_dict: pretrained_dict,
            edict,
            dict_ready,
        })
    }

    /// Push one torrent (in exact `doc_id` order 0, 1, 2, ...).
    pub fn push_doc(
        &mut self,
        name: &str,
        files: &[(String, u64, String)],
        ext_map: &AHashMap<String, u16>,
    ) -> Result<(), FormatError> {
        // 1. Write name directly to separate seekable Name column (ZERO tree decompression on search/RSS!)
        self.names_offsets.push(self.names_cur_off);
        let nb = name.as_bytes();
        self.names_data_w.write_payload(nb)?;
        self.names_cur_off += nb.len() as u64;

        // 2. Single-file torrents take ZERO tree bytes
        if files.is_empty() {
            self.tree_doc_locs.push(NO_TREE_LOC);
            return Ok(());
        }

        let rec = encode_single_tree(files, ext_map);
        // Oversized mega-torrent (> 256 KiB encoded) gets a dedicated single-record overflow block!
        if rec.len() > MAX_RAW_TREE_BLOCK_BYTES {
            self.flush_cur_block()?;
            let blk_idx = self.next_block_idx;
            self.next_block_idx += 1;
            self.num_overflow_blocks += 1;
            let raw = pack_tree_block(&[rec]);
            self.total_tree_raw_bytes += raw.len() as u64;
            self.tree_doc_locs.push(blk_idx << 4);
            self.pending_blocks.push((raw, true));
            self.maybe_drain()?;
            return Ok(());
        }

        // Byte-cap AND count-cap normal blocks (<= 16 records, <= 256 KiB raw)
        if !self.cur_block_recs.is_empty()
            && (self.cur_block_recs.len() >= MAX_RECORDS_PER_TREE_BLOCK
                || self.cur_block_bytes + rec.len() > MAX_RAW_TREE_BLOCK_BYTES)
        {
            self.flush_cur_block()?;
        }

        let in_blk = self.cur_block_recs.len() as u32;
        let blk_idx = self.next_block_idx;
        self.tree_doc_locs.push((blk_idx << 4) | (in_blk & 0x0F));
        self.cur_block_bytes += rec.len();
        self.cur_block_recs.push(rec);

        if self.cur_block_recs.len() >= MAX_RECORDS_PER_TREE_BLOCK {
            self.flush_cur_block()?;
        }
        Ok(())
    }

    fn flush_cur_block(&mut self) -> Result<(), FormatError> {
        if self.cur_block_recs.is_empty() {
            return Ok(());
        }
        let raw = pack_tree_block(&self.cur_block_recs);
        self.cur_block_recs.clear();
        self.cur_block_bytes = 0;
        self.total_tree_raw_bytes += raw.len() as u64;
        self.next_block_idx += 1;
        self.pending_blocks.push((raw, false));
        self.maybe_drain()
    }

    fn maybe_drain(&mut self) -> Result<(), FormatError> {
        if !self.dict_ready {
            if self.pending_blocks.len() >= 1024 {
                let samples: Vec<Vec<u8>> = self
                    .pending_blocks
                    .iter()
                    .filter(|(_, is_ov)| !is_ov)
                    .map(|(b, _)| b.clone())
                    .collect();
                self.zstd_dict = train_tree_dictionary(&samples);
                if !self.zstd_dict.is_empty() {
                    self.edict = Some(zstd::dict::EncoderDictionary::copy(&self.zstd_dict, 5));
                }
                self.dict_ready = true;
                self.drain_parallel()?;
            }
        } else if self.pending_blocks.len() >= 512 {
            self.drain_parallel()?;
        }
        Ok(())
    }

    fn drain_parallel(&mut self) -> Result<(), FormatError> {
        if self.pending_blocks.is_empty() {
            return Ok(());
        }
        let batch = std::mem::take(&mut self.pending_blocks);
        let edict_ref = &self.edict;

        // Each Rayon worker creates/reuses one compressor per parallel chunk via map_init!
        type CompressedBlock = Result<(Vec<u8>, u32, u32), FormatError>;
        let compressed: Vec<CompressedBlock> = batch
            .par_iter()
            .map_init(
                || -> Option<zstd::bulk::Compressor<'_>> {
                    edict_ref
                        .as_ref()
                        .and_then(|ed| zstd::bulk::Compressor::with_prepared_dictionary(ed).ok())
                },
                |comp_opt, (raw, is_overflow)| {
                    let uncomp_len = u32::try_from(raw.len()).map_err(|_| {
                        FormatError::CorruptPayload(
                            "tree block exceeds u32 uncompressed len".into(),
                        )
                    })?;
                    let (comp, uses_dict) = if !*is_overflow {
                        if let Some(ref mut enc) = comp_opt {
                            (enc.compress(raw).map_err(FormatError::Io)?, true)
                        } else {
                            (
                                zstd::bulk::compress(raw, 5).map_err(FormatError::Io)?,
                                false,
                            )
                        }
                    } else {
                        // Overflow mega-torrents compress directly without 112KB dictionary overhead
                        (
                            zstd::bulk::compress(raw, 5).map_err(FormatError::Io)?,
                            false,
                        )
                    };
                    let mut flags = 0u32;
                    if uses_dict {
                        flags |= BLK_FLAG_USES_DICT;
                    }
                    if *is_overflow {
                        flags |= BLK_FLAG_OVERFLOW_SINGLE;
                    }
                    Ok((comp, uncomp_len, flags))
                },
            )
            .collect();

        for item in compressed {
            let (comp, uncomp_len, flags) = item?;
            let mut hasher = Crc32::new();
            hasher.update(&comp);
            let crc = hasher.finalize();

            self.tree_offsets.push(self.tree_cur_off);
            let mut hdr = [0u8; 12];
            hdr[0..4].copy_from_slice(&crc.to_le_bytes());
            hdr[4..8].copy_from_slice(&uncomp_len.to_le_bytes());
            hdr[8..12].copy_from_slice(&flags.to_le_bytes());
            self.tree_blocks_w.write_payload(&hdr)?;
            self.tree_blocks_w.write_payload(&comp)?;
            self.tree_cur_off += 12 + comp.len() as u64;
        }
        Ok(())
    }

    pub fn finish(
        mut self,
        metas: &[MetaRow],
        ext_vocab: &[String],
    ) -> Result<CorePlaneBuildStats, FormatError> {
        self.flush_cur_block()?;
        if !self.dict_ready {
            let samples: Vec<Vec<u8>> = self
                .pending_blocks
                .iter()
                .filter(|(_, is_ov)| !is_ov)
                .map(|(b, _)| b.clone())
                .collect();
            self.zstd_dict = train_tree_dictionary(&samples);
            if !self.zstd_dict.is_empty() {
                self.edict = Some(zstd::dict::EncoderDictionary::copy(&self.zstd_dict, 5));
            }
            self.dict_ready = true;
        }
        self.drain_parallel()?;

        let num_docs = metas.len();
        if self.tree_doc_locs.len() != num_docs || self.names_offsets.len() != num_docs {
            return Err(FormatError::CorruptPayload(format!(
                "CoreWriter count mismatch: metas={num_docs}, names={}, locs={}",
                self.names_offsets.len(),
                self.tree_doc_locs.len()
            )));
        }

        // Finalize names column
        self.names_offsets.push(self.names_cur_off);
        let names_data_bytes = self.names_data_w.finish(num_docs as u64, 0, 0)?;
        let names_off_bytes = write_checked_pod_slice(
            &self.dir.join("names_offsets.bin"),
            FileKind::NameOffsets,
            self.generation_id,
            num_docs as u64,
            0,
            &self.names_offsets,
        )?;

        // Finalize tree blocks + offsets + doc_locs + dict + ext_vocab
        let num_tree_blocks = self.tree_offsets.len() as u64;
        self.tree_offsets.push(self.tree_cur_off);
        let tree_blocks_bytes = self.tree_blocks_w.finish(num_tree_blocks, 0, 0)?;
        let tree_offsets_bytes = write_checked_pod_slice(
            &self.dir.join("tree_offsets.bin"),
            FileKind::TreeOffsets,
            self.generation_id,
            num_tree_blocks,
            0,
            &self.tree_offsets,
        )?;
        let tree_locs_bytes = write_checked_pod_slice(
            &self.dir.join("tree_doc_locs.bin"),
            FileKind::TreeOffsets,
            self.generation_id,
            num_docs as u64,
            1, // flag=1 distinguishes doc_locs
            &self.tree_doc_locs,
        )?;
        let dict_bytes = write_checked_bytes(
            &self.dir.join("trees.zdict"),
            FileKind::TreeDict,
            self.generation_id,
            self.zstd_dict.len() as u64,
            0,
            0,
            &self.zstd_dict,
        )?;

        let mut ext_buf = Vec::new();
        write_varint_u32(&mut ext_buf, ext_vocab.len() as u32);
        for s in ext_vocab {
            write_varint_u32(&mut ext_buf, s.len() as u32);
            ext_buf.extend_from_slice(s.as_bytes());
        }
        let _ext_bytes = write_checked_bytes(
            &self.dir.join("ext_vocab.bin"),
            FileKind::ExtVocab,
            self.generation_id,
            ext_vocab.len() as u64,
            0,
            0,
            &ext_buf,
        )?;

        // Write 32B MetaRow column + 1B packed FacetByte column
        let meta_bytes = write_checked_pod_slice(
            &self.dir.join("meta_col.bin"),
            FileKind::MetaCol,
            self.generation_id,
            0,
            0,
            metas,
        )?;
        let facet_bytes_vec: Vec<u8> = metas.iter().map(MetaRow::facet_byte).collect();
        let facet_bytes = write_checked_bytes(
            &self.dir.join("facet_col.bin"),
            FileKind::FacetCol,
            self.generation_id,
            num_docs as u64,
            0,
            0,
            &facet_bytes_vec,
        )?;

        Ok(CorePlaneBuildStats {
            meta_bytes,
            facet_bytes,
            names_bytes: names_data_bytes + names_off_bytes,
            tree_blocks_bytes,
            tree_offsets_bytes,
            tree_locs_bytes,
            tree_raw_bytes: self.total_tree_raw_bytes,
            dict_bytes,
            num_tree_blocks,
            num_overflow_blocks: self.num_overflow_blocks,
        })
    }
}

thread_local! {
    static TL_PLAIN_DEC: RefCell<Option<zstd::bulk::Decompressor<'static>>> = const { RefCell::new(None) };
    static TL_DICT_DEC: RefCell<Option<(u64, zstd::bulk::Decompressor<'static>)>> = const { RefCell::new(None) };
}

/// Checked zero-heap mmap CorePlane with split seekable Name column, 1B Facet column,
/// and byte-capped 16-record Zstd-dictionary File-Tree store with thread-local reused decompressors.
pub struct CorePlane {
    pub generation_id: u64,
    pub num_docs: usize,
    meta_cm: CheckedMmap,
    facet_cm: CheckedMmap,
    names_data_cm: CheckedMmap,
    names_offsets_cm: CheckedMmap,
    tree_blocks_cm: CheckedMmap,
    tree_offsets_cm: CheckedMmap,
    tree_locs_cm: CheckedMmap,
    dict_bytes: Option<Vec<u8>>,
    dict_id: u64,
    ext_vocab: Vec<String>,
}

impl CorePlane {
    pub fn open(dir: &Path, expected_gen: Option<u64>) -> Result<Self, FormatError> {
        let meta_cm = CheckedMmap::open(
            &dir.join("meta_col.bin"),
            FileKind::MetaCol,
            expected_gen,
            false,
        )?;
        let generation_id = meta_cm.header().generation_id;
        let num_docs = meta_cm.typed_slice::<MetaRow>()?.len();

        let facet_cm = CheckedMmap::open(
            &dir.join("facet_col.bin"),
            FileKind::FacetCol,
            Some(generation_id),
            false,
        )?;
        if facet_cm.typed_slice::<u8>()?.len() != num_docs {
            return Err(FormatError::CorruptPayload(
                "facet_col length != num_docs".into(),
            ));
        }

        let names_data_cm = CheckedMmap::open(
            &dir.join("names_data.bin"),
            FileKind::NameData,
            Some(generation_id),
            false,
        )?;
        let names_offsets_cm = CheckedMmap::open(
            &dir.join("names_offsets.bin"),
            FileKind::NameOffsets,
            Some(generation_id),
            false,
        )?;
        if names_offsets_cm.typed_slice::<u64>()?.len() != num_docs + 1 {
            return Err(FormatError::CorruptPayload(
                "names_offsets length != num_docs + 1".into(),
            ));
        }

        let tree_blocks_cm = CheckedMmap::open(
            &dir.join("tree_blocks.bin"),
            FileKind::TreeBlocks,
            Some(generation_id),
            false,
        )?;
        let tree_offsets_cm = CheckedMmap::open(
            &dir.join("tree_offsets.bin"),
            FileKind::TreeOffsets,
            Some(generation_id),
            false,
        )?;
        let _ = tree_offsets_cm.typed_slice::<u64>()?;
        let tree_locs_cm = CheckedMmap::open(
            &dir.join("tree_doc_locs.bin"),
            FileKind::TreeOffsets,
            Some(generation_id),
            false,
        )?;
        if tree_locs_cm.typed_slice::<u32>()?.len() != num_docs {
            return Err(FormatError::CorruptPayload(
                "tree_doc_locs length != num_docs".into(),
            ));
        }

        let dict_cm = CheckedMmap::open(
            &dir.join("trees.zdict"),
            FileKind::TreeDict,
            Some(generation_id),
            true,
        )?;
        let dict_bytes = if !dict_cm.payload().is_empty() {
            Some(dict_cm.payload().to_vec())
        } else {
            None
        };
        let dict_id = u64::from_le_bytes(dict_cm.header().payload_blake3[0..8].try_into().unwrap());

        let ext_cm = CheckedMmap::open(
            &dir.join("ext_vocab.bin"),
            FileKind::ExtVocab,
            Some(generation_id),
            true,
        )?;
        let ep = ext_cm.payload();
        let mut pos = 0usize;
        let n_ext = read_varint_u32(ep, &mut pos)? as usize;
        let mut ext_vocab = Vec::with_capacity(n_ext.min(65536));
        for _ in 0..n_ext {
            let slen = read_varint_u32(ep, &mut pos)? as usize;
            let send = pos
                .checked_add(slen)
                .ok_or_else(|| FormatError::CorruptPayload("ext_vocab overflow".into()))?;
            if send > ep.len() {
                return Err(FormatError::CorruptPayload("ext_vocab truncated".into()));
            }
            let s = String::from_utf8(ep[pos..send].to_vec())
                .map_err(|_| FormatError::CorruptPayload("ext_vocab invalid UTF-8".into()))?;
            pos = send;
            ext_vocab.push(s);
        }

        Ok(Self {
            generation_id,
            num_docs,
            meta_cm,
            facet_cm,
            names_data_cm,
            names_offsets_cm,
            tree_blocks_cm,
            tree_offsets_cm,
            tree_locs_cm,
            dict_bytes,
            dict_id,
            ext_vocab,
        })
    }

    #[inline]
    pub fn metas(&self) -> &[MetaRow] {
        self.meta_cm.typed_slice::<MetaRow>().unwrap()
    }

    #[inline]
    pub fn facet_bytes(&self) -> &[u8] {
        self.facet_cm.typed_slice::<u8>().unwrap()
    }

    /// O(1) seekable name read — ZERO Zstd decompression!
    #[inline]
    pub fn get_name(&self, doc_id: u32) -> Result<&str, FormatError> {
        let idx = doc_id as usize;
        if idx >= self.num_docs {
            return Err(FormatError::CorruptPayload(format!(
                "doc_id {doc_id} out of range (num_docs={})",
                self.num_docs
            )));
        }
        let offs = self.names_offsets_cm.typed_slice::<u64>()?;
        let start = offs[idx] as usize;
        let end = offs[idx + 1] as usize;
        let data = self.names_data_cm.payload();
        if start > end || end > data.len() {
            return Err(FormatError::CorruptPayload(
                "name offset out of bounds".into(),
            ));
        }
        std::str::from_utf8(&data[start..end])
            .map_err(|_| FormatError::CorruptPayload("invalid UTF-8 in name column".into()))
    }

    /// Fast batch title/header hydration for search & RSS endpoints (ZERO tree decompression!).
    pub fn hydrate_headers_batch(
        &self,
        doc_ids: &[u32],
    ) -> Result<Vec<HydratedHeader>, FormatError> {
        let metas = self.metas();
        let mut out = Vec::with_capacity(doc_ids.len());
        for &doc_id in doc_ids {
            let name = self.get_name(doc_id)?.to_owned();
            out.push(HydratedHeader {
                doc_id,
                name,
                meta: metas[doc_id as usize],
            });
        }
        Ok(out)
    }

    fn decompress_tree_block(&self, blk_idx: usize) -> Result<Vec<u8>, FormatError> {
        let offs = self.tree_offsets_cm.typed_slice::<u64>()?;
        if blk_idx + 1 >= offs.len() {
            return Err(FormatError::CorruptPayload(format!(
                "tree block index {blk_idx} out of bounds"
            )));
        }
        let start = offs[blk_idx] as usize;
        let end = offs[blk_idx + 1] as usize;
        let data = self.tree_blocks_cm.payload();
        if start + 12 > end || end > data.len() {
            return Err(FormatError::CorruptPayload(
                "tree block frame truncated".into(),
            ));
        }
        let stored_crc = u32::from_le_bytes(data[start..start + 4].try_into().unwrap());
        let uncomp_len =
            u32::from_le_bytes(data[start + 4..start + 8].try_into().unwrap()) as usize;
        let flags = u32::from_le_bytes(data[start + 8..start + 12].try_into().unwrap());
        if uncomp_len > MAX_SAFE_DECOMPRESS_BYTES {
            return Err(FormatError::CorruptPayload(format!(
                "tree block uncompressed length {uncomp_len} exceeds safety cap"
            )));
        }
        let comp = &data[start + 12..end];
        let mut hasher = Crc32::new();
        hasher.update(comp);
        if hasher.finalize() != stored_crc {
            return Err(FormatError::CorruptPayload(format!(
                "tree block {blk_idx} CRC32 mismatch"
            )));
        }

        let uses_dict = (flags & BLK_FLAG_USES_DICT) != 0;
        if uses_dict {
            let Some(ref db) = self.dict_bytes else {
                return Err(FormatError::CorruptPayload(
                    "block requires dictionary but none loaded".into(),
                ));
            };
            TL_DICT_DEC.with(|cell| {
                let mut borrow = cell.borrow_mut();
                let need_init = match *borrow {
                    Some((id, _)) => id != self.dict_id,
                    None => true,
                };
                if need_init {
                    let dec =
                        zstd::bulk::Decompressor::with_dictionary(db).map_err(FormatError::Io)?;
                    *borrow = Some((self.dict_id, dec));
                }
                let (_, ref mut dec) = borrow.as_mut().unwrap();
                dec.decompress(comp, uncomp_len).map_err(FormatError::Io)
            })
        } else {
            TL_PLAIN_DEC.with(|cell| {
                let mut borrow = cell.borrow_mut();
                if borrow.is_none() {
                    *borrow = Some(zstd::bulk::Decompressor::new().map_err(FormatError::Io)?);
                }
                borrow
                    .as_mut()
                    .unwrap()
                    .decompress(comp, uncomp_len)
                    .map_err(FormatError::Io)
            })
        }
    }

    /// Random-access file-tree hydration for a single torrent.
    pub fn hydrate_full(&self, doc_id: u32) -> Result<HydratedTorrent, FormatError> {
        let name = self.get_name(doc_id)?.to_owned();
        let locs = self.tree_locs_cm.typed_slice::<u32>()?;
        let loc = locs[doc_id as usize];
        if loc == NO_TREE_LOC {
            return Ok(HydratedTorrent {
                doc_id,
                name,
                files: Vec::new(),
            });
        }
        let blk_idx = (loc >> 4) as usize;
        let in_blk = (loc & 0x0F) as usize;
        let raw = self.decompress_tree_block(blk_idx)?;
        let files = decode_record_from_block_checked(&raw, in_blk, &self.ext_vocab)?;
        Ok(HydratedTorrent {
            doc_id,
            name,
            files,
        })
    }

    /// Batch full hydration (caches decompressed blocks across the batch, propagates all errors!).
    pub fn hydrate_full_batch(&self, doc_ids: &[u32]) -> Result<Vec<HydratedTorrent>, FormatError> {
        if doc_ids.is_empty() {
            return Ok(Vec::new());
        }
        let locs = self.tree_locs_cm.typed_slice::<u32>()?;
        let mut blk_cache: AHashMap<usize, Vec<u8>> = AHashMap::with_capacity(doc_ids.len());
        let mut out = Vec::with_capacity(doc_ids.len());
        for &doc_id in doc_ids {
            let name = self.get_name(doc_id)?.to_owned();
            let loc = locs[doc_id as usize];
            if loc == NO_TREE_LOC {
                out.push(HydratedTorrent {
                    doc_id,
                    name,
                    files: Vec::new(),
                });
                continue;
            }
            let blk_idx = (loc >> 4) as usize;
            let in_blk = (loc & 0x0F) as usize;
            let raw = match blk_cache.entry(blk_idx) {
                std::collections::hash_map::Entry::Occupied(e) => e.into_mut(),
                std::collections::hash_map::Entry::Vacant(e) => {
                    let v = self.decompress_tree_block(blk_idx)?;
                    e.insert(v)
                }
            };
            let files = decode_record_from_block_checked(raw, in_blk, &self.ext_vocab)?;
            out.push(HydratedTorrent {
                doc_id,
                name,
                files,
            });
        }
        Ok(out)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_core_split_names_trees_and_mega_torrent_overflow() {
        let dir = std::env::temp_dir().join(format!("dhtms_core_test_{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();

        let mut ext_map = AHashMap::new();
        let ext_vocab = vec!["".to_string(), "mkv".to_string(), "txt".to_string()];
        ext_map.insert("".to_string(), 0);
        ext_map.insert("mkv".to_string(), 1);
        ext_map.insert("txt".to_string(), 2);

        let mut w = StreamingCoreWriter::new(&dir, 55, 4, Vec::new()).unwrap();
        let mut metas = Vec::new();

        // Doc 0: single-file (no tree block)
        w.push_doc("Single.Movie.2024.1080p.mkv", &[], &ext_map)
            .unwrap();
        metas.push(MetaRow {
            size_bytes: 2_000_000_000,
            files_count: 1,
            content_type: 1,
            resolution: 6,
            ..Default::default()
        });

        // Doc 1: small multi-file
        let small_files = vec![
            (
                "Show.S01/ep01.mkv".to_string(),
                1_000_000_000,
                "mkv".to_string(),
            ),
            ("Show.S01/readme.txt".to_string(), 512, "txt".to_string()),
        ];
        w.push_doc("Show.S01.1080p", &small_files, &ext_map)
            .unwrap();
        metas.push(MetaRow {
            size_bytes: 1_000_000_512,
            files_count: 2,
            content_type: 2,
            resolution: 6,
            ..Default::default()
        });

        // Doc 2: 5 MiB mega-torrent (> 4 MiB v0.2 cap!) with 80,000 files -> MUST succeed via overflow block!
        let mega_files: Vec<(String, u64, String)> = (0..80_000)
            .map(|i| {
                (
                    format!(
                        "MegaPack/subdir_{:03}/track_{:06}_extra_padding_filename.txt",
                        i / 1000,
                        i
                    ),
                    4096 + i as u64,
                    "txt".to_string(),
                )
            })
            .collect();
        w.push_doc("MegaPack.Archive", &mega_files, &ext_map)
            .unwrap();
        metas.push(MetaRow {
            size_bytes: 500_000_000,
            files_count: 80_000,
            content_type: 8,
            resolution: 0,
            ..Default::default()
        });

        let stats = w.finish(&metas, &ext_vocab).unwrap();
        assert_eq!(stats.num_overflow_blocks, 1);

        let core = CorePlane::open(&dir, Some(55)).unwrap();
        assert_eq!(core.get_name(0).unwrap(), "Single.Movie.2024.1080p.mkv");
        assert_eq!(core.get_name(2).unwrap(), "MegaPack.Archive");

        let h0 = core.hydrate_full(0).unwrap();
        assert!(h0.files.is_empty());

        let h1 = core.hydrate_full(1).unwrap();
        assert_eq!(h1.files.len(), 2);
        assert_eq!(h1.files[0].path, "Show.S01/ep01.mkv");

        let h2 = core.hydrate_full(2).unwrap();
        assert_eq!(h2.files.len(), 80_000);
        assert_eq!(h2.files[79_999].size, 4096 + 79_999);

        let _ = std::fs::remove_dir_all(&dir);
    }
}
