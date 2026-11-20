package query

import (
	"database/sql"
	"fmt"

	"github.com/spencernorton/DH-Waypoint/internal/database/dao"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	payloadPrefixMinSize = 128
	payloadPrefixMaxSize = 8192
)

type payloadCandidate struct {
	ID    string  `gorm:"column:_pg_cand_id"`
	Score float64 `gorm:"column:_order_0"`
}

type payloadCandidateIDOnly struct {
	ID string `gorm:"column:_pg_cand_id"`
}

// payloadGatePrefixItems accelerates payload-gated ungrouped torrent_contents
// browse, RSS sync, and text queries without weakening any serving authority
// predicate.
//
// Inside one read-only REPEATABLE READ transaction:
//  1. Select the top-K ordered candidate torrent_contents.id rows matching the
//     selective positive payload-evidence gate plus all client filters/facets
//     and joins, without running per-row multi-file bundle regexes or
//     quarantine/exclusion anti-joins over thousands of discarded rows.
//  2. Evaluate the complete authoritative eligible query (full serving.Policy
//     verbatim) strictly on those bounded candidate IDs.
//
// Because Step 1 is a strict superset of Step 2 ordered by the same
// deterministic ORDER BY, returning early once Step 1 is exhausted or Step 2
// yields offset+wanted survivors (with strict rank separation when ordered by
// query_string_rank) produces results identical to full direct execution.
func (gq *genericQuery[T]) payloadGatePrefixItems() ([]T, bool, error) {
	b, ok := gq.builder.(optionBuilder)
	if !ok || b.payloadGatePrefix == nil {
		return nil, false, nil
	}
	if b.grouping != nil || b.tableName != model.TableNameTorrentContent ||
		!b.limit.Valid || b.limit.Uint == 0 || b.limit.Uint > 1000 || b.offset > 10000 ||
		len(b.groupBy) > 0 || len(b.orderBy) == 0 {
		return nil, false, nil
	}

	rankOrder := b.orderBy[0].Column.Name == QueryStringRankField
	if rankOrder {
		if !b.orderBy[0].Desc {
			return nil, false, nil
		}
	} else {
		lastCol := b.orderBy[len(b.orderBy)-1].Column.Name
		if lastCol != "id" && lastCol != "info_hash" {
			return nil, false, nil
		}
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

	// Refuse if the caller already owns an outer transaction or added undeclared joins.
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

	wanted := int(b.limit.Uint)
	if b.nextPage {
		wanted++
	}
	needed := int(b.offset) + wanted
	prefix := payloadPrefixMinSize
	if minNeeded := needed + 32; minNeeded > prefix {
		prefix = minNeeded
	}
	if prefix > payloadPrefixMaxSize {
		prefix = payloadPrefixMaxSize
	}

	baseDB := freshDB(gq.factory(gq.ctx, gq.daoQ).UnderlyingDB(), gq.ctx)
	if _, inTx := baseDB.Statement.ConnPool.(gorm.TxCommitter); inTx {
		return nil, false, nil
	}

	gate := *b.payloadGatePrefix
	for ; prefix <= payloadPrefixMaxSize; prefix *= 4 {
		var (
			candidates []payloadCandidate
			survivors  []T
		)
		txErr := freshDB(baseDB, gq.ctx).Transaction(func(tx *gorm.DB) error {
			if settingErr := tx.Exec("SET LOCAL jit = off").Error; settingErr != nil {
				return settingErr
			}
			cleanTx := freshDB(tx, gq.ctx)
			txQ := dao.Use(cleanTx)

			// Step 1: positive payload gate + client filters/order only.
			candSub := GenericSubQuery[dao.ITorrentContentDo]{
				SubQuery: txQ.TorrentContent.WithContext(gq.ctx).ReadDB(),
			}
			candSub.UnderlyingDB().Where(gate.SQL, gate.Vars...)

			step1Builder := b
			step1Builder.dbContext = dbContext{q: txQ, tableName: b.tableName, factory: gq.factory}
			step1Builder.selections = []clause.Expr{{SQL: b.tableName + ".id AS _pg_cand_id"}}
			step1Builder.limit = model.NewNullUint(uint(prefix))
			step1Builder.offset = 0
			step1Builder.nextPage = false

			if selErr := step1Builder.applySelect(candSub.UnderlyingDB(), true); selErr != nil {
				return selErr
			}
			if preErr := step1Builder.applyPre(candSub, true); preErr != nil {
				return preErr
			}
			if postErr := step1Builder.applyPost(candSub.UnderlyingDB()); postErr != nil {
				return postErr
			}
			if rankOrder {
				if scanErr := candSub.UnderlyingDB().Scan(&candidates).Error; scanErr != nil {
					return scanErr
				}
			} else {
				var idRows []payloadCandidateIDOnly
				if scanErr := candSub.UnderlyingDB().Scan(&idRows).Error; scanErr != nil {
					return scanErr
				}
				candidates = make([]payloadCandidate, len(idRows))
				for i, r := range idRows {
					candidates[i].ID = r.ID
				}
			}
			if len(candidates) == 0 {
				return nil
			}

			candIDs := make([]string, len(candidates))
			for i, c := range candidates {
				candIDs[i] = c.ID
			}

			// Step 2: complete authoritative serving.Policy on ONLY the bounded candidate IDs.
			eligibleSub := gq.factory(gq.ctx, txQ)
			eligibleSub.UnderlyingDB().Where(b.tableName+".id IN ?", candIDs)

			step2Builder := b
			step2Builder.dbContext = dbContext{q: txQ, tableName: b.tableName, factory: gq.factory}
			step2Builder.limit = model.NewNullUint(uint(len(candIDs)))
			step2Builder.offset = 0
			step2Builder.nextPage = false

			if selErr := step2Builder.applySelect(eligibleSub.UnderlyingDB(), true); selErr != nil {
				return selErr
			}
			if preErr := step2Builder.applyPre(eligibleSub, true); preErr != nil {
				return preErr
			}
			if postErr := step2Builder.applyPost(eligibleSub.UnderlyingDB()); postErr != nil {
				return postErr
			}
			return eligibleSub.UnderlyingDB().Find(&survivors).Error
		}, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
		if txErr != nil {
			if gq.ctx.Err() != nil {
				return nil, false, gq.ctx.Err()
			}
			return nil, false, fmt.Errorf("payload gate candidate prefix: %w", txErr)
		}

		exhausted := len(candidates) < prefix
		canAccept := exhausted
		if !canAccept && len(survivors) >= needed {
			if !rankOrder {
				canAccept = true
			} else {
				boundaryScore := candidates[len(candidates)-1].Score
				if lastID, hasID := extractResultID(survivors[needed-1]); hasID {
					scoreByID := make(map[string]float64, len(candidates))
					for _, c := range candidates {
						scoreByID[c.ID] = c.Score
					}
					if lastScore, ok := scoreByID[lastID]; ok && lastScore > boundaryScore {
						canAccept = true
					}
				}
			}
		}

		if canAccept {
			recordSearchQuery(gq.builder, "payload_prefix")
			if int(b.offset) >= len(survivors) {
				return nil, true, nil
			}
			page := survivors[b.offset:]
			if len(page) > wanted {
				page = page[:wanted]
			}
			return page, true, nil
		}
		if err = gq.ctx.Err(); err != nil {
			return nil, false, err
		}
	}

	return nil, false, nil
}
