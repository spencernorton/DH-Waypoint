// Package serving defines reversible consumer visibility without changing stored data.
package serving

import (
	"fmt"
	"github.com/spencernorton/DH-Waypoint/internal/catalogueguard"
	"regexp"
	"strings"
	"unicode"

	"github.com/spencernorton/DH-Waypoint/internal/keywords"
	"github.com/spencernorton/DH-Waypoint/internal/namepolicy"
	"gorm.io/gorm/clause"
)

const PublicationReviewContract = "required-publication-review-v1"
const PublicationReviewPolicy = "core-publication-policy-v1"
const PayloadEvidenceContract = "piece-bound-media-format-v1"

// Config applies to consumer APIs, not processing or operator recovery reads.
type Config struct {
	ProcessorExclusions                   bool   `yaml:"-"`
	ExcludeAdult                          bool   `yaml:"exclude_adult"`
	RequiredProcessor                     string `yaml:"required_processor"`
	RequiredProcessorArtifactDigest       string `yaml:"required_processor_artifact_digest"`
	RequiredPayloadVerifierArtifactDigest string `yaml:"required_payload_verifier_artifact_digest"`
}

// Policy retains source-complete quarantine exclusion and optionally excludes
// known adult releases. Legacy incomplete snapshots need separate review.
type Policy struct {
	excludeAdult            bool
	processorExclusions     bool
	requiredProcessor       string
	requiredArtifact        string
	requiredPayloadArtifact string
	strongPattern           string
	candidateNeedles        []string
	mediaExtensions         []string
	names                   *namepolicy.Policy
}

