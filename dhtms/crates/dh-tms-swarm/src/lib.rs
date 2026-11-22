use crc32fast::Hasher as Crc32;
use dh_tms_format::{FileHeader, FileKind, FormatError, FORMAT_VERSION, HEADER_LEN};
use memmap2::MmapRaw;
use parking_lot::Mutex;
use roaring::RoaringBitmap;
use std::collections::{BinaryHeap, HashSet};
use std::fmt;
use std::fs::{File, OpenOptions};
use std::io::{self, Read, Seek, SeekFrom, Write};
use std::path::{Path, PathBuf};
use std::sync::atomic::{fence, AtomicU32, AtomicU64, Ordering};

const SWARM_WAL_MAGIC: [u8; 4] = *b"SWAL";

pub const UNKNOWN_COUNT_24: u32 = 0x00FF_FFFF; // 24-bit sentinel distinguishing unknown from authoritative 0
pub const MAX_COUNT_24: u32 = 0x00FF_FFFE; // 16,777,214 max seeders/leechers
pub const FLAG_TRACKER_KNOWN: u16 = 1 << 0;
pub const FLAG_LIVE: u16 = 1 << 1;

/// 16-byte seqlock-coherent telemetry slot (zero torn 128-bit reads!):
///   word0: bits 0..24  = seeders (24-bit, 0xFFFFFF = unknown)
///          bits 24..48 = leechers (24-bit, 0xFFFFFF = unknown)
///          bits 48..64 = seqlock_seq (even = stable, odd = writer active)
///   word1: bits 0..32  = checked_epoch_min (monotonic scrape timestamp)
///          bits 32..48 = peak_seeders (u16 saturating)
///          bits 48..64 = flags (low byte) | tracker_id (high byte)
#[repr(C, align(16))]
pub struct SwarmSlot {
    word0: AtomicU64,
    word1: AtomicU64,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct SwarmReading {
    pub seeders: Option<u32>,
    pub leechers: Option<u32>,
    pub peak_seeders: u16,
    pub checked_epoch_min: u32,
    pub tracker_known: bool,
    pub tracker_id: u8,
}

#[derive(Debug, Clone, Copy)]
pub struct SwarmUpdate {
    pub doc_id: u32,
    pub seeders: Option<u32>,
    pub leechers: Option<u32>,
    pub checked_epoch_min: u32,
    pub tracker_id: u8,
}

#[derive(Debug)]
pub enum SwarmError {
    Format(FormatError),
    Io(io::Error),
    OutOfRange { doc_id: u32, num_docs: usize },
    WalGenerationMismatch { expected: u64, actual: u64 },
    CorruptWal(String),
}

impl fmt::Display for SwarmError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Format(e) => write!(f, "swarm format error: {e}"),
            Self::Io(e) => write!(f, "swarm io error: {e}"),
            Self::OutOfRange { doc_id, num_docs } => {
                write!(
                    f,
                    "swarm doc_id {doc_id} out of bounds (num_docs={num_docs})"
                )
            }
            Self::WalGenerationMismatch { expected, actual } => {
                write!(
                    f,
                    "swarm WAL generation mismatch: expected {expected}, got {actual}"
                )
            }
            Self::CorruptWal(m) => write!(f, "corrupt swarm WAL: {m}"),
        }
    }
}

impl std::error::Error for SwarmError {}
impl From<FormatError> for SwarmError {
    fn from(e: FormatError) -> Self {
        Self::Format(e)
    }
}
impl From<io::Error> for SwarmError {
    fn from(e: io::Error) -> Self {
        Self::Io(e)
    }
}

#[inline]
fn pack_w0(seeders: Option<u32>, leechers: Option<u32>, seq: u16) -> u64 {
    let s = seeders
        .map(|v| v.min(MAX_COUNT_24))
        .unwrap_or(UNKNOWN_COUNT_24) as u64;
    let l = leechers
        .map(|v| v.min(MAX_COUNT_24))
        .unwrap_or(UNKNOWN_COUNT_24) as u64;
    (s & 0x00FF_FFFF) | ((l & 0x00FF_FFFF) << 24) | ((seq as u64) << 48)
}

