// Package match is a standalone dh-waypoint.processor.v1 reference plugin
// matching torrents against external metadata catalogues (AniList, MusicBrainz,
// IGDB, and OpenLibrary). It imports only the standard library and
// processorapi/v1.
package match

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
)

// Config configures one standalone reference match adapter instance.
type Config struct {
	Listen            string
	Provider          string // anilist | musicbrainz | igdb | openlibrary
	Endpoint          string // optional override for provider API URL
	Token             string // bearer token required from core; mandatory off loopback
	APIClientID       string // IGDB Twitch Client-ID
	APIBearerToken    string // IGDB Twitch OAuth bearer token
	UserAgent         string // polite User-Agent for MusicBrainz / OpenLibrary
	MinConfidence     float64
	MinMargin         float64
	Timeout           time.Duration
	Concurrency       int
	RetryAfterSeconds int
}

// DefaultConfig returns bounded, loopback-only defaults for a match provider.
func DefaultConfig(provider string) Config {
	return Config{
		Listen:            "127.0.0.1:8097",
		Provider:          provider,
		MinConfidence:     0.65,
		MinMargin:         0.08,
		Timeout:           10 * time.Second,
		Concurrency:       2,
		RetryAfterSeconds: 30,
	}
}

// Service serves the dh-waypoint.processor.v1 HTTP contract for one provider.
type Service struct {
	cfg      Config
	provider Provider
	endpoint string
	manifest v1.Manifest
	instance string
	started  string
	client   *http.Client
	slots    chan struct{}
	attempts atomic.Int64
}

// New validates Config and builds a ready-to-serve reference match Service.
func New(c Config) (*Service, error) {
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return nil, errors.New("invalid_listen_address")
	}
	if ip := net.ParseIP(host); c.Token == "" && host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, errors.New("processor_token_required_for_network_listen")
	}
	prov, err := newProvider(c)
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimSpace(c.Endpoint)
	if endpoint == "" {
		endpoint = prov.DefaultEndpoint()
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("invalid_provider_endpoint")
	}
	if c.MinConfidence < 0 || c.MinConfidence > 1 || c.MinMargin < 0 || c.MinMargin > 1 ||
		c.Concurrency < 1 || c.Concurrency > 16 || c.Timeout <= 0 || c.Timeout > time.Minute {
		return nil, errors.New("invalid_service_bounds")
	}
	if c.RetryAfterSeconds <= 0 || c.RetryAfterSeconds > 3600 {
		c.RetryAfterSeconds = 30
	}

	id := v1.Identity{
		ID:             "match-" + prov.Name(),
		Version:        "1.0.0",
		ModelVersion:   "none",
		PolicyVersion:  fmt.Sprintf("min-conf-%.2f", c.MinConfidence),
		ArtifactDigest: "none",
	}
	if id.Validate() != nil {
		return nil, errors.New("invalid_processor_identity")
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, errors.New("instance_id_unavailable")
	}

	return &Service{
		cfg:      c,
		provider: prov,
		endpoint: endpoint,
		instance: hex.EncodeToString(b[:]),
		started:  time.Now().UTC().Format(time.RFC3339Nano),
		manifest: v1.Manifest{
			Contract:            v1.Contract,
			Processor:           id,
			Capabilities:        []v1.Stage{v1.Extract, v1.Match},
			ConfigurationSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
			Qualification:       "synthetic",
		},
		client: &http.Client{
			Timeout: c.Timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		slots: make(chan struct{}, c.Concurrency),
	}, nil
}

// Handler returns the HTTP handler serving /v1/manifest, /v1/status, and /v1/process.
func (s *Service) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Token != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.cfg.Token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		switch {
		case r.URL.Path == "/v1/manifest" && r.Method == http.MethodGet:
			writeJSON(w, http.StatusOK, s.manifest)
		case r.URL.Path == "/v1/status" && r.Method == http.MethodGet:
			n := s.attempts.Load()
			writeJSON(w, http.StatusOK, v1.ServiceStatus{
				Contract:   v1.Contract,
				Processor:  s.manifest.Processor,
				State:      "ready",
				InstanceID: s.instance,
				StartedAt:  s.started,
				Attempts:   &n,
			})
		case r.URL.Path == "/v1/process" && r.Method == http.MethodPost:
			s.serveProcess(w, r)
		default:
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		}
	})
}

