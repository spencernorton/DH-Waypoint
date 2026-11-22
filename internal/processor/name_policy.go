package processor

import (
	"context"
	"database/sql/driver"
	"github.com/spencernorton/DH-Waypoint/internal/catalogueguard"
	"github.com/spencernorton/DH-Waypoint/internal/database/dao"
	llmwork "github.com/spencernorton/DH-Waypoint/internal/legacyresults"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/namepolicy"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"gorm.io/gen"
	"gorm.io/gen/field"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"sort"
	"time"
)

const persistLockChunkSize = 1000

type lockedTorrentBatch struct {
	torrents map[protocol.InfoHash]string
	contents map[protocol.InfoHash][]*model.TorrentContent
}

func (b *lockedTorrentBatch) covers(hashes []protocol.InfoHash) bool {
	if b == nil {
		return false
	}
	for _, h := range hashes {
		if _, ok := b.torrents[h]; !ok {
			return false
		}
	}
	return true
}

func sortedHashesFromMap[V any](m map[protocol.InfoHash]V) []protocol.InfoHash {
	hashes := make([]protocol.InfoHash, 0, len(m))
	for h := range m {
		hashes = append(hashes, h)
	}
	sort.Slice(hashes, func(i, j int) bool { return hashes[i].String() < hashes[j].String() })
	return hashes
}

func infoHashValuers(hashes []protocol.InfoHash) []driver.Valuer {
	values := make([]driver.Valuer, len(hashes))
	for i, h := range hashes {
		values[i] = h
	}
	return values
}

type namePolicyCondition struct {
	field.Expr
	condition clause.Expr
}

func (c namePolicyCondition) BeCond() interface{} { return c.condition }
func (c namePolicyCondition) CondError() error    { return nil }

// NamePolicyScope admits existing content rows using the shared declaration and
// any current adult row on their hash before files or metadata are hydrated.
// The table/column expressions are fixed backend identifiers, never caller text.
func NamePolicyScope(p *namepolicy.Policy) func(gen.Dao) gen.Dao {
	return func(db gen.Dao) gen.Dao {
		if !p.Enabled() {
			return db
		}
		condition, args := p.AllowsSQL("name_source.name", "name_source.info_hash")
		return db.Where(namePolicyCondition{Expr: field.EmptyExpr(), condition: clause.Expr{SQL: `EXISTS(SELECT 1 FROM torrents name_source WHERE name_source.info_hash=torrent_contents.info_hash AND (` + condition + `) AND NOT EXISTS(SELECT 1 FROM torrent_contents adult_source WHERE adult_source.info_hash=name_source.info_hash AND adult_source.content_type='xxx'))`, Vars: args}})
	}
}

// WithNameAdmission reuses the processor's current stored name/classification
// and independent privacy reader at direct provider boundaries.
func WithNameAdmission(ctx context.Context, d *dao.Query, p *namepolicy.Policy) context.Context {
	if !p.Enabled() {
		return ctx
	}
	return namepolicy.WithAdmission(ctx, (processor{dao: d, namePolicy: p}).providerNameAdmission)
}

// GuardNameRows protects a selected maintenance batch in its actual write
// transaction. It checks the captured name and ANY current adult row without
// changing original non-NULL claims, protections or provider allowances.
func GuardNameRows(ctx context.Context, tx *dao.Query, p *namepolicy.Policy, rows []*model.TorrentContent) error {
	if !p.Enabled() {
		return nil
	}
	names := map[protocol.InfoHash]string{}
	for _, row := range rows {
		names[row.InfoHash] = row.Torrent.Name
	}
	_, err := (processor{dao: tx, namePolicy: p}).guardNameApplications(ctx, tx, names)
	return err
}

// Resolve minimum authoritative names before loading files, attached metadata
// or running the classifier. Denial is a terminal no-op, never a delete/retry.
func (c processor) admitNames(ctx context.Context, hashes []protocol.InfoHash) ([]protocol.InfoHash, error) {
	values := infoHashValuers(hashes)
	rows, err := c.dao.Torrent.WithContext(ctx).Select(c.dao.Torrent.InfoHash, c.dao.Torrent.InfoHashV2, c.dao.Torrent.Name).Where(c.dao.Torrent.InfoHash.In(values...)).Find()
	if err != nil {
		return nil, err
	}
	byHash := map[protocol.InfoHash]model.Torrent{}
	for _, t := range rows {
		byHash[t.InfoHash] = *t
	}
	kept := make([]protocol.InfoHash, 0, len(hashes))
	for _, h := range hashes {
		if alias := byHash[h].InfoHashV2; alias != nil && c.namePolicy.ExcludesHash(*alias) {
			continue
		}
		d := c.namePolicy.Evaluate(h, byHash[h].Name)
		if !d.Eligible {
			c.namePolicy.Observe("processor", d)
			continue
		}
		kept = append(kept, h)
	}
	if len(kept) == 0 {
		return kept, nil
	}
	values = values[:0]
	for _, h := range kept {
		values = append(values, h)
	}
	contents, err := c.dao.TorrentContent.WithContext(ctx).Select(c.dao.TorrentContent.InfoHash, c.dao.TorrentContent.ContentType).Where(c.dao.TorrentContent.InfoHash.In(values...)).Find()
	if err != nil {
		return nil, err
	}
	adult := map[protocol.InfoHash]bool{}
	for _, tc := range contents {
		adult[tc.InfoHash] = adult[tc.InfoHash] || tc.ContentType.Valid && tc.ContentType.ContentType == model.ContentTypeXxx
	}
	eligible := kept[:0]
	for _, h := range kept {
		if adult[h] {
			d := c.namePolicy.EvaluateClassified(h, byHash[h].Name, "xxx")
			c.namePolicy.Observe("processor", d)
			continue
		}
		eligible = append(eligible, h)
	}
	return eligible, nil
}