#[inline]
fn pack_w1(checked_min: u32, peak_seeders: u16, flags: u16) -> u64 {
    (checked_min as u64) | ((peak_seeders as u64) << 32) | ((flags as u64) << 48)
}

struct SwarmWriterState {
    wal_file: File,
    chk_file: File,
    next_seq: u64,
    wal_bytes_since_ckpt: u64,
}

/// Strict-provenance-sound, seqlock-coherent, WAL-before-mutate SwarmPlane backed by `MmapRaw`.
pub struct SwarmPlane {
    pub generation_id: u64,
    pub num_docs: usize,
    raw_mmap: MmapRaw,
    wal_path: PathBuf,
    writer: Mutex<SwarmWriterState>,
    stale_cursor: AtomicU32,
}

// SAFETY: `MmapRaw` points to a fixed-size file-backed mapping owned by `SwarmPlane`.
// All reads and writes to slots go strictly through `AtomicU64` with seqlock coordination,
// and all writes are serialized by `self.writer: Mutex<_>`.
unsafe impl Send for SwarmPlane {}
unsafe impl Sync for SwarmPlane {}

impl SwarmPlane {
    /// Create a fresh checked `swarm.chk` file for a generation (staging atomically before publish).
    pub fn create(
        dir: &Path,
        generation_id: u64,
        initial: &[(i32, i32, u32)],
    ) -> Result<u64, SwarmError> {
        let num_docs = initial.len();
        let payload_len = (num_docs as u64)
            .checked_mul(std::mem::size_of::<SwarmSlot>() as u64)
            .ok_or_else(|| FormatError::CorruptPayload("swarm size overflow".into()))?;
        let total_len = (HEADER_LEN as u64)
            .checked_add(payload_len)
            .ok_or_else(|| FormatError::CorruptPayload("swarm total size overflow".into()))?;

        let chk_path = dir.join("swarm.chk");
        let tmp_path = dir.join("swarm.chk.tmp");
        let mut f = OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .truncate(true)
            .open(&tmp_path)?;
        f.set_len(total_len)?;

        let hdr = FileHeader {
            file_kind: FileKind::SwarmCheckpoint,
            format_version: FORMAT_VERSION,
            generation_id,
            payload_len,
            record_count: num_docs as u64,
            aux_u64: 0, // checkpointed WAL sequence number
            flags: 0,
            payload_blake3: [0u8; 32], // mutable checkpoint relies on header CRC + WAL CRC + fsync
        };
        f.seek(SeekFrom::Start(0))?;
        f.write_all(&hdr.to_bytes())?;

        // Write initial slot bytes sequentially
        let mut buf = Vec::with_capacity(64 * 1024);
        for &(s, l, checked_min) in initial {
            let so = if s < 0 { None } else { Some(s as u32) };
            let lo = if l < 0 { None } else { Some(l as u32) };
            let peak = so.map(|v| v.min(u16::MAX as u32) as u16).unwrap_or(0);
            let mut flags = 0u16;
            if so.is_some() {
                flags |= FLAG_TRACKER_KNOWN;
                if so.unwrap_or(0) > 0 || lo.unwrap_or(0) > 0 {
                    flags |= FLAG_LIVE;
                }
            }
            let w0 = pack_w0(so, lo, 0);
            let w1 = pack_w1(checked_min, peak, flags);
            buf.extend_from_slice(&w0.to_le_bytes());
            buf.extend_from_slice(&w1.to_le_bytes());
            if buf.len() >= 64 * 1024 {
                f.write_all(&buf)?;
                buf.clear();
            }
        }
        if !buf.is_empty() {
            f.write_all(&buf)?;
        }
        f.sync_data()?;
        drop(f);
        std::fs::rename(&tmp_path, &chk_path)?;
        Ok(total_len)
    }

