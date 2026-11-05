package processors

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/spencernorton/DH-Waypoint/internal/namepolicy"
	"github.com/spencernorton/DH-Waypoint/internal/serving"
	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
	"github.com/stretchr/testify/require"
)

func TestNativeSandboxReceiptMatchesBackendEvidenceContract(t *testing.T) {
	file := os.Getenv("DH_WAYPOINT_TEST_PAYLOAD_RECEIPT")
	if file == "" {
		t.Skip("native sandbox receipt not configured")
	}
	raw, err := os.ReadFile(file)
	require.NoError(t, err)
	require.LessOrEqual(t, len(raw), 26*1024*1024)
	var receipt struct {
		Lease    payloadLease    `json:"lease"`
		Evidence payloadEvidence `json:"evidence"`
	}
	require.NoError(t, json.Unmarshal(raw, &receipt))
	h, err := hex.DecodeString(receipt.Lease.InfoHash)
	require.NoError(t, err)
	src := source{Name: receipt.Lease.Name, Size: receipt.Lease.Size, FilesStatus: receipt.Lease.FilesStatus}
	require.Equal(t, "music", receipt.Lease.ContentType, "native fixture must remain fictional audio")
	_, err = validatePayloadEvidence(h, src, receipt.Lease.ContentType, receipt.Evidence)
	require.NoError(t, err, "the actual sandbox report must interoperate with independent core validation")
}

func TestPayloadAdmissionReservesBackfillAndCannotDuplicateConcurrentLeases(t *testing.T) {
	s, p, _ := fixture(t)
	s.cfg.PayloadVerifierToken = namepolicy.SecretToken(strings.Repeat("verifier", 8))
	svc := newService(t, "synthetic-reviewer", []v1.Stage{v1.Policy}, policyResult)
	svc.identity.ArtifactDigest = strings.Repeat("a", 64)
	register(t, s, svc)
	require.NoError(t, s.SetPublicationConfig(serving.Config{RequiredProcessor: svc.identity.ID, RequiredProcessorArtifactDigest: svc.identity.ArtifactDigest, RequiredPayloadVerifierArtifactDigest: strings.Repeat("b", 64)}))
	for i := byte(1); i <= 5; i++ {
		seedTorrent(t, p, i, "movie")
	}
	old := []byte{1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	_, err := p.Exec(testContext, `UPDATE torrents SET created_at=now()-interval '2 days' WHERE info_hash=$1`, old)
	require.NoError(t, err)
	n, err := s.Scan(testContext)
	require.NoError(t, err)
	require.Equal(t, 5, n)
	for i := 0; i < n; i++ {
		run(t, s)
	}
	for i := 0; i < 4; i++ {
		lease, e := s.leasePayload(testContext)
		require.NoError(t, e)
		if i == 3 {
			require.Equal(t, hex.EncodeToString(old), lease.InfoHash, "every fourth admission reserves old backlog progress")
		} else {
			require.NotEqual(t, hex.EncodeToString(old), lease.InfoHash)
		}
	}
	type result struct {
		lease payloadLease
		err   error
	}
	results := make(chan result, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); l, e := s.leasePayload(testContext); results <- result{l, e} }()
	}
	wg.Wait()
	close(results)
	count := 0
	for result := range results {
		if result.err == nil {
			count++
			require.NotEmpty(t, result.lease.LeaseID)
		} else {
			require.True(t, errors.Is(result.err, pgx.ErrNoRows), result.err)
		}
	}
	require.Equal(t, 1, count, "only one worker can hold the remaining release")
	var leases int
	require.NoError(t, p.QueryRow(testContext, `SELECT payload_leases FROM processor_control`).Scan(&leases))
	require.Equal(t, 5, leases)
}

