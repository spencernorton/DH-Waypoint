package importer

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/gin-gonic/gin"
	"github.com/spencernorton/DH-Waypoint/internal/database/dao"
	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/namepolicy"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/restapi"
	"github.com/spencernorton/DH-Waypoint/internal/serving"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

func syntheticMetainfo(t *testing.T, kind, name string, private bool) []byte {
	t.Helper()
	data := []byte("synthetic data")
	m := map[string]any{"name": name, "piece length": int64(16384)}
	if private {
		m["private"] = int64(1)
	}
	if kind != "v2" {
		hash := sha1.Sum(data)
		m["length"] = len(data)
		m["pieces"] = hash[:]
	}
	if kind != "v1" {
		hash := sha256.Sum256(data)
		m["meta version"] = int64(2)
		m["file tree"] = map[string]any{name: map[string]any{"": map[string]any{"length": len(data), "pieces root": hash[:]}}}
	}
	info, err := bencode.Marshal(m)
	require.NoError(t, err)
	raw, err := bencode.Marshal(map[string]any{"info": bencode.Bytes(info), "piece layers": map[string]string{}})
	require.NoError(t, err)
	return raw
}

func v2CatalogueEngine(t *testing.T, i *activeImport) *gin.Engine {
	t.Helper()
	policy, err := serving.NewPolicy(serving.Config{ExcludeAdult: true, ProcessorExclusions: true}, []string{"siterip"}, []string{"mkv", "jpg"}, i.namePolicy)
	require.NoError(t, err)
	s := search.New(search.Params{Query: lazy.New(func() (*dao.Query, error) { return i.dao, nil }), ServingPolicy: policy})
	gin.SetMode(gin.TestMode)
	e := gin.New()
	require.NoError(t, restapi.New(s.ServingSearch, zaptest.NewLogger(t).Sugar()).Apply(e))
	return e
}

func catalogueGET(t *testing.T, e *gin.Engine, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	return rec.Code, body
}

func TestPostgresV1V2HybridImportSearchMagnetRoundTripWithoutPlugins(t *testing.T) {
	i, pool := nameImportFixture(t)
	ctx := context.Background()
	e := v2CatalogueEngine(t, i)
	var items []Item
	for _, kind := range []string{"v1", "v2", "hybrid"} {
		item, err := ItemFromTorrentBytes(syntheticMetainfo(t, kind, "Synthetic.File.txt", false), nil, "metainfo_test")
		require.NoError(t, err)
		require.NoError(t, i.persistItems(item))
		items = append(items, item)
		ids := item.metadata.Identities
		for _, h := range []*protocol.InfoHash{ids.V1, ids.V2} {
			if h == nil {
				continue
			}
			status, body := catalogueGET(t, e, "/api/v2/torrents/"+h.String())
			require.Equal(t, 200, status, body)
			row := body["torrent"].(map[string]any)
			require.Equal(t, item.InfoHash.String(), row["info_hash"])
			claims, err := protocol.ParseMagnetIdentities(row["magnet"].(string))
			require.NoError(t, err)
			require.True(t, sameIdentities(ids, claims))
		}
	}
	var n int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM torrents").Scan(&n))
	require.Equal(t, 3, n, "equal names and sizes do not merge identities")
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM torrent_contents").Scan(&n))
	require.Zero(t, n, "raw query does not need classification")
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM processor_registry").Scan(&n))
	require.Zero(t, n, "indexing does not need plugins")
	status, body := catalogueGET(t, e, "/api/v2/search?q=Synthetic&limit=1")
	require.Equal(t, 200, status, body)
	seen := map[string]bool{}
	for {
		rows := body["items"].([]any)
		require.Len(t, rows, 1)
		h := rows[0].(map[string]any)["info_hash"].(string)
		require.False(t, seen[h])
		seen[h] = true
		if !body["has_next_page"].(bool) {
			break
		}
		status, body = catalogueGET(t, e, "/api/v2/search?q=Synthetic&limit=1&after="+body["next_after"].(string))
		require.Equal(t, 200, status, body)
	}
	require.Len(t, seen, 3)
	// A 20-byte prefix and another full SHA-256 with that prefix are distinct.
	v2 := items[1].InfoHash
	other := v2
	other[31] ^= 1
	status, body = catalogueGET(t, e, "/api/v2/torrents/"+other.String())
	require.Equal(t, 404, status, body)
	status, body = catalogueGET(t, e, "/api/v2/torrents/"+v2.WireID().String())
	require.Equal(t, 404, status, body)
	require.Error(t, i.persistItems(Item{InfoHash: other, Name: "Synthetic.File.txt", Source: "unverified"}), "a bare full hash cannot bypass metainfo validation")
	_, err := ItemFromTorrentBytes(syntheticMetainfo(t, "v2", "Synthetic.File.txt", false), &other, "metainfo_test")
	require.Error(t, err)
}

