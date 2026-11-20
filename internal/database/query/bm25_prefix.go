package query

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencernorton/DH-Waypoint/internal/database/dao"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/telemetry/dualemit"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	bm25PrefixMinSize      = 128
	bm25PrefixMaxSize      = 8192
	bm25PrefixSearchBudget = 3 * time.Second
)

// freshDB returns a clean GORM session whose Statement is reset immediately
// (stripping deferred scopes and WHERE clauses such as serving.Policy) while
// preserving ConnPool and binding ctx in one call. Chaining .WithContext()
// after Session{NewDB: true} without Initialized: true drops clone=1 in GORM
// and re-clones the prior Statement.
func freshDB(db *gorm.DB, ctx context.Context) *gorm.DB {
	return db.Session(&gorm.Session{NewDB: true, Initialized: true, Context: ctx})
}

var (
	bm25SQLSingleQuoted = regexp.MustCompile(`'(?:''|[^'])*'`)
	bm25SQLFuncCall     = regexp.MustCompile(`\b([a-z_][a-z0-9_]*)\s*\(`)
	bm25SQLIdent        = regexp.MustCompile(`\b[a-z_][a-z0-9_]*\b`)
	bm25AllowedIdents   = map[string]struct{}{
		"torrent_contents": {},
		"id":               {},
		"search_text":      {},
		"ep_text":          {},
		"content_type":     {},
		"video_resolution": {},
		"seeders":          {},
		"published_at":     {},
		"and":              {},
		"or":               {},
		"not":              {},
		"in":               {},
		"is":               {},
		"null":             {},
		"true":             {},
		"false":            {},
		"between":          {},
		"timestamptz":      {},
		"timestamp":        {},
		"text":             {},
		"integer":          {},
		"int":              {},
		"bigint":           {},
		"boolean":          {},
	}
	bm25AllowedFuncPrefix = map[string]struct{}{
		"in":  {},
		"not": {},
		"and": {},
		"or":  {},
	}
)

// isBM25IndexedWhereSQL reports whether the WHERE predicate of a rendered
// single-table torrent_contents query touches only columns indexed by
// torrent_contents_bm25_idx with no subqueries, joins, or SQL function calls
// (such as octet_length(info_hash) = 20) that force ParadeDB heap_filter scans.
func isBM25IndexedWhereSQL(renderedSQL string) bool {
	stripped := bm25SQLSingleQuoted.ReplaceAllString(renderedSQL, "''")
	upper := strings.ToUpper(stripped)
	whereIdx := strings.Index(upper, " WHERE ")
	if whereIdx < 0 {
		return true
	}
	wherePart := stripped[whereIdx+len(" WHERE "):]
	whereLower := strings.ToLower(wherePart)
	if strings.Contains(whereLower, "->") ||
		strings.Contains(whereLower, "@>") ||
		strings.Contains(whereLower, "#>") ||
		strings.Contains(whereLower, "select ") ||
		strings.Contains(whereLower, "exists") ||
		strings.Contains(whereLower, " join ") {
		return false
	}
	cleaned := strings.ReplaceAll(whereLower, `"`, "")
	for _, m := range bm25SQLFuncCall.FindAllStringSubmatch(cleaned, -1) {
		if _, ok := bm25AllowedFuncPrefix[m[1]]; !ok {
			return false
		}
	}
	for _, ident := range bm25SQLIdent.FindAllString(cleaned, -1) {
		if _, ok := bm25AllowedIdents[ident]; !ok {
			return false
		}
	}
	return true
}

