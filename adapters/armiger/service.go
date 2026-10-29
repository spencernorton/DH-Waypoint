// Package armiger is a standalone processor service. It answers
// dh-waypoint.processor.v1 classify jobs by asking an Armiger Ultra container
// (POST /v1/classify). The backend must never import this package: the only
// shared dependency is processorapi/v1.
package armiger

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
)

// Config is service-local; none of it is sent to the backend.
type Config struct {
	Listen         string        // address the backend calls
	ArmigerURL     string        // base URL of the Armiger Ultra container
	ArmigerVersion string        // reported as the manifest model version
	Token          string        // bearer token the backend must send; required off loopback
	MinConfidence  float64       // a category below this confidence abstains
	Timeout        time.Duration // per Armiger request
	Concurrency    int
}

// DefaultConfig binds only to loopback and expects Armiger on the Compose network.
func DefaultConfig() Config {
	return Config{Listen: "127.0.0.1:8095", ArmigerURL: "http://armiger-ultra:8096", ArmigerVersion: "unknown",
		MinConfidence: 0.6, Timeout: 15 * time.Second, Concurrency: 2}
}

type Service struct {
	cfg      Config
	manifest v1.Manifest
	instance string
	started  string
	client   *http.Client
	slots    chan struct{}
	attempts atomic.Int64
}

