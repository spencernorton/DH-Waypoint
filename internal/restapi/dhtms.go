package restapi

import (
	"errors"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/spencernorton/DH-Waypoint/internal/database/dhtms"
	"github.com/spencernorton/DH-Waypoint/internal/leanmode"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/serving"
)

// WithDHTMS wires an optional DH-TMS sidecar client for direct REST read
// serving with automatic fail-closed fallback to PostgreSQL.
func (a *API) WithDHTMS(
	client *dhtms.Client,
	policy *serving.Policy,
	metrics *dhtms.Metrics,
	lean *leanmode.Store,
) *API {
	a.dhtms = client
	a.servingPolicy = policy
	a.dhtmsMetrics = metrics
	a.leanStore = lean
	return a
}

func (a *API) dhtmsEligibleBase() bool {
	if a.dhtms == nil || !a.dhtms.Config().ShouldServe() {
		return false
	}
	if a.leanStore != nil {
		if _, _, _, active := a.leanStore.ActiveServingProfile(); active {
			return false
		}
	}
	return true
}

func (a *API) tryHandleV1SearchDHTMS(c *gin.Context) bool {
	if !a.dhtmsEligibleBase() {
		return false
	}
	// Bypass to PostgreSQL when PG-only filters or explicit cursor/sort options are requested.
	if strings.TrimSpace(c.Query("cursor")) != "" ||
		strings.TrimSpace(c.Query("video_source")) != "" ||
		strings.TrimSpace(c.Query("video_codec")) != "" ||
		strings.TrimSpace(c.Query("hdr")) != "" ||
		strings.TrimSpace(c.Query("min_seeders")) != "" ||
		strings.TrimSpace(c.Query("lean_profile")) != "" {
		a.dhtmsMetrics.RecordRead("rest_v1", "bypass")
		return false
	}
	orderByRaw := strings.ToLower(strings.TrimSpace(c.Query("order_by")))
	orderDirRaw := strings.ToLower(strings.TrimSpace(c.Query("order_dir")))
	if orderByRaw != "" && orderByRaw != "relevance" && orderByRaw != "published_at" {
		a.dhtmsMetrics.RecordRead("rest_v1", "bypass")
		return false
	}
	if orderDirRaw != "" && orderDirRaw != "desc" && orderDirRaw != "descending" {
		a.dhtmsMetrics.RecordRead("rest_v1", "bypass")
		return false
	}

	limit, offset, errMsg := paging(c)
	if errMsg != "" {
		return false // Let runSearch emit standard 400 response
	}
	includeTotal := true
	if rawInclude := strings.TrimSpace(c.Query("include_total")); rawInclude != "" {
		parsedBool, parseErr := strconv.ParseBool(rawInclude)
		if parseErr != nil {
			return false
		}
		includeTotal = parsedBool
	}

	var typeFilter *model.ContentType
	if typeParam := strings.TrimSpace(c.Query("type")); typeParam != "" {
		ct, err := model.ParseContentType(typeParam)
		if err != nil {
			return false
		}
		typeFilter = &ct
	}
	var resFilter *model.VideoResolution
	if resParam := strings.TrimSpace(c.Query("resolution")); resParam != "" {
		res, err := parseResolutionParam(resParam)
		if err != nil {
			return false
		}
		resFilter = &res
	}

	qStr := strings.TrimSpace(c.Query("q"))
	imdbParam := strings.TrimSpace(c.Query("imdb"))
	tmdbParam := strings.TrimSpace(c.Query("tmdb"))
	if imdbParam != "" && !imdbIDPattern.MatchString(imdbParam) {
		return false
	}
	if tmdbParam != "" && !tmdbIDPattern.MatchString(tmdbParam) {
		return false
	}

	seasonRaw := strings.TrimSpace(c.Query("season"))
	episodeRaw := strings.TrimSpace(c.Query("episode"))
	if episodeRaw != "" && seasonRaw == "" {
		return false
	}
	var seasonPtr, episodePtr *uint16
	if seasonRaw != "" {
		sNum, err := strconv.Atoi(seasonRaw)
		if err != nil || sNum < 0 || sNum > 1000 {
			return false
		}
		s := uint16(sNum)
		seasonPtr = &s
		if episodeRaw != "" {
			eNum, err := strconv.Atoi(episodeRaw)
			if err != nil || eNum < 0 || eNum > 10000 {
				return false
			}
			e := uint16(eNum)
			episodePtr = &e
		}
		if qStr == "" && imdbParam == "" && tmdbParam == "" {
			a.dhtmsMetrics.RecordRead("rest_v1", "bypass")
			return false
		}
	}

	// If explicit published_at order is requested alongside a text query, delegate to PG.
	if qStr != "" && orderByRaw == "published_at" {
		a.dhtmsMetrics.RecordRead("rest_v1", "bypass")
		return false
	}

	tier := dhtms.TierForPolicy(a.servingPolicy)
	ctx := c.Request.Context()
	fetchLimit := int(limit + offset + 1)
	if fetchLimit < 25 {
		fetchLimit = 25
	}
	if typeFilter != nil || resFilter != nil {
		if fetchLimit < 200 {
			fetchLimit = 200
		}
	}
	if fetchLimit > 500 {
		fetchLimit = 500
	}

	recordErr := func(err error) {
		if errors.Is(err, dhtms.ErrLeaseExpired) {
			a.dhtmsMetrics.RecordRead("rest_v1", "fallback_lease")
		} else {
			a.dhtmsMetrics.RecordRead("rest_v1", "fallback_error")
		}
	}

	type hitWithID struct {
		hit dhtms.DocHit
		src string
		id  string
	}
	var rawHits []hitWithID
	var totalFacetCount *uint64

	switch {
	case imdbParam != "" || tmdbParam != "":
		ctCode := uint8(0)
		if typeFilter != nil {
			ctCode = dhtms.ContentTypeToCode(string(*typeFilter))
		}
		src, cid := "imdb", imdbParam
		if tmdbParam != "" {
			src, cid = "tmdb", tmdbParam
		}
		lu, err := a.dhtms.Lookup(ctx, dhtms.LookupRequest{
			ContentType:   ctCode,
			ContentSource: src,
			ContentID:     cid,
			Season:        seasonPtr,
			Episode:       episodePtr,
			Tier:          tier,
			Limit:         fetchLimit,
		})
		if err != nil {
			recordErr(err)
			return false
		}
		for _, h := range lu.Linked {
			rawHits = append(rawHits, hitWithID{hit: h, src: src, id: cid})
		}
		for _, h := range lu.Unlinked {
			rawHits = append(rawHits, hitWithID{hit: h})
		}

	case qStr != "":
		sr, err := a.dhtms.Search(ctx, dhtms.SearchRequest{
			Query:         qStr,
			Mode:          dhtms.ModeAnd,
			Tier:          tier,
			Limit:         fetchLimit,
			TVSeason:      seasonPtr,
			TVEpisode:     episodePtr,
			IncludeFacets: includeTotal && typeFilter == nil && resFilter == nil && seasonPtr == nil,
		})
		if err != nil {
			recordErr(err)
			return false
		}
		totalFacetCount = sr.TotalFacets
		for _, h := range sr.Docs {
			rawHits = append(rawHits, hitWithID{hit: h})
		}

	default:
		var ctPtr *uint8
		if typeFilter != nil {
			code := dhtms.ContentTypeToCode(string(*typeFilter))
			ctPtr = &code
		}
		rss, err := a.dhtms.RSS(ctx, dhtms.RSSRequest{
			ContentType: ctPtr,
			Tier:        tier,
			Limit:       fetchLimit,
			Offset:      0,
		})
		if err != nil {
			recordErr(err)
			return false
		}
		for _, h := range rss.Docs {
			rawHits = append(rawHits, hitWithID{hit: h})
		}
	}

	filtered := make([]model.TorrentContent, 0, len(rawHits))
	for _, rh := range rawHits {
		tc, ok := dhtms.DocHitToTorrentContent(rh.hit, rh.src, rh.id)
		if !ok {
			continue
		}
		if typeFilter != nil && (!tc.ContentType.Valid || tc.ContentType.ContentType != *typeFilter) {
			continue
		}
		if resFilter != nil && (!tc.VideoResolution.Valid || tc.VideoResolution.VideoResolution != *resFilter) {
			continue
		}
		filtered = append(filtered, tc)
	}

	totalCount := uint(len(filtered))
	if totalFacetCount != nil && *totalFacetCount > uint64(totalCount) {
		totalCount = uint(*totalFacetCount)
	}
	if !includeTotal {
		totalCount = 0
	}

	hasNext := false
	start := int(offset)
	var page []model.TorrentContent
	if start < len(filtered) {
		rem := filtered[start:]
		if limit > 0 && len(rem) > int(limit) {
			page = rem[:limit]
			hasNext = true
		} else {
			page = rem
		}
	}

	items := make([]Item, 0, len(page))
	for _, tc := range page {
		items = append(items, toItem(tc))
	}

	orderName := "published_at"
	if qStr != "" {
		orderName = "relevance"
	}
	var nextCursor string
	if hasNext && len(page) > 0 && orderName != "relevance" {
		nextCursor = encodeV1Cursor(orderName, "desc", page[len(page)-1])
	}

	a.dhtmsMetrics.RecordRead("rest_v1", "hit")
	c.JSON(200, searchResponse{
		TotalCount:           totalCount,
		TotalCountIsEstimate: false,
		TotalCountOmitted:    !includeTotal,
		HasNextPage:          hasNext,
		NextCursor:           nextCursor,
		Items:                items,
	})
	return true
}

