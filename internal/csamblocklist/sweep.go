package csamblocklist

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/spencernorton/DH-Waypoint/internal/model"
)

// SweepMode is CSAM_SWEEP_MODE: what the clean-up of rows stored before the
// banned-keyword check existed does with a match.
type SweepMode string

const (
	// SweepApply removes a match the way RemoveStored does. The default.
	SweepApply SweepMode = "apply"
	// SweepDryRun counts matches and changes nothing.
	SweepDryRun SweepMode = "dry_run"
	// SweepOff reads nothing.
	SweepOff SweepMode = "off"
)

// SweepConfig is the `csam_sweep` configuration section.
type SweepConfig struct {
	Mode SweepMode `validate:"oneof=apply dry_run off"`
}

func NewDefaultSweepConfig() SweepConfig {
	return SweepConfig{Mode: SweepApply}
}

const (
	// OutcomeWouldRemove and OutcomeWouldHide are what a dry run counts where
	// apply would report OutcomeRemoved and OutcomeHiddenProtected.
	OutcomeWouldRemove Outcome = "would_remove"
	OutcomeWouldHide   Outcome = "would_hide_protected"

	sweepStateKey  = "csam_sweep"
	sweepBatchRows = 1000
	// The sweep rests after a batch as long as the batch took, and at least
	// this long, so it never uses more than half of one connection.
	sweepPause = 10 * time.Millisecond
)

// sweepRetry is how long the sweep waits after a failed batch.
var sweepRetry = 30 * time.Second

// Sweeper is what csamblocklistfx starts next to the refresh loop.
type Sweeper interface {
	RunSweep(ctx context.Context, mode SweepMode)
}

var _ Sweeper = (*service)(nil)

// gone is true for a side row whose torrent is no longer stored, under either
// of its hashes. Removing a stored torrent cleans its side rows, so a side
// row is tested only when this holds.
func gone(alias string) string {
	return `(not exists(select 1 from torrents t where t.info_hash=` + alias + `.info_hash)
  and not exists(select 1 from torrents t where t.info_hash_v2=` + alias + `.info_hash))`
}

// sweepSources are the tables that can hold a torrent's name or file paths
// without help: torrents, and every table where a copy outlives the torrents
// row. Each query returns the rows after key $1 in key order, at most $2, as
// (key, info_hash, info_hash_v2, name, files_status, paths, test). start is
// the key before the first row.
//
// The only filter is the key range, so every batch is one bounded walk of the
// primary key. Whether a row is tested is a column: as a filter it made the
// planner scan the whole table for each batch.
var sweepSources = []struct{ table, start, sql string }{
	{"torrents", "", `select encode(t.info_hash,'hex'),t.info_hash,t.info_hash_v2,t.name,t.files_status::text,
 array(select f.path from torrent_files f where f.info_hash=t.info_hash),true
 from torrents t where t.info_hash>decode($1,'hex') order by t.info_hash limit $2`},
	// The legacy junk purge deleted the torrents row and kept a full copy.
	{"junkpurge_quarantine", "", `select encode(q.info_hash,'hex'),q.info_hash,null::bytea,
 coalesce(q.torrent_snapshot->>'name',q.torrent_name),coalesce(q.torrent_snapshot->>'files_status',''),
 array(select coalesce(e->>'path','') from jsonb_array_elements(case when jsonb_typeof(q.files_snapshot)='array' then q.files_snapshot else '[]' end) e),` + gone("q") + `
 from junkpurge_quarantine q where q.info_hash>decode($1,'hex') order by q.info_hash limit $2`},
	{"junkpurge_judgments", "", `select encode(j.info_hash,'hex'),j.info_hash,null::bytea,coalesce(j.torrent_name,''),'','{}'::text[],` + gone("j") + `
 from junkpurge_judgments j where j.info_hash>decode($1,'hex') order by j.info_hash limit $2`},
	{"junkpurge_batch_items", "0:0", `select b.run_id||':'||b.ordinal,b.info_hash,null::bytea,b.torrent_name,'','{}'::text[],` + gone("b") + `
 from junkpurge_batch_items b where (b.run_id,b.ordinal)>(split_part($1,':',1)::bigint,split_part($1,':',2)::int)
 order by b.run_id,b.ordinal limit $2`},
	// A refused restore leaves its snapshot behind.
	{"catalogue_recovery_snapshots", "0", `select s.id::text,s.info_hash,null::bytea,
 coalesce(s.snapshot#>>'{torrents,0,name}',''),coalesce(s.snapshot#>>'{torrents,0,files_status}',''),
 array(select coalesce(e->>'path','') from jsonb_array_elements(case when jsonb_typeof(s.snapshot->'torrent_files')='array' then s.snapshot->'torrent_files' else '[]' end) e),` + gone("s") + `
 from catalogue_recovery_snapshots s where s.id>($1::text)::bigint order by s.id limit $2`},
	// Earlier releases sampled the name of a torrent the classifier deleted
	// into the evidence of its verdict. The reason is not tested: a fixed
	// reason that spells a keyword would match every row.
	{"torrent_verdict_events", "0", `select v.id::text,v.info_hash,null::bytea,coalesce(v.evidence->>'name',''),'','{}'::text[],
 case when jsonb_typeof(v.evidence)='object' and v.evidence ? 'name' then ` + gone("v") + ` else false end
 from torrent_verdict_events v where v.id>($1::text)::bigint order by v.id limit $2`},
	{"eval_frozen", ":", `select encode(convert_to(f.segment,'UTF8'),'hex')||':'||encode(f.info_hash,'hex'),f.info_hash,null::bytea,f.name,'','{}'::text[],` + gone("f") + `
 from eval_frozen f where (f.segment,f.info_hash)>(convert_from(decode(split_part($1,':',1),'hex'),'UTF8'),decode(split_part($1,':',2),'hex'))
 order by f.segment,f.info_hash limit $2`},
}

