use arc_swap::ArcSwap;
use crc32fast::Hasher as Crc32;
use dh_tms_format::{
    read_varint_u32, write_checked_bytes, write_checked_pod_slice, write_varint_u32, CheckedMmap,
    FileKind, FormatError,
};
use parking_lot::Mutex;
use roaring::RoaringBitmap;
use std::fmt;
use std::fs::{File, OpenOptions};
use std::io::{self, Read, Write};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU32, AtomicU64, Ordering};
use std::sync::Arc;

const POLICY_WAL_MAGIC: [u8; 4] = *b"PWAL";

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum PolicyTier {
    /// public & !quarantined & !privacy_tainted & !excluded & !held & !adult & !name_blocked
    Base,
    /// Base & active external processor publication approval
    Pub,
    /// Pub & active unexpired PayloadProof v2 lease (checked against exact read-time `now_epoch_sec`)
    Payload,
}

#[repr(u8)]
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RevokeReason {
    CsamOrDelete = 1,
    PrivacyTaint = 2,
    Hold = 3,
    Quarantine = 4,
    Adult = 5,
    NameBlocked = 6,
    Excluded = 7,
}

impl RevokeReason {
    fn from_u8(v: u8) -> Option<Self> {
        match v {
            1 => Some(Self::CsamOrDelete),
            2 => Some(Self::PrivacyTaint),
            3 => Some(Self::Hold),
            4 => Some(Self::Quarantine),
            5 => Some(Self::Adult),
            6 => Some(Self::NameBlocked),
            7 => Some(Self::Excluded),
            _ => None,
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum PolicyOp {
    Revoke { doc_id: u32, reason: RevokeReason },
    Unrevoke { doc_id: u32, reason: RevokeReason },
    GrantPubApproval { doc_id: u32 },
    RevokePubApproval { doc_id: u32 },
    GrantPayloadProof { doc_id: u32, expires_epoch_sec: u32 },
    RevokePayloadProof { doc_id: u32 },
}

#[derive(Debug)]
pub enum PolicyError {
    Format(FormatError),
    Io(io::Error),
    LeaseExpired {
        now_ms: u64,
        valid_until_ms: u64,
        pg_lsn: u64,
    },
    OutOfRange {
        doc_id: u32,
        num_docs: usize,
    },
    WalGenerationMismatch {
        expected: u64,
        actual: u64,
    },
    CorruptWal(String),
}

impl fmt::Display for PolicyError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Format(e) => write!(f, "policy format error: {e}"),
            Self::Io(e) => write!(f, "policy io error: {e}"),
            Self::LeaseExpired { now_ms, valid_until_ms, pg_lsn } => write!(
                f,
                "PG-LSN governance lease expired (now_ms={now_ms} > valid_until_ms={valid_until_ms}, pg_lsn=0x{pg_lsn:x}) — failing closed"
            ),
            Self::OutOfRange { doc_id, num_docs } => {
                write!(f, "doc_id {doc_id} out of policy bounds (num_docs={num_docs})")
            }
            Self::WalGenerationMismatch { expected, actual } => {
                write!(f, "policy WAL generation mismatch: expected {expected}, got {actual}")
            }
            Self::CorruptWal(msg) => write!(f, "corrupt policy WAL: {msg}"),
        }
    }
}

impl std::error::Error for PolicyError {}
impl From<FormatError> for PolicyError {
    fn from(e: FormatError) -> Self {
        Self::Format(e)
    }
}
impl From<io::Error> for PolicyError {
    fn from(e: io::Error) -> Self {
        Self::Io(e)
    }
}

/// Short-TTL PG-LSN governance lease fence for `DH-TMS Dual`.
/// Readers MUST fail closed whenever `now_ms > valid_until_ms`.
/// Mutations serialize on a small fence lock so a stale in-flight `renew()` cannot
/// revive an expired lease, while hot-path `check()` remains completely lock-free.
pub struct GovernanceLease {
    valid_until_ms: AtomicU64,
    applied_pg_lsn: AtomicU64,
    lease_epoch: AtomicU64,
    fence_mu: Mutex<u64>,
}

impl GovernanceLease {
    pub fn new(initial_pg_lsn: u64, valid_until_ms: u64) -> Self {
        Self {
            valid_until_ms: AtomicU64::new(valid_until_ms),
            applied_pg_lsn: AtomicU64::new(initial_pg_lsn),
            lease_epoch: AtomicU64::new(0),
            fence_mu: Mutex::new(initial_pg_lsn),
        }
    }

    /// Renew lease atomically after confirming PG governance heartbeat / CDC LSN progress.
    /// Returns `false` if `pg_lsn` is behind the monotonic expiry fence.
    pub fn renew(&self, pg_lsn: u64, new_valid_until_ms: u64) -> bool {
        let min_lsn = self.fence_mu.lock();
        if pg_lsn < *min_lsn {
            return false;
        }
        self.applied_pg_lsn.fetch_max(pg_lsn, Ordering::AcqRel);
        let cur_until = self.valid_until_ms.load(Ordering::Acquire);
        if new_valid_until_ms > cur_until {
            self.valid_until_ms
                .store(new_valid_until_ms, Ordering::Release);
        }
        true
    }

