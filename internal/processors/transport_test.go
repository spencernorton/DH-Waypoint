package processors

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
	"github.com/stretchr/testify/require"
)

func syntheticMultiCapJob(id, version string) v1.Job {
	return v1.Job{
		Binding: v1.Binding{
			Contract:            v1.Contract,
			JobID:               strings.Repeat("1", 64),
			InputRevision:       strings.Repeat("2", 64),
			PipelineRevision:    strings.Repeat("3", 64),
			ConfigurationDigest: strings.Repeat("4", 64),
			Processor:           v1.Identity{ID: id, Version: version, ModelVersion: "none", PolicyVersion: "1", ArtifactDigest: "none"},
			Stage:               v1.Classify,
		},
		Capabilities:  []v1.Stage{v1.Classify, v1.Policy},
		Configuration: json.RawMessage(`{}`),
		Input: v1.Input{
			InfoHash:      strings.Repeat("1", 40),
			Name:          "Synthetic.Film.2026.1080p.WEB-DL.mkv",
			Files:         []v1.File{},
			Candidates:    []v1.Candidate{},
			PriorResults:  []v1.Result{},
			Deterministic: v1.Deterministic{MatchingOutcome: "not_attempted", ProcessingOutcome: "completed"},
		},
		Deadline: time.Now().Add(5 * time.Second).UTC().Format(time.RFC3339Nano),
	}
}

func TestStrictFallbackDoesNotPoisonCacheOnTransientBadRequest(t *testing.T) {
	var expectedVersion atomic.Value
	expectedVersion.Store("2.0.0")
	var multiCapCalls, singleStageCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var j v1.Job
		require.NoError(t, json.NewDecoder(r.Body).Decode(&j))
		w.Header().Set("Content-Type", "application/json")
		if len(j.Capabilities) > 1 {
			multiCapCalls.Add(1)
		} else {
			singleStageCalls.Add(1)
		}
		if j.Processor.Version != expectedVersion.Load().(string) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"identity_mismatch"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(v1.Result{
			Binding:  j.Binding,
			Outcome:  v1.Completed,
			Proposal: &v1.Proposal{ContentType: "movie", AddTags: []string{"clean"}},
			Reasons:  []string{"clean"},
			Evidence: []v1.Evidence{},
			Trace:    json.RawMessage(`{"audio_language":"english","anime":false}`),
		})
	}))
	defer srv.Close()

	var fallbackEvents atomic.Int32
	tr := NewTransport(2 * time.Second)
	tr.onStrictFallback = func(string) { fallbackEvents.Add(1) }

	// Stale version triggers HTTP 400 on both multi-cap and strict probe (rolling upgrade window).
	_, err := tr.Process(testContext, srv.URL, "", syntheticMultiCapJob("modern-proc", "1.0.0"))
	require.EqualError(t, err, "service_unavailable")
	require.Equal(t, int32(0), fallbackEvents.Load(), "failed strict fallback must not record fallback metric")
	_, cached := tr.strictEndpoints.Load(srv.URL)
	require.False(t, cached, "transient HTTP 400 must not poison strictEndpoints cache")

	// Next job with refreshed version immediately runs single multi-cap call and preserves trace.
	beforeSingle := singleStageCalls.Load()
	res, err := tr.Process(testContext, srv.URL, "", syntheticMultiCapJob("modern-proc", "2.0.0"))
	require.NoError(t, err)
	require.Equal(t, v1.Completed, res.Outcome)
	require.Equal(t, int32(2), multiCapCalls.Load())
	require.Equal(t, beforeSingle, singleStageCalls.Load(), "must not issue single-stage strict calls after transient 400")
	require.JSONEq(t, `{"audio_language":"english","anime":false}`, string(res.Trace))
}

