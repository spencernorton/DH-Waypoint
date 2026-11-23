package adapter

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/spencernorton/DH-Waypoint/internal/database/dhtms"
	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/leanmode"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/serving"
	"github.com/spencernorton/DH-Waypoint/internal/torznab"
)

// WithDHTMS wires an optional DH-TMS sidecar client for direct Torznab serving
// with automatic fail-closed fallback to PostgreSQL whenever the sidecar's
// governance lease expires, is unbootstrapped, or is unreachable.
func (a Adapter) WithDHTMS(
	client *dhtms.Client,
	policy *serving.Policy,
	metrics *dhtms.Metrics,
	lean *leanmode.Store,
) Adapter {
	a.dhtms = client
	a.servingPolicy = policy
	a.dhtmsMetrics = metrics
	a.leanStore = lean
	return a
}

func (a Adapter) canServeFromDHTMS(req torznab.SearchRequest) bool {
	if a.dhtms == nil || !a.dhtms.Config().ShouldServe() {
		return false
	}
	if strings.TrimSpace(req.Profile.LeanProfile) != "" {
		a.dhtmsMetrics.RecordRead("torznab", "bypass")
		return false
	}
	if a.leanStore != nil {
		if _, _, _, active := a.leanStore.ActiveServingProfile(); active {
			a.dhtmsMetrics.RecordRead("torznab", "bypass")
			return false
		}
	}
	if len(req.Profile.Tags) > 0 || req.AirDate.IsValid() {
		a.dhtmsMetrics.RecordRead("torznab", "bypass")
		return false
	}
	// Delegate specialized categories not tracked in DH-TMS hot rows to PostgreSQL.
	for _, cat := range req.Cats {
		if cat == torznab.CategoryMovies3D.ID || cat == torznab.CategoryTVAnime.ID {
			a.dhtmsMetrics.RecordRead("torznab", "bypass")
			return false
		}
	}
	return true
}

