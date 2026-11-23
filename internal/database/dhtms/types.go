package dhtms

import "fmt"

// PolicyTier selects the serving visibility predicate enforced by dh-tmsd.
type PolicyTier string

const (
	TierBase    PolicyTier = "base"
	TierPub     PolicyTier = "pub"
	TierPayload PolicyTier = "payload"
)

// QueryMode selects lexical matching semantics for /v1/search.
type QueryMode string

const (
	ModeAnd    QueryMode = "and"
	ModeOr     QueryMode = "or"
	ModePhrase QueryMode = "phrase"
)

// RevokeReason identifies a fail-closed visibility denial cause.
type RevokeReason string

const (
	ReasonQuarantine  RevokeReason = "quarantine"
	ReasonPrivacy     RevokeReason = "privacy"
	ReasonExcluded    RevokeReason = "excluded"
	ReasonHold        RevokeReason = "hold"
	ReasonAdult       RevokeReason = "adult"
	ReasonNameBlocked RevokeReason = "name_blocked"
	ReasonCsamDelete  RevokeReason = "csam_delete"
)

// HealthResponse mirrors GET /v1/health.
type HealthResponse struct {
	Status           string `json:"status"`
	Engine           string `json:"engine"`
	GenerationID     uint64 `json:"generation_id"`
	NumDocs          int    `json:"num_docs"`
	PGLSN            uint64 `json:"pg_lsn"`
	LeaseActive      bool   `json:"lease_active"`
	LeaseRemainingMS uint64 `json:"lease_remaining_ms"`
	L0Count          int    `json:"l0_count"`
	BaseEligible     uint64 `json:"base_eligible"`
	PubEligible      uint64 `json:"pub_eligible"`
	PayloadEligible  uint64 `json:"payload_eligible"`
}

// FileEntry is a hydrated file entry within a torrent's file tree.
type FileEntry struct {
	Path      string `json:"path"`
	Size      uint64 `json:"size"`
	Extension string `json:"extension"`
}

// DocHit represents a single authorized torrent hit returned by dh-tmsd.
type DocHit struct {
	DocID           uint32      `json:"doc_id"`
	Score           float32     `json:"score"`
	InfoHashV1      string      `json:"info_hash_v1,omitempty"`
	InfoHashV2      string      `json:"info_hash_v2,omitempty"`
	MagnetXT        string      `json:"magnet_xt"`
	Name            string      `json:"name"`
	SizeBytes       uint64      `json:"size_bytes"`
	ContentType     uint8       `json:"content_type"`
	Resolution      uint8       `json:"resolution"`
	EntityID        uint32      `json:"entity_id"`
	Year            uint16      `json:"year"`
	PublishedEpoch  uint32      `json:"published_epoch"`
	CreatedEpoch    uint32      `json:"created_epoch"`
	FilesCount      uint32      `json:"files_count"`
	Seeders         *uint32     `json:"seeders,omitempty"`
	Leechers        *uint32     `json:"leechers,omitempty"`
	PeakSeeders     uint16      `json:"peak_seeders"`
	CheckedEpochMin uint32      `json:"checked_epoch_min"`
	Files           []FileEntry `json:"files,omitempty"`
}

// SearchRequest mirrors POST /v1/search.
type SearchRequest struct {
	Query         string     `json:"query"`
	Mode          QueryMode  `json:"mode,omitempty"`
	Tier          PolicyTier `json:"tier,omitempty"`
	Limit         int        `json:"limit,omitempty"`
	TVSeason      *uint16    `json:"tv_season,omitempty"`
	TVEpisode     *uint16    `json:"tv_episode,omitempty"`
	IncludeFacets bool       `json:"include_facets,omitempty"`
	IncludeFiles  bool       `json:"include_files,omitempty"`
	NowEpoch      *uint32    `json:"now_epoch,omitempty"`
}

// SearchResponse mirrors the JSON response of POST /v1/search.
type SearchResponse struct {
	Docs        []DocHit `json:"docs"`
	TotalFacets *uint64  `json:"total_facets,omitempty"`
	CTFacets    []uint64 `json:"ct_facets,omitempty"`
	ResFacets   []uint64 `json:"res_facets,omitempty"`
}