func TestPostgresHybridAliasPrivacyAndEligibilityProtectBothIdentities(t *testing.T) {
	i, pool := nameImportFixture(t)
	ctx := context.Background()
	item, err := ItemFromTorrentBytes(syntheticMetainfo(t, "hybrid", "Synthetic.Private.txt", false), nil, "metainfo_test")
	require.NoError(t, err)
	require.NoError(t, i.persistItems(item))
	e := v2CatalogueEngine(t, i)
	v2 := *item.metadata.Identities.V2
	status, body := catalogueGET(t, e, "/api/v2/torrents/"+v2.String())
	require.Equal(t, 200, status, body)
	_, err = pool.Exec(ctx, `INSERT INTO label_evidence(info_hash,source,category,source_kind,source_instance,source_object_id,observed_at,strength)
 VALUES($1,'qbittorrent','private','test','synthetic','synthetic',now(),1)`, v2.Bytes())
	require.NoError(t, err)
	for _, h := range []protocol.InfoHash{item.InfoHash, v2} {
		status, body = catalogueGET(t, e, "/api/v2/torrents/"+h.String())
		require.Equal(t, 404, status, body)
	}
	_, err = pool.Exec(ctx, "DELETE FROM label_evidence")
	require.NoError(t, err)
	// An explicit full-v2 hash exclusion also protects the canonical hybrid key.
	i.namePolicy, err = namepolicy.New(namepolicy.Config{Enabled: true, ExcludedInfoHashes: []string{v2.String()}})
	require.NoError(t, err)
	e = v2CatalogueEngine(t, i)
	for _, h := range []protocol.InfoHash{item.InfoHash, v2} {
		status, body = catalogueGET(t, e, "/api/v2/torrents/"+h.String())
		require.Equal(t, 404, status, body)
	}
}

func TestPostgresV2FilesUseValidatedTreeAndBoundedPagination(t *testing.T) {
	i, _ := nameImportFixture(t)
	root := sha256.Sum256([]byte("synthetic data"))
	file := map[string]any{"": map[string]any{"length": 14, "pieces root": root[:]}}
	info, err := bencode.Marshal(map[string]any{"name": "Synthetic.Collection", "piece length": 16384, "meta version": 2, "file tree": map[string]any{"a.txt": file, "dir": map[string]any{"b.txt": file}}})
	require.NoError(t, err)
	raw, err := bencode.Marshal(map[string]any{"info": bencode.Bytes(info), "piece layers": map[string]string{}})
	require.NoError(t, err)
	item, err := ItemFromTorrentBytes(raw, nil, "metainfo_test")
	require.NoError(t, err)
	require.NoError(t, i.persistItems(item))
	e := v2CatalogueEngine(t, i)
	status, body := catalogueGET(t, e, "/api/v2/torrents/"+item.InfoHash.String()+"/files?limit=1")
	require.Equal(t, 200, status, body)
	files := body["files"].([]any)
	require.Len(t, files, 1)
	require.Equal(t, "a.txt", files[0].(map[string]any)["path"])
	require.True(t, body["has_next_page"].(bool))
	status, body = catalogueGET(t, e, "/api/v2/torrents/"+item.InfoHash.String()+"/files?limit=1&after_index=0")
	require.Equal(t, 200, status, body)
	files = body["files"].([]any)
	require.Len(t, files, 1)
	require.Equal(t, "dir/b.txt", files[0].(map[string]any)["path"])
	require.False(t, body["has_next_page"].(bool))
}