    /// Open `swarm.chk` with `MmapRaw`, validate header/generation/extent/alignment,
    /// and replay `swarm.wal` (truncating any torn crash tail and rejecting wrong-generation WAL).
    pub fn open(
        gen_dir: &Path,
        wal_path: &Path,
        expected_gen: Option<u64>,
    ) -> Result<Self, SwarmError> {
        let chk_path = gen_dir.join("swarm.chk");
        let mut chk_file = OpenOptions::new().read(true).write(true).open(&chk_path)?;
        let file_len = chk_file.metadata()?.len();
        if file_len < HEADER_LEN as u64 {
            return Err(FormatError::Truncated {
                expected_min: HEADER_LEN,
                actual: file_len as usize,
            }
            .into());
        }
        let mut hdr_bytes = [0u8; HEADER_LEN];
        chk_file.seek(SeekFrom::Start(0))?;
        chk_file.read_exact(&mut hdr_bytes)?;
        let hdr =
            FileHeader::parse_and_validate(&hdr_bytes, FileKind::SwarmCheckpoint, expected_gen)?;
        let expected_total = (HEADER_LEN as u64)
            .checked_add(hdr.payload_len)
            .ok_or_else(|| FormatError::CorruptPayload("swarm header overflow".into()))?;
        if file_len != expected_total {
            return Err(FormatError::LengthMismatch {
                expected: expected_total,
                actual: file_len,
            }
            .into());
        }
        let expected_payload = hdr
            .record_count
            .checked_mul(std::mem::size_of::<SwarmSlot>() as u64)
            .ok_or_else(|| FormatError::CorruptPayload("swarm record count overflow".into()))?;
        if expected_payload != hdr.payload_len {
            return Err(FormatError::RecordCountMismatch {
                records: hdr.record_count,
                elem_size: std::mem::size_of::<SwarmSlot>(),
                payload_len: hdr.payload_len,
            }
            .into());
        }

        let raw_mmap = MmapRaw::map_raw(&chk_file)?;
        // Validate base pointer alignment (HEADER_LEN == 96 is 16-byte aligned; page start is 4096-aligned)
        let slots_ptr = unsafe { raw_mmap.as_mut_ptr().add(HEADER_LEN) };
        if !(slots_ptr as usize).is_multiple_of(std::mem::align_of::<SwarmSlot>()) {
            return Err(FormatError::AlignmentViolation {
                align: std::mem::align_of::<SwarmSlot>(),
                addr: slots_ptr as usize,
            }
            .into());
        }

        let num_docs = hdr.record_count as usize;
        let generation_id = hdr.generation_id;
        let ckpt_seq = hdr.aux_u64;

        let mut sp = Self {
            generation_id,
            num_docs,
            raw_mmap,
            wal_path: wal_path.to_path_buf(),
            writer: Mutex::new(SwarmWriterState {
                wal_file: OpenOptions::new()
                    .create(true)
                    .append(true)
                    .open(wal_path)?,
                chk_file,
                next_seq: ckpt_seq + 1,
                wal_bytes_since_ckpt: 0,
            }),
            stale_cursor: AtomicU32::new(0),
        };

        // Replay durable WAL frames strictly after ckpt_seq (handles torn crash tails!)
        let (last_seq, valid_wal_bytes) = sp.replay_wal(ckpt_seq)?;
        {
            let mut ws = sp.writer.lock();
            ws.next_seq = last_seq.max(ckpt_seq) + 1;
            ws.wal_bytes_since_ckpt = valid_wal_bytes;
            // Re-open wal_file in append mode in case replay truncated a torn tail
            ws.wal_file = OpenOptions::new()
                .create(true)
                .append(true)
                .open(wal_path)?;
        }
        Ok(sp)
    }

