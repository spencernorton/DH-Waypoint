package processors

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/spencernorton/DH-Waypoint/internal/classifier/contentfilter"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShouldRejectApplyContentTypeNonVideo(t *testing.T) {
	t.Parallel()

	// Enforce=false: DecideDeterministic.Allow is true even for a blocked
	// extension, but Reason is blocked — rejection must follow Reason.
	cfg := contentfilter.Config{
		Enabled:           true,
		Enforce:           false,
		BlockedExtensions: []string{"exe", "7z", "zip", "rar"},
	}
	filter := contentfilter.New(cfg)

	exeTorrent := model.Torrent{
		Name:        "South Park S29E04 1080p WEB H264-FLUX.exe",
		FilesStatus: model.FilesStatusSingle,
		Extension:   model.NewNullString("exe"),
		Size:        1000,
	}
	// Sanity: shadow mode allows despite a blocked reason.
	dec := filter.DecideDeterministic(contentfilter.Input{Title: exeTorrent.Name, ContentType: "tv_show", PrimaryExtension: "exe", AllExtensions: []string{"exe"}})
	require.Equal(t, contentfilter.ReasonBlockedExtension, dec.Reason)
	assert.True(t, dec.Allow, "shadow mode sets Allow despite blocked reason")

	assert.True(t, shouldRejectApplyContentType(filter,
		contentfilter.Input{Title: exeTorrent.Name, ContentType: "tv_show", PrimaryExtension: "exe", AllExtensions: []string{"exe"}},
		"tv_show", exeTorrent), "exe tv_show proposal must be rejected_policy even when Enforce=false")

	sevenZipTorrent := model.Torrent{
		Name:        "MobLand S02E07 1080p WEB H264.7z",
		FilesStatus: model.FilesStatusSingle,
		Extension:   model.NewNullString("7z"),
		Size:        1000,
	}
	assert.True(t, shouldRejectApplyContentType(filter,
		contentfilter.Input{Title: sevenZipTorrent.Name, ContentType: "tv_show", PrimaryExtension: "7z", AllExtensions: []string{"7z"}},
		"tv_show", sevenZipTorrent))

	// File-evidence rejection applies even with no filter at all.
	assert.True(t, shouldRejectApplyContentType(nil, contentfilter.Input{}, "tv_show", exeTorrent))
	assert.True(t, shouldRejectApplyContentType(nil, contentfilter.Input{}, "movie", sevenZipTorrent))
	assert.True(t, shouldRejectApplyContentType(nil, contentfilter.Input{}, "xxx", exeTorrent))

	// Name-only fallback: no Files, no Extension, name ends in .exe.
	assert.True(t, shouldRejectApplyContentType(nil, contentfilter.Input{}, "tv_show",
		model.Torrent{Name: "Some Show S01E01.exe"}))

	// Multi-file with only non-video files.
	assert.True(t, shouldRejectApplyContentType(nil, contentfilter.Input{}, "tv_show", model.Torrent{
		Name: "Show S01E01", FilesStatus: model.FilesStatusMulti,
		Files: []model.TorrentFile{
			{Path: "show.exe", Size: 100, Extension: model.NewNullString("exe")},
			{Path: "show.rar", Size: 100, Extension: model.NewNullString("rar")},
		},
	}))

	// A genuine video torrent is not rejected.
	videoTorrent := model.Torrent{
		Name:        "South Park S29E04 1080p WEB.mkv",
		FilesStatus: model.FilesStatusSingle,
		Extension:   model.NewNullString("mkv"),
		Size:        1000,
	}
	assert.False(t, shouldRejectApplyContentType(filter,
		contentfilter.Input{Title: videoTorrent.Name, ContentType: "tv_show", PrimaryExtension: "mkv", AllExtensions: []string{"mkv"}},
		"tv_show", videoTorrent))
	assert.False(t, shouldRejectApplyContentType(nil, contentfilter.Input{}, "tv_show", videoTorrent))

	// Non-video proposals are not affected by the video guard.
	assert.False(t, shouldRejectApplyContentType(nil, contentfilter.Input{}, "software", exeTorrent))
}

// fakeApplyTx is a minimal pgx.Tx that serves the two reads apply needs on
// the rejection path (the post-apply source snapshot and the feedback-event
// insert) and records every Exec so the test can prove no content update ran.
type fakeApplyTx struct {
	sourceRaw []byte
	outcomes  []string
	execs     []string
}

type fakeApplyRow struct{ scan func(dest ...any) error }

func (r fakeApplyRow) Scan(dest ...any) error { return r.scan(dest...) }

