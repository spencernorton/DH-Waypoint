use crate::builder::{build_empty_generation, build_generation_from_ndjson, BuildFileInput};
use arc_swap::ArcSwap;
use dh_tms_core::CorePlane;
use dh_tms_format::catalog::MmapContentCatalog;
use dh_tms_format::{read_current_manifest, FormatError};
use dh_tms_grammar::{
    parse_release_name, tokenize_alnum, EpisodeSpec, CT_UNKNOWN, NUM_CONTENT_TYPES,
};
use dh_tms_hashdir::{encode_hex, parse_info_hash_hex, HashDir, InfoHash};
use dh_tms_policy::{PolicyError, PolicyOp, PolicyPlane, PolicyTier, RevokeReason};
use dh_tms_search::l0::L0Doc;
use dh_tms_search::{
    term_hash, CustomSearchPlane, QueryMode, SearchError, SearchFilterClause, MAX_VIDEO_CODEC,
    MAX_VIDEO_SOURCE,
};
use std::collections::HashMap;
use dh_tms_swarm::{SwarmError, SwarmPlane, SwarmUpdate};
use parking_lot::Mutex;
use serde::{Deserialize, Serialize};
use std::io::BufRead;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::{SystemTime, UNIX_EPOCH};

#[derive(Debug)]
pub enum DaemonError {
    BadRequest(String),
    NotFound,
    TrigramsDisabled,
    TooManyCandidates,
    LeaseExpired {
        now_ms: u64,
        valid_until_ms: u64,
        pg_lsn: u64,
    },
    Internal(String),
}

impl From<FormatError> for DaemonError {
    fn from(e: FormatError) -> Self {
        Self::Internal(e.to_string())
    }
}

impl From<PolicyError> for DaemonError {
    fn from(e: PolicyError) -> Self {
        match e {
            PolicyError::LeaseExpired {
                now_ms,
                valid_until_ms,
                pg_lsn,
            } => Self::LeaseExpired {
                now_ms,
                valid_until_ms,
                pg_lsn,
            },
            other => Self::Internal(other.to_string()),
        }
    }
}

impl From<SearchError> for DaemonError {
    fn from(e: SearchError) -> Self {
        match e {
            SearchError::Policy(pe) => pe.into(),
            SearchError::TrigramsDisabled => Self::TrigramsDisabled,
            SearchError::TooManyCandidates => Self::TooManyCandidates,
            other => Self::Internal(other.to_string()),
        }
    }
}

impl From<SwarmError> for DaemonError {
    fn from(e: SwarmError) -> Self {
        match e {
            SwarmError::OutOfRange { doc_id, num_docs } => Self::BadRequest(format!(
                "doc_id {doc_id} out of range (num_docs={num_docs})"
            )),
            other => Self::Internal(other.to_string()),
        }
    }
}

pub fn now_epoch_ms() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_millis() as u64)
        .unwrap_or(0)
}

pub fn now_epoch_sec() -> u32 {
    (now_epoch_ms() / 1000) as u32
}

pub fn parse_tier(s: &str) -> Result<PolicyTier, DaemonError> {
    match s.to_ascii_lowercase().as_str() {
        "base" | "" => Ok(PolicyTier::Base),
        "pub" | "publication" => Ok(PolicyTier::Pub),
        "payload" => Ok(PolicyTier::Payload),
        other => Err(DaemonError::BadRequest(format!(
            "unknown policy tier: {other}"
        ))),
    }
}

pub fn parse_mode(s: &str) -> Result<QueryMode, DaemonError> {
    match s.to_ascii_lowercase().as_str() {
        "and" | "" => Ok(QueryMode::And),
        "or" => Ok(QueryMode::Or),
        "phrase" => Ok(QueryMode::Phrase),
        other => Err(DaemonError::BadRequest(format!(
            "unknown query mode: {other}"
        ))),
    }
}

pub fn parse_revoke_reason(s: &str) -> Result<RevokeReason, DaemonError> {
    match s.to_ascii_lowercase().as_str() {
        "quarantine" | "quarantined" => Ok(RevokeReason::Quarantine),
        "privacy" | "privacy_tainted" => Ok(RevokeReason::PrivacyTaint),
        "excluded" | "exclude" => Ok(RevokeReason::Excluded),
        "hold" | "held" => Ok(RevokeReason::Hold),
        "adult" => Ok(RevokeReason::Adult),
        "name_blocked" | "name" => Ok(RevokeReason::NameBlocked),
        "csam_delete" | "csam" | "delete" => Ok(RevokeReason::CsamOrDelete),
        other => Err(DaemonError::BadRequest(format!(
            "unknown revoke reason: {other}"
        ))),
    }
}

#[derive(Debug, Serialize)]
pub struct HealthResponse {
    pub status: String,
    pub engine: String,
    pub generation_id: u64,
    pub num_docs: usize,
    pub pg_lsn: u64,
    pub lease_active: bool,
    pub lease_remaining_ms: u64,
    pub l0_count: usize,
    pub l0_wal_bytes: u64,
    pub base_eligible: u64,
    pub pub_eligible: u64,
    pub payload_eligible: u64,
}

#[derive(Debug, Serialize)]
pub struct FileEntryResp {
    pub path: String,
    pub size: u64,
    pub extension: String,
}

#[derive(Debug, Serialize)]
pub struct DocHit {
    pub doc_id: u32,
    pub score: f32,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub info_hash_v1: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub info_hash_v2: Option<String>,
    pub magnet_xt: String,
    pub name: String,
    pub size_bytes: u64,
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
    pub files_count: u32,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub seeders: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub leechers: Option<u32>,
    pub peak_seeders: u16,
    pub checked_epoch_min: u32,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub files: Option<Vec<FileEntryResp>>,
}

#[derive(Debug, Clone, Default, Deserialize)]
pub struct FilterClause {
    #[serde(default)]
    pub content_type: Option<u8>,
    #[serde(default)]
    pub resolutions: Vec<u8>,
    #[serde(default)]
    pub exclude_resolutions: Vec<u8>,
    #[serde(default)]
    pub require_anime: bool,
    #[serde(default)]
    pub require_3d: bool,
    #[serde(default)]
    pub video_source: Option<u8>,
    #[serde(default)]
    pub video_codec: Option<u8>,
}

impl FilterClause {
    fn to_search_clause(&self) -> SearchFilterClause {
        SearchFilterClause {
            content_type: self.content_type,
            resolutions: self.resolutions.clone(),
            exclude_resolutions: self.exclude_resolutions.clone(),
            require_anime: self.require_anime,
            require_3d: self.require_3d,
            video_source: self.video_source,
            video_codec: self.video_codec,
        }
    }
}

#[derive(Debug, Deserialize)]
pub struct SearchRequest {
    pub query: String,
    #[serde(default)]
    pub mode: String,
    #[serde(default)]
    pub tier: String,
    #[serde(default = "default_limit")]
    pub limit: usize,
    #[serde(default)]
    pub tv_season: Option<u16>,
    #[serde(default)]
    pub tv_episode: Option<u16>,
    #[serde(default)]
    pub include_facets: bool,
    #[serde(default)]
    pub include_files: bool,
    #[serde(default)]
    pub any_clauses: Vec<FilterClause>,
    #[serde(default)]
    pub min_seeders: Option<u32>,
    #[serde(default)]
    pub require_v1: bool,
    #[serde(default)]
    pub now_epoch: Option<u32>,
}

