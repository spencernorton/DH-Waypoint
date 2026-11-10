package csamblocklist

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bits-and-blooms/bloom/v3"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"go.uber.org/zap"
)

// service is the live Manager implementation. Exposed via the Manager
// interface; constructed by the factory in csamblocklistfx.
//
// Concurrency model:
//   - active is an atomic pointer to the current bloom filter; reads
//     (IsBlocked / Filter) are lock-free.
//   - Refresh builds a fresh bloom filter from every configured feed
//     in parallel, then atomically swaps active. No reader sees a
//     partially-built filter.
//   - perFeed status map is protected by mutex; touched only by
//     Refresh and by Snapshot diagnostics.
//   - pending holds the matches Observe saw since the last refresh. The
//     bloom filter is read without a lock and cannot take a new entry, so
//     they wait in this small locked set until Refresh has loaded them
//     from csam_local_observations. pendingN lets lookups skip the lock
//     while the set is empty.
type service struct {
	cfg     Config
	metrics *Metrics
	logger  *zap.SugaredLogger
	client  *http.Client

	active atomic.Pointer[bloom.BloomFilter]

	mu      sync.Mutex
	perFeed map[string]FeedStatus
	// lastGoodEntries caches the most recent SUCCESSFUL fetch result
	// per feed URL. On Refresh, feeds that succeed update their cache
	// entry; feeds that fail retain their previous cache. The union
	// bloom is rebuilt from the cache for ALL feeds, so a transient
	// feed outage does not drop previously loaded entries (Jeeves
	// review finding: "Feed refresh failures drop previously loaded
	// blocklist entries"). A feed that has NEVER succeeded simply
	// has no cache entry and contributes nothing.
	lastGoodEntries map[string][][DoubleHashLen]byte
	// lastGoodLocal is the same cache for this instance's own
	// observations, used when the table cannot be read.
	lastGoodLocal [][DoubleHashLen]byte
	lastRefreshAt time.Time
	entryCount    atomic.Int64
	// refreshMu keeps two refreshes from interleaving: the later swap
	// must not install a filter older than the pending entries the
	// earlier one already released.
	refreshMu sync.Mutex

	// pool reads and writes csam_local_observations. Nil keeps
	// observations in memory only.
	pool     lazy.Lazy[*pgxpool.Pool]
	exporter interface {
		export(context.Context, protocol.InfoHash)
	}

	pendingMu sync.RWMutex
	pending   map[[DoubleHashLen]byte]string // double hash -> source
	pendingN  atomic.Int64
}

// NewService constructs the live Manager. The bloom filter starts
// empty (every IsBlocked returns false until the first Refresh
// completes); the worker triggers Refresh asynchronously on startup.
func NewService(cfg Config, logger *zap.SugaredLogger, metrics *Metrics) Manager {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	s := &service{
		cfg:     cfg,
		metrics: metrics,
		logger:  logger,
		client: &http.Client{
			Timeout: cfg.FeedFetchTimeout,
		},
		perFeed:         make(map[string]FeedStatus, len(cfg.FeedUrls)),
		lastGoodEntries: make(map[string][][DoubleHashLen]byte, len(cfg.FeedUrls)),
		pending:         make(map[[DoubleHashLen]byte]string),
	}
	// Empty bloom so reads before first refresh are O(1) and return
	// false for every hash. Sized to operator's configured capacity
	// so post-Refresh swaps don't change allocation behaviour.
	empty := bloom.NewWithEstimates(maxUint(cfg.BloomCapacity, 1), cfg.BloomFalsePositiveRate)
	s.active.Store(empty)
	for _, u := range cfg.FeedUrls {
		if u == "" {
			continue
		}
		s.perFeed[u] = FeedStatus{URL: u}
	}
	return s
}

// New is the factory used by csamblocklistfx. It always returns the live
// service: the filter also holds this instance's own banned-keyword
// matches, which must be remembered with no feed configured and with
// feeds switched off. Config.Enabled switches the outside feeds only.
func New(cfg Config, logger *zap.SugaredLogger, metrics *Metrics, pool lazy.Lazy[*pgxpool.Pool], exp Exporter) Manager {
	s := NewService(cfg, logger, metrics).(*service)
	s.pool = pool
	s.exporter, _ = exp.(interface {
		export(context.Context, protocol.InfoHash)
	})
	return s
}

// IsBlocked is the hot-path call. Atomic load + bloom test; the lock is
// taken only while an observation waits for the next refresh.
func (s *service) IsBlocked(infoHash protocol.InfoHash) bool {
	if s.metrics != nil {
		s.metrics.LookupsTotal.Inc()
	}
	return s.blocked(DoubleHash(infoHash))
}

func (s *service) blocked(dh [DoubleHashLen]byte) bool {
	if bf := s.active.Load(); bf != nil && bf.Test(dh[:]) {
		return true
	}
	if s.pendingN.Load() == 0 {
		return false
	}
	s.pendingMu.RLock()
	_, ok := s.pending[dh]
	s.pendingMu.RUnlock()
	return ok
}