    #[inline]
    fn slot_ptr(&self, doc_id: u32) -> *const SwarmSlot {
        debug_assert!((doc_id as usize) < self.num_docs);
        // SAFETY: Validated file extent (HEADER_LEN + num_docs * 16) and 16-byte alignment in open().
        // Pointer derives directly from MmapRaw::as_mut_ptr() (writable provenance preserved!).
        unsafe {
            (self.raw_mmap.as_mut_ptr().add(HEADER_LEN) as *const SwarmSlot).add(doc_id as usize)
        }
    }

    /// Lock-free coherent read using per-slot seqlock — NEVER returns a torn 128-bit state!
    #[inline]
    pub fn read(&self, doc_id: u32) -> Result<SwarmReading, SwarmError> {
        if (doc_id as usize) >= self.num_docs {
            return Err(SwarmError::OutOfRange {
                doc_id,
                num_docs: self.num_docs,
            });
        }
        let slot = unsafe { &*self.slot_ptr(doc_id) };
        loop {
            let w0_a = slot.word0.load(Ordering::Acquire);
            let seq_a = (w0_a >> 48) as u16;
            if (seq_a & 1) != 0 {
                std::hint::spin_loop();
                continue;
            }
            let w1 = slot.word1.load(Ordering::Acquire);
            fence(Ordering::Acquire);
            let w0_b = slot.word0.load(Ordering::Acquire);
            if w0_a == w0_b {
                let s_raw = (w0_a & 0x00FF_FFFF) as u32;
                let l_raw = ((w0_a >> 24) & 0x00FF_FFFF) as u32;
                let checked_epoch_min = w1 as u32;
                let peak_seeders = (w1 >> 32) as u16;
                let flags_tid = (w1 >> 48) as u16;
                return Ok(SwarmReading {
                    seeders: if s_raw == UNKNOWN_COUNT_24 {
                        None
                    } else {
                        Some(s_raw)
                    },
                    leechers: if l_raw == UNKNOWN_COUNT_24 {
                        None
                    } else {
                        Some(l_raw)
                    },
                    peak_seeders,
                    checked_epoch_min,
                    tracker_known: (flags_tid & FLAG_TRACKER_KNOWN) != 0,
                    tracker_id: (flags_tid >> 8) as u8,
                });
            }
            std::hint::spin_loop();
        }
    }

    /// Internal serialized seqlock mutation — called ONLY while holding `self.writer` lock (or during single-threaded replay).
    /// Rejects stale out-of-order scrapes (`checked_epoch_min < prev_checked`) and preserves monotonic `peak_seeders`.
    fn apply_slot_serialized(&self, u: SwarmUpdate) {
        let slot = unsafe { &*self.slot_ptr(u.doc_id) };
        let prev_w0 = slot.word0.load(Ordering::Relaxed);
        let prev_w1 = slot.word1.load(Ordering::Relaxed);
        let prev_checked = prev_w1 as u32;
        if u.checked_epoch_min < prev_checked {
            // Reject stale out-of-order scrape!
            return;
        }
        let prev_peak = (prev_w1 >> 32) as u16;
        let new_peak = u
            .seeders
            .map(|x| prev_peak.max(x.min(u16::MAX as u32) as u16))
            .unwrap_or(prev_peak);
        let mut flags = ((u.tracker_id as u16) << 8) & 0xFF00;
        if u.seeders.is_some() {
            flags |= FLAG_TRACKER_KNOWN;
            if u.seeders.unwrap_or(0) > 0 || u.leechers.unwrap_or(0) > 0 {
                flags |= FLAG_LIVE;
            }
        }
        let prev_seq = ((prev_w0 >> 48) as u16) & !1u16; // ensure even base
        let odd_seq = prev_seq.wrapping_add(1);
        let even_seq = prev_seq.wrapping_add(2);

        // 1. Mark slot write-in-progress (odd seq)
        let w0_odd = (prev_w0 & 0x0000_FFFF_FFFF_FFFF) | ((odd_seq as u64) << 48);
        slot.word0.store(w0_odd, Ordering::Release);
        fence(Ordering::Release);

        // 2. Write word1 (checked_min, peak, flags)
        let w1_new = pack_w1(u.checked_epoch_min, new_peak, flags);
        slot.word1.store(w1_new, Ordering::Relaxed);
        fence(Ordering::Release);

        // 3. Publish word0 with new counts + next even seq
        let w0_new = pack_w0(u.seeders, u.leechers, even_seq);
        slot.word0.store(w0_new, Ordering::Release);
    }