fn default_limit() -> usize {
    100
}

#[derive(Debug, Serialize)]
pub struct SearchResponse {
    pub docs: Vec<DocHit>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub total_facets: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub ct_facets: Option<[u64; NUM_CONTENT_TYPES]>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub res_facets: Option<[u64; 10]>,
}

#[derive(Debug, Deserialize)]
pub struct LookupRequest {
    #[serde(default)]
    pub entity_id: u32,
    #[serde(default)]
    pub content_type: u8,
    #[serde(default)]
    pub content_source: String,
    #[serde(default)]
    pub content_id: String,
    #[serde(default)]
    pub titles: Vec<String>,
    #[serde(default)]
    pub year: u16,
    #[serde(default)]
    pub season: Option<u16>,
    #[serde(default)]
    pub episode: Option<u16>,
    #[serde(default)]
    pub tier: String,
    #[serde(default = "default_limit")]
    pub limit: usize,
    #[serde(default)]
    pub include_files: bool,
    #[serde(default)]
    pub require_v1: bool,
    #[serde(default)]
    pub now_epoch: Option<u32>,
}

#[derive(Debug, Serialize)]
pub struct LookupResponse {
    pub linked: Vec<DocHit>,
    pub unlinked: Vec<DocHit>,
    pub entity_found: bool,
}

#[derive(Debug, Deserialize)]
pub struct RssRequest {
    #[serde(default)]
    pub content_type: Option<u8>,
    #[serde(default)]
    pub tier: String,
    #[serde(default = "default_limit")]
    pub limit: usize,
    #[serde(default)]
    pub offset: usize,
    #[serde(default)]
    pub include_files: bool,
    #[serde(default)]
    pub include_total: bool,
    #[serde(default)]
    pub any_clauses: Vec<FilterClause>,
    #[serde(default)]
    pub min_seeders: Option<u32>,
    #[serde(default)]
    pub require_v1: bool,
    #[serde(default)]
    pub now_epoch: Option<u32>,
}

#[derive(Debug, Serialize)]
pub struct RssResponse {
    pub docs: Vec<DocHit>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub total_count: Option<u64>,
}

#[derive(Debug, Deserialize)]
pub struct DashstatsRequest {
    #[serde(default)]
    pub tier: String,
    #[serde(default)]
    pub now_epoch: Option<u32>,
}

#[derive(Debug, Serialize)]
pub struct DashstatsResponse {
    pub movies_30d: u64,
    pub movies_linked_30d: u64,
    pub tv_30d: u64,
    pub tv_linked_30d: u64,
}

#[derive(Debug, Deserialize)]
pub struct SubstringRequest {
    pub substring: String,
    #[serde(default)]
    pub tier: String,
    #[serde(default = "default_limit")]
    pub limit: usize,
    #[serde(default)]
    pub include_files: bool,
    #[serde(default)]
    pub hash_order: bool,
    #[serde(default)]
    pub after_info_hash: Option<String>,
    #[serde(default)]
    pub now_epoch: Option<u32>,
}

#[derive(Debug, Deserialize)]
pub struct SyncResolveRequest {
    pub info_hashes: Vec<String>,
}

#[derive(Debug, Serialize)]
pub struct SyncResolveResponse {
    pub docs: HashMap<String, Option<u32>>,
}

#[derive(Debug, Deserialize)]
pub struct HydrateRequest {
    #[serde(default)]
    pub doc_ids: Vec<u32>,
    #[serde(default)]
    pub info_hashes: Vec<String>,
    #[serde(default)]
    pub tier: String,
    #[serde(default)]
    pub now_epoch: Option<u32>,
}

#[derive(Debug, Deserialize)]
pub struct HashLookupRequest {
    pub info_hash: String,
    #[serde(default)]
    pub tier: String,
    #[serde(default)]
    pub now_epoch: Option<u32>,
}

#[derive(Debug, Serialize)]
pub struct HashLookupResponse {
    pub found: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub doc_id: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub info_hash_v1: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub info_hash_v2: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub magnet_xt: Option<String>,
    pub eligible: bool,
}

#[derive(Debug, Deserialize)]
pub struct PolicyRevokeRequest {
    #[serde(default)]
    pub doc_id: Option<u32>,
    #[serde(default)]
    pub info_hash: Option<String>,
    pub reason: String,
    pub pg_lsn: u64,
    #[serde(default = "default_true")]
    pub sync: bool,
}

fn default_true() -> bool {
    true
}

#[derive(Debug, Serialize)]
pub struct PolicyRevokeResponse {
    pub ok: bool,
    pub doc_id: u32,
    pub pg_lsn: u64,
}

#[derive(Debug, Deserialize)]
pub struct PolicyLeaseRequest {
    pub pg_lsn: u64,
    pub ttl_ms: u64,
}

#[derive(Debug, Serialize)]
pub struct PolicyLeaseResponse {
    pub ok: bool,
    pub pg_lsn: u64,
    pub lease_until_ms: u64,
}

#[derive(Debug, Deserialize)]
pub struct SwarmUpdateItem {
    #[serde(default)]
    pub doc_id: Option<u32>,
    #[serde(default)]
    pub info_hash: Option<String>,
    #[serde(default)]
    pub seeders: Option<u32>,
    #[serde(default)]
    pub leechers: Option<u32>,
    pub checked_epoch_min: u32,
    #[serde(default)]
    pub tracker_id: u8,
}

#[derive(Debug, Deserialize)]
pub struct SwarmUpdateRequest {
    pub updates: Vec<SwarmUpdateItem>,
    #[serde(default = "default_true")]
    pub sync: bool,
}

#[derive(Debug, Serialize)]
pub struct SwarmUpdateResponse {
    pub ok: bool,
    pub seq: u64,
    pub applied: usize,
}

#[derive(Debug, Deserialize)]
pub struct SwarmSelectStaleRequest {
    #[serde(default = "default_limit")]
    pub limit: usize,
    #[serde(default)]
    pub now_epoch_min: Option<u32>,
    #[serde(default = "default_min_age")]
    pub min_age_min: u32,
}

fn default_min_age() -> u32 {
    360
}

#[derive(Debug, Serialize)]
pub struct StaleSwarmItem {
    pub doc_id: u32,
    pub info_hash_v1: String,
    pub checked_epoch_min: u32,
}

#[derive(Debug, Serialize)]
pub struct SwarmSelectStaleResponse {
    pub items: Vec<StaleSwarmItem>,
}

#[derive(Debug, Deserialize)]
pub struct L0UpsertItem {
    #[serde(default)]
    pub doc_id: Option<u32>,
    #[serde(default)]
    pub info_hash_v1: Option<String>,
    #[serde(default)]
    pub info_hash_v2: Option<String>,
    pub name: String,
    #[serde(default)]
    pub size_bytes: u64,
    pub content_type: u8,
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
    pub entity_id: u32,
    #[serde(default)]
    pub year: u16,
    pub published_epoch: u32,
    pub created_epoch: u32,
    #[serde(default)]
    pub files_count: u32,
    #[serde(default)]
    pub file_paths: Vec<String>,
    #[serde(default)]
    pub files: Vec<BuildFileInput>,
}