// sweepState is the saved position and the totals of one pass. It lives in
// key_values so a restart continues where the last batch ended and /metrics
// shows the whole pass, not what this process saw of it.
type sweepState struct {
	// List and Mode say which pass this is. Another list or mode starts over.
	List string    `json:"list"`
	Mode SweepMode `json:"mode"`
	// Table is the source being read, Key the last key handled in it.
	Table   string                       `json:"table"`
	Key     string                       `json:"key"`
	Scanned map[string]int64             `json:"scanned"`
	Matches map[string]map[Outcome]int64 `json:"matches"`
	Done    bool                         `json:"done"`
}

// sweepList names the compiled list and the queries that read the sources, so
// a release that changes either runs the sweep again.
var sweepList = sync.OnceValue(func() string {
	h := sha256.New()
	h.Write([]byte(bannedKeywords().String()))
	for _, src := range sweepSources {
		h.Write([]byte(src.table + src.start + src.sql))
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
})

// RunSweep reads every stored copy of a torrent name once and hands each
// match of the built-in banned-keyword list to the removal, or only counts it
// in a dry run. It returns when the pass is complete or ctx ends. A failed
// batch is tried again from the same row: the sweep waits, it does not skip.
func (s *service) RunSweep(ctx context.Context, mode SweepMode) {
	if mode == SweepOff || s.pool == nil {
		return
	}
	s.logger.Infow("banned-keyword sweep of stored rows started", "mode", mode)
	for {
		started := time.Now()
		st, err := s.sweepBatch(ctx, mode, sweepBatchRows)
		wait := max(sweepPause, time.Since(started))
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			// The key is a hash, so only the table is logged.
			s.logger.Warnw("banned-keyword sweep batch failed; it will be tried again", "table", st.Table, "err", err)
			if s.metrics != nil {
				s.metrics.SweepBatchErrorsTotal.Inc()
			}
			wait = sweepRetry
		case st.Done:
			s.logger.Infow("banned-keyword sweep of stored rows complete", "mode", mode, "scanned", st.Scanned, "matches", st.Matches)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// sweepBatch handles the next limit rows and saves the position. On an error
// it saves what it finished and stops before the row that failed.
func (s *service) sweepBatch(ctx context.Context, mode SweepMode, limit int) (st sweepState, err error) {
	pool, err := s.pool.Get()
	if err != nil {
		return st, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return st, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// One sweeper at a time, in whichever process: the state is read and
	// written under this lock.
	if _, err = tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext($1))`, sweepStateKey); err != nil {
		return st, err
	}
	var saved string
	switch err = tx.QueryRow(ctx, `select value from key_values where key=$1`, sweepStateKey).Scan(&saved); {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return st, err
	default:
		_ = json.Unmarshal([]byte(saved), &st) // unreadable state starts over
	}
	at := slices.IndexFunc(sweepSources, func(src struct{ table, start, sql string }) bool { return src.table == st.Table })
	if st.List != sweepList() || st.Mode != mode || st.Scanned == nil || st.Matches == nil || at < 0 {
		at = 0
		st = sweepState{List: sweepList(), Mode: mode, Table: sweepSources[0].table, Key: sweepSources[0].start,
			Scanned: map[string]int64{}, Matches: map[string]map[Outcome]int64{}}
	}
	var batchErr error
	if !st.Done {
		batchErr = s.sweepRows(ctx, tx, &st, at, limit)
	}
	value, err := json.Marshal(st)
	if err != nil {
		return st, err
	}
	// A shutdown must not lose the count of what this batch already removed.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if _, err = tx.Exec(saveCtx, `insert into key_values(key,value,created_at,updated_at) values($1,$2,now(),now())
 on conflict(key) do update set value=excluded.value,updated_at=excluded.updated_at`, sweepStateKey, string(value)); err != nil {
		return st, errors.Join(batchErr, err)
	}
	if err = tx.Commit(saveCtx); err != nil {
		return st, errors.Join(batchErr, err)
	}
	if s.metrics != nil {
		s.metrics.setSweep(st)
	}
	return st, batchErr
}

func (s *service) sweepRows(ctx context.Context, tx pgx.Tx, st *sweepState, at, limit int) error {
	type row struct {
		key, name, status string
		hash, v2          []byte
		paths             []string
		test              bool
	}
	found, err := tx.Query(ctx, sweepSources[at].sql, st.Key, limit)
	if err != nil {
		return err
	}
	rows, err := pgx.CollectRows(found, func(r pgx.CollectableRow) (out row, err error) {
		return out, r.Scan(&out.key, &out.hash, &out.v2, &out.name, &out.status, &out.paths, &out.test)
	})
	if err != nil {
		return err
	}
	// A removal cleans every copy of its hash, but a second copy read in the
	// same batch still holds the name in memory.
	removed := map[string]bool{}
	for _, r := range rows {
		t := model.Torrent{Name: r.name, FilesStatus: model.FilesStatus(r.status)}
		for _, p := range r.paths {
			t.Files = append(t.Files, model.TorrentFile{Path: p})
		}
		if r.test && !removed[string(r.hash)] && MatchesBannedKeyword(t) {
			hashes := [][]byte{r.hash}
			if r.v2 != nil {
				hashes = append(hashes, r.v2)
			}
			outcome, err := s.sweepMatch(ctx, tx, st.Mode, hashes)
			if err != nil {
				return err
			}
			if outcome != "" {
				if st.Matches[st.Table] == nil {
					st.Matches[st.Table] = map[Outcome]int64{}
				}
				st.Matches[st.Table][outcome]++
			}
			removed[string(r.hash)] = outcome == OutcomeRemoved
		}
		st.Scanned[st.Table]++
		st.Key = r.key
	}
	if len(rows) < limit {
		if at+1 == len(sweepSources) {
			st.Done = true
		} else {
			st.Table, st.Key = sweepSources[at+1].table, sweepSources[at+1].start
		}
	}
	return nil
}

func (s *service) sweepMatch(ctx context.Context, tx pgx.Tx, mode SweepMode, hashes [][]byte) (Outcome, error) {
	if mode == SweepDryRun {
		protected, err := protectedTx(ctx, tx, hashes)
		if err != nil {
			return "", err
		}
		if protected {
			return OutcomeWouldHide, nil
		}
		return OutcomeWouldRemove, nil
	}
	outcome, err := s.removeMatched(ctx, "sweep", hashes)
	if errors.Is(err, ErrNotBannedKeyword) {
		// The stored row was rewritten after the batch read it, and no longer matches.
		return "", nil
	}
	return outcome, err
}