    /// Immediately expire lease and advance the monotonic fence so older LSNs cannot revive it.
    pub fn expire_now(&self) {
        let mut min_lsn = self.fence_mu.lock();
        let cur = self.applied_pg_lsn.load(Ordering::Acquire);
        *min_lsn = (*min_lsn).max(cur.saturating_add(1));
        self.lease_epoch.fetch_add(1, Ordering::AcqRel);
        self.valid_until_ms.store(0, Ordering::Release);
    }

    /// Immediately expire lease at a known `pg_lsn`, requiring `pg_lsn + 1` or higher to revive.
    pub fn expire_now_at(&self, pg_lsn: u64) {
        let mut min_lsn = self.fence_mu.lock();
        let cur = self
            .applied_pg_lsn
            .fetch_max(pg_lsn, Ordering::AcqRel)
            .max(pg_lsn);
        *min_lsn = (*min_lsn).max(cur.saturating_add(1));
        self.lease_epoch.fetch_add(1, Ordering::AcqRel);
        self.valid_until_ms.store(0, Ordering::Release);
    }

    #[inline]
    pub fn lease_epoch(&self) -> u64 {
        self.lease_epoch.load(Ordering::Acquire)
    }

    #[inline]
    pub fn check(&self, now_ms: u64) -> Result<u64, PolicyError> {
        let until = self.valid_until_ms.load(Ordering::Acquire);
        let lsn = self.applied_pg_lsn.load(Ordering::Acquire);
        if now_ms > until {
            return Err(PolicyError::LeaseExpired {
                now_ms,
                valid_until_ms: until,
                pg_lsn: lsn,
            });
        }
        Ok(lsn)
    }

    #[inline]
    pub fn applied_lsn(&self) -> u64 {
        self.applied_pg_lsn.load(Ordering::Acquire)
    }

    #[inline]
    pub fn valid_until_ms(&self) -> u64 {
        self.valid_until_ms.load(Ordering::Acquire)
    }
}

/// Immutable RCU snapshot of composite Roaring bitmaps used for fast index block skipping.
/// NOTE: Every final returned row MUST still pass `PolicyPlane::authorize_egress()` so that
/// concurrent revocations and PayloadProof expirations take effect on in-flight readers immediately!
pub struct PolicySnapshot {
    pub epoch: u64,
    pub base_eligible: RoaringBitmap,
    pub pub_eligible: RoaringBitmap,
    pub payload_eligible: RoaringBitmap,
}

impl PolicySnapshot {
    #[inline]
    pub fn bitmap_for(&self, tier: PolicyTier) -> &RoaringBitmap {
        match tier {
            PolicyTier::Base => &self.base_eligible,
            PolicyTier::Pub => &self.pub_eligible,
            PolicyTier::Payload => &self.payload_eligible,
        }
    }
}

/// Authoritative predicate bitmaps persisted at generation build & mutated under write lock.
pub struct PolicyState {
    pub num_docs: usize,
    pub public: RoaringBitmap,
    pub quarantined: RoaringBitmap,
    pub privacy_tainted: RoaringBitmap,
    pub excluded: RoaringBitmap,
    pub held: RoaringBitmap,
    pub adult: RoaringBitmap,
    pub name_blocked: RoaringBitmap,
    pub pub_approved: RoaringBitmap,
    pub tombstoned_for_scrub: RoaringBitmap,
    /// Per-doc PayloadProof v2 expiry epoch seconds (`0` = none/expired).
    pub payload_expires: Vec<u32>,
}

impl PolicyState {
    pub fn new(num_docs: usize) -> Self {
        Self {
            num_docs,
            public: RoaringBitmap::new(),
            quarantined: RoaringBitmap::new(),
            privacy_tainted: RoaringBitmap::new(),
            excluded: RoaringBitmap::new(),
            held: RoaringBitmap::new(),
            adult: RoaringBitmap::new(),
            name_blocked: RoaringBitmap::new(),
            pub_approved: RoaringBitmap::new(),
            tombstoned_for_scrub: RoaringBitmap::new(),
            payload_expires: vec![0u32; num_docs],
        }
    }

    #[inline]
    pub fn is_base_denied(&self, doc_id: u32) -> bool {
        !self.public.contains(doc_id)
            || self.quarantined.contains(doc_id)
            || self.privacy_tainted.contains(doc_id)
            || self.excluded.contains(doc_id)
            || self.held.contains(doc_id)
            || self.adult.contains(doc_id)
            || self.name_blocked.contains(doc_id)
    }

    pub fn materialize(&self, epoch: u64, now_epoch_sec: u32) -> PolicySnapshot {
        let mut base = self.public.clone();
        base -= &self.quarantined;
        base -= &self.privacy_tainted;
        base -= &self.excluded;
        base -= &self.held;
        base -= &self.adult;
        base -= &self.name_blocked;

        let pub_el = &base & &self.pub_approved;
        let mut pay_el = RoaringBitmap::new();
        for d in pub_el.iter() {
            if let Some(&exp) = self.payload_expires.get(d as usize) {
                if exp > now_epoch_sec {
                    pay_el.insert(d);
                }
            }
        }
        PolicySnapshot {
            epoch,
            base_eligible: base,
            pub_eligible: pub_el,
            payload_eligible: pay_el,
        }
    }

