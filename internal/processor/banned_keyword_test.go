package processor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/spencernorton/DH-Waypoint/internal/classifier/classification"
	"github.com/spencernorton/DH-Waypoint/internal/classifier/contentfilter"
	"github.com/spencernorton/DH-Waypoint/internal/csamblocklist"
	"github.com/spencernorton/DH-Waypoint/internal/csamblocklist/csamblocklisttest"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/stretchr/testify/require"
)

type removalRecorder struct {
	csamblocklist.Manager
	mu      sync.Mutex
	sources []string
	hashes  []protocol.InfoHash
	err     error
}

func (r *removalRecorder) RemoveStored(
	_ context.Context,
	source string,
	t model.Torrent,
) (csamblocklist.Outcome, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sources = append(r.sources, source)
	r.hashes = append(r.hashes, t.InfoHash)
	return csamblocklist.OutcomeRemoved, r.err
}

// A stored torrent that matches the built-in list is handed to the removal in
// every mode and for a reprocess request. Nothing else runs on it: sqlmock
// has no expectation, so any query would fail the call.
func TestProcessRemovesBannedKeywordMatchInEveryMode(t *testing.T) {
	byName := processTestTorrent(processTestHash(5), csamblocklisttest.BannedName(), "mkv")
	byPath := processTestTorrent(processTestHash(6), "Sample Folder 4b1d", "")
	byPath.FilesStatus = model.FilesStatusMulti
	byPath.Files = []model.TorrentFile{
		{InfoHash: byPath.InfoHash, Index: 0, Path: "notes.txt", Size: 1},
		{InfoHash: byPath.InfoHash, Index: 1, Path: csamblocklisttest.BannedName() + ".mkv", Size: 1},
	}
	for _, mode := range []Mode{ModeRaw, ModeBasic, ModeFull} {
		for kind, reprocess := range map[string]string{"job": "", "reprocess": strings.Repeat("a", 64)} {
			for name, torrent := range map[string]model.Torrent{"name": byName, "file path": byPath} {
				t.Run(string(mode)+"/"+kind+"/"+name, func(t *testing.T) {
					torrent.Contents = []model.TorrentContent{{ID: "synthetic-existing", InfoHash: torrent.InfoHash}}
					var classified atomic.Bool
					p, mock, blocker := newProcessTestProcessor(t, []model.Torrent{torrent},
						processRunnerStub{run: func(model.Torrent) (classification.Result, error) {
							classified.Store(true)
							return classification.Result{}, classification.ErrDeleteTorrent
						}})
					p.mode = mode
					cfg := contentfilter.NewDefaultConfig()
					cfg.Enabled = true
					cfg.Enforce = true
					cfg.BlockedExtensions = []string{"mkv"}
					p.contentFilter = contentfilter.New(cfg)
					removal := &removalRecorder{}
					p.csamBlocklist = removal
					receipts := &receiptCapture{}
					p.coreRecorder = receipts

					require.NoError(t, p.Process(context.Background(), MessageParams{
						ReprocessRequestID: reprocess,
						InfoHashes:         []protocol.InfoHash{torrent.InfoHash},
					}))
					require.Equal(t, []string{"processor"}, removal.sources)
					require.Equal(t, []protocol.InfoHash{torrent.InfoHash}, removal.hashes)
					require.False(t, classified.Load(), "classifier ran on a banned-keyword match")
					// Lengths only: a failure must not print the torrent.
					require.Zero(t, len(blocker.blockRecords()))
					require.Zero(t, len(receipts.receipts))
					require.NoError(t, mock.ExpectationsWereMet())
				})
			}
		}
	}
}

// The rest of a batch is processed as usual, and a clean torrent never
// reaches the removal.
func TestProcessBannedKeywordMatchLeavesTheBatchAlone(t *testing.T) {
	keep := processTestHash(3)
	banned := processTestHash(5)
	p, mock, blocker := newProcessTestProcessor(t,
		[]model.Torrent{
			processTestTorrent(keep, "kept movie 2026", "mkv"),
			processTestTorrent(banned, csamblocklisttest.BannedName(), "mkv"),
		},
		processRunnerStub{run: func(torrent model.Torrent) (classification.Result, error) {
			if torrent.InfoHash == banned {
				return classification.Result{}, errors.New("classifier ran on a banned-keyword match")
			}
			return classification.Result{ContentAttributes: classification.ContentAttributes{
				ContentType: model.NewNullContentType(model.ContentTypeMovie),
			}}, nil
		}})
	removal := &removalRecorder{}
	p.csamBlocklist = removal
	expectTorrentContentInsert(mock)

	require.NoError(t, p.Process(context.Background(), MessageParams{InfoHashes: []protocol.InfoHash{keep, banned}}))
	require.Equal(t, []protocol.InfoHash{banned}, removal.hashes)
	require.Zero(t, len(blocker.blockRecords()))
	require.NoError(t, mock.ExpectationsWereMet())
}

// A failed removal fails the job, so the queue retries it.
func TestProcessBannedKeywordRemovalFailureFailsTheJob(t *testing.T) {
	hash := processTestHash(5)
	p, mock, _ := newProcessTestProcessor(t,
		[]model.Torrent{processTestTorrent(hash, csamblocklisttest.BannedName(), "mkv")},
		processRunnerStub{run: func(model.Torrent) (classification.Result, error) {
			return classification.Result{}, nil
		}})
	failure := errors.New("database unavailable")
	p.csamBlocklist = &removalRecorder{err: failure}

	require.ErrorIs(t, p.Process(context.Background(), MessageParams{InfoHashes: []protocol.InfoHash{hash}}), failure)
	require.NoError(t, mock.ExpectationsWereMet())
}
