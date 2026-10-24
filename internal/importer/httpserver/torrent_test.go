package httpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/gin-gonic/gin"
	"github.com/spencernorton/DH-Waypoint/internal/importer"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/metainfo"
	"github.com/stretchr/testify/require"
)

type torrentImportProbe struct {
	items                    []importer.Item
	started, closed, drained bool
	err                      error
}

func (p *torrentImportProbe) New(context.Context, importer.Info) importer.ActiveImport {
	p.started = true
	return p
}
func (p *torrentImportProbe) Import(items ...importer.Item) error {
	p.items = append(p.items, items...)
	return p.err
}
func (p *torrentImportProbe) Drain()       { p.drained = true }
func (p *torrentImportProbe) Close() error { p.closed = true; return p.err }
func (p *torrentImportProbe) Closed() bool { return p.closed }
func (p *torrentImportProbe) Err() error   { return p.err }

type unreadableTorrent struct{}

func (unreadableTorrent) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestTorrentImportValidatesBeforeDispatchAndCloses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	root := sha256.Sum256([]byte("synthetic"))
	info, err := bencode.Marshal(map[string]any{"name": "Synthetic.txt", "piece length": 16384, "meta version": 2, "file tree": map[string]any{"Synthetic.txt": map[string]any{"": map[string]any{"length": 9, "pieces root": root[:]}}}})
	require.NoError(t, err)
	raw, err := bencode.Marshal(map[string]any{"info": bencode.Bytes(info)})
	require.NoError(t, err)
	h := protocol.InfoHashV2(sha256.Sum256(info))
	for _, tc := range []struct {
		name, query string
		body        io.Reader
		importErr   error
		status      int
		started     bool
	}{
		{"valid", "?hash=" + h.String(), bytes.NewReader(raw), nil, 200, true},
		{"mismatched full hash", "?hash=" + strings.Repeat("ab", 32), bytes.NewReader(raw), nil, 400, false},
		{"invalid hash", "?hash=abcd", bytes.NewReader(raw), nil, 400, false},
		{"malformed", "", strings.NewReader("invalid"), nil, 400, false},
		{"oversized", "", strings.NewReader(strings.Repeat("x", 2*metainfo.MaxMetaInfoBytes+1)), nil, 413, false},
		{"read failure", "", unreadableTorrent{}, nil, 400, false},
		{"import failure", "", bytes.NewReader(raw), errors.New("synthetic failure"), 400, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &torrentImportProbe{err: tc.importErr}
			e := gin.New()
			e.POST("/import/torrent", func(c *gin.Context) { builder{}.handleTorrent(c, p) })
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, httptest.NewRequest("POST", "/import/torrent"+tc.query, tc.body))
			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			require.Equal(t, tc.started, p.started)
			require.Equal(t, tc.started, p.closed, "every started import must be closed")
			if tc.status == 200 {
				require.True(t, p.drained)
				require.Len(t, p.items, 1)
				require.Equal(t, h, p.items[0].InfoHash)
				require.Contains(t, rec.Body.String(), h.String())
			}
		})
	}
}