    /// Durable WAL-before-mutate batch update!
    /// 1. Validates all doc_ids up front.
    /// 2. Appends framed, generation-tagged, sequence-numbered, CRC32-checksummed WAL batch.
    /// 3. Calls `sync_data()` BEFORE mutating mapped slots when `sync_wal == true`.
    /// 4. Applies seqlock-coherent slot updates under serialized writer lock.
    pub fn update_batch_durable(
        &self,
        updates: &[SwarmUpdate],
        sync_wal: bool,
    ) -> Result<u64, SwarmError> {
        if updates.is_empty() {
            return Ok(0);
        }
        for u in updates {
            if (u.doc_id as usize) >= self.num_docs {
                return Err(SwarmError::OutOfRange {
                    doc_id: u.doc_id,
                    num_docs: self.num_docs,
                });
            }
        }

        let mut ws = self.writer.lock();
        let seq = ws.next_seq;
        let frame = encode_swarm_wal_frame(self.generation_id, seq, updates);
        ws.wal_file.write_all(&frame)?;
        ws.wal_file.flush()?;
        if sync_wal {
            ws.wal_file.sync_data()?;
        }
        ws.next_seq += 1;
        ws.wal_bytes_since_ckpt += frame.len() as u64;

        for &u in updates {
            self.apply_slot_serialized(u);
        }
        Ok(seq)
    }

    /// Checkpoint mapped telemetry state durably to `swarm.chk` and truncate `swarm.wal` to 0 bytes
    /// (preventing unbounded WAL growth!).
    pub fn checkpoint(&self) -> Result<u64, SwarmError> {
        let mut ws = self.writer.lock();
        let ckpt_seq = ws.next_seq.saturating_sub(1);
        // 1. Flush raw mapping pages to disk
        self.raw_mmap.flush()?;
        ws.chk_file.sync_data()?;

        // 2. Persist updated checkpoint sequence in header
        let hdr = FileHeader {
            file_kind: FileKind::SwarmCheckpoint,
            format_version: FORMAT_VERSION,
            generation_id: self.generation_id,
            payload_len: (self.num_docs * std::mem::size_of::<SwarmSlot>()) as u64,
            record_count: self.num_docs as u64,
            aux_u64: ckpt_seq,
            flags: 0,
            payload_blake3: [0u8; 32],
        };
        ws.chk_file.seek(SeekFrom::Start(0))?;
        ws.chk_file.write_all(&hdr.to_bytes())?;
        ws.chk_file.sync_data()?;

        // 3. Truncate WAL cleanly
        let f = OpenOptions::new().write(true).open(&self.wal_path)?;
        f.set_len(0)?;
        f.sync_data()?;
        ws.wal_file = OpenOptions::new()
            .create(true)
            .append(true)
            .open(&self.wal_path)?;
        ws.wal_bytes_since_ckpt = 0;
        Ok(ckpt_seq)
    }

    pub fn wal_bytes_since_checkpoint(&self) -> u64 {
        self.writer.lock().wal_bytes_since_ckpt
    }

