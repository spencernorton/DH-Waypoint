package match

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
)

func makeMultiCapJob(id v1.Identity, name string, candidates []v1.Candidate) v1.Job {
	return v1.Job{
		Binding: v1.Binding{
			Contract:            v1.Contract,
			JobID:               strings.Repeat("a", 64),
			InputRevision:       strings.Repeat("b", 64),
			PipelineRevision:    strings.Repeat("c", 64),
			ConfigurationDigest: strings.Repeat("d", 64),
			Processor:           id,
			Stage:               v1.Match,
		},
		Capabilities:  []v1.Stage{v1.Extract, v1.Match},
		Configuration: json.RawMessage(`{}`),
		Input: v1.Input{
			InfoHash:      strings.Repeat("e", 40),
			Name:          name,
			Size:          1024 * 1024 * 500,
			Files:         []v1.File{{Path: name, Size: 1024 * 1024 * 500}},
			Deterministic: v1.Deterministic{ProcessingOutcome: "completed", MatchingOutcome: "not_attempted"},
			Candidates:    candidates,
		},
		Deadline: time.Now().UTC().Add(10 * time.Second).Format(time.RFC3339Nano),
	}
}

func postJob(t *testing.T, h http.Handler, token string, j v1.Job) (int, v1.Result) {
	t.Helper()
	raw, err := json.Marshal(j)
	if err != nil {
		t.Fatalf("marshal job: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/process", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var res v1.Result
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatalf("unmarshal result: %v (body=%s)", err, rec.Body.String())
		}
		if err := res.Validate(j); err != nil {
			t.Fatalf("result failed processorapi/v1 validation: %v (result=%+v)", err, res)
		}
	}
	return rec.Code, res
}

func TestAniListProviderMatch(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"data": {
				"Page": {
					"media": [
						{
							"id": 154587,
							"idMal": 52991,
							"seasonYear": 2023,
							"title": {
								"romaji": "Sousou no Frieren",
								"english": "Frieren: Beyond Journey's End",
								"native": "葬送のフリーレン"
							},
							"synonyms": ["Frieren"]
						}
					]
				}
			}
		}`)
	}))
	defer upstream.Close()

	cfg := DefaultConfig("anilist")
	cfg.Endpoint = upstream.URL
	svc, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	job := makeMultiCapJob(svc.manifest.Processor, "[SubsPlease] Frieren - 01 (1080p) [ABC12345].mkv", nil)
	code, res := postJob(t, svc.Handler(), "", job)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if res.Outcome != v1.Completed || res.Proposal == nil || res.Proposal.Match == nil {
		t.Fatalf("expected completed match, got %+v", res)
	}
	if res.Proposal.Match.Source != "anilist" || res.Proposal.Match.ID != "154587" {
		t.Fatalf("unexpected match ref: %+v", res.Proposal.Match)
	}
	if res.Proposal.Extraction == nil || res.Proposal.Extraction.Year != 2023 {
		t.Fatalf("unexpected extraction: %+v", res.Proposal.Extraction)
	}
	if len(res.Evidence) < 2 || res.Evidence[1].Reference != "mal:52991" {
		t.Fatalf("expected mal cross-ref evidence, got %+v", res.Evidence)
	}
}

func TestMusicBrainzProviderMatch(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("User-Agent"), "DH-Waypoint") {
			t.Errorf("missing polite User-Agent: %q", r.Header.Get("User-Agent"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"release-groups": [
				{
					"id": "6e335887-60ba-38f0-95af-fae7774336bf",
					"title": "In Rainbows",
					"first-release-date": "2007-10-10",
					"artist-credit": [{"name": "Radiohead"}]
				}
			]
		}`)
	}))
	defer upstream.Close()

	cfg := DefaultConfig("musicbrainz")
	cfg.Endpoint = upstream.URL
	svc, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	job := makeMultiCapJob(svc.manifest.Processor, "Radiohead - In Rainbows (2007) [FLAC]", nil)
	code, res := postJob(t, svc.Handler(), "", job)
	if code != http.StatusOK || res.Outcome != v1.Completed || res.Proposal.Match.ID != "6e335887-60ba-38f0-95af-fae7774336bf" {
		t.Fatalf("unexpected musicbrainz result: code=%d res=%+v", code, res)
	}
}