func New(c Config) (*Service, error) {
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return nil, errors.New("invalid_listen_address")
	}
	if ip := net.ParseIP(host); c.Token == "" && host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, errors.New("processor_token_required_for_network_listen")
	}
	u, err := url.Parse(c.ArmigerURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid_armiger_url")
	}
	if c.MinConfidence < 0 || c.MinConfidence > 1 || c.Concurrency < 1 || c.Concurrency > 16 || c.Timeout <= 0 || c.Timeout > time.Minute {
		return nil, errors.New("invalid_service_bounds")
	}
	id := v1.Identity{ID: "armiger-ultra", Version: "1.0.0", ModelVersion: "armiger-" + c.ArmigerVersion,
		PolicyVersion: fmt.Sprintf("min-confidence-%.2f", c.MinConfidence), ArtifactDigest: "none"}
	if id.Validate() != nil {
		return nil, errors.New("invalid_armiger_version")
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, errors.New("instance_id_unavailable")
	}
	return &Service{cfg: c, instance: hex.EncodeToString(b[:]), started: time.Now().UTC().Format(time.RFC3339Nano),
		manifest: v1.Manifest{Contract: v1.Contract, Processor: id, Capabilities: []v1.Stage{v1.Classify},
			ConfigurationSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`), Qualification: "unqualified"},
		// Never follow redirects away from the configured Armiger container.
		client: &http.Client{Timeout: c.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		slots:  make(chan struct{}, c.Concurrency)}, nil
}

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
			state := "ready"
			if !s.armigerHealthy(r.Context()) {
				state = "unavailable"
			}
			writeJSON(w, http.StatusOK, v1.ServiceStatus{Contract: v1.Contract, Processor: s.manifest.Processor, State: state,
				InstanceID: s.instance, StartedAt: s.started, Attempts: &n})
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
		j.Processor != s.manifest.Processor || j.Stage != v1.Classify || strings.TrimSpace(string(j.Configuration)) != "{}" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "invalid_job"})
		return
	}
	for _, cap := range j.EffectiveCapabilities() {
		if cap != v1.Classify {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "unsupported_capability"})
			return
		}
	}
	deadline, _ := time.Parse(time.RFC3339Nano, j.Deadline)
	if !time.Now().Before(deadline) {
		writeJSON(w, http.StatusRequestTimeout, map[string]string{"error": "deadline_expired"})
		return
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		writeJSON(w, http.StatusOK, resultFor(j, v1.Deferred, "processor_busy"))
		return
	}
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	s.attempts.Add(1)
	writeJSON(w, http.StatusOK, s.classify(ctx, j))
}

type armigerFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

type armigerRequest struct {
	Name  string        `json:"name"`
	Size  int64         `json:"size"`
	Files []armigerFile `json:"files"`
}

type armigerResponse struct {
	Junk  string `json:"junk"`
	Heads struct {
		Category struct {
			Label      string  `json:"label"`
			Confidence float64 `json:"confidence"`
		} `json:"category"`
		Junk struct {
			Label string `json:"label"`
		} `json:"junk"`
	} `json:"heads"`
	HoneypotTrap struct {
		IsHoneypotTrap bool `json:"is_honeypot_trap"`
	} `json:"honeypot_trap"`
	CSAMSafety struct {
		IsSafe *bool `json:"is_safe"`
	} `json:"csam_safety"`
}

// classify maps Armiger's category head onto one classify proposal. Labels the
// backend has no content type for (course, unknown), unsafe/junk items, and low
// confidence abstain.
func (s *Service) classify(ctx context.Context, j v1.Job) v1.Result {
	in := armigerRequest{Name: j.Input.Name, Size: j.Input.Size, Files: []armigerFile{}}
	for _, f := range j.Input.Files {
		in.Files = append(in.Files, armigerFile{Path: f.Path, Size: f.Size})
	}
	if len(in.Files) == 0 && j.Input.Extension != nil && strings.TrimSpace(*j.Input.Extension) != "" {
		ext := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(*j.Input.Extension)), ".")
		path := j.Input.Name
		if ext != "" && !strings.HasSuffix(strings.ToLower(path), "."+ext) {
			path = path + "." + ext
		}
		in.Files = append(in.Files, armigerFile{Path: path, Size: j.Input.Size})
	}
	body, err := json.Marshal(in)
	if err != nil || len(body) > 1<<20 {
		return resultFor(j, v1.Abstained, "input_too_large")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.cfg.ArmigerURL, "/")+"/v1/classify", bytes.NewReader(body))
	if err != nil {
		return resultFor(j, v1.Failed, "armiger_request_invalid")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return resultFor(j, v1.Deferred, "armiger_unavailable")
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusBadRequest:
		return resultFor(j, v1.Abstained, "armiger_rejected_metadata")
	case resp.StatusCode != http.StatusOK:
		return resultFor(j, v1.Deferred, "armiger_unavailable")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	var out armigerResponse
	if err != nil || len(raw) > 1<<20 || json.Unmarshal(raw, &out) != nil {
		return resultFor(j, v1.Failed, "armiger_response_invalid")
	}
	if (out.CSAMSafety.IsSafe != nil && !*out.CSAMSafety.IsSafe) ||
		out.HoneypotTrap.IsHoneypotTrap ||
		out.Junk == "junk" ||
		out.Heads.Junk.Label == "junk" {
		return resultFor(j, v1.Abstained, "unsafe_or_junk")
	}
	label, confidence := out.Heads.Category.Label, out.Heads.Category.Confidence
	if !slices.Contains(v1.ValidContentTypes, label) {
		return resultFor(j, v1.Abstained, "category_outside_content_types")
	}
	if confidence < s.cfg.MinConfidence || confidence > 1 {
		return resultFor(j, v1.Abstained, "low_confidence")
	}
	u := 1 - confidence
	r := resultFor(j, v1.Completed, "armiger_category")
	r.Proposal, r.Uncertainty = &v1.Proposal{ContentType: label}, &u
	return r
}

func (s *Service) armigerHealthy(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.cfg.ArmigerURL, "/")+"/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func resultFor(j v1.Job, outcome v1.Outcome, reason string) v1.Result {
	r := v1.Result{Binding: j.Binding, Outcome: outcome, Reasons: []string{reason}, Evidence: []v1.Evidence{}}
	if outcome == v1.Deferred {
		r.RetryAfterSeconds = 30
	}
	return r
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
