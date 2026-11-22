pub mod blob_map;
pub mod catalog;

use crc32fast::Hasher as Crc32;
use memmap2::Mmap;
use serde::{Deserialize, Serialize};
use std::fmt;
use std::fs::{self, File, OpenOptions};
use std::io::{self, BufWriter, Seek, SeekFrom, Write};
use std::path::{Path, PathBuf};

pub const MAGIC: [u8; 8] = *b"DHTMS01\0";
pub const FORMAT_VERSION: u16 = 1;
pub const ENDIAN_MARKER: u32 = 0x0102_0304;
pub const HEADER_LEN: usize = 96; // 32-byte aligned for all repr(C) columnar structs
pub const EAGER_BLAKE3_MAX_BYTES: u64 = 32 * 1024 * 1024;

#[repr(u16)]
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum FileKind {
    MetaCol = 1,
    FacetCol = 2,
    PayloadExpCol = 3,
    NameData = 4,
    NameOffsets = 5,
    TreeBlocks = 6,
    TreeOffsets = 7,
    TreeDict = 8,
    ExtVocab = 9,
    HashRevV1 = 10,
    HashDirV1 = 11,
    HashV2Dir = 12,
    HashV2Rev = 13,
    BlobMap = 14,
    Catalog = 15,
    Inverted = 16,
    FacetsStatic = 17,
    TrigramIndex = 18,
    PolicySnapshot = 19,
    SwarmCheckpoint = 20,
    DenseTermPostings = 21,
}

impl FileKind {
    pub fn from_u16(v: u16) -> Option<Self> {
        match v {
            1 => Some(Self::MetaCol),
            2 => Some(Self::FacetCol),
            3 => Some(Self::PayloadExpCol),
            4 => Some(Self::NameData),
            5 => Some(Self::NameOffsets),
            6 => Some(Self::TreeBlocks),
            7 => Some(Self::TreeOffsets),
            8 => Some(Self::TreeDict),
            9 => Some(Self::ExtVocab),
            10 => Some(Self::HashRevV1),
            11 => Some(Self::HashDirV1),
            12 => Some(Self::HashV2Dir),
            13 => Some(Self::HashV2Rev),
            14 => Some(Self::BlobMap),
            15 => Some(Self::Catalog),
            16 => Some(Self::Inverted),
            17 => Some(Self::FacetsStatic),
            18 => Some(Self::TrigramIndex),
            19 => Some(Self::PolicySnapshot),
            20 => Some(Self::SwarmCheckpoint),
            21 => Some(Self::DenseTermPostings),
            _ => None,
        }
    }
}

#[derive(Debug)]
pub enum FormatError {
    Io(io::Error),
    Truncated {
        expected_min: usize,
        actual: usize,
    },
    BadMagic,
    UnsupportedVersion(u16),
    EndianMismatch(u32),
    KindMismatch {
        expected: FileKind,
        actual: u16,
    },
    GenerationMismatch {
        expected: u64,
        actual: u64,
    },
    HeaderCrcMismatch {
        expected: u32,
        actual: u32,
    },
    Blake3Mismatch,
    LengthMismatch {
        expected: u64,
        actual: u64,
    },
    AlignmentViolation {
        align: usize,
        addr: usize,
    },
    RecordCountMismatch {
        records: u64,
        elem_size: usize,
        payload_len: u64,
    },
    CorruptVarint(&'static str),
    CorruptPayload(String),
}

impl fmt::Display for FormatError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Io(e) => write!(f, "io error: {e}"),
            Self::Truncated {
                expected_min,
                actual,
            } => {
                write!(
                    f,
                    "file truncated: need >= {expected_min} bytes, got {actual}"
                )
            }
            Self::BadMagic => write!(f, "invalid DH-TMS magic bytes"),
            Self::UnsupportedVersion(v) => write!(f, "unsupported format version {v}"),
            Self::EndianMismatch(e) => write!(f, "endian marker mismatch: 0x{e:08x}"),
            Self::KindMismatch { expected, actual } => {
                write!(f, "file kind mismatch: expected {expected:?}, got {actual}")
            }
            Self::GenerationMismatch { expected, actual } => {
                write!(f, "generation mismatch: expected {expected}, got {actual}")
            }
            Self::HeaderCrcMismatch { expected, actual } => {
                write!(
                    f,
                    "header crc32 mismatch: expected 0x{expected:08x}, got 0x{actual:08x}"
                )
            }
            Self::Blake3Mismatch => write!(f, "payload BLAKE3 checksum mismatch"),
            Self::LengthMismatch { expected, actual } => {
                write!(
                    f,
                    "file length mismatch: header declares {expected}, file is {actual}"
                )
            }
            Self::AlignmentViolation { align, addr } => {
                write!(
                    f,
                    "alignment violation: addr 0x{addr:x} not aligned to {align}"
                )
            }
            Self::RecordCountMismatch {
                records,
                elem_size,
                payload_len,
            } => {
                write!(
                    f,
                    "record count mismatch: {records} * {elem_size} != {payload_len}"
                )
            }
            Self::CorruptVarint(msg) => write!(f, "corrupt varint: {msg}"),
            Self::CorruptPayload(msg) => write!(f, "corrupt payload: {msg}"),
        }
    }
}

