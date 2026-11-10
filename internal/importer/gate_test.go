package importer

import (
	"context"
	"errors"
	"testing"

	mi "github.com/anacrolix/torrent/metainfo"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/spencernorton/DH-Waypoint/internal/csamblocklist"
	"github.com/spencernorton/DH-Waypoint/internal/csamblocklist/csamblocklisttest"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/metainfo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeCsam drops any hash whose first byte is in `block`.
type fakeCsam struct {
	block    map[byte]struct{}
	observed *[]protocol.InfoHash
}

func (f fakeCsam) IsBlocked(h protocol.InfoHash) bool { _, ok := f.block[h[0]]; return ok }
func (f fakeCsam) Filter(hs []protocol.InfoHash) []protocol.InfoHash {
	out := make([]protocol.InfoHash, 0, len(hs))
	for _, h := range hs {
		if _, ok := f.block[h[0]]; !ok {
			out = append(out, h)
		}
	}
	return out
}
func (f fakeCsam) Observe(_ context.Context, _ string, hs ...protocol.InfoHash) {
	if f.observed != nil {
		*f.observed = append(*f.observed, hs...)
	}
}
func (fakeCsam) RemoveStored(context.Context, string, model.Torrent) (csamblocklist.Outcome, error) {
	return "", nil
}
func (fakeCsam) Refresh(context.Context) error    { return nil }
func (fakeCsam) Snapshot() csamblocklist.Snapshot { return csamblocklist.Snapshot{} }
func (fakeCsam) Enabled() bool                    { return true }

// fakeBlocking drops any hash whose first byte is in `block`, or errors.
type fakeBlocking struct {
	block map[byte]struct{}
	err   error
}

func (f fakeBlocking) Filter(_ context.Context, hs []protocol.InfoHash) ([]protocol.InfoHash, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]protocol.InfoHash, 0, len(hs))
	for _, h := range hs {
		if _, ok := f.block[h[0]]; !ok {
			out = append(out, h)
		}
	}
	return out, nil
}
func (fakeBlocking) Block(context.Context, []protocol.InfoHash, string, string, bool) error {
	return nil
}
func (fakeBlocking) Flush(context.Context) error { return nil }
func (fakeBlocking) Ignore([]protocol.InfoHash)  {}

func mkItems(firstBytes ...byte) []Item {
	items := make([]Item, len(firstBytes))
	for i, b := range firstBytes {
		var h protocol.InfoHash
		h[0] = b
		items[i] = Item{InfoHash: h, Name: "item"}
	}
	return items
}

func firstBytes(items []Item) []byte {
	out := make([]byte, len(items))
	for i, it := range items {
		out[i] = it.InfoHash[0]
	}
	return out
}

func newGate(csam csamblocklist.Manager, block *fakeBlocking) *activeImport {
	imp := importer{csamBlocklist: csam, metrics: NewMetrics()}
	if block != nil {
		imp.blockingManager = block
	}
	return &activeImport{importer: imp, ctx: context.Background()}
}

// The /import gate must apply CSAM (first, unconditional) then blocking, and
// FAIL CLOSED when the blocking lookup errors — never importing unfiltered.
func TestGateItems(t *testing.T) {
	// CSAM(2) + blocking(3) each drop one; 1 and 4 survive, order preserved.
	out, err := newGate(fakeCsam{block: map[byte]struct{}{2: {}}},
		&fakeBlocking{block: map[byte]struct{}{3: {}}}).gateItems(mkItems(1, 2, 3, 4))
	require.NoError(t, err)
	assert.Equal(t, []byte{1, 4}, firstBytes(out))

	// Blocking error → whole batch refused (fail closed), no items returned.
	out, err = newGate(fakeCsam{block: map[byte]struct{}{2: {}}},
		&fakeBlocking{err: errors.New("bloom flush failed")}).gateItems(mkItems(1, 2, 3))
	require.Error(t, err)
	assert.Nil(t, out)

	// Nothing dropped → original slice returned.
	in := mkItems(5, 6)
	out, err = newGate(fakeCsam{block: map[byte]struct{}{}},
		&fakeBlocking{block: map[byte]struct{}{}}).gateItems(in)
	require.NoError(t, err)
	assert.Equal(t, []byte{5, 6}, firstBytes(out))

	// Everything dropped → empty, no error (persistItems then no-ops).
	out, err = newGate(fakeCsam{block: map[byte]struct{}{7: {}}},
		&fakeBlocking{block: map[byte]struct{}{8: {}}}).gateItems(mkItems(7, 8))
	require.NoError(t, err)
	assert.Empty(t, out)

	// CSAM is applied even when blocking is nil (degraded wiring).
	out, err = newGate(fakeCsam{block: map[byte]struct{}{9: {}}}, nil).gateItems(mkItems(9, 10))
	require.NoError(t, err)
	assert.Equal(t, []byte{10}, firstBytes(out))

	// Both nil → pass-through, no panic (narrow test construction).
	g := &activeImport{importer: importer{metrics: NewMetrics()}, ctx: context.Background()}
	out, err = g.gateItems(mkItems(11, 12))
	require.NoError(t, err)
	assert.Len(t, out, 2)

	// Empty input short-circuits.
	out, err = newGate(fakeCsam{}, &fakeBlocking{}).gateItems(nil)
	require.NoError(t, err)
	assert.Empty(t, out)
}

// The built-in banned-keyword list applies to every import: by name, by a
// file path in verified metadata, with no feed entry for the hash, and with
// no blocklist wired at all. A match is dropped and its identities remembered.
func TestGateItemsDropsBannedKeywordMatches(t *testing.T) {
	banned := csamblocklisttest.BannedName()
	items := mkItems(1, 2, 3, 4)
	items[1].Name = banned + ".mkv"
	v2 := protocol.InfoHash{42, 31: 7} // differs from its 160-bit wire key
	items[2].metadata = &metainfo.ValidatedInfo{
		Info:       metainfo.Info{Name: "Sample Collection"},
		Identities: protocol.TorrentIdentities{V1: &items[2].InfoHash, V2: &v2},
		Files:      []mi.FileInfo{{Path: []string{"Sample Folder", "readme.txt"}}, {Path: []string{banned, "clip 01.mp4"}}},
	}

	var observed []protocol.InfoHash
	gate := newGate(fakeCsam{observed: &observed}, nil)
	out, err := gate.gateItems(items)
	require.NoError(t, err)
	assert.Equal(t, []byte{1, 4}, firstBytes(out))
	// Each identity is remembered with the wire key triage knows it by.
	assert.Equal(t, []protocol.InfoHash{items[1].InfoHash, items[1].InfoHash,
		items[2].InfoHash, items[2].InfoHash, v2, protocol.InfoHashV1(v2.WireID())}, observed)
	assert.Equal(t, 2.0, testutil.ToFloat64(gate.metrics.gated.WithLabelValues("csam_keyword")))

	out, err = newGate(nil, nil).gateItems(items)
	require.NoError(t, err)
	assert.Equal(t, []byte{1, 4}, firstBytes(out))
}