// Provider admission uses the current stored release name; a stale caller name
// cannot authorize an existing raw hash. Database errors stop external work.
func (c processor) providerNameAdmission(ctx context.Context, source namepolicy.Source) (namepolicy.Decision, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var current struct {
		Name           string
		Private, Adult bool
	}
	db := c.dao.Torrent.WithContext(ctx).UnderlyingDB()
	r := db.Raw(`SELECT name, private OR EXISTS(SELECT 1 FROM label_evidence e WHERE e.info_hash=t.info_hash AND `+catalogueguard.QBPrivacySQL("e.source", "e.category", "?")+`) AS private, EXISTS(SELECT 1 FROM torrent_contents tc WHERE tc.info_hash=t.info_hash AND tc.content_type='xxx') AS adult FROM torrents t WHERE info_hash=?`, catalogueguard.TagWhitespace, catalogueguard.TagWhitespace, source.InfoHash.Bytes()).Scan(&current)
	if r.Error != nil {
		return namepolicy.Decision{Reason: namepolicy.ReasonMissingName, Version: namepolicy.Version}, r.Error
	}
	if r.RowsAffected != 1 || current.Private {
		return namepolicy.Decision{Reason: namepolicy.ReasonNotServed, Version: namepolicy.Version}, nil
	}
	kind := ""
	if current.Adult {
		kind = "xxx"
	}
	d := c.namePolicy.EvaluateClassified(source.InfoHash, current.Name, kind)
	if d.Eligible && current.Name != source.Name {
		return namepolicy.Decision{Reason: namepolicy.ReasonMissingName, Version: namepolicy.Version}, nil
	}
	return d, nil
}

// Direct classification can complete after a stored name changes. Lock and
// re-read that source before any derived rows/tags are written, leaving the raw
// source intact and the response unapplied when its admission no longer holds.
//
// Rows are locked FOR UPDATE in deterministic ascending info_hash order across
// batched queries (avoiding per-row N+1 round-trips and SHARE->UPDATE lock
// upgrades on torrent_contents). The returned locked snapshot can be reused by
// restoreReleaseAttributes within the same transaction.
func (c processor) guardNameApplications(ctx context.Context, tx *dao.Query, names map[protocol.InfoHash]string) (*lockedTorrentBatch, error) {
	if !c.namePolicy.Enabled() || len(names) == 0 {
		return nil, nil
	}
	hashes := sortedHashesFromMap(names)
	torrentNames := make(map[protocol.InfoHash]string, len(hashes))
	for start := 0; start < len(hashes); start += persistLockChunkSize {
		end := min(start+persistLockChunkSize, len(hashes))
		values := infoHashValuers(hashes[start:end])
		torrents, err := tx.Torrent.WithContext(ctx).
			Select(tx.Torrent.InfoHash, tx.Torrent.Name).
			Where(tx.Torrent.InfoHash.In(values...)).
			Order(tx.Torrent.InfoHash).
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Find()
		if err != nil {
			return nil, err
		}
		for _, t := range torrents {
			torrentNames[t.InfoHash] = t.Name
		}
	}
	for _, h := range hashes {
		name, ok := torrentNames[h]
		if !ok {
			return nil, gorm.ErrRecordNotFound
		}
		d := c.namePolicy.Evaluate(h, name)
		if !d.Eligible {
			c.namePolicy.Observe("model_apply", d)
			return nil, llmwork.ErrHeld
		}
		if name != names[h] {
			return nil, llmwork.ErrHeld
		}
	}

	contentsByHash := make(map[protocol.InfoHash][]*model.TorrentContent, len(hashes))
	for start := 0; start < len(hashes); start += persistLockChunkSize {
		end := min(start+persistLockChunkSize, len(hashes))
		values := infoHashValuers(hashes[start:end])
		contents, err := tx.TorrentContent.WithContext(ctx).
			Select(tx.TorrentContent.InfoHash, tx.TorrentContent.ContentType, tx.TorrentContent.ReleaseAttributes).
			Where(tx.TorrentContent.InfoHash.In(values...)).
			Order(tx.TorrentContent.InfoHash, tx.TorrentContent.ID).
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Find()
		if err != nil {
			return nil, err
		}
		for _, tc := range contents {
			contentsByHash[tc.InfoHash] = append(contentsByHash[tc.InfoHash], tc)
		}
	}
	for _, h := range hashes {
		for _, tc := range contentsByHash[h] {
			if tc.ContentType.Valid && tc.ContentType.ContentType == model.ContentTypeXxx {
				d := c.namePolicy.EvaluateClassified(h, torrentNames[h], "xxx")
				c.namePolicy.Observe("model_apply", d)
				return nil, llmwork.ErrHeld
			}
		}
	}
	return &lockedTorrentBatch{
		torrents: torrentNames,
		contents: contentsByHash,
	}, nil
}