func (s *Service) serveProcess(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, v1.MaxBody+1))
	var j v1.Job
	if err != nil || len(raw) > v1.MaxBody || v1.Decode(raw, &j) != nil || j.Validate() != nil ||
		j.Processor != s.manifest.Processor || strings.TrimSpace(string(j.Configuration)) != "{}" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "invalid_job"})
		return
	}
	caps := j.EffectiveCapabilities()
	if !slices.Contains(caps, v1.Match) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "unsupported_stage"})
		return
	}
	for _, cap := range caps {
		if !slices.Contains(s.manifest.Capabilities, cap) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "unsupported_capability"})
			return
		}
	}

	s.attempts.Add(1)
	started := time.Now()

	ctx := r.Context()
	if dl, err := time.Parse(time.RFC3339Nano, j.Deadline); err == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, dl)
		defer cancel()
	}

	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		s.writeResult(w, j, v1.Deferred, nil, []string{"busy_deadline_exceeded"}, nil, nil, s.cfg.RetryAfterSeconds, started)
		return
	}

	q := NormalizeQuery(j.Input)
	if q.Title == "" {
		s.writeResult(w, j, v1.Abstained, nil, []string{"empty_normalized_title"}, nil, nil, 0, started)
		return
	}

	multiCap := len(j.Capabilities) > 1
	// When the core already provided explicit candidates, or in single-stage
	// Match mode, choose strictly among offered candidates to honor contract
	// invariants.
	if len(j.Input.Candidates) > 0 || !multiCap {
		s.matchOfferedCandidates(w, j, q, started)
		return
	}

	extCandidates, err := s.provider.Search(ctx, s.client, s.endpoint, q)
	if err != nil {
		if errors.Is(err, errUpstreamTransient) {
			s.writeResult(w, j, v1.Deferred, nil, []string{"upstream_transient_error"}, nil, nil, s.cfg.RetryAfterSeconds, started)
			return
		}
		s.writeResult(w, j, v1.Abstained, nil, []string{"upstream_rejected_query"}, nil, nil, 0, started)
		return
	}
	if len(extCandidates) == 0 {
		s.writeResult(w, j, v1.Abstained, nil, []string{"no_external_candidates"}, nil, nil, 0, started)
		return
	}

	bestIdx, bestScore, secondScore := rankExternalCandidates(q, extCandidates)
	if bestIdx < 0 || bestScore < s.cfg.MinConfidence {
		s.writeResult(w, j, v1.Abstained, nil, []string{"below_min_confidence"}, floatPtr(clamp01(1-bestScore)), nil, 0, started)
		return
	}
	if secondScore > 0 && bestScore-secondScore < s.cfg.MinMargin {
		s.writeResult(w, j, v1.Abstained, nil, []string{"ambiguous_top_candidates"}, floatPtr(clamp01(1-(bestScore-secondScore))), nil, 0, started)
		return
	}

	winner := extCandidates[bestIdx]
	ev := make([]v1.Evidence, 0, len(winner.Evidence))
	for _, ref := range winner.Evidence {
		if len(ev) >= 8 {
			break
		}
		ev = append(ev, v1.Evidence{Kind: "external_match", Reference: ref})
	}
	proposal := &v1.Proposal{
		Extraction: &v1.Extraction{
			Title: winner.Title,
			Year:  winner.Year,
		},
		Match: &v1.MatchRef{
			Source: winner.Source,
			ID:     winner.ID,
		},
	}
	s.writeResult(w, j, v1.Completed, proposal, []string{"matched_" + s.provider.Name()}, floatPtr(clamp01(1-bestScore)), ev, 0, started)
}

