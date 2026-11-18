package adapter

import (
	"context"
	"errors"
	"testing"

	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/torznab"
	"github.com/stretchr/testify/require"
)

type stubTopUpSearch struct {
	search.Search
	pages [][]search.TorrentContentResultItem
	calls int
}

func (s *stubTopUpSearch) TorrentContent(_ context.Context, _ ...query.Option) (search.TorrentContentResult, error) {
	idx := s.calls
	s.calls++
	if idx >= len(s.pages) {
		return search.TorrentContentResult{}, nil
	}
	return search.TorrentContentResult{Items: append([]search.TorrentContentResultItem(nil), s.pages[idx]...)}, nil
}

type stubLiveness struct {
	dead map[string]struct{}
	err  error
}

func (l stubLiveness) DeadSet(_ context.Context, _ [][]byte) (map[string]struct{}, error) {
	if l.err != nil {
		return nil, l.err
	}
	return l.dead, nil
}

type stubLivenessMetrics struct {
	excluded int
	errors   int
}

func (m *stubLivenessMetrics) TorznabExcluded()    { m.excluded++ }
func (m *stubLivenessMetrics) TorznabLookupError() { m.errors++ }

func makeTopUpItem(b byte, name string) search.TorrentContentResultItem {
	var id protocol.ID
	id[0] = b
	ih := protocol.InfoHashV1(id)
	return search.TorrentContentResultItem{
		TorrentContent: model.TorrentContent{
			InfoHash:    ih,
			ContentType: model.NewNullContentType(model.ContentTypeMovie),
			Torrent: model.Torrent{
				InfoHash: ih,
				Name:     name,
				Size:     1024,
			},
		},
	}
}

func TestAdapterTopsUpUnderfilledFilteredPagesAndCountsLivenessErrors(t *testing.T) {
	item1 := makeTopUpItem(0x01, "Dead.Release.1")
	item2 := makeTopUpItem(0x02, "Alive.Release.2")
	item3 := makeTopUpItem(0x03, "Alive.Release.3")

	st := &stubTopUpSearch{
		pages: [][]search.TorrentContentResultItem{
			{item1, item2},
			{item3},
		},
	}
	metrics := &stubLivenessMetrics{}
	deadKey := hashKey(item1.InfoHash.Bytes())
	ad := NewWithLiveness(st, stubLiveness{dead: map[string]struct{}{deadKey: {}}}, true, metrics)

	res, err := ad.Search(context.Background(), torznab.SearchRequest{
		Type:    torznab.FunctionMovie,
		Profile: torznab.Profile{ID: "default", DefaultLimit: 2, MaxLimit: 10},
		Limit:   model.NewNullUint(2),
	})
	require.NoError(t, err)
	require.Equal(t, 2, st.calls, "fetches bounded follow-up page to replace filtered dead item")
	require.Len(t, res.Channel.Items, 2)
	require.Equal(t, 1, metrics.excluded)

	// Verify DeadSet error increments TorznabLookupError while preserving search availability.
	stErr := &stubTopUpSearch{pages: [][]search.TorrentContentResultItem{{item2}}}
	adErr := NewWithLiveness(stErr, stubLiveness{err: errors.New("db_timeout")}, true, metrics)
	resErr, err := adErr.Search(context.Background(), torznab.SearchRequest{
		Type:    torznab.FunctionMovie,
		Profile: torznab.Profile{ID: "default", DefaultLimit: 2, MaxLimit: 10},
	})
	require.NoError(t, err)
	require.Len(t, resErr.Channel.Items, 1)
	require.Equal(t, 1, metrics.errors)
}