func (a *API) tryHandleV2SearchDHTMS(c *gin.Context, text string, limit uint) bool {
	if !a.dhtmsEligibleBase() {
		return false
	}
	tier := dhtms.TierForPolicy(a.servingPolicy)
	resp, err := a.dhtms.Substring(c.Request.Context(), dhtms.SubstringRequest{
		Substring: text,
		Tier:      tier,
		Limit:     int(limit) + 1,
	})
	if err != nil {
		if errors.Is(err, dhtms.ErrLeaseExpired) {
			a.dhtmsMetrics.RecordRead("rest_v2", "fallback_lease")
		} else {
			a.dhtmsMetrics.RecordRead("rest_v2", "fallback_error")
		}
		return false
	}

	torrents := make([]model.Torrent, 0, len(resp.Docs))
	for _, h := range resp.Docs {
		if t, ok := dhtms.DocHitToTorrent(h); ok {
			torrents = append(torrents, t)
		}
	}
	hasNext := false
	if uint(len(torrents)) > limit {
		torrents = torrents[:limit]
		hasNext = true
	}
	items := make([]v2Item, 0, len(torrents))
	for _, t := range torrents {
		items = append(items, toV2Item(t))
	}
	var after *string
	if hasNext && len(torrents) > 0 {
		h := torrents[len(torrents)-1].InfoHash.String()
		after = &h
	}
	a.dhtmsMetrics.RecordRead("rest_v2", "hit")
	c.JSON(200, gin.H{
		"contract":      catalogueV2Contract,
		"items":         items,
		"has_next_page": hasNext,
		"next_after":    after,
	})
	return true
}