impl std::error::Error for FormatError {}

impl From<io::Error> for FormatError {
    fn from(e: io::Error) -> Self {
        Self::Io(e)
    }
}

/// Marker trait for plain-old-data `#[repr(C)]` types safe to view from validated aligned bytes.
///
/// # Safety
/// Implementors must be `#[repr(C)]`, have no invalid bit patterns for any field,
/// no interior mutability, and no implicit uninitialized padding bytes written to disk.
pub unsafe trait Pod: Copy + 'static {}

unsafe impl Pod for u8 {}
unsafe impl Pod for u16 {}
unsafe impl Pod for u32 {}
unsafe impl Pod for u64 {}
unsafe impl Pod for i32 {}
unsafe impl Pod for i64 {}
unsafe impl Pod for [u8; 20] {}
unsafe impl Pod for [u8; 32] {}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct FileHeader {
    pub file_kind: FileKind,
    pub format_version: u16,
    pub generation_id: u64,
    pub payload_len: u64,
    pub record_count: u64,
    pub aux_u64: u64,
    pub flags: u32,
    pub payload_blake3: [u8; 32],
}

impl FileHeader {
    pub fn to_bytes(&self) -> [u8; HEADER_LEN] {
        let mut buf = [0u8; HEADER_LEN];
        buf[0..8].copy_from_slice(&MAGIC);
        buf[8..10].copy_from_slice(&(self.file_kind as u16).to_le_bytes());
        buf[10..12].copy_from_slice(&self.format_version.to_le_bytes());
        buf[12..16].copy_from_slice(&ENDIAN_MARKER.to_le_bytes());
        buf[16..24].copy_from_slice(&self.generation_id.to_le_bytes());
        buf[24..32].copy_from_slice(&self.payload_len.to_le_bytes());
        buf[32..40].copy_from_slice(&self.record_count.to_le_bytes());
        buf[40..48].copy_from_slice(&self.aux_u64.to_le_bytes());
        buf[48..52].copy_from_slice(&self.flags.to_le_bytes());
        // 52..56 = header_crc32
        // 56..64 = reserved (0)
        buf[64..96].copy_from_slice(&self.payload_blake3);
        let crc = header_crc(&buf);
        buf[52..56].copy_from_slice(&crc.to_le_bytes());
        buf
    }