    fn replay_wal(&mut self, ckpt_seq: u64) -> Result<(u64, u64), SwarmError> {
        if !self.wal_path.exists() {
            return Ok((ckpt_seq, 0));
        }
        let mut raw = Vec::new();
        File::open(&self.wal_path)?.read_to_end(&mut raw)?;
        let mut pos = 0usize;
        let mut last_seq = ckpt_seq;

        while pos < raw.len() {
            if pos + 28 > raw.len() {
                truncate_file(&self.wal_path, pos as u64)?;
                break;
            }
            if raw[pos..pos + 4] != SWARM_WAL_MAGIC {
                return Err(SwarmError::CorruptWal(format!(
                    "bad swarm WAL magic at {pos}"
                )));
            }
            let gen_id = u64::from_le_bytes(raw[pos + 4..pos + 12].try_into().unwrap());
            if gen_id != self.generation_id {
                return Err(SwarmError::WalGenerationMismatch {
                    expected: self.generation_id,
                    actual: gen_id,
                });
            }
            let seq = u64::from_le_bytes(raw[pos + 12..pos + 20].try_into().unwrap());
            let payload_len =
                u32::from_le_bytes(raw[pos + 20..pos + 24].try_into().unwrap()) as usize;
            let stored_crc = u32::from_le_bytes(raw[pos + 24..pos + 28].try_into().unwrap());
            if !payload_len.is_multiple_of(17) {
                return Err(SwarmError::CorruptWal(format!(
                    "invalid swarm WAL payload length {payload_len} at byte {pos}"
                )));
            }
            let Some(end) = pos.checked_add(28).and_then(|x| x.checked_add(payload_len)) else {
                return Err(SwarmError::CorruptWal(format!(
                    "swarm WAL frame offset overflow at byte {pos}"
                )));
            };
            if end > raw.len() {
                truncate_file(&self.wal_path, pos as u64)?;
                break;
            }
            let payload = &raw[pos + 28..end];
            let mut crc = Crc32::new();
            crc.update(&raw[pos + 4..pos + 24]);
            crc.update(payload);
            if crc.finalize() != stored_crc {
                if end == raw.len() {
                    truncate_file(&self.wal_path, pos as u64)?;
                    break;
                }
                return Err(SwarmError::CorruptWal(format!(
                    "mid-file swarm WAL CRC mismatch at byte {pos} (end={end} < len={})",
                    raw.len()
                )));
            }
            if seq > ckpt_seq {
                for rec in payload.as_chunks::<17>().0 {
                    let doc_id = u32::from_le_bytes(rec[0..4].try_into().unwrap());
                    let s_raw = u32::from_le_bytes(rec[4..8].try_into().unwrap());
                    let l_raw = u32::from_le_bytes(rec[8..12].try_into().unwrap());
                    let checked_epoch_min = u32::from_le_bytes(rec[12..16].try_into().unwrap());
                    let tracker_id = rec[16];
                    if (doc_id as usize) < self.num_docs {
                        self.apply_slot_serialized(SwarmUpdate {
                            doc_id,
                            seeders: if s_raw == UNKNOWN_COUNT_24 {
                                None
                            } else {
                                Some(s_raw)
                            },
                            leechers: if l_raw == UNKNOWN_COUNT_24 {
                                None
                            } else {
                                Some(l_raw)
                            },
                            checked_epoch_min,
                            tracker_id,
                        });
                    }
                }
                last_seq = seq;
            }
            pos = end;
        }
        Ok((last_seq, pos as u64))
    }

