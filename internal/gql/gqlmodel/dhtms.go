package gqlmodel

import (
	"context"
	"errors"
	"strings"

	"github.com/99designs/gqlgen/graphql"
	"github.com/spencernorton/DH-Waypoint/internal/database/dhtms"
	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/gql/gqlmodel/gen"
)

func (t TorrentContentQuery) trySearchDHTMS(
	ctx context.Context,
	input TorrentContentSearchQueryInput,
) (TorrentContentSearchResult, bool) {
	if t.DHTMS == nil || !t.DHTMS.Config().ShouldServe() {
		return TorrentContentSearchResult{}, false
	}
	if t.LeanStore != nil {
		if _, _, _, active := t.LeanStore.ActiveServingProfile(); active {
			t.DHTMSMetrics.RecordRead("graphql", "bypass")
			return TorrentContentSearchResult{}, false
		}
	}
	if !gqlSelectionEligibleForDHTMS(ctx) {
		t.DHTMSMetrics.RecordRead("graphql", "bypass")
		return TorrentContentSearchResult{}, false
	}
	if input.ContentIdentity != nil ||
		input.ReleaseAttributes != nil ||
		input.InfoHashes.IsSet() ||
		input.TorrentCreatedAfter.IsSet() ||
		input.TorrentCreatedBefore.IsSet() ||
		(input.GroupByContent.Valid && input.GroupByContent.Bool) {
		t.DHTMSMetrics.RecordRead("graphql", "bypass")
		return TorrentContentSearchResult{}, false
	}

	clauses, singleCT, facetsOK := gqlFacetsToDHTMSClauses(input.Facets)
	if !facetsOK {
		t.DHTMSMetrics.RecordRead("graphql", "bypass")
		return TorrentContentSearchResult{}, false
	}

	qStr := ""
	if input.QueryString.Valid {
		qStr = strings.TrimSpace(input.QueryString.String)
	}
	if !gqlOrderByEligibleForDHTMS(qStr, input.OrderBy) {
		t.DHTMSMetrics.RecordRead("graphql", "bypass")
		return TorrentContentSearchResult{}, false
	}

	limit := uint(10) // matches query.DefaultOption()
	if input.Limit.Valid {
		limit = input.Limit.Uint
	}
	offset := uint(0)
	if input.Limit.Valid && input.Page.Valid && input.Page.Uint > 0 {
		offset += (input.Page.Uint - 1) * input.Limit.Uint
	}
	if input.Offset.Valid {
		offset += input.Offset.Uint
	}
	if limit > 499 || (qStr != "" && limit+offset+1 > 500) {
		t.DHTMSMetrics.RecordRead("graphql", "bypass")
		return TorrentContentSearchResult{}, false
	}

	wantTotal := input.TotalCount.Valid && input.TotalCount.Bool
	wantNext := input.HasNextPage.Valid && input.HasNextPage.Bool
	tier := dhtms.TierForPolicy(t.ServingPolicy)

	recordErr := func(err error) {
		if errors.Is(err, dhtms.ErrLeaseExpired) {
			t.DHTMSMetrics.RecordRead("graphql", "fallback_lease")
		} else {
			t.DHTMSMetrics.RecordRead("graphql", "fallback_error")
		}
	}

	var (
		hits            []dhtms.DocHit
		totalCount      uint
		delegatedOffset bool
	)

	if qStr != "" {
		fetchLimit := int(limit + offset)
		if wantNext {
			fetchLimit++
		}
		if fetchLimit < 1 {
			fetchLimit = 1
		}
		if fetchLimit > 500 {
			fetchLimit = 500
		}
		sr, err := t.DHTMS.Search(ctx, dhtms.SearchRequest{
			Query:         qStr,
			Mode:          dhtms.ModeAnd,
			Tier:          tier,
			Limit:         fetchLimit,
			IncludeFacets: wantTotal,
			AnyClauses:    clauses,
		})
		if err != nil {
			recordErr(err)
			return TorrentContentSearchResult{}, false
		}
		hits = sr.Docs
		if wantTotal && sr.TotalFacets != nil {
			totalCount = uint(*sr.TotalFacets)
		}
	} else {
		rssClauses := clauses
		if singleCT != nil {
			rssClauses = nil
		}
		fetchLimit := int(limit)
		if wantNext {
			fetchLimit++
		}
		if fetchLimit < 1 {
			fetchLimit = 1
		}
		if fetchLimit > 500 {
			fetchLimit = 500
		}
		delegatedOffset = true
		rss, err := t.DHTMS.RSS(ctx, dhtms.RSSRequest{
			ContentType:  singleCT,
			AnyClauses:   rssClauses,
			IncludeTotal: wantTotal,
			Tier:         tier,
			Limit:        fetchLimit,
			Offset:       int(offset),
		})
		if err != nil {
			recordErr(err)
			return TorrentContentSearchResult{}, false
		}
		hits = rss.Docs
		if wantTotal && rss.TotalCount != nil {
			totalCount = uint(*rss.TotalCount)
		}
	}

	converted := make([]TorrentContent, 0, len(hits))
	for _, h := range hits {
		tc, ok := dhtms.DocHitToTorrentContent(h, "", "")
		if !ok {
			continue
		}
		converted = append(converted, NewTorrentContentFromResultItem(search.TorrentContentResultItem{
			TorrentContent: tc,
		}))
	}

	start := int(offset)
	if delegatedOffset {
		start = 0
	}
	var page []TorrentContent
	hasNext := false
	if limit == 0 {
		page = []TorrentContent{}
	} else if start < len(converted) {
		rem := converted[start:]
		if len(rem) > int(limit) {
			page = rem[:limit]
			if wantNext {
				hasNext = true
			}
		} else {
			page = rem
		}
	}

	t.DHTMSMetrics.RecordRead("graphql", "hit")
	return TorrentContentSearchResult{
		TotalCount:           totalCount,
		TotalCountIsEstimate: false,
		HasNextPage:          hasNext,
		Items:                page,
	}, true
}

