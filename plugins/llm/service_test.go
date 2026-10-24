package llm

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
)

const completion = `{"outcome":"completed","proposal":{"content_type":"movie"},"uncertainty":0.1,"reasons":["synthetic_fixture"]}`

func providerReply(w http.ResponseWriter, content, finish string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": "fixture", "usage": map[string]int{"total_tokens": 10},
		"choices": []any{map[string]any{"finish_reason": finish, "message": map[string]any{"content": content, "refusal": nil}}}})
}

func testService(t *testing.T, handler http.HandlerFunc, configure func(*Config)) *Service {
	t.Helper()
	t.Setenv("LLM_PROCESSOR_TOKEN", "")
	provider := httptest.NewServer(handler)
	t.Cleanup(provider.Close)
	c := DefaultConfig()
	c.Endpoint, c.Model, c.Capabilities = provider.URL+"/v1/chat/completions", "synthetic-model", []v1.Stage{v1.Classify}
	c.EnableInference, c.DryRun = true, false
	if configure != nil {
		configure(&c)
	}
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func testJob(s *Service) v1.Job {
	return v1.Job{Binding: v1.Binding{Contract: v1.Contract, JobID: strings.Repeat("a", 64),
		InputRevision: strings.Repeat("b", 64), PipelineRevision: strings.Repeat("c", 64),
		ConfigurationDigest: strings.Repeat("d", 64), Processor: s.manifest.Processor, Stage: v1.Classify},
		Configuration: json.RawMessage(`{}`), Deadline: time.Now().Add(2 * time.Second).UTC().Format(time.RFC3339Nano),
		Input: v1.Input{InfoHash: strings.Repeat("e", 40), Name: "Synthetic.Film.2026.mkv", Size: 100,
			Deterministic: v1.Deterministic{ProcessingOutcome: "completed", MatchingOutcome: "completed_unmatched"}}}
}

func sendJob(t *testing.T, s *Service, j v1.Job) (int, v1.Result) {
	t.Helper()
	b, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/process", bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	if s.token != "" {
		r.Header.Set("Authorization", "Bearer "+s.token)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	var result v1.Result
	if w.Code == 200 {
		if err := v1.Decode(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if err := result.Validate(j); err != nil {
			t.Fatalf("invalid adapter result: %v %s", err, w.Body)
		}
	}
	return w.Code, result
}

func TestInferenceIsTwoStageOptIn(t *testing.T) {
	for _, flags := range [][2]bool{{false, false}, {false, true}, {true, true}} {
		t.Run(stringBool(flags[0])+"-"+stringBool(flags[1]), func(t *testing.T) {
			var calls atomic.Int64
			s := testService(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); providerReply(w, completion, "stop") }, func(c *Config) {
				c.EnableInference, c.DryRun = flags[0], flags[1]
			})
			code, result := sendJob(t, s, testJob(s))
			if code != 200 || result.Outcome != v1.Abstained || calls.Load() != 0 {
				t.Fatalf("dry run made inference: %d %+v calls=%d", code, result, calls.Load())
			}
		})
	}
}

