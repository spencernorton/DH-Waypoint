package llm

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"slices"
	"sync"
	"time"

	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
)

type cachedResult struct {
	key    string
	result v1.Result
}

// Service owns provider I/O, process-lifetime budgets and optional feedback
// receipts. It never connects to a DH-Waypoint database.
type Service struct {
	cfg      Config
	manifest v1.Manifest
	prompt   string
	apiKey   string
	token    string
	client   *http.Client
	started  string
	instance string
	slots    chan struct{}
	mu       sync.Mutex
	calls    int64
	window   time.Time
	windowN  int
	attempts int64
	outcomes map[v1.Outcome]int64
	active   map[string]bool
	cache    map[string]cachedResult
	order    []string
	feedback sync.Mutex
}

// New validates configuration and credentials without contacting a provider.
// Readiness means the adapter is configured, not that an upstream is healthy.
func New(c Config) (*Service, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	s := &Service{cfg: c, prompt: defaultPrompt, slots: make(chan struct{}, c.Concurrency),
		outcomes: map[v1.Outcome]int64{}, cache: map[string]cachedResult{}, active: map[string]bool{}, started: time.Now().UTC().Format(time.RFC3339Nano)}
	if c.SystemPromptFile != "" {
		b, err := readBoundedFile(c.SystemPromptFile, 16384)
		if err != nil || len(b) == 0 {
			return nil, errors.New("prompt_unavailable")
		}
		s.prompt = string(b)
	}
	var err error
	if os.Getenv(c.ProcessorTokenEnv) != "" {
		s.token, err = secret(c.ProcessorTokenEnv, "")
		if err != nil {
			return nil, err
		}
	}
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return nil, errors.New("invalid_listen_address")
	}
	ip := net.ParseIP(host)
	if s.token == "" && host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, errors.New("processor_token_required_for_network_listen")
	}
	if c.EnableInference && !c.DryRun {
		s.apiKey, err = secret(c.APIKeyEnv, c.APIKeyFile)
		if err != nil {
			return nil, err
		}
		if s.token != "" && s.token == s.apiKey {
			return nil, errors.New("processor_and_provider_credentials_must_differ")
		}
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, errors.New("instance_id_unavailable")
	}
	s.instance = hex.EncodeToString(id[:])
	// Bind behavior to an immutable identity without publishing endpoints,
	// prompts or secrets through the manifest/status/trace.
	policy := fingerprint([]any{c.Capabilities, c.Endpoint, c.Model, s.prompt, c.JSONMode, c.Temperature,
		c.TokenLimitField, c.MaxOutputTokens, c.MaxInputBytes, c.EnableInference, c.DryRun, c.FeedbackFile != ""})
	s.manifest = v1.Manifest{Contract: v1.Contract, Processor: v1.Identity{ID: c.ID, Version: "1.0.0",
		ModelVersion: "model-" + fingerprint(c.Model)[:16], PolicyVersion: "policy-" + policy[:32], ArtifactDigest: "none"},
		Capabilities: slices.Clone(c.Capabilities), Qualification: "unqualified",
		ConfigurationSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`)}
	if err := s.manifest.Validate(); err != nil {
		return nil, err
	}
	s.client = &http.Client{Timeout: c.timeout(), CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("provider_redirect_forbidden")
	}, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, DisableCompression: true,
		MaxConnsPerHost: c.Concurrency, MaxIdleConnsPerHost: c.Concurrency, IdleConnTimeout: 30 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: c.timeout()}}
	return s, nil
}

// Handler serves only the versioned public processor endpoints.
func (s *Service) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

// Close releases idle provider connections after the server has shut down.
func (s *Service) Close() { s.client.CloseIdleConnections() }

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *Service) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if s.token != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.token)) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if r.URL.Path == "/v1/manifest" || r.URL.Path == "/v1/status" {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
			return
		}
		if r.URL.Path == "/v1/manifest" {
			writeJSON(w, http.StatusOK, s.manifest)
			return
		}
		s.mu.Lock()
		n := s.attempts
		outcomes := make(map[v1.Outcome]int64, len(s.outcomes))
		for k, v := range s.outcomes {
			outcomes[k] = v
		}
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, v1.ServiceStatus{Contract: v1.Contract, Processor: s.manifest.Processor,
			State: "ready", InstanceID: s.instance, StartedAt: s.started, Attempts: &n, Outcomes: outcomes})
		return
	}
	if r.URL.Path != "/v1/process" && r.URL.Path != "/v1/feedback" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if media != "application/json" {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "invalid_content_type"})
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, v1.MaxBody))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "body_limit"})
		return
	}
	if r.URL.Path == "/v1/feedback" {
		s.receiveFeedback(w, raw)
		return
	}
	var j v1.Job
	var cfg map[string]json.RawMessage
	if err := v1.Decode(raw, &j); err != nil || j.Validate() != nil || j.Processor != s.manifest.Processor ||
		json.Unmarshal(j.Configuration, &cfg) != nil || cfg == nil || len(cfg) != 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "invalid_job"})
		return
	}
	for _, cap := range j.EffectiveCapabilities() {
		if !slices.Contains(s.manifest.Capabilities, cap) {
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
	ctx, cancelTimeout := context.WithTimeout(ctx, s.cfg.timeout())
	defer cancelTimeout()
	result := s.process(ctx, j)
	writeJSON(w, http.StatusOK, result)
}

func resultFor(j v1.Job, outcome v1.Outcome, reason string) v1.Result {
	r := v1.Result{Binding: j.Binding, Outcome: outcome, Reasons: []string{reason}, Evidence: []v1.Evidence{}}
	if outcome == v1.Deferred {
		r.RetryAfterSeconds = 60
	}
	return r
}

func (s *Service) process(ctx context.Context, j v1.Job) (result v1.Result) {
	keyJob := j
	keyJob.Deadline = "" // retries carry a fresh attempt deadline
	key := fingerprint(keyJob)
	s.mu.Lock()
	if cached, ok := s.cache[j.JobID]; ok {
		s.mu.Unlock()
		if cached.key != key {
			return resultFor(j, v1.Failed, "job_id_reused")
		}
		return cached.result
	}
	if s.active[j.JobID] {
		s.mu.Unlock()
		return resultFor(j, v1.Deferred, "job_in_progress")
	}
	s.active[j.JobID] = true
	s.attempts++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.active, j.JobID)
		s.outcomes[result.Outcome]++
		s.mu.Unlock()
	}()
	if !s.cfg.EnableInference || s.cfg.DryRun {
		result = resultFor(j, v1.Abstained, "inference_disabled_or_dry_run")
	} else {
		s.mu.Lock()
		now := time.Now()
		if now.Sub(s.window) >= time.Minute {
			s.window = now
			s.windowN = 0
		}
		if s.calls >= s.cfg.MaxRequests {
			s.mu.Unlock()
			return resultFor(j, v1.Failed, "provider_request_budget_exhausted")
		}
		if s.windowN >= s.cfg.RequestsPerMinute {
			s.mu.Unlock()
			return resultFor(j, v1.Deferred, "provider_rate_budget_exhausted")
		}
		s.calls++ // failures and canceled requests consume budget too
		s.windowN++
		s.mu.Unlock()
		start := time.Now()
		var err error
		result, err = s.infer(ctx, j)
		if err != nil {
			return resultFor(j, v1.Failed, err.Error())
		}
		if ctx.Err() != nil {
			return resultFor(j, v1.Failed, "provider_deadline_expired")
		}
		ms := float64(time.Since(start).Microseconds()) / 1000
		result.Telemetry = v1.Telemetry{ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), InferenceMS: &ms}
	}
	result.RequestFeedback = s.cfg.FeedbackFile != ""
	s.mu.Lock()
	if len(s.order) >= s.cfg.ResultCacheEntries {
		delete(s.cache, s.order[0])
		s.order = s.order[1:]
	}
	s.cache[j.JobID] = cachedResult{key, result}
	s.order = append(s.order, j.JobID)
	s.mu.Unlock()
	return result
}

func (s *Service) receiveFeedback(w http.ResponseWriter, raw []byte) {
	if s.cfg.FeedbackFile == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "feedback_not_configured"})
		return
	}
	var batch v1.FeedbackBatch
	if err := v1.Decode(raw, &batch); err != nil || batch.Contract != v1.FeedbackContract || batch.Processor != s.manifest.Processor || len(batch.Events) < 1 || len(batch.Events) > 32 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "invalid_feedback"})
		return
	}
	for _, ev := range batch.Events {
		_, hashErr := hex.DecodeString(ev.EventID)
		if ev.ProcessorID != s.cfg.ID || len(ev.EventID) != 64 || hashErr != nil || !slices.Contains(v1.FeedbackTiers, ev.Tier) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "invalid_feedback"})
			return
		}
	}
	// Acknowledge only after durable append. Delivery is at least once: offline
	// consumers must deduplicate event_id. No learning or provider call occurs here.
	line, _ := json.Marshal(batch)
	line = append(line, '\n')
	s.feedback.Lock()
	defer s.feedback.Unlock()
	f, err := os.OpenFile(s.cfg.FeedbackFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err == nil {
		defer f.Close()
		var st os.FileInfo
		st, err = f.Stat()
		if err == nil && st.Size()+int64(len(line)) > s.cfg.MaxFeedbackBytes {
			err = errors.New("feedback_capacity")
		}
		if err == nil {
			_, err = f.Write(line)
		}
		if err == nil {
			err = f.Sync()
		}
	}
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "feedback_storage_unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, v1.FeedbackAck{Contract: v1.FeedbackContract, Accepted: len(batch.Events)})
}
