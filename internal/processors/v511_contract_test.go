package processors

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/gin-gonic/gin"
	"github.com/spencernorton/DH-Waypoint/internal/namepolicy"
	"github.com/spencernorton/DH-Waypoint/internal/serving"
	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
	"github.com/stretchr/testify/require"
)

func fictionalMKV(size int) []byte {
	payload := make([]byte, max(size, 64))
	copy(payload, []byte{0x1A, 0x45, 0xDF, 0xA3, 0x9F, 0x42, 0x86, 0x81, 0x01, 0x42, 0xF7, 0x81, 0x01, 0x42, 0xF2, 0x81, 0x04, 0x42, 0xF3, 0x81, 0x08, 0x42, 0x82, 0x88})
	copy(payload[24:], "matroska")
	for i := 32; i < len(payload); i++ {
		payload[i] = byte((i * 31) & 0xff)
	}
	return payload
}

func probeSpanPieces(offset, size, pieceLength int64) []int {
	first := min(payloadProbeBytes, size)
	tail := max(first, size-payloadProbeBytes)
	seen := map[int]struct{}{}
	var out []int
	for _, span := range [][2]int64{{offset, first}, {offset + tail, size - tail}} {
		if span[1] > 0 {
			sIdx := int(span[0] / pieceLength)
			eIdx := int((span[0] + span[1] - 1) / pieceLength)
			for idx := sIdx; idx <= eIdx; idx++ {
				if _, ok := seen[idx]; !ok {
					seen[idx] = struct{}{}
					out = append(out, idx)
				}
			}
		}
	}
	return out
}

func TestSelectPayloadProbeFilesStratifiedSeasonPackAndV1Guard(t *testing.T) {
	infoHashHex := strings.Repeat("2a", 20)
	pieceLen := int64(1024 * 1024)
	files := make([]payloadProbeFile, 0, 10)
	var offset int64
	for ep := 1; ep <= 10; ep++ {
		sz := int64(350 * 1024 * 1024)
		files = append(files, payloadProbeFile{
			Index:  ep - 1,
			Path:   fmt.Sprintf("Synthetic.Show.S01E%02d.1080p.WEB-DL.mkv", ep),
			Size:   sz,
			Offset: offset,
		})
		offset += sz
	}

	_, _, ok := SelectPayloadProbeFiles(infoHashHex, files, pieceLen, offset, PayloadEvidenceContract)
	require.False(t, ok, "v1 rejects 10-episode 1 MiB-piece pack exceeding 16 MiB total boundary pieces")

	selected1, budget1, ok := SelectPayloadProbeFiles(infoHashHex, files, pieceLen, offset, PayloadEvidenceContractV2)
	require.True(t, ok)
	require.Equal(t, payloadProofBytes, budget1)
	require.GreaterOrEqual(t, len(selected1), 3, "largest + deterministic hash-seeded episodes within 16 MiB")
	selected2, _, ok := SelectPayloadProbeFiles(infoHashHex, files, pieceLen, offset, PayloadEvidenceContractV2)
	require.True(t, ok)
	require.Equal(t, selected1, selected2, "selection must be deterministic for core/collector parity")

	// Disallowed executable sidecar is still rejected by publicationPayloadReason.
	count := int64(2)
	hostileSrc := source{
		Name:        "Synthetic.Show.S01",
		Size:        400*1024*1024 + 8192,
		FilesStatus: "multi",
		FileCount:   &count,
		Files: []sourceFile{
			{Index: 0, Path: "S01E01.mkv", Size: 400 * 1024 * 1024},
			{Index: 1, Path: "Extras/codec.exe", Size: 8192},
		},
	}
	require.Equal(t, "unsafe_media_payload", publicationPayloadReason(hostileSrc, "tv_show"))

	// Single 4K movie with 16 MiB pieces (32 MiB head+tail) fits v2's 32 MiB single-file budget.
	singleSz := int64(40 * 1024 * 1024 * 1024)
	bigSingle := []payloadProbeFile{{Index: 0, Path: "Synthetic.Film.2026.2160p.Remux.mkv", Size: singleSz, Offset: 0}}
	_, _, ok = SelectPayloadProbeFiles(infoHashHex, bigSingle, 16*1024*1024, singleSz, PayloadEvidenceContract)
	require.False(t, ok)
	singleSel, budgetSingle, ok := SelectPayloadProbeFiles(infoHashHex, bigSingle, 16*1024*1024, singleSz, PayloadEvidenceContractV2)
	require.True(t, ok)
	require.Equal(t, payloadSingleFileProofBytes, budgetSingle)
	require.Len(t, singleSel, 1)
}