func TestIGDBProviderMatchAndQuoteSanitization(t *testing.T) {
	var seenBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Client-ID") != "cid-test" || r.Header.Get("Authorization") != "Bearer tok-test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		seenBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[
			{
				"id": 194592,
				"name": "Hades II",
				"first_release_date": 1715040000,
				"alternative_names": [{"name": "Hades 2"}]
			}
		]`)
	}))
	defer upstream.Close()

	cfg := DefaultConfig("igdb")
	cfg.Endpoint = upstream.URL
	cfg.APIClientID = "cid-test"
	cfg.APIBearerToken = "tok-test"
	svc, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	job := makeMultiCapJob(svc.manifest.Processor, "Hades II v1.0-RUNE", nil)
	code, res := postJob(t, svc.Handler(), "", job)
	if code != http.StatusOK || res.Outcome != v1.Completed || res.Proposal.Match.Source != "igdb" || res.Proposal.Match.ID != "194592" {
		t.Fatalf("unexpected igdb result: code=%d res=%+v", code, res)
	}
	if !strings.Contains(seenBody, `search "Hades II";`) {
		t.Fatalf("unexpected apicalypse query: %s", seenBody)
	}
	sanitized := sanitizeApicalypseString(`Bad"; limit 500;`)
	if strings.Contains(sanitized, ";") || strings.Contains(sanitized, `Bad"`) {
		t.Fatalf("apicalypse injection not sanitized: %q", sanitized)
	}
}

func TestOpenLibraryProviderMatch(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"docs": [
				{
					"key": "/works/OL893415W",
					"title": "Dune",
					"first_publish_year": 1965,
					"author_name": ["Frank Herbert"]
				}
			]
		}`)
	}))
	defer upstream.Close()

	cfg := DefaultConfig("openlibrary")
	cfg.Endpoint = upstream.URL
	svc, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	job := makeMultiCapJob(svc.manifest.Processor, "Frank Herbert - Dune (1965) [EPUB]", nil)
	code, res := postJob(t, svc.Handler(), "", job)
	if code != http.StatusOK || res.Outcome != v1.Completed || res.Proposal.Match.Source != "openlibrary" || res.Proposal.Match.ID != "OL893415W" {
		t.Fatalf("unexpected openlibrary result: code=%d res=%+v", code, res)
	}
}

func TestOfferedCandidateModeAndAbstainAndDefer(t *testing.T) {
	rateLimited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer rateLimited.Close()

	cfg := DefaultConfig("anilist")
	cfg.Endpoint = rateLimited.URL
	cfg.Token = "proc-secret"
	svc, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Missing auth -> 401.
	req := httptest.NewRequest(http.MethodGet, "/v1/manifest", nil)
	rec := httptest.NewRecorder()
	svc.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", rec.Code)
	}

	// Valid auth on /v1/manifest and /v1/status.
	for _, path := range []string{"/v1/manifest", "/v1/status"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer proc-secret")
		rec := httptest.NewRecorder()
		svc.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s returned %d", path, rec.Code)
		}
	}

	// Upstream 429 -> Deferred with retry_after_seconds.
	job := makeMultiCapJob(svc.manifest.Processor, "[SubsPlease] Cowboy Bebop - 01 (1080p).mkv", nil)
	code, res := postJob(t, svc.Handler(), "proc-secret", job)
	if code != http.StatusOK || res.Outcome != v1.Deferred || res.RetryAfterSeconds != 30 {
		t.Fatalf("expected deferred on 429, got code=%d res=%+v", code, res)
	}

	// Offered candidates in single-stage Match mode select strictly from offered candidates.
	offered := []v1.Candidate{
		{MatchRef: v1.MatchRef{Source: "tmdb", ID: "100"}, ContentType: "tv_show", Title: "Cowboy Bebop", Year: 1998},
		{MatchRef: v1.MatchRef{Source: "tmdb", ID: "200"}, ContentType: "tv_show", Title: "Samurai Champloo", Year: 2004},
	}
	singleStageJob := makeMultiCapJob(svc.manifest.Processor, "Cowboy.Bebop.1998.S01E01.1080p.BluRay.x264-GRP", offered)
	singleStageJob.Capabilities = nil
	code, res = postJob(t, svc.Handler(), "proc-secret", singleStageJob)
	if code != http.StatusOK || res.Outcome != v1.Completed || res.Proposal.Match.ID != "100" {
		t.Fatalf("expected offered candidate 100, got code=%d res=%+v", code, res)
	}

	// Ambiguous tie abstains.
	tied := []v1.Candidate{
		{MatchRef: v1.MatchRef{Source: "tmdb", ID: "1"}, ContentType: "movie", Title: "Arrival", Year: 2016},
		{MatchRef: v1.MatchRef{Source: "tmdb", ID: "2"}, ContentType: "movie", Title: "Arrival", Year: 2016},
	}
	ambigJob := makeMultiCapJob(svc.manifest.Processor, "Arrival.2016.1080p.BluRay", tied)
	ambigJob.Capabilities = nil
	code, res = postJob(t, svc.Handler(), "proc-secret", ambigJob)
	if code != http.StatusOK || res.Outcome != v1.Abstained {
		t.Fatalf("expected abstained on tie, got code=%d res=%+v", code, res)
	}
}

func TestConfigRejectsUnsafeNonLoopbackWithoutToken(t *testing.T) {
	cfg := DefaultConfig("anilist")
	cfg.Listen = "0.0.0.0:8097"
	if _, err := New(cfg); err == nil {
		t.Fatal("expected error for non-loopback listen without token")
	}
	cfg.Provider = "unknown"
	cfg.Listen = "127.0.0.1:8097"
	if _, err := New(cfg); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}
