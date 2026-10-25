package armiger

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
)

func testJob(s *Service) v1.Job {
	return v1.Job{Binding: v1.Binding{Contract: v1.Contract, JobID: strings.Repeat("a", 64),
		InputRevision: strings.Repeat("b", 64), PipelineRevision: strings.Repeat("c", 64),
		ConfigurationDigest: strings.Repeat("d", 64), Processor: s.manifest.Processor, Stage: v1.Classify},
		Configuration: json.RawMessage(`{}`), Deadline: time.Now().Add(5 * time.Second).UTC().Format(time.RFC3339Nano),
		Input: v1.Input{InfoHash: strings.Repeat("e", 40), Name: "Synthetic.Show.S01E02.1080p.WEB-DL.mkv", Size: 100,
			Files:         []v1.File{{Path: "Synthetic.Show.S01E02.1080p.WEB-DL.mkv", Size: 100}},
			Deterministic: v1.Deterministic{ProcessingOutcome: "completed", MatchingOutcome: "not_attempted"}}}
}

func post(t *testing.T, s *Service, j v1.Job) (int, v1.Result) {
	t.Helper()
	raw, _ := json.Marshal(j)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/process", bytes.NewReader(raw)))
	var r v1.Result
	_ = json.Unmarshal(rec.Body.Bytes(), &r)
	return rec.Code, r
}

func TestClassifyMapsArmigerCategory(t *testing.T) {
	for _, tc := range []struct {
		name, reply string
		status      int
		outcome     v1.Outcome
		reason      string
		contentType string
	}{
		{"confident", `{"category":"tv_show","heads":{"category":{"label":"tv_show","confidence":0.99}}}`, 200, v1.Completed, "armiger_category", "tv_show"},
		{"no content type", `{"heads":{"category":{"label":"course","confidence":0.97}}}`, 200, v1.Abstained, "category_outside_content_types", ""},
		{"low confidence", `{"heads":{"category":{"label":"movie","confidence":0.41}}}`, 200, v1.Abstained, "low_confidence", ""},
		{"rejected metadata", `{"error":"invalid_metadata"}`, 400, v1.Abstained, "armiger_rejected_metadata", ""},
		{"not ready", `{"error":"classification_failed"}`, 500, v1.Deferred, "armiger_unavailable", ""},
		{"garbled", `not json`, 200, v1.Failed, "armiger_response_invalid", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got armigerRequest
			armiger := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/classify" || json.NewDecoder(r.Body).Decode(&got) != nil {
					t.Errorf("unexpected Armiger request %s", r.URL.Path)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.reply))
			}))
			defer armiger.Close()
			cfg := DefaultConfig()
			cfg.ArmigerURL = armiger.URL
			s, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			j := testJob(s)
			code, r := post(t, s, j)
			if code != 200 || r.Outcome != tc.outcome || len(r.Reasons) != 1 || r.Reasons[0] != tc.reason {
				t.Fatalf("got %d %s %v", code, r.Outcome, r.Reasons)
			}
			if got.Name != j.Input.Name || len(got.Files) != 1 {
				t.Fatalf("Armiger received %+v", got)
			}
			if tc.contentType != "" && (r.Proposal == nil || r.Proposal.ContentType != tc.contentType || r.Uncertainty == nil) {
				t.Fatalf("proposal %+v", r.Proposal)
			}
			// The backend applies only results that pass the public contract's own validation.
			if err := r.Validate(j); err != nil {
				t.Fatalf("result rejected by contract: %v", err)
			}
		})
	}
}

func TestUnavailableArmigerDefersAndReportsStatus(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ArmigerURL = "http://127.0.0.1:1"
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, r := post(t, s, testJob(s)); r.Outcome != v1.Deferred || r.RetryAfterSeconds == 0 {
		t.Fatalf("got %+v", r)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/status", nil))
	var st v1.ServiceStatus
	if json.Unmarshal(rec.Body.Bytes(), &st) != nil || st.State != "unavailable" || st.Validate() != nil {
		t.Fatalf("status %s", rec.Body)
	}
}

func TestJobAndStartupValidation(t *testing.T) {
	s, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*v1.Job){
		func(j *v1.Job) { j.Processor.ModelVersion = "armiger-other" },
		func(j *v1.Job) { j.Configuration = json.RawMessage(`{"variant":"fp32"}`) },
		func(j *v1.Job) { j.Stage = v1.Extract },
	} {
		j := testJob(s)
		mutate(&j)
		if code, _ := post(t, s, j); code != http.StatusUnprocessableEntity {
			t.Fatalf("accepted invalid job: %d", code)
		}
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/manifest", nil))
	var m v1.Manifest
	if json.Unmarshal(rec.Body.Bytes(), &m) != nil || m.Validate() != nil {
		t.Fatalf("manifest %s", rec.Body)
	}
	network := DefaultConfig()
	network.Listen = "0.0.0.0:8095"
	if _, err := New(network); err == nil {
		t.Fatal("network listen without a token was accepted")
	}
	network.Token = strings.Repeat("t", 32)
	if _, err := New(network); err != nil {
		t.Fatal(err)
	}
}