#[derive(Debug, Deserialize)]
pub struct L0UpsertRequest {
    pub docs: Vec<L0UpsertItem>,
    #[serde(default = "default_true")]
    pub sync: bool,
}

#[derive(Debug, Serialize)]
pub struct L0UpsertResponse {
    pub ok: bool,
    pub upserted: usize,
    pub l0_count: usize,
    pub l0_wal_bytes: u64,
    pub doc_ids: Vec<u32>,
}

pub struct DaemonEngine {
    pub generation_id: u64,
    pub core: CorePlane,
    pub hash_dir: HashDir,
    pub policy: PolicyPlane,
    pub swarm: SwarmPlane,
    pub catalog: MmapContentCatalog,
    pub search: CustomSearchPlane,
    pub allow_clock_override: bool,
}

impl DaemonEngine {
    pub fn open(
        db_root: &Path,
        wal_dir: &Path,
        initial_pg_lsn: u64,
        initial_lease_ttl_ms: u64,
    ) -> Result<Self, DaemonError> {
        let (manifest, gen_dir) = read_current_manifest(db_root)?;
        let gid = manifest.generation_id;
        let gen_wal_dir = wal_dir.join(format!("gen-{gid:016x}"));
        std::fs::create_dir_all(&gen_wal_dir).map_err(|e| DaemonError::Internal(e.to_string()))?;
        let now_s = now_epoch_sec();
        let now_m = now_epoch_ms();
        let lease_until = now_m.saturating_add(initial_lease_ttl_ms);

        // Seed writable swarm.chk into generation-scoped wal_dir so immutable generations and
        // concurrent readers across hot-swaps never collide on a live mmap.
        let wal_swarm_chk = gen_wal_dir.join("swarm.chk");
        if !wal_swarm_chk.exists() {
            std::fs::copy(gen_dir.join("swarm.chk"), &wal_swarm_chk)
                .map_err(|e| DaemonError::Internal(format!("seed swarm.chk: {e}")))?;
        }

        let core = CorePlane::open(&gen_dir, Some(gid))?;
        let hash_dir = HashDir::open(&gen_dir, Some(gid))?;
        let policy = PolicyPlane::open(
            &gen_dir,
            &gen_wal_dir.join("policy.wal"),
            Some(gid),
            now_s,
            initial_pg_lsn,
            lease_until,
        )?;
        let swarm = SwarmPlane::open(&gen_wal_dir, &gen_wal_dir.join("swarm.wal"), Some(gid))?;
        let catalog = MmapContentCatalog::open(&gen_dir.join("catalog.bin"), Some(gid))?;
        let search = CustomSearchPlane::open(&gen_dir, &gen_wal_dir.join("l0.wal"), Some(gid))?;

        Ok(Self {
            generation_id: gid,
            core,
            hash_dir,
            policy,
            swarm,
            catalog,
            search,
            allow_clock_override: false,
        })
    }

    pub fn health(&self) -> HealthResponse {
        let now_m = now_epoch_ms();
        let until = self.policy.lease().valid_until_ms();
        let active = now_m <= until;
        let rem = until.saturating_sub(now_m);
        let snap = self.policy.snapshot();
        HealthResponse {
            status: if active {
                "ok".into()
            } else {
                "lease_expired".into()
            },
            engine: "dh-tms-custom-v0.3".into(),
            generation_id: self.generation_id,
            num_docs: self.core.num_docs,
            pg_lsn: self.policy.lease().applied_lsn(),
            lease_active: active,
            lease_remaining_ms: rem,
            l0_count: self.search.l0.len(),
            l0_wal_bytes: self.search.l0.wal_bytes(),
            base_eligible: snap.base_eligible.len(),
            pub_eligible: snap.pub_eligible.len(),
            payload_eligible: snap.payload_eligible.len(),
        }
    }

    #[inline]
    fn effective_now_sec(&self, req_now: Option<u32>) -> u32 {
        let real = now_epoch_sec();
        if self.allow_clock_override {
            req_now.unwrap_or(real)
        } else {
            // Production default: never allow caller-supplied timestamps to wind the clock back
            // and bypass PayloadProof v2 expiry.
            real
        }
    }

    #[inline]
    fn fresh_now(&self, req_now: Option<u32>) -> (u32, u64) {
        let now_m = now_epoch_ms();
        let real_s = (now_m / 1000) as u32;
        let now_s = if self.allow_clock_override {
            req_now.unwrap_or(real_s)
        } else {
            real_s
        };
        (now_s, now_m)
    }

    #[inline]
    fn has_v1_hash(&self, doc_id: u32) -> bool {
        if (doc_id as usize) < self.core.num_docs {
            self.hash_dir.get_v1_for_doc(doc_id).is_some()
        } else {
            self.search
                .l0
                .get_doc(doc_id)
                .is_some_and(|ov| ov.info_hash_v1.is_some())
        }
    }

    fn primary_hash_bytes(&self, doc_id: u32) -> Option<Vec<u8>> {
        let (v1, v2) = self.doc_hashes_any(doc_id);
        if let Some(h1) = v1 {
            Some(h1.to_vec())
        } else {
            v2.map(|h2| h2.to_vec())
        }
    }

    fn lookup_hash_any(&self, ih: &InfoHash) -> Option<u32> {
        if let Some(d) = self.hash_dir.get(ih) {
            return Some(d);
        }
        match ih {
            InfoHash::V1(h) => self.search.l0.lookup_hash_v1(h),
            InfoHash::V2(h) => self.search.l0.lookup_hash_v2(h),
        }
    }

    fn doc_hashes_any(&self, doc_id: u32) -> (Option<[u8; 20]>, Option<[u8; 32]>) {
        if (doc_id as usize) < self.core.num_docs {
            return self.hash_dir.get_hashes_for_doc(doc_id);
        }
        if let Some(ov) = self.search.l0.get_doc(doc_id) {
            return (ov.info_hash_v1, ov.info_hash_v2);
        }
        (None, None)
    }

    fn resolve_doc_id(
        &self,
        doc_id: Option<u32>,
        info_hash: Option<&str>,
    ) -> Result<u32, DaemonError> {
        if let Some(d) = doc_id {
            if (d as usize) >= self.core.num_docs
                && ((d as usize) >= self.policy.max_docs || self.search.l0.get_doc(d).is_none())
            {
                return Err(DaemonError::BadRequest(format!("doc_id {d} out of range")));
            }
            return Ok(d);
        }
        let raw = info_hash
            .ok_or_else(|| DaemonError::BadRequest("doc_id or info_hash required".into()))?;
        let ih = parse_info_hash_hex(raw).ok_or_else(|| {
            DaemonError::BadRequest("invalid info_hash hex (expected 40 or 64 hex chars)".into())
        })?;
        self.lookup_hash_any(&ih)
            .ok_or_else(|| DaemonError::BadRequest("info_hash not found in generation".into()))
    }