// Filter returns the unblocked subset, mirroring BlockingManager.Filter
// signature so call sites read consistently.
func (s *service) Filter(infoHashes []protocol.InfoHash) []protocol.InfoHash {
	out := infoHashes[:0:len(infoHashes)]
	for _, h := range infoHashes {
		if s.metrics != nil {
			s.metrics.LookupsTotal.Inc()
		}
		if s.blocked(DoubleHash(h)) {
			if s.metrics != nil {
				s.metrics.PrefetchBlocksTotal.Inc()
			}
			continue
		}
		out = append(out, h)
	}
	return out
}

// Observe remembers the identities of one torrent whose name or file
// paths matched the built-in banned-keyword list. Lookups reject them
// from this call on. The double hashes go to csam_local_observations so
// a restart keeps them, and to the export log. A hash that is already
// rejected is not recorded or exported a second time.
func (s *service) Observe(ctx context.Context, source string, infoHashes ...protocol.InfoHash) {
	uniq := make([]protocol.InfoHash, 0, len(infoHashes))
	for _, h := range infoHashes {
		if !slices.Contains(uniq, h) && !s.blocked(DoubleHash(h)) {
			uniq = append(uniq, h)
		}
	}
	if len(uniq) == 0 {
		return
	}
	s.pendingMu.Lock()
	for _, h := range uniq {
		s.pending[DoubleHash(h)] = source
	}
	s.pendingN.Store(int64(len(s.pending)))
	s.pendingMu.Unlock()
	if s.metrics != nil {
		s.metrics.KeywordMatchesTotal.WithLabelValues(source).Inc()
	}
	if s.pool != nil {
		pool, err := s.pool.Get()
		if err == nil {
			err = SaveObservations(ctx, pool, source, uniq...)
		}
		if err != nil {
			// It stays in the pending set, and Refresh writes it again.
			s.logger.Warnw("csam local observation not saved; retrying at the next refresh", "source", source, "err", err)
		}
	}
	if s.exporter != nil {
		for _, h := range uniq {
			s.exporter.export(ctx, h)
		}
	}
}

// syncLocal writes the pending observations that are not saved yet and
// reads every saved one back. flushed lists the pending entries the
// returned rows are known to include.
func (s *service) syncLocal(ctx context.Context) (flushed, local [][DoubleHashLen]byte, err error) {
	if s.pool == nil {
		return nil, nil, nil
	}
	pool, err := s.pool.Get()
	if err != nil {
		return nil, nil, err
	}
	// The caller's deadline budgets the feed fetch. Waiting for the
	// database above, with its migrations at start, can outlast it, and
	// giving up here would leave remembered hashes out of the filter
	// until the next interval.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	s.pendingMu.RLock()
	hashes := make([][]byte, 0, len(s.pending))
	sources := make([]string, 0, len(s.pending))
	for dh, source := range s.pending {
		flushed = append(flushed, dh)
		hashes = append(hashes, dh[:])
		sources = append(sources, source)
	}
	s.pendingMu.RUnlock()
	if err := saveDoubleHashes(ctx, pool, hashes, sources); err != nil {
		return nil, nil, err
	}
	local, err = loadObservations(ctx, pool)
	if err != nil {
		return nil, nil, err
	}
	return flushed, local, nil
}