    pub fn parse_and_validate(
        bytes: &[u8],
        expected_kind: FileKind,
        expected_gen: Option<u64>,
    ) -> Result<Self, FormatError> {
        if bytes.len() < HEADER_LEN {
            return Err(FormatError::Truncated {
                expected_min: HEADER_LEN,
                actual: bytes.len(),
            });
        }
        if bytes[0..8] != MAGIC {
            return Err(FormatError::BadMagic);
        }
        let raw_kind = u16::from_le_bytes(bytes[8..10].try_into().unwrap());
        let kind = FileKind::from_u16(raw_kind).ok_or(FormatError::KindMismatch {
            expected: expected_kind,
            actual: raw_kind,
        })?;
        if kind != expected_kind {
            return Err(FormatError::KindMismatch {
                expected: expected_kind,
                actual: raw_kind,
            });
        }
        let ver = u16::from_le_bytes(bytes[10..12].try_into().unwrap());
        if ver != FORMAT_VERSION {
            return Err(FormatError::UnsupportedVersion(ver));
        }
        let endian = u32::from_le_bytes(bytes[12..16].try_into().unwrap());
        if endian != ENDIAN_MARKER {
            return Err(FormatError::EndianMismatch(endian));
        }
        let stored_crc = u32::from_le_bytes(bytes[52..56].try_into().unwrap());
        let calc_crc = header_crc(bytes);
        if stored_crc != calc_crc {
            return Err(FormatError::HeaderCrcMismatch {
                expected: stored_crc,
                actual: calc_crc,
            });
        }
        let generation_id = u64::from_le_bytes(bytes[16..24].try_into().unwrap());
        if let Some(exp) = expected_gen {
            if generation_id != exp {
                return Err(FormatError::GenerationMismatch {
                    expected: exp,
                    actual: generation_id,
                });
            }
        }
        let payload_len = u64::from_le_bytes(bytes[24..32].try_into().unwrap());
        let record_count = u64::from_le_bytes(bytes[32..40].try_into().unwrap());
        let aux_u64 = u64::from_le_bytes(bytes[40..48].try_into().unwrap());
        let flags = u32::from_le_bytes(bytes[48..52].try_into().unwrap());
        let mut payload_blake3 = [0u8; 32];
        payload_blake3.copy_from_slice(&bytes[64..96]);
        Ok(Self {
            file_kind: kind,
            format_version: ver,
            generation_id,
            payload_len,
            record_count,
            aux_u64,
            flags,
            payload_blake3,
        })
    }
}

fn header_crc(buf: &[u8]) -> u32 {
    let mut h = Crc32::new();
    h.update(&buf[0..52]);
    h.update(&buf[56..HEADER_LEN]);
    h.finalize()
}

/// Single checked read-only mmap wrapper verifying header, generation, length, alignment,
/// and BLAKE3 before exposing any payload or typed slice.
pub struct CheckedMmap {
    mmap: Mmap,
    header: FileHeader,
}

impl CheckedMmap {
    pub fn open(
        path: &Path,
        expected_kind: FileKind,
        expected_gen: Option<u64>,
        force_verify_blake3: bool,
    ) -> Result<Self, FormatError> {
        let f = File::open(path)?;
        let meta_len = f.metadata()?.len();
        if meta_len < HEADER_LEN as u64 {
            return Err(FormatError::Truncated {
                expected_min: HEADER_LEN,
                actual: meta_len as usize,
            });
        }
        // SAFETY: File is immutable inside a published generation directory.
        // We validate exact byte extents, header CRC, endianness, generation,
        // and alignment before exposing any slice.
        let mmap = unsafe { Mmap::map(&f)? };
        let header = FileHeader::parse_and_validate(&mmap, expected_kind, expected_gen)?;
        let expected_total = (HEADER_LEN as u64)
            .checked_add(header.payload_len)
            .ok_or_else(|| FormatError::CorruptPayload("payload_len overflow".into()))?;
        if mmap.len() as u64 != expected_total {
            return Err(FormatError::LengthMismatch {
                expected: expected_total,
                actual: mmap.len() as u64,
            });
        }
        let cm = Self { mmap, header };
        if force_verify_blake3 || header.payload_len <= EAGER_BLAKE3_MAX_BYTES {
            cm.verify_full_blake3()?;
        }
        Ok(cm)
    }

    #[inline]
    pub fn header(&self) -> &FileHeader {
        &self.header
    }

    #[inline]
    pub fn payload(&self) -> &[u8] {
        &self.mmap[HEADER_LEN..]
    }

    pub fn verify_full_blake3(&self) -> Result<(), FormatError> {
        let digest = blake3::hash(self.payload());
        if digest.as_bytes() != &self.header.payload_blake3 {
            return Err(FormatError::Blake3Mismatch);
        }
        Ok(())
    }

    /// Safely view the validated payload as `&[T]` after checking alignment and exact element count.
    pub fn typed_slice<T: Pod>(&self) -> Result<&[T], FormatError> {
        let elem_size = std::mem::size_of::<T>();
        let align = std::mem::align_of::<T>();
        if elem_size == 0 {
            return Err(FormatError::CorruptPayload(
                "zero-sized Pod not allowed".into(),
            ));
        }
        let expected_bytes = self
            .header
            .record_count
            .checked_mul(elem_size as u64)
            .ok_or(FormatError::RecordCountMismatch {
                records: self.header.record_count,
                elem_size,
                payload_len: self.header.payload_len,
            })?;
        if expected_bytes != self.header.payload_len {
            return Err(FormatError::RecordCountMismatch {
                records: self.header.record_count,
                elem_size,
                payload_len: self.header.payload_len,
            });
        }
        let p = self.payload();
        if self.header.record_count == 0 {
            return Ok(&[]);
        }
        let addr = p.as_ptr() as usize;
        if !addr.is_multiple_of(align) {
            return Err(FormatError::AlignmentViolation { align, addr });
        }
        // SAFETY: Alignment, exact byte length, Pod invariant, and lifetime tied to &self
        // have all been checked above.
        Ok(unsafe {
            std::slice::from_raw_parts(p.as_ptr() as *const T, self.header.record_count as usize)
        })
    }
}

