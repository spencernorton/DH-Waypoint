package csamblocklist

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencernorton/DH-Waypoint/internal/catalogueguard"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/verdicts"
)

// Outcome says what RemoveStored did with a stored match.
type Outcome string

const (
	// OutcomeRemoved: the torrent and every row that can hold its name or a
	// file path are gone, and its hash is remembered.
	OutcomeRemoved Outcome = "removed"
	// OutcomeHiddenProtected: the torrent is tied to the operator's own
	// tools, so it is kept and hidden from every search.
	OutcomeHiddenProtected Outcome = "hidden_protected"
)

// ErrNotBannedKeyword refuses to remove a torrent the built-in list does not
// match. RemoveStored keeps no copy, so nothing else may reach it.
var ErrNotBannedKeyword = errors.New("csamblocklist: torrent does not match the built-in banned-keyword list")

// Every table with an info_hash column is in exactly one of four groups.
// TestRemovalClassifiesEveryHashTable fails when a migration adds a table
// that is in none of them.

// cascadingTables follow `delete from torrents` through a foreign key.
var cascadingTables = []string{
	"junkpurge_sync_claims",
	"torrent_contents",
	"torrent_files",
	"torrent_hints",
	"torrent_pieces",
	"torrent_tags",
	"torrent_tracker_seeds",
	"torrents_torrent_sources",
}

// protectingTables mark a torrent as tied to the operator's own tools. A row
// there means the torrent is hidden, not removed, so removal never meets one.
var protectingTables = []string{
	"label_evidence",
	"torrent_canonical_labels",
	"torrent_grab_attempts",
}

// namelessTables were read column by column and hold no name, path or text
// copied from them: ids, digests, counters, states and fixed reason codes.
var namelessTables = []string{
	"junkpurge_student_releases",
	"processor_core_receipts",
	"processor_payload_pending",
	"processor_payload_reviews",
	"processor_payload_work",
	"processor_publication_hold_events",
	"processor_publication_holds",
	"processor_publication_pending",
	"processor_publication_reviews",
	// The revision is a fence that has to outlive the torrent row.
	"processor_publication_sources",
	"torrent_liveness",
	// The verdict that records the removal lives here.
	"torrent_verdict_state",
	"tracker_peers",
	"tracker_swarms",
}

// removalSteps delete or blank everything else, in an order the foreign keys
// allow. $1 is the torrent's identities: its key and, for a hybrid, its v2
// hash, because side tables may be keyed by either.
var removalSteps = []struct{ table, sql string }{
	// First, so the publication triggers on the tables below find no torrent
	// to queue work for. The eight cascading tables go with it.
	{"torrents", `delete from torrents where info_hash=any($1)`},
	// Retired LLM queue: events and applications point at the task, and
	// migration 61 kept a copy of unfinished tasks.
	{"processor_legacy_freeze", `delete from processor_legacy_freeze f using unnest($1::bytea[]) h where f.snapshot->>'info_hash'='\x'||encode(h,'hex')`},
	{"llm_work_events", `delete from llm_work_events where task_key in (select task_key from llm_work_tasks where info_hash=any($1))`},
	{"llm_work_applications", `delete from llm_work_applications where info_hash=any($1) or task_key in (select task_key from llm_work_tasks where info_hash=any($1))`},
	{"llm_work_tasks", `delete from llm_work_tasks where info_hash=any($1)`},
	// A capture holds the model input. Its result row follows it.
	{"llm_evaluation_captures", `delete from llm_evaluation_captures where capture_key in (select capture_key from llm_evaluation_capture_admissions where info_hash=any($1))`},
	{"llm_evaluation_capture_admissions", `delete from llm_evaluation_capture_admissions where info_hash=any($1)`},
	// One batch input file holds the names of every item in its run and no
	// running code reads it back, so the whole file is blanked.
	{"junkpurge_batch_attempts", `update junkpurge_batch_attempts set input_payload='' where run_id in (select run_id from junkpurge_batch_items where info_hash=any($1)) and input_payload<>''`},
	{"junkpurge_batch_items", `delete from junkpurge_batch_items where info_hash=any($1)`},
	{"junkpurge_judgments", `delete from junkpurge_judgments where info_hash=any($1)`},
	{"junkpurge_quarantine", `delete from junkpurge_quarantine where info_hash=any($1)`},
	{"processor_feedback_events", `delete from processor_feedback_events where info_hash=any($1)`},
	{"processor_applications", `delete from processor_applications where info_hash=any($1)`},
	{"processor_exclusions", `delete from processor_exclusions where info_hash=any($1)`},
	{"processor_reprocess_requests", `delete from processor_reprocess_requests where info_hash=any($1)`},
	// A job row deletes its attempts. The history floor moves first, the way
	// the processors' own job pruning moves it.
	{"processor_jobs", `with doomed as (select job_id from processor_jobs where info_hash=any($1)),
boundary as (select max(coalesce(a.finished_at,a.started_at)) as value from processor_attempts a join doomed d using(job_id)),
advance as (update processor_control set attempt_history_floor=greatest(attempt_history_floor,(select value from boundary))
 where singleton and (select value from boundary) is not null returning singleton)
delete from processor_jobs where job_id in (select job_id from doomed)`},
	{"release_field_repair_journal", `delete from release_field_repair_journal where info_hash=any($1)`},
	{"catalogue_policy_shadow", `delete from catalogue_policy_shadow where info_hash=any($1)`},
	{"eval_frozen", `delete from eval_frozen where info_hash=any($1)`},
	{"private_torrent_files", `delete from private_torrent_files where info_hash=any($1)`},
	// A recovery snapshot is a full copy. Blanking it leaves the ledger and
	// its budget as they are, and a restore of it then fails as a conflict.
	{"catalogue_recovery_events", `update catalogue_recovery_events set reason='' where snapshot_id in (select id from catalogue_recovery_snapshots where info_hash=any($1))`},
	{"catalogue_recovery_snapshots", `update catalogue_recovery_snapshots set snapshot='{}',reason='' where info_hash=any($1)`},
	// A sampled classifier delete may have written the name as its reason.
	{"torrent_verdict_events", `update torrent_verdict_events set reason='',evidence=null where info_hash=any($1)`},
}