// Refresh polls every configured feed in parallel, builds a single
// new bloom filter from the union of all per-feed last-good caches,
// and atomically swaps it in.
//
// Per-feed errors are logged + counted but do not abort the refresh —
// feeds that succeed update their cache; feeds that fail retain their
// previous cache (if any). A feed that has never succeeded simply
// contributes nothing. The active bloom is rebuilt every refresh
// from the union of the cache, so the bloom filter never shrinks due
// to a transient feed outage.
//
// This instance's own observations are read from
// csam_local_observations into the same filter, with or without feeds.
// If they cannot be read, the last rows that were read stay in the
// filter and nothing leaves the pending set.
//
// Returns the first error seen, or nil. Caller's worker loop logs but
// does not bail on this error — the next interval retries.
func (s *service) Refresh(ctx context.Context) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	feeds := make([]string, 0, len(s.cfg.FeedUrls))
	if s.cfg.Enabled {
		for _, u := range s.cfg.FeedUrls {
			if u != "" {
				feeds = append(feeds, u)
			}
		}
	}

	type feedResult struct {
		url     string
		entries [][DoubleHashLen]byte
		err     error
		took    time.Duration
	}
	results := make([]feedResult, len(feeds))
	var wg sync.WaitGroup
	for i, u := range feeds {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			t0 := time.Now()
			entries, err := s.fetchFeed(ctx, u)
			results[i] = feedResult{
				url:     u,
				entries: entries,
				err:     err,
				took:    time.Since(t0),
			}
		}(i, u)
	}
	wg.Wait()

	flushed, local, localErr := s.syncLocal(ctx)

	var firstErr error

	s.mu.Lock()
	if len(feeds) == 0 {
		// No feed is configured or feeds are off: drop what earlier ones loaded.
		s.lastGoodEntries = make(map[string][][DoubleHashLen]byte)
	}
	for _, r := range results {
		st := s.perFeed[r.url]
		st.URL = r.url
		now := time.Now()
		if r.err != nil {
			st.LastError = r.err.Error()
			st.LastErrorAt = now
			st.ConsecutiveErrs++
			if firstErr == nil {
				firstErr = fmt.Errorf("feed %s: %w", r.url, r.err)
			}
			if s.metrics != nil {
				s.metrics.FeedRefreshTotal.WithLabelValues(feedLabel(r.url), "error").Inc()
				s.metrics.FeedRefreshDurationSeconds.WithLabelValues(feedLabel(r.url)).Observe(r.took.Seconds())
			}
			cachedN := len(s.lastGoodEntries[r.url])
			s.logger.Warnw("csam blocklist feed refresh failed; retaining cached entries",
				"url", r.url, "err", r.err, "consecutive_errs", st.ConsecutiveErrs, "retained_entries", cachedN)
			// IMPORTANT: do NOT touch s.lastGoodEntries[r.url] — keep
			// the previous-good cache so the union bloom retains those
			// entries when rebuilt below. EntriesContributed reflects
			// the cached count, not zero, so the operator dashboard
			// shows the real protection coverage even during outages.
			st.EntriesContributed = cachedN
		} else {
			st.LastSuccessAt = now
			st.ConsecutiveErrs = 0
			st.LastError = ""
			st.EntriesContributed = len(r.entries)
			s.lastGoodEntries[r.url] = r.entries
			if s.metrics != nil {
				s.metrics.FeedRefreshTotal.WithLabelValues(feedLabel(r.url), "success").Inc()
				s.metrics.FeedRefreshDurationSeconds.WithLabelValues(feedLabel(r.url)).Observe(r.took.Seconds())
				s.metrics.FeedEntriesGauge.WithLabelValues(feedLabel(r.url)).Set(float64(len(r.entries)))
			}
		}
		s.perFeed[r.url] = st
	}
	s.lastRefreshAt = time.Now()
	if localErr != nil {
		s.logger.Warnw("csam local observations not loaded; keeping the previous ones", "err", localErr)
		if firstErr == nil {
			firstErr = fmt.Errorf("local observations: %w", localErr)
		}
		flushed, local = nil, s.lastGoodLocal
	}
	s.lastGoodLocal = local

	// Build the union bloom from EVERY feed's last-good cache (whether
	// the most recent attempt succeeded or failed). This is the core of
	// fix #2: failed feeds keep contributing their previously-loaded
	// entries until the next successful refresh replaces the cache.
	// Local observations only grow, so the filter is sized for the real
	// entry count once that passes the configured capacity.
	totalEntries := len(local)
	for _, entries := range s.lastGoodEntries {
		totalEntries += len(entries)
	}
	newBloom := bloom.NewWithEstimates(maxUint(s.cfg.BloomCapacity, maxUint(uint(totalEntries), 1)), s.cfg.BloomFalsePositiveRate)
	for _, entries := range s.lastGoodEntries {
		for _, e := range entries {
			newBloom.Add(e[:])
		}
	}
	s.mu.Unlock()
	for _, e := range local {
		newBloom.Add(e[:])
	}

	s.active.Store(newBloom)
	s.entryCount.Store(int64(totalEntries))
	if s.metrics != nil {
		s.metrics.EntriesGauge.Set(float64(totalEntries))
		s.metrics.FeedEntriesGauge.WithLabelValues("local").Set(float64(len(local)))
	}

	// The new filter holds these now. Entries Observe added during this
	// refresh are not in flushed and stay pending.
	s.pendingMu.Lock()
	for _, dh := range flushed {
		delete(s.pending, dh)
	}
	s.pendingN.Store(int64(len(s.pending)))
	s.pendingMu.Unlock()

	return firstErr
}

func (s *service) Enabled() bool { return true }

func (s *service) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	pf := make(map[string]FeedStatus, len(s.perFeed))
	for k, v := range s.perFeed {
		pf[k] = v
	}
	return Snapshot{
		EntryCount:             int(s.entryCount.Load()),
		BloomCapacity:          int(s.cfg.BloomCapacity),
		BloomFalsePositiveRate: s.cfg.BloomFalsePositiveRate,
		LastRefreshAt:          s.lastRefreshAt,
		PerFeed:                pf,
	}
}

// Run is the periodic poll loop. Called from the fx worker hook;
// returns when ctx is cancelled.
//
// The fx OnStart hook (csamblocklistfx.provideRefreshWorker) does the
// FIRST refresh synchronously before launching this loop, so the
// dhtcrawler never sees an empty filter when traffic starts flowing.
// This loop only services the periodic refresh interval — it does
// NOT do an initial refresh on entry.
func (s *service) Run(ctx context.Context) {
	tick := s.cfg.FeedRefreshInterval
	if tick <= 0 {
		tick = 6 * time.Hour
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Refresh(ctx); err != nil {
				if !errors.Is(err, context.Canceled) {
					s.logger.Warnw("csam blocklist periodic refresh had errors", "err", err)
				}
			}
		}
	}
}

func maxUint(a uint, b uint) uint {
	if a > b {
		return a
	}
	return b
}