/// Write a slice of bytes atomically with a 96-byte validated BLAKE3 header (tmp + fsync + rename).
pub fn write_checked_bytes(
    path: &Path,
    kind: FileKind,
    generation_id: u64,
    record_count: u64,
    aux_u64: u64,
    flags: u32,
    payload: &[u8],
) -> Result<u64, FormatError> {
    let digest = blake3::hash(payload);
    let hdr = FileHeader {
        file_kind: kind,
        format_version: FORMAT_VERSION,
        generation_id,
        payload_len: payload.len() as u64,
        record_count,
        aux_u64,
        flags,
        payload_blake3: *digest.as_bytes(),
    };
    let tmp = tmp_path_for(path);
    {
        let f = OpenOptions::new()
            .write(true)
            .create(true)
            .truncate(true)
            .open(&tmp)?;
        let mut w = BufWriter::with_capacity(4 * 1024 * 1024, f);
        w.write_all(&hdr.to_bytes())?;
        w.write_all(payload)?;
        w.flush()?;
        w.get_ref().sync_data()?;
    }
    fs::rename(&tmp, path)?;
    Ok(HEADER_LEN as u64 + payload.len() as u64)
}

/// Write a typed Pod slice atomically with validated header.
pub fn write_checked_pod_slice<T: Pod>(
    path: &Path,
    kind: FileKind,
    generation_id: u64,
    aux_u64: u64,
    flags: u32,
    slice: &[T],
) -> Result<u64, FormatError> {
    let bytes = unsafe {
        std::slice::from_raw_parts(slice.as_ptr() as *const u8, std::mem::size_of_val(slice))
    };
    write_checked_bytes(
        path,
        kind,
        generation_id,
        slice.len() as u64,
        aux_u64,
        flags,
        bytes,
    )
}

/// Streaming writer that computes BLAKE3 incrementally, then seeks back to patch the 96-byte header,
/// fsyncs, and atomically renames into place.
pub struct CheckedFileWriter {
    tmp_path: PathBuf,
    final_path: PathBuf,
    writer: BufWriter<File>,
    hasher: blake3::Hasher,
    payload_len: u64,
    kind: FileKind,
    generation_id: u64,
}

impl CheckedFileWriter {
    pub fn create(
        final_path: &Path,
        kind: FileKind,
        generation_id: u64,
    ) -> Result<Self, FormatError> {
        let tmp_path = tmp_path_for(final_path);
        let f = OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .truncate(true)
            .open(&tmp_path)?;
        let mut writer = BufWriter::with_capacity(4 * 1024 * 1024, f);
        writer.write_all(&[0u8; HEADER_LEN])?;
        Ok(Self {
            tmp_path,
            final_path: final_path.to_path_buf(),
            writer,
            hasher: blake3::Hasher::new(),
            payload_len: 0,
            kind,
            generation_id,
        })
    }

    #[inline]
    pub fn write_payload(&mut self, data: &[u8]) -> io::Result<()> {
        self.writer.write_all(data)?;
        self.hasher.update(data);
        self.payload_len += data.len() as u64;
        Ok(())
    }

    #[inline]
    pub fn payload_len(&self) -> u64 {
        self.payload_len
    }

    pub fn finish(
        mut self,
        record_count: u64,
        aux_u64: u64,
        flags: u32,
    ) -> Result<u64, FormatError> {
        self.writer.flush()?;
        let mut f = self
            .writer
            .into_inner()
            .map_err(|e| FormatError::Io(e.into_error()))?;
        let digest = self.hasher.finalize();
        let hdr = FileHeader {
            file_kind: self.kind,
            format_version: FORMAT_VERSION,
            generation_id: self.generation_id,
            payload_len: self.payload_len,
            record_count,
            aux_u64,
            flags,
            payload_blake3: *digest.as_bytes(),
        };
        f.seek(SeekFrom::Start(0))?;
        f.write_all(&hdr.to_bytes())?;
        f.sync_data()?;
        drop(f);
        fs::rename(&self.tmp_path, &self.final_path)?;
        Ok(HEADER_LEN as u64 + self.payload_len)
    }
}

