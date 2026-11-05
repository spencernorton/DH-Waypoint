package processors

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/serving"
)

type payloadLease struct {
	Contract         string              `json:"contract"`
	InfoHash         string              `json:"info_hash"`
	InfoHashV2       *string             `json:"info_hash_v2"`
	SourceRevision   int64               `json:"source_revision"`
	LeaseID          string              `json:"lease_id"`
	VerifierArtifact string              `json:"verifier_artifact"`
	Deadline         time.Time           `json:"deadline"`
	Name             string              `json:"name"`
	Size             int64               `json:"size"`
	FilesStatus      string              `json:"files_status"`
	Files            []sourceFile        `json:"files"`
	ContentType      string              `json:"content_type"`
	ProbeBytes       int64               `json:"probe_bytes"`
	MaxProofBytes    int                 `json:"max_proof_bytes"`
	MaxInfoBytes     int                 `json:"max_info_bytes"`
	Claim            payloadClaimRequest `json:"claim"`
}
type payloadWrite struct {
	Contract         string           `json:"contract"`
	InfoHash         string           `json:"info_hash"`
	SourceRevision   int64            `json:"source_revision"`
	LeaseID          string           `json:"lease_id"`
	VerifierArtifact string           `json:"verifier_artifact"`
	Outcome          string           `json:"outcome"`
	Reason           string           `json:"reason"`
	Evidence         *payloadEvidence `json:"evidence"`
}

func (s *Store) applyPayloadHTTP(e *gin.Engine) {
	g := e.Group("/api/publication/v1", func(c *gin.Context) {
		if s.cfg.PayloadVerifierToken == "" || s.publication.RequiredPayloadVerifierArtifactDigest == "" {
			c.AbortWithStatus(503)
			return
		}
		if subtle.ConstantTimeCompare([]byte(c.GetHeader("Authorization")), []byte("Bearer "+string(s.cfg.PayloadVerifierToken))) != 1 {
			c.AbortWithStatus(401)
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Header("Cache-Control", "no-store")
		c.Next()
	})
	g.POST("/lease", func(c *gin.Context) {
		var request struct {
			Artifact string `json:"artifact"`
		}
		if !publicationBody(c, &request, 1024) {
			return
		}
		if request.Artifact != s.publication.RequiredPayloadVerifierArtifactDigest {
			c.AbortWithStatus(409)
			return
		}
		lease, err := s.leasePayload(c.Request.Context())
		if errors.Is(err, pgx.ErrNoRows) {
			c.Status(204)
			return
		}
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, lease)
	})
	g.POST("/result", func(c *gin.Context) {
		var w payloadWrite
		if !publicationBody(c, &w, 26*1024*1024) {
			return
		}
		if w.Contract != PayloadEvidenceContract || w.VerifierArtifact != s.publication.RequiredPayloadVerifierArtifactDigest {
			c.AbortWithStatus(409)
			return
		}
		if err := s.finishPayload(c.Request.Context(), w); err != nil {
			if errors.Is(err, errPayloadEvidence) {
				c.AbortWithStatusJSON(422, gin.H{"error": "invalid_payload_evidence"})
				return
			}
			apiError(c, err)
			return
		}
		c.Status(204)
	})
}