    /// Fair `SelectStale` (matching DH-Waypoint v5.14.0 priority fairness):
    /// 1. 25% priority slice: globally stalest `payload_eligible` docs.
    /// 2. 75% general slice: globally stalest remaining `base_eligible` docs (INCLUDING payload-eligible
    ///    overflow docs not picked in the 25% slice!), using a persistent ring cursor over `checked == 0`
    ///    never-checked ties + a bounded max-heap over `(checked_epoch_min, rotated_id)` so high `doc_id`s
    ///    NEVER starve!
    pub fn select_stale(
        &self,
        now_epoch_min: u32,
        min_age_min: u32,
        limit: usize,
        payload_eligible: &RoaringBitmap,
        base_eligible: &RoaringBitmap,
    ) -> Vec<u32> {
        if limit == 0 || self.num_docs == 0 {
            return Vec::new();
        }
        let cutoff = now_epoch_min.saturating_sub(min_age_min);
        let mut out = Vec::with_capacity(limit);
        let prio_cap = limit / 4;

        // 1. Priority payload-verified slice: globally stalest checked_epoch_min ASC
        let mut prio_cands: Vec<(u32, u32)> = Vec::new();
        for doc_id in payload_eligible.iter() {
            if (doc_id as usize) >= self.num_docs {
                continue;
            }
            let slot = unsafe { &*self.slot_ptr(doc_id) };
            let checked = slot.word1.load(Ordering::Relaxed) as u32;
            if checked == 0 || checked < cutoff {
                prio_cands.push((checked, doc_id));
            }
        }
        prio_cands.sort_unstable();
        let mut picked_prio: HashSet<u32> = HashSet::with_capacity(prio_cap);
        for &(_, d) in prio_cands.iter().take(prio_cap) {
            picked_prio.insert(d);
            out.push(d);
        }

        let rem_cap = limit.saturating_sub(out.len());
        if rem_cap == 0 {
            return out;
        }

        // 2. General pool (includes payload-eligible overflow NOT already in `picked_prio`!):
        // Use a ring-rotated tie-breaker so ties (especially checked == 0) rotate fairly across ALL doc_ids!
        let n = self.num_docs as u32;
        let cursor = self.stale_cursor.load(Ordering::Relaxed) % n;
        // Max-heap of size `rem_cap` keeps the `rem_cap` smallest `(checked_epoch_min, rotated_doc_id)`
        let mut heap: BinaryHeap<(u32, u32, u32)> = BinaryHeap::with_capacity(rem_cap + 1);

        for doc_id in base_eligible.iter() {
            if doc_id >= n || picked_prio.contains(&doc_id) {
                continue;
            }
            let slot = unsafe { &*self.slot_ptr(doc_id) };
            let checked = slot.word1.load(Ordering::Relaxed) as u32;
            if checked != 0 && checked >= cutoff {
                continue;
            }
            let rot = if doc_id >= cursor {
                doc_id - cursor
            } else {
                (n - cursor) + doc_id
            };
            let key = (checked, rot, doc_id);
            if heap.len() < rem_cap {
                heap.push(key);
            } else if let Some(&worst) = heap.peek() {
                if (key.0, key.1) < (worst.0, worst.1) {
                    heap.pop();
                    heap.push(key);
                }
            }
        }

        let mut general: Vec<(u32, u32, u32)> = heap.into_vec();
        general.sort_unstable();
        if let Some(&(_, _, last_doc)) = general.last() {
            self.stale_cursor
                .store((last_doc + 1) % n, Ordering::Relaxed);
        }
        for (_, _, d) in general {
            out.push(d);
        }
        out
    }
}

/// Frame layout (28-byte header + 17B/record payload):
/// `[SWAL: 4][gen: 8][seq: 8][payload_len: 4][crc32: 4][records...]`
fn encode_swarm_wal_frame(gen_id: u64, seq: u64, updates: &[SwarmUpdate]) -> Vec<u8> {
    let payload_len = updates.len() * 17;
    let mut payload = Vec::with_capacity(payload_len);
    for u in updates {
        let s = u
            .seeders
            .map(|v| v.min(MAX_COUNT_24))
            .unwrap_or(UNKNOWN_COUNT_24);
        let l = u
            .leechers
            .map(|v| v.min(MAX_COUNT_24))
            .unwrap_or(UNKNOWN_COUNT_24);
        payload.extend_from_slice(&u.doc_id.to_le_bytes());
        payload.extend_from_slice(&s.to_le_bytes());
        payload.extend_from_slice(&l.to_le_bytes());
        payload.extend_from_slice(&u.checked_epoch_min.to_le_bytes());
        payload.push(u.tracker_id);
    }
    let mut crc = Crc32::new();
    crc.update(&gen_id.to_le_bytes());
    crc.update(&seq.to_le_bytes());
    crc.update(&(payload_len as u32).to_le_bytes());
    crc.update(&payload);
    let checksum = crc.finalize();

    let mut frame = Vec::with_capacity(28 + payload_len);
    frame.extend_from_slice(&SWARM_WAL_MAGIC);
    frame.extend_from_slice(&gen_id.to_le_bytes());
    frame.extend_from_slice(&seq.to_le_bytes());
    frame.extend_from_slice(&(payload_len as u32).to_le_bytes());
    frame.extend_from_slice(&checksum.to_le_bytes());
    frame.extend_from_slice(&payload);
    frame
}