func (tx *fakeApplyTx) Begin(context.Context) (pgx.Tx, error) { return tx, nil }
func (tx *fakeApplyTx) Commit(context.Context) error          { return nil }
func (tx *fakeApplyTx) Rollback(context.Context) error        { return nil }
func (tx *fakeApplyTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 0, nil
}
func (tx *fakeApplyTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults { return nil }
func (tx *fakeApplyTx) LargeObjects() pgx.LargeObjects                         { return pgx.LargeObjects{} }
func (tx *fakeApplyTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	return nil, nil
}
func (tx *fakeApplyTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	tx.execs = append(tx.execs, sql)
	return pgconn.CommandTag{}, nil
}
func (tx *fakeApplyTx) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, fmt.Errorf("unexpected Query")
}
func (tx *fakeApplyTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "processor_feedback_events") {
		if len(args) > 9 {
			if gt, ok := args[9].(string); ok {
				var parsed struct {
					Outcome string `json:"outcome"`
				}
				if err := json.Unmarshal([]byte(gt), &parsed); err == nil {
					tx.outcomes = append(tx.outcomes, parsed.Outcome)
				}
			}
		}
		return fakeApplyRow{scan: func(dest ...any) error {
			if len(dest) != 2 {
				return fmt.Errorf("feedback scan: %d dest", len(dest))
			}
			*(dest[0].(*int64)) = 1
			*(dest[1].(*time.Time)) = time.Unix(1_700_000_000, 0)
			return nil
		}}
	}
	// readSource snapshot.
	return fakeApplyRow{scan: func(dest ...any) error {
		if len(dest) != 1 {
			return fmt.Errorf("source scan: %d dest", len(dest))
		}
		*(dest[0].(*[]byte)) = append([]byte(nil), tx.sourceRaw...)
		return nil
	}}
}
func (tx *fakeApplyTx) Conn() *pgx.Conn { return nil }

// TestApplyRejectsTVShowProposalForNonVideoTorrents exercises the real
// Store.apply path (not just shouldRejectApplyContentType): an optional
// processor in apply mode proposing tv_show on a single-file .exe / .7z
// torrent is rejected with "rejected_policy" — observed through the
// operational feedback ground truth apply records — even when the content
// filter runs with Enforce=false, where DecideDeterministic.Allow is true.
func TestApplyRejectsTVShowProposalForNonVideoTorrents(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		torrentName string
		ext         string
		wantOutcome string
		wantUpdate  bool
	}{
		{"exe", "South Park S29E04 1080p WEB H264-FLUX.exe", "exe", "rejected_policy", false},
		{"7z", "MobLand S02E07 1080p WEB H264.7z", "7z", "rejected_policy", false},
		{"mkv video control", "South Park S29E04 1080p WEB H264-FLUX.mkv", "mkv", "applied", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			filter := contentfilter.New(contentfilter.Config{
				Enabled:           true,
				Enforce:           false,
				BlockedExtensions: []string{"exe", "7z", "zip", "rar"},
			})
			// Shadow mode sanity: blocked yet allowed.
			dec := filter.DecideDeterministic(contentfilter.Input{
				Title: tc.torrentName, ContentType: "tv_show",
				PrimaryExtension: tc.ext, AllExtensions: []string{tc.ext},
			})
			if tc.ext != "mkv" {
				require.Equal(t, contentfilter.ReasonBlockedExtension, dec.Reason)
				require.True(t, dec.Allow)
			}

			store := &Store{filter: filter}
			hash := bytes.Repeat([]byte{0x2a}, 20)
			ext := tc.ext
			src := source{
				Name:        tc.torrentName,
				Size:        50_000_000,
				FilesStatus: "single",
				Extension:   &ext,
				Files:       []sourceFile{{Path: tc.torrentName, Size: 50_000_000, Index: 0, Extension: &ext}},
				Contents:    []json.RawMessage{json.RawMessage(`{"id":"tc-1"}`)},
			}
			sourceRaw, err := json.Marshal(src)
			require.NoError(t, err)
			tx := &fakeApplyTx{sourceRaw: sourceRaw}
			lease := Lease{
				ID:   "job-1",
				Hash: hash,
				Plan: Plan{Steps: []Step{{Entry: Entry{ID: "test-processor", Mode: "apply", ExecutionGeneration: 1}}}},
				Results: []Receipt{{Result: v1.Result{
					Outcome:         v1.Completed,
					Proposal:        &v1.Proposal{ContentType: "tv_show"},
					RequestFeedback: true,
				}}},
			}

			require.NoError(t, store.apply(context.Background(), tx, lease, src))

			require.Equal(t, []string{tc.wantOutcome}, tx.outcomes,
				"apply must record the step outcome via feedback ground truth")
			updated := false
			for _, sql := range tx.execs {
				if strings.Contains(sql, "UPDATE torrent_contents") {
					updated = true
				}
			}
			assert.Equal(t, tc.wantUpdate, updated, "execs: %v", tx.execs)
		})
	}
}
