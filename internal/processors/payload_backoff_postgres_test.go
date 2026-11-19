package processors

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/spencernorton/DH-Waypoint/internal/namepolicy"
	"github.com/spencernorton/DH-Waypoint/internal/serving"
	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
	"github.com/stretchr/testify/require"
)

func TestPayloadDeferredBackoffEscalatesAndResetsOnNewVerifier(t *testing.T) {
	s, p, _ := fixture(t)
	s.cfg.PayloadVerifierToken = namepolicy.SecretToken(strings.Repeat("verifier", 8))

	svc := newService(t, "synthetic-reviewer", []v1.Stage{v1.Policy}, policyResult)
	svc.identity.ArtifactDigest = strings.Repeat("a", 64)
	register(t, s, svc)

	vA := strings.Repeat("b", 64)
	vB := strings.Repeat("c", 64)
	require.NoError(t, s.SetPublicationConfig(serving.Config{
		RequiredProcessor:                     svc.identity.ID,
		RequiredProcessorArtifactDigest:       svc.identity.ArtifactDigest,
		RequiredPayloadVerifierArtifactDigest: vB,
		PreviousPayloadVerifierArtifactDigest: vA,
	}))

	h := seedTorrent(t, p, 42, "movie")
	_, err := s.Scan(testContext)
	require.NoError(t, err)
	run(t, s)

	engine := gin.New()
	require.NoError(t, s.Apply(engine))
	post := func(path string, body any) *httptest.ResponseRecorder {
		raw, mErr := json.Marshal(body)
		require.NoError(t, mErr)
		req := httptest.NewRequest("POST", path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+string(s.cfg.PayloadVerifierToken))
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		return w
	}

	leaseAndDefer := func(artifact, reason string) (attempts int, delayMins float64) {
		lw := post("/api/publication/v1/lease", map[string]string{
			"artifact": artifact,
			"contract": PayloadEvidenceContractV2,
		})
		require.Equal(t, http.StatusOK, lw.Code, lw.Body.String())
		var l payloadLease
		require.NoError(t, json.Unmarshal(lw.Body.Bytes(), &l))
		require.Equal(t, hex.EncodeToString(h), l.InfoHash)

		rw := post("/api/publication/v1/result", payloadWrite{
			Contract:         PayloadEvidenceContractV2,
			InfoHash:         l.InfoHash,
			SourceRevision:   l.SourceRevision,
			LeaseID:          l.LeaseID,
			VerifierArtifact: artifact,
			Outcome:          "deferred",
			Reason:           reason,
		})
		require.Equal(t, http.StatusNoContent, rw.Code, rw.Body.String())

		require.NoError(t, p.QueryRow(testContext, `SELECT attempts, EXTRACT(EPOCH FROM (available_at - clock_timestamp()))/60.0 FROM processor_payload_work WHERE info_hash=$1`, h).Scan(&attempts, &delayMins))
		return attempts, delayMins
	}

	requeueNow := func() {
		_, e := p.Exec(testContext, `UPDATE processor_payload_work SET available_at=now()-interval '1 second', lease_until=now()-interval '1 second' WHERE info_hash=$1;
 UPDATE processor_payload_reviews SET expires_at=now()-interval '1 second' WHERE info_hash=$1;
 INSERT INTO processor_payload_pending(info_hash,source_revision,priority) SELECT info_hash,source_revision,1 FROM processor_payload_work WHERE info_hash=$1 ON CONFLICT DO NOTHING`, h)
		require.NoError(t, e)
	}

	att, mins := leaseAndDefer(vA, "verified_pieces_unavailable")
	require.Equal(t, 1, att)
	require.InDelta(t, 5.0, mins, 0.5)

	requeueNow()
	att, mins = leaseAndDefer(vA, "verified_pieces_unavailable")
	require.Equal(t, 2, att)
	require.InDelta(t, 15.0, mins, 0.5)

	requeueNow()
	att, mins = leaseAndDefer(vA, "verified_pieces_unavailable")
	require.Equal(t, 3, att)
	require.InDelta(t, 60.0, mins, 1.0)

	// Local contention stays at 5 minutes even on higher attempt counts.
	requeueNow()
	att, mins = leaseAndDefer(vA, "proof_capacity")
	require.Equal(t, 4, att)
	require.InDelta(t, 5.0, mins, 0.5)

	// Switching to a new verifier artifact resets attempts to 1 and applies claim_not_available's 1h floor.
	requeueNow()
	att, mins = leaseAndDefer(vB, "claim_not_available")
	require.Equal(t, 1, att)
	require.InDelta(t, 60.0, mins, 1.0)
}