func fictionalWAV() []byte {
	payload := make([]byte, 44+16000)
	copy(payload, "RIFF")
	binary.LittleEndian.PutUint32(payload[4:], uint32(len(payload)-8))
	copy(payload[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(payload[16:], 16)
	binary.LittleEndian.PutUint16(payload[20:], 1)
	binary.LittleEndian.PutUint16(payload[22:], 1)
	binary.LittleEndian.PutUint32(payload[24:], 8000)
	binary.LittleEndian.PutUint32(payload[28:], 8000)
	binary.LittleEndian.PutUint16(payload[32:], 1)
	binary.LittleEndian.PutUint16(payload[34:], 8)
	copy(payload[36:], "data")
	binary.LittleEndian.PutUint32(payload[40:], 16000)
	for i := 44; i < len(payload); i++ {
		payload[i] = 128
	}
	return payload
}

func fictionalPayloadEvidence(t *testing.T, name string, payload []byte) ([]byte, payloadEvidence, source) {
	t.Helper()
	pieceLength := 16384
	pieces := []payloadPiece{}
	hashes := []byte{}
	for offset := 0; offset < len(payload); offset += pieceLength {
		data := bytes.Clone(payload[offset:min(offset+pieceLength, len(payload))])
		h := sha1.Sum(data)
		hashes = append(hashes, h[:]...)
		pieces = append(pieces, payloadPiece{Index: len(pieces), Data: data})
	}
	info, err := bencode.Marshal(map[string]any{"name": name, "length": len(payload), "piece length": pieceLength, "pieces": hashes})
	require.NoError(t, err)
	h := sha1.Sum(info)
	sample := sha256.New()
	_ = binary.Write(sample, binary.BigEndian, uint64(len(payload)))
	_ = binary.Write(sample, binary.BigEndian, uint64(len(payload)))
	sample.Write(payload)
	// This binding test models an authenticated decoder report. Separate native
	// sandbox acceptance must prove that the decoder actually produces it.
	report := payloadFileReport{Index: 0, Format: "wav", MediaType: "audio", DecodedFrames: 1, SampleRate: 8000, SampleDigest: hex.EncodeToString(sample.Sum(nil))}
	return h[:], payloadEvidence{Info: info, Pieces: pieces, Files: []payloadFileReport{report}}, source{Name: name, Size: int64(len(payload)), FilesStatus: "single"}
}

func TestPayloadEvidenceBindsActualBytesAndCompleteSource(t *testing.T) {
	h, evidence, src := fictionalPayloadEvidence(t, "Fictional.Track.wav", fictionalWAV())
	_, err := validatePayloadEvidence(h, src, "music", evidence)
	require.NoError(t, err)
	changed := evidence
	changed.Pieces = append([]payloadPiece(nil), evidence.Pieces...)
	changed.Pieces[0].Data = bytes.Clone(changed.Pieces[0].Data)
	changed.Pieces[0].Data[0] ^= 1
	_, err = validatePayloadEvidence(h, src, "music", changed)
	require.ErrorIs(t, err, errPayloadEvidence)
	changed = evidence
	changed.Files = append([]payloadFileReport(nil), evidence.Files...)
	changed.Files[0].DecodedFrames = 0
	_, err = validatePayloadEvidence(h, src, "music", changed)
	require.ErrorIs(t, err, errPayloadEvidence)
	changed = evidence
	changed.Files = append([]payloadFileReport(nil), evidence.Files...)
	changed.Files[0].SampleDigest = strings.Repeat("a", 64)
	_, err = validatePayloadEvidence(h, src, "music", changed)
	require.ErrorIs(t, err, errPayloadEvidence)
	src.Name = "Changed.wav"
	_, err = validatePayloadEvidence(h, src, "music", evidence)
	require.ErrorIs(t, err, errPayloadEvidence)
	bad := fictionalWAV()
	copy(bad, "MZ")
	h, evidence, src = fictionalPayloadEvidence(t, "Fictional.Track.wav", bad)
	_, err = validatePayloadEvidence(h, src, "music", evidence)
	require.ErrorIs(t, err, errPayloadEvidence, "a hash-valid executable renamed to media is still denied")
}

func TestRequiredPayloadProofAndAuditedHoldThroughRealHTTP(t *testing.T) {
	s, p, _ := fixture(t)
	h, evidence, src := fictionalPayloadEvidence(t, "Fictional.Track.wav", fictionalWAV())
	seed := func() {
		_, err := p.Exec(testContext, `INSERT INTO torrents(info_hash,name,size,private,files_status,created_at,updated_at) VALUES($1,$2,$3,false,'single',now(),now()); INSERT INTO torrent_contents(info_hash,content_type,size,created_at,updated_at) VALUES($1,'music',$3,now(),now())`, h, src.Name, src.Size)
		require.NoError(t, err)
	}
	seed()
	svc := newService(t, "synthetic-reviewer", []v1.Stage{v1.Policy}, policyResult)
	svc.identity.ArtifactDigest = strings.Repeat("a", 64)
	register(t, s, svc)
	_, err := s.Scan(testContext)
	require.NoError(t, err)
	run(t, s)
	s.cfg.AdminToken = namepolicy.SecretToken(strings.Repeat("admin", 12))
	s.cfg.PayloadVerifierToken = namepolicy.SecretToken(strings.Repeat("verifier", 8))
	cfg := serving.Config{RequiredProcessor: svc.identity.ID, RequiredProcessorArtifactDigest: svc.identity.ArtifactDigest, RequiredPayloadVerifierArtifactDigest: strings.Repeat("b", 64)}
	require.NoError(t, s.SetPublicationConfig(cfg))
	// Enabling the required verifier resets bounded catch-up and enrolls an
	// existing current policy approval without redispatching the model.
	_, err = s.Scan(testContext)
	require.NoError(t, err)
	consumer := publicationConsumerForConfig(t, p, cfg)
	visible := func(want int) {
		t.Helper()
		page, e := consumer.TorrentContent(testContext)
		require.NoError(t, e)
		require.Len(t, page.Items, want)
	}
	engine := gin.New()
	require.NoError(t, s.Apply(engine))
	request := func(method, path, token string, value any) *httptest.ResponseRecorder {
		t.Helper()
		raw, e := json.Marshal(value)
		require.NoError(t, e)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, r)
		return w
	}
	lease := func() payloadLease {
		t.Helper()
		w := request("POST", "/api/publication/v1/lease", string(s.cfg.PayloadVerifierToken), map[string]string{"artifact": cfg.RequiredPayloadVerifierArtifactDigest})
		require.Equal(t, 200, w.Code, w.Body.String())
		var l payloadLease
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &l))
		return l
	}
	finish := func(l payloadLease) {
		t.Helper()
		w := payloadWrite{Contract: PayloadEvidenceContract, InfoHash: l.InfoHash, SourceRevision: l.SourceRevision, LeaseID: l.LeaseID, VerifierArtifact: l.VerifierArtifact, Outcome: "approved", Reason: "decoded_media", Evidence: &evidence}
		r := request("POST", "/api/publication/v1/result", string(s.cfg.PayloadVerifierToken), w)
		require.Equal(t, 204, r.Code, r.Body.String())
	}
	visible(0)
	require.Equal(t, 401, request("POST", "/api/publication/v1/lease", string(s.cfg.AdminToken), map[string]string{"artifact": cfg.RequiredPayloadVerifierArtifactDigest}).Code, "verifier purpose does not inherit admin authority")
	first := lease()
	write := payloadWrite{Contract: PayloadEvidenceContract, InfoHash: first.InfoHash, SourceRevision: first.SourceRevision, LeaseID: first.LeaseID, VerifierArtifact: first.VerifierArtifact, Outcome: "approved", Reason: "decoded_media", Evidence: &evidence}
	write.SourceRevision++
	require.Equal(t, 409, request("POST", "/api/publication/v1/result", string(s.cfg.PayloadVerifierToken), write).Code)
	write.SourceRevision = first.SourceRevision
	write.LeaseID = strings.Repeat("e", 64)
	require.Equal(t, 409, request("POST", "/api/publication/v1/result", string(s.cfg.PayloadVerifierToken), write).Code)
	write.LeaseID = first.LeaseID
	_, err = p.Exec(testContext, `UPDATE processor_payload_work SET lease_until=now()-interval '1 second',available_at=now()-interval '1 second'`)
	require.NoError(t, err)
	require.Equal(t, 409, request("POST", "/api/publication/v1/result", string(s.cfg.PayloadVerifierToken), write).Code)
	visible(0)
	first = lease()
	finish(first)
	write.LeaseID = first.LeaseID
	require.Equal(t, 409, request("POST", "/api/publication/v1/result", string(s.cfg.PayloadVerifierToken), write).Code, "completed leases cannot be replayed")
	visible(1)
	_, err = p.Exec(testContext, `UPDATE processor_payload_reviews SET expires_at=now()-interval '1 second';UPDATE processor_payload_work SET available_at=now()-interval '1 second'`)
	require.NoError(t, err)
	visible(0)
	finish(lease())
	visible(1)
	var revision int64
	require.NoError(t, p.QueryRow(testContext, `SELECT revision FROM processor_publication_sources WHERE info_hash=$1`, h).Scan(&revision))
	hold := publicationHoldWrite{RequestID: strings.Repeat("c", 64), ExpectedRevision: revision, Held: true, Reason: "suspect_payload"}
	path := "/api/processors/v1/publication/" + hex.EncodeToString(h) + "/hold"
	require.Equal(t, 401, request("PUT", path, string(s.cfg.PayloadVerifierToken), hold).Code)
	r := request("PUT", path, string(s.cfg.AdminToken), hold)
	require.Equal(t, 200, r.Code, r.Body.String())
	visible(0)
	require.Equal(t, 200, request("PUT", path, string(s.cfg.AdminToken), hold).Code, "exact retries are idempotent")
	var auditCount int
	require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM processor_publication_hold_events`).Scan(&auditCount))
	require.Equal(t, 1, auditCount)
	require.NoError(t, p.QueryRow(testContext, `SELECT revision FROM processor_publication_sources WHERE info_hash=$1`, h).Scan(&revision))
	hold.RequestID = strings.Repeat("d", 64)
	hold.ExpectedRevision = revision
	hold.Held = false
	hold.Reason = "review_requested"
	r = request("PUT", path, string(s.cfg.AdminToken), hold)
	require.Equal(t, 200, r.Code, r.Body.String())
	visible(0)
	_, err = s.Scan(testContext)
	require.NoError(t, err)
	run(t, s)
	visible(0)
	finish(lease())
	visible(1)
	before := revision
	require.NoError(t, p.QueryRow(testContext, `SELECT revision FROM processor_publication_sources WHERE info_hash=$1`, h).Scan(&before))
	_, err = p.Exec(testContext, `DELETE FROM torrents WHERE info_hash=$1`, h)
	require.NoError(t, err)
	seed()
	require.NoError(t, p.QueryRow(testContext, `SELECT revision FROM processor_publication_sources WHERE info_hash=$1`, h).Scan(&revision))
	require.Greater(t, revision, before)
	_, err = s.Scan(testContext)
	require.NoError(t, err)
	run(t, s)
	visible(0)
	finish(lease())
	visible(1)
}
