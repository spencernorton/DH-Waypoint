package leanmode

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencernorton/DH-Waypoint/internal/catalogueguard"
	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/model"
)

// ErrDestructiveUnconfirmed refuses physical pruning unless prune.confirm_destructive=true.
var ErrDestructiveUnconfirmed = errors.New("leanmode: mode=prune requires prune.confirm_destructive=true")

// ErrPruneCircuitBreaker fires when a single cycle would exceed prune.max_delete_fraction_per_cycle.
var ErrPruneCircuitBreaker = errors.New("leanmode: prune candidate fraction exceeds max_delete_fraction_per_cycle")

var (
	videoExtensions = map[string]bool{
		"mkv": true, "mp4": true, "avi": true, "m4v": true, "mov": true,
		"wmv": true, "ts": true, "m2ts": true, "webm": true, "mpg": true, "mpeg": true,
	}
	subtitleExtensions = map[string]bool{
		"srt": true, "ass": true, "ssa": true, "sub": true, "idx": true, "vtt": true, "sup": true,
	}
	clutterExtensions = map[string]bool{
		"exe": true, "lnk": true, "bat": true, "cmd": true, "scr": true, "msi": true, "com": true,
	}
)

// Store encapsulates PostgreSQL persistence for lean_winners, lean_tombstones, and lean_state.
type Store struct {
	pool lazy.Lazy[*pgxpool.Pool]

	mu            sync.RWMutex
	activeProfile *ProfileConfig
	activeDigest  string
	serveGlobally bool
}

func NewStore(pool lazy.Lazy[*pgxpool.Pool]) *Store {
	return &Store{pool: pool}
}

// SetActiveProfile publishes the validated runtime profile for fast lock-protected reads
// by crawler/importer tombstone filters and global serving hooks.
func (s *Store) SetActiveProfile(cfg *ProfileConfig, serveGlobally bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cfg == nil {
		s.activeProfile = nil
		s.activeDigest = ""
		s.serveGlobally = false
		return
	}
	cp := *cfg
	s.activeProfile = &cp
	s.activeDigest = cp.ConfigDigest()
	s.serveGlobally = serveGlobally
}