// protectedSQL is true for a torrent tied to the operator's own tools: the
// private flag, a canonical label, *arr or qBittorrent evidence, or a hint
// with a content id. Protected tags are tested in Go.
const protectedSQL = `select exists(select 1 from torrents where info_hash=any($1) and private)
 or exists(select 1 from torrent_canonical_labels where info_hash=any($1))
 or exists(select 1 from label_evidence where info_hash=any($1))
 or exists(select 1 from torrent_grab_attempts where info_hash=any($1))
 or exists(select 1 from torrent_hints where info_hash=any($1) and content_id is not null)`

// holdSQL hides a protected torrent the way an operator's publication hold
// does: every search and the optional processors skip a held torrent. A
// torrent that is already private is served nowhere and takes no hold, as in
// the hold API. $2 is the whitespace set of catalogueguard, $3 a request id.
var holdSQL = `insert into processor_publication_holds(info_hash,held,reason,request_id)
select t.info_hash,true,'banned_keyword',$3 from torrents t where t.info_hash=any($1) and ` + catalogueguard.PublicSQL("t", "$2") + `
on conflict(info_hash) do update set held=true,reason=excluded.reason,request_id=excluded.request_id,updated_at=clock_timestamp()
where not processor_publication_holds.held`

// lockSQL bounds the waits and takes the locks in the order the other writers
// use: the processors' control row before any torrent row, then the tables no
// foreign key ties to the torrent. No evidence, label, grab or verdict for it
// can then arrive between the protection check and the commit.
const lockSQL = `set local statement_timeout='15s'; set local lock_timeout='2s';
select 1 from processor_control where singleton for update;
lock table label_evidence,torrent_canonical_labels,torrent_grab_attempts in share mode;
lock table torrent_verdict_state,torrent_verdict_events in share row exclusive mode`

// RemoveStored removes a stored torrent whose name or file paths match the
// built-in banned-keyword list, keeping no copy: there is no undo. A torrent
// tied to the operator's own tools is most likely a false hit, so it is kept
// and hidden. Nothing here goes through cataloguerecovery.
func (s *service) RemoveStored(ctx context.Context, source string, t model.Torrent) (Outcome, error) {
	if !MatchesBannedKeyword(t) {
		return "", ErrNotBannedKeyword
	}
	hashes := [][]byte{t.InfoHash.Bytes()}
	if t.InfoHashV2 != nil {
		hashes = append(hashes, t.InfoHashV2.Bytes())
	}
	return s.removeMatched(ctx, source, hashes)
}

