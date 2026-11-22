package seeds

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/namepolicy"
)

// Store is the persistence layer for the seeds worker. It owns the
// torrent_tracker_seeds ledger and mirrors positive results into the
// torrents_torrent_sources(source='tracker') surfacing row.
type Store struct {
	pool  lazy.Lazy[*pgxpool.Pool]
	names *namepolicy.Policy

	// Keyset cursors allow successive SelectStale cycles to advance past
	// permanently excluded rows (private/adult/name-policy) and already-scraped
	// prefixes instead of re-scanning them from the start of the index every
	// cycle. Each cursor wraps cleanly back to empty when a pass exhausts its
	// partition.
	cursorMu         sync.Mutex
	recheckCursorAt  time.Time
	recheckCursorH   []byte
	neverCursorH     []byte
	payloadTableMu      sync.Mutex
	payloadTableChecked bool
	hasPayloadTable     bool
}

func NewStore(pool lazy.Lazy[*pgxpool.Pool], names ...*namepolicy.Policy) *Store {
	s := &Store{pool: pool}
	if len(names) > 0 {
		s.names = names[0]
	}
	return s
}

func (s *Store) payloadReviewsAvailable(ctx context.Context, pool *pgxpool.Pool) bool {
	s.payloadTableMu.Lock()
	if s.payloadTableChecked {
		exists := s.hasPayloadTable
		s.payloadTableMu.Unlock()
		return exists
	}
	s.payloadTableMu.Unlock()

	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('processor_payload_reviews') IS NOT NULL`).Scan(&exists); err != nil {
		return false
	}

	s.payloadTableMu.Lock()
	s.hasPayloadTable = exists
	s.payloadTableChecked = true
	s.payloadTableMu.Unlock()
	return exists
}

func (s *Store) queryRechecks(
	ctx context.Context,
	pool *pgxpool.Pool,
	minAge time.Duration,
	limit int,
	afterAt time.Time,
	afterHash []byte,
	seen map[string]struct{},
) (hashes [][]byte, lastAt time.Time, lastHash []byte, rawCount int, err error) {
	if limit <= 0 {
		return nil, afterAt, afterHash, 0, nil
	}
	var sql string
	var args []any
	if !afterAt.IsZero() && len(afterHash) > 0 {
		adm, admArgs := s.selectionAdmissionAt(5)
		sql = `
select t.info_hash, s.checked_at
from torrent_tracker_seeds s
join torrents t on t.info_hash = s.info_hash
where s.checked_at < now() - make_interval(secs => $1)
  and (s.checked_at > $3 or (s.checked_at = $3 and t.info_hash > $4))
  AND ` + adm + `
order by s.checked_at asc, t.info_hash asc
limit $2::bigint`
		args = append([]any{minAge.Seconds(), limit, afterAt, afterHash}, admArgs...)
	} else {
		adm, admArgs := s.selectionAdmissionAt(3)
		sql = `
select t.info_hash, s.checked_at
from torrent_tracker_seeds s
join torrents t on t.info_hash = s.info_hash
where s.checked_at < now() - make_interval(secs => $1) AND ` + adm + `
order by s.checked_at asc, t.info_hash asc
limit $2::bigint`
		args = append([]any{minAge.Seconds(), limit}, admArgs...)
	}
	rows, qErr := pool.Query(ctx, sql, args...)
	if qErr != nil {
		return nil, time.Time{}, nil, 0, qErr
	}
	defer rows.Close()
	for rows.Next() {
		var h []byte
		var checkedAt time.Time
		if scanErr := rows.Scan(&h, &checkedAt); scanErr != nil {
			return nil, time.Time{}, nil, rawCount, scanErr
		}
		rawCount++
		lastAt = checkedAt
		lastHash = append([]byte(nil), h...)
		if _, dup := seen[string(h)]; dup {
			continue
		}
		seen[string(h)] = struct{}{}
		hashes = append(hashes, h)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, time.Time{}, nil, rawCount, rowsErr
	}
	return hashes, lastAt, lastHash, rawCount, nil
}

func (s *Store) queryNeverChecked(
	ctx context.Context,
	pool *pgxpool.Pool,
	limit int,
	afterHash []byte,
	seen map[string]struct{},
) (hashes [][]byte, lastHash []byte, rawCount int, err error) {
	if limit <= 0 {
		return nil, afterHash, 0, nil
	}
	var sql string
	var args []any
	if len(afterHash) > 0 {
		adm, admArgs := s.selectionAdmissionAt(3)
		sql = `
select t.info_hash
from torrents t
where t.info_hash > $2
  and not exists (select 1 from torrent_tracker_seeds s where s.info_hash = t.info_hash)
  AND ` + adm + `
order by t.info_hash asc
limit $1::bigint`
		args = append([]any{limit, afterHash}, admArgs...)
	} else {
		adm, admArgs := s.selectionAdmissionAt(2)
		sql = `
select t.info_hash
from torrents t
where not exists (select 1 from torrent_tracker_seeds s where s.info_hash = t.info_hash)
  AND ` + adm + `
order by t.info_hash asc
limit $1::bigint`
		args = append([]any{limit}, admArgs...)
	}
	rows, qErr := pool.Query(ctx, sql, args...)
	if qErr != nil {
		return nil, nil, 0, qErr
	}
	defer rows.Close()
	for rows.Next() {
		var h []byte
		if scanErr := rows.Scan(&h); scanErr != nil {
			return nil, nil, rawCount, scanErr
		}
		rawCount++
		lastHash = append([]byte(nil), h...)
		if _, dup := seen[string(h)]; dup {
			continue
		}
		seen[string(h)] = struct{}{}
		hashes = append(hashes, h)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, nil, rawCount, rowsErr
	}
	return hashes, lastHash, rawCount, nil
}

// queryPriorityApproved selects up to limit hashes that carry active approved
// payload evidence and have either never been scraped or are older than minAge,
// ensuring user-facing verified releases receive fresh seed/leech counts ahead
// of the unverified backlog.
func (s *Store) queryPriorityApproved(
	ctx context.Context,
	pool *pgxpool.Pool,
	minAge time.Duration,
	limit int,
	seen map[string]struct{},
) (neverHashes [][]byte, recheckHashes [][]byte, err error) {
	if limit <= 0 || !s.payloadReviewsAvailable(ctx, pool) {
		return nil, nil, nil
	}
	adm, admArgs := s.selectionAdmissionAt(3)
	sql := `
select t.info_hash, (s.info_hash is not null) as is_recheck
from processor_payload_reviews p
join torrents t on t.info_hash = p.info_hash
left join torrent_tracker_seeds s on s.info_hash = p.info_hash
where p.approved and p.expires_at > now()
  and (s.info_hash is null or s.checked_at < now() - make_interval(secs => $1))
  AND ` + adm + `
order by s.checked_at asc nulls first, t.info_hash asc
limit $2::bigint`
	rows, qErr := pool.Query(ctx, sql, append([]any{minAge.Seconds(), limit}, admArgs...)...)
	if qErr != nil {
		return nil, nil, qErr
	}
	defer rows.Close()
	for rows.Next() {
		var h []byte
		var isRecheck bool
		if scanErr := rows.Scan(&h, &isRecheck); scanErr != nil {
			return nil, nil, scanErr
		}
		if _, dup := seen[string(h)]; dup {
			continue
		}
		seen[string(h)] = struct{}{}
		if isRecheck {
			recheckHashes = append(recheckHashes, h)
		} else {
			neverHashes = append(neverHashes, h)
		}
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, nil, rowsErr
	}
	return neverHashes, recheckHashes, nil
}

// SelectStale returns up to limit info-hashes that have never been scraped or
// whose last scrape is older than minAge. Up to a quarter of multi-item
// batches prioritizes active payload-verified torrents so user-facing results
// stay fresh ahead of large backlogs. Half of remaining capacity is reserved
// for oldest previously checked hashes so a continuous never-checked backlog
// cannot starve rechecks, while keyset cursors skip permanently excluded
// prefixes across successive cycles. A one-item batch retains never-checked
// priority.
func (s *Store) SelectStale(ctx context.Context, minAge time.Duration, limit int) ([][]byte, error) {
	if limit < 0 {
		return nil, fmt.Errorf("seeds: select stale: limit must be non-negative: %d", limit)
	}
	if limit == 0 {
		return nil, nil
	}
	pool, err := s.pool.Get()
	if err != nil {
		return nil, fmt.Errorf("seeds: acquire pool: %w", err)
	}

	s.cursorMu.Lock()
	startRecheckAt := s.recheckCursorAt
	startRecheckH := append([]byte(nil), s.recheckCursorH...)
	startNeverH := append([]byte(nil), s.neverCursorH...)
	s.cursorMu.Unlock()

	seen := make(map[string]struct{}, limit)
	var priorityNever, priorityRecheck [][]byte
	if limit >= 4 {
		prioLimit := limit / 4
		pNever, pRecheck, pErr := s.queryPriorityApproved(ctx, pool, minAge, prioLimit, seen)
		if pErr != nil {
			return nil, fmt.Errorf("seeds: select priority approved: %w", pErr)
		}
		priorityNever = pNever
		priorityRecheck = pRecheck
	}

	recheckTarget := limit / 2
	recheckLimit := recheckTarget - len(priorityRecheck)
	if recheckLimit < 0 {
		recheckLimit = 0
	}
	var rechecks [][]byte
	nextRecheckAt := startRecheckAt
	nextRecheckH := startRecheckH

	if recheckLimit > 0 {
		got, lastAt, lastH, rawN, qErr := s.queryRechecks(ctx, pool, minAge, recheckLimit, startRecheckAt, startRecheckH, seen)
		if qErr != nil {
			return nil, fmt.Errorf("seeds: select stale rechecks: %w", qErr)
		}
		rechecks = append(rechecks, got...)
		if rawN == recheckLimit && !lastAt.IsZero() && len(lastH) > 0 {
			nextRecheckAt = lastAt
			nextRecheckH = lastH
		} else if !startRecheckAt.IsZero() && len(startRecheckH) > 0 {
			// Exhausted the tail past the cursor; wrap cleanly to the start.
			nextRecheckAt = time.Time{}
			nextRecheckH = nil
			if rem := recheckLimit - len(rechecks); rem > 0 {
				wrapGot, wAt, wH, wRaw, wErr := s.queryRechecks(ctx, pool, minAge, rem, time.Time{}, nil, seen)
				if wErr != nil {
					return nil, fmt.Errorf("seeds: select stale rechecks wrap: %w", wErr)
				}
				rechecks = append(rechecks, wrapGot...)
				if wRaw == rem && !wAt.IsZero() && len(wH) > 0 {
					nextRecheckAt = wAt
					nextRecheckH = wH
				}
			}
		}
	}

	neverLimit := limit - len(priorityNever) - len(priorityRecheck) - len(rechecks)
	if neverLimit < 0 {
		neverLimit = 0
	}
	var neverChecked [][]byte
	nextNeverH := startNeverH

	if neverLimit > 0 {
		got, lastH, rawN, qErr := s.queryNeverChecked(ctx, pool, neverLimit, startNeverH, seen)
		if qErr != nil {
			return nil, fmt.Errorf("seeds: select never-checked: %w", qErr)
		}
		neverChecked = append(neverChecked, got...)
		if rawN == neverLimit && len(lastH) > 0 {
			nextNeverH = lastH
		} else if len(startNeverH) > 0 {
			// Exhausted the tail past the cursor; wrap cleanly to the start.
			nextNeverH = nil
			if rem := neverLimit - len(neverChecked); rem > 0 {
				wrapGot, wH, wRaw, wErr := s.queryNeverChecked(ctx, pool, rem, nil, seen)
				if wErr != nil {
					return nil, fmt.Errorf("seeds: select never-checked wrap: %w", wErr)
				}
				neverChecked = append(neverChecked, wrapGot...)
				if wRaw == rem && len(wH) > 0 {
					nextNeverH = wH
				}
			}
		}
	}

	extraLimit := limit - len(priorityNever) - len(priorityRecheck) - len(rechecks) - len(neverChecked)
	if extraLimit > 0 {
		got, lastAt, lastH, rawN, qErr := s.queryRechecks(ctx, pool, minAge, extraLimit, nextRecheckAt, nextRecheckH, seen)
		if qErr != nil {
			return nil, fmt.Errorf("seeds: select extra rechecks: %w", qErr)
		}
		rechecks = append(rechecks, got...)
		if rawN == extraLimit && !lastAt.IsZero() && len(lastH) > 0 {
			nextRecheckAt = lastAt
			nextRecheckH = lastH
		} else if !nextRecheckAt.IsZero() && len(nextRecheckH) > 0 {
			nextRecheckAt = time.Time{}
			nextRecheckH = nil
			if rem := extraLimit - len(got); rem > 0 {
				wrapGot, wAt, wH, wRaw, wErr := s.queryRechecks(ctx, pool, minAge, rem, time.Time{}, nil, seen)
				if wErr != nil {
					return nil, fmt.Errorf("seeds: select extra rechecks wrap: %w", wErr)
				}
				rechecks = append(rechecks, wrapGot...)
				if wRaw == rem && !wAt.IsZero() && len(wH) > 0 {
					nextRecheckAt = wAt
					nextRecheckH = wH
				}
			}
		}
	}

	s.cursorMu.Lock()
	// Only advance cursors if they were not concurrently overwritten by another caller.
	if s.recheckCursorAt.Equal(startRecheckAt) && bytes.Equal(s.recheckCursorH, startRecheckH) {
		s.recheckCursorAt = nextRecheckAt
		s.recheckCursorH = append([]byte(nil), nextRecheckH...)
	}
	if bytes.Equal(s.neverCursorH, startNeverH) {
		s.neverCursorH = append([]byte(nil), nextNeverH...)
	}
	s.cursorMu.Unlock()

	// Preserve never-checked first, followed by oldest rechecks, with approved
	// priority hashes leading each class.
	out := make([][]byte, 0, len(priorityNever)+len(neverChecked)+len(priorityRecheck)+len(rechecks))
	out = append(out, priorityNever...)
	out = append(out, neverChecked...)
	out = append(out, priorityRecheck...)
	out = append(out, rechecks...)
	return out, nil
}

// Every write is guarded by `where exists (select 1 from torrents ...)` so a
// torrent deleted by retention/junkpurge between select and persist yields a
// no-op instead of an FK violation that would fail the whole batch.
const ledgerUpsertSQL = `
insert into torrent_tracker_seeds
  (info_hash, tracker_known, seeders, leechers, completed, best_tracker, checked_at,
   peak_seeders, last_positive_at)
select $1, $2, $3::int, $4::int, $5::int, $6, now(),
   $3::int, case when $3::int > 0 or $4::int > 0 then now() end
where exists (select 1 from torrents where info_hash = $1)
on conflict (info_hash) do update set
  tracker_known = excluded.tracker_known,
  -- history BEFORE overwriting: prev is the reading this scrape replaces;
  -- peak keeps the historical maximum; last_positive_at only advances on a
  -- live reading, preserving the "how long dead" clock through zeros and
  -- unknowns.
  prev_seeders     = torrent_tracker_seeds.seeders,
  peak_seeders     = case
                       when excluded.seeders is null then torrent_tracker_seeds.peak_seeders
                       else greatest(coalesce(torrent_tracker_seeds.peak_seeders, 0), excluded.seeders)
                     end,
  last_positive_at = case
                       when excluded.seeders > 0 or excluded.leechers > 0 then excluded.checked_at
                       else torrent_tracker_seeds.last_positive_at
                     end,
  seeders       = excluded.seeders,
  leechers      = excluded.leechers,
  completed     = excluded.completed,
  best_tracker  = excluded.best_tracker,
  checked_at    = excluded.checked_at`

const sourceUpsertSQL = `
insert into torrents_torrent_sources
  (source, info_hash, seeders, leechers, created_at, updated_at)
select 'tracker', $1, $2::int, $3::int, now(), now()
where exists (select 1 from torrents where info_hash = $1)
on conflict (source, info_hash) do update set
  seeders    = excluded.seeders,
  leechers   = excluded.leechers,
  updated_at = now()`

const sourceDeleteSQL = `
delete from torrents_torrent_sources where source = 'tracker' and info_hash = $1`

// denormSyncSQL recomputes the denormalized torrent_contents.seeders/leechers
// for a batch of hashes to exactly what the next classify/persist would set,
// mirroring model.Torrent.Seeders(): the authoritative 'tracker' source row
// wins outright when it carries a count (including 0 for a known-dead swarm);
// MAX across the remaining sources applies only when there is no tracker
// verdict. Per-column coalesce keeps the two columns independent, matching
// the Go read.
// The UPDATE is driven from the cycle's hash list (unnest) and LEFT JOINs the
// per-hash aggregate, so a hash whose only source row was just cleared (a
// tracker-unknown with no remaining source) has no aggregate row and is set
// to NULL rather than left carrying a stale positive count.
// Deliberately does not touch updated_at — that column carries classify-time
// staleness semantics — and tc.tsv is generated from search_string only, so
// this update never churns the FTS index.
const denormSyncSQL = `
update torrent_contents tc set
  seeders  = agg.seeders,
  leechers = agg.leechers
from (
  select h.info_hash, mx.seeders, mx.leechers
  from unnest($1::bytea[]) as h(info_hash)
  left join (
    select info_hash,
      coalesce(
        max(seeders)  filter (where source = 'tracker'),
        max(seeders)  filter (where source <> 'tracker')
      ) as seeders,
      coalesce(
        max(leechers) filter (where source = 'tracker'),
        max(leechers) filter (where source <> 'tracker')
      ) as leechers
    from torrents_torrent_sources
    where info_hash = any($1)
    group by info_hash
  ) mx on mx.info_hash = h.info_hash
) agg
where tc.info_hash = agg.info_hash
  and (tc.seeders is distinct from agg.seeders or tc.leechers is distinct from agg.leechers)`

// Persist writes one scrape cycle's results: the ledger row for every hash,
// plus a mirrored authoritative source row for every tracker-known hash —
// positive counts as before, and 0/0 for a known-zero verdict so downstream
// readers (the torznab authoritative-zero filter, Torrent.Seeders()) can tell
// a real dead swarm from a bloom-approximated zero. A tracker-unknown result
// clears any stale source row: honest-unknown leaves no 'tracker' row at all.
// Returns how many source rows were actually upserted and cleared. hashes
// fixes the iteration order so batch results map back deterministically.
func (s *Store) Persist(ctx context.Context, hashes [][]byte, outcomes map[string]*ScrapeOutcome) (upserted, cleared int, err error) {
	err = s.withAdmission(ctx, hashes, true, func(tx pgx.Tx, allowed [][]byte) error {
		var e error
		upserted, cleared, e = persist(ctx, tx, allowed, outcomes)
		return e
	})
	if err != nil {
		return 0, 0, err
	}
	return
}

func persist(ctx context.Context, tx pgx.Tx, hashes [][]byte, outcomes map[string]*ScrapeOutcome) (upserted, cleared int, err error) {

	const (
		opUpsert = iota
		opDelete
	)
	batch := &pgx.Batch{}
	kinds := make([]int, 0, len(hashes))

	for _, h := range hashes {
		o := outcomes[string(h)]
		var seeders, leechers, completed *int32
		var best *string
		trackerKnown := false
		if o != nil {
			trackerKnown = o.TrackerKnown
			if o.TrackerKnown {
				sv, lv, cv := o.Seeders, o.Leechers, o.Completed
				seeders, leechers, completed = &sv, &lv, &cv
				if o.BestTracker != "" {
					bt := o.BestTracker
					best = &bt
				}
			}
		}
		batch.Queue(ledgerUpsertSQL, h, trackerKnown, seeders, leechers, completed, best)

		if o != nil && o.TrackerKnown {
			batch.Queue(sourceUpsertSQL, h, o.Seeders, o.Leechers)
			kinds = append(kinds, opUpsert)
		} else {
			batch.Queue(sourceDeleteSQL, h)
			kinds = append(kinds, opDelete)
		}
	}

	br := tx.SendBatch(ctx, batch)
	defer br.Close()

	for i := range hashes {
		if _, e := br.Exec(); e != nil { // ledger upsert
			return upserted, cleared, fmt.Errorf("seeds: ledger upsert: %w", e)
		}
		ct, e := br.Exec() // source op
		if e != nil {
			return upserted, cleared, fmt.Errorf("seeds: source op: %w", e)
		}
		if ct.RowsAffected() > 0 {
			if kinds[i] == opUpsert {
				upserted++
			} else {
				cleared++
			}
		}
	}
	return upserted, cleared, nil
}

// SyncDenormalizedCounts refreshes torrent_contents.seeders/leechers for the
// cycle's hashes from the just-written source rows. Without this the
// denormalized columns (what torznab ordering, the zero-seeder filter, and the
// UI read) stay frozen at classify time — measured at 62% drift against the
// ledger before this existed. Returns how many rows actually changed.
func (s *Store) SyncDenormalizedCounts(ctx context.Context, hashes [][]byte) (int64, error) {
	if len(hashes) == 0 {
		return 0, nil
	}

	var synced int64
	err := s.withAdmission(ctx, hashes, true, func(tx pgx.Tx, allowed [][]byte) error {
		ct, e := tx.Exec(ctx, denormSyncSQL, allowed)
		synced = ct.RowsAffected()
		return e
	})
	if err != nil {
		return 0, err
	}
	return synced, nil
}

// CoverageCounts returns standing ledger coverage for the gauges: total rows
// checked, rows a tracker knows about, and rows currently carrying a positive
// (live) count.
func (s *Store) CoverageCounts(ctx context.Context) (ledger, trackerKnown, positive int64, err error) {
	pool, err := s.pool.Get()
	if err != nil {
		return 0, 0, 0, fmt.Errorf("seeds: acquire pool: %w", err)
	}
	const q = `
select
  count(*),
  count(*) filter (where tracker_known),
  count(*) filter (where seeders > 0 or leechers > 0)
from torrent_tracker_seeds`
	err = pool.QueryRow(ctx, q).Scan(&ledger, &trackerKnown, &positive)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("seeds: coverage counts: %w", err)
	}
	return ledger, trackerKnown, positive, nil
}
