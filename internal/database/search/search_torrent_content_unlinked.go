package search

import (
	"context"
	"database/sql"
	"strings"
	"time"
	"unicode"

	"github.com/mozillazg/go-unidecode"
	"github.com/spencernorton/DH-Waypoint/internal/classifier/classification"
	"github.com/spencernorton/DH-Waypoint/internal/classifier/parsers"
	"github.com/spencernorton/DH-Waypoint/internal/database/dao"
	"github.com/spencernorton/DH-Waypoint/internal/database/fts"
	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/regex"
	"github.com/spencernorton/DH-Waypoint/internal/titlenorm"
	"gorm.io/gorm"
)

const (
	unlinkedCandidateMinBatch uint = 50
	unlinkedCandidateMaxBatch uint = 250
	unlinkedCandidateMaxScan  uint = 1000
)

// IdentifierWithUnlinkedTorrentContentSearch extends consumer identifier
// searches (such as Torznab ID lookups) with a second tier of unlinked
// torrent_contents rows (content_id IS NULL) whose parsed release title
// matches the resolved canonical content while preserving all caller filters.
// Linked rows always precede unlinked rows across limit/offset pages.
type IdentifierWithUnlinkedTorrentContentSearch interface {
	TorrentContentByIdentifiersWithUnlinked(
		ctx context.Context,
		refs []model.ContentRef,
		limit, offset uint,
		options ...query.Option,
	) (TorrentContentResult, error)
}