func (a Adapter) trySearchDHTMS(
	ctx context.Context,
	req torznab.SearchRequest,
	identifiers []model.ContentRef,
	limit, offset uint,
) (search.TorrentContentResult, bool) {
	tier := dhtms.TierForPolicy(a.servingPolicy)
	fetchLimit := int(limit + offset + 1)
	if fetchLimit < 25 {
		fetchLimit = 25
	}
	if fetchLimit > 500 {
		fetchLimit = 500
	}
	// Over-fetch modestly when local post-filters or category filters are active.
	if len(req.Cats) > 0 || a.liveness != nil || a.verdicts != nil || !a.freshness.IsZero() {
		if fetchLimit < 200 {
			fetchLimit = 200
		}
	}

	type taggedHit struct {
		hit           dhtms.DocHit
		contentSource string
		contentID     string
		linked        bool
	}

	var (
		collected []taggedHit
		seen      = make(map[string]struct{})
		isIDQuery = len(identifiers) > 0
	)
	addHit := func(h dhtms.DocHit, src, cid string, linked bool) {
		if len(h.InfoHashV1) != 40 {
			return
		}
		if _, dup := seen[h.InfoHashV1]; dup {
			return
		}
		seen[h.InfoHashV1] = struct{}{}
		collected = append(collected, taggedHit{
			hit:           h,
			contentSource: src,
			contentID:     cid,
			linked:        linked,
		})
	}

	recordErr := func(err error) {
		if errors.Is(err, dhtms.ErrLeaseExpired) {
			a.dhtmsMetrics.RecordRead("torznab", "fallback_lease")
		} else {
			a.dhtmsMetrics.RecordRead("torznab", "fallback_error")
		}
	}

	var seasonPtr, episodePtr *uint16
	if req.Type == torznab.FunctionTV && req.Season.Valid && req.Season.Int >= 0 && req.Season.Int <= 65535 {
		s := uint16(req.Season.Int)
		seasonPtr = &s
		if req.Episode.Valid && req.Episode.Int >= 0 && req.Episode.Int <= 65535 {
			e := uint16(req.Episode.Int)
			episodePtr = &e
		}
	}

	switch {
	case isIDQuery:
		var linkedHits, unlinkedHits []taggedHit
		seenLinked := make(map[string]struct{})
		seenUnlinked := make(map[string]struct{})
		for _, ref := range identifiers {
			lu, err := a.dhtms.Lookup(ctx, dhtms.LookupRequest{
				ContentType:   dhtms.ContentTypeToCode(string(ref.Type)),
				ContentSource: ref.Source,
				ContentID:     ref.ID,
				Season:        seasonPtr,
				Episode:       episodePtr,
				Tier:          tier,
				Limit:         fetchLimit,
			})
			if err != nil {
				recordErr(err)
				return search.TorrentContentResult{}, false
			}
			for _, h := range lu.Linked {
				if len(h.InfoHashV1) == 40 {
					if _, dup := seenLinked[h.InfoHashV1]; !dup {
						seenLinked[h.InfoHashV1] = struct{}{}
						linkedHits = append(linkedHits, taggedHit{
							hit: h, contentSource: ref.Source, contentID: ref.ID, linked: true,
						})
					}
				}
			}
			for _, h := range lu.Unlinked {
				if len(h.InfoHashV1) == 40 {
					if _, dupL := seenLinked[h.InfoHashV1]; dupL {
						continue
					}
					if _, dupU := seenUnlinked[h.InfoHashV1]; !dupU {
						seenUnlinked[h.InfoHashV1] = struct{}{}
						unlinkedHits = append(unlinkedHits, taggedHit{
							hit: h, linked: false,
						})
					}
				}
			}
		}
		collected = append(collected, linkedHits...)
		collected = append(collected, unlinkedHits...)

	case effectiveSearchQuery(req) != "":
		sr, err := a.dhtms.Search(ctx, dhtms.SearchRequest{
			Query:     effectiveSearchQuery(req),
			Mode:      dhtms.ModeAnd,
			Tier:      tier,
			Limit:     fetchLimit,
			TVSeason:  seasonPtr,
			TVEpisode: episodePtr,
		})
		if err != nil {
			recordErr(err)
			return search.TorrentContentResult{}, false
		}
		for _, h := range sr.Docs {
			addHit(h, "", "", false)
		}

	default:
		ctPtr := singleContentTypeForTorznab(req)
		rss, err := a.dhtms.RSS(ctx, dhtms.RSSRequest{
			ContentType: ctPtr,
			Tier:        tier,
			Limit:       fetchLimit,
			Offset:      0,
		})
		if err != nil {
			recordErr(err)
			return search.TorrentContentResult{}, false
		}
		for _, h := range rss.Docs {
			addHit(h, "", "", false)
		}
	}

	items := make([]search.TorrentContentResultItem, 0, len(collected))
	for _, th := range collected {
		tc, ok := dhtms.DocHitToTorrentContent(th.hit, th.contentSource, th.contentID)
		if !ok {
			continue
		}
		if !matchesTorznabFilters(req, tc, th.hit.Year, th.linked) {
			continue
		}
		items = append(items, search.TorrentContentResultItem{
			TorrentContent: tc,
		})
	}

	res := search.TorrentContentResult{Items: items}
	if a.liveness != nil || a.verdicts != nil {
		filtered, ferr := a.applyLivenessFilter(ctx, res)
		if ferr == nil {
			res = filtered
		} else if errSink, ok := a.livenessMetrics.(LivenessErrorMetrics); ok {
			errSink.TorznabLookupError()
		}
	}
	if !a.freshness.IsZero() {
		res = a.applyFreshnessFilter(res)
	}
	if a.reranker != nil {
		res.Items = rerankPreservingLinkedFirst(ctx, a.reranker, res.Items, isIDQuery)
	}

	start := int(offset)
	if start >= len(res.Items) {
		res.Items = nil
		res.HasNextPage = false
	} else {
		remaining := res.Items[start:]
		if limit > 0 && len(remaining) > int(limit) {
			res.Items = remaining[:limit]
			res.HasNextPage = true
		} else {
			res.Items = remaining
			res.HasNextPage = false
		}
	}

	a.dhtmsMetrics.RecordRead("torznab", "hit")
	return res, true
}

