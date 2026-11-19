package query

import (
	"database/sql"
	"fmt"
	"reflect"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencernorton/DH-Waypoint/internal/database/dao"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/telemetry/dualemit"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	bm25PrefixMinSize = 128
	bm25PrefixMaxSize = 8192
)

var searchQueriesTotal = dualemit.NewCounterVec(prometheus.CounterOpts{
	Namespace: "dh_waypoint",
	Subsystem: "search",
	Name:      "queries_total",
	Help:      "Torrent-content text search queries executed, labelled by search engine (bm25, tsquery) and execution path (prefix, direct).",
}, []string{"engine", "path"})

// SearchQueriesCounter returns the Prometheus collector tracking text search
// engine and execution path routing.
func SearchQueriesCounter() prometheus.Collector {
	return searchQueriesTotal
}

func recordSearchQuery(b OptionBuilder, path string) {
	ob, ok := b.(optionBuilder)
	if !ok || ob.tsquery == "" || ob.tableName != model.TableNameTorrentContent {
		return
	}
	engine := "tsquery"
	if _, bm25 := ob.bm25Terms(); bm25 {
		engine = "bm25"
	}
	searchQueriesTotal.With(prometheus.Labels{
		"engine": engine,
		"path":   path,
	}).Inc()
}

type bm25Candidate struct {
	ID    string
	Score float64
}

func extractResultID[T any](item T) (string, bool) {
	v := reflect.ValueOf(item)
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return "", false
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return "", false
	}
	f := v.FieldByName("ID")
	if !f.IsValid() || f.Kind() != reflect.String {
		return "", false
	}
	return f.String(), true
}

func bm25ValuesJoin(candidates []bm25Candidate) (string, []interface{}) {
	var sb strings.Builder
	sb.WriteString("INNER JOIN (VALUES ")
	args := make([]interface{}, 0, len(candidates)*2)
	for i, c := range candidates {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString("(?, ?::float8)")
		args = append(args, c.ID, c.Score)
	}
	sb.WriteString(") AS _bm25_prefix(_bm25_id, _bm25_score) ON _bm25_prefix._bm25_id = torrent_contents.id")
	return sb.String(), args
}