// NewPolicy uses only the core classifier's precision-vetted strong keywords.
// Weak keywords and the matcher's abstention heuristic are not serving evidence.
func NewPolicy(cfg Config, strongKeywords, mediaExtensions []string, names ...*namepolicy.Policy) (*Policy, error) {
	if cfg.RequiredProcessor != "" {
		if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,95}$`).MatchString(cfg.RequiredProcessor) ||
			!regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(cfg.RequiredProcessorArtifactDigest) {
			return nil, fmt.Errorf("required publication processor needs a valid ID and pinned artifact digest")
		}
	} else if cfg.RequiredProcessorArtifactDigest != "" {
		return nil, fmt.Errorf("publication artifact digest needs a required processor")
	}
	if cfg.RequiredPayloadVerifierArtifactDigest != "" && (cfg.RequiredProcessor == "" || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(cfg.RequiredPayloadVerifierArtifactDigest)) {
		return nil, fmt.Errorf("payload evidence needs a required processor and pinned verifier artifact digest")
	}
	if len(names) > 1 {
		return nil, fmt.Errorf("serving name policy must be a single instance")
	}
	if len(strongKeywords) == 0 || len(mediaExtensions) == 0 {
		return nil, fmt.Errorf("serving adult evidence is empty")
	}
	r, err := keywords.NewRegexFromKeywords(strongKeywords...)
	if err != nil {
		return nil, fmt.Errorf("serving adult evidence: %w", err)
	}
	// PostgreSQL ARE does not support Go's Unicode property escape. Expand the
	// exact Go letter set instead of locale-dependent [:alpha:], which can widen
	// keyword boundaries on C-locale databases. Go's \d is ASCII, too.
	pattern := strings.ReplaceAll(r.String(), `\p{L}`, postgresLetterRanges())
	pattern = strings.ReplaceAll(pattern, `\d`, `0-9`)
	p := &Policy{processorExclusions: cfg.ProcessorExclusions, excludeAdult: cfg.ExcludeAdult, strongPattern: pattern, mediaExtensions: append([]string(nil), mediaExtensions...)}
	p.requiredProcessor, p.requiredArtifact = cfg.RequiredProcessor, cfg.RequiredProcessorArtifactDigest
	p.requiredPayloadArtifact = cfg.RequiredPayloadVerifierArtifactDigest
	p.candidateNeedles = candidateForPattern(r.String()).needles
	if len(names) == 1 {
		p.names = names[0]
	}
	return p, nil
}

func postgresLetterRanges() string {
	var out strings.Builder
	write := func(lo, hi rune, stride uint32) {
		if stride == 1 {
			out.WriteRune(lo)
			if hi != lo {
				out.WriteByte('-')
				out.WriteRune(hi)
			}
			return
		}
		for r := lo; r <= hi; r += rune(stride) {
			out.WriteRune(r)
		}
	}
	for _, r := range unicode.L.R16 {
		write(rune(r.Lo), rune(r.Hi), uint32(r.Stride))
	}
	for _, r := range unicode.L.R32 {
		write(rune(r.Lo), rune(r.Hi), r.Stride)
	}
	return out.String()
}

// TorrentCondition is applied to the base query before client filtering, counts,
// grouping or pagination. A source-complete quarantine cannot reappear after
// recrawl. Snapshot expiry does not confer or revoke its visibility authority.
func (p *Policy) TorrentCondition(table string) clause.Expr {
	switch table {
	case "torrents", "torrent_contents", "torrent_files", "torrent_tags":
	default:
		panic("invalid serving torrent table")
	}
	root := table + ".info_hash"
	// Keep positive-review joins ahead of the many anti-joins. PostgreSQL's
	// bounded join-order search otherwise starts with the catalogue and can
	// scan it before discovering that no current approval exists.
	sql := "TRUE"
	args := []any{}
	if p.requiredProcessor != "" {
		// A semi-join starts from the small set of explicit approvals; an
		// application, catalogue match or old registry identity cannot grant access.
		sql += ` AND ` + root + ` IN (
 SELECT review.info_hash FROM processor_publication_reviews review
 JOIN processor_publication_sources reviewed_t ON reviewed_t.info_hash=review.info_hash
 JOIN processor_registry reviewer ON reviewer.id=review.processor_id
 WHERE review.processor_id=? AND review.approved
 AND review.policy_contract=?
 AND review.source_revision=reviewed_t.revision
 AND reviewer.mode='apply' AND reviewer.state='active'
 AND reviewer.execution_generation=review.execution_generation
 AND reviewer.manifest->'processor'=review.processor_identity
 AND review.processor_identity->>'artifact_digest'=?
 AND jsonb_exists(reviewer.manifest->'capabilities','policy'))`
		args = append(args, p.requiredProcessor, PublicationReviewPolicy, p.requiredArtifact)
	}
	if p.requiredPayloadArtifact != "" {
		sql += ` AND ` + root + ` IN (
 SELECT payload.info_hash FROM processor_payload_reviews payload
 JOIN processor_publication_sources payload_source ON payload_source.info_hash=payload.info_hash
 WHERE payload.approved AND payload.source_revision=payload_source.revision
 AND payload.verifier_artifact=? AND payload.policy=? AND payload.expires_at>CURRENT_TIMESTAMP)`
		args = append(args, p.requiredPayloadArtifact, PayloadEvidenceContract)
	}
	// Authentic source snapshots were added with the complete recovery producer.
	// Do not silently elevate legacy marker-only records into consumer authority.
	// Keep the qualifying snapshot durable rather than depending on a capture
	// receipt that may have a shorter retention period. Shape and hash bindings
	// reject incomplete/mismatched snapshots without unsafe JSON casts.
	sql += " AND NOT EXISTS (SELECT 1 FROM junkpurge_quarantine serving_q WHERE serving_q.info_hash = " + root + `
 AND jsonb_typeof(serving_q.sources_snapshot) = 'array'
 AND jsonb_typeof(serving_q.torrent_snapshot) = 'object'
 AND serving_q.torrent_snapshot->>'info_hash' = chr(92) || 'x' || encode(serving_q.info_hash, 'hex')
 AND serving_q.torrent_snapshot->>'name' = serving_q.torrent_name
 AND serving_q.torrent_snapshot->'private' = 'false'::jsonb
 AND (serving_q.files_snapshot IS NULL OR jsonb_typeof(serving_q.files_snapshot) = 'array')
 AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements(CASE
  WHEN jsonb_typeof(serving_q.files_snapshot) = 'array' THEN serving_q.files_snapshot
  ELSE '[]'::jsonb END) serving_file
  WHERE jsonb_typeof(serving_file) <> 'object' OR serving_file->>'info_hash'
   IS DISTINCT FROM chr(92) || 'x' || encode(serving_q.info_hash, 'hex')
   OR jsonb_typeof(serving_file->'path') IS DISTINCT FROM 'string')
 AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements(CASE
  WHEN jsonb_typeof(serving_q.sources_snapshot) = 'array' THEN serving_q.sources_snapshot
  ELSE '[]'::jsonb END) serving_source
  WHERE jsonb_typeof(serving_source) <> 'object' OR serving_source->>'info_hash'
   IS DISTINCT FROM chr(92) || 'x' || encode(serving_q.info_hash, 'hex')
   OR jsonb_typeof(serving_source->'source') IS DISTINCT FROM 'string'
   OR COALESCE(serving_source->>'source', '') = ''
   OR jsonb_typeof(serving_source->'source_metadata') IS DISTINCT FROM 'object'
   OR serving_source->'source_metadata'->>'key' IS DISTINCT FROM serving_source->>'source'))`
	// The consumer view cannot advertise a private hash through list, files,
	// tags or native Torznab merely because a separate grab route refuses it.
	// This authority is independent of optional name/adult policy.
	private := table + ".private = FALSE"
	if table != "torrents" {
		private = "EXISTS (SELECT 1 FROM torrents serving_private WHERE serving_private.info_hash = " + root + " AND serving_private.private = FALSE)"
	}
	sql += " AND (" + private + ")" + `
 AND NOT EXISTS (SELECT 1 FROM label_evidence serving_e WHERE ` + catalogueguard.IdentityMatchSQL("serving_e.info_hash", root) + ` AND ` + catalogueguard.QBPrivacySQL("serving_e.source", "serving_e.category", "?") + `)
 AND NOT EXISTS (SELECT 1 FROM torrent_tags serving_tag WHERE serving_tag.info_hash = ` + root + ` AND ` + catalogueguard.PrivacyTagSQL("serving_tag.name", "?") + `)
 AND NOT EXISTS (SELECT 1 FROM torrent_canonical_labels serving_label WHERE ` + catalogueguard.IdentityMatchSQL("serving_label.info_hash", root) + ` AND lower(btrim(serving_label.category,?)) IN ('private','bitgrab'))`
	if p.processorExclusions || p.requiredProcessor != "" {
		sql += " AND NOT EXISTS(SELECT 1 FROM processor_exclusions serving_processor WHERE " + catalogueguard.IdentityMatchSQL("serving_processor.info_hash", root) + ")"
	}
	sql += " AND NOT EXISTS(SELECT 1 FROM processor_publication_holds serving_hold WHERE " + catalogueguard.IdentityMatchSQL("serving_hold.info_hash", root) + " AND serving_hold.held)"
	args = append(args, catalogueguard.TagWhitespace, catalogueguard.TagWhitespace,
		catalogueguard.TagWhitespace, catalogueguard.TagWhitespace, catalogueguard.TagWhitespace,
		catalogueguard.TagWhitespace)
	if p.excludeAdult {
		sql += ` AND NOT EXISTS (
 SELECT 1 FROM torrent_contents serving_tc
 LEFT JOIN content serving_c ON serving_c.type = serving_tc.content_type
  AND serving_c.source = serving_tc.content_source AND serving_c.id = serving_tc.content_id
 WHERE serving_tc.info_hash = ` + root + `
  AND (serving_tc.content_type = 'xxx' OR serving_c.adult IS TRUE))`
		// This independently catches vetted strong native signals even when a
		// stale ordinary-content attachment prevented the classifier from typing XXX.
		// The positive media-byte test mirrors core XXX typing; arbitrary archives
		// and a single weak title token never become adult evidence here.
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(p.mediaExtensions)), ",")
		nameMatch, nameArgs := p.strongMatchSQL(`CASE WHEN serving_t.files_status = 'single' AND serving_t.extension IS NOT NULL
   THEN left(serving_t.name, greatest(char_length(serving_t.name)-char_length(serving_t.extension)-1, 0))
   ELSE serving_t.name END`, "serving_t.name")
		fileMatch, fileArgs := p.strongMatchSQL(`CASE WHEN serving_f.extension IS NOT NULL
     THEN left(serving_f.path, greatest(char_length(serving_f.path)-char_length(serving_f.extension)-1, 0))
     ELSE serving_f.path END`, "serving_f.path")
		sql += ` AND NOT EXISTS (
 SELECT 1 FROM torrents serving_t WHERE serving_t.info_hash = ` + root + `
  AND (CASE WHEN serving_t.files_status = 'single'
   THEN serving_t.size > 0 AND serving_t.extension IN (` + placeholders + `)
   ELSE COALESCE((SELECT sum(CASE WHEN serving_f.extension IN (` + placeholders + `)
    THEN serving_f.size ELSE -serving_f.size END) FROM torrent_files serving_f
    WHERE serving_f.info_hash = serving_t.info_hash), 0) > 0 END)
  AND (` + nameMatch + ` OR EXISTS (SELECT 1 FROM torrent_files serving_f
   WHERE serving_f.info_hash = serving_t.info_hash AND ` + fileMatch + `)))`
		for range 2 {
			for _, extension := range p.mediaExtensions {
				args = append(args, extension)
			}
		}
		args = append(args, nameArgs...)
		args = append(args, fileArgs...)
	}
	if p.names.Enabled() {
		nameColumn, hashColumn := "torrents.name", root
		if table != "torrents" {
			nameColumn, hashColumn = "serving_name.name", "serving_name.info_hash"
		}
		allowed, nameArgs := p.names.AllowsSQL(nameColumn, hashColumn)
		if table != "torrents" {
			allowed = "EXISTS (SELECT 1 FROM torrents serving_name WHERE serving_name.info_hash = " + root + " AND (" + allowed + "))"
		}
		sql += " AND (" + allowed + ")"
		args = append(args, nameArgs...)
	}
	return clause.Expr{SQL: sql, Vars: args}
}

// ExcludesAdult reports the mandatory consumer policy. Query optimizations may
// use it to recognize impossible facet selections; TorrentCondition still
// enforces the complete evidence guard independently of client input.
func (p *Policy) ExcludesAdult() bool { return p.excludeAdult }

// ContentCondition leaves common metadata stored and hides only positive adult metadata.
func (p *Policy) ContentCondition() clause.Expr {
	if !p.excludeAdult {
		return clause.Expr{SQL: "TRUE"}
	}
	return clause.Expr{SQL: "content.type <> 'xxx' AND content.adult IS NOT TRUE"}
}
