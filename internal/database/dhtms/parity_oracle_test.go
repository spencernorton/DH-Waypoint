package dhtms

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/classifier"
	"github.com/spencernorton/DH-Waypoint/internal/keywords"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/namepolicy"
	"github.com/spencernorton/DH-Waypoint/internal/serving"
)

func TestMappingAndGormPlaceholderConversion(t *testing.T) {
	for code := uint8(1); code <= 10; code++ {
		ct := CodeToContentType(code)
		if !ct.Valid {
			t.Fatalf("code %d returned invalid content type", code)
		}
		if got := ContentTypeToCode(ct.ContentType.String()); got != code {
			t.Fatalf("content type roundtrip code %d -> %q -> %d", code, ct.ContentType, got)
		}
	}
	if CodeToContentType(0).Valid || ContentTypeToCode("unknown") != 0 {
		t.Fatalf("unexpected unknown content type mapping")
	}

	for code := uint8(1); code <= 9; code++ {
		res := CodeToResolution(code)
		if !res.Valid {
			t.Fatalf("code %d returned invalid resolution", code)
		}
		if got := ResolutionToCode(res.VideoResolution.String()); got != code {
			t.Fatalf("resolution roundtrip code %d -> %q -> %d", code, res.VideoResolution, got)
		}
	}
	if ResolutionToCode("1080p") != 6 || ResolutionToCode("2160p") != 8 {
		t.Fatalf("alias resolution mapping failed")
	}

	lsn, err := ParsePGLSN("1/2A")
	if err != nil || lsn != ((1<<32)|0x2A) {
		t.Fatalf("ParsePGLSN(1/2A) = %x, %v", lsn, err)
	}
	lsnZero, err := ParsePGLSN("0/0")
	if err != nil || lsnZero != 1 {
		t.Fatalf("ParsePGLSN(0/0) should clamp to 1, got %d, %v", lsnZero, err)
	}

	sqlOut, args, next := GormExprToPG("a = ? AND b = '?' AND c = 'it''s ?' AND d = ?", []any{"x", "y"}, 3)
	want := "a = $3 AND b = '?' AND c = 'it''s ?' AND d = $4"
	if sqlOut != want || next != 5 || len(args) != 2 {
		t.Fatalf("GormExprToPG mismatch: got %q (next=%d), want %q", sqlOut, next, want)
	}
}

func TestConfigFromEnvAutoDefaultsToServeWhenEndpointPresent(t *testing.T) {
	t.Setenv("DH_TMS_ENDPOINT", "http://dh-tmsd:7841")
	t.Setenv("DH_TMS_READ_MODE", "")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv auto-serve failed: %v", err)
	}
	if !cfg.Enabled() || !cfg.ShouldServe() {
		t.Fatalf("expected auto-serve mode when endpoint is set, got %+v", cfg)
	}

	t.Setenv("DH_TMS_READ_MODE", "off")
	cfgOff, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv explicit off failed: %v", err)
	}
	if cfgOff.Enabled() || cfgOff.ShouldServe() {
		t.Fatalf("explicit off must disable dh-tms, got %+v", cfgOff)
	}
}

