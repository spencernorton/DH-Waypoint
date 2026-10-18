package gqlmodel

import (
	"context"

	"github.com/spencernorton/DH-Waypoint/internal/database/dao"
	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/gql/gqlmodel/gen"
	"github.com/spencernorton/DH-Waypoint/internal/metrics/torrentmetrics"
	"github.com/spencernorton/DH-Waypoint/internal/model"
)

type TorrentQuery struct {
	Dao                  *dao.Query
	Search               search.Search
	TorrentMetricsClient torrentmetrics.Client
}

func (t TorrentQuery) SuggestTags(
	ctx context.Context,
	input *gen.SuggestTagsQueryInput,
) (search.TorrentSuggestTagsResult, error) {
	suggestTagsQuery := search.SuggestTagsQuery{}

	if input != nil {
		if prefix, ok := input.Prefix.ValueOK(); ok && prefix != nil {
			suggestTagsQuery.Prefix = *prefix
		}

		if exclusions, ok := input.Exclusions.ValueOK(); ok {
			suggestTagsQuery.Exclusions = exclusions
		}
	}

	return t.Search.TorrentSuggestTags(ctx, suggestTagsQuery)
}

func (t TorrentQuery) ListSources(ctx context.Context) (gen.TorrentListSourcesResult, error) {
	result, err := t.Dao.TorrentSource.WithContext(ctx).Order(t.Dao.TorrentSource.Key.Asc()).Find()
	if err != nil {
		return gen.TorrentListSourcesResult{}, err
	}

	sources := make([]model.TorrentSource, len(result))
	for i := range result {
		sources[i] = *result[i]
	}

	return gen.TorrentListSourcesResult{
		Sources: sources,
	}, nil
}

type TorrentMutation struct{}