// filterBM25Step1Scopes retains only single-table scopes whose rendered WHERE
// predicates touch columns indexed by torrent_contents_bm25_idx. Inside GORM's
// ToSQL callback, tx starts with clone=2, so Initialized: true is required for
// mutating Scope callbacks (db.Where(...)) to record their predicates on tx.
func filterBM25Step1Scopes(db *gorm.DB, ctx context.Context, tableName string, scopes []Scope) ([]Scope, error) {
	kept := make([]Scope, 0, len(scopes))
	for _, sc := range scopes {
		var scopeErr error
		probeSQL := freshDB(db, ctx).Table(tableName).ToSQL(func(tx *gorm.DB) *gorm.DB {
			initTx := tx.Session(&gorm.Session{Initialized: true})
			scopeErr = sc(initTx)
			return initTx.Find(&[]interface{}{})
		})
		if scopeErr != nil {
			return nil, scopeErr
		}
		if isBM25IndexedWhereSQL(probeSQL) {
			kept = append(kept, sc)
		}
	}
	return kept, nil
}

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
// for ungrouped relevance searches that carry serving-policy subqueries,
// non-BM25 heap filters (such as octet_length(info_hash)=20), or at-most-one
// SQL joins. Without this two-stage prefix, EXISTS/NOT EXISTS serving guards,
// heap filters, or INNER JOIN torrents force ParadeDB to degrade Top-K into a
// full index scan plus PostgreSQL sort.
//
// Inside one read-only REPEATABLE READ transaction:
//  1. Pull a bounded top-K prefix (id, pdb.score(id)) from plain single-table
//     torrent_contents applying only BM25-indexed predicates so ParadeDB
//     executes pure TopKScanExecState natively without heap_filter fetches.
//  2. Join those candidate IDs back into the authoritative eligible query
//     (which applies every serving policy, facet, scope, and join) using the
//     precomputed _bm25_score without re-running pdb.score.
//
// Exactness holds when the candidate corpus is exhausted within the prefix OR
// the requested page (+1 for hasNextPage) is filled and its last item strictly
// outranks the prefix boundary score. If the prefix expands to
// bm25PrefixMaxSize or exceeds bm25PrefixSearchBudget without filling the page,
// it signals fallbackTsquery=true so the caller falls back to tsquery rather
// than ParadeDB NormalScanExecState over the full corpus.
func (gq *genericQuery[T]) bm25CandidatePrefixItems() ([]T, bool, bool, error) {
	b, ok := gq.builder.(optionBuilder)
	if !ok {
		return nil, false, false, nil
	}
	terms, bm25 := b.bm25Terms()
	if !bm25 || b.grouping != nil || b.tableName != model.TableNameTorrentContent ||
		!b.limit.Valid || b.limit.Uint == 0 || b.limit.Uint > 1000 || b.offset > 10000 ||
		len(b.groupBy) > 0 || len(b.orderBy) == 0 {
		return nil, false, false, nil
	}
	if b.orderBy[0].Column.Name != QueryStringRankField || !b.orderBy[0].Desc {
		return nil, false, false, nil
	}

	required := b.requiredJoins.Copy()
	criteria, err := b.createFacetsFilterCriteria()
	if err != nil {
		return nil, false, false, err
	}
	raw, err := criteria.Raw(b)
	if err != nil {
		return nil, false, false, err
	}
	required.SetEntries(raw.Joins.Entries()...)
	for _, ob := range b.orderBy {
		for _, table := range ob.RequiredJoins {
			required.Set(table, struct{}{})
		}
	}
	joins, err := extractRequiredJoins(b.tableName, b.joins, required)
	if err != nil {
		return nil, false, false, err
	}
	for _, join := range joins {
		if !join.AtMostOne {
			return nil, false, false, nil
		}
		switch join.Table.TableName() {
		case model.TableNameTorrent, model.TableNameContent, model.TableNameMetadataSource:
		default:
			return nil, false, false, nil
		}
	}

	sq := gq.factory(gq.ctx, gq.daoQ)
	_, hasHint := sq.(prefixCandidateQuery)

	// Verify the caller did not pass an active external transaction and no
	// undeclared joins exist on the base query.
	var rendered *gorm.DB
	checkSub := gq.factory(gq.ctx, gq.daoQ)
	if err = b.applySelect(checkSub.UnderlyingDB(), true); err != nil {
		return nil, false, false, err
	}
	if err = b.applyPre(checkSub, true); err != nil {
		return nil, false, false, err
	}
	_ = checkSub.UnderlyingDB().ToSQL(func(tx *gorm.DB) *gorm.DB {
		rendered = tx.Find(&[]interface{}{})
		return rendered
	})
	if rendered.Error != nil {
		return nil, false, false, rendered.Error
	}
	if _, inTx := rendered.Statement.ConnPool.(gorm.TxCommitter); inTx {
		return nil, false, false, nil
	}
	from, _ := rendered.Statement.Clauses["FROM"].Expression.(clause.From)
	if len(rendered.Statement.Joins) != 0 || len(from.Joins) != len(joins) {
		return nil, false, false, nil
	}

	// Partition b.scopes into BM25-indexed single-table predicates safe for Step 1
	// (e.g. ep_text, content_type, video_resolution, seeders, published_at) vs
	// non-indexed predicates (e.g. octet_length(info_hash)=20, video_codec,
	// release_attributes, subqueries) that must stay in Step 2 so Step 1 never
	// degrades into ParadeDB heap_filter scans.
	step1Scopes, err := filterBM25Step1Scopes(gq.daoQ.TorrentContent.UnderlyingDB(), gq.ctx, b.tableName, b.scopes)
	if err != nil {
		return nil, false, false, err
	}

	pushFacetsInStep1 := false
	if len(raw.Joins.Keys()) == 0 {
		facetSQL := freshDB(gq.daoQ.TorrentContent.UnderlyingDB(), gq.ctx).Table(b.tableName).ToSQL(func(tx *gorm.DB) *gorm.DB {
			return tx.Where(raw.Query, raw.Args...).Find(&[]interface{}{})
		})
		pushFacetsInStep1 = isBM25IndexedWhereSQL(facetSQL)
	}

	hasNonIndexedScope := len(step1Scopes) < len(b.scopes)
	if !hasHint && len(joins) == 0 && !hasNonIndexedScope && pushFacetsInStep1 {
		// Pure single-table BM25-indexed query with no serving policy, joins, or
		// heap filters already hits ParadeDB TopKScanExecState directly in one
		// round-trip.
		return nil, false, false, nil
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
	prefixCtx, prefixCancel := context.WithTimeout(gq.ctx, bm25PrefixSearchBudget)
	defer prefixCancel()

	// Preserve the factory's ConnPool while resetting its Statement immediately
	// so deferred serving.Policy scopes never leak into Step 1.
	baseDB := freshDB(gq.factory(prefixCtx, gq.daoQ).UnderlyingDB(), prefixCtx)
	if _, inTx := baseDB.Statement.ConnPool.(gorm.TxCommitter); inTx {
		return nil, false, false, nil
	}

	for ; prefix <= bm25PrefixMaxSize; prefix *= 4 {
		var (
			items      []T
			candidates []bm25Candidate
		)
		txErr := freshDB(baseDB, prefixCtx).Transaction(func(tx *gorm.DB) error {
			if settingErr := tx.Exec("SET LOCAL jit = off").Error; settingErr != nil {
				return settingErr
			}
			cleanTx := freshDB(tx, prefixCtx)
			txQ := dao.Use(cleanTx)

			// Step 1: pure single-table BM25 Top-K candidate scan (no joins, no
			// serving-policy subqueries, no non-indexed heap filters).
			candDB := freshDB(tx, prefixCtx).Table(b.tableName)
			for _, sc := range step1Scopes {
				if scErr := sc(candDB); scErr != nil {
					return scErr
				}
			}
			candDB = candDB.Where(b.tableName+".search_text &&& ?", searchTerms)
			if pushFacetsInStep1 && raw.Query != nil {
				candDB = candDB.Where(raw.Query, raw.Args...)
			}
			if scanErr := candDB.
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
			eligibleSub := gq.factory(prefixCtx, txQ)
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
			if gq.ctx.Err() != nil {
				return nil, false, false, gq.ctx.Err()
			}
			if prefixCtx.Err() == context.DeadlineExceeded {
				return nil, false, true, nil
			}
			return nil, false, false, fmt.Errorf("bm25 candidate prefix: %w", txErr)
		}

		exhausted := len(candidates) < prefix
		if exhausted {
			recordSearchQuery(gq.builder, "prefix")
			return items, true, false, nil
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
					return items, true, false, nil
				}
			}
		}
		if err = gq.ctx.Err(); err != nil {
			return nil, false, false, err
		}
		if prefixCtx.Err() == context.DeadlineExceeded {
			return nil, false, true, nil
		}
	}

	return nil, false, true, nil
}