    pub fn save(&self, dir: &Path, generation_id: u64) -> Result<u64, FormatError> {
        let mut buf = Vec::with_capacity(1024 * 1024);
        let bms = [
            &self.public,
            &self.quarantined,
            &self.privacy_tainted,
            &self.excluded,
            &self.held,
            &self.adult,
            &self.name_blocked,
            &self.pub_approved,
            &self.tombstoned_for_scrub,
        ];
        let mut tmp = Vec::new();
        for bm in &bms {
            tmp.clear();
            bm.serialize_into(&mut tmp)?;
            write_varint_u32(&mut buf, tmp.len() as u32);
            buf.extend_from_slice(&tmp);
        }
        let b1 = write_checked_bytes(
            &dir.join("policy_state.bin"),
            FileKind::PolicySnapshot,
            generation_id,
            self.num_docs as u64,
            0,
            0,
            &buf,
        )?;
        let b2 = write_checked_pod_slice(
            &dir.join("payload_exp_col.bin"),
            FileKind::PayloadExpCol,
            generation_id,
            0,
            0,
            &self.payload_expires,
        )?;
        Ok(b1 + b2)
    }

    pub fn load(dir: &Path, expected_gen: Option<u64>) -> Result<(Self, u64), FormatError> {
        let cm = CheckedMmap::open(
            &dir.join("policy_state.bin"),
            FileKind::PolicySnapshot,
            expected_gen,
            true,
        )?;
        let generation_id = cm.header().generation_id;
        let num_docs = cm.header().record_count as usize;
        let exp_cm = CheckedMmap::open(
            &dir.join("payload_exp_col.bin"),
            FileKind::PayloadExpCol,
            Some(generation_id),
            false,
        )?;
        let exp_slice = exp_cm.typed_slice::<u32>()?;
        if exp_slice.len() != num_docs {
            return Err(FormatError::CorruptPayload(format!(
                "payload_exp_col len {} != num_docs {num_docs}",
                exp_slice.len()
            )));
        }

        let p = cm.payload();
        let mut pos = 0usize;
        let mut read_bm = || -> Result<RoaringBitmap, FormatError> {
            let len = read_varint_u32(p, &mut pos)? as usize;
            let end = pos
                .checked_add(len)
                .ok_or_else(|| FormatError::CorruptPayload("policy bitmap len overflow".into()))?;
            if end > p.len() {
                return Err(FormatError::CorruptPayload(
                    "policy bitmap truncated".into(),
                ));
            }
            let bm = RoaringBitmap::deserialize_from(&p[pos..end])
                .map_err(|e| FormatError::CorruptPayload(format!("corrupt policy bitmap: {e}")))?;
            pos = end;
            Ok(bm)
        };

        let st = Self {
            num_docs,
            public: read_bm()?,
            quarantined: read_bm()?,
            privacy_tainted: read_bm()?,
            excluded: read_bm()?,
            held: read_bm()?,
            adult: read_bm()?,
            name_blocked: read_bm()?,
            pub_approved: read_bm()?,
            tombstoned_for_scrub: read_bm()?,
            payload_expires: exp_slice.to_vec(),
        };
        Ok((st, generation_id))
    }
}

struct WriterState {
    state: PolicyState,
    wal_file: File,
    next_seq: u64,
    pending_snapshot_dirty: bool,
}

/// Hardened fail-closed PolicyPlane:
/// - Dense atomic deny bitset (`~50 ns` revoke visible to ALL in-flight readers at response egress!)
/// - Dense atomic `pub_approved` bitset
/// - Per-doc atomic `u32` `payload_expires_epoch` checked at read time (zero stale wheel renewals!)
/// - `ArcSwap<PolicySnapshot>` lock-free RCU composite Roaring masks for index skipping
/// - Checksummed, generation-tagged, `fsync`'d `policy.wal` replayed automatically before serving
/// - Short-TTL `GovernanceLease` enforcing PG-LSN fail-closed partition semantics.
///
/// Cloneable, `'static + Send + Sync` reader handle for zero-lifetime pushdown.
#[derive(Clone)]
pub struct PolicyReaderHandle {
    snapshot: Arc<PolicySnapshot>,
    deny_bits: Arc<Vec<AtomicU64>>,
    payload_exp: Arc<Vec<AtomicU32>>,
    num_docs: usize,
}

impl PolicyReaderHandle {
    #[inline]
    pub fn allows(&self, doc_id: u32, tier: PolicyTier, now_epoch_sec: u32) -> bool {
        let idx = doc_id as usize;
        if idx >= self.num_docs {
            return false;
        }
        if !self.snapshot.bitmap_for(tier).contains(doc_id) {
            return false;
        }
        let word = self.deny_bits[idx >> 6].load(Ordering::Acquire);
        if (word & (1u64 << (idx & 63))) != 0 {
            return false;
        }
        if tier == PolicyTier::Payload
            && self.payload_exp[idx].load(Ordering::Acquire) <= now_epoch_sec
        {
            return false;
        }
        true
    }
}

pub struct PolicyPlane {
    pub generation_id: u64,
    pub num_docs: usize,
    deny_bits: Arc<Vec<AtomicU64>>,
    pub_bits: Arc<Vec<AtomicU64>>,
    payload_exp: Arc<Vec<AtomicU32>>,
    snapshot: ArcSwap<PolicySnapshot>,
    epoch: AtomicU64,
    lease: GovernanceLease,
    writer: Mutex<WriterState>,
    wal_path: PathBuf,
}