// removeMatched is RemoveStored for a caller that matched the name itself:
// the sweep, which reads names from tables that hold no model.Torrent.
func (s *service) removeMatched(ctx context.Context, source string, hashes [][]byte) (Outcome, error) {
	if s.pool == nil {
		return "", errors.New("csamblocklist: no database to remove a stored match from")
	}
	pool, err := s.pool.Get()
	if err != nil {
		return "", err
	}
	outcome, hashes, err := removeStored(ctx, pool, hashes)
	if err != nil {
		return "", fmt.Errorf("csamblocklist: remove stored match: %w", err)
	}
	if s.metrics != nil {
		s.metrics.StoredMatchesTotal.WithLabelValues(source, string(outcome)).Inc()
	}
	if outcome == OutcomeRemoved {
		// After the commit, so only what is really gone is remembered and
		// exported. A crash in between is caught by the crawler's own check.
		remembered := make([]protocol.InfoHash, 0, 2*len(hashes))
		for _, h := range hashes {
			if id, err := protocol.NewInfoHash(h); err == nil {
				remembered = append(remembered, id, protocol.InfoHashV1(id.WireID()))
			}
		}
		s.remember(ctx, source, remembered)
	}
	return outcome, nil
}

// removeStored returns the identities it worked on: the given ones and the
// v2 hash of the stored row, because side tables may be keyed by either.
func removeStored(ctx context.Context, pool *pgxpool.Pool, hashes [][]byte) (Outcome, [][]byte, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, lockSQL); err != nil {
		return "", nil, err
	}
	// The row lock keeps new child rows out until this ends. The row may be
	// gone already when only side tables still hold the name.
	rows, err := tx.Query(ctx, `select t.info_hash_v2,jsonb_build_object('name',t.name,'files_status',t.files_status),
 coalesce((select jsonb_agg(jsonb_build_object('path',f.path)) from torrent_files f where f.info_hash=t.info_hash),'[]')
 from torrents t where t.info_hash=any($1) for update of t`, hashes)
	if err != nil {
		return "", nil, err
	}
	type storedRow struct{ v2, torrent, files []byte }
	stored, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (r storedRow, err error) {
		return r, row.Scan(&r.v2, &r.torrent, &r.files)
	})
	if err != nil {
		return "", nil, err
	}
	for _, r := range stored {
		// The caller matched its own copy. The row may have been rewritten
		// since, so the locked row decides.
		if banned, err := SnapshotMatchesBannedKeyword(r.torrent, r.files); err != nil || !banned {
			return "", nil, errors.Join(ErrNotBannedKeyword, err)
		}
		if r.v2 != nil && !slices.ContainsFunc(hashes, func(h []byte) bool { return bytes.Equal(h, r.v2) }) {
			hashes = append(hashes, r.v2)
		}
	}
	protected, err := protectedTx(ctx, tx, hashes)
	if err != nil {
		return "", nil, err
	}
	if protected {
		request := sha256.Sum256(append([]byte("csam banned-keyword hold:"), hashes[0]...))
		if _, err = tx.Exec(ctx, holdSQL, hashes, catalogueguard.TagWhitespace, hex.EncodeToString(request[:])); err != nil {
			return "", nil, err
		}
		return OutcomeHiddenProtected, hashes, tx.Commit(ctx)
	}
	for _, step := range removalSteps {
		if _, err = tx.Exec(ctx, step.sql, hashes); err != nil {
			return "", nil, fmt.Errorf("%s: %w", step.table, err)
		}
	}
	// Hash and mechanism only: no reason, no evidence.
	if err = verdicts.RecordTx(ctx, tx, verdicts.Event{InfoHash: hashes[0], Verdict: verdicts.VerdictTombstoned, Mechanism: verdicts.MechanismCsam}); err != nil {
		return "", nil, err
	}
	return OutcomeRemoved, hashes, tx.Commit(ctx)
}

func protectedTx(ctx context.Context, tx pgx.Tx, hashes [][]byte) (bool, error) {
	// qBittorrent and the *arr tools know a v2-only torrent by the first 20
	// bytes of its hash, so their evidence may sit under that key. Looking
	// there too can only protect more.
	hashes = slices.Clone(hashes)
	for _, h := range hashes {
		if len(h) == 32 {
			hashes = append(hashes, h[:20])
		}
	}
	var protected bool
	if err := tx.QueryRow(ctx, protectedSQL, hashes).Scan(&protected); err != nil || protected {
		return protected, err
	}
	rows, err := tx.Query(ctx, `select name from torrent_tags where info_hash=any($1)`, hashes)
	if err != nil {
		return false, err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return false, err
	}
	for _, name := range names {
		if catalogueguard.ProtectedTag(name, true) {
			return true, nil
		}
	}
	return false, nil
}