// startTestDaemon boots an isolated empty-volume dh-tmsd process when DH_TMSD_BIN
// is set, or connects to an externally managed daemon when DH_TMSD_URL is set.
func startTestDaemon(t *testing.T) (*Client, string) {
	t.Helper()
	if url := strings.TrimSpace(os.Getenv("DH_TMSD_URL")); url != "" {
		cli, err := NewClient(Config{
			Mode:      ReadModeServe,
			Endpoint:  url,
			AuthToken: strings.TrimSpace(os.Getenv("DH_TMS_AUTH_TOKEN")),
			Timeout:   3 * time.Second,
		})
		if err != nil {
			t.Fatalf("NewClient(%s): %v", url, err)
		}
		t.Cleanup(cli.Close)
		return cli, url
	}

	bin := strings.TrimSpace(os.Getenv("DH_TMSD_BIN"))
	if bin == "" {
		t.Skip("neither DH_TMSD_BIN nor DH_TMSD_URL is set; skipping live dh-tmsd test")
	}
	tmpDir := t.TempDir()
	dbRoot := filepath.Join(tmpDir, "db")
	walDir := filepath.Join(tmpDir, "wal")
	sockPath := filepath.Join(tmpDir, "dh-tmsd.sock")
	authTok := fmt.Sprintf("oracle-%d", time.Now().UnixNano())

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(
		ctx,
		bin,
		"serve",
		"--db-root", dbRoot,
		"--wal-dir", walDir,
		"--listen", "unix://"+sockPath,
		"--pg-lsn", "1",
		"--initial-lease-ms", "60000",
	)
	cmd.Env = append(os.Environ(), "DH_TMS_AUTH_TOKEN="+authTok)
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start dh-tmsd: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
	})

	ep := "unix://" + sockPath
	cli, err := NewClient(Config{
		Mode:      ReadModeServe,
		Endpoint:  ep,
		AuthToken: authTok,
		Timeout:   3 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(cli.Close)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if h, err := cli.Health(context.Background()); err == nil && h.Status != "" {
			return cli, ep
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("dh-tmsd failed to become healthy on %s", ep)
	return nil, ""
}

// TestGoRustGoldenCorpusParityOracle deterministically verifies S2-2:
// Go serving.Policy + namepolicy + release metadata streamed via chunked NDJSON
// to dh-tmsd produces exact zero-leak visibility across base/pub/payload tiers,
// preserves accents/sequels/episodes/unlinked fallbacks, and indexes file #20+.
func TestGoRustGoldenCorpusParityOracle(t *testing.T) {
	cli, _ := startTestDaemon(t)
	ctx := context.Background()

	strongKW, mediaExt, err := classifier.CoreAdultServingEvidence()
	if err != nil {
		t.Fatalf("CoreAdultServingEvidence: %v", err)
	}
	adultRe, err := keywords.NewRegexFromKeywords(strongKW...)
	if err != nil {
		t.Fatalf("NewRegexFromKeywords: %v", err)
	}
	mediaSet := make(map[string]bool, len(mediaExt))
	for _, ext := range mediaExt {
		mediaSet[strings.ToLower(ext)] = true
	}

	np, err := namepolicy.New(namepolicy.Config{Enabled: true})
	if err != nil || !np.Enabled() {
		t.Fatalf("namepolicy.New failed: %v", err)
	}
	if TierForPolicy(nil) != TierBase {
		t.Fatalf("nil policy should map to TierBase")
	}
	sp, err := serving.NewPolicy(serving.Config{ExcludeAdult: true}, strongKW, mediaExt, np)
	if err != nil || TierForPolicy(sp) != TierBase {
		t.Fatalf("NewPolicy failed: %v", err)
	}

	isAdultGo := func(name string, files []BuildFileInput) bool {
		var mediaBytes int64
		for _, f := range files {
			if mediaSet[strings.ToLower(f.Extension)] {
				mediaBytes += int64(f.Size)
			} else {
				mediaBytes -= int64(f.Size)
			}
		}
		if mediaBytes <= 0 {
			return false
		}
		if adultRe.MatchString(name) {
			return true
		}
		for _, f := range files {
			if adultRe.MatchString(f.Path) {
				return true
			}
		}
		return false
	}

	nowEpoch := uint32(time.Now().Unix())
	mkHash := func(b byte) string {
		return strings.Repeat(fmt.Sprintf("%02x", b), 20)
	}

	deepFiles := make([]BuildFileInput, 25)
	for i := range deepFiles {
		name := fmt.Sprintf("Season.01/Disc.%02d/Episode.%02d.mkv", 1, i+1)
		if i == 19 {
			name = "Season.01/Extras/GoldenOracleTokenTwenty.mkv"
		}
		deepFiles[i] = BuildFileInput{Path: name, Size: 500_000_000, Extension: "mkv"}
	}

	type oracleCase struct {
		doc           BuildDocLine
		expectBase    bool
		expectPub     bool
		expectPayload bool
	}

	cases := []oracleCase{
		{
			doc: BuildDocLine{
				Kind: "doc", InfoHashV1: mkHash(0x11),
				Name: "The.Usual.Suspects.1995.1080p.BluRay.x264-GRP", SizeBytes: 9_000_000_000,
				ContentType: ContentTypeToCode(string(model.ContentTypeMovie)), EntityID: 1, Year: 1995,
				PublishedEpoch: nowEpoch - 100, CreatedEpoch: nowEpoch - 100, Resolution: 6,
				Files:               []BuildFileInput{{Path: "The.Usual.Suspects.1995.1080p.mkv", Size: 9_000_000_000, Extension: "mkv"}},
				AuthoritativePolicy: true, PubApproved: true, PayloadExpiresEpoch: nowEpoch + 3600,
				Seeders: 42, Leechers: 3,
			},
			expectBase: true, expectPub: true, expectPayload: true,
		},
		{
			doc: BuildDocLine{
				Kind: "doc", InfoHashV1: mkHash(0x12),
				Name: "Amélie.2001.1080p.WEB-DL.H264", SizeBytes: 4_000_000_000,
				ContentType: ContentTypeToCode(string(model.ContentTypeMovie)), EntityID: 2, Year: 2001,
				PublishedEpoch: nowEpoch - 90, CreatedEpoch: nowEpoch - 90, Resolution: 6,
				Files:               []BuildFileInput{{Path: "Amelie.2001.mkv", Size: 4_000_000_000, Extension: "mkv"}},
				AuthoritativePolicy: true, PubApproved: true, PayloadExpiresEpoch: nowEpoch - 60, // EXPIRED payload!
				Seeders: 10, Leechers: 1,
			},
			expectBase: true, expectPub: true, expectPayload: false,
		},
		{
			doc: BuildDocLine{
				Kind: "doc", InfoHashV1: mkHash(0x13),
				Name: "Amelie.2001.720p.WEBRip", SizeBytes: 2_000_000_000,
				ContentType: 0, EntityID: 0, Year: 2001, // Unlinked fallback candidate
				PublishedEpoch: nowEpoch - 80, CreatedEpoch: nowEpoch - 80, Resolution: 5,
				Files:               []BuildFileInput{{Path: "Amelie.2001.720p.mkv", Size: 2_000_000_000, Extension: "mkv"}},
				AuthoritativePolicy: true, PubApproved: false, Seeders: 8, Leechers: 2,
			},
			expectBase: true, expectPub: false, expectPayload: false,
		},
		{
			doc: BuildDocLine{
				Kind: "doc", InfoHashV1: mkHash(0x14),
				Name: "Suits.S01E02-E04.1080p.WEB-DL", SizeBytes: 6_000_000_000,
				ContentType: ContentTypeToCode(string(model.ContentTypeTvShow)), EntityID: 3, Year: 2011,
				PublishedEpoch: nowEpoch - 70, CreatedEpoch: nowEpoch - 70, Resolution: 6,
				Files: deepFiles, AuthoritativePolicy: true, PubApproved: true, PayloadExpiresEpoch: nowEpoch + 3600,
				Seeders: 25, Leechers: 4,
			},
			expectBase: true, expectPub: true, expectPayload: true,
		},
		{
			doc: BuildDocLine{
				Kind: "doc", InfoHashV1: mkHash(0x15),
				Name: "FC2-PPV-1234567.1080p.mp4", SizeBytes: 2_000_000_000,
				ContentType: ContentTypeToCode(string(model.ContentTypeMovie)), EntityID: 0,
				PublishedEpoch: nowEpoch - 60, CreatedEpoch: nowEpoch - 60,
				Files:               []BuildFileInput{{Path: "FC2-PPV-1234567.mp4", Size: 2_000_000_000, Extension: "mp4"}},
				AuthoritativePolicy: true, PubApproved: true, PayloadExpiresEpoch: nowEpoch + 3600,
			},
			expectBase: false, expectPub: false, expectPayload: false,
		},
		{
			doc: BuildDocLine{
				Kind: "doc", InfoHashV1: mkHash(0x16),
				Name: "流浪地球.The.Wandering.Earth.2019.1080p", SizeBytes: 5_000_000_000,
				ContentType:    ContentTypeToCode(string(model.ContentTypeMovie)),
				PublishedEpoch: nowEpoch - 50, CreatedEpoch: nowEpoch - 50,
				Files:               []BuildFileInput{{Path: "movie.mkv", Size: 5_000_000_000, Extension: "mkv"}},
				AuthoritativePolicy: true, PubApproved: true, PayloadExpiresEpoch: nowEpoch + 3600,
			},
			expectBase: false, expectPub: false, expectPayload: false,
		},
		{
			doc: BuildDocLine{
				Kind: "doc", InfoHashV1: mkHash(0x17),
				Name: "Private.Usual.Suspects.1995.1080p", SizeBytes: 5_000_000_000,
				ContentType: ContentTypeToCode(string(model.ContentTypeMovie)), EntityID: 1,
				PublishedEpoch: nowEpoch - 40, CreatedEpoch: nowEpoch - 40,
				IsPrivate: true, AuthoritativePolicy: true, PubApproved: true, PayloadExpiresEpoch: nowEpoch + 3600,
			},
			expectBase: false, expectPub: false, expectPayload: false,
		},
		{
			doc: BuildDocLine{
				Kind: "doc", InfoHashV1: mkHash(0x18),
				Name: "Quarantined.Usual.Suspects.1995.1080p", SizeBytes: 5_000_000_000,
				ContentType: ContentTypeToCode(string(model.ContentTypeMovie)), EntityID: 1,
				PublishedEpoch: nowEpoch - 30, CreatedEpoch: nowEpoch - 30,
				Quarantined: true, AuthoritativePolicy: true, PubApproved: true, PayloadExpiresEpoch: nowEpoch + 3600,
			},
			expectBase: false, expectPub: false, expectPayload: false,
		},
	}

	// Evaluate Go adult & namepolicy predicates identically to Syncer -> authoritative_policy.
	for i := range cases {
		if isAdultGo(cases[i].doc.Name, cases[i].doc.Files) {
			cases[i].doc.Adult = true
		}
		h, _ := protocol.ParseInfoHash(cases[i].doc.InfoHashV1)
		if !np.Evaluate(h, cases[i].doc.Name).Eligible {
			cases[i].doc.NameBlocked = true
		}
	}

	var payload bytes.Buffer
	enc := json.NewEncoder(&payload)
	_ = enc.Encode(BuildHeaderLine{Kind: "header", PGLSN: 500, LeaseTTLMS: 60000, PGLSNText: "0/1F4"})
	_ = enc.Encode(BuildCatalogLine{
		Kind: "catalog", EntityID: 1, ContentType: 1, Source: "tmdb", ID: "629",
		Title: "The Usual Suspects", Year: 1995, AltIdentifiers: [][2]string{{"imdb", "tt0114814"}},
	})
	_ = enc.Encode(BuildCatalogLine{
		Kind: "catalog", EntityID: 2, ContentType: 1, Source: "tmdb", ID: "194",
		Title: "Amélie", Year: 2001, AltIdentifiers: [][2]string{{"imdb", "tt0211915"}},
	})
	_ = enc.Encode(BuildCatalogLine{
		Kind: "catalog", EntityID: 3, ContentType: 2, Source: "tmdb", ID: "37680",
		Title: "Suits", Year: 2011, AltIdentifiers: [][2]string{{"imdb", "tt1632701"}},
	})
	for _, tc := range cases {
		_ = enc.Encode(tc.doc)
	}

	// Stream via chunked HTTP transfer (-1) to exercise ChunkedReader end-to-end.
	resp, err := cli.BuildGeneration(ctx, bytes.NewReader(payload.Bytes()), -1)
	if err != nil {
		t.Fatalf("BuildGeneration chunked failed: %v", err)
	}
	if !resp.OK || resp.NumDocs != len(cases) {
		t.Fatalf("unexpected build response: %+v", resp)
	}

	// Verify exact visibility across every case and all 3 tiers.
	for _, tc := range cases {
		for _, check := range []struct {
			tier PolicyTier
			want bool
		}{
			{TierBase, tc.expectBase},
			{TierPub, tc.expectPub},
			{TierPayload, tc.expectPayload},
		} {
			hl, err := cli.HashLookup(ctx, HashLookupRequest{
				InfoHash: tc.doc.InfoHashV1,
				Tier:     check.tier,
				NowEpoch: &nowEpoch,
			})
			if err != nil {
				t.Fatalf("HashLookup(%s, %s): %v", tc.doc.Name, check.tier, err)
			}
			if !hl.Found || hl.Eligible != check.want {
				t.Fatalf("parity violation on %q tier=%s: got found=%v eligible=%v, want eligible=%v",
					tc.doc.Name, check.tier, hl.Found, hl.Eligible, check.want)
			}
		}
	}

	// Verify file #20 token search hit (Gate 0 12-file truncation fix).
	deepHit, err := cli.Search(ctx, SearchRequest{
		Query: "goldenoracletokentwenty", Mode: ModeAnd, Tier: TierPayload, Limit: 10, NowEpoch: &nowEpoch,
	})
	if err != nil || len(deepHit.Docs) != 1 || deepHit.Docs[0].InfoHashV1 != mkHash(0x14) {
		t.Fatalf("expected file #20 hit on Suits torrent, got err=%v docs=%+v", err, deepHit)
	}

	// Verify IMDB alternate-identifier lookup returns linked + unlinked fallback with 0 leaks.
	amelie, err := cli.Lookup(ctx, LookupRequest{
		ContentType: 1, ContentSource: "imdb", ContentID: "tt0211915", Tier: TierBase, Limit: 10, NowEpoch: &nowEpoch,
	})
	if err != nil || len(amelie.Linked) != 1 || len(amelie.Unlinked) != 1 {
		t.Fatalf("expected 1 linked + 1 unlinked for Amelie IMDB tt0211915, got err=%v resp=%+v", err, amelie)
	}

	// Verify zero adult / non-Latin / private / quarantined leaks on search.
	for _, q := range []string{"fc2 ppv", "wandering earth", "private usual", "quarantined usual"} {
		res, err := cli.Search(ctx, SearchRequest{Query: q, Mode: ModeAnd, Tier: TierBase, Limit: 10, NowEpoch: &nowEpoch})
		if err != nil {
			t.Fatalf("search %q failed: %v", q, err)
		}
		if len(res.Docs) != 0 {
			t.Fatalf("visibility leak on %q: got %+v", q, res.Docs)
		}
	}
}