func singleContentTypeForTorznab(req torznab.SearchRequest) *uint8 {
	var code uint8
	switch req.Type {
	case torznab.FunctionMovie:
		code = dhtms.ContentTypeToCode(string(model.ContentTypeMovie))
	case torznab.FunctionTV:
		code = dhtms.ContentTypeToCode(string(model.ContentTypeTvShow))
	case torznab.FunctionMusic:
		code = dhtms.ContentTypeToCode(string(model.ContentTypeMusic))
	default:
		return nil
	}
	return &code
}

func matchesTorznabFilters(req torznab.SearchRequest, tc model.TorrentContent, year uint16, isLinked bool) bool {
	// Function content-type constraint (keep unlinked fallback hits on ID lookups).
	if tc.ContentType.Valid {
		ct := tc.ContentType.ContentType
		switch req.Type {
		case torznab.FunctionMovie:
			if ct != model.ContentTypeMovie {
				return false
			}
		case torznab.FunctionTV:
			if ct != model.ContentTypeTvShow {
				return false
			}
		case torznab.FunctionMusic:
			if ct != model.ContentTypeMusic {
				return false
			}
		case torznab.FunctionAudio:
			if ct != model.ContentTypeMusic && ct != model.ContentTypeAudiobook {
				return false
			}
		case torznab.FunctionBook:
			if ct != model.ContentTypeEbook && ct != model.ContentTypeComic && ct != model.ContentTypeAudiobook {
				return false
			}
		}
	} else if !isLinked && req.Type != torznab.FunctionSearch && len(requestIdentifierRefs(req)) == 0 {
		return false
	}

	if req.Year.Valid && req.Year.Int >= 1800 && req.Year.Int <= 2200 {
		if int(year) != req.Year.Int {
			return false
		}
	}

	mergedCats := mergeResolutionCats(req.Cats)
	if len(mergedCats) == 0 {
		return true
	}
	for _, cat := range mergedCats {
		if matchesSingleTorznabCat(cat, tc) {
			return true
		}
	}
	return false
}

func matchesSingleTorznabCat(cat int, tc model.TorrentContent) bool {
	if !tc.ContentType.Valid {
		return false
	}
	ct := tc.ContentType.ContentType
	switch {
	case torznab.CategoryMovies.Has(cat):
		if ct != model.ContentTypeMovie {
			return false
		}
		switch cat {
		case torznab.CategoryMoviesSD.ID:
			return !tc.VideoResolution.Valid || !slices.Contains(hdResolutions, tc.VideoResolution.VideoResolution)
		case torznab.CategoryMoviesHD.ID:
			return tc.VideoResolution.Valid && slices.Contains(hdResolutions, tc.VideoResolution.VideoResolution)
		case torznab.CategoryMoviesUHD.ID:
			return tc.VideoResolution.Valid && slices.Contains(uhdResolutions, tc.VideoResolution.VideoResolution)
		default:
			return true
		}
	case torznab.CategoryTV.Has(cat):
		if ct != model.ContentTypeTvShow {
			return false
		}
		switch cat {
		case torznab.CategoryTVSD.ID:
			return !tc.VideoResolution.Valid || !slices.Contains(hdResolutions, tc.VideoResolution.VideoResolution)
		case torznab.CategoryTVHD.ID:
			return tc.VideoResolution.Valid && slices.Contains(hdResolutions, tc.VideoResolution.VideoResolution)
		case torznab.CategoryTVUHD.ID:
			return tc.VideoResolution.Valid && slices.Contains(uhdResolutions, tc.VideoResolution.VideoResolution)
		default:
			return true
		}
	case torznab.CategoryXXX.Has(cat):
		return ct == model.ContentTypeXxx
	case torznab.CategoryPC.Has(cat):
		return ct == model.ContentTypeSoftware || ct == model.ContentTypeGame
	case torznab.CategoryAudioAudiobook.Has(cat):
		return ct == model.ContentTypeAudiobook
	case torznab.CategoryAudio.Has(cat):
		return ct == model.ContentTypeMusic
	case torznab.CategoryBooksComics.Has(cat):
		return ct == model.ContentTypeComic
	case torznab.CategoryBooks.Has(cat):
		return ct == model.ContentTypeEbook || ct == model.ContentTypeComic || ct == model.ContentTypeAudiobook
	default:
		return true
	}
}
