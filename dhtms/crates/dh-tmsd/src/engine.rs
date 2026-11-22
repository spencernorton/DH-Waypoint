use dh_tms_core::CorePlane;
use dh_tms_format::catalog::MmapContentCatalog;
use dh_tms_format::{read_current_manifest, FormatError};
use dh_tms_grammar::{
    parse_release_name, tokenize_alnum, EpisodeSpec, CT_UNKNOWN, NUM_CONTENT_TYPES,
};
use dh_tms_hashdir::{encode_hex, parse_info_hash_hex, HashDir, InfoHash};
use dh_tms_policy::{PolicyError, PolicyOp, PolicyPlane, PolicyTier, RevokeReason};
use dh_tms_search::l0::L0Doc;
use dh_tms_search::{term_hash, CustomSearchPlane, QueryMode, SearchError};
use dh_tms_swarm::{SwarmError, SwarmPlane, SwarmUpdate};
use serde::{Deserialize, Serialize};
use std::path::Path;
use std::time::{SystemTime, UNIX_EPOCH};

#[derive(Debug)]
pub enum DaemonError {
    BadRequest(String),
    NotFound,
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
    pub now_epoch: Option<u32>,
}

#[derive(Debug, Serialize)]
pub struct LookupResponse {
    pub linked: Vec<DocHit>,
    pub unlinked: Vec<DocHit>,
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
    pub now_epoch: Option<u32>,
}

#[derive(Debug, Serialize)]
pub struct RssResponse {
    pub docs: Vec<DocHit>,
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
    pub now_epoch: Option<u32>,
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
    pub doc_id: u32,
    pub name: String,
    pub content_type: u8,
    #[serde(default)]
    pub resolution: u8,
    #[serde(default)]
    pub entity_id: u32,
    #[serde(default)]
    pub year: u16,
    pub published_epoch: u32,
    pub created_epoch: u32,
    #[serde(default)]
    pub file_paths: Vec<String>,
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
        std::fs::create_dir_all(wal_dir).map_err(|e| DaemonError::Internal(e.to_string()))?;
        let (manifest, gen_dir) = read_current_manifest(db_root)?;
        let gid = manifest.generation_id;
        let now_s = now_epoch_sec();
        let now_m = now_epoch_ms();
        let lease_until = now_m.saturating_add(initial_lease_ttl_ms);

        // Seed writable swarm.chk into wal_dir if not yet present so immutable generation stays untouched!
        let wal_swarm_chk = wal_dir.join("swarm.chk");
        if !wal_swarm_chk.exists() {
            std::fs::copy(gen_dir.join("swarm.chk"), &wal_swarm_chk)
                .map_err(|e| DaemonError::Internal(format!("seed swarm.chk: {e}")))?;
        }

        let core = CorePlane::open(&gen_dir, Some(gid))?;
        let hash_dir = HashDir::open(&gen_dir, Some(gid))?;
        let policy = PolicyPlane::open(
            &gen_dir,
            &wal_dir.join("policy.wal"),
            Some(gid),
            now_s,
            initial_pg_lsn,
            lease_until,
        )?;
        let swarm = SwarmPlane::open(wal_dir, &wal_dir.join("swarm.wal"), Some(gid))?;
        let catalog = MmapContentCatalog::open(&gen_dir.join("catalog.bin"), Some(gid))?;
        let search = CustomSearchPlane::open(&gen_dir, &wal_dir.join("l0.wal"), Some(gid))?;

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

    fn resolve_doc_id(
        &self,
        doc_id: Option<u32>,
        info_hash: Option<&str>,
    ) -> Result<u32, DaemonError> {
        if let Some(d) = doc_id {
            if (d as usize) >= self.core.num_docs {
                return Err(DaemonError::BadRequest(format!("doc_id {d} out of range")));
            }
            return Ok(d);
        }
        let raw = info_hash
            .ok_or_else(|| DaemonError::BadRequest("doc_id or info_hash required".into()))?;
        let ih = parse_info_hash_hex(raw).ok_or_else(|| {
            DaemonError::BadRequest("invalid info_hash hex (expected 40 or 64 hex chars)".into())
        })?;
        self.hash_dir
            .get(&ih)
            .ok_or_else(|| DaemonError::BadRequest("info_hash not found in generation".into()))
    }

