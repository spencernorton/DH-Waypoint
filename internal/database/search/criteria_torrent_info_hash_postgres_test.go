package search

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/stretchr/testify/require"
)

func TestInfoHashPostgresPreservesFullIdentitiesAndAliases(t *testing.T) {
	f := newAdultServingPostgresFixture(t)
	ctx := context.Background()
	legacy := protocol.MustParseInfoHash(strings.Repeat("a1", 20))
	hybrid := protocol.MustParseInfoHash(strings.Repeat("b2", 20))
	alias := protocol.MustParseInfoHash(strings.Repeat("c3", 32))
	pure := protocol.MustParseInfoHash(strings.Repeat("d4", 32))
	// Sharing a discovery prefix never makes distinct full identities aliases.
	collision := protocol.MustParseInfoHash(strings.Repeat("d4", 20) + strings.Repeat("e5", 12))
	prefix := protocol.MustParseInfoHash(strings.Repeat("d4", 20))
	for _, hash := range []protocol.InfoHash{legacy, hybrid, pure, collision, prefix} {
		_, err := f.pool.Exec(ctx, `INSERT INTO torrents(info_hash,name) VALUES($1,'Synthetic identity')`, hash.Bytes())
		require.NoError(t, err)
		_, err = f.pool.Exec(ctx, `INSERT INTO torrent_contents(id,info_hash) VALUES($1,$2)`, hash.String(), hash.Bytes())
		require.NoError(t, err)
		_, err = f.pool.Exec(ctx, `INSERT INTO torrent_files(info_hash,path) VALUES($1,'Synthetic.mkv')`, hash.Bytes())
		require.NoError(t, err)
	}
	_, err := f.pool.Exec(ctx, `UPDATE torrents SET info_hash_v2=$1 WHERE info_hash=$2`, alias.Bytes(), hybrid.Bytes())
	require.NoError(t, err)
	for _, table := range []string{"torrents", "torrent_contents", "torrent_files"} {
		for _, tc := range []struct {
			name string
			keys []protocol.InfoHash
			want []string
		}{
			{"empty", nil, nil},
			{"v1", []protocol.InfoHash{legacy}, []string{legacy.String()}},
			{"hybrid alias", []protocol.InfoHash{alias}, []string{hybrid.String()}},
			{"duplicate aliases", []protocol.InfoHash{alias, hybrid, alias}, []string{hybrid.String()}},
			{"pure v2", []protocol.InfoHash{pure}, []string{pure.String()}},
			{"other full v2", []protocol.InfoHash{collision}, []string{collision.String()}},
			{"prefix is v1 only", []protocol.InfoHash{prefix}, []string{prefix.String()}},
			{"mixed", []protocol.InfoHash{legacy, alias, pure}, []string{legacy.String(), hybrid.String(), pure.String()}},
		} {
			t.Run(table+"/"+tc.name, func(t *testing.T) {
				raw, e := infoHashCriteria(table, tc.keys...).Raw(nil)
				require.NoError(t, e)
				rows, e := f.pool.Query(ctx, "SELECT encode(info_hash,'hex') FROM "+table+" WHERE "+raw.Query.(string), raw.Args...)
				require.NoError(t, e)
				defer rows.Close()
				var got []string
				for rows.Next() {
					var hash string
					require.NoError(t, rows.Scan(&hash))
					got = append(got, hash)
				}
				require.NoError(t, rows.Err())
				require.ElementsMatch(t, tc.want, got)
			})
		}
	}
}

func TestInfoHashPostgresPageUsesIndexedKeyLookups(t *testing.T) {
	f := newAdultServingPostgresFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := f.pool.Exec(ctx, `
CREATE UNIQUE INDEX ON torrents(info_hash_v2) WHERE info_hash_v2 IS NOT NULL;
INSERT INTO torrents(info_hash,info_hash_v2,name)
SELECT decode(md5('key-'||i)||'00000000','hex'),
       CASE WHEN i%2=0 THEN decode(md5('alias-'||i)||md5('tail-'||i),'hex') END,
       'Synthetic row '||i FROM generate_series(1,100000) i;
ANALYZE torrents;`)
	require.NoError(t, err)
	rows, err := f.pool.Query(ctx, `SELECT info_hash,info_hash_v2 FROM torrents
WHERE name LIKE 'Synthetic row %' ORDER BY info_hash LIMIT 60`)
	require.NoError(t, err)
	var keys []protocol.InfoHash
	for rows.Next() {
		var key, alias []byte
		require.NoError(t, rows.Scan(&key, &alias))
		if alias != nil {
			key = alias
		}
		keys = append(keys, protocol.MustNewInfoHash(key))
	}
	require.NoError(t, rows.Err())
	rows.Close()
	require.Len(t, keys, 60)
	raw, err := TorrentInfoHashCriteria(keys...).Raw(nil)
	require.NoError(t, err)
	var count int
	require.NoError(t, f.pool.QueryRow(ctx, "SELECT count(*) FROM torrents WHERE "+raw.Query.(string)).Scan(&count))
	require.Equal(t, 60, count)
	var plan []byte
	require.NoError(t, f.pool.QueryRow(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) SELECT * FROM torrents WHERE "+raw.Query.(string)).Scan(&plan))
	var documents []map[string]any
	require.NoError(t, json.Unmarshal(plan, &documents))
	var visit func(map[string]any)
	visit = func(node map[string]any) {
		if node["Relation Name"] == "torrents" {
			require.NotEqual(t, "Seq Scan", node["Node Type"], "a page must not scan the catalogue")
		}
		children, _ := node["Plans"].([]any)
		for _, child := range children {
			visit(child.(map[string]any))
		}
	}
	visit(documents[0]["Plan"].(map[string]any))
}