    fn hydrate_hit(
        &self,
        doc_id: u32,
        score: f32,
        include_files: bool,
    ) -> Result<DocHit, DaemonError> {
        let l0_ov = self.search.l0.get_doc(doc_id);
        if (doc_id as usize) >= self.core.num_docs {
            let ov = l0_ov
                .ok_or_else(|| DaemonError::Internal(format!("missing dynamic L0 doc {doc_id}")))?;
            let (v1, v2) = (ov.info_hash_v1, ov.info_hash_v2);
            let magnet_xt = if let Some(h1) = v1 {
                InfoHash::V1(h1).to_magnet_xt()
            } else if let Some(h2) = v2 {
                InfoHash::V2(h2).to_magnet_xt()
            } else {
                String::new()
            };
            let sw = self.swarm.read(doc_id)?;
            let files = if include_files {
                Some(
                    ov.files
                        .into_iter()
                        .map(|(path, size, extension)| FileEntryResp {
                            path,
                            size,
                            extension,
                        })
                        .collect(),
                )
            } else {
                None
            };
            return Ok(DocHit {
                doc_id,
                score,
                info_hash_v1: v1.map(|h| encode_hex(&h)),
                info_hash_v2: v2.map(|h| encode_hex(&h)),
                magnet_xt,
                name: ov.name,
                size_bytes: ov.size_bytes,
                content_type: ov.content_type,
                resolution: ov.resolution,
                is_anime: ov.is_anime,
                video_3d: ov.video_3d,
                video_source: ov.video_source,
                video_codec: ov.video_codec,
                entity_id: ov.entity_id,
                year: ov.year,
                published_epoch: ov.published_epoch,
                created_epoch: ov.created_epoch,
                files_count: ov.files_count,
                seeders: sw.seeders,
                leechers: sw.leechers,
                peak_seeders: sw.peak_seeders,
                checked_epoch_min: sw.checked_epoch_min,
                files,
            });
        }
        let mut meta = self.core.metas()[doc_id as usize];
        let mut is_anime = meta.is_anime();
        let mut video_3d = meta.video_3d();
        let mut video_source = meta.source();
        let mut video_codec = meta.codec();
        if let Some(ref ov) = l0_ov {
            meta.content_type = ov.content_type;
            meta.resolution = ov.resolution;
            meta.entity_id = ov.entity_id;
            meta.year = ov.year;
            meta.published_epoch = ov.published_epoch;
            meta.created_epoch = ov.created_epoch;
            is_anime = ov.is_anime;
            video_3d = ov.video_3d;
            video_source = ov.video_source;
            video_codec = ov.video_codec;
            if ov.size_bytes > 0 {
                meta.size_bytes = ov.size_bytes;
            }
            if ov.files_count > 0 {
                meta.files_count = ov.files_count;
            }
        }
        let (v1, v2) = self.hash_dir.get_hashes_for_doc(doc_id);
        let magnet_xt = if let Some(h1) = v1 {
            InfoHash::V1(h1).to_magnet_xt()
        } else if let Some(h2) = v2 {
            InfoHash::V2(h2).to_magnet_xt()
        } else {
            String::new()
        };
        let sw = self.swarm.read(doc_id)?;
        let (mut name, files) = if include_files {
            let full = self.core.hydrate_full(doc_id)?;
            let fl = full
                .files
                .into_iter()
                .map(|f| FileEntryResp {
                    path: f.path,
                    size: f.size,
                    extension: f.ext,
                })
                .collect();
            (full.name, Some(fl))
        } else {
            (self.core.get_name(doc_id)?.to_owned(), None)
        };
        if let Some(ov) = l0_ov {
            name = ov.name;
        }
        Ok(DocHit {
            doc_id,
            score,
            info_hash_v1: v1.map(|h| encode_hex(&h)),
            info_hash_v2: v2.map(|h| encode_hex(&h)),
            magnet_xt,
            name,
            size_bytes: meta.size_bytes,
            content_type: meta.content_type,
            resolution: meta.resolution,
            is_anime,
            video_3d,
            video_source,
            video_codec,
            entity_id: meta.entity_id,
            year: meta.year,
            published_epoch: meta.published_epoch,
            created_epoch: meta.created_epoch,
            files_count: meta.files_count,
            seeders: sw.seeders,
            leechers: sw.leechers,
            peak_seeders: sw.peak_seeders,
            checked_epoch_min: sw.checked_epoch_min,
            files,
        })
    }

    pub fn handle_search(&self, req: SearchRequest) -> Result<SearchResponse, DaemonError> {
        let tier = parse_tier(&req.tier)?;
        let mode = parse_mode(&req.mode)?;
        let limit = req.limit.clamp(1, 500);
        let now_s = self.effective_now_sec(req.now_epoch);
        let now_m = now_epoch_ms();
        self.policy.lease().check(now_m)?;

        let search_clauses: Vec<SearchFilterClause> = req
            .any_clauses
            .iter()
            .map(FilterClause::to_search_clause)
            .collect();
        let clause_bm = if !search_clauses.is_empty() {
            let snap = self.policy.snapshot();
            Some(
                self.search
                    .build_filter_bitmap(&search_clauses, snap.bitmap_for(tier)),
            )
        } else {
            None
        };
        let l0_closure = |ov: &L0Doc| search_clauses.iter().any(|c| c.matches_l0(ov));
        let l0_pred: Option<&dyn Fn(&L0Doc) -> bool> = if !search_clauses.is_empty() {
            Some(&l0_closure)
        } else {
            None
        };
        let min_seeders = req.min_seeders;
        let require_v1 = req.require_v1;
        let doc_closure = |doc_id: u32| {
            if require_v1 && !self.has_v1_hash(doc_id) {
                return false;
            }
            if let Some(min_s) = min_seeders {
                return self
                    .swarm
                    .read(doc_id)
                    .ok()
                    .and_then(|sw| sw.seeders)
                    .is_some_and(|s| s >= min_s);
            }
            true
        };
        let doc_pred: Option<&dyn Fn(u32) -> bool> = if min_seeders.is_some() || require_v1 {
            Some(&doc_closure)
        } else {
            None
        };

        let scored = if let Some(season) = req.tv_season {
            self.search.tvsearch_text_filtered(
                &req.query,
                season,
                req.tv_episode,
                clause_bm.as_ref(),
                l0_pred,
                doc_pred,
                &self.policy,
                tier,
                &self.core,
                limit,
                now_s,
                now_m,
            )?
        } else {
            self.search.search_text_filtered(
                &req.query,
                mode,
                clause_bm.as_ref(),
                l0_pred,
                doc_pred,
                &self.policy,
                tier,
                &self.core,
                limit,
                now_s,
                now_m,
            )?
        };

        let mut docs = Vec::with_capacity(scored.len());
        for sd in scored {
            let (egress_s, egress_m) = self.fresh_now(req.now_epoch);
            if !self
                .policy
                .authorize_egress_one(sd.doc_id, tier, egress_s, egress_m)?
            {
                continue;
            }
            docs.push(self.hydrate_hit(sd.doc_id, sd.score, req.include_files)?);
        }

        let (total_facets, ct_facets, res_facets) = if req.include_facets {
            let (facet_s, facet_m) = self.fresh_now(req.now_epoch);
            let (ct, res, tot) = self.search.text_facet_counts_filtered(
                &req.query,
                clause_bm.as_ref(),
                l0_pred,
                doc_pred,
                &self.policy,
                tier,
                self.core.facet_bytes(),
                facet_s,
                facet_m,
            )?;
            (Some(tot), Some(ct), Some(res))
        } else {
            (None, None, None)
        };

        Ok(SearchResponse {
            docs,
            total_facets,
            ct_facets,
            res_facets,
        })
    }