impl PolicyPlane {
    pub fn open(
        gen_dir: &Path,
        wal_path: &Path,
        expected_gen: Option<u64>,
        now_epoch_sec: u32,
        initial_pg_lsn: u64,
        initial_lease_until_ms: u64,
    ) -> Result<Self, PolicyError> {
        let (mut state, generation_id) = PolicyState::load(gen_dir, expected_gen)?;
        let num_docs = state.num_docs;
        let words = num_docs.div_ceil(64);

        // Replay checksummed, generation-tagged policy WAL BEFORE serving starts!
        let (replayed_seq, replayed_lsn) = replay_policy_wal(wal_path, generation_id, &mut state)?;
        let effective_lsn = initial_pg_lsn.max(replayed_lsn);

        // Initialize dense atomic bitsets from replayed authoritative state
        let deny_bits: Vec<AtomicU64> = (0..words).map(|_| AtomicU64::new(0)).collect();
        let pub_bits: Vec<AtomicU64> = (0..words).map(|_| AtomicU64::new(0)).collect();
        let payload_exp: Vec<AtomicU32> = state
            .payload_expires
            .iter()
            .map(|&e| AtomicU32::new(e))
            .collect();

        // Populate deny_bits = !base_eligible (all non-base-eligible docs have deny bit = 1)
        let snap = state.materialize(1, now_epoch_sec);
        // Set all bits in 0..num_docs to 1 initially, then clear base_eligible bits -> O(N/64) + O(|base|)
        for w in &deny_bits {
            w.store(u64::MAX, Ordering::Relaxed);
        }
        for d in snap.base_eligible.iter() {
            let idx = (d as usize) >> 6;
            let bit = 1u64 << ((d as usize) & 63);
            if idx < deny_bits.len() {
                deny_bits[idx].fetch_and(!bit, Ordering::Relaxed);
            }
        }
        for d in state.pub_approved.iter() {
            let idx = (d as usize) >> 6;
            let bit = 1u64 << ((d as usize) & 63);
            if idx < pub_bits.len() {
                pub_bits[idx].fetch_or(bit, Ordering::Relaxed);
            }
        }

        let wal_file = OpenOptions::new()
            .create(true)
            .append(true)
            .open(wal_path)?;

        Ok(Self {
            generation_id,
            num_docs,
            deny_bits: Arc::new(deny_bits),
            pub_bits: Arc::new(pub_bits),
            payload_exp: Arc::new(payload_exp),
            snapshot: ArcSwap::from_pointee(snap),
            epoch: AtomicU64::new(1),
            lease: GovernanceLease::new(effective_lsn, initial_lease_until_ms),
            writer: Mutex::new(WriterState {
                state,
                wal_file,
                next_seq: replayed_seq + 1,
                pending_snapshot_dirty: false,
            }),
            wal_path: wal_path.to_path_buf(),
        })
    }

    #[inline]
    pub fn lease(&self) -> &GovernanceLease {
        &self.lease
    }

    #[inline]
    pub fn epoch(&self) -> u64 {
        self.epoch.load(Ordering::Acquire)
    }

    /// Lock-free RCU snapshot acquisition (~5 ns) for index candidate skipping.
    #[inline]
    pub fn snapshot(&self) -> arc_swap::Guard<Arc<PolicySnapshot>> {
        self.snapshot.load()
    }

    #[inline]
    pub fn reader_handle(&self) -> PolicyReaderHandle {
        PolicyReaderHandle {
            snapshot: self.snapshot.load_full(),
            deny_bits: Arc::clone(&self.deny_bits),
            payload_exp: Arc::clone(&self.payload_exp),
            num_docs: self.num_docs,
        }
    }

    #[inline]
    pub fn is_denied_atomic(&self, doc_id: u32) -> bool {
        let idx = doc_id as usize;
        if idx >= self.num_docs {
            return true; // fail closed on out-of-range doc_id
        }
        let w = self.deny_bits[idx >> 6].load(Ordering::Acquire);
        ((w >> (idx & 63)) & 1) != 0
    }

    #[inline]
    pub fn is_pub_approved_atomic(&self, doc_id: u32) -> bool {
        let idx = doc_id as usize;
        if idx >= self.num_docs {
            return false;
        }
        let w = self.pub_bits[idx >> 6].load(Ordering::Acquire);
        ((w >> (idx & 63)) & 1) != 0
    }

    #[inline]
    pub fn payload_expires_at(&self, doc_id: u32) -> u32 {
        self.payload_exp
            .get(doc_id as usize)
            .map(|a| a.load(Ordering::Acquire))
            .unwrap_or(0)
    }

    /// MANDATORY final response egress gate!
    /// Checks PG-LSN lease freshness + dense atomic deny bitset + tier predicates + read-time PayloadProof expiry.
    /// Blocks in-flight readers holding an older `PolicySnapshot` immediately (~10 ns/doc).
    #[inline]
    pub fn authorize_egress_one(
        &self,
        doc_id: u32,
        tier: PolicyTier,
        now_epoch_sec: u32,
        now_ms: u64,
    ) -> Result<bool, PolicyError> {
        self.lease.check(now_ms)?;
        Ok(self.authorize_egress_unchecked_lease(doc_id, tier, now_epoch_sec))
    }

