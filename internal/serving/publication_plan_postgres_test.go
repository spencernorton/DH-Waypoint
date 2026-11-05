package serving_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/serving"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestRequiredPublicationEmptyApprovalsAvoidCatalogueScan(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, q := newServingPostgresDatabase(t, ctx)
	_, err := pool.Exec(ctx, `
INSERT INTO torrents(info_hash,name,size,private,files_status,created_at,updated_at)
SELECT decode(md5(i::text)||substring(md5('fictional-'||i::text),1,8),'hex'),
 'Fictional.Release.mkv',4096,false,'single',now(),now() FROM generate_series(1,20000) i;
INSERT INTO torrent_contents(info_hash,content_type,tsv,created_at,updated_at)
SELECT info_hash,'movie',to_tsvector('simple','fictional release'),now(),now() FROM torrents;
ANALYZE torrents; ANALYZE torrent_contents;
ANALYZE processor_publication_reviews; ANALYZE processor_publication_sources;
ANALYZE processor_payload_reviews;`)
	require.NoError(t, err)
	for _, payload := range []bool{false, true} {
		cfg := serving.Config{RequiredProcessor: "synthetic-reviewer", RequiredProcessorArtifactDigest: strings.Repeat("a", 64)}
		if payload {
			cfg.RequiredPayloadVerifierArtifactDigest = strings.Repeat("b", 64)
		}
		policy, err := serving.NewPolicy(cfg, []string{"syntheticadult"}, []string{"mkv"})
		require.NoError(t, err)
		statement := q.TorrentContent.UnderlyingDB().Session(&gorm.Session{NewDB: true, DryRun: true}).
			Table("torrent_contents").Select("info_hash").Where(policy.TorrentCondition("torrent_contents")).
			Where("tsv @@ to_tsquery('simple', ?)", "fictional:*").
			Order("ts_rank_cd(tsv, to_tsquery('simple', 'fictional:*')) DESC").Limit(5).
			Find(&[]struct{ InfoHash []byte }{}).Statement
		var raw []byte
		require.NoError(t, pool.QueryRow(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+statement.SQL.String(), statement.Vars...).Scan(&raw))
		var plans []map[string]any
		require.NoError(t, json.Unmarshal(raw, &plans))
		var visit func(map[string]any)
		visit = func(node map[string]any) {
			if node["Relation Name"] == "torrent_contents" {
				require.Zero(t, node["Actual Loops"], "an empty approval set must stop the catalogue scan (payload=%v)", payload)
			}
			for _, child := range children(node) {
				visit(child)
			}
		}
		visit(plans[0]["Plan"].(map[string]any))
	}
}

func children(node map[string]any) []map[string]any {
	var out []map[string]any
	values, _ := node["Plans"].([]any)
	for _, value := range values {
		out = append(out, value.(map[string]any))
	}
	return out
}