func stringBool(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func TestProviderWireAndCredentialSeparation(t *testing.T) {
	for _, model := range []string{"gpt-5", "gpt-5-mini", "openai/gpt-5.6", "synthetic-model"} {
		t.Run(model, func(t *testing.T) {
			t.Setenv("TEST_PROVIDER_KEY", "synthetic-provider-key")
			t.Setenv("TEST_PROCESSOR_KEY", "synthetic-processor-key")
			var payload map[string]json.RawMessage
			var auth, path string
			s := testService(t, func(w http.ResponseWriter, r *http.Request) {
				auth, path = r.Header.Get("Authorization"), r.URL.Path
				_ = json.NewDecoder(r.Body).Decode(&payload)
				providerReply(w, completion, "stop")
			}, func(c *Config) {
				c.Model, c.APIKeyEnv, c.ProcessorTokenEnv = model, "TEST_PROVIDER_KEY", "TEST_PROCESSOR_KEY"
				c.TokenLimitField, c.JSONMode = "max_completion_tokens", true
				temp := 0.2
				c.Temperature = &temp
			})
			j := testJob(s)
			code, result := sendJob(t, s, j)
			if code != 200 || result.Outcome != v1.Completed || result.Binding != j.Binding {
				t.Fatalf("unexpected result: %d %+v", code, result)
			}
			if auth != "Bearer synthetic-provider-key" || path != "/v1/chat/completions" {
				t.Fatalf("provider request: %s %s", auth, path)
			}
			_, temperature := payload["temperature"]
			if temperature != (model == "synthetic-model") {
				t.Fatalf("temperature handling for %s: %s", model, payload["temperature"])
			}
			if payload["max_completion_tokens"] == nil || payload["max_tokens"] != nil || payload["response_format"] == nil {
				t.Fatalf("provider dialect: %v", payload)
			}
			manifest, _ := json.Marshal(s.manifest)
			if strings.Contains(string(manifest), "synthetic-provider-key") || strings.Contains(string(manifest), "synthetic-processor-key") {
				t.Fatal("manifest leaked a secret")
			}
		})
	}
}

func TestStrictModelOutputAndCapabilityBoundary(t *testing.T) {
	for name, output := range map[string]string{
		"unknown_field":       `{"outcome":"completed","proposal":{"content_type":"movie"},"binding":{}}`,
		"duplicate":           `{"outcome":"completed","outcome":"abstained"}`,
		"invalid_type":        `{"outcome":"completed","proposal":{"content_type":"invented"}}`,
		"wrong_capability":    `{"outcome":"completed","proposal":{"content_type":"movie","extraction":{"title":"Invented"}}}`,
		"invented_match":      `{"outcome":"completed","proposal":{"match":{"source":"tmdb","id":"999"}}}`,
		"policy_injection":    `{"outcome":"completed","proposal":{"content_type":"movie","add_tags":["trusted"]}}`,
		"unexpected_proposal": `{"outcome":"abstained","proposal":{"content_type":"movie"}}`,
		"non_json":            "```json\n" + completion + "\n```",
	} {
		t.Run(name, func(t *testing.T) {
			s := testService(t, func(w http.ResponseWriter, r *http.Request) { providerReply(w, output, "stop") }, nil)
			code, result := sendJob(t, s, testJob(s))
			if code != 200 || result.Outcome != v1.Failed || result.Proposal != nil {
				t.Fatalf("model output escaped validation: %d %+v", code, result)
			}
		})
	}
}

func TestUnsolicitedMatchRejectedWithMultipleCapabilities(t *testing.T) {
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		providerReply(w, `{"outcome":"completed","proposal":{"content_type":"movie","match":{"source":"tmdb","id":"999"}}}`, "stop")
	}, func(c *Config) { c.Capabilities = []v1.Stage{v1.Classify, v1.Match} })
	j := testJob(s)
	j.Capabilities = s.cfg.Capabilities
	_, result := sendJob(t, s, j)
	if result.Outcome != v1.Failed {
		t.Fatalf("unoffered candidate was accepted: %+v", result)
	}
}

func TestProviderFailureModes(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"http_error": func(w http.ResponseWriter, r *http.Request) { http.Error(w, "secret provider error", 429) },
		"truncated":  func(w http.ResponseWriter, r *http.Request) { providerReply(w, completion, "length") },
		"oversize": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, strings.Repeat(" ", v1.MaxBody+1))
		},
		"html": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, completion)
		},
		"refusal": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"stop","message":{"refusal":"no","content":""}}]}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := testService(t, handler, nil)
			_, result := sendJob(t, s, testJob(s))
			b, _ := json.Marshal(result)
			if result.Outcome != v1.Failed || strings.Contains(string(b), "secret provider error") {
				t.Fatalf("bad failure: %s", b)
			}
		})
	}
}

