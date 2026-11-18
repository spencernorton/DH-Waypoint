package processors

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/spencernorton/DH-Waypoint/internal/serving"
	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
)

// Identity evidence protects enrichment, never exempts local policy review.
func planForSource(p Plan, src source) Plan {
	var canonical struct {
		MediaID *string `json:"media_id"`
	}
	_ = json.Unmarshal(src.Canonical, &canonical)
	if canonical.MediaID == nil || *canonical.MediaID == "" {
		return p
	}
	steps := []Step{}
	for _, st := range p.Steps {
		if slices.Contains(st.EffectiveCapabilities(), v1.Policy) {
			st.Stage = v1.Policy
			st.Capabilities = []v1.Stage{v1.Policy}
			steps = append(steps, st)
		}
	}
	p.Steps = steps
	return p
}

// fileEvidenceReason says why a source cannot be approved for lack of file
// evidence, or "". The approval path and Scan's gate both use it, so a
// torrent is sent for review exactly when this rule could let it through.
func fileEvidenceReason(src source) string {
	if src.Size <= 0 {
		return "missing_payload_evidence"
	}
	complete := src.FilesStatus == "single" || src.FilesStatus == "multi" && src.FileCount != nil && *src.FileCount > 0 && int64(len(src.Files)) == *src.FileCount
	if !complete {
		return "incomplete_file_evidence"
	}
	if src.FilesStatus == "multi" {
		remaining := src.Size
		for _, f := range src.Files {
			if f.Size < 0 || f.Size > remaining {
				return "inconsistent_file_evidence"
			}
			remaining -= f.Size
		}
		if remaining != 0 {
			return "inconsistent_file_evidence"
		}
	}
	return ""
}

func publicationPayloadReason(src source, kind string) string {
	if reason := fileEvidenceReason(src); reason != "" {
		return reason
	}
	if kind == "" || kind == "unknown" {
		return "unresolved_content_type"
	}
	if kind != "movie" && kind != "tv_show" && kind != "xxx" && kind != "music" && kind != "audiobook" {
		return ""
	}
	paths := []string{}
	if src.FilesStatus == "single" {
		paths = append(paths, src.Name)
	} else {
		for _, f := range src.Files {
			paths = append(paths, f.Path)
		}
	}
	mediaBytes := int64(0)
	for index, p := range paths {
		for _, r := range p {
			if unicode.IsControl(r) || r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069 {
				return "unsafe_media_path"
			}
		}
		for _, part := range strings.Split(strings.ReplaceAll(p, "\\", "/"), "/") {
			if part == "" || part == "." || part == ".." || strings.Contains(part, ":") {
				return "unsafe_media_path"
			}
		}
		base := p
		if i := strings.LastIndexAny(base, "/\\"); i >= 0 {
			base = base[i+1:]
		}
		base = strings.ToLower(strings.TrimRight(strings.TrimSpace(base), ". "))
		ext := ""
		if i := strings.LastIndexByte(base, '.'); i >= 0 {
			ext = base[i+1:]
		}
		switch ext {
		case "exe", "scr", "com", "pif", "bat", "cmd", "ps1", "vbs", "vbe", "js", "jse", "lnk", "hta", "msi", "msix", "dll", "cpl", "jar", "zip", "zipx", "rar", "7z", "gz", "bz2", "xz", "iso", "dmg":
			return "unsafe_media_payload"
		}
		if format, _ := mediaProbeFormat(base, kind); format != "" {
			if src.FilesStatus == "single" {
				mediaBytes = src.Size
			} else {
				mediaBytes += src.Files[index].Size
			}
		} else if !slices.Contains([]string{"srt", "ass", "ssa", "vtt", "sub", "idx", "nfo", "txt", "sfv", "md", "jpg", "jpeg", "png", "webp"}, ext) {
			return "unsupported_media_sidecar"
		}
	}
	if mediaBytes <= src.Size/2 {
		return "missing_media_payload"
	}
	return ""
}

