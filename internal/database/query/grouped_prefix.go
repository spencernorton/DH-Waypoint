package query

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/database/dao"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const groupedPrefixSearchBudget = 750 * time.Millisecond

type prefixCandidateQuery struct {
	SubQuery
	candidates SubQuery
}

// WithCandidatePrefixSource supplies a cheaper eligibility superset for the
// speculative ordered prefix. Every eligible row must exist there unchanged.
// Winners and returned records still come from the authoritative eligible query;
// a short prefix always expands or falls back to the complete grouped query.
func WithCandidatePrefixSource(eligible, candidates SubQuery) SubQuery {
	return prefixCandidateQuery{SubQuery: eligible, candidates: candidates}
}

// A selective full-text predicate can make the ordered prefix less efficient
// than grouping the complete matching set. Bound only that optional attempt;
// keep its speculative work below an interactive page's latency budget. The
// authoritative grouped query retains the caller's original context.
func (gq *genericQuery[T]) groupedPrefixItemsWithSearchBudget() ([]T, bool, error) {
	b, ok := gq.builder.(optionBuilder)
	if !ok || b.tsquery == "" {
		return gq.groupedPrefixItems()
	}
	ctx, cancel := context.WithTimeout(gq.ctx, groupedPrefixSearchBudget)
	prefix := genericQuery[T]{ctx: ctx, daoQ: gq.daoQ, factory: gq.factory, builder: gq.builder}
	items, complete, err := prefix.groupedPrefixItems()
	expired := ctx.Err() == context.DeadlineExceeded
	cancel()
	if err != nil && expired && gq.ctx.Err() == nil {
		return nil, false, nil
	}
	return items, complete, err
}