    pub fn handle_lookup(&self, req: LookupRequest) -> Result<LookupResponse, DaemonError> {
        let tier = parse_tier(&req.tier)?;
        let limit = req.limit.clamp(1, 500);
        let now_s = self.effective_now_sec(req.now_epoch);
        let now_m = now_epoch_ms();
        self.policy.lease().check(now_m)?;

        let mut entity_id = req.entity_id;
        let mut entity_found = entity_id != 0;
        let mut effective_ct = req.content_type;
        let mut titles = req.titles;
        let mut year = req.year;
        if entity_id == 0 && !req.content_id.is_empty() {
            let src = if req.content_source.is_empty() {
                "tmdb"
            } else {
                req.content_source.as_str()
            };
            let mut resolved = self
                .catalog
                .find_by_key(effective_ct, src, &req.content_id)?;
            if resolved.is_none() {
                resolved = self
                    .catalog
                    .find_by_alt_id(effective_ct, src, &req.content_id)?
                    .into_iter()
                    .next();
            }
            if resolved.is_none() && effective_ct == 0 {
                for probe_ct in [1u8, 2u8] {
                    resolved = self.catalog.find_by_key(probe_ct, src, &req.content_id)?;
                    if resolved.is_none() {
                        resolved = self
                            .catalog
                            .find_by_alt_id(probe_ct, src, &req.content_id)?
                            .into_iter()
                            .next();
                    }
                    if resolved.is_some() {
                        break;
                    }
                }
            }
            if let Some(rec) = resolved {
                entity_found = true;
                entity_id = rec.entity_id;
                if effective_ct == 0 {
                    effective_ct = rec.content_type;
                }
                if year == 0 {
                    year = rec.year;
                }
                if titles.is_empty() {
                    titles.push(rec.title);
                    if !rec.original_title.is_empty() {
                        titles.push(rec.original_title);
                    }
                }
            }
        }

        let season_ep = req.season.map(|s| (s, req.episode));
        let v1_closure = |doc_id: u32| self.has_v1_hash(doc_id);
        let doc_pred: Option<&dyn Fn(u32) -> bool> = if req.require_v1 {
            Some(&v1_closure)
        } else {
            None
        };
        let (linked_ids, unlinked_ids) = self.search.lookup_entity_with_fallback_filtered(
            entity_id,
            &titles,
            effective_ct,
            year,
            season_ep,
            doc_pred,
            &self.policy,
            tier,
            &self.core,
            limit,
            now_s,
            now_m,
        )?;

        let mut linked = Vec::with_capacity(linked_ids.len());
        for d in linked_ids {
            let (egress_s, egress_m) = self.fresh_now(req.now_epoch);
            if self.policy.authorize_egress_one(d, tier, egress_s, egress_m)? {
                linked.push(self.hydrate_hit(d, 0.0, req.include_files)?);
            }
        }
        let mut unlinked = Vec::with_capacity(unlinked_ids.len());
        for d in unlinked_ids {
            let (egress_s, egress_m) = self.fresh_now(req.now_epoch);
            if self.policy.authorize_egress_one(d, tier, egress_s, egress_m)? {
                unlinked.push(self.hydrate_hit(d, 0.0, req.include_files)?);
            }
        }
        Ok(LookupResponse {
            linked,
            unlinked,
            entity_found,
        })
    }

    pub fn handle_rss(&self, req: RssRequest) -> Result<RssResponse, DaemonError> {
        let tier = parse_tier(&req.tier)?;
        let limit = req.limit.clamp(1, 500);
        let now_s = self.effective_now_sec(req.now_epoch);
        let now_m = now_epoch_ms();
        self.policy.lease().check(now_m)?;
        let ct_opt = req.content_type.filter(|&c| c != CT_UNKNOWN);

        let search_clauses: Vec<SearchFilterClause> = req
            .any_clauses
            .iter()
            .map(FilterClause::to_search_clause)
            .collect();
        let clause_bm = if !search_clauses.is_empty() {
            let snap = self.policy.snapshot();
            Some(
                self.search
                    .build_filter_bitmap(&search_clauses, snap.bitmap_for(tier)),
            )
        } else {
            None
        };
        let l0_closure = |ov: &L0Doc| search_clauses.iter().any(|c| c.matches_l0(ov));
        let l0_pred: Option<&dyn Fn(&L0Doc) -> bool> = if !search_clauses.is_empty() {
            Some(&l0_closure)
        } else {
            None
        };
        let min_seeders = req.min_seeders;
        let require_v1 = req.require_v1;
        let doc_closure = |doc_id: u32| {
            if require_v1 && !self.has_v1_hash(doc_id) {
                return false;
            }
            if let Some(min_s) = min_seeders {
                return self
                    .swarm
                    .read(doc_id)
                    .ok()
                    .and_then(|sw| sw.seeders)
                    .is_some_and(|s| s >= min_s);
            }
            true
        };
        let doc_pred: Option<&dyn Fn(u32) -> bool> = if min_seeders.is_some() || require_v1 {
            Some(&doc_closure)
        } else {
            None
        };

        let doc_ids = self.search.browse_newest_filtered(
            ct_opt,
            clause_bm.as_ref(),
            l0_pred,
            doc_pred,
            &self.policy,
            tier,
            self.core.metas(),
            limit,
            req.offset,
            now_s,
            now_m,
        )?;
        let mut docs = Vec::with_capacity(doc_ids.len());
        for d in doc_ids {
            let (egress_s, egress_m) = self.fresh_now(req.now_epoch);
            if self.policy.authorize_egress_one(d, tier, egress_s, egress_m)? {
                docs.push(self.hydrate_hit(d, 0.0, req.include_files)?);
            }
        }
        let total_count = if req.include_total {
            let (count_s, count_m) = self.fresh_now(req.now_epoch);
            Some(self.search.count_browse_filtered(
                ct_opt,
                clause_bm.as_ref(),
                l0_pred,
                doc_pred,
                &self.policy,
                tier,
                count_s,
                count_m,
            )?)
        } else {
            None
        };
        Ok(RssResponse { docs, total_count })
    }

    pub fn handle_dashstats(
        &self,
        req: DashstatsRequest,
    ) -> Result<DashstatsResponse, DaemonError> {
        let tier = parse_tier(&req.tier)?;
        let now_s = self.effective_now_sec(req.now_epoch);
        let now_m = now_epoch_ms();
        self.policy.lease().check(now_m)?;
        let (movies_30d, movies_linked_30d, tv_30d, tv_linked_30d) =
            self.search
                .dashstats_30d(now_s, &self.policy, tier, self.core.metas(), now_m)?;
        Ok(DashstatsResponse {
            movies_30d,
            movies_linked_30d,
            tv_30d,
            tv_linked_30d,
        })
    }

