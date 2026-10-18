package restapi

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/stretchr/testify/require"
)

// fakeSearch implements ServingSearch by embedding the interface and
// overriding only the two methods the REST layer calls.
type fakeSearch struct {
	search.ServingSearch
	content search.TorrentContentResult
	files   search.TorrentFilesResult
}

func (f fakeSearch) TorrentContent(context.Context, ...query.Option) (search.TorrentContentResult, error) {
	return f.content, nil
}
func (f fakeSearch) TorrentFiles(context.Context, ...query.Option) (search.TorrentFilesResult, error) {
	return f.files, nil
}

func TestToItemMapping(t *testing.T) {
	hash := protocol.ID{0xab}
	seeders := uint(12)
	tc := model.TorrentContent{
		InfoHash:    hash,
		ContentType: model.NewNullContentType(model.ContentTypeMovie),
		Size:        1234,
		Seeders:     model.NewNullUint(seeders),
		PublishedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	tc.Torrent.Name = "Some Film 2026"
	tc.Content.Title = "Some Film"
	it := toItem(tc)
	require.Equal(t, hash.String(), it.InfoHash)
	require.Equal(t, "movie", it.ContentType)
	require.Equal(t, "Some Film", it.Title)
	require.NotNil(t, it.Seeders)
	require.Equal(t, seeders, *it.Seeders)
	require.Contains(t, it.Magnet, "magnet:?xt=urn:btih:"+hash.String())
}

// The checked-in documentation copy must never drift from the served
// embedded specification.
func TestOpenAPIDocsCopyMatchesEmbedded(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "reference", "openapi.yaml"))
	require.NoError(t, err)
	require.Equal(t, openAPISpec, doc)
}
