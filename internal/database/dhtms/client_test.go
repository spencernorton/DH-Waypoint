package dhtms

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConfigValidationAndEnv(t *testing.T) {
	cfg := DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config should validate: %v", err)
	}
	if cfg.Enabled() || cfg.ShouldShadow() || cfg.ShouldServe() {
		t.Fatalf("default config must be off")
	}

	t.Setenv("DH_TMS_READ_MODE", "shadow")
	t.Setenv("DH_TMS_ENDPOINT", "unix:///tmp/dh-tmsd.sock")
	t.Setenv("DH_TMS_TIMEOUT_MS", "250")
	t.Setenv("DH_TMS_SHADOW_SAMPLE_RATE", "0.25")
	loaded, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv failed: %v", err)
	}
	if !loaded.Enabled() || !loaded.ShouldShadow() || loaded.ShouldServe() {
		t.Fatalf("expected shadow mode, got %+v", loaded)
	}
	if loaded.Timeout != 250*time.Millisecond {
		t.Fatalf("unexpected timeout: %s", loaded.Timeout)
	}

	badCases := []Config{
		{Mode: "invalid", Timeout: time.Second},
		{Mode: ReadModeServe, Endpoint: "", Timeout: time.Second},
		{Mode: ReadModeServe, Endpoint: "ftp://localhost:1234", Timeout: time.Second},
		{Mode: ReadModeServe, Endpoint: "unix://", Timeout: time.Second},
		{Mode: ReadModeOff, Timeout: 0},
		{Mode: ReadModeOff, Timeout: time.Second, ShadowSampleRate: 1.5},
	}
	for i, tc := range badCases {
		if err := tc.Validate(); err == nil {
			t.Errorf("case %d (%+v): expected validation error", i, tc)
		}
	}
}

func TestClientMockHTTPAndErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/health":
			_ = json.NewEncoder(w).Encode(HealthResponse{
				Status:       "ok",
				Engine:       "dh-tms-custom-v0.3",
				GenerationID: 7,
				NumDocs:      100,
				LeaseActive:  true,
			})
		case "/v1/search":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"governance_lease_expired","code":"lease_expired","now_ms":200,"valid_until_ms":100,"pg_lsn":9}`))
		case "/v1/hash/lookup":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid info_hash hex","code":"bad_request"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not_found","code":"not_found"}`))
		}
	}))
	defer srv.Close()

	cli, err := NewClient(Config{
		Mode:     ReadModeServe,
		Endpoint: srv.URL,
		Timeout:  2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cli.Close()

	ctx := context.Background()
	h, err := cli.Health(ctx)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if h.GenerationID != 7 || !h.LeaseActive {
		t.Fatalf("unexpected health: %+v", h)
	}

	_, err = cli.Search(ctx, SearchRequest{Query: "test", Tier: TierPub})
	if !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("expected ErrLeaseExpired, got %v", err)
	}

	_, err = cli.HashLookup(ctx, HashLookupRequest{InfoHash: "bad"})
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("expected ErrBadRequest, got %v", err)
	}
}

func TestShadowComparatorStrictZeroLeakAndLeakDetection(t *testing.T) {
	comp := NewShadowComparator()
	v1 := strings.Repeat("a", 40)
	v2 := strings.Repeat("b", 64)
	extra := strings.Repeat("c", 40)

	diff := comp.CompareStrict(
		[]string{strings.ToUpper(v1), v2},
		[]DocHit{
			{DocID: 1, InfoHashV1: v1},
			{DocID: 2, InfoHashV2: v2},
		},
		15*time.Millisecond,
		time.Millisecond,
	)
	if !diff.ExactSet || len(diff.LeakedHashes) != 0 || diff.Recall != 1.0 {
		t.Fatalf("expected exact zero-leak match, got %+v", diff)
	}

	leakDiff := comp.CompareStrict(
		[]string{v1},
		[]DocHit{
			{DocID: 1, InfoHashV1: v1},
			{DocID: 9, InfoHashV1: extra},
		},
		10*time.Millisecond,
		time.Millisecond,
	)
	if leakDiff.ExactSet || len(leakDiff.LeakedHashes) != 1 || leakDiff.LeakedHashes[0] != extra {
		t.Fatalf("expected single leak for %s, got %+v", extra, leakDiff)
	}

	// Verify concurrent updates under -race.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			comp.CompareStrict([]string{v1}, []DocHit{{InfoHashV1: v1}}, time.Millisecond, time.Millisecond)
			comp.RecordError(ErrLeaseExpired)
		}()
	}
	wg.Wait()

	snap := comp.Snapshot()
	if snap.QueriesCompared != 10 || snap.TotalLeaks != 1 || snap.LeaseExpiredCount != 8 || snap.ErrorCount != 8 {
		t.Fatalf("unexpected shadow stats: %+v", snap)
	}
}