// LookupRequest mirrors POST /v1/lookup (entity ID + unlinked fallback).
type LookupRequest struct {
	EntityID      uint32     `json:"entity_id,omitempty"`
	ContentType   uint8      `json:"content_type,omitempty"`
	ContentSource string     `json:"content_source,omitempty"`
	ContentID     string     `json:"content_id,omitempty"`
	Titles        []string   `json:"titles,omitempty"`
	Year          uint16     `json:"year,omitempty"`
	Season        *uint16    `json:"season,omitempty"`
	Episode       *uint16    `json:"episode,omitempty"`
	Tier          PolicyTier `json:"tier,omitempty"`
	Limit         int        `json:"limit,omitempty"`
	IncludeFiles  bool       `json:"include_files,omitempty"`
	NowEpoch      *uint32    `json:"now_epoch,omitempty"`
}

// LookupResponse separates linked entity hits from unlinked fallback hits.
type LookupResponse struct {
	Linked   []DocHit `json:"linked"`
	Unlinked []DocHit `json:"unlinked"`
}

// RSSRequest mirrors POST /v1/rss.
type RSSRequest struct {
	ContentType  *uint8     `json:"content_type,omitempty"`
	Tier         PolicyTier `json:"tier,omitempty"`
	Limit        int        `json:"limit,omitempty"`
	Offset       int        `json:"offset,omitempty"`
	IncludeFiles bool       `json:"include_files,omitempty"`
	NowEpoch     *uint32    `json:"now_epoch,omitempty"`
}

// RSSResponse mirrors POST /v1/rss, POST /v1/substring, and POST /v1/hydrate.
type RSSResponse struct {
	Docs []DocHit `json:"docs"`
}

// DashstatsRequest mirrors POST /v1/dashstats.
type DashstatsRequest struct {
	Tier     PolicyTier `json:"tier,omitempty"`
	NowEpoch *uint32    `json:"now_epoch,omitempty"`
}

// DashstatsResponse mirrors GET/POST /v1/dashstats.
type DashstatsResponse struct {
	Movies30d       uint64 `json:"movies_30d"`
	MoviesLinked30d uint64 `json:"movies_linked_30d"`
	TV30d           uint64 `json:"tv_30d"`
	TVLinked30d     uint64 `json:"tv_linked_30d"`
}

// SubstringRequest mirrors POST /v1/substring.
type SubstringRequest struct {
	Substring    string     `json:"substring"`
	Tier         PolicyTier `json:"tier,omitempty"`
	Limit        int        `json:"limit,omitempty"`
	IncludeFiles bool       `json:"include_files,omitempty"`
	NowEpoch     *uint32    `json:"now_epoch,omitempty"`
}

// HydrateRequest mirrors POST /v1/hydrate.
type HydrateRequest struct {
	DocIDs     []uint32   `json:"doc_ids,omitempty"`
	InfoHashes []string   `json:"info_hashes,omitempty"`
	Tier       PolicyTier `json:"tier,omitempty"`
	NowEpoch   *uint32    `json:"now_epoch,omitempty"`
}

// HashLookupRequest mirrors POST /v1/hash/lookup.
type HashLookupRequest struct {
	InfoHash string     `json:"info_hash"`
	Tier     PolicyTier `json:"tier,omitempty"`
	NowEpoch *uint32    `json:"now_epoch,omitempty"`
}

// HashLookupResponse mirrors POST /v1/hash/lookup.
type HashLookupResponse struct {
	Found      bool    `json:"found"`
	DocID      *uint32 `json:"doc_id,omitempty"`
	InfoHashV1 string  `json:"info_hash_v1,omitempty"`
	InfoHashV2 string  `json:"info_hash_v2,omitempty"`
	MagnetXT   string  `json:"magnet_xt,omitempty"`
	Eligible   bool    `json:"eligible"`
}

// PolicyRevokeRequest mirrors POST /v1/policy/revoke.
type PolicyRevokeRequest struct {
	DocID    *uint32      `json:"doc_id,omitempty"`
	InfoHash string       `json:"info_hash,omitempty"`
	Reason   RevokeReason `json:"reason"`
	PGLSN    uint64       `json:"pg_lsn"`
	Sync     bool         `json:"sync"`
}

// PolicyRevokeResponse mirrors POST /v1/policy/revoke.
type PolicyRevokeResponse struct {
	OK    bool   `json:"ok"`
	DocID uint32 `json:"doc_id"`
	PGLSN uint64 `json:"pg_lsn"`
}

// PolicyBatchOp is a single mutation inside PolicyBatchRequest.
type PolicyBatchOp struct {
	Op           string       `json:"op"`
	DocID        *uint32      `json:"doc_id,omitempty"`
	InfoHash     string       `json:"info_hash,omitempty"`
	Reason       RevokeReason `json:"reason,omitempty"`
	ExpiresEpoch uint32       `json:"expires_epoch,omitempty"`
}