    pub fn handle_substring(&self, req: SubstringRequest) -> Result<RssResponse, DaemonError> {
        let tier = parse_tier(&req.tier)?;
        let limit = req.limit.clamp(1, 500);
        let now_s = self.effective_now_sec(req.now_epoch);
        let now_m = now_epoch_ms();
        self.policy.lease().check(now_m)?;
        let after_bytes: Option<Vec<u8>> = match req.after_info_hash.as_deref() {
            Some(raw) if !raw.trim().is_empty() => match parse_info_hash_hex(raw.trim()) {
                Some(InfoHash::V1(h)) => Some(h.to_vec()),
                Some(InfoHash::V2(h)) => Some(h.to_vec()),
                None => {
                    return Err(DaemonError::BadRequest(
                        "invalid after_info_hash hex".into(),
                    ))
                }
            },
            _ => None,
        };
        let doc_ids = if req.hash_order || after_bytes.is_some() {
            const SUBSTRING_HASH_ORDER_MAX_SCAN: usize = 100_000;
            self.search.search_substring_hash_ordered(
                &req.substring,
                after_bytes.as_deref(),
                |d| self.primary_hash_bytes(d),
                SUBSTRING_HASH_ORDER_MAX_SCAN,
                &self.policy,
                tier,
                &self.core,
                limit,
                now_s,
                now_m,
            )?
        } else {
            self.search.search_substring(
                &req.substring,
                &self.policy,
                tier,
                &self.core,
                limit,
                now_s,
                now_m,
            )?
        };
        let mut docs = Vec::with_capacity(doc_ids.len());
        for d in doc_ids {
            let (egress_s, egress_m) = self.fresh_now(req.now_epoch);
            if self.policy.authorize_egress_one(d, tier, egress_s, egress_m)? {
                docs.push(self.hydrate_hit(d, 0.0, req.include_files)?);
            }
        }
        Ok(RssResponse {
            docs,
            total_count: None,
        })
    }

    pub fn handle_hydrate(&self, req: HydrateRequest) -> Result<RssResponse, DaemonError> {
        let tier = parse_tier(&req.tier)?;
        let now_m = now_epoch_ms();
        self.policy.lease().check(now_m)?;

        let mut targets: Vec<u32> = Vec::with_capacity(req.doc_ids.len() + req.info_hashes.len());
        for d in req.doc_ids {
            if (d as usize) < self.core.num_docs
                || ((d as usize) < self.policy.max_docs && self.search.l0.get_doc(d).is_some())
            {
                targets.push(d);
            }
        }
        for raw in &req.info_hashes {
            if let Some(ih) = parse_info_hash_hex(raw) {
                if let Some(d) = self.lookup_hash_any(&ih) {
                    targets.push(d);
                }
            }
        }
        let mut docs = Vec::with_capacity(targets.len());
        for d in targets {
            let (egress_s, egress_m) = self.fresh_now(req.now_epoch);
            if self.policy.authorize_egress_one(d, tier, egress_s, egress_m)? {
                docs.push(self.hydrate_hit(d, 0.0, true)?);
            }
        }
        Ok(RssResponse {
            docs,
            total_count: None,
        })
    }

    pub fn handle_hash_lookup(
        &self,
        req: HashLookupRequest,
    ) -> Result<HashLookupResponse, DaemonError> {
        let tier = parse_tier(&req.tier)?;
        let now_m = now_epoch_ms();
        self.policy.lease().check(now_m)?;

        let ih = parse_info_hash_hex(&req.info_hash)
            .ok_or_else(|| DaemonError::BadRequest("invalid info_hash hex".into()))?;
        let Some(doc_id) = self.lookup_hash_any(&ih) else {
            return Ok(HashLookupResponse {
                found: false,
                doc_id: None,
                info_hash_v1: None,
                info_hash_v2: None,
                magnet_xt: None,
                eligible: false,
            });
        };
        let (egress_s, egress_m) = self.fresh_now(req.now_epoch);
        let eligible = self
            .policy
            .authorize_egress_one(doc_id, tier, egress_s, egress_m)?;
        let (v1, v2) = self.doc_hashes_any(doc_id);
        let magnet_xt = if let Some(h1) = v1 {
            Some(InfoHash::V1(h1).to_magnet_xt())
        } else {
            v2.map(|h2| InfoHash::V2(h2).to_magnet_xt())
        };
        Ok(HashLookupResponse {
            found: true,
            doc_id: Some(doc_id),
            info_hash_v1: v1.map(|h| encode_hex(&h)),
            info_hash_v2: v2.map(|h| encode_hex(&h)),
            magnet_xt,
            eligible,
        })
    }

    /// Authenticated internal sync resolver: maps hex infohashes to internal `doc_id`s
    /// WITHOUT requiring or renewing the public governance lease, so the sync worker can
    /// replay pending revocations while public reads remain strictly fail-closed.
    pub fn handle_sync_resolve(
        &self,
        req: SyncResolveRequest,
    ) -> Result<SyncResolveResponse, DaemonError> {
        let mut docs = HashMap::with_capacity(req.info_hashes.len());
        for raw in req.info_hashes {
            let ih = parse_info_hash_hex(&raw)
                .ok_or_else(|| DaemonError::BadRequest(format!("invalid info_hash hex: {raw}")))?;
            docs.insert(raw, self.lookup_hash_any(&ih));
        }
        Ok(SyncResolveResponse { docs })
    }

    pub fn handle_policy_revoke(
        &self,
        req: PolicyRevokeRequest,
    ) -> Result<PolicyRevokeResponse, DaemonError> {
        let doc_id = self.resolve_doc_id(req.doc_id, req.info_hash.as_deref())?;
        let reason = parse_revoke_reason(&req.reason)?;
        self.policy
            .revoke_immediate(doc_id, reason, req.pg_lsn, req.sync)?;
        Ok(PolicyRevokeResponse {
            ok: true,
            doc_id,
            pg_lsn: req.pg_lsn,
        })
    }

    pub fn handle_policy_lease(
        &self,
        req: PolicyLeaseRequest,
    ) -> Result<PolicyLeaseResponse, DaemonError> {
        let until = if req.ttl_ms == 0 {
            self.policy.lease().expire_now_at(req.pg_lsn);
            0
        } else {
            let u = now_epoch_ms().saturating_add(req.ttl_ms);
            if !self.policy.lease().renew(req.pg_lsn, u) {
                return Err(DaemonError::BadRequest(format!(
                    "stale pg_lsn {} rejected by governance lease fence",
                    req.pg_lsn
                )));
            }
            u
        };
        Ok(PolicyLeaseResponse {
            ok: true,
            pg_lsn: self.policy.lease().applied_lsn(),
            lease_until_ms: until,
        })
    }

    pub fn handle_swarm_update(
        &self,
        req: SwarmUpdateRequest,
    ) -> Result<SwarmUpdateResponse, DaemonError> {
        let mut updates = Vec::with_capacity(req.updates.len());
        for item in req.updates {
            let doc_id = self.resolve_doc_id(item.doc_id, item.info_hash.as_deref())?;
            updates.push(SwarmUpdate {
                doc_id,
                seeders: item.seeders,
                leechers: item.leechers,
                checked_epoch_min: item.checked_epoch_min,
                tracker_id: item.tracker_id,
            });
        }
        let seq = self.swarm.update_batch_durable(&updates, req.sync)?;
        Ok(SwarmUpdateResponse {
            ok: true,
            seq,
            applied: updates.len(),
        })
    }

