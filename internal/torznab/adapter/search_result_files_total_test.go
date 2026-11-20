package adapter

import (
	"testing"

	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/torznab"
	"github.com/stretchr/testify/require"
)

func filesAttr(item torznab.SearchResultItem) (string, bool) {
	for _, attr := range item.TorznabAttrs {
		if attr.AttrName == torznab.AttrFiles {
			return attr.AttrValue, true
		}
	}
	return "", false
}

func TestFilesAttrEmitsFromHydratedCountsWithoutPreloadingFiles(t *testing.T) {
	itemFromTC := makeItem(10)
	itemFromTC.FilesCount = model.NewNullUint(14)
	val, ok := filesAttr(torrentContentResultItemToTorznabResultItem(itemFromTC, true))
	require.True(t, ok)
	require.Equal(t, "14", val)

	itemFromTorrent := makeItem(11)
	itemFromTorrent.Torrent.FilesCount = model.NewNullUint(7)
	val, ok = filesAttr(torrentContentResultItemToTorznabResultItem(itemFromTorrent, true))
	require.True(t, ok)
	require.Equal(t, "7", val)

	itemSingle := makeItem(12)
	itemSingle.Torrent.FilesStatus = model.FilesStatusSingle
	val, ok = filesAttr(torrentContentResultItemToTorznabResultItem(itemSingle, true))
	require.True(t, ok)
	require.Equal(t, "1", val)

	itemNoInfo := makeItem(13)
	itemNoInfo.Torrent.FilesStatus = model.FilesStatusNoInfo
	_, ok = filesAttr(torrentContentResultItemToTorznabResultItem(itemNoInfo, true))
	require.False(t, ok)
}

func TestTorznabResponseTotalUsesExactOrHasNextPageLowerBound(t *testing.T) {
	req := torznab.SearchRequest{
		Profile: torznab.ProfileDefault,
		Offset:  model.NewNullUint(20),
	}
	items := []search.TorrentContentResultItem{makeItem(1), makeItem(2), makeItem(3)}

	finalPage := torrentContentResultToTorznabResult(req, search.TorrentContentResult{
		Items:       items,
		HasNextPage: false,
	}, true)
	require.Equal(t, uint(20), finalPage.Channel.Response.Offset)
	require.Equal(t, uint(23), finalPage.Channel.Response.Total)

	morePages := torrentContentResultToTorznabResult(req, search.TorrentContentResult{
		Items:       items,
		HasNextPage: true,
	}, true)
	require.Equal(t, uint(24), morePages.Channel.Response.Total)

	explicitCount := torrentContentResultToTorznabResult(req, search.TorrentContentResult{
		Items:       items,
		TotalCount:  250,
		HasNextPage: true,
	}, true)
	require.Equal(t, uint(250), explicitCount.Channel.Response.Total)

	emptyPage := torrentContentResultToTorznabResult(req, search.TorrentContentResult{}, true)
	require.Equal(t, uint(0), emptyPage.Channel.Response.Total)
}