fn tmp_path_for(path: &Path) -> PathBuf {
    let mut os = path.as_os_str().to_os_string();
    os.push(".tmp");
    PathBuf::from(os)
}

pub fn sync_dir(dir: &Path) -> io::Result<()> {
    let f = File::open(dir)?;
    f.sync_all()
}

/// Atomic CURRENT pointer manifest for published immutable generations.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct CurrentManifest {
    pub generation_id: u64,
    pub engine_kind: String,
    pub num_docs: u64,
    pub pg_lsn: String,
    pub created_epoch_sec: u64,
    pub max_created_epoch_sec: u64,
    pub hash_seed: u64,
}

pub fn generation_dir(root: &Path, generation_id: u64) -> PathBuf {
    root.join("generations")
        .join(format!("gen-{generation_id:016x}"))
}

pub fn stage_generation_dir(root: &Path, generation_id: u64) -> io::Result<PathBuf> {
    let gens = root.join("generations");
    fs::create_dir_all(&gens)?;
    let tmp = gens.join(format!("gen-{generation_id:016x}.tmp"));
    if tmp.exists() {
        fs::remove_dir_all(&tmp)?;
    }
    fs::create_dir_all(&tmp)?;
    Ok(tmp)
}

/// Publish a staged generation directory atomically:
/// fsync dir -> rename gen.tmp -> gen -> fsync generations/ -> write CURRENT.tmp -> fsync -> rename -> fsync root.
pub fn publish_generation(
    root: &Path,
    staged_dir: &Path,
    manifest: &CurrentManifest,
) -> Result<PathBuf, FormatError> {
    sync_dir(staged_dir)?;
    let final_dir = generation_dir(root, manifest.generation_id);
    if final_dir.exists() {
        return Err(FormatError::CorruptPayload(format!(
            "generation {:016x} already exists; refusing in-place overwrite",
            manifest.generation_id
        )));
    }
    fs::rename(staged_dir, &final_dir)?;
    sync_dir(&root.join("generations"))?;

    let current_path = root.join("CURRENT.json");
    let current_tmp = root.join("CURRENT.json.tmp");
    let json = serde_json::to_vec_pretty(manifest)
        .map_err(|e| FormatError::CorruptPayload(e.to_string()))?;
    {
        let mut f = OpenOptions::new()
            .write(true)
            .create(true)
            .truncate(true)
            .open(&current_tmp)?;
        f.write_all(&json)?;
        f.sync_data()?;
    }
    fs::rename(&current_tmp, &current_path)?;
    sync_dir(root)?;
    Ok(final_dir)
}

pub fn read_current_manifest(root: &Path) -> Result<(CurrentManifest, PathBuf), FormatError> {
    let raw = fs::read(root.join("CURRENT.json"))?;
    let m: CurrentManifest = serde_json::from_slice(&raw)
        .map_err(|e| FormatError::CorruptPayload(format!("invalid CURRENT.json: {e}")))?;
    let gdir = generation_dir(root, m.generation_id);
    if !gdir.is_dir() {
        return Err(FormatError::CorruptPayload(format!(
            "generation directory missing: {}",
            gdir.display()
        )));
    }
    Ok((m, gdir))
}

// ---------------- Checked LEB128 Varints ----------------

#[inline]
pub fn write_varint_u64(out: &mut Vec<u8>, mut v: u64) {
    while v >= 0x80 {
        out.push((v as u8 & 0x7F) | 0x80);
        v >>= 7;
    }
    out.push(v as u8);
}

#[inline]
pub fn write_varint_u32(out: &mut Vec<u8>, mut v: u32) {
    while v >= 0x80 {
        out.push((v as u8 & 0x7F) | 0x80);
        v >>= 7;
    }
    out.push(v as u8);
}