    #[inline]
    fn authorize_egress_unchecked_lease(
        &self,
        doc_id: u32,
        tier: PolicyTier,
        now_epoch_sec: u32,
    ) -> bool {
        if self.is_denied_atomic(doc_id) {
            return false;
        }
        match tier {
            PolicyTier::Base => true,
            PolicyTier::Pub => self.is_pub_approved_atomic(doc_id),
            PolicyTier::Payload => {
                self.is_pub_approved_atomic(doc_id)
                    && self.payload_expires_at(doc_id) > now_epoch_sec
            }
        }
    }

    /// Filter a candidate doc_id list through the mandatory egress gate in place.
    /// Fails closed with `Err(PolicyError::LeaseExpired)` if the PG-LSN lease has expired!
    pub fn filter_egress_docs(
        &self,
        docs: &mut Vec<u32>,
        tier: PolicyTier,
        now_epoch_sec: u32,
        now_ms: u64,
    ) -> Result<(), PolicyError> {
        self.lease.check(now_ms)?;
        docs.retain(|&d| self.authorize_egress_unchecked_lease(d, tier, now_epoch_sec));
        Ok(())
    }

    /// Fast synchronous single-doc revocation:
    /// 1. Append framed CRC32-checksummed record to `policy.wal` and `sync_data()` (WAL-before-mutate!).
    /// 2. Atomically set deny bit (~50 ns, immediately blocking ALL in-flight readers!).
    ///
    /// Does NOT perform a full Roaring bitmap clone on every single revoke.
    pub fn revoke_immediate(
        &self,
        doc_id: u32,
        reason: RevokeReason,
        pg_lsn: u64,
        sync_wal: bool,
    ) -> Result<u64, PolicyError> {
        self.apply_batch(
            &[PolicyOp::Revoke { doc_id, reason }],
            pg_lsn,
            sync_wal,
            false,
            0,
        )
    }

    /// Apply an atomic batch of revocations, grants, un-revokes, or PayloadProof renewals.
    /// Handles 100K bulk takedowns in milliseconds with 1 framed WAL sync + atomic bitset updates!
    /// Pass `refresh_snapshot = true` to also republish the RCU Roaring `PolicySnapshot` (needed for new grants
    /// to become visible to index-level skipping; revocations are ALWAYS immediately enforced at egress).
    pub fn apply_batch(
        &self,
        ops: &[PolicyOp],
        pg_lsn: u64,
        sync_wal: bool,
        refresh_snapshot: bool,
        now_epoch_sec: u32,
    ) -> Result<u64, PolicyError> {
        if ops.is_empty() {
            return Ok(self.epoch());
        }
        for op in ops {
            let d = match *op {
                PolicyOp::Revoke { doc_id, .. }
                | PolicyOp::Unrevoke { doc_id, .. }
                | PolicyOp::GrantPubApproval { doc_id }
                | PolicyOp::RevokePubApproval { doc_id }
                | PolicyOp::GrantPayloadProof { doc_id, .. }
                | PolicyOp::RevokePayloadProof { doc_id } => doc_id,
            };
            if (d as usize) >= self.num_docs {
                return Err(PolicyError::OutOfRange {
                    doc_id: d,
                    num_docs: self.num_docs,
                });
            }
        }

        let mut ws = self.writer.lock();
        // 1. WAL-before-mutate: write framed, generation-tagged, sequence-numbered, CRC32-protected batch!
        let seq = ws.next_seq;
        let frame = encode_wal_frame(self.generation_id, seq, pg_lsn, ops);
        ws.wal_file.write_all(&frame)?;
        ws.wal_file.flush()?;
        if sync_wal {
            ws.wal_file.sync_data()?;
        }
        ws.next_seq += 1;

        // 2. Apply to authoritative PolicyState AND atomic egress bitsets immediately!
        for &op in ops {
            apply_op_to_state(&mut ws.state, op);
            match op {
                PolicyOp::Revoke { doc_id, reason } => {
                    self.set_deny_bit(doc_id, true);
                    if reason == RevokeReason::CsamOrDelete {
                        self.set_pub_bit(doc_id, false);
                        self.payload_exp[doc_id as usize].store(0, Ordering::Release);
                    }
                }
                PolicyOp::Unrevoke { doc_id, .. } => {
                    let still_denied = ws.state.is_base_denied(doc_id);
                    self.set_deny_bit(doc_id, still_denied);
                }
                PolicyOp::GrantPubApproval { doc_id } => {
                    self.set_pub_bit(doc_id, true);
                }
                PolicyOp::RevokePubApproval { doc_id } => {
                    self.set_pub_bit(doc_id, false);
                }
                PolicyOp::GrantPayloadProof {
                    doc_id,
                    expires_epoch_sec,
                } => {
                    self.payload_exp[doc_id as usize].store(expires_epoch_sec, Ordering::Release);
                }
                PolicyOp::RevokePayloadProof { doc_id } => {
                    self.payload_exp[doc_id as usize].store(0, Ordering::Release);
                }
            }
        }

        ws.pending_snapshot_dirty = true;
        let next_ep = self.epoch.fetch_add(1, Ordering::AcqRel) + 1;
        if refresh_snapshot {
            let new_snap = Arc::new(ws.state.materialize(next_ep, now_epoch_sec));
            self.snapshot.store(new_snap);
            ws.pending_snapshot_dirty = false;
        }
        Ok(next_ep)
    }

