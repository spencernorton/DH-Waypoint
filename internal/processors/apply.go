package processors

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/spencernorton/DH-Waypoint/internal/catalogueguard"
	"github.com/spencernorton/DH-Waypoint/internal/classifier"
	"github.com/spencernorton/DH-Waypoint/internal/classifier/classification"
	"github.com/spencernorton/DH-Waypoint/internal/classifier/contentfilter"
	"github.com/spencernorton/DH-Waypoint/internal/classifier/parsers"
	"github.com/spencernorton/DH-Waypoint/internal/database/fts"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
)

var dbTagRegex = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func normalizeDBTag(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.NewReplacer("_", "-", ".", "-", " ", "-").Replace(s)
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	s = strings.Trim(s, "-")
	if dbTagRegex.MatchString(s) && !catalogueguard.ProtectedTag(s, false) {
		return s
	}
	return ""
}

func effectiveType(l Lease) string {
	kind := l.Input.Deterministic.ContentType
	if kind != "" {
		return kind
	}
	for i, r := range l.Results {
		if i < len(l.Plan.Steps) && l.Plan.Steps[i].Entry.Mode == "apply" && r.Result.Outcome == v1.Completed && r.Result.Proposal != nil && r.Result.Proposal.ContentType != "" {
			return r.Result.Proposal.ContentType
		}
	}
	return ""
}

func effectiveTitleAndYear(l Lease, parsed classification.ContentAttributes) (string, model.Year) {
	title := parsed.BaseTitle.String
	year := parsed.Date.Year
	for i, r := range l.Results {
		if i < len(l.Plan.Steps) && l.Plan.Steps[i].Entry.Mode == "apply" && r.Result.Outcome == v1.Completed && r.Result.Proposal != nil && r.Result.Proposal.Extraction != nil {
			if title == "" && r.Result.Proposal.Extraction.Title != "" {
				title = r.Result.Proposal.Extraction.Title
			}
			if year == 0 && r.Result.Proposal.Extraction.Year > 0 {
				year = model.Year(r.Result.Proposal.Extraction.Year)
			}
		}
	}
	return title, year
}