    pub fn handle_swarm_select_stale(
        &self,
        req: SwarmSelectStaleRequest,
    ) -> Result<SwarmSelectStaleResponse, DaemonError> {
        let now_m = now_epoch_ms();
        self.policy.lease().check(now_m)?;
        let now_min = req.now_epoch_min.unwrap_or_else(|| now_epoch_sec() / 60);
        let limit = req.limit.clamp(1, 50_000);
        let snap = self.policy.snapshot();
        let doc_ids = self.swarm.select_stale(
            now_min,
            req.min_age_min,
            limit,
            &snap.payload_eligible,
            &snap.base_eligible,
        );
        let mut items = Vec::with_capacity(doc_ids.len());
        for d in doc_ids {
            if let Some(h1) = self.hash_dir.get_v1_for_doc(d) {
                let sw = self.swarm.read(d)?;
                items.push(StaleSwarmItem {
                    doc_id: d,
                    info_hash_v1: encode_hex(&h1),
                    checked_epoch_min: sw.checked_epoch_min,
                });
            }
        }
        Ok(SwarmSelectStaleResponse { items })
    }

    pub fn handle_swarm_checkpoint(&self) -> Result<serde_json::Value, DaemonError> {
        let seq = self.swarm.checkpoint()?;
        Ok(serde_json::json!({ "ok": true, "checkpoint_seq": seq }))
    }

    pub fn handle_l0_upsert(&self, req: L0UpsertRequest) -> Result<L0UpsertResponse, DaemonError> {
        let mut batch = Vec::with_capacity(req.docs.len());
        let mut assigned_ids = Vec::with_capacity(req.docs.len());
        for item in req.docs {
            let v1_opt = match item.info_hash_v1.as_deref() {
                Some(raw) if !raw.is_empty() => match parse_info_hash_hex(raw) {
                    Some(InfoHash::V1(h)) => Some(h),
                    _ => {
                        return Err(DaemonError::BadRequest(format!(
                            "invalid info_hash_v1: {raw}"
                        )))
                    }
                },
                _ => None,
            };
            let v2_opt = match item.info_hash_v2.as_deref() {
                Some(raw) if !raw.is_empty() => match parse_info_hash_hex(raw) {
                    Some(InfoHash::V2(h)) => Some(h),
                    _ => {
                        return Err(DaemonError::BadRequest(format!(
                            "invalid info_hash_v2: {raw}"
                        )))
                    }
                },
                _ => None,
            };

            let doc_id = if let Some(d) = item.doc_id {
                if (d as usize) >= self.policy.max_docs {
                    return Err(DaemonError::BadRequest(format!(
                        "doc_id {} out of range (max_docs={})",
                        d, self.policy.max_docs
                    )));
                }
                if (d as usize) >= self.core.num_docs
                    && self.search.l0.get_doc(d).is_none()
                    && v1_opt.is_none()
                    && v2_opt.is_none()
                {
                    return Err(DaemonError::BadRequest(format!(
                        "doc_id {} out of range (num_docs={})",
                        d, self.core.num_docs
                    )));
                }
                d
            } else {
                if v1_opt.is_none() && v2_opt.is_none() {
                    return Err(DaemonError::BadRequest(
                        "doc_id or info_hash_v1/info_hash_v2 required for l0 upsert".into(),
                    ));
                }
                if let Some(d) = v1_opt
                    .and_then(|h| self.hash_dir.get(&InfoHash::V1(h)))
                    .or_else(|| v2_opt.and_then(|h| self.hash_dir.get(&InfoHash::V2(h))))
                {
                    d
                } else {
                    self.search
                        .l0
                        .allocate_or_get_doc_id(
                            self.core.num_docs as u32,
                            self.policy.max_docs as u32,
                            v1_opt,
                            v2_opt,
                        )
                        .map_err(|e| DaemonError::BadRequest(e.to_string()))?
                }
            };

            let pr = parse_release_name(&item.name);
            let mut terms: Vec<(u64, u16)> = Vec::new();
            for t in &pr.title_tokens {
                terms.push((term_hash(t, self.search.term_seed), 1500));
            }
            for t in &pr.raw_tokens {
                terms.push((term_hash(t, self.search.term_seed), 600));
            }
            let files: Vec<(String, u64, String)> = if !item.files.is_empty() {
                item.files
                    .into_iter()
                    .take(256)
                    .map(|f| (f.path, f.size, f.extension.to_ascii_lowercase()))
                    .collect()
            } else {
                item.file_paths
                    .into_iter()
                    .take(256)
                    .map(|fp| (fp, 0u64, String::new()))
                    .collect()
            };
            for (fp, _, _) in files.iter().take(64) {
                for t in tokenize_alnum(fp) {
                    terms.push((term_hash(&t, self.search.term_seed), 250));
                }
            }
            let files_count = if item.files_count > 0 {
                item.files_count
            } else {
                files.len() as u32
            };
            let eps: Vec<EpisodeSpec> = pr.episodes;
            if item.video_3d > 3 {
                return Err(DaemonError::BadRequest(format!(
                    "video_3d {} exceeds supported range 0..=3",
                    item.video_3d
                )));
            }
            if item.video_source > MAX_VIDEO_SOURCE {
                return Err(DaemonError::BadRequest(format!(
                    "video_source {} exceeds supported range 0..={}",
                    item.video_source, MAX_VIDEO_SOURCE
                )));
            }
            if item.video_codec > MAX_VIDEO_CODEC {
                return Err(DaemonError::BadRequest(format!(
                    "video_codec {} exceeds supported range 0..={}",
                    item.video_codec, MAX_VIDEO_CODEC
                )));
            }
            let v3d = if item.video_3d != 0 {
                item.video_3d
            } else if item.is_3d {
                1
            } else {
                0
            };
            assigned_ids.push(doc_id);
            batch.push(L0Doc {
                doc_id,
                name: item.name,
                content_type: item.content_type,
                resolution: if item.resolution != 0 {
                    item.resolution
                } else {
                    pr.resolution
                },
                is_anime: item.is_anime,
                video_3d: v3d,
                video_source: item.video_source,
                video_codec: item.video_codec,
                entity_id: item.entity_id,
                year: if item.year != 0 { item.year } else { pr.year },
                published_epoch: item.published_epoch,
                created_epoch: item.created_epoch,
                episodes: eps,
                strict_title_hashes: pr.strict_title_hashes,
                family_hashes: pr.family_hashes,
                terms,
                info_hash_v1: v1_opt,
                info_hash_v2: v2_opt,
                size_bytes: item.size_bytes,
                files_count,
                files,
            });
        }
        let upserted = self
            .search
            .l0
            .upsert_batch(&batch, req.sync)
            .map_err(|e| DaemonError::Internal(e.to_string()))?;
        Ok(L0UpsertResponse {
            ok: true,
            upserted,
            l0_count: self.search.l0.len(),
            l0_wal_bytes: self.search.l0.wal_bytes(),
            doc_ids: assigned_ids,
        })
    }