// groupedPrefixItems tests an ordered candidate superset rather
// than sorting every release before returning a small grouped page. A candidate
// is returned only when it is the exact highest-seeder/hash-ascending eligible
// representative of its group. Winners outside the prefix are never returned.
// A complete requested page is exact: every unseen representative is itself an
// unseen candidate, after this prefix. A short result requires either proof that
// the entire candidate superset was exhausted in the same snapshot, expansion,
// or the original whole-corpus query to establish the final short page.
func (gq *genericQuery[T]) groupedPrefixItems() ([]T, bool, error) {
	b, ok := gq.builder.(optionBuilder)
	if !ok || b.grouping == nil || !b.grouping.candidatePrefix || b.tableName != model.TableNameTorrentContent || !b.limit.Valid ||
		b.limit.Uint == 0 || b.limit.Uint > 1000 || b.offset > 10000 || len(b.orderBy) == 0 || len(b.groupBy) > 0 {
		return nil, false, nil
	}
	keyNames := []string{"content_type", "content_source", "content_id"}
	qualifiedKeys := make([]string, len(keyNames))
	for i, key := range keyNames {
		qualifiedKeys[i] = b.tableName + "." + key
	}
	keySQL := strings.Join(qualifiedKeys, ", ")
	if b.grouping.distinctOnSQL != keySQL || b.grouping.innerOrderSQL != keySQL+
		", COALESCE("+b.tableName+".seeders, -1) DESC, "+b.tableName+".info_hash" {
		return nil, false, nil
	}
	// Joins with multiple rows per release could duplicate candidates. The
	// existing flat/grouped paths remain authoritative for those query shapes.
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
	flat := b
	flat.grouping = nil
	flat.selections = []clause.Expr{{SQL: b.tableName + ".*"}}
	sq := gq.factory(gq.ctx, gq.daoQ)
	var candidateSource SubQuery
	if hinted, ok := sq.(prefixCandidateQuery); ok {
		candidateSource = hinted.candidates
	}
	if err = flat.applySelect(sq.UnderlyingDB(), true); err != nil {
		return nil, false, err
	}
	if err = flat.applyPre(sq, true); err != nil {
		return nil, false, err
	}
	// A raw scope/factory join has no cardinality declaration. Only the
	// resolved, declared joins above may participate in the prefix path.
	// GORM defers scopes (including declared gen joins) until rendering.
	// Inspect that completed dry-run statement, never the pre-scope builder.
	var rendered *gorm.DB
	eligibleSQL := sq.UnderlyingDB().ToSQL(func(tx *gorm.DB) *gorm.DB {
		rendered = tx.Find(&[]interface{}{})
		return rendered
	})
	if rendered.Error != nil {
		return nil, false, rendered.Error
	}
	statement := rendered.Statement
	// Only own transactions may change query-local planner settings. Inspect
	// the resolved factory/scopes so a caller transaction retains its snapshot,
	// uncommitted rows and settings, including prepared-statement wrappers.
	if _, inTransaction := statement.ConnPool.(gorm.TxCommitter); inTransaction {
		return nil, false, nil
	}
	from, _ := statement.Clauses["FROM"].Expression.(clause.From)
	if len(statement.Joins) != 0 || len(from.Joins) != len(joins) {
		return nil, false, nil
	}
	candidateTable, candidateCTE, candidateSQL, winnerSourceSQL := "eligible", "", "", ""
	selectiveCandidates := false
	if candidateSource != nil {
		winnerSourceSQL = candidateSource.UnderlyingDB().ToSQL(func(tx *gorm.DB) *gorm.DB { return tx.Find(&[]interface{}{}) })
		if err = flat.applySelect(candidateSource.UnderlyingDB(), true); err != nil {
			return nil, false, err
		}
		if err = flat.applyPre(candidateSource, true); err != nil {
			return nil, false, err
		}
		var candidateRendered *gorm.DB
		candidateSQL = candidateSource.UnderlyingDB().ToSQL(func(tx *gorm.DB) *gorm.DB {
			candidateRendered = tx.Find(&[]interface{}{})
			return candidateRendered
		})
		if candidateRendered.Error != nil {
			return nil, false, candidateRendered.Error
		}
		cs := candidateRendered.Statement
		cf, _ := cs.Clauses["FROM"].Expression.(clause.From)
		if _, inTransaction := cs.ConnPool.(gorm.TxCommitter); inTransaction {
			return nil, false, nil
		}
		if cs.Table != statement.Table || !reflect.DeepEqual(cs.TableExpr, statement.TableExpr) ||
			!reflect.DeepEqual(cf.Tables, from.Tables) || len(cs.Joins) != 0 || len(cf.Joins) != len(joins) {
			return nil, false, nil
		}
		candidateTable = "candidate_source"
		candidateCTE = ", winner_source AS NOT MATERIALIZED (" + winnerSourceSQL + "), candidate_source AS NOT MATERIALIZED (" + candidateSQL + ")"
	}
	winnerOrder := "COALESCE(winner.seeders,-1) DESC,winner.info_hash"
	// Separate NULL branches let each winner lookup use the ordered covering
	// group index, while retaining SQL NULL grouping semantics for every key.
	branches := make([]string, 0, 2)
	fastBranches := make([]string, 0, 2)
	for _, sourceNull := range []bool{false, true} {
		conditions := []string{"winner.content_type=keys.content_type", "winner.content_id=keys.content_id"}
		if sourceNull {
			conditions = append(conditions, "keys.content_source IS NULL", "winner.content_source IS NULL")
		} else {
			conditions = append(conditions, "keys.content_source IS NOT NULL", "winner.content_source=keys.content_source")
		}
		where := strings.Join(conditions, " AND ")
		fallbackOnly := ""
		if winnerSourceSQL != "" {
			// This ordered root is only an eligibility superset. The dependent
			// point lookup returns the fully guarded record, including all client
			// filters. A found winner is exact because every earlier root row was
			// tested. If these32 candidates contain no eligible row, keep the
			// original whole-group lookup as the authoritative fallback.
			fastBranches = append(fastBranches, "(SELECT visible.* FROM (SELECT winner.*, "+
				"COALESCE(winner.seeders,-1) AS _winner_seeders FROM winner_source winner WHERE "+where+
				" ORDER BY "+winnerOrder+" LIMIT 32) trial JOIN LATERAL "+
				"(SELECT checked.* FROM eligible checked WHERE checked.id=trial.id OFFSET 0) visible ON TRUE "+
				"ORDER BY trial._winner_seeders DESC,trial.info_hash LIMIT 1)")
			fallbackOnly = " AND NOT EXISTS (SELECT 1 FROM fast)"
		}
		branches = append(branches, "(SELECT winner.* FROM eligible winner WHERE "+where+
			fallbackOnly+" ORDER BY "+winnerOrder+" LIMIT 1)")
	}
	winnerSQL := strings.Join(branches, " UNION ALL ")
	if len(fastBranches) > 0 {
		winnerSQL = "WITH fast AS MATERIALIZED (" + strings.Join(fastBranches, " UNION ALL ") +
			") SELECT * FROM fast UNION ALL " + winnerSQL
	}
	fresh := func() *gorm.DB {
		return gq.factory(gq.ctx, gq.daoQ).UnderlyingDB().Session(&gorm.Session{NewDB: true})
	}
	if candidateSQL != "" && b.tsquery != "" {
		// For a selective text query, letting ORDER/LIMIT pull through this
		// source can choose a long walk of the seeder index instead of the GIN
		// match set. A bounded, cheap superset probe distinguishes that case
		// from common terms, which should retain their ordered-index strategy.
		// This is a cost hint only: no result/exhaustion decision uses the count.
		var matches int64
		executionDB := fresh()
		if _, inTransaction := executionDB.Statement.ConnPool.(gorm.TxCommitter); inTransaction {
			return nil, false, nil
		}
		probeCtx, cancelProbe := context.WithTimeout(gq.ctx, 150*time.Millisecond)
		probeErr := executionDB.WithContext(probeCtx).Transaction(func(tx *gorm.DB) error {
			if settingErr := tx.Exec("SET LOCAL jit = off").Error; settingErr != nil {
				return settingErr
			}
			return tx.Raw("SELECT count(*) FROM (" + candidateSQL + " LIMIT 32769) bounded_matches").Scan(&matches).Error
		}, &sql.TxOptions{ReadOnly: true})
		probeExpired := probeCtx.Err() == context.DeadlineExceeded
		cancelProbe()
		if probeErr != nil && (!probeExpired || gq.ctx.Err() != nil) {
			return nil, false, fmt.Errorf("grouped candidate match probe: %w", probeErr)
		}
		// A common or slow probe leaves the ordered-index strategy intact and
		// shares the original speculative budget with the subsequent prefix.
		if probeErr == nil && matches <= 32768 {
			selectiveCandidates = true
			candidateCTE = ", winner_source AS NOT MATERIALIZED (" + winnerSourceSQL + "), candidate_source AS MATERIALIZED (" + candidateSQL + ")"
		}
	}
	outer := fresh().Table("selected")
	if err = b.applyPost(outer); err != nil {
		return nil, false, err
	}
	outerSQL := dao.ToSQL(outer)
	wanted := int(b.limit.Uint)
	if b.nextPage {
		wanted++
	}
	prefix := 512
	if b.tsquery != "" && candidateSQL != "" {
		prefix = 64
		for prefix < 2*(int(b.offset)+wanted) {
			prefix *= 2
		}
	}
	if selectiveCandidates {
		// A small complete match set is cheaper to test once than repeatedly
		// revisiting the same expensive title representatives during expansion.
		prefix = 32768
	}
	if b.orderBy[0].Column.Table == model.TableNameTorrent && b.orderBy[0].Column.Name == "name" {
		prefix = 4096
		if b.offset > 0 {
			prefix = 8192
		}
	}
	for ; prefix <= 32768; prefix *= 2 {
		candidateBuilder := b
		candidateBuilder.offset = 0
		candidateBuilder.limit = model.NewNullUint(uint(prefix))
		candidateBuilder.nextPage = false
		candidateBuilder.preloads = nil
		candidates := fresh().Table(candidateTable)
		if err = candidateBuilder.applyPost(candidates); err != nil {
			return nil, false, err
		}
		keys := strings.Join(keyNames, ",")
		candidateSQLPrefix := "WITH eligible AS NOT MATERIALIZED (" + eligibleSQL +
			")" + candidateCTE + ", candidates AS MATERIALIZED (" + dao.ToSQL(candidates) + ")"
		prefixSQL := candidateSQLPrefix + ", keys AS MATERIALIZED (SELECT DISTINCT " + keys + " FROM candidates), " +
			"winners AS MATERIALIZED (SELECT representative.* FROM keys JOIN LATERAL (" +
			winnerSQL + ") representative ON TRUE), " +
			"selected AS (SELECT winners.* FROM candidates candidate JOIN winners ON winners.id=candidate.id) " + outerSQL
		var items []T
		var candidateCount int64
		executionDB := fresh()
		if _, inTransaction := executionDB.Statement.ConnPool.(gorm.TxCommitter); inTransaction {
			return nil, false, nil
		}
		// These bounded pages can cross PostgreSQL's JIT cost threshold even
		// though execution is short. Compiling hundreds of expressions costs
		// more than the page itself. SET LOCAL ends with this owned read-only
		// transaction; ordinary queries, fallback and pooled settings retain
		// their existing behavior on success, failure or cancellation.
		if err = executionDB.Transaction(func(tx *gorm.DB) error {
			if settingErr := tx.Exec("SET LOCAL jit = off").Error; settingErr != nil {
				return settingErr
			}
			if countErr := tx.Raw(candidateSQLPrefix + " SELECT count(*) FROM candidates").Scan(&candidateCount).Error; countErr != nil {
				return countErr
			}
			return tx.Raw(prefixSQL).Scan(&items).Error
		}, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead}); err != nil {
			return nil, false, fmt.Errorf("grouped candidate prefix: %w", err)
		}
		// The count and page use this owned repeatable-read snapshot. When
		// the raw prefix is short, every eligible representative must already
		// be among these candidates, so the final short/empty page is exact.
		if len(items) >= wanted || candidateCount < int64(prefix) {
			return items, true, nil
		}
		if err = gq.ctx.Err(); err != nil {
			return nil, false, err
		}
	}
	return nil, false, nil
}
