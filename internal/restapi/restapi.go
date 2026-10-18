// Package restapi is a small stable REST read API over the same
// serving-policy search the GraphQL endpoint uses. It exists so UIs
// and scripts that do not want a GraphQL client can still build on
// the core: search, one torrent's detail and files, and catalogue
// totals. Everything is read-only; private torrents are excluded
// by the serving policy exactly as in GraphQL/Torznab. The contract
// is docs/reference/openapi.yaml (also served at
// GET /api/v1/openapi.yaml).
package restapi

import (
	_ "embed"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"go.uber.org/zap"
)

//go:embed openapi.yaml
var openAPISpec []byte

type API struct {
	search lazy.Lazy[search.ServingSearch]
	logger *zap.SugaredLogger
}

func New(s lazy.Lazy[search.ServingSearch], logger *zap.SugaredLogger) *API {
	return &API{search: s, logger: logger}
}

// Key identifies this HTTP option (see httpserver.Options config).
func (a *API) Key() string { return "rest_api" }

func (a *API) Apply(e *gin.Engine) error {
	g := e.Group("/api/v1")
	g.GET("/search", a.handleSearch)
	g.GET("/torrents/:hash", a.handleDetail)
	g.GET("/torrents/:hash/files", a.handleFiles)
	g.GET("/stats", a.handleStats)
	g.GET("/openapi.yaml", func(c *gin.Context) {
		c.Data(200, "application/yaml", openAPISpec)
	})
	return nil
}

// Item is the stable REST representation of one torrent-content row.
type Item struct {
	InfoHash        string `json:"info_hash"`
	Name            string `json:"name"`
	Title           string `json:"title,omitempty"`
	ContentType     string `json:"content_type,omitempty"`
	ContentSource   string `json:"content_source,omitempty"`
	ContentID       string `json:"content_id,omitempty"`
	Size            uint   `json:"size"`
	FilesCount      *uint  `json:"files_count,omitempty"`
	Seeders         *uint  `json:"seeders,omitempty"`
	Leechers        *uint  `json:"leechers,omitempty"`
	PublishedAt     string `json:"published_at"`
	VideoResolution string `json:"video_resolution,omitempty"`
	VideoSource     string `json:"video_source,omitempty"`
	VideoCodec      string `json:"video_codec,omitempty"`
	ReleaseGroup    string `json:"release_group,omitempty"`
	IsAnime         bool   `json:"is_anime"`
	Magnet          string `json:"magnet"`
}

func toItem(tc model.TorrentContent) Item {
	it := Item{
		InfoHash:    tc.InfoHash.String(),
		Name:        tc.Torrent.Name,
		Title:       tc.Content.Title,
		Size:        tc.Size,
		PublishedAt: tc.PublishedAt.UTC().Format(time.RFC3339),
		IsAnime:     tc.IsAnime,
	}
	if tc.ContentType.Valid {
		it.ContentType = tc.ContentType.ContentType.String()
	}
	if tc.ContentSource.Valid {
		it.ContentSource = tc.ContentSource.String
	}
	if tc.ContentID.Valid {
		it.ContentID = tc.ContentID.String
	}
	if tc.FilesCount.Valid {
		v := tc.FilesCount.Uint
		it.FilesCount = &v
	}
	if tc.Seeders.Valid {
		v := tc.Seeders.Uint
		it.Seeders = &v
	}
	if tc.Leechers.Valid {
		v := tc.Leechers.Uint
		it.Leechers = &v
	}
	if tc.VideoResolution.Valid {
		it.VideoResolution = tc.VideoResolution.VideoResolution.String()
	}
	if tc.VideoSource.Valid {
		it.VideoSource = tc.VideoSource.VideoSource.String()
	}
	if tc.VideoCodec.Valid {
		it.VideoCodec = tc.VideoCodec.VideoCodec.String()
	}
	if tc.ReleaseGroup.Valid {
		it.ReleaseGroup = tc.ReleaseGroup.String
	}
	it.Magnet = "magnet:?xt=urn:btih:" + it.InfoHash + "&dn=" + url.QueryEscape(it.Name)
	return it
}

type searchResponse struct {
	TotalCount  uint   `json:"total_count"`
	HasNextPage bool   `json:"has_next_page"`
	Items       []Item `json:"items"`
}