func (s *Service) matchOfferedCandidates(w http.ResponseWriter, j v1.Job, q Query, started time.Time) {
	if len(j.Input.Candidates) == 0 {
		s.writeResult(w, j, v1.Abstained, nil, []string{"no_offered_candidates"}, nil, nil, 0, started)
		return
	}
	wrapped := make([]ExternalCandidate, 0, len(j.Input.Candidates))
	for _, c := range j.Input.Candidates {
		wrapped = append(wrapped, ExternalCandidate{
			Source:  c.Source,
			ID:      c.ID,
			Title:   c.Title,
			Year:    c.Year,
			Aliases: c.Aliases,
		})
	}
	bestIdx, bestScore, secondScore := rankExternalCandidates(q, wrapped)
	if bestIdx < 0 || bestScore < s.cfg.MinConfidence {
		s.writeResult(w, j, v1.Abstained, nil, []string{"below_min_confidence"}, floatPtr(clamp01(1-bestScore)), nil, 0, started)
		return
	}
	if secondScore > 0 && bestScore-secondScore < s.cfg.MinMargin {
		s.writeResult(w, j, v1.Abstained, nil, []string{"ambiguous_top_candidates"}, floatPtr(clamp01(1-(bestScore-secondScore))), nil, 0, started)
		return
	}
	chosen := j.Input.Candidates[bestIdx]
	proposal := &v1.Proposal{
		Match: &v1.MatchRef{Source: chosen.Source, ID: chosen.ID},
	}
	ev := []v1.Evidence{{Kind: "candidate_match", Reference: chosen.Source + ":" + chosen.ID}}
	s.writeResult(w, j, v1.Completed, proposal, []string{"matched_offered_candidate"}, floatPtr(clamp01(1-bestScore)), ev, 0, started)
}