    fn hydrate_hit(
        &self,
        doc_id: u32,
        score: f32,
        include_files: bool,
    ) -> Result<DocHit, DaemonError> {
        let mut meta = self.core.metas()[doc_id as usize];
        let l0_ov = self.search.l0.get_doc(doc_id);
        if let Some(ref ov) = l0_ov {
            meta.content_type = ov.content_type;
            meta.resolution = ov.resolution;
            meta.entity_id = ov.entity_id;
            meta.year = ov.year;
            meta.published_epoch = ov.published_epoch;
            meta.created_epoch = ov.created_epoch;
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

        let scored = if let Some(season) = req.tv_season {
            self.search.tvsearch_text(
                &req.query,
                season,
                req.tv_episode,
                &self.policy,
                tier,
                &self.core,
                limit,
                now_s,
                now_m,
            )?
        } else {
            self.search.search_text(
                &req.query,
                mode,
                None,
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
            if !self
                .policy
                .authorize_egress_one(sd.doc_id, tier, now_s, now_m)?
            {
                continue;
            }
            docs.push(self.hydrate_hit(sd.doc_id, sd.score, req.include_files)?);
        }

        let (total_facets, ct_facets, res_facets) = if req.include_facets {
            let (ct, res, tot) = self.search.text_facet_counts(
                &req.query,
                &self.policy,
                tier,
                self.core.facet_bytes(),
                now_s,
                now_m,
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
        let mut titles = req.titles;
        let mut year = req.year;
        if entity_id == 0 && !req.content_id.is_empty() {
            let src = if req.content_source.is_empty() {
                "tmdb"
            } else {
                req.content_source.as_str()
            };
            if let Some(rec) = self
                .catalog
                .find_by_key(req.content_type, src, &req.content_id)?
            {
                entity_id = rec.entity_id;
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
        let (linked_ids, unlinked_ids) = self.search.lookup_entity_with_fallback(
            entity_id,
            &titles,
            req.content_type,
            year,
            season_ep,
            &self.policy,
            tier,
            &self.core,
            limit,
            now_s,
            now_m,
        )?;

        let mut linked = Vec::with_capacity(linked_ids.len());
        for d in linked_ids {
            if self.policy.authorize_egress_one(d, tier, now_s, now_m)? {
                linked.push(self.hydrate_hit(d, 0.0, req.include_files)?);
            }
        }
        let mut unlinked = Vec::with_capacity(unlinked_ids.len());
        for d in unlinked_ids {
            if self.policy.authorize_egress_one(d, tier, now_s, now_m)? {
                unlinked.push(self.hydrate_hit(d, 0.0, req.include_files)?);
            }
        }
        Ok(LookupResponse { linked, unlinked })
    }

    pub fn handle_rss(&self, req: RssRequest) -> Result<RssResponse, DaemonError> {
        let tier = parse_tier(&req.tier)?;
        let limit = req.limit.clamp(1, 500);
        let now_s = self.effective_now_sec(req.now_epoch);
        let now_m = now_epoch_ms();
        self.policy.lease().check(now_m)?;
        let ct_opt = req.content_type.filter(|&c| c != CT_UNKNOWN);
        let doc_ids = self.search.browse_newest(
            ct_opt,
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
            if self.policy.authorize_egress_one(d, tier, now_s, now_m)? {
                docs.push(self.hydrate_hit(d, 0.0, req.include_files)?);
            }
        }
        Ok(RssResponse { docs })
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
        let doc_ids = self.search.search_substring(
            &req.substring,
            &self.policy,
            tier,
            &self.core,
            limit,
            now_s,
            now_m,
        )?;
        let mut docs = Vec::with_capacity(doc_ids.len());
        for d in doc_ids {
            if self.policy.authorize_egress_one(d, tier, now_s, now_m)? {
                docs.push(self.hydrate_hit(d, 0.0, req.include_files)?);
            }
        }
        Ok(RssResponse { docs })
    }

    pub fn handle_hydrate(&self, req: HydrateRequest) -> Result<RssResponse, DaemonError> {
        let tier = parse_tier(&req.tier)?;
        let now_s = self.effective_now_sec(req.now_epoch);
        let now_m = now_epoch_ms();
        self.policy.lease().check(now_m)?;

        let mut targets: Vec<u32> = Vec::with_capacity(req.doc_ids.len() + req.info_hashes.len());
        for d in req.doc_ids {
            if (d as usize) < self.core.num_docs {
                targets.push(d);
            }
        }
        for raw in &req.info_hashes {
            if let Some(ih) = parse_info_hash_hex(raw) {
                if let Some(d) = self.hash_dir.get(&ih) {
                    targets.push(d);
                }
            }
        }
        let mut docs = Vec::with_capacity(targets.len());
        for d in targets {
            if self.policy.authorize_egress_one(d, tier, now_s, now_m)? {
                docs.push(self.hydrate_hit(d, 0.0, true)?);
            }
        }
        Ok(RssResponse { docs })
    }

    pub fn handle_hash_lookup(
        &self,
        req: HashLookupRequest,
    ) -> Result<HashLookupResponse, DaemonError> {
        let tier = parse_tier(&req.tier)?;
        let now_s = self.effective_now_sec(req.now_epoch);
        let now_m = now_epoch_ms();
        self.policy.lease().check(now_m)?;

        let ih = parse_info_hash_hex(&req.info_hash)
            .ok_or_else(|| DaemonError::BadRequest("invalid info_hash hex".into()))?;
        let Some(doc_id) = self.hash_dir.get(&ih) else {
            return Ok(HashLookupResponse {
                found: false,
                doc_id: None,
                info_hash_v1: None,
                info_hash_v2: None,
                magnet_xt: None,
                eligible: false,
            });
        };
        let eligible = self
            .policy
            .authorize_egress_one(doc_id, tier, now_s, now_m)?;
        let (v1, v2) = self.hash_dir.get_hashes_for_doc(doc_id);
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
        for item in req.docs {
            if (item.doc_id as usize) >= self.core.num_docs {
                return Err(DaemonError::BadRequest(format!(
                    "doc_id {} out of range (num_docs={})",
                    item.doc_id, self.core.num_docs
                )));
            }
            let pr = parse_release_name(&item.name);
            let mut terms: Vec<(u64, u16)> = Vec::new();
            for t in &pr.title_tokens {
                terms.push((term_hash(t, self.search.term_seed), 1500));
            }
            for t in &pr.raw_tokens {
                terms.push((term_hash(t, self.search.term_seed), 600));
            }
            for fp in item.file_paths.iter().take(64) {
                for t in tokenize_alnum(fp) {
                    terms.push((term_hash(&t, self.search.term_seed), 250));
                }
            }
            let eps: Vec<EpisodeSpec> = pr.episodes;
            batch.push(L0Doc {
                doc_id: item.doc_id,
                name: item.name,
                content_type: item.content_type,
                resolution: if item.resolution != 0 {
                    item.resolution
                } else {
                    pr.resolution
                },
                entity_id: item.entity_id,
                year: if item.year != 0 { item.year } else { pr.year },
                published_epoch: item.published_epoch,
                created_epoch: item.created_epoch,
                episodes: eps,
                strict_title_hashes: pr.strict_title_hashes,
                family_hashes: pr.family_hashes,
                terms,
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