func paging(c *gin.Context) (limit, offset uint, errMsg string) {
	limit, offset = 20, 0
	if v := c.Query("limit"); v != "" {
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil || n > 100 {
			return 0, 0, "limit must be 0-100"
		}
		limit = uint(n)
	}
	if v := c.Query("offset"); v != "" {
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return 0, 0, "offset must be a non-negative integer"
		}
		offset = uint(n)
	}
	return limit, offset, ""
}

func (a *API) runSearch(c *gin.Context, extra ...query.Option) (search.TorrentContentResult, bool) {
	s, err := a.search.Get()
	if err != nil {
		a.fail(c, err)
		return search.TorrentContentResult{}, false
	}
	limit, offset, errMsg := paging(c)
	if errMsg != "" {
		c.JSON(400, gin.H{"error": errMsg})
		return search.TorrentContentResult{}, false
	}
	params := query.SearchParams{
		Limit:       model.NewNullUint(limit),
		Offset:      model.NewNullUint(offset),
		TotalCount:  model.NewNullBool(true),
		HasNextPage: model.NewNullBool(true),
	}
	if q := strings.TrimSpace(c.Query("q")); q != "" {
		params.QueryString = model.NewNullString(q)
	}
	options := []query.Option{search.TorrentContentDefaultOption(), params.Option()}
	if typeParam := strings.TrimSpace(c.Query("type")); typeParam != "" {
		ct, err := model.ParseContentType(typeParam)
		if err != nil {
			c.JSON(400, gin.H{"error": "unknown content type"})
			return search.TorrentContentResult{}, false
		}
		options = append(options, query.Where(search.TorrentContentTypeCriteria(ct)))
	}
	options = append(options, extra...)
	result, err := s.TorrentContent(c.Request.Context(), options...)
	if err != nil {
		a.fail(c, err)
		return search.TorrentContentResult{}, false
	}
	return result, true
}

func (a *API) fail(c *gin.Context, err error) {
	if a.logger != nil {
		a.logger.Warnw("rest api", "error", err)
	}
	c.JSON(500, gin.H{"error": "internal_error"})
}

func (a *API) handleSearch(c *gin.Context) {
	result, ok := a.runSearch(c)
	if !ok {
		return
	}
	items := make([]Item, 0, len(result.Items))
	for _, row := range result.Items {
		items = append(items, toItem(row.TorrentContent))
	}
	c.JSON(200, searchResponse{TotalCount: result.TotalCount, HasNextPage: result.HasNextPage, Items: items})
}

func parseHash(c *gin.Context) (protocol.ID, bool) {
	id, err := protocol.ParseID(c.Param("hash"))
	if err != nil {
		c.JSON(400, gin.H{"error": "invalid_info_hash"})
		return protocol.ID{}, false
	}
	return id, true
}

func (a *API) handleDetail(c *gin.Context) {
	hash, ok := parseHash(c)
	if !ok {
		return
	}
	result, ok := a.runSearch(c, query.Where(search.TorrentContentInfoHashCriteria(hash)))
	if !ok {
		return
	}
	if len(result.Items) == 0 {
		c.JSON(404, gin.H{"error": "not_found"})
		return
	}
	c.JSON(200, toItem(result.Items[0].TorrentContent))
}

type fileItem struct {
	Path string `json:"path"`
	Size uint   `json:"size"`
}

func (a *API) handleFiles(c *gin.Context) {
	hash, ok := parseHash(c)
	if !ok {
		return
	}
	s, err := a.search.Get()
	if err != nil {
		a.fail(c, err)
		return
	}
	result, err := s.TorrentFiles(c.Request.Context(),
		query.Where(search.TorrentFileInfoHashCriteria(hash)),
		query.Limit(10000))
	if err != nil {
		a.fail(c, err)
		return
	}
	files := make([]fileItem, 0, len(result.Items))
	for _, f := range result.Items {
		files = append(files, fileItem{Path: f.Path, Size: f.Size})
	}
	c.JSON(200, gin.H{"info_hash": hash.String(), "files": files})
}

func (a *API) handleStats(c *gin.Context) {
	s, err := a.search.Get()
	if err != nil {
		a.fail(c, err)
		return
	}
	params := query.SearchParams{Limit: model.NewNullUint(0), TotalCount: model.NewNullBool(true)}
	result, err := s.TorrentContent(c.Request.Context(), search.TorrentContentDefaultOption(), params.Option())
	if err != nil {
		a.fail(c, err)
		return
	}
	c.JSON(200, gin.H{"torrent_contents": result.TotalCount})
}
