package query

import (
	"context"
	"strings"
	"testing"

	"github.com/spencernorton/DH-Waypoint/internal/database/dao"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func bm25Builder(t *testing.T, gdb *gorm.DB, search string, bm25 bool, grouped bool) optionBuilder {
	t.Helper()

	b := newQueryContext(dbContext{q: dao.Use(gdb), tableName: "torrent_contents"}).
		QueryString(search).
		OrderBy(OrderByColumn{OrderByColumn: clause.OrderByColumn{
			Column: clause.Column{Name: QueryStringRankField},
			Desc:   true,
		}}).
		Limit(10)

	if grouped {
		b = b.GroupByDistinctOn(testGroupKeySQL, testGroupOrderSQL)
	}

	next, err := WithBM25Search(bm25)(b)
	if err != nil {
		t.Fatalf("WithBM25Search: %v", err)
	}

	return next.(optionBuilder)
}

func bm25QuerySQL(t *testing.T, gdb *gorm.DB, b optionBuilder) string {
	t.Helper()

	return gdb.ToSQL(func(tx *gorm.DB) *gorm.DB {
		sq := GenericSubQuery[dao.ITorrentContentDo]{SubQuery: dao.Use(tx).TorrentContent.WithContext(context.Background()).ReadDB()}
		if err := b.applySelect(sq.UnderlyingDB(), true); err != nil {
			t.Fatalf("applySelect: %v", err)
		}
		if err := b.applyPre(sq, true); err != nil {
			t.Fatalf("applyPre: %v", err)
		}
		if err := b.applyPost(sq.UnderlyingDB()); err != nil {
			t.Fatalf("applyPost: %v", err)
		}

		return sq.UnderlyingDB().Find(&map[string]any{})
	})
}

func TestBM25EligibleQueryUsesParadeDBScoreAndSkipsCap(t *testing.T) {
	t.Parallel()

	gdb := newDryRunDB(t)
	b := bm25Builder(t, gdb, "synthetic 1080p", true, false)

	if _, ok := b.bm25Terms(); !ok {
		t.Fatal("expected BM25-eligible terms")
	}

	sql := bm25QuerySQL(t, gdb, b)
	t.Logf("BM25 SQL:\n%s", sql)

	for _, want := range []string{"torrent_contents.search_text &&&", "pdb.score(torrent_contents.id)"} {
		if !strings.Contains(sql, want) {
			t.Errorf("BM25 SQL missing %q in:\n%s", want, sql)
		}
	}
	for _, unwanted := range []string{"ts_rank_cd", "tc_cap", "tsv @@"} {
		if strings.Contains(sql, unwanted) {
			t.Errorf("BM25 SQL must not contain %q in:\n%s", unwanted, sql)
		}
	}
}

func TestBM25IneligibleAndDisabledQueriesKeepTsqueryPath(t *testing.T) {
	t.Parallel()

	gdb := newDryRunDB(t)

	for _, tc := range []struct {
		name   string
		search string
		bm25   bool
	}{
		{"bare season falls back", "synthetic s5", true},
		{"disabled keeps stock path", "synthetic 1080p", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			b := bm25Builder(t, gdb, tc.search, tc.bm25, false)
			if _, ok := b.bm25Terms(); ok {
				t.Fatal("expected BM25-ineligible resolution")
			}

			sql := bm25QuerySQL(t, gdb, b)
			for _, want := range []string{"ts_rank_cd", "tc_cap", "tsv @@"} {
				if !strings.Contains(sql, want) {
					t.Errorf("stock SQL missing %q in:\n%s", want, sql)
				}
			}
			if strings.Contains(sql, "pdb.score") || strings.Contains(sql, "&&&") {
				t.Errorf("stock SQL must not use ParadeDB operators in:\n%s", sql)
			}
		})
	}
}

func TestBM25GroupedQueriesStayOnTsqueryPath(t *testing.T) {
	t.Parallel()

	gdb := newDryRunDB(t)
	b := bm25Builder(t, gdb, "synthetic 1080p", true, true)

	if _, ok := b.bm25Terms(); ok {
		t.Fatal("grouped queries must never resolve BM25 terms")
	}
	if b.shouldTryCteStrategy() {
		t.Fatal("grouped queries must not try the CTE strategy")
	}
}

func TestBM25DisablesCteStrategy(t *testing.T) {
	t.Parallel()

	gdb := newDryRunDB(t)
	build := func(bm25 bool) optionBuilder {
		b := newQueryContext(dbContext{q: dao.Use(gdb), tableName: "torrent_contents"}).
			QueryString("synthetic 1080p").
			OrderBy(OrderByColumn{OrderByColumn: clause.OrderByColumn{
				Column: clause.Column{Table: "torrent_contents", Name: "published_at"},
				Desc:   true,
			}}).
			Limit(10)
		next, err := WithBM25Search(bm25)(b)
		if err != nil {
			t.Fatalf("WithBM25Search: %v", err)
		}
		return next.(optionBuilder)
	}

	if !build(false).shouldTryCteStrategy() {
		t.Fatal("expected stock tsquery ordering to try the CTE strategy")
	}
	if build(true).shouldTryCteStrategy() {
		t.Fatal("BM25 execution must not race the materialized CTE strategy")
	}
}