// PolicyBatchRequest mirrors POST /v1/policy/batch.
type PolicyBatchRequest struct {
	Ops             []PolicyBatchOp `json:"ops"`
	PGLSN           uint64          `json:"pg_lsn"`
	Sync            bool            `json:"sync"`
	RebuildSnapshot bool            `json:"rebuild_snapshot"`
	NowEpoch        *uint32         `json:"now_epoch,omitempty"`
}

// PolicyBatchResponse mirrors POST /v1/policy/batch.
type PolicyBatchResponse struct {
	OK      bool   `json:"ok"`
	Applied int    `json:"applied"`
	PGLSN   uint64 `json:"pg_lsn"`
}

// PolicyLeaseRequest mirrors POST /v1/policy/lease.
type PolicyLeaseRequest struct {
	PGLSN uint64 `json:"pg_lsn"`
	TTLMS uint64 `json:"ttl_ms"`
}

// PolicyLeaseResponse mirrors POST /v1/policy/lease.
type PolicyLeaseResponse struct {
	OK           bool   `json:"ok"`
	PGLSN        uint64 `json:"pg_lsn"`
	LeaseUntilMS uint64 `json:"lease_until_ms"`
}

// SwarmUpdateItem is a single tracker scrape result for POST /v1/swarm/update.
type SwarmUpdateItem struct {
	DocID           *uint32 `json:"doc_id,omitempty"`
	InfoHash        string  `json:"info_hash,omitempty"`
	Seeders         *uint32 `json:"seeders,omitempty"`
	Leechers        *uint32 `json:"leechers,omitempty"`
	CheckedEpochMin uint32  `json:"checked_epoch_min"`
	TrackerID       uint8   `json:"tracker_id,omitempty"`
}

// SwarmUpdateRequest mirrors POST /v1/swarm/update.
type SwarmUpdateRequest struct {
	Updates []SwarmUpdateItem `json:"updates"`
	Sync    bool              `json:"sync"`
}

// SwarmUpdateResponse mirrors POST /v1/swarm/update.
type SwarmUpdateResponse struct {
	OK      bool   `json:"ok"`
	Seq     uint64 `json:"seq"`
	Applied int    `json:"applied"`
}

// SwarmSelectStaleRequest mirrors POST /v1/swarm/select-stale.
type SwarmSelectStaleRequest struct {
	Limit       int     `json:"limit,omitempty"`
	NowEpochMin *uint32 `json:"now_epoch_min,omitempty"`
	MinAgeMin   uint32  `json:"min_age_min,omitempty"`
}

// StaleSwarmItem identifies a stale torrent due for re-scrape.
type StaleSwarmItem struct {
	DocID           uint32 `json:"doc_id"`
	InfoHashV1      string `json:"info_hash_v1"`
	CheckedEpochMin uint32 `json:"checked_epoch_min"`
}

// SwarmSelectStaleResponse mirrors POST /v1/swarm/select-stale.
type SwarmSelectStaleResponse struct {
	Items []StaleSwarmItem `json:"items"`
}

// SwarmCheckpointResponse mirrors POST /v1/swarm/checkpoint.
type SwarmCheckpointResponse struct {
	OK            bool   `json:"ok"`
	CheckpointSeq uint64 `json:"checkpoint_seq"`
}

// L0UpsertItem describes a live metadata update overlaid on the active generation.
type L0UpsertItem struct {
	DocID          uint32   `json:"doc_id"`
	Name           string   `json:"name"`
	ContentType    uint8    `json:"content_type"`
	Resolution     uint8    `json:"resolution,omitempty"`
	EntityID       uint32   `json:"entity_id,omitempty"`
	Year           uint16   `json:"year,omitempty"`
	PublishedEpoch uint32   `json:"published_epoch"`
	CreatedEpoch   uint32   `json:"created_epoch"`
	FilePaths      []string `json:"file_paths,omitempty"`
}

// L0UpsertRequest mirrors POST /v1/l0/upsert.
type L0UpsertRequest struct {
	Docs []L0UpsertItem `json:"docs"`
	Sync bool           `json:"sync"`
}

// L0UpsertResponse mirrors POST /v1/l0/upsert.
type L0UpsertResponse struct {
	OK       bool `json:"ok"`
	Upserted int  `json:"upserted"`
	L0Count  int  `json:"l0_count"`
}