// Called in the same transaction as validated core application. Shadow,
// partial, failed and denied results never create publication authority.
func (s *Store) recordPublicationReviews(ctx context.Context, tx pgx.Tx, l Lease, src source, kind string, outcomes map[int]string, pipelineOK bool) error {
	hasPolicy := false
	for _, st := range l.Plan.Steps {
		hasPolicy = hasPolicy || st.Entry.Mode == "apply" && slices.Contains(st.EffectiveCapabilities(), v1.Policy)
	}
	if !hasPolicy {
		return nil
	}
	var excluded bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM processor_exclusions WHERE info_hash=$1)`, l.Hash).Scan(&excluded); err != nil {
		return err
	}
	pipelineOK = pipelineOK && !excluded
	for i, receipt := range l.Results {
		st := l.Plan.Steps[i]
		r := receipt.Result
		if st.Entry.Mode != "apply" || !slices.Contains(st.EffectiveCapabilities(), v1.Policy) {
			continue
		}
		reason := publicationPayloadReason(src, kind)
		approved := pipelineOK && r.Outcome == v1.Completed && r.Proposal != nil && r.Proposal.Exclusion == nil && reason == "" && !strings.HasPrefix(outcomes[i], "rejected")
		if approved {
			reason = "approved"
		} else if reason == "" {
			reason = "policy_not_approved"
		}
		identity, err := json.Marshal(r.Processor)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO processor_publication_reviews(info_hash,processor_id,source_revision,execution_generation,processor_identity,approved,reason,job_id,policy_contract)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(info_hash,processor_id) DO UPDATE SET
 source_revision=$3,execution_generation=$4,processor_identity=$5,approved=$6,reason=$7,job_id=$8,policy_contract=$9,evaluated_at=clock_timestamp()`, l.Hash, st.Entry.ID, src.PublicationRevision, st.Entry.ExecutionGeneration, string(identity), approved, reason, l.ID, serving.PublicationReviewPolicy)
		if err != nil {
			return err
		}
		if st.Entry.ID == s.publication.RequiredProcessor && approved {
			if err = s.enqueuePayload(ctx, tx, l.Hash, src.PublicationRevision); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) publicationReviewsCurrent(ctx context.Context, tx pgx.Tx, h []byte, revision int64, p Plan) (bool, error) {
	for _, st := range p.Steps {
		if st.Entry.Mode != "apply" || !slices.Contains(st.EffectiveCapabilities(), v1.Policy) {
			continue
		}
		identity, err := json.Marshal(st.Entry.Manifest.Processor)
		if err != nil {
			return false, err
		}
		var current bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM processor_publication_reviews WHERE info_hash=$1 AND processor_id=$2 AND source_revision=$3 AND execution_generation=$4 AND processor_identity=$5::jsonb AND policy_contract=$6)`, h, st.Entry.ID, revision, st.Entry.ExecutionGeneration, string(identity), serving.PublicationReviewPolicy).Scan(&current)
		if err != nil || !current {
			return false, err
		}
	}
	return true, nil
}

func makePublicationRoom(ctx context.Context, tx pgx.Tx, limit int) (int, error) {
	var removed int
	err := tx.QueryRow(ctx, `WITH doomed AS (
 SELECT job_id FROM processor_jobs WHERE state IN('completed','failed','stale','canceled')
 ORDER BY finished_at NULLS FIRST,created_at LIMIT $1
), boundary AS (
 SELECT max(COALESCE(a.finished_at,a.started_at)) value FROM processor_attempts a JOIN doomed d USING(job_id)
), advance AS (
 UPDATE processor_control SET attempt_history_floor=GREATEST(attempt_history_floor,(SELECT value FROM boundary)) WHERE singleton RETURNING singleton
), removed AS (DELETE FROM processor_jobs WHERE job_id IN(SELECT job_id FROM doomed) RETURNING job_id)
SELECT count(*) FROM removed`, limit).Scan(&removed)
	return removed, err
}