func TestPlanForSourcePreservesRequiredProcessorOnCanonicalTorrents(t *testing.T) {
	basePlan := Plan{
		Steps: []Step{
			{
				Entry:        Entry{ID: "armiger-ultra", Mode: "apply"},
				Stage:        v1.Classify,
				Capabilities: []v1.Stage{v1.Classify, v1.Extract, v1.Match, v1.Policy},
			},
			{
				Entry:        Entry{ID: "auxiliary-policy", Mode: "apply"},
				Stage:        v1.Classify,
				Capabilities: []v1.Stage{v1.Classify, v1.Policy},
			},
			{
				Entry:        Entry{ID: "auxiliary-classifier", Mode: "apply"},
				Stage:        v1.Classify,
				Capabilities: []v1.Stage{v1.Classify},
			},
		},
	}
	src := source{
		Name:      "Synthetic.Movie.2026.1080p.WEB-DL.mkv",
		Size:      1024 * 1024,
		Canonical: json.RawMessage(`{"media_type":"movie","media_id":"tmdb:12345","category":"radarr"}`),
	}
	withoutReq := planForSource(basePlan, src)
	require.Len(t, withoutReq.Steps, 2)
	require.Equal(t, []v1.Stage{v1.Policy}, withoutReq.Steps[0].Capabilities)
	require.Equal(t, []v1.Stage{v1.Policy}, withoutReq.Steps[1].Capabilities)

	withReq := planForSource(basePlan, src, "armiger-ultra")
	require.Len(t, withReq.Steps, 2)
	require.Equal(t, []v1.Stage{v1.Classify, v1.Extract, v1.Match, v1.Policy}, withReq.Steps[0].Capabilities, "required publication reviewer keeps full stage capabilities on canonical rows")
	require.Equal(t, []v1.Stage{v1.Policy}, withReq.Steps[1].Capabilities, "other policy processors still narrow to policy")
}