func TestProviderRedirectDoesNotForwardCredentials(t *testing.T) {
	var calls atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer other.Close()
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}, nil)
	_, result := sendJob(t, s, testJob(s))
	if result.Outcome != v1.Failed || calls.Load() != 0 {
		t.Fatalf("redirect followed: %+v calls=%d", result, calls.Load())
	}
}

func TestDeadlineCancelsProvider(t *testing.T) {
	canceled := make(chan struct{})
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-time.After(time.Second):
		}
	}, nil)
	j := testJob(s)
	j.Deadline = time.Now().Add(100 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	_, result := sendJob(t, s, j)
	if result.Outcome != v1.Failed {
		t.Fatalf("deadline returned success: %+v", result)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("provider request was not canceled")
	}
}

func TestRetryCacheAndBudget(t *testing.T) {
	var calls atomic.Int64
	s := testService(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); providerReply(w, completion, "stop") }, func(c *Config) { c.MaxRequests = 1 })
	j := testJob(s)
	_, first := sendJob(t, s, j)
	j.Deadline = time.Now().Add(3 * time.Second).UTC().Format(time.RFC3339Nano)
	_, retry := sendJob(t, s, j)
	if first.Outcome != v1.Completed || retry.Outcome != v1.Completed || calls.Load() != 1 {
		t.Fatalf("retry charged twice: %d", calls.Load())
	}
	j.Input.Name = "changed source"
	_, changed := sendJob(t, s, j)
	if changed.Outcome != v1.Failed || changed.Reasons[0] != "job_id_reused" {
		t.Fatalf("reused id accepted: %+v", changed)
	}
	j.JobID = strings.Repeat("f", 64)
	_, exhausted := sendJob(t, s, j)
	if exhausted.Outcome != v1.Failed || calls.Load() != 1 {
		t.Fatal("request cap did not hold")
	}
}

func TestParallelRetryDoesNotDoubleCharge(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		close(entered)
		<-release
		providerReply(w, completion, "stop")
	}, func(c *Config) { c.Concurrency = 2 })
	j := testJob(s)
	done := make(chan struct{})
	go func() { defer close(done); _, _ = sendJob(t, s, j) }()
	<-entered
	_, retry := sendJob(t, s, j)
	close(release)
	<-done
	if retry.Outcome != v1.Deferred || calls.Load() != 1 {
		t.Fatalf("parallel retry duplicated inference: %+v %d", retry, calls.Load())
	}
}

func TestJobAndTransportValidation(t *testing.T) {
	var calls atomic.Int64
	s := testService(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); providerReply(w, completion, "stop") }, nil)
	for _, change := range []func(*v1.Job){
		func(j *v1.Job) { j.Deadline = time.Now().Add(-time.Second).Format(time.RFC3339Nano) },
		func(j *v1.Job) { j.Processor.ModelVersion = "other" },
		func(j *v1.Job) { j.Configuration = json.RawMessage(`{"model":"override"}`) },
		func(j *v1.Job) { j.Capabilities = []v1.Stage{v1.Policy} },
	} {
		j := testJob(s)
		change(&j)
		code, _ := sendJob(t, s, j)
		if code < 400 {
			t.Fatalf("invalid job accepted: %+v", j)
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/process", strings.NewReader(strings.Repeat(" ", v1.MaxBody+1)))
	r.Header.Set("Content-Type", "application/json")
	s.Handler().ServeHTTP(w, r)
	if w.Code != 413 || calls.Load() != 0 {
		t.Fatalf("unbounded body: code=%d calls=%d", w.Code, calls.Load())
	}
}

func TestFeedbackDurableAckAndCapacity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feedback.jsonl")
	s := testService(t, func(w http.ResponseWriter, r *http.Request) { t.Error("feedback called provider") }, func(c *Config) { c.FeedbackFile = path; c.DryRun = true; c.MaxFeedbackBytes = 1024 })
	batch := v1.FeedbackBatch{Contract: v1.FeedbackContract, Processor: s.manifest.Processor,
		Events: []v1.FeedbackEvent{{EventID: strings.Repeat("a", 64), ProcessorID: s.cfg.ID, Tier: v1.FeedbackTierOperationalReceipt, GroundTruth: v1.FeedbackGroundTruth{Outcome: "applied"}}}}
	raw, _ := json.Marshal(batch)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/feedback", bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("feedback: %d %s", w.Code, w.Body)
	}
	b, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(b, []byte(batch.Events[0].EventID)) {
		t.Fatalf("ack before append: %v %s", err, b)
	}
	batch.Events[0].GroundTruth.Reason = strings.Repeat("x", 2000)
	raw, _ = json.Marshal(batch)
	w = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/v1/feedback", bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	s.Handler().ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatal("full feedback spool was acknowledged")
	}
}