func TestBM25PrefixScoreExprSkipsRedundantPredicateAndScore(t *testing.T) {
	t.Parallel()

	gdb := newDryRunDB(t)
	b := bm25Builder(t, gdb, "synthetic 1080p", true, false)
	b.bm25PrefixScoreExpr = "_bm25_prefix._bm25_score"

	sql := bm25QuerySQL(t, gdb, b)
	if !strings.Contains(sql, "_bm25_prefix._bm25_score AS _order_0") {
		t.Errorf("prefix SQL missing precomputed score alias in:\n%s", sql)
	}
	for _, unwanted := range []string{"pdb.score(", "search_text &&&", "ts_rank_cd", "tsv @@"} {
		if strings.Contains(sql, unwanted) {
			t.Errorf("prefix SQL must not repeat %q in:\n%s", unwanted, sql)
		}
	}
}

func TestBM25ValuesJoinAndExtractResultID(t *testing.T) {
	t.Parallel()

	sql, args := bm25ValuesJoin([]bm25Candidate{{ID: "a", Score: 2.5}, {ID: "b", Score: 1.25}})
	if !strings.Contains(sql, "INNER JOIN (VALUES (?, ?::float8), (?, ?::float8)) AS _bm25_prefix(_bm25_id, _bm25_score)") {
		t.Fatalf("unexpected VALUES join SQL: %s", sql)
	}
	if len(args) != 4 || args[0] != "a" || args[1] != 2.5 || args[2] != "b" || args[3] != 1.25 {
		t.Fatalf("unexpected VALUES join args: %#v", args)
	}

	type inner struct{ ID string }
	type wrapped struct{ inner }
	if id, ok := extractResultID(wrapped{inner: inner{ID: "tc-42"}}); !ok || id != "tc-42" {
		t.Fatalf("extractResultID(wrapped) = (%q, %v), want (tc-42, true)", id, ok)
	}
	if id, ok := extractResultID(&wrapped{inner: inner{ID: "tc-99"}}); !ok || id != "tc-99" {
		t.Fatalf("extractResultID(*wrapped) = (%q, %v), want (tc-99, true)", id, ok)
	}
}

func TestFreshDBStripsDeferredScopes(t *testing.T) {
	t.Parallel()

	gdb := newDryRunDB(t)
	scoped := gdb.Scopes(func(tx *gorm.DB) *gorm.DB {
		return tx.Where("torrent_contents.info_hash IN (SELECT info_hash FROM processor_publication_reviews)")
	})
	clean := freshDB(scoped, context.Background())
	sql := clean.ToSQL(func(tx *gorm.DB) *gorm.DB {
		return dao.Use(tx).TorrentContent.WithContext(context.Background()).ReadDB().UnderlyingDB().
			Where("torrent_contents.search_text &&& ?", "synthetic 1080p").
			Select("torrent_contents.id AS id, pdb.score(torrent_contents.id) AS score").
			Order("score DESC").
			Limit(128).
			Scan(&[]bm25Candidate{})
	})
	if strings.Contains(sql, "processor_publication_reviews") {
		t.Fatalf("freshDB leaked deferred scope into clean query:\n%s", sql)
	}
	if !strings.Contains(sql, "search_text &&&") {
		t.Fatalf("expected search_text predicate in:\n%s", sql)
	}
}

func TestIsBM25IndexedWhereSQL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		sql  string
		want bool
	}{
		{"no where clause", `SELECT * FROM "torrent_contents"`, true},
		{"content_type IN", `SELECT * FROM "torrent_contents" WHERE "torrent_contents"."content_type" IN ('movie')`, true},
		{"video_resolution eq", `SELECT * FROM "torrent_contents" WHERE torrent_contents.video_resolution = 'V1080p'`, true},
		{"ep_text single", `SELECT * FROM "torrent_contents" WHERE torrent_contents.ep_text ||| 's2all s2e5'`, true},
		{"ep_text multi", `SELECT * FROM "torrent_contents" WHERE (torrent_contents.ep_text &&& 's2all' OR torrent_contents.ep_text &&& 's2e5 s2e6')`, true},
		{"seeders and published_at", `SELECT * FROM "torrent_contents" WHERE torrent_contents.seeders >= 10 AND torrent_contents.published_at > '2026-01-01'::timestamptz`, true},
		{"quoted keyword inside literal allowed", `SELECT * FROM "torrent_contents" WHERE torrent_contents.search_text &&& 'select exists octet_length'`, true},
		{"legacy octet_length rejected", `SELECT * FROM "torrent_contents" WHERE octet_length(torrent_contents.info_hash)=20`, false},
		{"non-indexed video_codec rejected", `SELECT * FROM "torrent_contents" WHERE torrent_contents.video_codec = 'H264'`, false},
		{"jsonb_exists episodes rejected", `SELECT * FROM "torrent_contents" WHERE jsonb_exists(torrent_contents.episodes, '2')`, false},
		{"release_attributes jsonb rejected", `SELECT * FROM "torrent_contents" WHERE torrent_contents.release_attributes @> '{"hdr":"HDR10"}'`, false},
		{"subquery IN SELECT rejected", `SELECT * FROM "torrent_contents" WHERE torrent_contents.info_hash IN (SELECT info_hash FROM processor_payload_reviews)`, false},
		{"EXISTS subquery rejected", `SELECT * FROM "torrent_contents" WHERE EXISTS (SELECT 1 FROM torrents t WHERE t.info_hash = torrent_contents.info_hash)`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isBM25IndexedWhereSQL(tc.sql); got != tc.want {
				t.Errorf("isBM25IndexedWhereSQL(%q) = %v, want %v", tc.sql, got, tc.want)
			}
		})
	}
}