func TestStrictFallbackPreservesAndMergesTraceAndRequestFeedback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var j v1.Job
		require.NoError(t, json.NewDecoder(r.Body).Decode(&j))
		w.Header().Set("Content-Type", "application/json")
		if len(j.Capabilities) > 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"bad_request"}`))
			return
		}
		res := v1.Result{
			Binding:  j.Binding,
			Outcome:  v1.Completed,
			Reasons:  []string{"clean"},
			Evidence: []v1.Evidence{},
		}
		if j.Stage == v1.Classify {
			res.Proposal = &v1.Proposal{ContentType: "movie"}
			res.Trace = json.RawMessage(`{"audio_language":"english","anime":false,"neural_heads":{"category":"movie"}}`)
		} else {
			res.Proposal = &v1.Proposal{AddTags: []string{"clean"}}
			res.Trace = json.RawMessage(`{"audio_language":null,"policy_gate":"allow"}`)
			res.RequestFeedback = true
		}
		_ = json.NewEncoder(w).Encode(res)
	}))
	defer srv.Close()

	tr := NewTransport(2 * time.Second)
	res, err := tr.Process(testContext, srv.URL, "", syntheticMultiCapJob("strict-proc", "1.0.0"))
	require.NoError(t, err)
	require.Equal(t, v1.Completed, res.Outcome)
	require.True(t, res.RequestFeedback)
	require.JSONEq(t, `{"audio_language":"english","anime":false,"neural_heads":{"category":"movie"},"policy_gate":"allow"}`, string(res.Trace))
}

func TestStrictFallbackTraceBoundsPreservesPriorTrace(t *testing.T) {
	bigA := strings.Repeat("a", 50000)
	bigB := strings.Repeat("b", 30000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var j v1.Job
		require.NoError(t, json.NewDecoder(r.Body).Decode(&j))
		w.Header().Set("Content-Type", "application/json")
		if len(j.Capabilities) > 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"bad_request"}`))
			return
		}
		res := v1.Result{
			Binding:  j.Binding,
			Outcome:  v1.Completed,
			Reasons:  []string{},
			Evidence: []v1.Evidence{},
		}
		if j.Stage == v1.Classify {
			res.Proposal = &v1.Proposal{ContentType: "movie"}
			res.Trace, _ = json.Marshal(map[string]string{"audio_language": "english", "pad_a": bigA})
		} else {
			res.Proposal = &v1.Proposal{AddTags: []string{"clean"}}
			res.Trace, _ = json.Marshal(map[string]string{"pad_b": bigB})
		}
		_ = json.NewEncoder(w).Encode(res)
	}))
	defer srv.Close()

	tr := NewTransport(2 * time.Second)
	res, err := tr.Process(testContext, srv.URL, "", syntheticMultiCapJob("strict-proc", "1.0.0"))
	require.NoError(t, err)
	require.LessOrEqual(t, len(res.Trace), 65536)
	var trace map[string]string
	require.NoError(t, json.Unmarshal(res.Trace, &trace))
	require.Equal(t, "english", trace["audio_language"])
	require.Equal(t, bigA, trace["pad_a"])
}

func TestClearStrictEndpointAndEvictOnBadRequestRestoreMultiCapability(t *testing.T) {
	var mode atomic.Value
	mode.Store("strict") // "strict" -> "mismatch" -> "multi"
	var multiCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var j v1.Job
		require.NoError(t, json.NewDecoder(r.Body).Decode(&j))
		w.Header().Set("Content-Type", "application/json")
		cur := mode.Load().(string)
		if cur == "mismatch" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"identity_mismatch"}`))
			return
		}
		if cur == "strict" && len(j.Capabilities) > 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"bad_request"}`))
			return
		}
		if len(j.Capabilities) > 1 {
			multiCalls.Add(1)
		}
		prop := &v1.Proposal{ContentType: "movie", AddTags: []string{"clean"}}
		if len(j.Capabilities) <= 1 && j.Stage == v1.Classify {
			prop = &v1.Proposal{ContentType: "movie"}
		} else if len(j.Capabilities) <= 1 && j.Stage == v1.Policy {
			prop = &v1.Proposal{AddTags: []string{"clean"}}
		}
		_ = json.NewEncoder(w).Encode(v1.Result{
			Binding:  j.Binding,
			Outcome:  v1.Completed,
			Proposal: prop,
			Reasons:  []string{},
			Evidence: []v1.Evidence{},
		})
	}))
	defer srv.Close()

	tr := NewTransport(2 * time.Second)
	tr.strictTTL = 15 * time.Minute

	// 1. Strict server caches endpoint.
	_, err := tr.Process(testContext, srv.URL, "", syntheticMultiCapJob("proc", "1.0.0"))
	require.NoError(t, err)
	_, cached := tr.strictEndpoints.Load(srv.URL)
	require.True(t, cached)

	// 2. Explicit ClearStrictEndpoint (called on registry refresh) immediately restores multi-cap.
	mode.Store("multi")
	tr.ClearStrictEndpoint(srv.URL)
	_, err = tr.Process(testContext, srv.URL, "", syntheticMultiCapJob("proc", "2.0.0"))
	require.NoError(t, err)
	require.Equal(t, int32(1), multiCalls.Load())

	// 3. Re-enter strict cache, then fail with HTTP 400 on cached strict call -> cache evicted.
	mode.Store("strict")
	_, err = tr.Process(testContext, srv.URL, "", syntheticMultiCapJob("proc", "2.0.0"))
	require.NoError(t, err)
	_, cached = tr.strictEndpoints.Load(srv.URL)
	require.True(t, cached)

	mode.Store("mismatch")
	_, err = tr.Process(testContext, srv.URL, "", syntheticMultiCapJob("proc", "2.0.0"))
	require.EqualError(t, err, "service_unavailable")
	_, cached = tr.strictEndpoints.Load(srv.URL)
	require.False(t, cached, "HTTP 400 on cached strict endpoint must evict stale cache entry")

	mode.Store("multi")
	_, err = tr.Process(testContext, srv.URL, "", syntheticMultiCapJob("proc", "3.0.0"))
	require.NoError(t, err)
	require.Equal(t, int32(2), multiCalls.Load())
}