    pub fn handle_policy_batch(&self, body: &[u8]) -> Result<serde_json::Value, DaemonError> {
        #[derive(Deserialize)]
        struct OpItem {
            op: String,
            #[serde(default)]
            doc_id: Option<u32>,
            #[serde(default)]
            info_hash: Option<String>,
            #[serde(default)]
            reason: String,
            #[serde(default)]
            expires_epoch: u32,
        }
        #[derive(Deserialize)]
        struct Req {
            ops: Vec<OpItem>,
            pg_lsn: u64,
            #[serde(default = "default_true")]
            sync: bool,
            #[serde(default = "default_true")]
            rebuild_snapshot: bool,
            #[serde(default)]
            now_epoch: Option<u32>,
        }
        let req: Req = serde_json::from_slice(body)
            .map_err(|e| DaemonError::BadRequest(format!("invalid json: {e}")))?;
        let mut ops = Vec::with_capacity(req.ops.len());
        for it in req.ops {
            let doc_id = self.resolve_doc_id(it.doc_id, it.info_hash.as_deref())?;
            let op = match it.op.as_str() {
                "revoke" => PolicyOp::Revoke {
                    doc_id,
                    reason: parse_revoke_reason(&it.reason)?,
                },
                "unrevoke" => PolicyOp::Unrevoke {
                    doc_id,
                    reason: parse_revoke_reason(&it.reason)?,
                },
                "grant_pub" => PolicyOp::GrantPubApproval { doc_id },
                "revoke_pub" => PolicyOp::RevokePubApproval { doc_id },
                "grant_payload" => PolicyOp::GrantPayloadProof {
                    doc_id,
                    expires_epoch_sec: it.expires_epoch,
                },
                "revoke_payload" => PolicyOp::RevokePayloadProof { doc_id },
                "grant_base" => PolicyOp::GrantBase { doc_id },
                other => {
                    return Err(DaemonError::BadRequest(format!(
                        "unknown policy op: {other}"
                    )))
                }
            };
            ops.push(op);
        }
        let now_s = self.effective_now_sec(req.now_epoch);
        let applied = ops.len();
        self.policy
            .apply_batch(&ops, req.pg_lsn, req.sync, req.rebuild_snapshot, now_s)?;
        Ok(serde_json::json!({ "ok": true, "applied": applied, "pg_lsn": req.pg_lsn }))
    }
}

#[derive(Debug, Deserialize)]
pub struct ReloadRequest {
    #[serde(default)]
    pub pg_lsn: Option<u64>,
    #[serde(default = "default_reload_lease_ttl")]
    pub lease_ttl_ms: u64,
}

fn default_reload_lease_ttl() -> u64 {
    60_000
}

#[derive(Debug, Serialize)]
pub struct ReloadResponse {
    pub ok: bool,
    pub generation_id: u64,
    pub num_docs: usize,
    pub pg_lsn: u64,
}

#[derive(Debug, Serialize)]
pub struct BuildGenerationResponse {
    pub ok: bool,
    pub generation_id: u64,
    pub num_docs: usize,
    pub pg_lsn: u64,
}

/// Lock-free hot-swappable host around `DaemonEngine` backed by `ArcSwap`.
/// Supports empty-volume bootstrap, streaming NDJSON generation builds, and atomic reload.
pub struct EngineHost {
    engine: ArcSwap<DaemonEngine>,
    db_root: PathBuf,
    wal_dir: PathBuf,
    allow_clock_override: bool,
    swap_mu: Mutex<()>,
}

impl EngineHost {
    pub fn open_or_init(
        db_root: &Path,
        wal_dir: &Path,
        initial_pg_lsn: u64,
        initial_lease_ttl_ms: u64,
        allow_clock_override: bool,
    ) -> Result<Self, DaemonError> {
        let initialized_empty = !db_root.join("CURRENT.json").exists();
        if initialized_empty {
            build_empty_generation(db_root)?;
        }
        let mut eng = DaemonEngine::open(db_root, wal_dir, initial_pg_lsn, initial_lease_ttl_ms)?;
        if initialized_empty || eng.generation_id == 0 {
            // Unbootstrapped generation 0 must fail closed on data reads until the sync worker
            // completes its initial /v1/generation/build, while /v1/health stays 200 OK.
            eng.policy.lease().expire_now();
        }
        eng.allow_clock_override = allow_clock_override;
        Ok(Self {
            engine: ArcSwap::from_pointee(eng),
            db_root: db_root.to_path_buf(),
            wal_dir: wal_dir.to_path_buf(),
            allow_clock_override,
            swap_mu: Mutex::new(()),
        })
    }

    pub fn from_engine(engine: DaemonEngine, db_root: PathBuf, wal_dir: PathBuf) -> Self {
        let allow_clock_override = engine.allow_clock_override;
        Self {
            engine: ArcSwap::from_pointee(engine),
            db_root,
            wal_dir,
            allow_clock_override,
            swap_mu: Mutex::new(()),
        }
    }

    #[inline]
    pub fn load(&self) -> Arc<DaemonEngine> {
        self.engine.load_full()
    }

    pub fn build_and_swap<R: BufRead>(
        &self,
        reader: R,
    ) -> Result<BuildGenerationResponse, DaemonError> {
        let _guard = self.swap_mu.lock();
        let cur = self.engine.load();
        let next_gid = cur.generation_id.saturating_add(1).max(1);
        let outcome = build_generation_from_ndjson(reader, &self.db_root, next_gid)?;
        let mut next_eng = DaemonEngine::open(
            &self.db_root,
            &self.wal_dir,
            outcome.pg_lsn,
            outcome.lease_ttl_ms,
        )?;
        next_eng.allow_clock_override = self.allow_clock_override;
        let resp = BuildGenerationResponse {
            ok: true,
            generation_id: next_eng.generation_id,
            num_docs: next_eng.core.num_docs,
            pg_lsn: outcome.pg_lsn,
        };
        self.engine.store(Arc::new(next_eng));
        prune_old_wal_dirs(&self.wal_dir, resp.generation_id);
        Ok(resp)
    }

    pub fn reload(&self, req: ReloadRequest) -> Result<ReloadResponse, DaemonError> {
        let _guard = self.swap_mu.lock();
        let cur = self.engine.load();
        let pg_lsn = req
            .pg_lsn
            .unwrap_or_else(|| cur.policy.lease().applied_lsn());
        let mut next_eng = DaemonEngine::open(
            &self.db_root,
            &self.wal_dir,
            pg_lsn,
            req.lease_ttl_ms.max(1_000),
        )?;
        next_eng.allow_clock_override = self.allow_clock_override;
        let resp = ReloadResponse {
            ok: true,
            generation_id: next_eng.generation_id,
            num_docs: next_eng.core.num_docs,
            pg_lsn,
        };
        self.engine.store(Arc::new(next_eng));
        prune_old_wal_dirs(&self.wal_dir, resp.generation_id);
        Ok(resp)
    }
}

fn prune_old_wal_dirs(wal_dir: &Path, current_gen: u64) {
    let Ok(entries) = std::fs::read_dir(wal_dir) else {
        return;
    };
    let mut dirs: Vec<(u64, PathBuf)> = Vec::new();
    for ent in entries.flatten() {
        let name = ent.file_name();
        let s = name.to_string_lossy();
        if let Some(hex) = s.strip_prefix("gen-") {
            if let Ok(id) = u64::from_str_radix(hex, 16) {
                if id != current_gen {
                    dirs.push((id, ent.path()));
                }
            }
        }
    }
    dirs.sort_by_key(|(id, _)| *id);
    if dirs.len() > 1 {
        let drop_n = dirs.len() - 1;
        for (_, path) in dirs.into_iter().take(drop_n) {
            let _ = std::fs::remove_dir_all(path);
        }
    }
}