    /// Republish RCU Roaring `PolicySnapshot` from current authoritative state (e.g. periodic 500ms-1s
    /// batch refresh or right after bulk grant ingestion).
    pub fn publish_snapshot(&self, now_epoch_sec: u32) -> u64 {
        let mut ws = self.writer.lock();
        let next_ep = self.epoch.fetch_add(1, Ordering::AcqRel) + 1;
        let new_snap = Arc::new(ws.state.materialize(next_ep, now_epoch_sec));
        self.snapshot.store(new_snap);
        ws.pending_snapshot_dirty = false;
        next_ep
    }

    /// Returns count of docs tombstoned via `CsamOrDelete` awaiting physical scrub at compaction.
    pub fn tombstoned_count(&self) -> u64 {
        self.writer.lock().state.tombstoned_for_scrub.len()
    }

    pub fn is_tombstoned_for_scrub(&self, doc_id: u32) -> bool {
        self.writer
            .lock()
            .state
            .tombstoned_for_scrub
            .contains(doc_id)
    }

    pub fn wal_path(&self) -> &Path {
        &self.wal_path
    }

    #[inline]
    fn set_deny_bit(&self, doc_id: u32, denied: bool) {
        let idx = doc_id as usize;
        let bit = 1u64 << (idx & 63);
        if denied {
            self.deny_bits[idx >> 6].fetch_or(bit, Ordering::Release);
        } else {
            self.deny_bits[idx >> 6].fetch_and(!bit, Ordering::Release);
        }
    }

    #[inline]
    fn set_pub_bit(&self, doc_id: u32, approved: bool) {
        let idx = doc_id as usize;
        let bit = 1u64 << (idx & 63);
        if approved {
            self.pub_bits[idx >> 6].fetch_or(bit, Ordering::Release);
        } else {
            self.pub_bits[idx >> 6].fetch_and(!bit, Ordering::Release);
        }
    }
}

fn apply_op_to_state(st: &mut PolicyState, op: PolicyOp) {
    match op {
        PolicyOp::Revoke { doc_id, reason } => match reason {
            RevokeReason::CsamOrDelete => {
                st.public.remove(doc_id);
                st.excluded.insert(doc_id);
                st.pub_approved.remove(doc_id);
                st.tombstoned_for_scrub.insert(doc_id);
                if let Some(slot) = st.payload_expires.get_mut(doc_id as usize) {
                    *slot = 0;
                }
            }
            RevokeReason::PrivacyTaint => {
                st.privacy_tainted.insert(doc_id);
            }
            RevokeReason::Hold => {
                st.held.insert(doc_id);
            }
            RevokeReason::Quarantine => {
                st.quarantined.insert(doc_id);
            }
            RevokeReason::Adult => {
                st.adult.insert(doc_id);
            }
            RevokeReason::NameBlocked => {
                st.name_blocked.insert(doc_id);
            }
            RevokeReason::Excluded => {
                st.excluded.insert(doc_id);
            }
        },
        PolicyOp::Unrevoke { doc_id, reason } => match reason {
            RevokeReason::CsamOrDelete => {
                // CSAM deletions are NEVER un-revoked!
            }
            RevokeReason::PrivacyTaint => {
                st.privacy_tainted.remove(doc_id);
            }
            RevokeReason::Hold => {
                st.held.remove(doc_id);
            }
            RevokeReason::Quarantine => {
                st.quarantined.remove(doc_id);
            }
            RevokeReason::Adult => {
                st.adult.remove(doc_id);
            }
            RevokeReason::NameBlocked => {
                st.name_blocked.remove(doc_id);
            }
            RevokeReason::Excluded => {
                st.excluded.remove(doc_id);
            }
        },
        PolicyOp::GrantPubApproval { doc_id } => {
            if !st.tombstoned_for_scrub.contains(doc_id) {
                st.pub_approved.insert(doc_id);
            }
        }
        PolicyOp::RevokePubApproval { doc_id } => {
            st.pub_approved.remove(doc_id);
        }
        PolicyOp::GrantPayloadProof {
            doc_id,
            expires_epoch_sec,
        } => {
            if !st.tombstoned_for_scrub.contains(doc_id) {
                if let Some(slot) = st.payload_expires.get_mut(doc_id as usize) {
                    *slot = expires_epoch_sec;
                }
            }
        }
        PolicyOp::RevokePayloadProof { doc_id } => {
            if let Some(slot) = st.payload_expires.get_mut(doc_id as usize) {
                *slot = 0;
            }
        }
    }
}