func (s *Store) leasePayload(ctx context.Context) (payloadLease, error) {
	out := payloadLease{Contract: PayloadEvidenceContract, VerifierArtifact: s.publication.RequiredPayloadVerifierArtifactDigest, ProbeBytes: payloadProbeBytes, MaxProofBytes: payloadProofBytes, MaxInfoBytes: payloadInfoBytes}
	tx, err := s.begin(ctx)
	if err != nil {
		return out, err
	}
	defer rollback(tx)
	var admissions int64
	if err = tx.QueryRow(ctx, `SELECT payload_leases FROM processor_control WHERE singleton FOR UPDATE`).Scan(&admissions); err != nil {
		return out, err
	}
	// Expiry uses an indexed bounded refill. New model approvals enter this
	// outbox in their application transaction; browsing never schedules probes.
	_, err = tx.Exec(ctx, `INSERT INTO processor_payload_pending(info_hash,source_revision,priority)
 SELECT proof.info_hash,proof.source_revision,CASE WHEN proof.approved OR t.created_at>=CURRENT_TIMESTAMP-interval '24 hours' THEN 1 ELSE 0 END
 FROM processor_payload_reviews proof JOIN torrents t ON t.info_hash=proof.info_hash
 JOIN processor_publication_sources source ON source.info_hash=proof.info_hash AND source.revision=proof.source_revision
 WHERE proof.expires_at<=CURRENT_TIMESTAMP AND NOT EXISTS(SELECT 1 FROM processor_payload_pending p WHERE p.info_hash=proof.info_hash)
 ORDER BY proof.expires_at,proof.info_hash LIMIT 32 ON CONFLICT DO NOTHING`)
	if err != nil {
		return out, err
	}
	var hash []byte
	err = tx.QueryRow(ctx, `SELECT review.info_hash,review.source_revision
 FROM processor_payload_pending pending
 JOIN processor_publication_reviews review ON review.info_hash=pending.info_hash AND review.source_revision=pending.source_revision
 JOIN processor_publication_sources source ON source.info_hash=review.info_hash AND source.revision=review.source_revision
 JOIN processor_registry registry ON registry.id=review.processor_id
 LEFT JOIN processor_payload_work work ON work.info_hash=review.info_hash
 WHERE review.processor_id=$1 AND review.approved AND registry.state='active' AND registry.mode='apply'
 AND review.policy_contract=$5
 AND registry.execution_generation=review.execution_generation AND registry.manifest->'processor'=review.processor_identity
 AND review.processor_identity->>'artifact_digest'=$2
 AND NOT EXISTS(SELECT 1 FROM processor_payload_reviews proof WHERE proof.info_hash=review.info_hash AND proof.source_revision=review.source_revision AND proof.verifier_artifact=$3 AND proof.policy=$4 AND proof.approved AND proof.expires_at>CURRENT_TIMESTAMP)
 AND NOT EXISTS(SELECT 1 FROM processor_publication_holds h WHERE h.info_hash=review.info_hash AND h.held)
 AND NOT EXISTS(SELECT 1 FROM processor_exclusions h WHERE h.info_hash=review.info_hash)
 AND (work.info_hash IS NULL OR work.source_revision<>review.source_revision OR work.verifier_artifact<>$3 OR (work.lease_until<CURRENT_TIMESTAMP AND work.available_at<=CURRENT_TIMESTAMP))
 ORDER BY CASE WHEN $6 THEN pending.priority ELSE -pending.priority END DESC,pending.enqueued_at,review.info_hash LIMIT 1`, s.publication.RequiredProcessor, s.publication.RequiredProcessorArtifactDigest, out.VerifierArtifact, PayloadEvidenceContract, serving.PublicationReviewPolicy, admissions%4 != 3).Scan(&hash, &out.SourceRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		if e := tx.Commit(ctx); e != nil {
			return out, e
		}
		return out, err
	}
	if err != nil {
		return out, err
	}
	src, _, err := readSource(ctx, tx, hash, true)
	if err != nil {
		return out, err
	}
	if src.PublicationRevision != out.SourceRevision || !src.admitted() || !src.admittedByNames(s.names, hash) {
		// A changed name policy or protected source cannot permanently block
		// the head of this outbox. A future source/review change enrolls it again.
		if _, err = tx.Exec(ctx, `DELETE FROM processor_payload_pending WHERE info_hash=$1 AND source_revision=$2`, hash, out.SourceRevision); err != nil {
			return out, err
		}
		if err = tx.Commit(ctx); err != nil {
			return out, err
		}
		return out, pgx.ErrNoRows
	}
	var alreadyValid bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM processor_payload_reviews WHERE info_hash=$1 AND source_revision=$2 AND verifier_artifact=$3 AND policy=$4 AND approved AND expires_at>clock_timestamp())`, hash, out.SourceRevision, out.VerifierArtifact, PayloadEvidenceContract).Scan(&alreadyValid); err != nil {
		return out, err
	}
	if alreadyValid {
		if _, err = tx.Exec(ctx, `DELETE FROM processor_payload_pending WHERE info_hash=$1 AND source_revision=$2`, hash, out.SourceRevision); err != nil {
			return out, err
		}
		if err = tx.Commit(ctx); err != nil {
			return out, err
		}
		return out, pgx.ErrNoRows
	}
	var content struct {
		Type string `json:"content_type"`
	}
	if err = json.Unmarshal(src.Contents[0], &content); err != nil {
		return out, err
	}
	var nonce [32]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return out, err
	}
	out.LeaseID = hex.EncodeToString(nonce[:])
	out.InfoHash = hex.EncodeToString(hash)
	out.InfoHashV2 = src.InfoHashV2
	out.Name, out.Size, out.FilesStatus, out.Files, out.ContentType = src.Name, src.Size, src.FilesStatus, src.Files, content.Type
	out.Claim, err = sourceClaim(src)
	if err != nil {
		return out, err
	}
	if src.FilesStatus == "single" {
		out.Files = []sourceFile{{Index: 0, Path: src.Name, Size: src.Size}}
	}
	err = tx.QueryRow(ctx, `INSERT INTO processor_payload_work(info_hash,source_revision,verifier_artifact,lease_id,lease_until,available_at)
 VALUES($1,$2,$3,$4,clock_timestamp()+interval '2 minutes',clock_timestamp()+interval '2 minutes')
 ON CONFLICT(info_hash) DO UPDATE SET source_revision=$2,verifier_artifact=$3,lease_id=$4,lease_until=EXCLUDED.lease_until,available_at=EXCLUDED.available_at,attempts=processor_payload_work.attempts+1,outcome='leased'
 WHERE processor_payload_work.source_revision<>$2 OR processor_payload_work.verifier_artifact<>$3 OR (processor_payload_work.lease_until<clock_timestamp() AND processor_payload_work.available_at<=clock_timestamp())
 RETURNING lease_until`, hash, out.SourceRevision, out.VerifierArtifact, out.LeaseID).Scan(&out.Deadline)
	if err != nil {
		return out, err
	}
	if _, err = tx.Exec(ctx, `UPDATE processor_control SET payload_leases=payload_leases+1 WHERE singleton`); err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

var publicationReason = regexp.MustCompile(`^[a-z][a-z0-9_]{0,127}$`)

func (s *Store) finishPayload(ctx context.Context, w payloadWrite) error {
	hash, err := hex.DecodeString(w.InfoHash)
	if err != nil || (len(hash) != 20 && len(hash) != 32) || !jobIdentity.MatchString(w.LeaseID) || w.SourceRevision < 1 || !publicationReason.MatchString(w.Reason) {
		return ErrControl
	}
	if w.Outcome != "approved" && w.Outcome != "rejected" && w.Outcome != "deferred" {
		return ErrControl
	}
	if (w.Outcome == "approved") != (w.Evidence != nil) {
		return ErrControl
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	// The same source lock/revision seal used by model application prevents a
	// concurrent source write from committing under this proof's revision.
	src, _, err := readSource(ctx, tx, hash, true)
	if err != nil {
		return err
	}
	if src.PublicationRevision != w.SourceRevision || !src.admitted() || !src.admittedByNames(s.names, hash) {
		return ErrStale
	}
	var valid bool
	err = tx.QueryRow(ctx, `SELECT lease_id=$2 AND source_revision=$3 AND verifier_artifact=$4 AND lease_until>clock_timestamp() AND outcome='leased' FROM processor_payload_work WHERE info_hash=$1 FOR UPDATE`, hash, w.LeaseID, w.SourceRevision, w.VerifierArtifact).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return ErrStale
	}
	var receipt any = map[string]string{"outcome": w.Outcome, "reason": w.Reason}
	if w.Evidence != nil {
		var content struct {
			Type string `json:"content_type"`
		}
		if err = json.Unmarshal(src.Contents[0], &content); err != nil {
			return err
		}
		receipt, err = validatePayloadEvidence(hash, src, content.Type, *w.Evidence)
		if err != nil {
			return err
		}
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	delay := "7 days"
	if w.Outcome == "deferred" {
		delay = "5 minutes"
	}
	_, err = tx.Exec(ctx, `INSERT INTO processor_payload_reviews(info_hash,source_revision,verifier_artifact,policy,approved,reason,receipt,expires_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,clock_timestamp()+$8::interval)
 ON CONFLICT(info_hash) DO UPDATE SET source_revision=$2,verifier_artifact=$3,policy=$4,approved=$5,reason=$6,receipt=$7,evaluated_at=clock_timestamp(),expires_at=EXCLUDED.expires_at`, hash, w.SourceRevision, w.VerifierArtifact, PayloadEvidenceContract, w.Outcome == "approved", w.Reason, string(raw), delay)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE processor_payload_work SET outcome=$2,lease_until=clock_timestamp(),available_at=clock_timestamp()+$3::interval WHERE info_hash=$1`, hash, w.Outcome, delay)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM processor_payload_pending WHERE info_hash=$1 AND source_revision=$2`, hash, w.SourceRevision); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) enqueuePayload(ctx context.Context, tx pgx.Tx, hash []byte, revision int64) error {
	if s.publication.RequiredPayloadVerifierArtifactDigest == "" {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO processor_payload_pending(info_hash,source_revision,priority)
 SELECT $1,$2,CASE WHEN EXISTS(SELECT 1 FROM torrents WHERE info_hash=$1 AND created_at>=CURRENT_TIMESTAMP-interval '24 hours') OR EXISTS(SELECT 1 FROM processor_payload_reviews WHERE info_hash=$1 AND approved) THEN 1 ELSE 0 END
 WHERE EXISTS(SELECT 1 FROM processor_publication_reviews r WHERE r.info_hash=$1 AND r.processor_id=$3 AND r.source_revision=$2 AND r.approved AND r.policy_contract=$6)
 AND NOT EXISTS(SELECT 1 FROM processor_payload_reviews p WHERE p.info_hash=$1 AND p.source_revision=$2 AND p.verifier_artifact=$4 AND p.policy=$5 AND p.expires_at>CURRENT_TIMESTAMP)
 ON CONFLICT(info_hash) DO UPDATE SET source_revision=$2,priority=EXCLUDED.priority`, hash, revision, s.publication.RequiredProcessor, s.publication.RequiredPayloadVerifierArtifactDigest, PayloadEvidenceContract, serving.PublicationReviewPolicy)
	return err
}

