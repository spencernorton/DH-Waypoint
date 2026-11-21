package restapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
)

const maxCursorLen = 512

type v1Cursor struct {
	OrderBy     string  `json:"o"`
	OrderDir    string  `json:"d"`
	PublishedAt string  `json:"p,omitempty"`
	NumVal      *int64  `json:"n,omitempty"`
	InfoHash    string  `json:"h"`
	RowID       string  `json:"id,omitempty"`
}

func encodeV1Cursor(orderBy, orderDir string, tc model.TorrentContent) string {
	c := v1Cursor{
		OrderBy:  orderBy,
		OrderDir: orderDir,
		InfoHash: tc.InfoHash.String(),
	}
	switch orderBy {
	case "published_at":
		c.PublishedAt = tc.PublishedAt.UTC().Format(time.RFC3339Nano)
		c.RowID = tc.ID
	case "seeders":
		v := int64(-1)
		if tc.Seeders.Valid {
			v = int64(tc.Seeders.Uint)
		}
		c.NumVal = &v
	case "leechers":
		v := int64(-1)
		if tc.Leechers.Valid {
			v = int64(tc.Leechers.Uint)
		}
		c.NumVal = &v
	case "size":
		v := int64(tc.Size)
		c.NumVal = &v
	case "files_count":
		v := int64(0)
		if tc.FilesCount.Valid {
			v = int64(tc.FilesCount.Uint)
		}
		c.NumVal = &v
	default:
		return ""
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeV1Cursor(raw string, expectedOrderBy, expectedOrderDir string) (query.DBCriteria, error) {
	if len(raw) == 0 || len(raw) > maxCursorLen {
		return query.DBCriteria{}, errors.New("invalid cursor")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return query.DBCriteria{}, errors.New("invalid cursor encoding")
	}
	var c v1Cursor
	if err := json.Unmarshal(decoded, &c); err != nil {
		return query.DBCriteria{}, errors.New("invalid cursor payload")
	}
	if c.OrderBy != expectedOrderBy || c.OrderDir != expectedOrderDir {
		return query.DBCriteria{}, errors.New("cursor does not match requested order_by/order_dir")
	}
	h, err := protocol.ParseID(c.InfoHash)
	if err != nil {
		return query.DBCriteria{}, errors.New("invalid cursor info_hash")
	}
	op := "<"
	if expectedOrderDir == "asc" {
		op = ">"
	}
	switch expectedOrderBy {
	case "published_at":
		ts, err := time.Parse(time.RFC3339Nano, c.PublishedAt)
		if err != nil || c.RowID == "" {
			return query.DBCriteria{}, errors.New("invalid cursor published_at")
		}
		return query.DBCriteria{
			SQL:  fmt.Sprintf("(torrent_contents.published_at, torrent_contents.info_hash, torrent_contents.id) %s (?, ?, ?)", op),
			Args: []any{ts.UTC(), h[:], c.RowID},
		}, nil
	case "seeders":
		if c.NumVal == nil {
			return query.DBCriteria{}, errors.New("invalid cursor seeders")
		}
		return query.DBCriteria{
			SQL:  fmt.Sprintf("(COALESCE(torrent_contents.seeders, -1), torrent_contents.info_hash) %s (?, ?)", op),
			Args: []any{*c.NumVal, h[:]},
		}, nil
	case "leechers":
		if c.NumVal == nil {
			return query.DBCriteria{}, errors.New("invalid cursor leechers")
		}
		return query.DBCriteria{
			SQL:  fmt.Sprintf("(COALESCE(torrent_contents.leechers, -1), torrent_contents.info_hash) %s (?, ?)", op),
			Args: []any{*c.NumVal, h[:]},
		}, nil
	case "size":
		if c.NumVal == nil || *c.NumVal < 0 {
			return query.DBCriteria{}, errors.New("invalid cursor size")
		}
		return query.DBCriteria{
			SQL:  fmt.Sprintf("(torrent_contents.size, torrent_contents.info_hash) %s (?, ?)", op),
			Args: []any{*c.NumVal, h[:]},
		}, nil
	case "files_count":
		if c.NumVal == nil || *c.NumVal < 0 {
			return query.DBCriteria{}, errors.New("invalid cursor files_count")
		}
		return query.DBCriteria{
			SQL:  fmt.Sprintf("(COALESCE(torrent_contents.files_count, 0), torrent_contents.info_hash) %s (?, ?)", op),
			Args: []any{*c.NumVal, h[:]},
		}, nil
	default:
		return query.DBCriteria{}, errors.New("cursor pagination is not supported for this order_by")
	}
}

func resolveOrderSpec(orderByRaw, orderDirRaw, qStr string) (order search.TorrentContentOrderBy, orderName string, dir search.OrderDirection, dirName string, explicit bool, errMsg string) {
	explicit = orderByRaw != "" || orderDirRaw != ""
	dir = search.OrderDirectionDescending
	dirName = "desc"
	switch orderDirRaw {
	case "", "desc":
		dir = search.OrderDirectionDescending
		dirName = "desc"
	case "asc":
		dir = search.OrderDirectionAscending
		dirName = "asc"
	default:
		return "", "", "", "", false, "order_dir must be asc or desc"
	}
	switch orderByRaw {
	case "":
		if qStr != "" {
			return search.TorrentContentOrderByRelevance, "relevance", dir, dirName, explicit, ""
		}
		return search.TorrentContentOrderByPublishedAt, "published_at", dir, dirName, explicit, ""
	case "published_at":
		return search.TorrentContentOrderByPublishedAt, "published_at", dir, dirName, explicit, ""
	case "seeders":
		return search.TorrentContentOrderBySeeders, "seeders", dir, dirName, explicit, ""
	case "leechers":
		return search.TorrentContentOrderByLeechers, "leechers", dir, dirName, explicit, ""
	case "size":
		return search.TorrentContentOrderBySize, "size", dir, dirName, explicit, ""
	case "files_count":
		return search.TorrentContentOrderByFilesCount, "files_count", dir, dirName, explicit, ""
	case "relevance":
		if qStr == "" {
			return "", "", "", "", false, "order_by=relevance requires q"
		}
		return search.TorrentContentOrderByRelevance, "relevance", dir, dirName, explicit, ""
	default:
		return "", "", "", "", false, "unknown order_by"
	}
}