// APIError captures structured non-2xx responses from dh-tmsd.
type APIError struct {
	StatusCode   int    `json:"-"`
	Code         string `json:"code"`
	Message      string `json:"error"`
	NowMS        uint64 `json:"now_ms,omitempty"`
	ValidUntilMS uint64 `json:"valid_until_ms,omitempty"`
	PGLSN        uint64 `json:"pg_lsn,omitempty"`
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("dh-tmsd http %d (%s): %s", e.StatusCode, e.Code, e.Message)
	}
	return fmt.Sprintf("dh-tmsd http %d: %s", e.StatusCode, e.Message)
}

// BuildHeaderLine is the leading NDJSON record streamed to POST /v1/generation/build.
type BuildHeaderLine struct {
	Kind       string `json:"kind"`
	PGLSN      uint64 `json:"pg_lsn"`
	LeaseTTLMS uint64 `json:"lease_ttl_ms"`
	PGLSNText  string `json:"pg_lsn_text,omitempty"`
}

// BuildCatalogLine is a content catalog NDJSON record streamed to POST /v1/generation/build.
type BuildCatalogLine struct {
	Kind           string      `json:"kind"`
	EntityID       uint32      `json:"entity_id"`
	ContentType    uint8       `json:"content_type"`
	Source         string      `json:"source"`
	ID             string      `json:"id"`
	Title          string      `json:"title"`
	OriginalTitle  string      `json:"original_title,omitempty"`
	Year           uint16      `json:"year,omitempty"`
	Adult          bool        `json:"adult,omitempty"`
	AltIdentifiers [][2]string `json:"alt_identifiers,omitempty"`
}

// BuildFileInput is a single file entry inside BuildDocLine.
type BuildFileInput struct {
	Path      string `json:"path"`
	Size      uint64 `json:"size,omitempty"`
	Extension string `json:"extension,omitempty"`
}

// BuildDocLine is a torrent document NDJSON record streamed to POST /v1/generation/build.
type BuildDocLine struct {
	Kind                string           `json:"kind"`
	InfoHashV1          string           `json:"info_hash_v1,omitempty"`
	InfoHashV2          string           `json:"info_hash_v2,omitempty"`
	Name                string           `json:"name"`
	SizeBytes           uint64           `json:"size_bytes"`
	ContentType         uint8            `json:"content_type"`
	EntityID            uint32           `json:"entity_id,omitempty"`
	Year                uint16           `json:"year,omitempty"`
	PublishedEpoch      uint32           `json:"published_epoch"`
	CreatedEpoch        uint32           `json:"created_epoch"`
	Resolution          uint8            `json:"resolution,omitempty"`
	Files               []BuildFileInput `json:"files,omitempty"`
	IsPrivate           bool             `json:"is_private,omitempty"`
	Quarantined         bool             `json:"quarantined,omitempty"`
	PrivacyTainted      bool             `json:"privacy_tainted,omitempty"`
	Excluded            bool             `json:"excluded,omitempty"`
	Held                bool             `json:"held,omitempty"`
	AuthoritativePolicy bool             `json:"authoritative_policy,omitempty"`
	Adult               bool             `json:"adult,omitempty"`
	NameBlocked         bool             `json:"name_blocked,omitempty"`
	PubApproved         bool             `json:"pub_approved,omitempty"`
	PayloadExpiresEpoch uint32           `json:"payload_expires_epoch,omitempty"`
	Seeders             int32            `json:"seeders"`
	Leechers            int32            `json:"leechers"`
	CheckedEpochMin     uint32           `json:"checked_epoch_min,omitempty"`
}

// BuildGenerationResponse mirrors POST /v1/generation/build.
type BuildGenerationResponse struct {
	OK           bool   `json:"ok"`
	GenerationID uint64 `json:"generation_id"`
	NumDocs      int    `json:"num_docs"`
	PGLSN        uint64 `json:"pg_lsn"`
}

// ReloadRequest mirrors POST /v1/reload.
type ReloadRequest struct {
	PGLSN      *uint64 `json:"pg_lsn,omitempty"`
	LeaseTTLMS uint64  `json:"lease_ttl_ms,omitempty"`
}

// ReloadResponse mirrors POST /v1/reload.
type ReloadResponse struct {
	OK           bool   `json:"ok"`
	GenerationID uint64 `json:"generation_id"`
	NumDocs      int    `json:"num_docs"`
	PGLSN        uint64 `json:"pg_lsn"`
}
