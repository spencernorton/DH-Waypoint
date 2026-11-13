package processors

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/spencernorton/DH-Waypoint/internal/serving"
	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
)

type Receipt struct {
	Result     v1.Result      `json:"result"`
	Candidates []v1.Candidate `json:"candidates"`
}
type Lease struct {
	ID                              string
	Hash                            []byte
	InputRevision, PipelineRevision string
	Epoch, Generation               int64
	PublicationRevision             int64
	Input                           v1.Input
	Source                          json.RawMessage
	Plan                            Plan
	Results                         []Receipt
	Step, StepAttempts              int
	AvailableAt                     time.Time
}

// Scan is a bounded, restartable catch-up sweep, independent of core queue_jobs.
// The control row serializes admission across replicas. With no enabled service
// it performs no catalogue scan and makes no network request.
func (s *Store) Scan(ctx context.Context) (int, error) {
	s.pruneOutbox(ctx)
	ctx, c := context.WithTimeout(ctx, 2*time.Second)
	defer c()
	tx, e := s.begin(ctx)
	if e != nil {
		return 0, e
	}
	defer rollback(tx)
	var after []byte
	if e = tx.QueryRow(ctx, `SELECT scan_after FROM processor_control WHERE singleton FOR UPDATE`).Scan(&after); e != nil {
		return 0, e
	}
	locked := time.Now()
	if s.publication.RequiredProcessor != "" {
		var configured string
		if e = tx.QueryRow(ctx, `SELECT publication_policy FROM processor_control WHERE singleton`).Scan(&configured); e != nil {
			return 0, e
		}
		current := digest([]any{serving.PublicationReviewPolicy, s.publication.RequiredProcessor, s.publication.RequiredProcessorArtifactDigest, s.publication.RequiredPayloadVerifierArtifactDigest})
		if configured != current {
			if _, e = tx.Exec(ctx, `UPDATE processor_control SET publication_policy=$1,scan_after=''::bytea WHERE singleton`, current); e != nil {
				return 0, e
			}
			after = []byte{}
		}
	}
	plan, draining, e := readPlan(ctx, tx, s.policy)
	if e != nil {
		return 0, e
	}
	s.active.Store(len(plan.Steps) > 0)
	if e = s.settleReprocessing(ctx, tx, plan, draining); e != nil {
		return 0, e
	}
	if len(plan.Steps) == 0 || draining || plan.Admission.BasisPoints == 0 {
		return 0, tx.Commit(ctx)
	}
	e = pruneJobs(ctx, tx, s.cfg.Retention)
	if e != nil {
		return 0, e
	}
	var pending, total int
	if e = tx.QueryRow(ctx, `SELECT count(*) FILTER(WHERE state IN('queued','deferred','leased')),count(*) FROM processor_jobs`).Scan(&pending, &total); e != nil {
		return 0, e
	}
	// Completed audit history must not prevent a fresh release from entering
	// the bounded work queue. Eviction advances the honest history floor.
	room := min(s.cfg.ScanBatch, s.cfg.QueueCapacity-pending)
	if room > 0 && total+room > s.cfg.RetainedJobs {
		removed, err := makePublicationRoom(ctx, tx, min(128, total+room-s.cfg.RetainedJobs))
		if err != nil {
			return 0, err
		}
		total -= removed
	}
	capacity := min(s.cfg.ScanBatch, s.cfg.QueueCapacity-pending, s.cfg.RetainedJobs-total)
	if capacity <= 0 {
		s.metrics.admission.WithLabelValues("full").Inc()
		return 0, tx.Commit(ctx)
	}
	// Explicit requests that completed ordinary processing get bounded priority
	// without moving the catalogue catch-up cursor past unrelated torrents.
	priority := map[string]bool{}
	var hashes [][]byte
	rows, e := tx.Query(ctx, `SELECT info_hash FROM processor_reprocess_requests WHERE state='ready' ORDER BY created_at LIMIT $1`, min(16, capacity))
	if e != nil {
		return 0, e
	}
	for rows.Next() {
		var h []byte
		if e = rows.Scan(&h); e != nil {
			rows.Close()
			return 0, e
		}
		hashes = append(hashes, h)
		priority[string(h)] = true
	}
	rows.Close()
	if e = rows.Err(); e != nil {
		return 0, e
	}
	requested := len(hashes)
	// Source changes and newly persisted releases precede hash-order catch-up.
	// Keep half a scan for backfill; explicit requests retain their own priority.
	rows, e = tx.Query(ctx, `SELECT p.info_hash FROM processor_publication_pending p
 WHERE EXISTS(SELECT 1 FROM torrent_contents c WHERE c.info_hash=p.info_hash)
 AND NOT EXISTS(SELECT 1 FROM processor_reprocess_requests r WHERE r.info_hash=p.info_hash AND r.state='waiting_core')
 ORDER BY p.enqueued_at,p.info_hash LIMIT $1`, min(16, max(0, (capacity-len(hashes)+1)/2)))
	if e != nil {
		return 0, e
	}
	for rows.Next() {
		var h []byte
		if e = rows.Scan(&h); e != nil {
			rows.Close()
			return 0, e
		}
		if !priority[string(h)] {
			hashes = append(hashes, h)
			priority[string(h)] = true
		}
	}
	rows.Close()
	if e = rows.Err(); e != nil {
		return 0, e
	}
	changed := len(hashes)
	// The repeated cursor bound keeps both probes on index ranges: without it
	// the plan merge-joins from the start of the hash space.
	catchUp := `SELECT t.info_hash FROM torrents t WHERE t.info_hash>$1 AND EXISTS(SELECT 1 FROM torrent_contents c WHERE c.info_hash=t.info_hash AND c.info_hash>$1)
 AND NOT EXISTS(SELECT 1 FROM processor_reprocess_requests r WHERE r.info_hash=t.info_hash AND r.state='waiting_core' AND r.info_hash>$1)
 ORDER BY t.info_hash LIMIT $2`
	if s.publication.RequiredProcessor != "" {
		// Torrents that fail fileEvidenceSQL are left out: a source trigger
		// queues them when their files arrive.
		catchUp = strings.Replace(catchUp, `WHERE `, `WHERE `+fileEvidenceSQL+` AND `, 1)
	}
	rows, e = tx.Query(ctx, catchUp, after, s.cfg.ScanBatch-len(hashes))
	if e != nil {
		return 0, e
	}
	normalCount := 0
	for rows.Next() {
		var h []byte
		if e = rows.Scan(&h); e != nil {
			rows.Close()
			return 0, e
		}
		normalCount++
		if !priority[string(h)] {
			hashes = append(hashes, h)
		}
	}
	rows.Close()
	if e = rows.Err(); e != nil {
		return 0, e
	}
	admitted, bounded := 0, false
	for i := 0; i < len(hashes); i++ {
		// Bound the hold, but process at least the first hash of each class
		// (requests, source changes, catch-up) so every class advances. The
		// cursor only moves past processed hashes.
		if i != 0 && i != requested && i != changed && time.Since(locked) > s.scanHold {
			bounded = true
			if i < requested {
				i = requested - 1
				continue
			}
			if i < changed {
				i = changed - 1
				continue
			}
			break
		}
		h := hashes[i]
		if !priority[string(h)] {
			after = h
		}
		if !admittedByPolicy(h, plan.Admission) {
			if _, err := tx.Exec(ctx, `DELETE FROM processor_publication_pending WHERE info_hash=$1`, h); err != nil {
				return 0, err
			}
			continue
		}
		src, raw, err := readSource(ctx, tx, h, false)
		if errors.Is(err, errSourceTooLarge) {
			s.metrics.admission.WithLabelValues("oversize").Inc()
			if _, err = tx.Exec(ctx, `DELETE FROM processor_publication_pending WHERE info_hash=$1 AND source_revision<=$2`, h, src.PublicationRevision); err != nil {
				return 0, err
			}
			if _, err = tx.Exec(ctx, `UPDATE processor_reprocess_requests SET state='held',state_reason='source_too_large',updated_at=clock_timestamp() WHERE info_hash=$1 AND state='ready'`, h); err != nil {
				return 0, err
			}
			continue
		}
		if errors.Is(err, pgx.ErrNoRows) {
			// The torrent is gone. Hold its request, as settleReprocessing does for
			// a waiting one, instead of failing every tick until the request expires.
			if _, err = tx.Exec(ctx, `UPDATE processor_reprocess_requests SET state='held',state_reason='source_unavailable',updated_at=clock_timestamp() WHERE info_hash=$1 AND state='ready'`, h); err != nil {
				return 0, err
			}
			continue
		}
		if err != nil {
			return 0, err
		}
		if !src.admitted() {
			if priority[string(h)] {
				if _, err = tx.Exec(ctx, `UPDATE processor_reprocess_requests SET state='held',state_reason='current_authority_denied',updated_at=clock_timestamp() WHERE info_hash=$1 AND state='ready'`, h); err != nil {
					return 0, err
				}
			}
			if _, err = tx.Exec(ctx, `DELETE FROM processor_publication_pending WHERE info_hash=$1 AND source_revision<=$2`, h, src.PublicationRevision); err != nil {
				return 0, err
			}
			continue
		}
		if !src.admittedByNames(s.names, h) {
			if priority[string(h)] {
				if _, err = tx.Exec(ctx, `UPDATE processor_reprocess_requests SET state='held',state_reason='current_authority_denied',updated_at=clock_timestamp() WHERE info_hash=$1 AND state='ready'`, h); err != nil {
					return 0, err
				}
			}
			if _, err = tx.Exec(ctx, `DELETE FROM processor_publication_pending WHERE info_hash=$1 AND source_revision<=$2`, h, src.PublicationRevision); err != nil {
				return 0, err
			}
			continue
		}
		if s.publication.RequiredProcessor != "" {
			if reason := fileEvidenceReason(src); reason != "" {
				// The required review could not approve it, so it is not sent.
				// A source trigger queues it again when its file evidence changes.
				s.metrics.admission.WithLabelValues("file_evidence").Inc()
				if priority[string(h)] {
					if _, err = tx.Exec(ctx, `UPDATE processor_reprocess_requests SET state='held',state_reason=$2,updated_at=clock_timestamp() WHERE info_hash=$1 AND state='ready'`, h, reason); err != nil {
						return 0, err
					}
				}
				if _, err = tx.Exec(ctx, `DELETE FROM processor_publication_pending WHERE info_hash=$1 AND source_revision<=$2`, h, src.PublicationRevision); err != nil {
					return 0, err
				}
				continue
			}
		}
		effectivePlan := planForSource(planForIdentity(plan, h), src)
		if len(effectivePlan.Steps) == 0 {
			if priority[string(h)] {
				// No active step applies to this torrent. Settle it as core_only, as
				// settleReprocessing does for a waiting one with no active plan: a
				// request left ready would head its class every tick and, once the
				// hold bound stops the class after its first hash, block the rest.
				if _, err = tx.Exec(ctx, `UPDATE processor_reprocess_requests SET state='core_only',state_reason='no_applicable_processors',updated_at=clock_timestamp() WHERE info_hash=$1 AND state='ready'`, h); err != nil {
					return 0, err
				}
			}
			if _, err = tx.Exec(ctx, `DELETE FROM processor_publication_pending WHERE info_hash=$1 AND source_revision<=$2`, h, src.PublicationRevision); err != nil {
				return 0, err
			}
			continue
		}
		// Pipeline identity describes the configured registry. Source authority
		// may narrow execution to policy-only without changing that identity.
		revision, pipeline := digest(raw), digest(planForIdentity(plan, h))
		var already bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM processor_applications WHERE info_hash=$1 AND applied_revision=$2 AND pipeline_revision=$3)`, h, revision, pipeline).Scan(&already)
		if err != nil {
			return 0, err
		}
		if already {
			already, err = s.publicationReviewsCurrent(ctx, tx, h, src.PublicationRevision, effectivePlan)
			if err != nil {
				return 0, err
			}
			if already {
				if err = s.enqueuePayload(ctx, tx, h, src.PublicationRevision); err != nil {
					return 0, err
				}
				if _, err = tx.Exec(ctx, `DELETE FROM processor_publication_pending WHERE info_hash=$1 AND source_revision<=$2`, h, src.PublicationRevision); err != nil {
					return 0, err
				}
				continue
			}
		}
		input := src.input(h, s.policy, s.noiseV2)
		ib, _ := json.Marshal(input)
		pb, _ := json.Marshal(effectivePlan)
		if len(ib) > 100000 {
			s.metrics.admission.WithLabelValues("oversize").Inc()
			if _, err = tx.Exec(ctx, `DELETE FROM processor_publication_pending WHERE info_hash=$1 AND source_revision<=$2`, h, src.PublicationRevision); err != nil {
				return 0, err
			}
			if _, err = tx.Exec(ctx, `UPDATE processor_reprocess_requests SET state='held',state_reason='input_too_large',updated_at=clock_timestamp() WHERE info_hash=$1 AND state='ready'`, h); err != nil {
				return 0, err
			}
			continue
		}
		id := digest([]any{hex.EncodeToString(h), revision, pipeline, src.PublicationRevision})
		tag, err := tx.Exec(ctx, `INSERT INTO processor_jobs(job_id,info_hash,input_revision,pipeline_revision,epoch,input,source,plan,publication_revision) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT DO NOTHING`, id, h, revision, pipeline, plan.Epoch, string(ib), string(raw), string(pb), src.PublicationRevision)
		if err != nil {
			return 0, err
		}
		admitted += int(tag.RowsAffected())
		if tag.RowsAffected() == 1 {
			_, err = tx.Exec(ctx, `UPDATE processor_reprocess_requests SET state='queued',result_job_id=$2,updated_at=clock_timestamp()
 WHERE info_hash=$1 AND state='ready'`, h, id)
			if err != nil {
				return 0, err
			}
		}
		if _, err = tx.Exec(ctx, `DELETE FROM processor_publication_pending WHERE info_hash=$1 AND source_revision<=$2`, h, src.PublicationRevision); err != nil {
			return 0, err
		}
		if admitted >= capacity {
			break
		}
	}
	if normalCount == 0 && len(priority) == 0 {
		after = []byte{}
	}
	_, e = tx.Exec(ctx, `UPDATE processor_control SET scan_after=$1 WHERE singleton`, after)
	if e != nil {
		return 0, e
	}
	e = tx.Commit(ctx)
	if e == nil {
		s.metrics.admission.WithLabelValues("queued").Add(float64(admitted))
		if bounded {
			s.metrics.admission.WithLabelValues("bounded").Inc()
		}
	}
	return admitted, e
}

// scanHoldBound is how long Scan keeps admitting hashes while it holds the
// control row. Each hash can cost 10-25 ms on the already-current path.
const scanHoldBound = 150 * time.Millisecond

// pruneOutbox removes rows Scan cannot admit from a bounded window of the
// publication outbox before Scan takes the control row: content-less rows
// and, with a required processor, rows whose torrent fails fileEvidenceSQL.
// A later contents insert or file-evidence change re-enrols the torrent
// through its trigger, so these rows only slow Scan's held outbox query.
// A failure skips the prune, never the Scan.
func (s *Store) pruneOutbox(ctx context.Context) {
	ctx, c := context.WithTimeout(ctx, 500*time.Millisecond)
	defer c()
	tx, e := s.begin(ctx)
	if e != nil {
		return
	}
	defer rollback(tx)
	gate := s.publication.RequiredProcessor != ""
	at, after := s.outboxCursor.get()
	rows, e := tx.Query(ctx, pruneWindowSQL, at, after, outboxPruneWindow, gate)
	if e != nil {
		return
	}
	var drop [][]byte
	n := 0
	for rows.Next() {
		var prunable bool
		if e = rows.Scan(&after, &at, &prunable); e != nil {
			rows.Close()
			return
		}
		n++
		if prunable {
			drop = append(drop, after)
		}
	}
	rows.Close()
	if rows.Err() != nil {
		return
	}
	if n < outboxPruneWindow {
		at, after = time.Time{}, nil
	}
	if len(drop) > 0 {
		// Lock, then delete in a new statement. The delete's snapshot sees
		// every contents insert or torrents update committed before it; a
		// later one's trigger waits on this row lock and re-enrols the torrent
		// once it is gone. One statement would miss a write committed between
		// its snapshot and its lock. Rows held by another transaction are skipped.
		var held [][]byte
		if e = queryHashes(ctx, tx, &held, pruneLockSQL, drop); e != nil {
			return
		}
		if _, e = tx.Exec(ctx, pruneDeleteSQL, held, gate); e != nil {
			return
		}
	}
	if tx.Commit(ctx) == nil {
		s.outboxCursor.set(at, after)
	}
}

const (
	outboxPruneWindow = 1024
	// fileEvidenceSQL is the part of fileEvidenceReason that a torrents row t
	// decides alone. Both columns are in the torrents source trigger.
	fileEvidenceSQL = `t.size>0 AND t.files_status IN('single','multi')`
	// The last parameter of the window and delete statements is the gate:
	// true with a required processor.
	pruneWindowSQL = `SELECT p.info_hash,p.enqueued_at,NOT EXISTS(SELECT 1 FROM torrent_contents c WHERE c.info_hash=p.info_hash)
 OR $4 AND EXISTS(SELECT 1 FROM torrents t WHERE t.info_hash=p.info_hash AND NOT (` + fileEvidenceSQL + `))
 FROM processor_publication_pending p WHERE (p.enqueued_at,p.info_hash)>($1,$2) ORDER BY p.enqueued_at,p.info_hash LIMIT $3`
	pruneLockSQL   = `SELECT info_hash FROM processor_publication_pending WHERE info_hash=ANY($1) FOR UPDATE SKIP LOCKED`
	pruneDeleteSQL = `DELETE FROM processor_publication_pending p WHERE p.info_hash=ANY($1) AND (NOT EXISTS(SELECT 1 FROM torrent_contents c WHERE c.info_hash=p.info_hash)
 OR $2 AND EXISTS(SELECT 1 FROM torrents t WHERE t.info_hash=p.info_hash AND NOT (` + fileEvidenceSQL + `)))`
	// requeueStaleSQL puts a torrent whose job went stale back into the outbox
	// at its current revision, with a required processor only: admission
	// deleted its row, and the catch-up pass may be weeks away. A torrent
	// without a processor_publication_sources row is at revision 1, as in
	// readSource; a deleted torrent gets no row. It writes no sources row, and
	// never calls processor_enqueue_publication: that raises the revision and
	// would void a current approval.
	requeueStaleSQL = `INSERT INTO processor_publication_pending(info_hash,source_revision)
 SELECT t.info_hash,COALESCE(s.revision,1) FROM torrents t LEFT JOIN processor_publication_sources s ON s.info_hash=t.info_hash
 WHERE t.info_hash=$1 ON CONFLICT DO NOTHING`
)

func queryHashes(ctx context.Context, tx pgx.Tx, out *[][]byte, sql string, args ...any) error {
	rows, e := tx.Query(ctx, sql, args...)
	if e != nil {
		return e
	}
	*out, e = pgx.CollectRows(rows, pgx.RowTo[[]byte])
	return e
}

// keyset is an in-memory resume point for a bounded walk over an
// (timestamp, info_hash) index. The zero value starts from the beginning.
type keyset struct {
	mu   sync.Mutex
	at   time.Time
	hash []byte
}

func (k *keyset) get() (time.Time, []byte) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.at, append([]byte{}, k.hash...)
}

func (k *keyset) set(at time.Time, hash []byte) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.at, k.hash = at, hash
}

func (s *Store) Claim(ctx context.Context) (*Lease, error) {
	ctx, c := context.WithTimeout(ctx, time.Second)
	defer c()
	tx, e := s.begin(ctx)
	if e != nil {
		return nil, e
	}
	defer rollback(tx)
	var epoch int64
	if e = tx.QueryRow(ctx, `SELECT epoch FROM processor_control WHERE singleton FOR UPDATE`).Scan(&epoch); e != nil {
		return nil, e
	}
	_, e = tx.Exec(ctx, `WITH expired AS (
 UPDATE processor_jobs SET state='queued',lease_until=NULL,reason='lease_expired'
 WHERE state='leased' AND lease_until<clock_timestamp() RETURNING job_id,lease_generation)
 UPDATE processor_attempts a SET finished_at=clock_timestamp(),outcome='failed',reason='lease_expired'
 FROM expired j WHERE j.job_id=a.job_id AND j.lease_generation=a.lease_generation AND a.finished_at IS NULL`)
	if e != nil {
		return nil, e
	}
	var busy int
	if e = tx.QueryRow(ctx, `SELECT count(*) FROM processor_jobs WHERE state='leased'`).Scan(&busy); e != nil {
		return nil, e
	}
	if busy >= s.cfg.Concurrency {
		return nil, tx.Commit(ctx)
	}
	var l Lease
	var ib, pb, rb []byte
	e = tx.QueryRow(ctx, `UPDATE processor_jobs SET state='leased',lease_generation=lease_generation+1,lease_until=clock_timestamp()+$1::interval
 WHERE job_id=(SELECT job_id FROM processor_jobs WHERE state IN('queued','deferred') AND available_at<=clock_timestamp() AND epoch=$2 ORDER BY available_at,created_at,job_id FOR UPDATE SKIP LOCKED LIMIT 1)
 RETURNING job_id,info_hash,input_revision,pipeline_revision,epoch,lease_generation,input,source,plan,results,step,step_attempts,available_at,publication_revision`, (s.cfg.RequestTimeout+5*time.Second).String(), epoch).Scan(&l.ID, &l.Hash, &l.InputRevision, &l.PipelineRevision, &l.Epoch, &l.Generation, &ib, &l.Source, &pb, &rb, &l.Step, &l.StepAttempts, &l.AvailableAt, &l.PublicationRevision)
	if errors.Is(e, pgx.ErrNoRows) {
		return nil, tx.Commit(ctx)
	}
	if e != nil {
		return nil, e
	}
	for _, x := range []struct {
		raw []byte
		out any
	}{{ib, &l.Input}, {pb, &l.Plan}, {rb, &l.Results}} {
		if e = json.Unmarshal(x.raw, x.out); e != nil {
			return nil, e
		}
	}
	if e = tx.Commit(ctx); e != nil {
		return nil, e
	}
	return &l, nil
}

func (s *Store) fresh(ctx context.Context, tx pgx.Tx, l Lease, lock bool) (source, error) {
	var epoch int64
	if e := tx.QueryRow(ctx, `SELECT epoch FROM processor_control WHERE singleton FOR SHARE`).Scan(&epoch); e != nil {
		return source{}, e
	}
	if epoch != l.Epoch || l.Plan.CorePolicy != s.policy {
		return source{}, ErrStale
	}
	src, raw, e := readSource(ctx, tx, l.Hash, lock)
	if errors.Is(e, errSourceTooLarge) {
		return src, ErrStale
	}
	if e != nil {
		return src, e
	}
	if !src.admitted() || digest(raw) != l.InputRevision || src.PublicationRevision != l.PublicationRevision {
		return src, ErrStale
	}
	if !src.admittedByNames(s.names, l.Hash) {
		return src, ErrStale
	}
	return src, nil
}

func (s *Store) matchesStepSelector(l Lease) (bool, error) {
	step := l.Plan.Steps[l.Step]
	if step.Entry.Selector.IsEmpty() {
		return true, nil
	}
	var src source
	if err := json.Unmarshal(l.Source, &src); err != nil {
		return false, err
	}
	tc := buildTorrentContext(l.Hash, src, l.Input, l.Results, l.Plan, l.Step)
	return matchesSelector(step.Entry.Selector, step.Entry.ID, tc), nil
}

// Prepare rechecks authority immediately before disclosure. No transaction is
// held across the service call. Stable step IDs are reused on every retry.
func (s *Store) Prepare(ctx context.Context, l Lease) (v1.Job, error) {
	step := l.Plan.Steps[l.Step]
	tx, e := s.begin(ctx)
	if e != nil {
		return v1.Job{}, e
	}
	defer rollback(tx)
	if _, e = s.fresh(ctx, tx, l, false); e != nil {
		return v1.Job{}, e
	}
	var frozen []byte
	var valid bool
	e = tx.QueryRow(ctx, `SELECT prepared_job,state='leased' AND lease_generation=$2 FROM processor_jobs WHERE job_id=$1 FOR UPDATE`, l.ID, l.Generation).Scan(&frozen, &valid)
	if e != nil {
		return v1.Job{}, e
	}
	if !valid {
		return v1.Job{}, ErrStale
	}
	if len(frozen) > 0 {
		var saved v1.Job
		if e = json.Unmarshal(frozen, &saved); e != nil {
			return saved, e
		}
		saved.Deadline = time.Now().Add(s.cfg.RequestTimeout).UTC().Format(time.RFC3339Nano)
		return saved, validateJob(saved)
	}
	var src source
	_ = json.Unmarshal(l.Source, &src)
	tc := buildTorrentContext(l.Hash, src, l.Input, l.Results, l.Plan, l.Step)
	in := l.Input
	in.PriorResults = []v1.Result{}
	for _, r := range l.Results {
		in.PriorResults = append(in.PriorResults, r.Result)
	}
	if len(tc.Scores) > 0 {
		in.AccumulatedScores = tc.Scores
	}
	if len(tc.Tags) > 0 {
		tags := make([]string, 0, len(tc.Tags))
		for t := range tc.Tags {
			tags = append(tags, t)
		}
		sort.Strings(tags)
		in.AccumulatedTags = tags
	}
	if slices.Contains(step.EffectiveCapabilities(), v1.Match) {
		in.Candidates, e = s.candidates(ctx, tx, in, l)
		if e != nil {
			return v1.Job{}, e
		}
	}
	// Swarm telemetry is a volatile hint frozen into this step's prepared
	// job. The stored InputRevision still digests only the stable source
	// snapshot, so later scrape updates cannot stale the lease or a
	// reprocess expectation; retries reuse the frozen prepared job.
	if srcs, sw := s.swarmAndSources(ctx, tx, l.Hash, src.Name, src.Extension); len(srcs) > 0 || sw != nil {
		in.Sources = srcs
		in.Swarm = sw
	}
	// Candidate content and all previous results are part of the step revision.
	revision := digest([]any{l.InputRevision, in})
	j := v1.Job{
		Binding: v1.Binding{
			Contract:            step.Entry.Manifest.Contract,
			JobID:               digest([]any{l.ID, l.Step, revision}),
			InputRevision:       revision,
			PipelineRevision:    l.PipelineRevision,
			ConfigurationDigest: digest(step.Entry.Configuration),
			Processor:           step.Entry.Manifest.Processor,
			Stage:               step.Stage,
		},
		Capabilities:  step.Capabilities,
		Configuration: step.Entry.Configuration,
		Input:         in,
		Deadline:      time.Now().Add(s.cfg.RequestTimeout).UTC().Format(time.RFC3339Nano),
	}
	if e = validateJob(j); e != nil {
		return j, e
	}
	raw, _ := json.Marshal(j)
	if len(raw) > v1.MaxBody {
		return j, errors.New("input_body_limit")
	}
	if _, e = tx.Exec(ctx, `UPDATE processor_jobs SET prepared_job=$2 WHERE job_id=$1`, l.ID, string(raw)); e != nil {
		return j, e
	}
	return j, tx.Commit(ctx)
}
func (s *Store) startAttempt(ctx context.Context, l Lease) error {
	tx, e := s.begin(ctx)
	if e != nil {
		return e
	}
	defer rollback(tx)
	tag, e := tx.Exec(ctx, `UPDATE processor_jobs SET attempts=attempts+1,step_attempts=step_attempts+1 WHERE job_id=$1 AND state='leased' AND lease_generation=$2 AND lease_until>clock_timestamp()`, l.ID, l.Generation)
	if e != nil {
		return e
	}
	if tag.RowsAffected() != 1 {
		return ErrStale
	}
	st := l.Plan.Steps[l.Step]
	wait := max(0, time.Since(l.AvailableAt).Seconds())
	_, e = tx.Exec(ctx, `INSERT INTO processor_attempts(job_id,lease_generation,processor_id,generation,stage,queue_wait_ms) VALUES($1,$2,$3,$4,$5,$6)`, l.ID, l.Generation, st.Entry.ID, st.Entry.ExecutionGeneration, string(st.Stage), wait*1000)
	if e != nil {
		return e
	}
	if e = tx.Commit(ctx); e != nil {
		return e
	}
	gen := strconv.FormatInt(st.Entry.ExecutionGeneration, 10)
	s.metrics.note(st.Entry.ID, gen)
	s.metrics.attempts.WithLabelValues(st.Entry.ID, gen).Inc()
	s.metrics.wait.WithLabelValues(st.Entry.ID, gen, string(st.Stage)).Observe(wait)
	return nil
}

// Finish is a compare-and-swap on the still-valid lease. Terminal results are
// applied once in this transaction, or not at all; duplicate/late responses lose.
func (s *Store) Finish(ctx context.Context, l Lease, j v1.Job, r v1.Result, dispatched bool) error {
	if e := validateResult(r, j); e != nil {
		return e
	}
	ctx, c := context.WithTimeout(ctx, 2*time.Second)
	defer c()
	tx, e := s.begin(ctx)
	if e != nil {
		return e
	}
	defer rollback(tx)
	// Registry locks precede job locks everywhere to avoid upgrade deadlocks.
	var epoch int64
	if e = tx.QueryRow(ctx, `SELECT epoch FROM processor_control WHERE singleton FOR SHARE`).Scan(&epoch); e != nil {
		return e
	}
	var valid bool
	e = tx.QueryRow(ctx, `SELECT state='leased' AND lease_generation=$2 AND lease_until>clock_timestamp() FROM processor_jobs WHERE job_id=$1 FOR UPDATE`, l.ID, l.Generation).Scan(&valid)
	if e != nil {
		return e
	}
	if !valid {
		return ErrStale
	}
	if epoch != l.Epoch {
		return ErrStale
	}
	if dispatched {
		reason := ""
		if len(r.Reasons) > 0 {
			reason = r.Reasons[0]
			if len(reason) > 128 {
				runes := []rune(reason)
				reason = string(runes[:min(128, len(runes))])
			}
		}
		_, e = tx.Exec(ctx, `UPDATE processor_attempts SET finished_at=clock_timestamp(),outcome=$3,reason=$4,inference_ms=$5 WHERE job_id=$1 AND lease_generation=$2`, l.ID, l.Generation, string(r.Outcome), reason, r.Telemetry.InferenceMS)
		if e != nil {
			return e
		}
	}
	policyRetry := l.Step < len(l.Plan.Steps) && l.Plan.Steps[l.Step].Entry.Mode == "apply" && slices.Contains(l.Plan.Steps[l.Step].EffectiveCapabilities(), v1.Policy)
	if (r.Outcome == v1.Failed || r.Outcome == v1.Deferred) && (l.StepAttempts+1 < s.cfg.MaxAttempts || policyRetry) && dispatched {
		delay := max(time.Second*time.Duration(1<<l.StepAttempts), time.Duration(r.RetryAfterSeconds)*time.Second)
		reset := policyRetry && l.StepAttempts+1 >= s.cfg.MaxAttempts
		if reset {
			delay = max(30*time.Second, time.Duration(r.RetryAfterSeconds)*time.Second)
		}
		_, e = tx.Exec(ctx, `UPDATE processor_jobs SET state='deferred',outcome=$3,reason='retry_scheduled',lease_until=NULL,step_attempts=CASE WHEN $5 THEN 0 ELSE step_attempts END,available_at=clock_timestamp()+$4::interval WHERE job_id=$1 AND lease_generation=$2`, l.ID, l.Generation, string(r.Outcome), delay.String(), reset)
		if e != nil {
			return e
		}
		return tx.Commit(ctx)
	}
	l.Results = append(l.Results, Receipt{r, j.Input.Candidates})
	raw, e := json.Marshal(l.Results)
	if e != nil {
		return e
	}
	if len(raw) > 900000 {
		return errors.New("pipeline_result_limit")
	}
	state, reason := "queued", ""
	if l.Step+1 == len(l.Plan.Steps) {
		src, err := s.fresh(ctx, tx, l, true)
		if err != nil {
			if errors.Is(err, ErrStale) || errors.Is(err, pgx.ErrNoRows) {
				state, reason = "stale", "source_changed"
			} else {
				return err
			}
		} else {
			if err = s.apply(ctx, tx, l, src); err != nil {
				return err
			}
			state = "completed"
		}
	}
	_, e = tx.Exec(ctx, `UPDATE processor_jobs SET results=$3,prepared_job=NULL,step=step+1,step_attempts=0,state=$4,reason=$5,outcome=CASE WHEN $4='queued' THEN '' ELSE $6 END,lease_until=NULL,available_at=clock_timestamp(),finished_at=CASE WHEN $4 IN('completed','stale') THEN clock_timestamp() END WHERE job_id=$1 AND lease_generation=$2`, l.ID, l.Generation, string(raw), state, reason, string(r.Outcome))
	if e != nil {
		return e
	}
	if state == "stale" && s.publication.RequiredProcessor != "" {
		if _, e = tx.Exec(ctx, requeueStaleSQL, l.Hash); e != nil {
			return e
		}
	}
	if state == "completed" || state == "stale" {
		_, e = tx.Exec(ctx, `UPDATE processor_reprocess_requests SET state='settled',state_reason=$2,updated_at=clock_timestamp()
 WHERE result_job_id=$1 AND state='queued'`, l.ID, state+":"+string(r.Outcome))
		if e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}

// abandon marks a leased job stale and, in the same statement, puts its
// torrent back into the outbox on requeueStaleSQL's terms: at its current
// revision, 1 without a sources row, and only while the torrent exists.
func (s *Store) abandon(ctx context.Context, l Lease, reason string) {
	p, e := s.pool.Get()
	if e != nil {
		return
	}
	_, _ = p.Exec(ctx, `WITH abandoned AS (
 UPDATE processor_jobs SET state='stale',reason=$3,lease_until=NULL,finished_at=clock_timestamp()
 WHERE job_id=$1 AND state='leased' AND lease_generation=$2 RETURNING job_id,lease_generation,info_hash),
 attempts AS (
 UPDATE processor_attempts a SET finished_at=clock_timestamp(),outcome='unknown',reason='source_or_input_invalid'
 FROM abandoned j WHERE a.job_id=j.job_id AND a.lease_generation=j.lease_generation AND a.finished_at IS NULL)
 INSERT INTO processor_publication_pending(info_hash,source_revision)
 SELECT t.info_hash,COALESCE(s.revision,1) FROM abandoned j JOIN torrents t ON t.info_hash=j.info_hash
 LEFT JOIN processor_publication_sources s ON s.info_hash=t.info_hash WHERE $4 ON CONFLICT DO NOTHING`, l.ID, l.Generation, reason, s.publication.RequiredProcessor != "")
}

// A preparation failure is local work, not a dispatched service attempt. Keep
// its bounded retry counter separate from plugin outcomes and retain the input.
func (s *Store) failPreparation(ctx context.Context, l Lease) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	var epoch int64
	if err = tx.QueryRow(ctx, `SELECT epoch FROM processor_control WHERE singleton FOR SHARE`).Scan(&epoch); err != nil {
		return err
	}
	if epoch != l.Epoch {
		return ErrStale
	}
	if l.Step < len(l.Plan.Steps) && l.Plan.Steps[l.Step].Entry.Mode == "apply" && slices.Contains(l.Plan.Steps[l.Step].EffectiveCapabilities(), v1.Policy) {
		tag, e := tx.Exec(ctx, `UPDATE processor_jobs SET preparation_failures=CASE WHEN preparation_failures+1 >= $3 THEN 0 ELSE preparation_failures+1 END,
 state='deferred',outcome='failed',reason='preparation_failed',lease_until=NULL,finished_at=NULL,
 available_at=clock_timestamp()+interval '30 seconds'
 WHERE job_id=$1 AND state='leased' AND lease_generation=$2 AND lease_until>clock_timestamp()`, l.ID, l.Generation, s.cfg.MaxAttempts)
		if e != nil {
			return e
		}
		if tag.RowsAffected() != 1 {
			return ErrStale
		}
		return tx.Commit(ctx)
	}
	tag, err := tx.Exec(ctx, `UPDATE processor_jobs SET
 preparation_failures=preparation_failures+1,
 state=CASE WHEN preparation_failures+1 >= $3 THEN 'failed' ELSE 'deferred' END,
 outcome='failed',reason='preparation_failed',lease_until=NULL,
 available_at=clock_timestamp()+interval '1 second'*(1 << LEAST(preparation_failures,4)),
 finished_at=CASE WHEN preparation_failures+1 >= $3 THEN clock_timestamp() END
 WHERE job_id=$1 AND state='leased' AND lease_generation=$2 AND lease_until>clock_timestamp()`, l.ID, l.Generation, s.cfg.MaxAttempts)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrStale
	}
	return tx.Commit(ctx)
}

func (s *Store) RunOne(ctx context.Context) (bool, error) {
	l, e := s.Claim(ctx)
	if e != nil || l == nil {
		return false, e
	}
	bounded, c := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer c()
	j, e := s.Prepare(bounded, *l)
	if e != nil {
		if errors.Is(e, ErrStale) || errors.Is(e, pgx.ErrNoRows) {
			s.abandon(ctx, *l, "source_changed")
		} else if recordErr := s.failPreparation(ctx, *l); recordErr != nil {
			e = errors.Join(e, recordErr)
		}
		return true, fmt.Errorf("prepare processor job: %w", e)
	}
	if matched, selErr := s.matchesStepSelector(*l); selErr == nil && !matched {
		skipRes := v1.Result{
			Binding:  j.Binding,
			Outcome:  v1.Abstained,
			Reasons:  []string{"selector_skipped"},
			Evidence: []v1.Evidence{},
		}
		return true, s.Finish(ctx, *l, j, skipRes, false)
	}
	r := v1.Result{Binding: j.Binding, Outcome: v1.Failed, Reasons: []string{"retry_exhausted"}, Evidence: []v1.Evidence{}}
	dispatched := l.StepAttempts < s.cfg.MaxAttempts
	if dispatched {
		if e = s.startAttempt(bounded, *l); e != nil {
			return true, e
		}
		st := l.Plan.Steps[l.Step]
		start := time.Now()
		var result v1.Result
		var err error
		if st.Entry.Driver != "" && st.Entry.Driver != v1.DriverHTTP {
			result, err = s.processBuiltin(bounded, st.Entry, j)
		} else {
			result, err = s.transport.Process(bounded, st.Entry.Endpoint, st.Entry.AuthEnv, j)
		}
		if err == nil {
			cumulative, _ := json.Marshal(append(l.Results, Receipt{Result: result, Candidates: j.Input.Candidates}))
			if len(cumulative) <= 100000 {
				r = result
			} else {
				r.Reasons = []string{"pipeline_result_limit"}
			}
		} else {
			r.Reasons = []string{failureReason(err)}
		}
		gen := strconv.FormatInt(st.Entry.ExecutionGeneration, 10)
		s.metrics.duration.WithLabelValues(st.Entry.ID, gen, string(st.Stage)).Observe(time.Since(start).Seconds())
		s.metrics.note(st.Entry.ID, gen)
		s.metrics.outcomes.WithLabelValues(st.Entry.ID, gen, string(st.Stage), string(r.Outcome)).Inc()
	}
	return true, s.Finish(ctx, *l, j, r, dispatched)
}

// Only bounded implementation-owned reason codes reach durable technical errors.
func failureReason(err error) string {
	switch err.Error() {
	case retiredLLMReason, retiredLearningReason, "binding_mismatch", "invalid_outcome", "result_bounds", "unproven_complete_miss", "unexpected_proposal", "missing_proposal", "one_proposal_per_stage", "invalid_content_type", "invalid_telemetry", "invalid_telemetry_time", "invalid_exclusion", "unexpected_exclusion", "invalid_language", "invalid_liveness", "invalid_tag", "invalid_weights", "unsupported_driver", "service_unavailable", "transport_failed", "transport_credential_unavailable", "invalid_uncertainty", "incompatible_contract":
		return err.Error()
	default:
		return "malformed_response"
	}
}