// ActiveServingProfile returns the global lean serving criteria if LEAN MODE is
// enabled in serve/prune mode with serve_globally=true.
func (s *Store) ActiveServingProfile() (profileID, unlinkedPolicy, nonVideoPolicy string, active bool) {
	if s == nil {
		return "", "", "", false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.activeProfile == nil || !s.serveGlobally {
		return "", "", "", false
	}
	if s.activeProfile.Mode != ModeServe && s.activeProfile.Mode != ModePrune {
		return "", "", "", false
	}
	return s.activeProfile.ID, s.activeProfile.Scope.UnlinkedPolicy, s.activeProfile.Scope.NonVideoPolicy, true
}

// ActivePruneIdentity returns (profileID, configDigest, true) only when LEAN MODE
// is active in prune mode with confirmed destructive pruning.
func (s *Store) ActivePruneIdentity() (string, string, bool) {
	if s == nil {
		return "", "", false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.activeProfile == nil || s.activeProfile.Mode != ModePrune || !s.activeProfile.Prune.ConfirmDestructive {
		return "", "", false
	}
	return s.activeProfile.ID, s.activeDigest, true
}

// ServingWinnersCriteria constructs the SQL predicate narrowing torrent_contents
// to rank-1 LEAN MODE winners for profileID. Always AND'd on top of serving.Policy;
// it can only narrow visibility, never bypass publication, payload, private, or adult gates.
func ServingWinnersCriteria(profileID, unlinkedPolicy, nonVideoPolicy string) (query.DBCriteria, error) {
	if !ProfileIDPattern.MatchString(profileID) {
		return query.DBCriteria{}, fmt.Errorf("leanmode: invalid profile id %q", profileID)
	}
	winnerSubquery := "torrent_contents.info_hash IN (SELECT lw.info_hash FROM lean_winners lw WHERE lw.profile_id = ? AND lw.rank = 1)"
	if unlinkedPolicy == UnlinkedExcludeFromServe && nonVideoPolicy == NonVideoExcludeFromServe {
		return query.DBCriteria{
			SQL:  winnerSubquery,
			Args: []any{profileID},
		}, nil
	}
	var passClauses []string
	if unlinkedPolicy == "" || unlinkedPolicy == UnlinkedPassThrough {
		passClauses = append(passClauses, "torrent_contents.content_id IS NULL")
	}
	if nonVideoPolicy == "" || nonVideoPolicy == NonVideoPassThrough {
		passClauses = append(passClauses, "(torrent_contents.content_type IS NULL OR torrent_contents.content_type NOT IN ('movie','tv_show'))")
	}
	if len(passClauses) == 0 {
		return query.DBCriteria{
			SQL:  winnerSubquery,
			Args: []any{profileID},
		}, nil
	}
	return query.DBCriteria{
		SQL:  "(" + winnerSubquery + " OR " + strings.Join(passClauses, " OR ") + ")",
		Args: []any{profileID},
	}, nil
}

// TombstonedSet checks raw info_hashes against active config-digest-bound lean_tombstones.
// Returns nil with zero DB queries when LEAN MODE pruning is inactive.
func (s *Store) TombstonedSet(ctx context.Context, infoHashes [][]byte) (map[string]struct{}, error) {
	if s == nil || len(infoHashes) == 0 {
		return nil, nil
	}
	profileID, digest, active := s.ActivePruneIdentity()
	if !active {
		return nil, nil
	}
	return s.TombstonedSetForProfile(ctx, profileID, digest, infoHashes)
}

// TombstonedSetForProfile runs a single PK-indexed lookup on lean_tombstones.
func (s *Store) TombstonedSetForProfile(ctx context.Context, profileID, configDigest string, infoHashes [][]byte) (map[string]struct{}, error) {
	if len(infoHashes) == 0 {
		return nil, nil
	}
	pool, err := s.pool.Get()
	if err != nil {
		return nil, fmt.Errorf("leanmode: acquire pool: %w", err)
	}
	rows, err := pool.Query(ctx, `
SELECT info_hash FROM lean_tombstones
WHERE info_hash = ANY($1)
  AND profile_id = $2
  AND config_digest = $3
  AND (expires_at IS NULL OR expires_at > clock_timestamp())`,
		infoHashes, profileID, configDigest)
	if err != nil {
		return nil, fmt.Errorf("leanmode: tombstone lookup: %w", err)
	}
	defer rows.Close()
	out := make(map[string]struct{})
	for rows.Next() {
		var h []byte
		if err := rows.Scan(&h); err != nil {
			return nil, fmt.Errorf("leanmode: tombstone scan: %w", err)
		}
		out[hex.EncodeToString(h)] = struct{}{}
	}
	return out, rows.Err()
}

// ReconcileOptions bounds a single reconciliation pass.
type ReconcileOptions struct {
	TitleBatchSize int
	MaxTitles      int // 0 = process entire catalog
	ResetCursor    bool
	Now            time.Time
}

// ReconcileReport summarizes a reconciliation + optional prune run.
type ReconcileReport struct {
	ProfileID          string         `json:"profile_id"`
	Mode               string         `json:"mode"`
	ConfigDigest       string         `json:"config_digest"`
	TitlesEvaluated    int            `json:"titles_evaluated"`
	SlotsEvaluated     int            `json:"slots_evaluated"`
	CandidatesEvaluated int           `json:"candidates_evaluated"`
	WinnersKept        int            `json:"winners_kept"`
	RunnerUpsKept      int            `json:"runner_ups_kept"`
	LosersCount        int            `json:"losers_count"`
	DisqualifiedCount  int            `json:"disqualified_count"`
	DisqualByReason    map[string]int `json:"disqualified_by_reason"`
	WouldPruneCount    int            `json:"would_prune_count"`
	WouldReclaimBytes  uint64         `json:"would_reclaim_bytes"`
	PrunedCount        int            `json:"pruned_count"`
	PrunedBytes        uint64         `json:"pruned_bytes"`
	ProtectedSkipped   int            `json:"protected_skipped"`
	TooYoungSkipped    int            `json:"too_young_skipped"`
	TombstonesWritten  int            `json:"tombstones_written"`
	RevivedSlots       int            `json:"revived_slots"`
	HysteresisHolds    int            `json:"hysteresis_holds"`
	DurationMs         float64        `json:"duration_ms"`
	TitlesPerSec       float64        `json:"titles_per_sec"`
	SlotsPerSec        float64        `json:"slots_per_sec"`
}

type titleKey struct {
	ContentType   string
	ContentSource string
	ContentID     string
}

func (k titleKey) cursor() string {
	return k.ContentType + "\x1f" + k.ContentSource + "\x1f" + k.ContentID
}

// Reconcile evaluates linked movie/TV titles against cfg, atomically updates
// lean_winners, and — only when cfg.Mode == "prune" and confirm_destructive == true —
// executes bounded tombstone pruning while protecting all operator/private/evidence rows.
func (s *Store) Reconcile(ctx context.Context, cfg ProfileConfig, opts ReconcileOptions) (ReconcileReport, error) {
	if err := cfg.Validate(); err != nil {
		return ReconcileReport{}, err
	}
	if cfg.Mode == ModePrune && !cfg.Prune.ConfirmDestructive {
		return ReconcileReport{}, ErrDestructiveUnconfirmed
	}
	pool, err := s.pool.Get()
	if err != nil {
		return ReconcileReport{}, fmt.Errorf("leanmode: acquire pool: %w", err)
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	batchSize := opts.TitleBatchSize
	if batchSize <= 0 {
		batchSize = 200
	}
	digest := cfg.ConfigDigest()
	start := time.Now()
	report := ReconcileReport{
		ProfileID:       cfg.ID,
		Mode:            cfg.Mode,
		ConfigDigest:    digest,
		DisqualByReason: make(map[string]int),
	}

	// Clean expired tombstones and any tombstones from superseded config digests for this profile.
	if _, err := pool.Exec(ctx, `
DELETE FROM lean_tombstones
WHERE profile_id = $1
  AND (config_digest <> $2 OR (expires_at IS NOT NULL AND expires_at <= $3))`,
		cfg.ID, digest, now); err != nil {
		return report, fmt.Errorf("leanmode: sweep stale tombstones: %w", err)
	}

	// Load cursor unless ResetCursor or MaxTitles==0 full sweep.
	var lastCursor string
	if !opts.ResetCursor && opts.MaxTitles > 0 {
		_ = pool.QueryRow(ctx, `SELECT last_content_cursor FROM lean_state WHERE profile_id = $1`, cfg.ID).Scan(&lastCursor)
	}

	type pruneCandidate struct {
		InfoHash   []byte
		SlotKey    string
		ReasonCode string
		SizeBytes  uint64
		CreatedAt  time.Time
		Protected  bool
	}
	var pendingPrunes []pruneCandidate
	var reviveSlotKeys []string

	lastType, lastSource, lastID := splitCursor(lastCursor)
	for {
		remaining := batchSize
		if opts.MaxTitles > 0 {
			left := opts.MaxTitles - report.TitlesEvaluated
			if left <= 0 {
				break
			}
			if left < remaining {
				remaining = left
			}
		}

		titles, err := fetchNextTitles(ctx, pool, cfg.Scope.ContentTypes, lastType, lastSource, lastID, remaining)
		if err != nil {
			return report, fmt.Errorf("leanmode: fetch titles: %w", err)
		}
		if len(titles) == 0 {
			// Wrap cursor to beginning for next incremental cycle.
			lastCursor = ""
			break
		}

		for _, tk := range titles {
			cands, err := loadTitleCandidates(ctx, pool, cfg, tk, now)
			if err != nil {
				return report, fmt.Errorf("leanmode: load candidates %s/%s/%s: %w", tk.ContentType, tk.ContentSource, tk.ContentID, err)
			}
			incumbents, err := loadTitleIncumbents(ctx, pool, cfg.ID, tk)
			if err != nil {
				return report, fmt.Errorf("leanmode: load incumbents: %w", err)
			}
			td := ResolveTitle(cfg, tk.ContentType, tk.ContentSource, tk.ContentID, cands, incumbents, now)

			if err := persistTitleWinners(ctx, pool, cfg.ID, digest, td, incumbents, now); err != nil {
				return report, fmt.Errorf("leanmode: persist winners %s/%s/%s: %w", tk.ContentType, tk.ContentSource, tk.ContentID, err)
			}

			report.TitlesEvaluated++
			report.CandidatesEvaluated += len(cands)
			for _, sd := range td.Slots {
				report.SlotsEvaluated++
				if sd.HysteresisHeld {
					report.HysteresisHolds++
				}
				if sd.NeedsSlotRevival && cfg.Prune.ReviveSlotOnWinnerDeath {
					reviveSlotKeys = append(reviveSlotKeys, sd.SlotKey)
				}
				for _, w := range sd.Winners {
					if w.KeptRank == 1 {
						report.WinnersKept++
					} else if w.KeptRank > 1 {
						report.RunnerUpsKept++
					}
				}
				for _, l := range sd.Losers {
					report.LosersCount++
					pendingPrunes = append(pendingPrunes, pruneCandidate{
						InfoHash:   l.Candidate.InfoHash,
						SlotKey:    sd.SlotKey,
						ReasonCode: l.PruneReason,
						SizeBytes:  l.Candidate.SizeBytes,
						CreatedAt:  l.Candidate.CreatedAt,
						Protected:  l.Candidate.Protected,
					})
				}
				for _, dq := range sd.Disqualified {
					report.DisqualifiedCount++
					if len(dq.DisqualReasons) > 0 {
						report.DisqualByReason[dq.DisqualReasons[0]]++
					}
					pendingPrunes = append(pendingPrunes, pruneCandidate{
						InfoHash:   dq.Candidate.InfoHash,
						SlotKey:    sd.SlotKey,
						ReasonCode: dq.PruneReason,
						SizeBytes:  dq.Candidate.SizeBytes,
						CreatedAt:  dq.Candidate.CreatedAt,
						Protected:  dq.Candidate.Protected,
					})
				}
			}

			lastType, lastSource, lastID = tk.ContentType, tk.ContentSource, tk.ContentID
			lastCursor = tk.cursor()
		}

		if len(titles) < remaining {
			lastCursor = ""
			break
		}
	}

	// Slot revival: clear tombstones for slots whose winners died.
	if len(reviveSlotKeys) > 0 {
		tag, err := pool.Exec(ctx, `DELETE FROM lean_tombstones WHERE profile_id = $1 AND slot_key = ANY($2)`, cfg.ID, reviveSlotKeys)
		if err != nil {
			return report, fmt.Errorf("leanmode: revive slots: %w", err)
		}
		if tag.RowsAffected() > 0 {
			report.RevivedSlots = len(reviveSlotKeys)
		}
	}

	// Deduplicate prune candidates by info_hash and filter out protected/young/winner rows.
	seenPrune := make(map[string]struct{}, len(pendingPrunes))
	eligiblePrunes := make([]pruneCandidate, 0, len(pendingPrunes))
	minAge := cfg.Prune.MinAge.Std()
	for _, pc := range pendingPrunes {
		k := hex.EncodeToString(pc.InfoHash)
		if _, dup := seenPrune[k]; dup {
			continue
		}
		seenPrune[k] = struct{}{}
		if pc.Protected {
			report.ProtectedSkipped++
			continue
		}
		if minAge > 0 && now.Sub(pc.CreatedAt) < minAge {
			report.TooYoungSkipped++
			continue
		}
		eligiblePrunes = append(eligiblePrunes, pc)
		report.WouldPruneCount++
		report.WouldReclaimBytes += pc.SizeBytes
	}

	// Phase 3: Opt-in physical pruning with circuit breaker & protected-row re-verification.
	if cfg.Mode == ModePrune && len(eligiblePrunes) > 0 {
		if report.CandidatesEvaluated > 0 {
			frac := float64(len(eligiblePrunes)) / float64(report.CandidatesEvaluated)
			if frac > cfg.Prune.MaxDeleteFractionPerCycle && cfg.Prune.MaxDeleteFractionPerCycle < 1.0 {
				return report, fmt.Errorf("%w: %.2f%% > %.2f%%", ErrPruneCircuitBreaker, frac*100, cfg.Prune.MaxDeleteFractionPerCycle*100)
			}
		}
		var expAt *time.Time
		if cfg.Prune.TombstoneTTL.Std() > 0 {
			t := now.Add(cfg.Prune.TombstoneTTL.Std())
			expAt = &t
		}
		chunkSize := cfg.Prune.BatchSize
		for startIdx := 0; startIdx < len(eligiblePrunes); startIdx += chunkSize {
			endIdx := startIdx + chunkSize
			if endIdx > len(eligiblePrunes) {
				endIdx = len(eligiblePrunes)
			}
			chunk := eligiblePrunes[startIdx:endIdx]
			hashes := make([][]byte, len(chunk))
			slots := make([]string, len(chunk))
			reasons := make([]string, len(chunk))
			sizeByHash := make(map[string]uint64, len(chunk))
			for i, item := range chunk {
				hashes[i] = item.InfoHash
				slots[i] = item.SlotKey
				rc := item.ReasonCode
				if len(rc) > 64 {
					rc = rc[:64]
				}
				if rc == "" {
					rc = "dominated"
				}
				reasons[i] = rc
				sizeByHash[hex.EncodeToString(item.InfoHash)] = item.SizeBytes
			}

			deletedHashes, err := pruneChunkTx(ctx, pool, cfg.ID, digest, hashes, slots, reasons, expAt)
			if err != nil {
				return report, fmt.Errorf("leanmode: prune chunk: %w", err)
			}
			for _, dh := range deletedHashes {
				report.PrunedCount++
				report.TombstonesWritten++
				report.PrunedBytes += sizeByHash[hex.EncodeToString(dh)]
			}
		}
	}

	// Persist lean_state summary.
	if _, err := pool.Exec(ctx, `
INSERT INTO lean_state (profile_id, mode, config_digest, last_content_cursor, slots_evaluated, winners_kept, runner_ups_kept, pruned_total, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, clock_timestamp())
ON CONFLICT (profile_id) DO UPDATE SET
  mode = EXCLUDED.mode,
  config_digest = EXCLUDED.config_digest,
  last_content_cursor = EXCLUDED.last_content_cursor,
  slots_evaluated = EXCLUDED.slots_evaluated,
  winners_kept = EXCLUDED.winners_kept,
  runner_ups_kept = EXCLUDED.runner_ups_kept,
  pruned_total = lean_state.pruned_total + EXCLUDED.pruned_total,
  updated_at = clock_timestamp()`,
		cfg.ID, cfg.Mode, digest, lastCursor, report.SlotsEvaluated, report.WinnersKept, report.RunnerUpsKept, report.PrunedCount); err != nil {
		return report, fmt.Errorf("leanmode: update state: %w", err)
	}

	elapsed := time.Since(start).Seconds()
	report.DurationMs = elapsed * 1000
	if elapsed > 0 {
		report.TitlesPerSec = float64(report.TitlesEvaluated) / elapsed
		report.SlotsPerSec = float64(report.SlotsEvaluated) / elapsed
	}
	return report, nil
}

// ExplainTitle runs deterministic evaluation for a single movie or TV show without mutating DB state.
func (s *Store) ExplainTitle(ctx context.Context, cfg ProfileConfig, contentType, contentSource, contentID string, now time.Time) (TitleDecision, error) {
	if err := cfg.Validate(); err != nil {
		return TitleDecision{}, err
	}
	pool, err := s.pool.Get()
	if err != nil {
		return TitleDecision{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tk := titleKey{ContentType: contentType, ContentSource: contentSource, ContentID: contentID}
	cands, err := loadTitleCandidates(ctx, pool, cfg, tk, now)
	if err != nil {
		return TitleDecision{}, err
	}
	incumbents, err := loadTitleIncumbents(ctx, pool, cfg.ID, tk)
	if err != nil {
		return TitleDecision{}, err
	}
	return ResolveTitle(cfg, contentType, contentSource, contentID, cands, incumbents, now), nil
}

// ProfileStatus summarizes persisted state for a profile.
type ProfileStatus struct {
	ProfileID       string    `json:"profile_id"`
	Mode            string    `json:"mode"`
	ConfigDigest    string    `json:"config_digest"`
	WinnersRank1    int64     `json:"winners_rank_1"`
	RunnerUps       int64     `json:"runner_ups"`
	ActiveTombstones int64    `json:"active_tombstones"`
	PrunedTotal     int64     `json:"pruned_total"`
	SlotsEvaluated  int64     `json:"slots_evaluated"`
	UpdatedAt       time.Time `json:"updated_at"`
}

func (s *Store) Status(ctx context.Context, profileID string) ([]ProfileStatus, error) {
	pool, err := s.pool.Get()
	if err != nil {
		return nil, err
	}
	q := `
SELECT st.profile_id, st.mode, st.config_digest, st.slots_evaluated, st.pruned_total, st.updated_at,
  COALESCE((SELECT count(*) FROM lean_winners w WHERE w.profile_id = st.profile_id AND w.rank = 1), 0),
  COALESCE((SELECT count(*) FROM lean_winners w WHERE w.profile_id = st.profile_id AND w.rank > 1), 0),
  COALESCE((SELECT count(*) FROM lean_tombstones tb WHERE tb.profile_id = st.profile_id AND tb.config_digest = st.config_digest AND (tb.expires_at IS NULL OR tb.expires_at > clock_timestamp())), 0)
FROM lean_state st`
	var rows pgx.Rows
	if profileID != "" {
		rows, err = pool.Query(ctx, q+` WHERE st.profile_id = $1 ORDER BY st.profile_id`, profileID)
	} else {
		rows, err = pool.Query(ctx, q+` ORDER BY st.profile_id`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProfileStatus
	for rows.Next() {
		var ps ProfileStatus
		if err := rows.Scan(&ps.ProfileID, &ps.Mode, &ps.ConfigDigest, &ps.SlotsEvaluated, &ps.PrunedTotal, &ps.UpdatedAt,
			&ps.WinnersRank1, &ps.RunnerUps, &ps.ActiveTombstones); err != nil {
			return nil, err
		}
		out = append(out, ps)
	}
	return out, rows.Err()
}

// ClearTombstones removes tombstones for profileID (optionally restricted to one slotKey).
func (s *Store) ClearTombstones(ctx context.Context, profileID, slotKey string) (int64, error) {
	pool, err := s.pool.Get()
	if err != nil {
		return 0, err
	}
	if slotKey != "" {
		tag, err := pool.Exec(ctx, `DELETE FROM lean_tombstones WHERE profile_id = $1 AND slot_key = $2`, profileID, slotKey)
		return tag.RowsAffected(), err
	}
	tag, err := pool.Exec(ctx, `DELETE FROM lean_tombstones WHERE profile_id = $1`, profileID)
	return tag.RowsAffected(), err
}

func fetchNextTitles(ctx context.Context, pool *pgxpool.Pool, contentTypes []string, lastType, lastSource, lastID string, limit int) ([]titleKey, error) {
	var rows pgx.Rows
	var err error
	if lastType == "" {
		rows, err = pool.Query(ctx, `
SELECT DISTINCT content_type::text, content_source, content_id
FROM torrent_contents
WHERE content_type::text = ANY($1)
  AND content_source IS NOT NULL
  AND content_id IS NOT NULL
ORDER BY 1, 2, 3
LIMIT $2`, contentTypes, limit)
	} else {
		rows, err = pool.Query(ctx, `
SELECT DISTINCT content_type::text, content_source, content_id
FROM torrent_contents
WHERE content_type::text = ANY($1)
  AND content_source IS NOT NULL
  AND content_id IS NOT NULL
  AND (content_type::text, content_source, content_id) > ($2, $3, $4)
ORDER BY 1, 2, 3
LIMIT $5`, contentTypes, lastType, lastSource, lastID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]titleKey, 0, limit)
	for rows.Next() {
		var tk titleKey
		if err := rows.Scan(&tk.ContentType, &tk.ContentSource, &tk.ContentID); err != nil {
			return nil, err
		}
		out = append(out, tk)
	}
	return out, rows.Err()
}

func loadTitleCandidates(ctx context.Context, pool *pgxpool.Pool, cfg ProfileConfig, tk titleKey, now time.Time) ([]Candidate, error) {
	pubProcessor := cfg.Required.RequirePublicationReview
	if pubProcessor == "" && cfg.Required.RequireArmigerApproved {
		pubProcessor = "armiger-ultra"
	}
	rows, err := pool.Query(ctx, `
SELECT
  tc.info_hash,
  t.name,
  tc.content_type::text,
  tc.content_source,
  tc.content_id,
  tc.size,
  t.files_status::text,
  COALESCE(tc.files_count, 0),
  COALESCE(tc.video_resolution::text, ''),
  COALESCE(tc.video_source::text, ''),
  COALESCE(tc.video_codec::text, ''),
  COALESCE(tc.video_modifier::text, ''),
  COALESCE(tc.video_3d::text, ''),
  COALESCE(tc.release_group, ''),
  COALESCE(to_jsonb(tc.languages), '[]'::jsonb),
  COALESCE(tc.english_audio::text, ''),
  COALESCE(tc.episodes, '{}'::jsonb),
  COALESCE(tc.release_granularity::text, ''),
  COALESCE(tc.anime_absolute_episode, 0),
  tc.release_attributes,
  tc.seeders,
  tts_tr.seeders,
  tc.published_at,
  tc.created_at,
  COALESCE(array_remove(b.extensions, NULL), '{}'::text[]),
  COALESCE(t.extension, ''),
  COALESCE(liv.status = 'dead', false),
  COALESCE(ppr.approved AND ppr.source_revision = pps.revision, false),
  ppr.evaluated_at,
  COALESCE(plr.approved AND plr.source_revision = pps.revision AND plr.expires_at > $4, false),
  plr.evaluated_at,
  (
    t.private
    OR EXISTS (SELECT 1 FROM torrent_canonical_labels cl WHERE cl.info_hash = tc.info_hash)
    OR EXISTS (SELECT 1 FROM label_evidence le WHERE le.info_hash = tc.info_hash)
    OR EXISTS (SELECT 1 FROM torrent_grab_attempts ga WHERE ga.info_hash = tc.info_hash)
    OR EXISTS (SELECT 1 FROM torrent_hints th WHERE th.info_hash = tc.info_hash AND th.content_id IS NOT NULL)
    OR EXISTS (SELECT 1 FROM processor_publication_holds ph WHERE ph.info_hash = tc.info_hash AND ph.held)
    OR EXISTS (SELECT 1 FROM junkpurge_quarantine jq WHERE jq.info_hash = tc.info_hash AND jq.expired_at IS NULL)
  ) AS protected
FROM torrent_contents tc
JOIN torrents t ON t.info_hash = tc.info_hash
LEFT JOIN torrent_file_bundles b ON b.info_hash = tc.info_hash
LEFT JOIN torrents_torrent_sources tts_tr ON tts_tr.info_hash = tc.info_hash AND tts_tr.source = 'tracker'
LEFT JOIN torrent_liveness liv ON liv.info_hash = tc.info_hash
LEFT JOIN processor_publication_sources pps ON pps.info_hash = tc.info_hash
LEFT JOIN processor_publication_reviews ppr ON ppr.info_hash = tc.info_hash AND ppr.processor_id = $5
LEFT JOIN processor_payload_reviews plr ON plr.info_hash = tc.info_hash
WHERE tc.content_type = $1
  AND tc.content_source = $2
  AND tc.content_id = $3
ORDER BY tc.info_hash`,
		tk.ContentType, tk.ContentSource, tk.ContentID, now, pubProcessor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Candidate
	for rows.Next() {
		var (
			c          Candidate
			sizeInt    int64
			filesCount int
			absEp      int
			langsJSON  []byte
			engAudio   string
			epsJSON    []byte
			attrsJSON  []byte
			tcSeeders  *int
			trSeeders  *int
			bundleExts []string
			singleExt  string
		)
		if err := rows.Scan(
			&c.InfoHash,
			&c.Name,
			&c.ContentType,
			&c.ContentSource,
			&c.ContentID,
			&sizeInt,
			&c.FilesStatus,
			&filesCount,
			&c.VideoResolution,
			&c.VideoSource,
			&c.VideoCodec,
			&c.VideoModifier,
			&c.Video3D,
			&c.ReleaseGroup,
			&langsJSON,
			&engAudio,
			&epsJSON,
			&c.ReleaseGranularity,
			&absEp,
			&attrsJSON,
			&tcSeeders,
			&trSeeders,
			&c.PublishedAt,
			&c.CreatedAt,
			&bundleExts,
			&singleExt,
			&c.LivenessDead,
			&c.ArmigerApproved,
			&c.ArmigerEvaluatedAt,
			&c.PayloadProofActive,
			&c.PayloadProofVerifiedAt,
			&c.Protected,
		); err != nil {
			return nil, err
		}
		if sizeInt > 0 {
			c.SizeBytes = uint64(sizeInt)
		}
		c.FilesCount = filesCount
		c.AnimeAbsoluteEpisode = absEp
		if len(langsJSON) > 0 {
			_ = json.Unmarshal(langsJSON, &c.Languages)
		}
		if engAudio == "dub" {
			t := true
			c.EnglishAudio = &t
		} else if engAudio == "none" {
			f := false
			c.EnglishAudio = &f
		}
		if len(epsJSON) > 0 {
			_ = json.Unmarshal(epsJSON, &c.Episodes)
		}
		if len(attrsJSON) > 0 {
			var ra model.ReleaseAttributes
			if json.Unmarshal(attrsJSON, &ra) == nil {
				c.HDRFormats = ra.HDRFormats
				c.AudioFormats = ra.AudioFormats
				c.AudioFeatures = ra.AudioFeatures
				c.Revisions = ra.Revisions
			}
		}
		// Freshest-authoritative seeders: tracker row overrides cached DHT count.
		if trSeeders != nil {
			c.Seeders = trSeeders
			c.SeedersAuthoritative = true
		} else if tcSeeders != nil {
			c.Seeders = tcSeeders
		}
		// Classify file composition from compact bundle extensions or single-file extension.
		if len(bundleExts) > 0 {
			for _, ext := range bundleExts {
				le := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(ext), "."))
				if videoExtensions[le] {
					c.VideoFileCount++
				}
				if subtitleExtensions[le] {
					c.HasEmbeddedSubtitles = true
				}
				if clutterExtensions[le] {
					c.HasClutterFiles = true
				}
			}
		} else if singleExt != "" {
			le := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(singleExt), "."))
			if videoExtensions[le] {
				c.VideoFileCount = 1
			}
			if clutterExtensions[le] {
				c.HasClutterFiles = true
			}
		}
		if !c.HasEmbeddedSubtitles && (engAudio == "sub" || strings.Contains(strings.ToLower(c.Name), "multi") || strings.Contains(strings.ToLower(c.Name), "subs")) {
			c.HasEmbeddedSubtitles = true
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func loadTitleIncumbents(ctx context.Context, pool *pgxpool.Pool, profileID string, tk titleKey) (map[string]IncumbentState, error) {
	rows, err := pool.Query(ctx, `
SELECT slot_key, info_hash, preferred_score, tiebreak_score, incumbent_since
FROM lean_winners
WHERE profile_id = $1
  AND content_type = $2
  AND content_source = $3
  AND content_id = $4
  AND rank = 1`,
		profileID, tk.ContentType, tk.ContentSource, tk.ContentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]IncumbentState)
	for rows.Next() {
		var slotKey string
		var st IncumbentState
		if err := rows.Scan(&slotKey, &st.InfoHash, &st.PreferredScore, &st.TiebreakScore, &st.IncumbentSince); err != nil {
			return nil, err
		}
		out[slotKey] = st
	}
	return out, rows.Err()
}

func persistTitleWinners(
	ctx context.Context,
	pool *pgxpool.Pool,
	profileID, digest string,
	td TitleDecision,
	incumbents map[string]IncumbentState,
	now time.Time,
) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Replace winners for this title atomically inside one short transaction.
	// Touches ONLY lean_winners — zero triggers on torrents or processor_publication_sources!
	if _, err := tx.Exec(ctx, `
DELETE FROM lean_winners
WHERE profile_id = $1
  AND content_type = $2
  AND content_source = $3
  AND content_id = $4`,
		profileID, td.ContentType, td.ContentSource, td.ContentID); err != nil {
		return err
	}

	for _, sd := range td.Slots {
		for _, w := range sd.Winners {
			incSince := now
			if prev, ok := incumbents[sd.SlotKey]; ok && slices.Equal(prev.InfoHash, w.Candidate.InfoHash) && !prev.IncumbentSince.IsZero() {
				incSince = prev.IncumbentSince
			}
			if _, err := tx.Exec(ctx, `
INSERT INTO lean_winners (
  profile_id, slot_key, rank, info_hash, content_type, content_source, content_id,
  config_digest, preferred_score, tiebreak_score, incumbent_since, evaluated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
				profileID, sd.SlotKey, w.KeptRank, w.Candidate.InfoHash,
				td.ContentType, td.ContentSource, td.ContentID,
				digest, w.PreferredScore, w.TiebreakScore, incSince, now); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

func pruneChunkTx(
	ctx context.Context,
	pool *pgxpool.Pool,
	profileID, digest string,
	hashes [][]byte,
	slots, reasons []string,
	expiresAt *time.Time,
) ([][]byte, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = '15s'; SET LOCAL lock_timeout = '2s'`); err != nil {
		return nil, err
	}

	// Lock only uncontended, non-winner, non-protected rows in stable hash order.
	rows, err := tx.Query(ctx, `
WITH candidates AS (
  SELECT info_hash, slot_key, reason_code
  FROM unnest($1::bytea[], $2::text[], $3::text[]) AS u(info_hash, slot_key, reason_code)
),
locked AS (
  SELECT t.info_hash, c.slot_key, c.reason_code
  FROM torrents t
  JOIN candidates c ON c.info_hash = t.info_hash
  WHERE t.private = FALSE
    AND NOT EXISTS (SELECT 1 FROM lean_winners w WHERE w.info_hash = t.info_hash)
    AND NOT EXISTS (SELECT 1 FROM torrent_canonical_labels cl WHERE cl.info_hash = t.info_hash)
    AND NOT EXISTS (SELECT 1 FROM label_evidence le WHERE le.info_hash = t.info_hash)
    AND NOT EXISTS (SELECT 1 FROM torrent_grab_attempts ga WHERE ga.info_hash = t.info_hash)
    AND NOT EXISTS (SELECT 1 FROM torrent_hints th WHERE th.info_hash = t.info_hash AND th.content_id IS NOT NULL)
    AND NOT EXISTS (SELECT 1 FROM processor_publication_holds ph WHERE ph.info_hash = t.info_hash AND ph.held)
    AND NOT EXISTS (SELECT 1 FROM junkpurge_quarantine jq WHERE jq.info_hash = t.info_hash AND jq.expired_at IS NULL)
    AND NOT EXISTS (SELECT 1 FROM torrent_tags tt WHERE tt.info_hash = t.info_hash AND `+catalogueguard.PrivacyTagSQL("tt.name", "$4")+`)
  ORDER BY t.info_hash
  FOR UPDATE OF t SKIP LOCKED
)
SELECT info_hash, slot_key, reason_code FROM locked`,
		hashes, slots, reasons, catalogueguard.TagWhitespace)
	if err != nil {
		return nil, err
	}
	type doomedRow struct {
		hash   []byte
		slot   string
		reason string
	}
	var doomed []doomedRow
	for rows.Next() {
		var r doomedRow
		if err := rows.Scan(&r.hash, &r.slot, &r.reason); err != nil {
			rows.Close()
			return nil, err
		}
		doomed = append(doomed, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(doomed) == 0 {
		return nil, tx.Commit(ctx)
	}

	delHashes := make([][]byte, len(doomed))
	delSlots := make([]string, len(doomed))
	delReasons := make([]string, len(doomed))
	for i, d := range doomed {
		delHashes[i] = d.hash
		delSlots[i] = d.slot
		delReasons[i] = d.reason
	}

	// Delete torrents (cascades cleanly to torrent_contents, torrent_file_bundles,
	// torrents_torrent_sources, torrent_tracker_seeds, etc.; never touches kept winners!).
	if _, err := tx.Exec(ctx, `DELETE FROM torrents WHERE info_hash = ANY($1)`, delHashes); err != nil {
		return nil, err
	}

	// Record compact config-digest-bound tombstones preventing DHT/importer re-crawl churn.
	if _, err := tx.Exec(ctx, `
INSERT INTO lean_tombstones (info_hash, profile_id, config_digest, slot_key, reason_code, pruned_at, expires_at)
SELECT u.info_hash, $4, $5, u.slot_key, u.reason_code, clock_timestamp(), $6
FROM unnest($1::bytea[], $2::text[], $3::text[]) AS u(info_hash, slot_key, reason_code)
ON CONFLICT (info_hash) DO UPDATE SET
  profile_id = EXCLUDED.profile_id,
  config_digest = EXCLUDED.config_digest,
  slot_key = EXCLUDED.slot_key,
  reason_code = EXCLUDED.reason_code,
  pruned_at = clock_timestamp(),
  expires_at = EXCLUDED.expires_at`,
		delHashes, delSlots, delReasons, profileID, digest, expiresAt); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return delHashes, nil
}

func splitCursor(cursor string) (string, string, string) {
	if cursor == "" {
		return "", "", ""
	}
	parts := strings.SplitN(cursor, "\x1f", 3)
	if len(parts) != 3 {
		return "", "", ""
	}
	return parts[0], parts[1], parts[2]
}