func TestSafeStartupAndIdentity(t *testing.T) {
	c := DefaultConfig()
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	changed := c
	changed.MaxOutputTokens++
	other, err := New(changed)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if s.manifest.Processor == other.manifest.Processor {
		t.Fatal("semantic config changed without identity change")
	}
	c.Listen = "0.0.0.0:8091"
	if _, err := New(c); err == nil {
		t.Fatal("network listener accepted without service token")
	}
	c = DefaultConfig()
	c.EnableInference, c.DryRun = true, false
	c.Endpoint = "https://user:secret@example.invalid/v1/chat/completions"
	c.Model = "fixture"
	if _, err := New(c); err == nil {
		t.Fatal("credential-bearing provider URL accepted")
	}
}

func TestRateBudgetAndInputCap(t *testing.T) {
	var calls atomic.Int64
	s := testService(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); providerReply(w, completion, "stop") }, func(c *Config) { c.RequestsPerMinute = 1 })
	j := testJob(s)
	_, result := sendJob(t, s, j)
	if result.Outcome != v1.Completed {
		t.Fatal(result)
	}
	j.JobID = strings.Repeat("f", 64)
	_, result = sendJob(t, s, j)
	if result.Outcome != v1.Deferred || calls.Load() != 1 {
		t.Fatalf("minute budget exceeded: %+v calls=%d", result, calls.Load())
	}
	s = testService(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }, func(c *Config) { c.MaxInputBytes = 256 })
	j = testJob(s)
	j.Input.Name = strings.Repeat("x", 4096)
	_, result = sendJob(t, s, j)
	if result.Outcome != v1.Failed || calls.Load() != 1 {
		t.Fatal("oversized input reached provider")
	}
}

func TestServiceAuthenticationAndContentType(t *testing.T) {
	t.Setenv("TEST_SERVICE_TOKEN", "synthetic-service-token")
	s := testService(t, func(w http.ResponseWriter, r *http.Request) { t.Error("invalid request reached provider") }, func(c *Config) { c.ProcessorTokenEnv = "TEST_SERVICE_TOKEN" })
	for _, path := range []string{"/v1/manifest", "/v1/status", "/v1/process", "/v1/feedback"} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(`{}`)))
		if w.Code != 401 {
			t.Fatalf("unauthenticated %s: %d", path, w.Code)
		}
	}
	r := httptest.NewRequest("POST", "/v1/process", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer synthetic-service-token")
	r.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 415 {
		t.Fatalf("unexpected content type accepted: %d", w.Code)
	}
}

func TestExampleServiceConfigurationsStayDisabled(t *testing.T) {
	for _, path := range []string{"local.example.json", "hosted.example.json"} {
		c, err := LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if c.EnableInference || !c.DryRun {
			t.Fatalf("shipped config enables inference: %s", path)
		}
		s, err := New(c)
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
}