func (s search) TorrentContentByIdentifiersWithUnlinked(
	ctx context.Context,
	refs []model.ContentRef,
	limit, offset uint,
	options ...query.Option,
) (result TorrentContentResult, err error) {
	refs = dedupeContentRefs(refs)
	if len(refs) == 0 {
		opts := append(append([]query.Option(nil), options...), query.Limit(limit), query.Offset(offset))
		return s.TorrentContent(ctx, opts...)
	}

	db := s.q.TorrentContent.WithContext(ctx).UnderlyingDB().Session(&gorm.Session{NewDB: true})
	err = db.Transaction(func(tx *gorm.DB) error {
		snapshot := s
		snapshot.q = dao.Use(tx)

		keys, exactKeys, resolveErr := resolveSnapshotIdentifierKeys(ctx, tx, refs)
		if resolveErr != nil {
			return resolveErr
		}
		var linkedCriteria query.Criteria
		if exactKeys && len(keys) > 0 {
			linkedCriteria = torrentContentKeysCriteria(keys)
		} else {
			linkedCriteria = TorrentContentIdentifiersCriteria(refs...)
		}

		linkedOpts := append(
			append([]query.Option(nil), options...),
			query.Where(linkedCriteria),
			query.Limit(limit),
			query.Offset(offset),
		)
		linked, queryErr := snapshot.torrentContent(ctx, true, linkedOpts...)
		if queryErr != nil {
			return queryErr
		}
		result = linked

		if limit == 0 || uint(len(linked.Items)) >= limit || !exactKeys || len(keys) == 0 {
			return nil
		}

		contents, contentErr := snapshot.resolveCanonicalContents(ctx, tx, keys)
		if contentErr != nil {
			return contentErr
		}
		if len(contents) == 0 {
			return nil
		}

		matchers, resolvedTypes, tsq := buildUnlinkedMatchersAndTSQuery(contents)
		if tsq == "" || len(matchers) == 0 || len(resolvedTypes) == 0 {
			return nil
		}

		unlinkedNeeded := int(limit) - len(linked.Items)
		var unlinkedSkip uint
		if offset > 0 && len(linked.Items) == 0 {
			countOpts := append(
				append([]query.Option(nil), options...),
				query.Where(linkedCriteria),
				query.Limit(0),
				query.Offset(0),
				query.WithTotalCount(true),
				query.WithAggregationBudget(0),
			)
			linkedCountRes, countErr := snapshot.torrentContent(ctx, true, countOpts...)
			if countErr != nil {
				return countErr
			}
			if offset > linkedCountRes.TotalCount {
				unlinkedSkip = offset - linkedCountRes.TotalCount
			}
		}

		seenHashes := make(map[protocol.InfoHash]struct{}, len(linked.Items)+unlinkedNeeded)
		for _, item := range linked.Items {
			seenHashes[item.InfoHash] = struct{}{}
		}

		unlinkedBaseOpts := append(
			append([]query.Option(nil), options...),
			query.Where(TorrentContentTypeCriteria(resolvedTypes...)),
			query.Where(query.DBCriteria{
				SQL:  model.TableNameTorrentContent + ".content_id IS NULL AND " + model.TableNameTorrentContent + ".tsv @@ ?::tsquery",
				Args: []interface{}{tsq},
			}),
			query.WithTotalCount(false),
			query.WithHasNextPage(false),
		)

		var matched []TorrentContentResultItem
		var skipped uint
		var sqlOffset uint

		for len(matched) < unlinkedNeeded && sqlOffset < unlinkedCandidateMaxScan {
			remainingScan := unlinkedCandidateMaxScan - sqlOffset
			batchSize := uint((unlinkedNeeded-len(matched))*2) + unlinkedSkip - skipped
			if batchSize < unlinkedCandidateMinBatch {
				batchSize = unlinkedCandidateMinBatch
			}
			if batchSize > unlinkedCandidateMaxBatch {
				batchSize = unlinkedCandidateMaxBatch
			}
			if batchSize > remainingScan {
				batchSize = remainingScan
			}

			pageOpts := append(
				append([]query.Option(nil), unlinkedBaseOpts...),
				query.Limit(batchSize),
				query.Offset(sqlOffset),
			)
			page, pageErr := snapshot.torrentContent(ctx, true, pageOpts...)
			if pageErr != nil {
				return pageErr
			}
			if len(page.Items) == 0 {
				break
			}

			if hydrateErr := hydrateMissingTorrentNames(ctx, tx, page.Items); hydrateErr != nil {
				return hydrateErr
			}

			for _, item := range page.Items {
				if _, exists := seenHashes[item.InfoHash]; exists {
					continue
				}
				if !matchesAnyResolvedContent(item, matchers) {
					continue
				}
				if skipped < unlinkedSkip {
					skipped++
					continue
				}
				seenHashes[item.InfoHash] = struct{}{}
				matched = append(matched, item)
				if len(matched) >= unlinkedNeeded {
					break
				}
			}

			if uint(len(page.Items)) < batchSize {
				break
			}
			sqlOffset += uint(len(page.Items))
		}

		if len(matched) > 0 {
			result.Items = append(result.Items, matched...)
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	return result, err
}

func resolveSnapshotIdentifierKeys(
	requestCtx context.Context,
	tx *gorm.DB,
	refs []model.ContentRef,
) ([]model.ContentRef, bool, error) {
	if len(refs) == 1 && refs[0].Source == model.SourceTmdb {
		return refs, true, nil
	}
	keysSQL, args := torrentContentIdentifierKeyQuery(refs)
	lookupSQL := "SELECT DISTINCT content_type, content_source, content_id FROM (" + keysSQL + ") identifier_keys LIMIT 33"
	lookupCtx, cancel := context.WithTimeout(requestCtx, 5*time.Second)
	defer cancel()
	if err := lookupCtx.Err(); err != nil {
		return nil, false, err
	}
	var rows []resolvedIdentifierKey
	if err := tx.WithContext(lookupCtx).Raw(lookupSQL, args...).Scan(&rows).Error; err != nil {
		return nil, false, err
	}
	if len(rows) > maxResolvedIdentifierKeys {
		return nil, false, nil
	}
	keys := make([]model.ContentRef, 0, len(rows))
	for _, row := range rows {
		keys = append(keys, model.ContentRef{
			Type:   model.ContentType(row.ContentType),
			Source: row.ContentSource,
			ID:     row.ContentID,
		})
	}
	return dedupeContentRefs(keys), true, nil
}

func (s search) resolveCanonicalContents(
	requestCtx context.Context,
	tx *gorm.DB,
	keys []model.ContentRef,
) ([]model.Content, error) {
	lookupCtx, cancel := context.WithTimeout(requestCtx, 5*time.Second)
	defer cancel()
	if err := lookupCtx.Err(); err != nil {
		return nil, err
	}

	arms := make([]string, 0, len(keys))
	args := make([]interface{}, 0, len(keys)*3)
	for _, key := range keys {
		arms = append(arms, "(content.type = ? AND content.source = ? AND content.id = ?)")
		args = append(args, key.Type.String(), key.Source, key.ID)
	}

	q := tx.WithContext(lookupCtx).Table(model.TableNameContent).Where(strings.Join(arms, " OR "), args...)
	if s.adultPolicy != nil {
		q = q.Where(s.adultPolicy.ContentCondition())
	}

	var contents []model.Content
	if err := q.Find(&contents).Error; err != nil {
		return nil, err
	}
	return contents, nil
}

func hydrateMissingTorrentNames(ctx context.Context, tx *gorm.DB, items []TorrentContentResultItem) error {
	var missing [][]byte
	for i := range items {
		if items[i].Torrent.Name == "" {
			missing = append(missing, items[i].InfoHash.Bytes())
		}
	}
	if len(missing) == 0 {
		return nil
	}
	type hashName struct {
		InfoHash protocol.InfoHash `gorm:"column:info_hash"`
		Name     string            `gorm:"column:name"`
	}
	var rows []hashName
	if err := tx.WithContext(ctx).Table(model.TableNameTorrent).
		Select("info_hash, name").
		Where("info_hash IN ?", missing).
		Scan(&rows).Error; err != nil {
		return err
	}
	byHash := make(map[protocol.InfoHash]string, len(rows))
	for _, r := range rows {
		byHash[r.InfoHash] = r.Name
	}
	for i := range items {
		if items[i].Torrent.Name == "" {
			if name, ok := byHash[items[i].InfoHash]; ok {
				items[i].Torrent.InfoHash = items[i].InfoHash
				items[i].Torrent.Name = name
			}
		}
	}
	return nil
}

type resolvedContentMatcher struct {
	contentType  model.ContentType
	releaseYear  model.Year
	strictKeys   map[string]struct{}
	multiWordKey map[string]struct{}
}

func buildUnlinkedMatchersAndTSQuery(contents []model.Content) ([]resolvedContentMatcher, []model.ContentType, string) {
	var matchers []resolvedContentMatcher
	typeSeen := make(map[model.ContentType]struct{}, len(contents))
	var resolvedTypes []model.ContentType
	phraseSeen := make(map[string]struct{})
	var phrases []string

	addTitleVariants := func(raw string, m *resolvedContentMatcher) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		variants := []string{raw}
		collapsed := strings.NewReplacer("'", "", "’", "", "`", "").Replace(raw)
		if collapsed != raw {
			variants = append(variants, collapsed)
		}
		if strings.Contains(raw, "&") {
			variants = append(variants, strings.ReplaceAll(raw, "&", " and "))
		}
		for _, v := range variants {
			if strict := strictTitleKey(v); strict != "" {
				m.strictKeys[strict] = struct{}{}
			}
			if fk := titlenorm.FamilyKey(v); fk != "" && strings.Contains(fk, " ") {
				m.multiWordKey[fk] = struct{}{}
			}
			sanitized := strings.ReplaceAll(v, `"`, " ")
			expr := fts.AppQueryToTsquery(`"` + sanitized + `"`)
			if expr != "" {
				if _, ok := phraseSeen[expr]; !ok {
					phraseSeen[expr] = struct{}{}
					phrases = append(phrases, "("+expr+")")
				}
			}
		}
	}

	for _, c := range contents {
		m := resolvedContentMatcher{
			contentType:  c.Type,
			releaseYear:  c.ReleaseYear,
			strictKeys:   make(map[string]struct{}, 4),
			multiWordKey: make(map[string]struct{}, 4),
		}
		addTitleVariants(c.Title, &m)
		if c.OriginalTitle.Valid && c.OriginalTitle.String != c.Title {
			addTitleVariants(c.OriginalTitle.String, &m)
		}
		if len(m.strictKeys) == 0 {
			continue
		}
		matchers = append(matchers, m)
		if _, ok := typeSeen[c.Type]; !ok {
			typeSeen[c.Type] = struct{}{}
			resolvedTypes = append(resolvedTypes, c.Type)
		}
	}

	if len(phrases) == 0 {
		return nil, nil, ""
	}
	if len(phrases) == 1 {
		return matchers, resolvedTypes, phrases[0]
	}
	return matchers, resolvedTypes, strings.Join(phrases, " | ")
}

// strictTitleKey normalises punctuation, accents, case, '&' -> 'and' and
// apostrophes while preserving leading articles so single-word titles
// ("Americans" vs "The Americans", "Bear" vs "The Bear") stay distinct.
func strictTitleKey(s string) string {
	s = strings.NewReplacer("'", "", "’", "", "`", "").Replace(s)
	s = strings.ReplaceAll(s, "&", " and ")
	if titlenorm.ContainsKana(s) {
		s = titlenorm.KanaToRomaji(s)
	}
	s = regex.NormalizeString(unidecode.Unidecode(s))
	fields := strings.Fields(s)
	out := fields[:0]
	for _, tok := range fields {
		var b strings.Builder
		for _, r := range tok {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				b.WriteRune(unicode.ToLower(r))
			}
		}
		if b.Len() > 0 {
			out = append(out, b.String())
		}
	}
	// Run canonical roman-numeral / Part-X folding without stripping articles by
	// prefixing a synthetic guard token before NormalizeTitleForMatch.
	if len(out) == 0 {
		return ""
	}
	guarded := titlenorm.FamilyKey("zzguard " + strings.Join(out, " "))
	return strings.TrimPrefix(guarded, "zzguard ")
}

func matchesAnyResolvedContent(item TorrentContentResultItem, matchers []resolvedContentMatcher) bool {
	if item.Torrent.Name == "" {
		return false
	}
	baseTitle, parsedYear, parsedEpisodes := extractUnlinkedReleaseTitle(item)
	if baseTitle == "" {
		return false
	}
	collapsed := strings.NewReplacer("'", "", "’", "", "`", "").Replace(baseTitle)
	candStrict := strictTitleKey(baseTitle)
	candStrictCollapsed := strictTitleKey(collapsed)
	candFamily := titlenorm.FamilyKey(collapsed)

	episodes := item.Episodes
	if len(episodes) == 0 {
		episodes = parsedEpisodes
	}

	for _, m := range matchers {
		if item.ContentType.Valid && item.ContentType.ContentType != m.contentType {
			continue
		}
		_, okStrict := m.strictKeys[candStrict]
		if !okStrict && candStrictCollapsed != candStrict {
			_, okStrict = m.strictKeys[candStrictCollapsed]
		}
		_, okMulti := m.multiWordKey[candFamily]
		if !okStrict && !okMulti {
			continue
		}
		if !unlinkedYearCompatible(m.contentType, m.releaseYear, parsedYear, episodes) {
			continue
		}
		return true
	}
	return false
}

func extractUnlinkedReleaseTitle(item TorrentContentResultItem) (string, model.Year, model.Episodes) {
	t := item.Torrent
	if t.InfoHash.IsZero() {
		t.InfoHash = item.InfoHash
	}
	parsed, err := parsers.ParseVideoContentWithOptions(
		t,
		classification.Result{ContentAttributes: classification.ContentAttributes{ContentType: item.ContentType}},
		parsers.ParseOptions{NoiseV2: true},
	)
	if err == nil && parsed.BaseTitle.Valid && strings.TrimSpace(parsed.BaseTitle.String) != "" {
		return parsed.BaseTitle.String, parsed.Date.Year, parsed.Episodes
	}
	title, year, episodes, _, fallbackErr := parsers.ParseTitleYearEpisodes(item.ContentType, t.Name)
	if fallbackErr == nil && strings.TrimSpace(title) != "" {
		return title, year, episodes
	}
	return "", 0, nil
}

func unlinkedYearCompatible(
	contentType model.ContentType,
	canonicalYear model.Year,
	parsedYear model.Year,
	episodes model.Episodes,
) bool {
	if canonicalYear.IsNil() || parsedYear.IsNil() {
		return true
	}
	diff := int(parsedYear) - int(canonicalYear)
	switch contentType {
	case model.ContentTypeMovie:
		return diff >= -1 && diff <= 1
	case model.ContentTypeTvShow:
		if diff < -1 {
			return false
		}
		if isOnlySeasonOne(episodes) {
			return diff >= -1 && diff <= 1
		}
		return true
	default:
		return true
	}
}

func isOnlySeasonOne(episodes model.Episodes) bool {
	if len(episodes) != 1 {
		return false
	}
	_, ok := episodes[1]
	return ok
}