func (s *Store) parsedInput(l Lease, src source) (classification.ContentAttributes, error) {
	return parsers.ParseVideoContentWithOptions(src.torrent(hex.EncodeToString(l.Hash)), classification.Result{ContentAttributes: classification.ContentAttributes{ContentType: model.NewNullContentType(model.ContentType(effectiveType(l)))}}, parsers.ParseOptions{NoiseV2: s.noiseV2})
}
func (s *Store) candidates(ctx context.Context, tx pgx.Tx, in v1.Input, l Lease) ([]v1.Candidate, error) {
	out := []v1.Candidate{}
	if in.Deterministic.Match != nil {
		return out, nil
	}
	var src source
	if e := json.Unmarshal(l.Source, &src); e != nil {
		return out, e
	}
	parsed, e := s.parsedInput(l, src)
	if e != nil {
		return out, e
	}
	title, _ := effectiveTitleAndYear(l, parsed)
	kind := effectiveType(l)
	multiCapClassifyMatch := false
	if kind == "" && l.Step < len(l.Plan.Steps) {
		caps := l.Plan.Steps[l.Step].EffectiveCapabilities()
		multiCapClassifyMatch = slices.Contains(caps, v1.Classify) && slices.Contains(caps, v1.Match)
	}
	if title == "" || (kind != "movie" && kind != "tv_show" && !multiCapClassifyMatch) {
		return out, nil
	}
	var rows pgx.Rows
	if kind == "movie" || kind == "tv_show" {
		rows, e = tx.Query(ctx, `SELECT c.source,c.id,c.type,c.title,COALESCE(c.release_year,0),
 ARRAY(SELECT value FROM content_attributes a WHERE a.content_type=c.type AND a.content_source=c.source AND a.content_id=c.id AND a.key LIKE 'alt_title:%' ORDER BY a.key LIMIT 16),c.original_title
 FROM content c WHERE c.type=$1 AND c.tsv @@ plainto_tsquery('simple',$2) ORDER BY ts_rank(c.tsv,plainto_tsquery('simple',$2)) DESC,c.source,c.id LIMIT 32`, kind, title)
	} else {
		rows, e = tx.Query(ctx, `SELECT c.source,c.id,c.type,c.title,COALESCE(c.release_year,0),
 ARRAY(SELECT value FROM content_attributes a WHERE a.content_type=c.type AND a.content_source=c.source AND a.content_id=c.id AND a.key LIKE 'alt_title:%' ORDER BY a.key LIMIT 16),c.original_title
 FROM content c WHERE c.type IN('movie','tv_show') AND c.tsv @@ plainto_tsquery('simple',$1) ORDER BY ts_rank(c.tsv,plainto_tsquery('simple',$1)) DESC,c.source,c.id LIMIT 32`, title)
	}
	if e != nil {
		return out, e
	}
	defer rows.Close()
	for rows.Next() {
		var c v1.Candidate
		var original *string
		if e = rows.Scan(&c.Source, &c.ID, &c.ContentType, &c.Title, &c.Year, &c.Aliases, &original); e != nil {
			return out, e
		}
		if original != nil && *original != "" {
			c.Aliases = append(c.Aliases, *original)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// apply is deliberately narrow: existing deterministic facts always win. Typed
// extraction, language and vectors remain attributed proposals, not physical
// facts or eligibility grants. A match can attach only an offered, still-present
// catalogue identity independently confirmed against the source title/year.
func (s *Store) apply(ctx context.Context, tx pgx.Tx, l Lease, src source) error {
	var tc model.TorrentContent
	if e := decodeRow(src.Contents[0], &tc); e != nil {
		return e
	}
	tc.Torrent = src.torrent(hex.EncodeToString(l.Hash))
	tc.InfoHash = tc.Torrent.InfoHash
	originalID := tc.ID
	changed := false
	canEnrich := true
	stepOutcomes := map[int]string{}
	finalExcluded := false
	for i, r := range l.Results {
		if i < len(l.Plan.Steps) && l.Plan.Steps[i].Entry.Mode == "apply" && (r.Result.Outcome == v1.Failed || r.Result.Outcome == v1.Deferred) {
			canEnrich = false
		}
	}

	for i, receipt := range l.Results {
		r, entry := receipt.Result, l.Plan.Steps[i].Entry
		stepOutcomes[i] = string(r.Outcome)
		if entry.Mode != "apply" || r.Proposal == nil {
			continue
		}
		p := r.Proposal
		if r.Outcome == v1.Excluded && p.Exclusion != nil {
			binding, _ := json.Marshal(r.Binding)
			if _, e := tx.Exec(ctx, `INSERT INTO processor_exclusions(info_hash,job_id,processor_id,reason,binding) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, l.Hash, l.ID, entry.ID, p.Exclusion.Reason, string(binding)); e != nil {
				return e
			}
			finalExcluded = true
			stepOutcomes[i] = "applied"
		}
		if !canEnrich || (r.Outcome != v1.Completed && r.Outcome != v1.Excluded) {
			if !canEnrich && r.Outcome == v1.Completed {
				stepOutcomes[i] = "rejected_policy"
			}
			continue
		}
		appliedAny := false
		if p.ContentType != "" && tc.ContentType.Valid && string(tc.ContentType.ContentType) != p.ContentType {
			s.metrics.typeDisagreement.WithLabelValues(string(tc.ContentType.ContentType), p.ContentType).Inc()
		}
		if p.ContentType != "" && !tc.ContentType.Valid && src.HintIsEmpty() {
			// Type output cannot overwrite any core or operator type. The normal
			// parser supplies release details; a plugin cannot invent them.
			if s.forbiddenTypes[p.ContentType] || s.names != nil && !s.names.EvaluateClassified(tc.InfoHash, tc.Torrent.Name, p.ContentType).Eligible {
				stepOutcomes[i] = "rejected_policy"
				continue
			}
			{
				ext := ""
				if src.Extension != nil {
					ext = *src.Extension
				}
				exts := []string{}
				if src.FilesStatus == "single" && ext != "" {
					exts = append(exts, ext)
				}
				for _, f := range src.Files {
					if f.Extension != nil {
						exts = append(exts, *f.Extension)
					}
				}
				langs := []string{}
				for lang := range tc.Languages {
					langs = append(langs, string(lang))
				}
				filterInput := contentfilter.Input{Title: src.Name, ContentType: p.ContentType, PrimaryExtension: ext, AllExtensions: exts, Languages: langs}
				if shouldRejectApplyContentType(s.filter, filterInput, p.ContentType, tc.Torrent) {
					stepOutcomes[i] = "rejected_policy"
					continue
				}
			}
			tc.ContentType = model.NewNullContentType(model.ContentType(p.ContentType))
			changed = true
			appliedAny = true
			enrichOpts := classifier.LocalTypeEnrichmentOptions{
				ParseNoiseV2:          s.noiseV2,
				SingleEpisodeMaxBytes: s.singleEpisodeMax,
				LocalSearchEnabled:    true,
				AltTitleMatch:         s.altTitleMatch,
			}
			baseRes := classification.Result{ContentAttributes: classification.ContentAttributes{ContentType: tc.ContentType}}
			cl, e := classifier.EnrichTypeLocally(ctx, tc.Torrent, baseRes, txLocalSearch{tx: tx}, enrichOpts)
			if e == nil {
				if cl.Content != nil && !tc.ContentID.Valid {
					// Recheck local uniqueness under a short share lock so concurrent
					// catalogue inserts cannot introduce an indistinguishable twin.
					if _, lockErr := tx.Exec(ctx, `LOCK TABLE content,content_attributes IN SHARE MODE`); lockErr != nil {
						return lockErr
					}
					if recheck, recheckErr := classifier.EnrichTypeLocally(ctx, tc.Torrent, baseRes, txLocalSearch{tx: tx}, enrichOpts); recheckErr == nil {
						cl = recheck
					} else {
						cl.Content = nil
					}
				}
				copyEnrichment(&tc, cl)
				for tag := range cl.Tags {
					if norm := normalizeDBTag(tag); norm != "" {
						if _, tagErr := tx.Exec(ctx, `INSERT INTO torrent_tags(info_hash,name,created_at,updated_at) VALUES($1,$2,clock_timestamp(),clock_timestamp()) ON CONFLICT DO NOTHING`, l.Hash, norm); tagErr != nil {
							return tagErr
						}
					}
				}
			}
		}
		if p.Extraction != nil {
			if applyExtraction(&tc, p.Extraction) {
				changed = true
				appliedAny = true
			}
		}
		if len(p.AddTags) > 0 {
			for _, rawTag := range p.AddTags {
				if norm := normalizeDBTag(rawTag); norm != "" {
					if _, e := tx.Exec(ctx, `INSERT INTO torrent_tags(info_hash,name,created_at,updated_at) VALUES($1,$2,clock_timestamp(),clock_timestamp()) ON CONFLICT DO NOTHING`, l.Hash, norm); e != nil {
						return e
					}
					appliedAny = true
				}
			}
		}
		if len(p.RemoveTags) > 0 {
			for _, rawTag := range p.RemoveTags {
				if norm := normalizeDBTag(rawTag); norm != "" {
					if _, e := tx.Exec(ctx, `DELETE FROM torrent_tags WHERE info_hash=$1 AND name=$2`, l.Hash, norm); e != nil {
						return e
					}
					appliedAny = true
				}
			}
		}
		if p.Liveness != nil || len(p.Weights) > 0 || p.Language != nil || p.Embedding != nil {
			appliedAny = true
		}
		if p.Match != nil && !tc.ContentID.Valid && tc.ContentType.Valid {
			// Catalogue changes may introduce another equally exact identity while
			// a job runs. Recheck uniqueness under a short lock; ranking is never
			// authority to choose between indistinguishable catalogue records.
			if _, e := tx.Exec(ctx, `LOCK TABLE content,content_attributes IN SHARE MODE`); e != nil {
				return e
			}
			current, e := s.candidates(ctx, tx, l.Input, l)
			if e != nil {
				return e
			}
			if len(current) == 32 || len(receipt.Candidates) == 32 {
				stepOutcomes[i] = "rejected_ambiguous"
				continue // truncated candidate sets cannot prove uniqueness
			}
			var raw []byte
			var tsv string
			e = tx.QueryRow(ctx, `SELECT to_jsonb(c)-'tsv',c.tsv::text FROM content c WHERE type=$1 AND source=$2 AND id=$3 FOR SHARE`, tc.ContentType.ContentType.String(), p.Match.Source, p.Match.ID).Scan(&raw, &tsv)
			if e == pgx.ErrNoRows {
				stepOutcomes[i] = "rejected_ambiguous"
				continue
			}
			if e != nil {
				return e
			}
			var content model.Content
			if e = decodeRow(raw, &content); e != nil {
				return e
			}
			offered := false
			for _, cand := range receipt.Candidates {
				if cand.MatchRef == *p.Match && cand.Title == content.Title && cand.Year == int(content.ReleaseYear) && cand.ContentType == content.Type.String() {
					offered = true
				}
			}
			if !offered {
				stepOutcomes[i] = "rejected_ambiguous"
				continue
			}
			parsed, e := parsers.ParseVideoContentWithOptions(tc.Torrent, classification.Result{ContentAttributes: classification.ContentAttributes{ContentType: tc.ContentType}}, parsers.ParseOptions{NoiseV2: s.noiseV2})
			effTitle, effYear := effectiveTitleAndYear(l, parsed)
			if e != nil || !classifier.ExactContentTitle(effTitle, content) {
				stepOutcomes[i] = "rejected_ambiguous"
				continue
			}
			if content.Type == model.ContentTypeMovie && (effYear == 0 || effYear != content.ReleaseYear) {
				stepOutcomes[i] = "rejected_ambiguous"
				continue
			}
			if content.Type == model.ContentTypeTvShow && len(tc.Episodes) == 0 {
				stepOutcomes[i] = "rejected_ambiguous"
				continue
			}
			matches := 0
			selectedCurrent := false
			for _, cand := range current {
				c := model.Content{Title: cand.Title, Type: model.ContentType(cand.ContentType), ReleaseYear: model.Year(cand.Year)}
				exact := classifier.ExactContentTitle(effTitle, c)
				for _, alias := range cand.Aliases {
					c.OriginalTitle = model.NewNullString(alias)
					exact = exact || classifier.ExactContentTitle(effTitle, c)
				}
				if exact && (content.Type != model.ContentTypeMovie || effYear == c.ReleaseYear) {
					matches++
					selectedCurrent = selectedCurrent || cand.MatchRef == *p.Match
				}
			}
			if matches != 1 || !selectedCurrent {
				stepOutcomes[i] = "rejected_ambiguous"
				continue
			}
			content.Tsv, e = fts.ParseTsvector(tsv)
			if e != nil {
				return e
			}
			tc.Content = content
			tc.ContentSource = model.NewNullString(p.Match.Source)
			tc.ContentID = model.NewNullString(p.Match.ID)
			changed = true
			appliedAny = true
		}
		if appliedAny {
			stepOutcomes[i] = "applied"
		}
	}
	if changed {
		tc.UpdateTsv()
		episodes, _ := json.Marshal(tc.Episodes)
		var attrs any
		if tc.ReleaseAttributes != nil {
			encoded, _ := json.Marshal(tc.ReleaseAttributes)
			attrs = string(encoded)
		}
		// Update just core-validated fields. No catalogue delete, no privacy mutation,
		// no English-audio claim, no replacement of an existing match or manual hint.
		_, e := tx.Exec(ctx, `UPDATE torrent_contents SET content_type=$2,content_source=$3,content_id=$4,episodes=$5,video_resolution=$6,video_source=$7,video_codec=$8,video_3d=$9,video_modifier=$10,release_group=$11,release_attributes=$12,release_granularity=$13,tsv=$14,updated_at=clock_timestamp() WHERE id=$1`, originalID, tc.ContentType, tc.ContentSource, tc.ContentID, string(episodes), tc.VideoResolution, tc.VideoSource, tc.VideoCodec, tc.Video3D, tc.VideoModifier, tc.ReleaseGroup, attrs, tc.ReleaseGranularity, tc.Tsv.String())
		if e != nil {
			return e
		}
	}
	afterSource, after, e := readSource(ctx, tx, l.Hash, false)
	if e != nil {
		return e
	}
	results, _ := json.Marshal(l.Results)
	_, e = tx.Exec(ctx, `INSERT INTO processor_applications(info_hash,job_id,input_revision,applied_revision,pipeline_revision,results) VALUES($1,$2,$3,$4,$5,$6)
 ON CONFLICT(info_hash) DO UPDATE SET job_id=$2,input_revision=$3,applied_revision=$4,pipeline_revision=$5,results=$6,applied_at=clock_timestamp()`, l.Hash, l.ID, l.InputRevision, digest(after), l.PipelineRevision, string(results))
	if e != nil {
		return e
	}
	finalType := ""
	if tc.ContentType.Valid {
		finalType = tc.ContentType.ContentType.String()
	}
	var finalMatch *v1.MatchRef
	if tc.ContentSource.Valid && tc.ContentID.Valid {
		finalMatch = &v1.MatchRef{Source: tc.ContentSource.String, ID: tc.ContentID.String}
	}
	if e = s.recordPublicationReviews(ctx, tx, l, afterSource, finalType, stepOutcomes, canEnrich && !finalExcluded); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, `DELETE FROM processor_publication_pending WHERE info_hash=$1 AND source_revision<=$2`, l.Hash, afterSource.PublicationRevision); e != nil {
		return e
	}
	return s.recordExecutionFeedback(ctx, tx, l, stepOutcomes, finalType, finalMatch, finalExcluded)
}

// shouldRejectApplyContentType reports whether an optional processor's
// content-type proposal must be rejected. DecideDeterministic.Allow is
// true in shadow mode (Enforce=false) even when Reason is blocked, so the
// Reason is checked directly. A video proposal (movie/tv_show/xxx) is also
// rejected when the torrent's file evidence contradicts video, regardless
// of filter enforcement.
func shouldRejectApplyContentType(filter *contentfilter.Filter, filterInput contentfilter.Input, proposed string, torrent model.Torrent) bool {
	if filter != nil && filter.DecideDeterministic(filterInput).Reason != contentfilter.ReasonNone {
		return true
	}
	if (proposed == "movie" || proposed == "tv_show" || proposed == "xxx") && classifier.HasContradictoryNonVideoFiles(torrent) {
		return true
	}
	// Fallback for torrents whose Files slice is empty: the name extension
	// alone (e.g. ".exe"/".7z") is already covered by
	// HasContradictoryNonVideoFiles via Extension/Name, so no extra check
	// is needed here.
	return false
}

func applyExtraction(tc *model.TorrentContent, ex *v1.Extraction) bool {
	if ex == nil {
		return false
	}
	changed := false
	if !tc.VideoResolution.Valid && ex.VideoResolution != "" {
		if parsed, err := model.ParseVideoResolution(ex.VideoResolution); err == nil {
			tc.VideoResolution = model.NewNullVideoResolution(parsed)
			changed = true
		} else if inferred := model.InferVideoResolution(ex.VideoResolution); inferred.Valid {
			tc.VideoResolution = inferred
			changed = true
		}
	}
	if !tc.VideoSource.Valid && ex.VideoSource != "" {
		if parsed, err := model.ParseVideoSource(ex.VideoSource); err == nil {
			tc.VideoSource = model.NewNullVideoSource(parsed)
			changed = true
		} else if inferred := model.InferVideoSource(ex.VideoSource); inferred.Valid {
			tc.VideoSource = inferred
			changed = true
		}
	}
	if !tc.VideoCodec.Valid && ex.VideoCodec != "" {
		if parsed, err := model.ParseVideoCodec(ex.VideoCodec); err == nil {
			tc.VideoCodec = model.NewNullVideoCodec(parsed)
			changed = true
		} else if inferred := model.InferVideoCodec(ex.VideoCodec); inferred.Valid {
			tc.VideoCodec = inferred
			changed = true
		}
	}
	if !tc.Video3D.Valid && ex.Video3D != "" {
		if parsed, err := model.ParseVideo3D(ex.Video3D); err == nil {
			tc.Video3D = model.NewNullVideo3D(parsed)
			changed = true
		}
	}
	if !tc.VideoModifier.Valid && ex.VideoModifier != "" {
		if parsed, err := model.ParseVideoModifier(ex.VideoModifier); err == nil {
			tc.VideoModifier = model.NewNullVideoModifier(parsed)
			changed = true
		}
	}
	if !tc.ReleaseGroup.Valid && ex.ReleaseGroup != "" {
		tc.ReleaseGroup = model.NewNullString(ex.ReleaseGroup)
		changed = true
	}
	if len(tc.Episodes) == 0 && (ex.Season != nil || len(ex.Episodes) > 0) {
		season := 1
		if ex.Season != nil && *ex.Season > 0 {
			season = *ex.Season
		}
		eps := make(model.Episodes)
		if len(ex.Episodes) == 0 {
			eps.AddSeason(season)
		} else {
			for _, ep := range ex.Episodes {
				if ep > 0 {
					eps.AddEpisode(season, ep)
				}
			}
		}
		if len(eps) > 0 {
			tc.Episodes = eps
			changed = true
		}
	}
	if !tc.ReleaseGranularity.Valid && ex.ReleaseGranularity != "" {
		if parsed, err := model.ParseReleaseGranularity(ex.ReleaseGranularity); err == nil {
			tc.ReleaseGranularity = model.NewNullReleaseGranularity(parsed)
			changed = true
		}
	} else if !tc.ReleaseGranularity.Valid && changed {
		tc.ReleaseGranularity = model.DeriveReleaseGranularity(tc.ContentType, tc.Episodes, tc.Torrent.Name)
	}
	return changed
}

func (s source) HintIsEmpty() bool {
	return len(s.Hint) == 0 || strings.TrimSpace(string(s.Hint)) == "null"
}
func copyEnrichment(tc *model.TorrentContent, cl classification.Result) {
	if !tc.VideoResolution.Valid {
		tc.VideoResolution = cl.VideoResolution
	}
	if !tc.VideoSource.Valid {
		tc.VideoSource = cl.VideoSource
	}
	if !tc.VideoCodec.Valid {
		tc.VideoCodec = cl.VideoCodec
	}
	if !tc.Video3D.Valid {
		tc.Video3D = cl.Video3D
	}
	if !tc.VideoModifier.Valid {
		tc.VideoModifier = cl.VideoModifier
	}
	if !tc.ReleaseGroup.Valid {
		tc.ReleaseGroup = cl.ReleaseGroup
	}
	if len(tc.Episodes) == 0 {
		tc.Episodes = cl.Episodes
	}
	if tc.ReleaseAttributes == nil {
		tc.ReleaseAttributes = cl.ReleaseAttributes
	}
	if !tc.ReleaseGranularity.Valid {
		tc.ReleaseGranularity = model.DeriveReleaseGranularity(tc.ContentType, tc.Episodes, tc.Torrent.Name)
	}
	if cl.Content != nil && !tc.ContentID.Valid && cl.Content.Source != "" && cl.Content.ID != "" {
		tc.Content = *cl.Content
		tc.ContentSource = model.NewNullString(cl.Content.Source)
		tc.ContentID = model.NewNullString(cl.Content.ID)
	}
}

// txLocalSearch executes bounded, read-only local catalogue lookups inside an
// active apply transaction so plugin-typed rows can continue to exact local
// movie/TV identity linking without external metadata calls.
type txLocalSearch struct {
	tx pgx.Tx
}

func (l txLocalSearch) ContentByID(context.Context, model.ContentRef) (model.Content, error) {
	return model.Content{}, classification.ErrUnmatched
}

func (l txLocalSearch) ContentBySearch(context.Context, model.ContentType, string, model.Year) (model.Content, error) {
	return model.Content{}, classification.ErrUnmatched
}

func (l txLocalSearch) ContentCandidatesBySearch(
	ctx context.Context,
	ct model.ContentType,
	baseTitle string,
	year model.Year,
	limit int,
) ([]model.Content, error) {
	if l.tx == nil || strings.TrimSpace(baseTitle) == "" || limit <= 0 {
		return nil, nil
	}
	var (
		rows pgx.Rows
		err  error
	)
	if ct == model.ContentTypeMovie && !year.IsNil() {
		rows, err = l.tx.Query(ctx, `
SELECT c.type, c.source, c.id, c.title, c.original_title, COALESCE(c.release_year, 0), COALESCE(c.tsv::text, ''),
  COALESCE(ARRAY(
    SELECT a.key || E'\x1f' || a.value
    FROM content_attributes a
    WHERE a.content_type = c.type AND a.content_source = c.source AND a.content_id = c.id
      AND a.key LIKE 'alt_title:%'
    ORDER BY a.key
    LIMIT 16
  ), '{}'::text[])
FROM content c
WHERE c.type = $1
  AND c.tsv @@ plainto_tsquery('simple', $2)
  AND c.release_year BETWEEN $3 AND $4
ORDER BY ts_rank(c.tsv, plainto_tsquery('simple', $2)) DESC, c.source, c.id
LIMIT $5`, ct.String(), baseTitle, int(year)-1, int(year)+1, limit)
	} else {
		rows, err = l.tx.Query(ctx, `
SELECT c.type, c.source, c.id, c.title, c.original_title, COALESCE(c.release_year, 0), COALESCE(c.tsv::text, ''),
  COALESCE(ARRAY(
    SELECT a.key || E'\x1f' || a.value
    FROM content_attributes a
    WHERE a.content_type = c.type AND a.content_source = c.source AND a.content_id = c.id
      AND a.key LIKE 'alt_title:%'
    ORDER BY a.key
    LIMIT 16
  ), '{}'::text[])
FROM content c
WHERE c.type = $1
  AND c.tsv @@ plainto_tsquery('simple', $2)
ORDER BY ts_rank(c.tsv, plainto_tsquery('simple', $2)) DESC, c.source, c.id
LIMIT $3`, ct.String(), baseTitle, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.Content, 0, limit)
	for rows.Next() {
		var (
			cType, cSource, cID, title, tsvStr string
			orig                               *string
			relYear                            int
			rawAttrs                           []string
		)
		if err := rows.Scan(&cType, &cSource, &cID, &title, &orig, &relYear, &tsvStr, &rawAttrs); err != nil {
			return nil, err
		}
		mc := model.Content{
			Type:        model.ContentType(cType),
			Source:      cSource,
			ID:          cID,
			Title:       title,
			ReleaseYear: model.Year(relYear),
		}
		if orig != nil && *orig != "" {
			mc.OriginalTitle = model.NewNullString(*orig)
		}
		if tsvStr != "" {
			if parsedTsv, parseErr := fts.ParseTsvector(tsvStr); parseErr == nil {
				mc.Tsv = parsedTsv
			}
		}
		for _, pair := range rawAttrs {
			if k, v, ok := strings.Cut(pair, ""); ok {
				mc.Attributes = append(mc.Attributes, model.ContentAttribute{
					ContentType:   mc.Type,
					ContentSource: mc.Source,
					ContentID:     mc.ID,
					Key:           k,
					Value:         v,
				})
			}
		}
		out = append(out, mc)
	}
	return out, rows.Err()
}
