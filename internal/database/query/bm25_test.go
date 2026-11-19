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