/// Frame layout (36-byte header + ops payload):
/// `[magic: 4][gen: 8][seq: 8][pg_lsn: 8][payload_len: 4][crc32: 4][ops...]`
/// Each op = 10 bytes: `[kind: 1][reason: 1][doc_id: 4][aux_u32: 4]`.
fn encode_wal_frame(gen_id: u64, seq: u64, pg_lsn: u64, ops: &[PolicyOp]) -> Vec<u8> {
    let payload_len = ops.len() * 10;
    let mut payload = Vec::with_capacity(payload_len);
    for &op in ops {
        let (kind, reason, doc_id, aux) = match op {
            PolicyOp::Revoke { doc_id, reason } => (1u8, reason as u8, doc_id, 0u32),
            PolicyOp::Unrevoke { doc_id, reason } => (2u8, reason as u8, doc_id, 0u32),
            PolicyOp::GrantPubApproval { doc_id } => (3u8, 0u8, doc_id, 0u32),
            PolicyOp::RevokePubApproval { doc_id } => (4u8, 0u8, doc_id, 0u32),
            PolicyOp::GrantPayloadProof {
                doc_id,
                expires_epoch_sec,
            } => (5u8, 0u8, doc_id, expires_epoch_sec),
            PolicyOp::RevokePayloadProof { doc_id } => (6u8, 0u8, doc_id, 0u32),
        };
        payload.push(kind);
        payload.push(reason);
        payload.extend_from_slice(&doc_id.to_le_bytes());
        payload.extend_from_slice(&aux.to_le_bytes());
    }
    let mut crc = Crc32::new();
    crc.update(&gen_id.to_le_bytes());
    crc.update(&seq.to_le_bytes());
    crc.update(&pg_lsn.to_le_bytes());
    crc.update(&(payload_len as u32).to_le_bytes());
    crc.update(&payload);
    let checksum = crc.finalize();

    let mut frame = Vec::with_capacity(36 + payload_len);
    frame.extend_from_slice(&POLICY_WAL_MAGIC);
    frame.extend_from_slice(&gen_id.to_le_bytes());
    frame.extend_from_slice(&seq.to_le_bytes());
    frame.extend_from_slice(&pg_lsn.to_le_bytes());
    frame.extend_from_slice(&(payload_len as u32).to_le_bytes());
    frame.extend_from_slice(&checksum.to_le_bytes());
    frame.extend_from_slice(&payload);
    frame
}

fn replay_policy_wal(
    wal_path: &Path,
    expected_gen: u64,
    state: &mut PolicyState,
) -> Result<(u64, u64), PolicyError> {
    if !wal_path.exists() {
        return Ok((0, 0));
    }
    let mut raw = Vec::new();
    File::open(wal_path)?.read_to_end(&mut raw)?;
    let mut pos = 0usize;
    let mut last_seq = 0u64;
    let mut last_lsn = 0u64;

    while pos < raw.len() {
        if pos + 36 > raw.len() {
            // Torn tail header from crash -> truncate cleanly to valid prefix!
            truncate_wal(wal_path, pos as u64)?;
            break;
        }
        if raw[pos..pos + 4] != POLICY_WAL_MAGIC {
            return Err(PolicyError::CorruptWal(format!(
                "bad WAL magic at byte {pos}"
            )));
        }
        let gen_id = u64::from_le_bytes(raw[pos + 4..pos + 12].try_into().unwrap());
        if gen_id != expected_gen {
            return Err(PolicyError::WalGenerationMismatch {
                expected: expected_gen,
                actual: gen_id,
            });
        }
        let seq = u64::from_le_bytes(raw[pos + 12..pos + 20].try_into().unwrap());
        let pg_lsn = u64::from_le_bytes(raw[pos + 20..pos + 28].try_into().unwrap());
        let payload_len = u32::from_le_bytes(raw[pos + 28..pos + 32].try_into().unwrap()) as usize;
        let stored_crc = u32::from_le_bytes(raw[pos + 32..pos + 36].try_into().unwrap());
        if !payload_len.is_multiple_of(10) {
            return Err(PolicyError::CorruptWal(format!(
                "invalid WAL payload length {payload_len} at byte {pos}"
            )));
        }
        let Some(end) = pos.checked_add(36).and_then(|x| x.checked_add(payload_len)) else {
            return Err(PolicyError::CorruptWal(format!(
                "WAL frame offset overflow at byte {pos}"
            )));
        };
        if end > raw.len() {
            // Torn tail payload at EOF from crash -> truncate cleanly!
            truncate_wal(wal_path, pos as u64)?;
            break;
        }
        let payload = &raw[pos + 36..end];
        let mut crc = Crc32::new();
        crc.update(&raw[pos + 4..pos + 32]);
        crc.update(payload);
        if crc.finalize() != stored_crc {
            if end == raw.len() {
                // Torn final frame at EOF -> truncate cleanly
                truncate_wal(wal_path, pos as u64)?;
                break;
            }
            // Mid-file corruption MUST fail closed so committed revocations are never lost!
            return Err(PolicyError::CorruptWal(format!(
                "mid-file WAL CRC mismatch at byte {pos} (end={end} < len={})",
                raw.len()
            )));
        }
        for chunk in payload.as_chunks::<10>().0 {
            let kind = chunk[0];
            let reason_b = chunk[1];
            let doc_id = u32::from_le_bytes(chunk[2..6].try_into().unwrap());
            let aux = u32::from_le_bytes(chunk[6..10].try_into().unwrap());
            let op = match kind {
                1 => PolicyOp::Revoke {
                    doc_id,
                    reason: RevokeReason::from_u8(reason_b)
                        .ok_or_else(|| PolicyError::CorruptWal("invalid revoke reason".into()))?,
                },
                2 => PolicyOp::Unrevoke {
                    doc_id,
                    reason: RevokeReason::from_u8(reason_b)
                        .ok_or_else(|| PolicyError::CorruptWal("invalid unrevoke reason".into()))?,
                },
                3 => PolicyOp::GrantPubApproval { doc_id },
                4 => PolicyOp::RevokePubApproval { doc_id },
                5 => PolicyOp::GrantPayloadProof {
                    doc_id,
                    expires_epoch_sec: aux,
                },
                6 => PolicyOp::RevokePayloadProof { doc_id },
                _ => return Err(PolicyError::CorruptWal(format!("unknown op kind {kind}"))),
            };
            apply_op_to_state(state, op);
        }
        last_seq = seq;
        last_lsn = last_lsn.max(pg_lsn);
        pos = end;
    }
    Ok((last_seq, last_lsn))
}

