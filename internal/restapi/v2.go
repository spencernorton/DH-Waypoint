package restapi

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"gorm.io/gorm/clause"
)

const catalogueV2Contract = "dh-waypoint.catalogue.v2"

type v2Item struct {
	InfoHash    string                     `json:"info_hash"`
	Identities  protocol.TorrentIdentities `json:"identities"`
	Name        string                     `json:"name"`
	Size        uint                       `json:"size"`
	FilesStatus model.FilesStatus          `json:"files_status"`
	FilesCount  *uint                      `json:"files_count,omitempty"`
	Magnet      string                     `json:"magnet"`
}

func toV2Item(t model.Torrent) v2Item {
	out := v2Item{InfoHash: t.InfoHash.String(), Identities: t.Identities(), Name: t.Name, Size: t.Size, FilesStatus: t.FilesStatus, Magnet: t.MagnetURI()}
	if t.FilesCount.Valid {
		n := t.FilesCount.Uint
		out.FilesCount = &n
	}
	return out
}

func v2Limit(c *gin.Context) (uint, bool) {
	if c.Query("offset") != "" {
		c.JSON(400, gin.H{"error": "v2 pagination uses after, not offset"})
		return 0, false
	}
	if c.Query("limit") == "" {
		return 20, true
	}
	n, err := strconv.ParseUint(c.Query("limit"), 10, 32)
	if err != nil || n < 1 || n > 100 {
		c.JSON(400, gin.H{"error": "limit must be 1-100"})
		return 0, false
	}
	return uint(n), true
}

// Raw catalogue search is available with no classifier or external processor.
// The mandatory ServingSearch policy still gates rows before pagination.
func (a *API) handleV2Search(c *gin.Context) {
	limit, ok := v2Limit(c)
	if !ok {
		return
	}
	s, err := a.search.Get()
	if err != nil {
		a.fail(c, err)
		return
	}
	p := query.SearchParams{Limit: model.NewNullUint(limit), HasNextPage: model.NewNullBool(true)}
	text := strings.TrimSpace(c.Query("q"))
	if len(text) > 4096 || !utf8.ValidString(text) || text != "" && utf8.RuneCountInString(text) < 3 {
		c.JSON(400, gin.H{"error": "q must contain 3-4096 valid UTF-8 characters/bytes"})
		return
	}
	opts := []query.Option{p.Option(), query.OrderBy(query.OrderByColumn{OrderByColumn: clause.OrderByColumn{Column: clause.Column{Table: "torrents", Name: "info_hash"}}})}
	if text != "" {
		escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(text)
		opts = append(opts, query.Where(query.DBCriteria{SQL: "torrents.name ILIKE ?", Args: []any{"%" + escaped + "%"}}))
	}
	if after := c.Query("after"); after != "" {
		h, err := protocol.ParseInfoHash(after)
		if err != nil {
			c.JSON(400, gin.H{"error": "invalid_after"})
			return
		}
		opts = append(opts, query.Where(query.DBCriteria{SQL: "torrents.info_hash > DECODE('" + h.String() + "','hex')"}))
	}
	if hash := c.Query("hash"); hash != "" {
		h, err := protocol.ParseInfoHash(hash)
		if err != nil {
			c.JSON(400, gin.H{"error": "invalid_info_hash"})
			return
		}
		opts = append(opts, query.Where(search.TorrentInfoHashCriteria(h)))
	}
	rows, err := s.Torrents(c.Request.Context(), opts...)
	if err != nil {
		a.fail(c, err)
		return
	}
	items := make([]v2Item, 0, len(rows.Items))
	for _, t := range rows.Items {
		items = append(items, toV2Item(t))
	}
	var after *string
	if rows.HasNextPage && len(rows.Items) > 0 {
		h := rows.Items[len(rows.Items)-1].InfoHash.String()
		after = &h
	}
	c.JSON(200, gin.H{"contract": catalogueV2Contract, "items": items, "has_next_page": rows.HasNextPage, "next_after": after})
}

func (a *API) v2Detail(c *gin.Context) (model.Torrent, bool) {
	h, err := protocol.ParseInfoHash(c.Param("hash"))
	if err != nil {
		c.JSON(400, gin.H{"error": "invalid_info_hash"})
		return model.Torrent{}, false
	}
	s, err := a.search.Get()
	if err != nil {
		a.fail(c, err)
		return model.Torrent{}, false
	}
	rows, err := s.Torrents(c.Request.Context(), query.Where(search.TorrentInfoHashCriteria(h)), query.Limit(1))
	if err != nil {
		a.fail(c, err)
		return model.Torrent{}, false
	}
	if len(rows.Items) == 0 {
		c.JSON(404, gin.H{"error": "not_found"})
		return model.Torrent{}, false
	}
	return rows.Items[0], true
}

func (a *API) handleV2Detail(c *gin.Context) {
	t, ok := a.v2Detail(c)
	if ok {
		c.JSON(200, gin.H{"contract": catalogueV2Contract, "torrent": toV2Item(t)})
	}
}

func (a *API) handleV2Files(c *gin.Context) {
	t, ok := a.v2Detail(c)
	if !ok {
		return
	}
	limit, ok := v2Limit(c)
	if !ok {
		return
	}
	s, err := a.search.Get()
	if err != nil {
		a.fail(c, err)
		return
	}
	opts := []query.Option{query.Where(search.TorrentFileInfoHashCriteria(t.InfoHash)), query.SearchParams{Limit: model.NewNullUint(limit), HasNextPage: model.NewNullBool(true)}.Option(), query.OrderBy(query.OrderByColumn{OrderByColumn: clause.OrderByColumn{Column: clause.Column{Table: "torrent_files", Name: "index"}}})}
	if after := c.Query("after_index"); after != "" {
		n, err := strconv.ParseUint(after, 10, 32)
		if err != nil {
			c.JSON(400, gin.H{"error": "invalid_after_index"})
			return
		}
		opts = append(opts, query.Where(query.DBCriteria{SQL: "torrent_files.index > " + strconv.FormatUint(n, 10)}))
	}
	rows, err := s.TorrentFiles(c.Request.Context(), opts...)
	if err != nil {
		a.fail(c, err)
		return
	}
	files := make([]gin.H, 0, len(rows.Items))
	for _, f := range rows.Items {
		files = append(files, gin.H{"index": f.Index, "path": f.Path, "size": f.Size})
	}
	var after *uint
	if rows.HasNextPage && len(rows.Items) > 0 {
		n := rows.Items[len(rows.Items)-1].Index
		after = &n
	}
	c.JSON(200, gin.H{"contract": catalogueV2Contract, "info_hash": t.InfoHash.String(), "files": files, "has_next_page": rows.HasNextPage, "next_after_index": after})
}