func (s *Service) writeResult(w http.ResponseWriter, j v1.Job, outcome v1.Outcome, proposal *v1.Proposal, reasons []string, uncertainty *float64, evidence []v1.Evidence, retryAfter int, started time.Time) {
	if reasons == nil {
		reasons = []string{}
	}
	if evidence == nil {
		evidence = []v1.Evidence{}
	}
	ms := float64(time.Since(started).Microseconds()) / 1000.0
	res := v1.Result{
		Binding:           j.Binding,
		Outcome:           outcome,
		Proposal:          proposal,
		Reasons:           reasons,
		Uncertainty:       uncertainty,
		Evidence:          evidence,
		RetryAfterSeconds: retryAfter,
		Telemetry: v1.Telemetry{
			ObservedAt:  time.Now().UTC().Format(time.RFC3339Nano),
			InferenceMS: &ms,
		},
	}
	writeJSON(w, http.StatusOK, res)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func floatPtr(v float64) *float64 { return &v }

func clamp01(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return math.Round(v*10000) / 10000
}

// --- Deterministic Title Normalization & Scoring ---

var (
	reBracketed     = regexp.MustCompile(`\[[^\]]*\]|\([^)]*\)`)
	reYear          = regexp.MustCompile(`\b(19\d{2}|20\d{2})\b`)
	reAnimeEpCut    = regexp.MustCompile(`(?i)\s+-\s+\d{1,4}(?:v\d+)?\b.*$`)
	reEpisodeCut    = regexp.MustCompile(`(?i)\b(s\d{1,2}e\d{1,3}|s\d{1,2}|season\s+\d+|ep?\s*\d{1,3}|v\d+[\.\s]\d+|build\s*\d+|2160p|1080p|720p|480p|4k|uhd|web-?dl|webrip|bluray|bdrip|remux|x264|x265|h\.?264|h\.?265|hevc|av1|flac|mp3|320kbps|epub|mobi|pdf|azw3|repack|proper|gog|rune|codex|fitgirl|elamigos|dodi)\b.*$`)
	reFileExt       = regexp.MustCompile(`(?i)\.(mkv|mp4|avi|mov|wmv|flac|mp3|m4b|epub|pdf|mobi|iso|zip|rar|7z)$`)
	reMultiSpace    = regexp.MustCompile(`\s+`)
	reDashReleaseGp = regexp.MustCompile(`-[A-Za-z0-9]{2,14}$`)
)

// NormalizeQuery extracts a clean searchable title and optional year from a
// processor job input.
func NormalizeQuery(in v1.Input) Query {
	q := Query{
		Title:       strings.TrimSpace(in.Deterministic.Title),
		Year:        in.Deterministic.Year,
		ContentType: strings.TrimSpace(in.Deterministic.ContentType),
	}
	if q.Title != "" {
		return q
	}
	raw := strings.TrimSpace(in.Name)
	raw = reFileExt.ReplaceAllString(raw, "")
	if q.Year == 0 {
		if m := reYear.FindStringSubmatch(raw); len(m) == 2 {
			q.Year, _ = strconv.Atoi(m[1])
		}
	}
	cleaned := reBracketed.ReplaceAllString(raw, " ")
	cleaned = strings.NewReplacer(".", " ", "_", " ").Replace(cleaned)
	cleaned = reAnimeEpCut.ReplaceAllString(cleaned, "")
	cleaned = reEpisodeCut.ReplaceAllString(cleaned, "")
	cleaned = reDashReleaseGp.ReplaceAllString(strings.TrimSpace(cleaned), "")
	cleaned = strings.Trim(reMultiSpace.ReplaceAllString(cleaned, " "), " -–—:")
	q.Title = cleaned
	return q
}

func rankExternalCandidates(q Query, candidates []ExternalCandidate) (bestIdx int, bestScore, secondScore float64) {
	bestIdx = -1
	for i, c := range candidates {
		score := scoreCandidate(q, c)
		if score > bestScore {
			secondScore = bestScore
			bestScore = score
			bestIdx = i
		} else if score > secondScore {
			secondScore = score
		}
	}
	return bestIdx, bestScore, secondScore
}

func scoreCandidate(q Query, c ExternalCandidate) float64 {
	bestText := titleSimilarity(q.Title, c.Title)
	for _, alias := range c.Aliases {
		if s := titleSimilarity(q.Title, alias); s > bestText {
			bestText = s
		}
	}
	score := bestText
	if q.Year > 0 && c.Year > 0 {
		diff := q.Year - c.Year
		if diff < 0 {
			diff = -diff
		}
		switch {
		case diff == 0:
			score += 0.12
		case diff == 1:
			score += 0.05
		case diff >= 3:
			score -= 0.18
		}
	}
	return clamp01(score)
}

func tokenize(s string) []string {
	var tokens []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return tokens
}

func titleSimilarity(a, b string) float64 {
	ta := tokenize(a)
	tb := tokenize(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	na := strings.Join(ta, " ")
	nb := strings.Join(tb, " ")
	if na == nb {
		return 0.95
	}
	setA := make(map[string]struct{}, len(ta))
	for _, t := range ta {
		setA[t] = struct{}{}
	}
	setB := make(map[string]struct{}, len(tb))
	for _, t := range tb {
		setB[t] = struct{}{}
	}
	inter := 0
	for t := range setA {
		if _, ok := setB[t]; ok {
			inter++
		}
	}
	union := len(setA) + len(setB) - inter
	if union == 0 {
		return 0
	}
	jaccard := float64(inter) / float64(union)
	// Containment bonus when a shorter query is an exact contiguous substring
	// of a longer official subtitle (e.g., "Frieren" -> "Frieren Beyond Journey s End").
	if inter == len(setA) && (strings.HasPrefix(nb, na+" ") || strings.Contains(nb, " "+na+" ") || strings.HasSuffix(nb, " "+na)) {
		ratio := float64(len(setA)) / float64(len(setB))
		containment := 0.65 + 0.25*ratio
		if containment > jaccard {
			return containment
		}
	}
	return jaccard
}