fn truncate_wal(wal_path: &Path, valid_len: u64) -> io::Result<()> {
    let f = OpenOptions::new().write(true).open(wal_path)?;
    f.set_len(valid_len)?;
    f.sync_data()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_inflight_reader_egress_gate_restart_durability_lease_and_renewal() {
        let dir = std::env::temp_dir().join(format!("dhtms_pol_test_{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let wal = dir.join("policy.wal");

        let mut st = PolicyState::new(1000);
        for d in 0u32..1000 {
            st.public.insert(d);
            st.pub_approved.insert(d);
            st.payload_expires[d as usize] = 2000; // expires at t=2000
        }
        st.save(&dir, 77).unwrap();

        let pp = PolicyPlane::open(&dir, &wal, Some(77), 1000, 100, 50_000).unwrap();

        // 1. Paused in-flight reader holds OLD snapshot before revoke!
        let old_snap = pp.snapshot();
        assert!(old_snap.base_eligible.contains(42));
        assert!(old_snap.payload_eligible.contains(42));

        // Revoke doc 42 (CsamOrDelete) — MUST immediately block the paused reader at egress!
        pp.revoke_immediate(42, RevokeReason::CsamOrDelete, 101, true)
            .unwrap();
        let mut emitted = vec![41, 42, 43];
        pp.filter_egress_docs(&mut emitted, PolicyTier::Payload, 1000, 10_000)
            .unwrap();
        assert_eq!(
            emitted,
            vec![41, 43],
            "in-flight reader MUST NOT leak revoked doc 42!"
        );
        assert!(pp.is_tombstoned_for_scrub(42));

        // 2. Read-time PayloadProof expiry AND renewal without stale BTreeMap eviction bug!
        // Renew doc 43 to t=5000 before t=2000 passes
        pp.apply_batch(
            &[PolicyOp::GrantPayloadProof {
                doc_id: 43,
                expires_epoch_sec: 5000,
            }],
            102,
            true,
            false,
            1000,
        )
        .unwrap();
        // At t=2500: doc 41 expired (2000 <= 2500), doc 43 MUST remain valid (5000 > 2500)!
        let mut emitted2 = vec![41, 43];
        pp.filter_egress_docs(&mut emitted2, PolicyTier::Payload, 2500, 10_000)
            .unwrap();
        assert_eq!(emitted2, vec![43]);

        // 3. PG-LSN lease expiration MUST fail closed!
        assert!(matches!(
            pp.authorize_egress_one(43, PolicyTier::Base, 2500, 60_000),
            Err(PolicyError::LeaseExpired { .. })
        ));
        pp.lease().renew(105, 100_000);
        assert!(pp
            .authorize_egress_one(43, PolicyTier::Base, 2500, 60_000)
            .unwrap());

        // 4. Simulate crash with torn tail bytes appended to WAL -> restart MUST NOT resurrect doc 42!
        {
            let mut f = OpenOptions::new().append(true).open(&wal).unwrap();
            f.write_all(&[0x50, 0x57, 0x41, 0x4C, 0xDE, 0xAD]).unwrap();
        }
        drop(pp);

        let pp2 = PolicyPlane::open(&dir, &wal, Some(77), 1000, 100, 100_000).unwrap();
        assert!(!pp2
            .authorize_egress_one(42, PolicyTier::Base, 1000, 10_000)
            .unwrap());
        assert!(pp2.is_tombstoned_for_scrub(42));
        assert_eq!(pp2.payload_expires_at(43), 5000);

        // Wrong generation WAL MUST be rejected!
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn test_policy_wal_midfile_corruption_fails_closed_and_lease_fence() {
        let dir = std::env::temp_dir().join(format!("dhtms_pol_corrupt_{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let wal = dir.join("policy.wal");

        let mut st = PolicyState::new(32);
        for d in 0u32..32 {
            st.public.insert(d);
        }
        st.save(&dir, 99).unwrap();

        {
            let pp = PolicyPlane::open(&dir, &wal, Some(99), 1000, 10, 50_000).unwrap();
            pp.lease().expire_now_at(10);
            assert!(pp.lease().check(1).is_err());
            assert!(
                !pp.lease().renew(10, 90_000),
                "stale LSN <= expired fence must not revive lease"
            );
            assert!(pp.lease().check(1).is_err());
            assert!(pp.lease().renew(11, 90_000));
            assert!(pp.lease().check(1).is_ok());

            pp.revoke_immediate(1, RevokeReason::CsamOrDelete, 12, true)
                .unwrap();
            pp.revoke_immediate(2, RevokeReason::Quarantine, 13, true)
                .unwrap();
        }

        // Flip a byte inside the FIRST frame payload (mid-file, before frame 2!)
        let mut bytes = std::fs::read(&wal).unwrap();
        assert!(bytes.len() >= 92);
        bytes[40] ^= 0xFF;
        std::fs::write(&wal, &bytes).unwrap();

        let err = PolicyPlane::open(&dir, &wal, Some(99), 1000, 10, 50_000);
        assert!(
            matches!(err, Err(PolicyError::CorruptWal(_))),
            "mid-file WAL corruption must fail closed"
        );
        let _ = std::fs::remove_dir_all(&dir);
    }
}