fn truncate_file(path: &Path, len: u64) -> io::Result<()> {
    let f = OpenOptions::new().write(true).open(path)?;
    f.set_len(len)?;
    f.sync_data()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_swarm_seqlock_wal_crash_recovery_stale_rejection_and_fair_select() {
        let dir = std::env::temp_dir().join(format!("dhtms_swarm_test_{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let wal = dir.join("swarm.wal");

        let init: Vec<(i32, i32, u32)> = (0..100)
            .map(|i| (-1, -1, if i < 50 { 100 } else { 50 }))
            .collect();
        SwarmPlane::create(&dir, 99, &init).unwrap();

        let sp = SwarmPlane::open(&dir, &wal, Some(99)).unwrap();
        // Verify unknown vs authoritative zero
        assert_eq!(sp.read(0).unwrap().seeders, None);

        // Durable update + monotonic peak + stale scrape rejection
        sp.update_batch_durable(
            &[
                SwarmUpdate {
                    doc_id: 10,
                    seeders: Some(50),
                    leechers: Some(12),
                    checked_epoch_min: 200,
                    tracker_id: 3,
                },
                SwarmUpdate {
                    doc_id: 10,
                    seeders: Some(10),
                    leechers: Some(4),
                    checked_epoch_min: 150,
                    tracker_id: 2,
                }, // stale! MUST be ignored
                SwarmUpdate {
                    doc_id: 10,
                    seeders: Some(0),
                    leechers: Some(0),
                    checked_epoch_min: 210,
                    tracker_id: 3,
                }, // authoritative 0, peak stays 50!
            ],
            true,
        )
        .unwrap();
        let r = sp.read(10).unwrap();
        assert_eq!(r.seeders, Some(0));
        assert_eq!(r.leechers, Some(0));
        assert_eq!(r.peak_seeders, 50);
        assert_eq!(r.checked_epoch_min, 210);
        assert!(r.tracker_known);

        // Fair SelectStale: docs 50..100 have checked=50 (stalest!) -> MUST be picked ahead of 0..50 (checked=100)!
        let mut base = RoaringBitmap::new();
        for d in 0..100u32 {
            base.insert(d);
        }
        let mut pay = RoaringBitmap::new();
        for d in 0..20u32 {
            pay.insert(d);
        } // 20 payload docs, prio_cap = 20/4 = 5; 15 overflow docs remain eligible for general pool if stale!
        let picked = sp.select_stale(1000, 60, 20, &pay, &base);
        assert_eq!(picked.len(), 20);
        // General 15 picks MUST come from docs 50..100 (checked=50), proving high doc_ids do NOT starve!
        assert!(picked[5..].iter().all(|&d| d >= 50));

        // Append torn crash tail to WAL, reopen -> MUST recover cleanly without losing doc 10!
        {
            let mut f = OpenOptions::new().append(true).open(&wal).unwrap();
            f.write_all(&[b'S', b'W', b'A', b'L', 1, 2, 3]).unwrap();
        }
        drop(sp);

        let sp2 = SwarmPlane::open(&dir, &wal, Some(99)).unwrap();
        let r2 = sp2.read(10).unwrap();
        assert_eq!(r2.seeders, Some(0));
        assert_eq!(r2.peak_seeders, 50);
        assert_eq!(r2.checked_epoch_min, 210);

        // Checkpoint truncates WAL cleanly
        sp2.checkpoint().unwrap();
        assert_eq!(std::fs::metadata(&wal).unwrap().len(), 0);

        let _ = std::fs::remove_dir_all(&dir);
    }
}