type publicationHoldWrite struct {
	RequestID        string `json:"request_id"`
	ExpectedRevision int64  `json:"expected_source_revision"`
	Held             bool   `json:"held"`
	Reason           string `json:"reason"`
}

func (s *Store) publicationHash(ctx context.Context, value string) ([]byte, error) {
	h, err := protocol.ParseInfoHash(value)
	if err != nil {
		return nil, ErrControl
	}
	p, err := s.pool.Get()
	if err != nil {
		return nil, err
	}
	var hash []byte
	err = p.QueryRow(ctx, `SELECT info_hash FROM torrents WHERE info_hash=$1 OR info_hash_v2=$1 ORDER BY info_hash LIMIT 1`, h.Bytes()).Scan(&hash)
	return hash, err
}

func (s *Store) applyPublicationAdminHTTP(g *gin.RouterGroup) {
	g.GET("/publication/:hash", func(c *gin.Context) {
		hash, err := s.publicationHash(c.Request.Context(), c.Param("hash"))
		if err != nil {
			apiError(c, err)
			return
		}
		tx, err := s.begin(c.Request.Context())
		if err != nil {
			apiError(c, err)
			return
		}
		defer rollback(tx)
		src, _, err := readSource(c.Request.Context(), tx, hash, false)
		if err != nil {
			apiError(c, err)
			return
		}
		if src.Private || src.PrivateEvidence {
			c.Status(404)
			return
		}
		var status []byte
		err = tx.QueryRow(c.Request.Context(), `SELECT jsonb_build_object('info_hash',$2::text,'source_revision',$3::bigint,
 'hold',(SELECT to_jsonb(h) FROM processor_publication_holds h WHERE h.info_hash=$1),
 'reviews',(SELECT jsonb_agg(to_jsonb(r)) FROM processor_publication_reviews r WHERE r.info_hash=$1),
 'payload',(SELECT to_jsonb(p) FROM processor_payload_reviews p WHERE p.info_hash=$1))`, hash, hex.EncodeToString(hash), src.PublicationRevision).Scan(&status)
		if err != nil {
			apiError(c, err)
			return
		}
		c.Header("Cache-Control", "no-store")
		c.Data(200, "application/json", status)
	})
	g.PUT("/publication/:hash/hold", func(c *gin.Context) {
		var w publicationHoldWrite
		if !publicationBody(c, &w, 2048) {
			return
		}
		if !jobIdentity.MatchString(w.RequestID) || w.ExpectedRevision < 1 || !publicationReason.MatchString(w.Reason) {
			apiError(c, ErrControl)
			return
		}
		hash, err := s.publicationHash(c.Request.Context(), c.Param("hash"))
		if err != nil {
			apiError(c, err)
			return
		}
		result, err := s.setPublicationHold(c.Request.Context(), hash, w)
		if err != nil {
			apiError(c, err)
			return
		}
		c.JSON(200, result)
	})
}