// bm25CandidatePrefixItems preserves ParadeDB Block-Max WAND TopKScanExecState
// for ungrouped relevance searches that carry serving-policy subqueries or
// at-most-one SQL joins. Without this two-stage prefix, EXISTS/NOT EXISTS
// serving guards or INNER JOIN torrents force ParadeDB to degrade Top-K into a
// full index scan plus PostgreSQL sort.
//
// Inside one read-only REPEATABLE READ transaction:
//  1. Pull a bounded top-K prefix (id, pdb.score(id)) from plain single-table
//     torrent_contents so ParadeDB executes TopKScanExecState natively.
//  2. Join those candidate IDs back into the authoritative eligible query
//     (which applies every serving policy, facet, scope, and join) using the
//     precomputed _bm25_score without re-running pdb.score.
//
// Exactness holds when the candidate corpus is exhausted within the prefix OR
// the requested page (+1 for hasNextPage) is filled and its last item strictly
// outranks the prefix boundary score. Otherwise the prefix expands up to
// bm25PrefixMaxSize before falling back to the standard full query.
func (gq *genericQuery[T]) bm25CandidatePrefixItems() ([]T, bool, error) {
	b, ok := gq.builder.(optionBuilder)
	if !ok {
		return nil, false, nil
	}
	terms, bm25 := b.bm25Terms()
	if !bm25 || b.grouping != nil || b.tableName != model.TableNameTorrentContent ||
		!b.limit.Valid || b.limit.Uint == 0 || b.limit.Uint > 1000 || b.offset > 10000 ||
		len(b.groupBy) > 0 || len(b.orderBy) == 0 {
		return nil, false, nil
	}
	if b.orderBy[0].Column.Name != QueryStringRankField || !b.orderBy[0].Desc {
		return nil, false, nil
	}

	required := b.requiredJoins.Copy()
	criteria, err := b.createFacetsFilterCriteria()
	if err != nil {
		return nil, false, err
	}
	raw, err := criteria.Raw(b)
	if err != nil {
		return nil, false, err
	}
	required.SetEntries(raw.Joins.Entries()...)
	for _, ob := range b.orderBy {
		for _, table := range ob.RequiredJoins {
			required.Set(table, struct{}{})
		}
	}
	joins, err := extractRequiredJoins(b.tableName, b.joins, required)
	if err != nil {
		return nil, false, err
	}
	for _, join := range joins {
		if !join.AtMostOne {
			return nil, false, nil
		}
		switch join.Table.TableName() {
		case model.TableNameTorrent, model.TableNameContent, model.TableNameMetadataSource:
		default:
			return nil, false, nil
		}
	}

	sq := gq.factory(gq.ctx, gq.daoQ)
	hinted, hasHint := sq.(prefixCandidateQuery)
	if !hasHint && len(joins) == 0 {
		// Pure single-table query with no serving policy or joins already hits
		// ParadeDB TopKScanExecState directly in one round-trip.
		return nil, false, nil
	}

	// Verify the caller did not pass an active external transaction and no
	// undeclared joins exist on the base query.
	var rendered *gorm.DB
	checkSub := gq.factory(gq.ctx, gq.daoQ)
	if err = b.applySelect(checkSub.UnderlyingDB(), true); err != nil {
		return nil, false, err
	}
	if err = b.applyPre(checkSub, true); err != nil {
		return nil, false, err
	}
	_ = checkSub.UnderlyingDB().ToSQL(func(tx *gorm.DB) *gorm.DB {
		rendered = tx.Find(&[]interface{}{})
		return rendered
	})
	if rendered.Error != nil {
		return nil, false, rendered.Error
	}
	if _, inTx := rendered.Statement.ConnPool.(gorm.TxCommitter); inTx {
		return nil, false, nil
	}
	from, _ := rendered.Statement.Clauses["FROM"].Expression.(clause.From)
	if len(rendered.Statement.Joins) != 0 || len(from.Joins) != len(joins) {
		return nil, false, nil
	}

	// Determine whether all client predicates are single-table torrent_contents
	// filters (no joins). When join-free, pushing them into Step 1 lets Tantivy
	// filter content_type/video_resolution/ep_text/seeders/published_at inside
	// TopKScanExecState; when client filters require joins, Step 1 runs pure
	// search_text &&& ? and Step 2 enforces the joined filters + policy.
	pushClientFilters := len(joins) == 0
	if pushClientFilters {
		var candProbe *gorm.DB
		probeSub := hinted.candidates
		if probeSub == nil {
			probeSub = GenericSubQuery[dao.ITorrentContentDo]{SubQuery: gq.daoQ.TorrentContent.WithContext(gq.ctx).ReadDB()}
		}
		if err = b.applyPre(probeSub, false); err != nil {
			return nil, false, err
		}
		_ = probeSub.UnderlyingDB().ToSQL(func(tx *gorm.DB) *gorm.DB {
			candProbe = tx.Find(&[]interface{}{})
			return candProbe
		})
		if candProbe.Error != nil {
			return nil, false, candProbe.Error
		}
		cf, _ := candProbe.Statement.Clauses["FROM"].Expression.(clause.From)
		if len(candProbe.Statement.Joins) != 0 || len(cf.Joins) != 0 {
			pushClientFilters = false
		}
	}

	wanted := int(b.limit.Uint)
	if b.nextPage {
		wanted++
	}
	needed := int(b.offset) + wanted
	prefix := bm25PrefixMinSize
	for prefix < bm25PrefixMaxSize && prefix < 4*needed {
		prefix *= 2
	}
	if prefix > bm25PrefixMaxSize {
		prefix = bm25PrefixMaxSize
	}

	searchTerms := strings.Join(terms, " ")
	baseDB := gq.factory(gq.ctx, gq.daoQ).UnderlyingDB().Session(&gorm.Session{NewDB: true})
	if _, inTx := baseDB.Statement.ConnPool.(gorm.TxCommitter); inTx {
		return nil, false, nil
	}

	for ; prefix <= bm25PrefixMaxSize; prefix *= 4 {
		var (
			items      []T
			candidates []bm25Candidate
		)
		txErr := baseDB.WithContext(gq.ctx).Transaction(func(tx *gorm.DB) error {
			if settingErr := tx.Exec("SET LOCAL jit = off").Error; settingErr != nil {
				return settingErr
			}
			txQ := dao.Use(tx)

			// Step 1: single-table BM25 Top-K candidate scan (no joins, no policy subqueries).
			candSub := GenericSubQuery[dao.ITorrentContentDo]{
				SubQuery: txQ.TorrentContent.WithContext(gq.ctx).ReadDB(),
			}
			if pushClientFilters {
				candBuilder := b
				candBuilder.dbContext = dbContext{q: txQ, tableName: b.tableName, factory: gq.factory}
				if preErr := candBuilder.applyPre(candSub, false); preErr != nil {
					return preErr
				}
			} else {
				candSub.UnderlyingDB().Where(b.tableName+".search_text &&& ?", searchTerms)
			}
			if scanErr := candSub.UnderlyingDB().
				Select(b.tableName + ".id AS id, pdb.score(" + b.tableName + ".id) AS score").
				Order("score DESC").
				Limit(prefix).
				Scan(&candidates).Error; scanErr != nil {
				return scanErr
			}
			if len(candidates) == 0 {
				return nil
			}

			// Step 2: authoritative eligible query on the bounded candidate set.
			eligibleSub := gq.factory(gq.ctx, txQ)
			joinSQL, joinArgs := bm25ValuesJoin(candidates)
			eligibleSub.UnderlyingDB().Joins(joinSQL, joinArgs...)

			eligibleBuilder := b
			eligibleBuilder.dbContext = dbContext{q: txQ, tableName: b.tableName, factory: gq.factory}
			eligibleBuilder.bm25PrefixScoreExpr = "_bm25_prefix._bm25_score"

			if selErr := eligibleBuilder.applySelect(eligibleSub.UnderlyingDB(), true); selErr != nil {
				return selErr
			}
			if preErr := eligibleBuilder.applyPre(eligibleSub, true); preErr != nil {
				return preErr
			}
			if postErr := eligibleBuilder.applyPost(eligibleSub.UnderlyingDB()); postErr != nil {
				return postErr
			}
			return eligibleSub.UnderlyingDB().Find(&items).Error
		}, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
		if txErr != nil {
			return nil, false, fmt.Errorf("bm25 candidate prefix: %w", txErr)
		}

		exhausted := len(candidates) < prefix
		if exhausted {
			recordSearchQuery(gq.builder, "prefix")
			return items, true, nil
		}
		if len(items) >= wanted {
			boundaryScore := candidates[len(candidates)-1].Score
			lastID, hasID := extractResultID(items[len(items)-1])
			if hasID {
				scoreByID := make(map[string]float64, len(candidates))
				for _, c := range candidates {
					scoreByID[c.ID] = c.Score
				}
				if lastScore, ok := scoreByID[lastID]; ok && lastScore > boundaryScore {
					recordSearchQuery(gq.builder, "prefix")
					return items, true, nil
				}
			}
		}
		if err = gq.ctx.Err(); err != nil {
			return nil, false, err
		}
	}

	return nil, false, nil
}