#[inline]
pub fn read_varint_u64(buf: &[u8], pos: &mut usize) -> Result<u64, FormatError> {
    let mut v: u64 = 0;
    let mut shift: u32 = 0;
    loop {
        if *pos >= buf.len() {
            return Err(FormatError::CorruptVarint("unexpected EOF in u64 varint"));
        }
        let b = buf[*pos];
        *pos += 1;
        if shift == 63 && (b & 0xFE) != 0 {
            return Err(FormatError::CorruptVarint("u64 varint overflow"));
        }
        v |= ((b & 0x7F) as u64) << shift;
        if (b & 0x80) == 0 {
            return Ok(v);
        }
        shift += 7;
        if shift >= 64 {
            return Err(FormatError::CorruptVarint("u64 varint exceeds 10 bytes"));
        }
    }
}

#[inline]
pub fn read_varint_u32(buf: &[u8], pos: &mut usize) -> Result<u32, FormatError> {
    let mut v: u32 = 0;
    let mut shift: u32 = 0;
    loop {
        if *pos >= buf.len() {
            return Err(FormatError::CorruptVarint("unexpected EOF in u32 varint"));
        }
        let b = buf[*pos];
        *pos += 1;
        if shift == 28 && (b & 0xF0) != 0 {
            return Err(FormatError::CorruptVarint("u32 varint overflow"));
        }
        v |= ((b & 0x7F) as u32) << shift;
        if (b & 0x80) == 0 {
            return Ok(v);
        }
        shift += 7;
        if shift >= 32 {
            return Err(FormatError::CorruptVarint("u32 varint exceeds 5 bytes"));
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_varint_roundtrip_and_errors() {
        let mut buf = Vec::new();
        for &v in &[0u64, 1, 127, 128, 16384, u32::MAX as u64, u64::MAX] {
            write_varint_u64(&mut buf, v);
        }
        let mut pos = 0;
        for &expected in &[0u64, 1, 127, 128, 16384, u32::MAX as u64, u64::MAX] {
            assert_eq!(read_varint_u64(&buf, &mut pos).unwrap(), expected);
        }
        assert_eq!(pos, buf.len());

        // Truncated varint must error
        let mut p = 0;
        assert!(read_varint_u64(&[0x80], &mut p).is_err());

        // Overflow u32 varint must error
        let mut p = 0;
        assert!(read_varint_u32(&[0xFF, 0xFF, 0xFF, 0xFF, 0x10], &mut p).is_err());

        // Overlong u64 varint must error
        let mut p = 0;
        assert!(read_varint_u64(&[0x80; 11], &mut p).is_err());
    }

    #[test]
    fn test_checked_mmap_rejects_truncation_corruption_and_wrong_gen() {
        let dir = std::env::temp_dir().join(format!("dhtms_fmt_test_{}", std::process::id()));
        let _ = fs::remove_dir_all(&dir);
        fs::create_dir_all(&dir).unwrap();
        let path = dir.join("meta.bin");
        let data: Vec<u64> = vec![10, 20, 30, 40];
        write_checked_pod_slice(&path, FileKind::MetaCol, 42, 0, 0, &data).unwrap();

        // Valid open
        let cm = CheckedMmap::open(&path, FileKind::MetaCol, Some(42), true).unwrap();
        assert_eq!(cm.typed_slice::<u64>().unwrap(), &data[..]);

        // Wrong generation rejected
        assert!(matches!(
            CheckedMmap::open(&path, FileKind::MetaCol, Some(99), true),
            Err(FormatError::GenerationMismatch { .. })
        ));

        // Wrong kind rejected
        assert!(matches!(
            CheckedMmap::open(&path, FileKind::FacetCol, Some(42), true),
            Err(FormatError::KindMismatch { .. })
        ));

        // Corrupt 1 payload byte -> BLAKE3 mismatch
        let mut raw = fs::read(&path).unwrap();
        raw[HEADER_LEN + 3] ^= 0xFF;
        let bad_path = dir.join("bad_hash.bin");
        fs::write(&bad_path, &raw).unwrap();
        assert!(matches!(
            CheckedMmap::open(&bad_path, FileKind::MetaCol, Some(42), true),
            Err(FormatError::Blake3Mismatch)
        ));

        // Truncated file rejected before slice creation
        let trunc_path = dir.join("trunc.bin");
        fs::write(&trunc_path, &raw[..HEADER_LEN + 8]).unwrap();
        assert!(matches!(
            CheckedMmap::open(&trunc_path, FileKind::MetaCol, Some(42), false),
            Err(FormatError::LengthMismatch { .. })
        ));

        let _ = fs::remove_dir_all(&dir);
    }
}