func (s *Store) setPublicationHold(ctx context.Context, hash []byte, w publicationHoldWrite) (map[string]any, error) {
	out := map[string]any{"request_id": w.RequestID, "info_hash": hex.EncodeToString(hash), "held": w.Held}
	tx, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer rollback(tx)
	src, _, err := readSource(ctx, tx, hash, true)
	if err != nil {
		return nil, err
	}
	if src.Private || src.PrivateEvidence {
		return nil, ErrControl
	}
	var previousHash []byte
	var previousRevision, afterRevision int64
	var previousHeld bool
	var previousReason string
	err = tx.QueryRow(ctx, `SELECT info_hash,expected_source_revision,source_revision,held,reason FROM processor_publication_hold_events WHERE request_id=$1`, w.RequestID).Scan(&previousHash, &previousRevision, &afterRevision, &previousHeld, &previousReason)
	if err == nil {
		if hex.EncodeToString(previousHash) != hex.EncodeToString(hash) || previousRevision != w.ExpectedRevision || previousHeld != w.Held || previousReason != w.Reason {
			return nil, ErrConflict
		}
		out["source_revision"] = afterRevision
		return out, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if src.PublicationRevision != w.ExpectedRevision {
		return nil, ErrConflict
	}
	_, err = tx.Exec(ctx, `INSERT INTO processor_publication_holds(info_hash,held,reason,request_id) VALUES($1,$2,$3,$4)
 ON CONFLICT(info_hash) DO UPDATE SET held=$2,reason=$3,request_id=$4,updated_at=clock_timestamp()`, hash, w.Held, w.Reason, w.RequestID)
	if err != nil {
		return nil, err
	}
	err = tx.QueryRow(ctx, `SELECT revision FROM processor_publication_sources WHERE info_hash=$1`, hash).Scan(&afterRevision)
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO processor_publication_hold_events(request_id,info_hash,expected_source_revision,source_revision,held,reason) VALUES($1,$2,$3,$4,$5,$6)`, w.RequestID, hash, w.ExpectedRevision, afterRevision, w.Held, w.Reason)
	if err != nil {
		return nil, err
	}
	out["source_revision"] = afterRevision
	return out, tx.Commit(ctx)
}