func gqlSelectionEligibleForDHTMS(ctx context.Context) bool {
	if !graphql.HasOperationContext(ctx) {
		return true
	}
	opCtx := graphql.GetOperationContext(ctx)
	fields := graphql.CollectFieldsCtx(ctx, nil)
	for _, f := range fields {
		switch f.Name {
		case "totalCount", "totalCountIsEstimate", "hasNextPage", "__typename":
		case "items":
			itemFields := graphql.CollectFields(opCtx, f.Selections, nil)
			for _, itf := range itemFields {
				switch itf.Name {
				case "infoHash", "contentType", "title", "videoResolution",
					"videoSource", "videoCodec", "video3d",
					"seeders", "leechers", "publishedAt", "createdAt", "updatedAt", "__typename":
				case "torrent":
					torFields := graphql.CollectFields(opCtx, itf.Selections, nil)
					for _, tf := range torFields {
						switch tf.Name {
						case "infoHash", "name", "size", "private", "filesCount",
							"magnetUri", "seeders", "leechers", "createdAt", "updatedAt", "__typename":
						default:
							return false
						}
					}
				default:
					return false
				}
			}
		default:
			return false
		}
	}
	return true
}

func gqlOrderByEligibleForDHTMS(qStr string, orderBy []gen.TorrentContentOrderByInput) bool {
	if len(orderBy) == 0 {
		return true
	}
	if len(orderBy) > 1 {
		return false
	}
	ob := orderBy[0]
	desc := false
	if d, ok := ob.Descending.ValueOK(); ok && d != nil {
		desc = *d
	}
	if qStr != "" {
		return ob.Field == gen.TorrentContentOrderByFieldRelevance && desc
	}
	return (ob.Field == gen.TorrentContentOrderByFieldPublishedAt && desc) ||
		ob.Field == gen.TorrentContentOrderByFieldRelevance
}

func gqlFacetsToDHTMSClauses(f *gen.TorrentContentFacetsInput) (clauses []dhtms.FilterClause, singleCT *uint8, ok bool) {
	if f == nil {
		return nil, nil, true
	}
	if f.TorrentSource.IsSet() ||
		f.TorrentTag.IsSet() ||
		f.TorrentFileType.IsSet() ||
		f.Language.IsSet() ||
		f.Genre.IsSet() ||
		f.ReleaseYear.IsSet() {
		return nil, nil, false
	}

	var cts []uint8
	if ctIn, has := f.ContentType.ValueOK(); has && ctIn != nil {
		if agg, hasAgg := ctIn.Aggregate.ValueOK(); hasAgg && agg != nil && *agg {
			return nil, nil, false
		}
		if filt, hasFilt := ctIn.Filter.ValueOK(); hasFilt && len(filt) > 0 {
			for _, v := range filt {
				if v == nil {
					return nil, nil, false
				}
				code := dhtms.ContentTypeToCode(string(*v))
				if code == 0 {
					return nil, nil, false
				}
				cts = append(cts, code)
			}
		}
	}

	var ress []uint8
	if resIn, has := f.VideoResolution.ValueOK(); has && resIn != nil {
		if agg, hasAgg := resIn.Aggregate.ValueOK(); hasAgg && agg != nil && *agg {
			return nil, nil, false
		}
		if filt, hasFilt := resIn.Filter.ValueOK(); hasFilt && len(filt) > 0 {
			for _, v := range filt {
				if v == nil {
					return nil, nil, false
				}
				code := dhtms.ResolutionToCode(v.String())
				if code == 0 {
					return nil, nil, false
				}
				ress = append(ress, code)
			}
		}
	}

	var srcs []uint8
	if srcIn, has := f.VideoSource.ValueOK(); has && srcIn != nil {
		if agg, hasAgg := srcIn.Aggregate.ValueOK(); hasAgg && agg != nil && *agg {
			return nil, nil, false
		}
		if filt, hasFilt := srcIn.Filter.ValueOK(); hasFilt && len(filt) > 0 {
			for _, v := range filt {
				if v == nil {
					return nil, nil, false
				}
				code := dhtms.VideoSourceToCode(v.String())
				if code == 0 {
					return nil, nil, false
				}
				srcs = append(srcs, code)
			}
		}
	}

	if len(cts) == 0 && len(ress) == 0 && len(srcs) == 0 {
		return nil, nil, true
	}
	if len(cts) == 1 && len(ress) == 0 && len(srcs) == 0 {
		c := cts[0]
		return []dhtms.FilterClause{{ContentType: &c}}, &c, true
	}

	ctOpts := make([]*uint8, 0, max(1, len(cts)))
	if len(cts) == 0 {
		ctOpts = append(ctOpts, nil)
	} else {
		for _, c := range cts {
			cc := c
			ctOpts = append(ctOpts, &cc)
		}
	}
	srcOpts := make([]*uint8, 0, max(1, len(srcs)))
	if len(srcs) == 0 {
		srcOpts = append(srcOpts, nil)
	} else {
		for _, s := range srcs {
			ss := s
			srcOpts = append(srcOpts, &ss)
		}
	}
	for _, ct := range ctOpts {
		for _, src := range srcOpts {
			clauses = append(clauses, dhtms.FilterClause{
				ContentType: ct,
				Resolutions: ress,
				VideoSource: src,
			})
		}
	}
	return clauses, nil, true
}