func TestStrictEndpointFallbackAutoExpiresAndCountsMetric(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var j v1.Job
		require.NoError(t, json.NewDecoder(r.Body).Decode(&j))
		w.Header().Set("Content-Type", "application/json")
		if len(j.Capabilities) > 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"bad_request"}`))
			return
		}
		prop := &v1.Proposal{ContentType: "movie"}
		if j.Stage == v1.Policy {
			prop = &v1.Proposal{AddTags: []string{"clean"}}
		}
		_ = json.NewEncoder(w).Encode(v1.Result{
			Binding:  j.Binding,
			Outcome:  v1.Completed,
			Proposal: prop,
			Reasons:  []string{"clean"},
			Evidence: []v1.Evidence{},
		})
	}))
	defer srv.Close()

	tr := NewTransport(2 * time.Second)
	tr.strictTTL = 25 * time.Millisecond
	tr.onStrictFallback = func(string) { calls++ }

	job := v1.Job{
		Binding: v1.Binding{
			Contract:            v1.Contract,
			JobID:               strings.Repeat("1", 64),
			InputRevision:       strings.Repeat("2", 64),
			PipelineRevision:    strings.Repeat("3", 64),
			ConfigurationDigest: strings.Repeat("4", 64),
			Processor:           v1.Identity{ID: "strict-proc", Version: "1.0.0", ModelVersion: "none", PolicyVersion: "1", ArtifactDigest: "none"},
			Stage:               v1.Classify,
		},
		Capabilities:  []v1.Stage{v1.Classify, v1.Policy},
		Configuration: json.RawMessage(`{}`),
		Input: v1.Input{
			InfoHash:      strings.Repeat("1", 40),
			Name:          "synthetic",
			Files:         []v1.File{},
			Candidates:    []v1.Candidate{},
			PriorResults:  []v1.Result{},
			Deterministic: v1.Deterministic{MatchingOutcome: "not_attempted", ProcessingOutcome: "completed"},
		},
		Deadline: time.Now().Add(5 * time.Second).UTC().Format(time.RFC3339Nano),
	}
	res, err := tr.Process(testContext, srv.URL, "", job)
	require.NoError(t, err)
	require.Equal(t, v1.Completed, res.Outcome)
	require.Equal(t, 1, calls)

	// Cached strict fallback avoids probing multi-capability request before TTL.
	_, err = tr.Process(testContext, srv.URL, "", job)
	require.NoError(t, err)
	require.Equal(t, 1, calls)

	// After TTL expiry, transport re-probes v1 multi-cap and increments fallback metric again if still strict.
	time.Sleep(35 * time.Millisecond)
	_, err = tr.Process(testContext, srv.URL, "", job)
	require.NoError(t, err)
	require.Equal(t, 2, calls)
}

func TestPayloadProofV2MultiFileRolloverFeedbackReadOnlyAndExclusionRetract(t *testing.T) {
	s, p, _ := fixture(t)
	s.cfg.AdminToken = namepolicy.SecretToken(strings.Repeat("admin", 12))
	s.cfg.PayloadVerifierToken = namepolicy.SecretToken(strings.Repeat("verifier", 8))
	s.cfg.PolicyMaxRetryWindow = 10 * time.Minute

	svc := newService(t, "synthetic-reviewer", []v1.Stage{v1.Classify, v1.Policy}, policyResult)
	svc.identity.ArtifactDigest = strings.Repeat("a", 64)
	register(t, s, svc)

	prevVerifier := strings.Repeat("b", 64)
	nextVerifier := strings.Repeat("d", 64)
	cfg := serving.Config{
		RequiredProcessor:                     svc.identity.ID,
		RequiredProcessorArtifactDigest:       svc.identity.ArtifactDigest,
		RequiredPayloadVerifierArtifactDigest: nextVerifier,
		PreviousPayloadVerifierArtifactDigest: prevVerifier,
	}
	require.NoError(t, s.SetPublicationConfig(cfg))

	// Multi-file season pack (4 MKV files + 1 SRT) with 16 KiB pieces.
	pieceLength := 16384
	rawFiles := []struct {
		name string
		data []byte
		ext  string
	}{
		{"S01E01.mkv", fictionalMKV(20000), "mkv"},
		{"S01E02.mkv", fictionalMKV(22000), "mkv"},
		{"S01E03.mkv", fictionalMKV(18000), "mkv"},
		{"S01E04.mkv", fictionalMKV(24000), "mkv"},
		{"S01E01.srt", []byte("1\n00:00:01,000 --> 00:00:02,000\nSynthetic subtitle\n"), "srt"},
	}
	var concat []byte
	bencodeFiles := make([]map[string]any, 0, len(rawFiles))
	mediaCandidates := make([]payloadProbeFile, 0, 4)
	var curOffset int64
	for idx, rf := range rawFiles {
		bencodeFiles = append(bencodeFiles, map[string]any{"length": len(rf.data), "path": []string{rf.name}})
		if rf.ext == "mkv" {
			mediaCandidates = append(mediaCandidates, payloadProbeFile{Index: idx, Path: rf.name, Size: int64(len(rf.data)), Offset: curOffset})
		}
		concat = append(concat, rf.data...)
		curOffset += int64(len(rf.data))
	}
	var pieceHashes []byte
	allPieces := []payloadPiece{}
	for off := 0; off < len(concat); off += pieceLength {
		chunk := bytes.Clone(concat[off:min(off+pieceLength, len(concat))])
		sum := sha1.Sum(chunk)
		pieceHashes = append(pieceHashes, sum[:]...)
		allPieces = append(allPieces, payloadPiece{Index: len(allPieces), Data: chunk})
	}
	packName := "Synthetic.Show.S01.1080p.WEB-DL"
	infoBytes, err := bencode.Marshal(map[string]any{
		"name":         packName,
		"piece length": pieceLength,
		"pieces":       pieceHashes,
		"files":        bencodeFiles,
	})
	require.NoError(t, err)
	hArr := sha1.Sum(infoBytes)
	h := hArr[:]

	_, err = p.Exec(testContext, `INSERT INTO content(type,source,id,title,release_year,tsv,created_at,updated_at) VALUES('tv_show','tmdb','999','Synthetic Show',2020,to_tsvector('simple','Synthetic Show'),now(),now());
 INSERT INTO torrents(info_hash,name,size,private,files_status,files_count,created_at,updated_at) VALUES($1,$2,$3,false,'multi',$4,now(),now());
 INSERT INTO torrent_contents(info_hash,content_type,content_source,content_id,episodes,size,created_at,updated_at) VALUES($1,'tv_show','tmdb','999','{"1":{}}'::jsonb,$3,now(),now())`, h, packName, int64(len(concat)), len(rawFiles))
	require.NoError(t, err)
	for idx, rf := range rawFiles {
		_, err = p.Exec(testContext, `INSERT INTO torrent_files(info_hash,"index",path,size,created_at,updated_at) VALUES($1,$2,$3,$4,now(),now())`, h, idx, rf.name, int64(len(rf.data)))
		require.NoError(t, err)
	}
	// Canonical Sonarr label with media_id verifies requiredProcessor keeps full stage list and approves.
	_, err = p.Exec(testContext, `INSERT INTO torrent_canonical_labels(info_hash,media_type,media_id,category,resolved_source,resolved_strength,resolved_at) VALUES($1,'tv_show','tmdb:999','sonarr','sonarr',10,now())`, h)
	require.NoError(t, err)

	_, err = s.Scan(testContext)
	require.NoError(t, err)
	run(t, s)

	var approved bool
	require.NoError(t, p.QueryRow(testContext, `SELECT approved FROM processor_publication_reviews WHERE info_hash=$1 AND processor_id=$2`, h, svc.identity.ID).Scan(&approved))
	require.True(t, approved, "canonical-labelled torrent must receive required publication review approval")

	selectedFiles, _, ok := SelectPayloadProbeFiles(hex.EncodeToString(h), mediaCandidates, int64(pieceLength), int64(len(concat)), PayloadEvidenceContractV2)
	require.True(t, ok)
	reports := make([]payloadFileReport, 0, len(selectedFiles))
	neededPieces := map[int]payloadPiece{}
	for _, sf := range selectedFiles {
		fdata := rawFiles[sf.Index].data
		sample := sha256.New()
		_ = binary.Write(sample, binary.BigEndian, uint64(len(fdata)))
		_ = binary.Write(sample, binary.BigEndian, uint64(len(fdata)))
		sample.Write(fdata)
		reports = append(reports, payloadFileReport{
			Index:         sf.Index,
			Format:        "matroska",
			MediaType:     "video",
			DecodedFrames: 2,
			Width:         1920,
			Height:        1080,
			SampleDigest:  hex.EncodeToString(sample.Sum(nil)),
		})
		for _, pi := range probeSpanPieces(sf.Offset, sf.Size, int64(pieceLength)) {
			neededPieces[pi] = allPieces[pi]
		}
	}
	pieceSlice := make([]payloadPiece, 0, len(neededPieces))
	for i := 0; i < len(allPieces); i++ {
		if pc, exists := neededPieces[i]; exists {
			pieceSlice = append(pieceSlice, pc)
		}
	}
	evidence := payloadEvidence{Info: infoBytes, Pieces: pieceSlice, Files: reports}

	engine := gin.New()
	require.NoError(t, s.Apply(engine))
	doReq := func(method, path, token string, body any) *httptest.ResponseRecorder {
		var rdr *bytes.Reader
		if body != nil {
			raw, mErr := json.Marshal(body)
			require.NoError(t, mErr)
			rdr = bytes.NewReader(raw)
		} else {
			rdr = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(method, path, rdr)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		return w
	}

	lw := doReq("POST", "/api/publication/v1/lease", string(s.cfg.PayloadVerifierToken), map[string]string{
		"artifact": prevVerifier,
		"contract": PayloadEvidenceContractV2,
	})
	require.Equal(t, http.StatusOK, lw.Code, lw.Body.String())
	var leased payloadLease
	require.NoError(t, json.Unmarshal(lw.Body.Bytes(), &leased))
	require.Equal(t, PayloadEvidenceContractV2, leased.Contract)
	require.Equal(t, prevVerifier, leased.VerifierArtifact)
	evidence.Claim = &payloadClaim{
		Contract:            payloadClaimContract,
		Source:              "tmdb",
		ID:                  "999",
		Type:                "tv_show",
		CheckedAt:           time.Now().UTC(),
		EpisodeDates:        map[int]map[int]string{1: {1: "2020-01-01", 2: "2020-01-08", 3: "2020-01-15", 4: "2020-01-22"}},
		CompleteSeasons:     []int{1},
		SeasonEpisodeCounts: map[int]int{1: 4},
		ProviderReceipts:    []string{strings.Repeat("9", 64)},
	}

	rw := doReq("POST", "/api/publication/v1/result", string(s.cfg.PayloadVerifierToken), payloadWrite{
		Contract:         PayloadEvidenceContractV2,
		InfoHash:         leased.InfoHash,
		SourceRevision:   leased.SourceRevision,
		LeaseID:          leased.LeaseID,
		VerifierArtifact: prevVerifier,
		Outcome:          "approved",
		Reason:           "decoded_media",
		Evidence:         &evidence,
	})
	require.Equal(t, http.StatusNoContent, rw.Code, rw.Body.String())

	consumer := publicationConsumerForConfig(t, p, cfg)
	page, err := consumer.TorrentContent(testContext)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)

	var ttlHours, availHours float64
	require.NoError(t, p.QueryRow(testContext, `SELECT EXTRACT(EPOCH FROM (r.expires_at - now()))/3600.0, EXTRACT(EPOCH FROM (w.available_at - now()))/3600.0
 FROM processor_payload_reviews r JOIN processor_payload_work w ON w.info_hash=r.info_hash WHERE r.info_hash=$1`, h).Scan(&ttlHours, &availHours))
	require.InDelta(t, 30*24, ttlHours, 1.0)
	require.InDelta(t, 29*24, availHours, 1.0)

	fbUI := doReq("GET", "/api/processors/v1/feedback?processor_id="+svc.identity.ID+"&limit=10", string(s.cfg.AdminToken), nil)
	require.Equal(t, http.StatusOK, fbUI.Code, fbUI.Body.String())
	var consumerRows int
	require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM processor_feedback_cursors WHERE processor_id=$1`, svc.identity.ID).Scan(&consumerRows))
	require.Equal(t, 0, consumerRows, "UI read without consumer_id must not create a permanent feedback subscription")

	fbTrainer := doReq("GET", "/api/processors/v1/feedback?processor_id="+svc.identity.ID+"&consumer_id=trainer-1&limit=10", string(s.cfg.AdminToken), nil)
	require.Equal(t, http.StatusOK, fbTrainer.Code, fbTrainer.Body.String())
	require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM processor_feedback_cursors WHERE processor_id=$1`, svc.identity.ID).Scan(&consumerRows))
	require.Equal(t, 1, consumerRows)

	_, err = p.Exec(testContext, `INSERT INTO processor_exclusions(info_hash,job_id,processor_id,reason,binding) VALUES($1,'job-1',$2,'false_positive_test','{}'::jsonb)`, h, svc.identity.ID)
	require.NoError(t, err)
	var curRev int64
	require.NoError(t, p.QueryRow(testContext, `SELECT revision FROM processor_publication_sources WHERE info_hash=$1`, h).Scan(&curRev))
	retractPath := "/api/processors/v1/exclusions/" + hex.EncodeToString(h) + "/retract"
	retractBody := exclusionRetractWrite{
		RequestID:        strings.Repeat("f", 64),
		ExpectedRevision: curRev,
		Reason:           "operator_verified_clean",
	}
	ret := doReq("POST", retractPath, string(s.cfg.AdminToken), retractBody)
	require.Equal(t, http.StatusOK, ret.Code, ret.Body.String())
	retDup := doReq("POST", retractPath, string(s.cfg.AdminToken), retractBody)
	require.Equal(t, http.StatusOK, retDup.Code, "same request_id is idempotent")
	var exclCount, eventCount int
	require.NoError(t, p.QueryRow(testContext, `SELECT (SELECT count(*) FROM processor_exclusions WHERE info_hash=$1),(SELECT count(*) FROM processor_exclusion_events WHERE info_hash=$1)`, h).Scan(&exclCount, &eventCount))
	require.Equal(t, 0, exclCount)
	require.Equal(t, 1, eventCount)
}

func TestPolicyRetryWindowParksOverduePolicyJobs(t *testing.T) {
	s, p, _ := fixture(t)
	s.cfg.PolicyMaxRetryWindow = 5 * time.Minute
	svc := newService(t, "synthetic-reviewer", []v1.Stage{v1.Policy}, func(j v1.Job) v1.Result {
		return v1.Result{
			Binding: j.Binding,
			Outcome: v1.Deferred,
			Reasons: []string{"transient_overload"},
		}
	})
	svc.identity.ArtifactDigest = strings.Repeat("a", 64)
	register(t, s, svc)
	require.NoError(t, s.SetPublicationConfig(serving.Config{
		RequiredProcessor:               svc.identity.ID,
		RequiredProcessorArtifactDigest: svc.identity.ArtifactDigest,
	}))
	seedTorrent(t, p, 9, "movie")
	_, err := s.Scan(testContext)
	require.NoError(t, err)

	// Within retry window: policy deferral reschedules as deferred.
	run(t, s)
	var state, reason string
	require.NoError(t, p.QueryRow(testContext, `SELECT state,reason FROM processor_jobs LIMIT 1`).Scan(&state, &reason))
	require.Equal(t, "deferred", state)
	require.Equal(t, "retry_scheduled", reason)

	// Beyond PolicyMaxRetryWindow: policy deferral parks as failed/policy_retry_exhausted.
	_, err = p.Exec(testContext, `UPDATE processor_jobs SET created_at=now()-interval '10 minutes',available_at=now()-interval '1 second'`)
	require.NoError(t, err)
	run(t, s)
	require.NoError(t, p.QueryRow(testContext, `SELECT state,reason FROM processor_jobs LIMIT 1`).Scan(&state, &reason))
	require.Equal(t, "failed", state)
	require.Equal(t, "policy_retry_exhausted", reason)
}