func TestEndToEndWithRealDaemon(t *testing.T) {
	bin := strings.TrimSpace(os.Getenv("DH_TMSD_BIN"))
	if bin == "" {
		t.Skip("DH_TMSD_BIN not set; skipping live dh-tmsd Unix-socket end-to-end test")
	}

	tmpDir := t.TempDir()
	dbRoot := filepath.Join(tmpDir, "db")
	walDir := filepath.Join(tmpDir, "wal")
	sockPath := filepath.Join(tmpDir, "dh-tmsd.sock")

	initCmd := exec.Command(bin, "init-fixture", "--out", dbRoot)
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("dh-tmsd init-fixture failed: %v\n%s", err, out)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const authTok = "phase1-test-shared-token"
	srvCmd := exec.CommandContext(
		ctx,
		bin,
		"serve",
		"--db-root", dbRoot,
		"--wal-dir", walDir,
		"--listen", "unix://"+sockPath,
		"--pg-lsn", "42",
		"--initial-lease-ms", "60000",
	)
	srvCmd.Env = append(os.Environ(), "DH_TMS_AUTH_TOKEN="+authTok)
	if err := srvCmd.Start(); err != nil {
		t.Fatalf("start dh-tmsd: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = srvCmd.Wait()
	})

	unauthCli, err := NewClient(Config{
		Mode:     ReadModeServe,
		Endpoint: "unix://" + sockPath,
		Timeout:  3 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClient unauth: %v", err)
	}
	defer unauthCli.Close()

	cli, err := NewClient(Config{
		Mode:      ReadModeServe,
		Endpoint:  "unix://" + sockPath,
		AuthToken: authTok,
		Timeout:   3 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cli.Close()

	// Wait for Unix socket readiness.
	var health *HealthResponse
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		health, err = cli.Health(ctx)
		if err == nil && health.LeaseActive {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("dh-tmsd never became ready: %v", err)
	}
	if health.NumDocs != 100 || health.Engine != "dh-tms-custom-v0.3" {
		t.Fatalf("unexpected health response: %+v", health)
	}

	if _, err := unauthCli.Search(ctx, SearchRequest{Query: "usual suspects", Tier: TierPub}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized without token, got %v", err)
	}
	nowEpoch := uint32(time.Now().Unix())

	// 1. Text search + facets + files + magnet_xt + BEP-52 v2 alias.
	sr, err := cli.Search(ctx, SearchRequest{
		Query:         "usual suspects",
		Mode:          ModeAnd,
		Tier:          TierPub,
		Limit:         20,
		IncludeFacets: true,
		IncludeFiles:  true,
		NowEpoch:      &nowEpoch,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(sr.Docs) != 1 {
		t.Fatalf("expected 1 pub_approved doc, got %d", len(sr.Docs))
	}
	hit := sr.Docs[0]
	if len(hit.InfoHashV1) != 40 || len(hit.InfoHashV2) != 64 || !strings.HasPrefix(hit.MagnetXT, "urn:btih:") {
		t.Fatalf("invalid magnet/hash metadata: %+v", hit)
	}
	if len(hit.Files) != 2 {
		t.Fatalf("expected 2 hydrated files, got %d", len(hit.Files))
	}

	// 2. ShadowComparator strict verification on clean pub search.
	comp := NewShadowComparator()
	diff := comp.CompareStrict([]string{hit.InfoHashV1}, sr.Docs, 5*time.Millisecond, time.Millisecond)
	if !diff.ExactSet || len(diff.LeakedHashes) != 0 {
		t.Fatalf("expected exact shadow parity, got %+v", diff)
	}

	// 3. Adult FC2-PPV must be excluded even under base tier.
	adultRes, err := cli.Search(ctx, SearchRequest{
		Query:    "fc2 ppv",
		Mode:     ModeAnd,
		Tier:     TierBase,
		Limit:    20,
		NowEpoch: &nowEpoch,
	})
	if err != nil {
		t.Fatalf("adult Search: %v", err)
	}
	if len(adultRes.Docs) != 0 {
		t.Fatalf("adult leak detected: got %d docs", len(adultRes.Docs))
	}

	// 4. Accented title lookup with linked + CT_UNKNOWN unlinked fallback.
	amelie, err := cli.Lookup(ctx, LookupRequest{
		ContentType:   1,
		ContentSource: "tmdb",
		ContentID:     "102",
		Tier:          TierBase,
		Limit:         20,
		NowEpoch:      &nowEpoch,
	})
	if err != nil {
		t.Fatalf("Lookup amelie: %v", err)
	}
	if len(amelie.Linked) != 1 || len(amelie.Unlinked) != 1 {
		t.Fatalf("expected 1 linked + 1 unlinked fallback, got linked=%d unlinked=%d", len(amelie.Linked), len(amelie.Unlinked))
	}

	// 5. TV episode range (S01E02-E04 -> S01E03) + 800-file S02 pack hydration.
	s1, e3 := uint16(1), uint16(3)
	epRes, err := cli.Lookup(ctx, LookupRequest{
		ContentType:   2,
		ContentSource: "tmdb",
		ContentID:     "201",
		Season:        &s1,
		Episode:       &e3,
		Tier:          TierPub,
		Limit:         20,
		NowEpoch:      &nowEpoch,
	})
	if err != nil {
		t.Fatalf("Lookup S01E03: %v", err)
	}
	if len(epRes.Linked) != 1 {
		t.Fatalf("expected 1 TV episode match for S01E03, got %d", len(epRes.Linked))
	}

	s2, e5 := uint16(2), uint16(5)
	packRes, err := cli.Lookup(ctx, LookupRequest{
		ContentType:   2,
		ContentSource: "tmdb",
		ContentID:     "201",
		Season:        &s2,
		Episode:       &e5,
		Tier:          TierPub,
		Limit:         20,
		IncludeFiles:  true,
		NowEpoch:      &nowEpoch,
	})
	if err != nil {
		t.Fatalf("Lookup S02 pack: %v", err)
	}
	if len(packRes.Linked) != 1 || len(packRes.Linked[0].Files) != 800 {
		t.Fatalf("expected 800-file S02 pack, got linked=%d", len(packRes.Linked))
	}

	// 6. Revoke by BEP-52 v2 64-hex hash -> immediately blocks v1 identity at egress.
	rev, err := cli.PolicyRevoke(ctx, PolicyRevokeRequest{
		InfoHash: hit.InfoHashV2,
		Reason:   ReasonCsamDelete,
		PGLSN:    100,
		Sync:     true,
	})
	if err != nil || !rev.OK {
		t.Fatalf("PolicyRevoke by v2: %v (%+v)", err, rev)
	}
	hl, err := cli.HashLookup(ctx, HashLookupRequest{
		InfoHash: hit.InfoHashV1,
		Tier:     TierBase,
		NowEpoch: &nowEpoch,
	})
	if err != nil {
		t.Fatalf("HashLookup after v2 revoke: %v", err)
	}
	if !hl.Found || hl.Eligible {
		t.Fatalf("v2 revocation must block v1 identity, got %+v", hl)
	}

	// 7. RSS + Substring + Hydrate + Dashstats.
	ctMovie := uint8(1)
	rss, err := cli.RSS(ctx, RSSRequest{ContentType: &ctMovie, Tier: TierPub, Limit: 10, NowEpoch: &nowEpoch})
	if err != nil || len(rss.Docs) == 0 {
		t.Fatalf("RSS: err=%v docs=%d", err, len(rss.Docs))
	}
	sub, err := cli.Substring(ctx, SubstringRequest{Substring: "amelie", Tier: TierBase, Limit: 10, NowEpoch: &nowEpoch})
	if err != nil || len(sub.Docs) == 0 {
		t.Fatalf("Substring: err=%v docs=%d", err, len(sub.Docs))
	}
	hyd, err := cli.Hydrate(ctx, HydrateRequest{DocIDs: []uint32{rss.Docs[0].DocID}, Tier: TierPub, NowEpoch: &nowEpoch})
	if err != nil || len(hyd.Docs) != 1 {
		t.Fatalf("Hydrate: err=%v docs=%d", err, len(hyd.Docs))
	}
	ds, err := cli.Dashstats(ctx, DashstatsRequest{Tier: TierPub, NowEpoch: &nowEpoch})
	if err != nil || ds.Movies30d == 0 {
		t.Fatalf("Dashstats: err=%v resp=%+v", err, ds)
	}

	// 8. Swarm select-stale + durable update + checkpoint.
	nowMin := uint32(time.Now().Unix()/60 + 5)
	stale, err := cli.SwarmSelectStale(ctx, SwarmSelectStaleRequest{Limit: 5, NowEpochMin: &nowMin, MinAgeMin: 10})
	if err != nil || len(stale.Items) == 0 {
		t.Fatalf("SwarmSelectStale: err=%v items=%d", err, len(stale.Items))
	}
	sVal, lVal := uint32(512), uint32(19)
	swUp, err := cli.SwarmUpdate(ctx, SwarmUpdateRequest{
		Updates: []SwarmUpdateItem{{
			InfoHash:        stale.Items[0].InfoHashV1,
			Seeders:         &sVal,
			Leechers:        &lVal,
			CheckedEpochMin: nowMin,
		}},
		Sync: true,
	})
	if err != nil || swUp.Applied != 1 {
		t.Fatalf("SwarmUpdate: err=%v resp=%+v", err, swUp)
	}
	chk, err := cli.SwarmCheckpoint(ctx)
	if err != nil || !chk.OK {
		t.Fatalf("SwarmCheckpoint: err=%v resp=%+v", err, chk)
	}

	// 9. Live L0 upsert overlays name & metadata immediately on hydration.
	l0Target := rss.Docs[0].DocID
	l0Resp, err := cli.L0Upsert(ctx, L0UpsertRequest{
		Docs: []L0UpsertItem{{
			DocID:          &l0Target,
			Name:           "Reclassified.Overlay.2026.2160p",
			ContentType:    1,
			EntityID:       9999,
			Year:           2026,
			PublishedEpoch: 1800000050,
			CreatedEpoch:   1800000050,
		}},
		Sync: true,
	})
	if err != nil || l0Resp.L0Count != 1 {
		t.Fatalf("L0Upsert: err=%v resp=%+v", err, l0Resp)
	}
	reHyd, err := cli.Hydrate(ctx, HydrateRequest{DocIDs: []uint32{l0Target}, Tier: TierPub, NowEpoch: &nowEpoch})
	if err != nil || len(reHyd.Docs) != 1 || reHyd.Docs[0].Name != "Reclassified.Overlay.2026.2160p" || reHyd.Docs[0].EntityID != 9999 {
		t.Fatalf("expected L0 overlay on hydrated doc, got err=%v resp=%+v", err, reHyd)
	}

	// 10. Policy batch operation.
	pb, err := cli.PolicyBatch(ctx, PolicyBatchRequest{
		Ops: []PolicyBatchOp{{
			Op:           "grant_payload",
			DocID:        &l0Target,
			ExpiresEpoch: nowEpoch + 3600,
		}},
		PGLSN:           101,
		Sync:            true,
		RebuildSnapshot: true,
		NowEpoch:        &nowEpoch,
	})
	if err != nil || !pb.OK || pb.Applied != 1 {
		t.Fatalf("PolicyBatch: err=%v resp=%+v", err, pb)
	}

	// 11. Fail-closed lease expiry -> stale LSN rejected -> HTTP 503 ErrLeaseExpired -> newer LSN recovers.
	if _, err := cli.PolicyLease(ctx, PolicyLeaseRequest{PGLSN: 102, TTLMS: 0}); err != nil {
		t.Fatalf("PolicyLease expire: %v", err)
	}
	if _, err := cli.PolicyLease(ctx, PolicyLeaseRequest{PGLSN: 102, TTLMS: 60000}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("expected ErrBadRequest on stale pg_lsn renew after expiry, got %v", err)
	}
	_, err = cli.Search(ctx, SearchRequest{Query: "amelie", Tier: TierPub, Limit: 5, NowEpoch: &nowEpoch})
	if !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("expected ErrLeaseExpired when lease is expired, got %v", err)
	}
	if _, err := cli.PolicyLease(ctx, PolicyLeaseRequest{PGLSN: 103, TTLMS: 60000}); err != nil {
		t.Fatalf("PolicyLease renew: %v", err)
	}
	if _, err := cli.Dashstats(ctx, DashstatsRequest{Tier: TierPub, NowEpoch: &nowEpoch}); err != nil {
		t.Fatalf("expected recovery after lease renew, got %v", err)
	}
}