func TestWithoutRelevanceCapSkipsTcCap(t *testing.T) {
	t.Parallel()

	gdb := newDryRunDB(t)
	b := bm25Builder(t, gdb, "synthetic 1080p", false, false)
	next, err := WithoutRelevanceCap()(b)
	if err != nil {
		t.Fatalf("WithoutRelevanceCap: %v", err)
	}
	sql := bm25QuerySQL(t, gdb, next.(optionBuilder))
	for _, want := range []string{"tsv @@", "ts_rank_cd("} {
		if !strings.Contains(sql, want) {
			t.Errorf("uncapped tsquery SQL missing %q in:\n%s", want, sql)
		}
	}
	if strings.Contains(sql, "tc_cap") {
		t.Errorf("WithoutRelevanceCap must omit tc_cap subquery in:\n%s", sql)
	}
}

func TestFilterBM25Step1Scopes(t *testing.T) {
	t.Parallel()

	gdb := newDryRunDB(t)
	scopes := []Scope{
		func(db *gorm.DB) error { db.Where("octet_length(torrent_contents.info_hash)=20"); return nil },
		func(db *gorm.DB) error { db.Where("torrent_contents.ep_text ||| ?", "s2all s2e5"); return nil },
		func(db *gorm.DB) error { db.Where("torrents.extension = ?", "mkv"); return nil },
		func(db *gorm.DB) error { db.Where("torrent_contents.content_type = ?", "movie"); return nil },
	}
	kept, err := filterBM25Step1Scopes(gdb, context.Background(), "torrent_contents", scopes, nil)
	if err != nil {
		t.Fatalf("filterBM25Step1Scopes: %v", err)
	}
	if len(kept) != 2 {
		t.Fatalf("len(kept) = %d, want 2 (only ep_text and content_type)", len(kept))
	}
	sql := freshDB(gdb, context.Background()).Table("torrent_contents").ToSQL(func(tx *gorm.DB) *gorm.DB {
		initTx := tx.Session(&gorm.Session{Initialized: true})
		for _, sc := range kept {
			_ = sc(initTx)
		}
		return initTx.Find(&[]interface{}{})
	})
	for _, want := range []string{"ep_text |||", "content_type ="} {
		if !strings.Contains(sql, want) {
			t.Errorf("kept scopes SQL missing %q in:\n%s", want, sql)
		}
	}
	for _, unwanted := range []string{"octet_length", "torrents.extension"} {
		if strings.Contains(sql, unwanted) {
			t.Errorf("kept scopes SQL must not include %q in:\n%s", unwanted, sql)
		}
	}
}

func TestFilterBM25Step1ScopesWithExtras(t *testing.T) {
	t.Parallel()

	gdb := newDryRunDB(t)
	scopes := []Scope{
		func(db *gorm.DB) error { db.Where("torrent_contents.video_codec = ?", "x265"); return nil },
		func(db *gorm.DB) error { db.Where("torrent_contents.is_anime = ?", true); return nil },
		func(db *gorm.DB) error { db.Where("octet_length(torrent_contents.info_hash)=20"); return nil },
	}
	baseKept, err := filterBM25Step1Scopes(gdb, context.Background(), "torrent_contents", scopes, nil)
	if err != nil {
		t.Fatalf("filterBM25Step1Scopes(nil): %v", err)
	}
	if len(baseKept) != 0 {
		t.Fatalf("expected 0 kept scopes without extra columns, got %d", len(baseKept))
	}
	extras := map[string]struct{}{"video_codec": {}, "is_anime": {}}
	extraKept, err := filterBM25Step1Scopes(gdb, context.Background(), "torrent_contents", scopes, extras)
	if err != nil {
		t.Fatalf("filterBM25Step1Scopes(extras): %v", err)
	}
	if len(extraKept) != 2 {
		t.Fatalf("expected 2 kept scopes with video_codec+is_anime extras, got %d", len(extraKept))
	}
}
