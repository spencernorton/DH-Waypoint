package processors

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
	v2 "github.com/spencernorton/DH-Waypoint/processorapi/v2"
	"github.com/xeipuuv/gojsonschema"
)

var authName = regexp.MustCompile(`^DH_WAYPOINT_PROCESSOR_TOKEN_[A-Z0-9_]{1,64}$`)

const defaultStrictFallbackTTL = 15 * time.Minute

type Transport struct {
	client              *http.Client
	CredentialDirectory string
	strictEndpoints     sync.Map
	strictTTL           time.Duration
	onStrictFallback    func(endpoint string)
}

func (t *Transport) effectiveStrictTTL() time.Duration {
	if t.strictTTL > 0 {
		return t.strictTTL
	}
	return defaultStrictFallbackTTL
}

// ClearStrictEndpoint removes any cached strict-v1 fallback entry for endpoint
// so subsequent jobs immediately probe the multi-capability contract.
func (t *Transport) ClearStrictEndpoint(endpoint string) {
	if endpoint != "" {
		t.strictEndpoints.Delete(endpoint)
	}
}

var errBadRequest = errors.New("bad_request")
var errStrictBadRequest = errors.New("strict_bad_request")

func NewTransport(timeout time.Duration) *Transport {
	return &Transport{client: &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect_forbidden") }, Transport: &http.Transport{Proxy: nil, MaxConnsPerHost: 16, MaxIdleConns: 16, IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: timeout, DisableCompression: true}}}
}
func validateEndpoint(endpoint, auth string) error {
	u, e := url.Parse(endpoint)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") || len(endpoint) > 1024 {
		return errors.New("invalid_endpoint")
	}
	if auth != "" && !authName.MatchString(auth) {
		return errors.New("invalid_auth_reference")
	}
	return nil
}
func (t *Transport) call(ctx context.Context, endpoint, auth, path string, body any, out any) error {
	if e := validateEndpoint(endpoint, auth); e != nil {
		return e
	}
	var raw []byte
	var e error
	method := http.MethodGet
	if body != nil {
		method = http.MethodPost
		raw, e = json.Marshal(body)
		if e != nil {
			return e
		}
		if len(raw) > v1.MaxBody {
			return errors.New("request_too_large")
		}
	}
	req, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(endpoint, "/")+path, bytes.NewReader(raw))
	if e != nil {
		return e
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		secret := os.Getenv(auth)
		if secret == "" && t.CredentialDirectory != "" {
			file, err := os.Open(filepath.Join(t.CredentialDirectory, auth))
			if err == nil {
				data, readErr := io.ReadAll(io.LimitReader(file, 4097))
				_ = file.Close()
				if readErr == nil && len(data) <= 4096 {
					secret = strings.TrimSpace(string(data))
				}
			}
		}
		if secret == "" {
			return errors.New("transport_credential_unavailable")
		}
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	res, e := t.client.Do(req)
	if e != nil {
		return errors.New("transport_failed")
	}
	defer res.Body.Close()
	media, _, mediaErr := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if mediaErr != nil || media != "application/json" {
		return errors.New("invalid_content_type")
	}
	if res.StatusCode == http.StatusBadRequest {
		return errBadRequest
	}
	if res.StatusCode != http.StatusOK {
		return errors.New("service_unavailable")
	}
	raw, e = io.ReadAll(io.LimitReader(res.Body, v1.MaxBody+1))
	if e != nil {
		return errors.New("response_read_failed")
	}
	return v1.Decode(raw, out)
}
func (t *Transport) Inspect(ctx context.Context, w v1.RegistryWrite) (v1.Manifest, v1.ServiceStatus, error) {
	var m v1.Manifest
	var s v1.ServiceStatus
	if e := t.call(ctx, w.Endpoint, w.AuthEnv, "/v1/manifest", nil, &m); e != nil {
		return m, s, e
	}
	if (m.Contract == v1.Contract || m.Contract == v2.Contract) && m.Processor.ID != "" {
		m.ApplyDefaults(m.Processor.ID)
	}
	if e := validateManifest(m); e != nil {
		return m, s, e
	}
	if len(w.Configuration) == 0 || len(w.Configuration) > 4096 {
		return m, s, errors.New("invalid_configuration")
	}
	var config map[string]any
	if e := v1.Decode(w.Configuration, &config); e != nil {
		return m, s, e
	}
	if config == nil || containsCredentials(config) {
		return m, s, errors.New("configuration_contains_credentials")
	}
	checked, e := gojsonschema.Validate(gojsonschema.NewBytesLoader(m.ConfigurationSchema), gojsonschema.NewGoLoader(config))
	if e != nil || !checked.Valid() {
		return m, s, errors.New("configuration_schema_mismatch")
	}
	s, e = t.Status(ctx, w.Endpoint, w.AuthEnv)
	if e != nil {
		return m, s, e
	}
	if s.Contract != m.Contract || s.Processor != m.Processor {
		return m, s, errors.New("service_identity_mismatch")
	}
	if w.State == "active" && s.State != "ready" {
		return m, s, errors.New("service_not_ready")
	}
	return m, s, nil
}
func (t *Transport) Status(ctx context.Context, endpoint, auth string) (v1.ServiceStatus, error) {
	var s v1.ServiceStatus
	e := t.call(ctx, endpoint, auth, "/v1/status", nil, &s)
	if e == nil {
		if (s.Contract == v1.Contract || s.Contract == v2.Contract) && s.Processor.ID != "" {
			s.Processor.ApplyDefaults(s.Processor.ID)
		}
		e = validateStatus(s)
	}
	return s, e
}
func (t *Transport) Process(ctx context.Context, endpoint, auth string, j v1.Job) (v1.Result, error) {
	var r v1.Result
	if e := validateJob(j); e != nil {
		return r, e
	}
	if j.Contract == v2.Contract {
		if e := t.call(ctx, endpoint, auth, "/v2/process", j, &r); e != nil {
			return r, e
		}
		r.FillDefaultBinding(j)
		return r, validateResult(r, j)
	}
	if val, strict := t.strictEndpoints.Load(endpoint); strict {
		if until, ok := val.(time.Time); ok && time.Now().Before(until) {
			strictRes, strictErr := t.processStrictV1(ctx, endpoint, auth, j)
			if errors.Is(strictErr, errStrictBadRequest) {
				t.strictEndpoints.Delete(endpoint)
				return strictRes, errors.New("service_unavailable")
			}
			return strictRes, strictErr
		}
		t.strictEndpoints.Delete(endpoint)
	}
	e := t.call(ctx, endpoint, auth, "/v1/process", j, &r)
	if errors.Is(e, errBadRequest) {
		strictRes, strictErr := t.processStrictV1(ctx, endpoint, auth, j)
		if strictErr != nil {
			if errors.Is(strictErr, errStrictBadRequest) {
				return strictRes, errors.New("service_unavailable")
			}
			return strictRes, strictErr
		}
		t.strictEndpoints.Store(endpoint, time.Now().Add(t.effectiveStrictTTL()))
		if t.onStrictFallback != nil {
			t.onStrictFallback(endpoint)
		}
		return strictRes, nil
	}
	if e != nil {
		return r, e
	}
	r.FillDefaultBinding(j)
	return r, validateResult(r, j)
}

func strictV1Job(j v1.Job, stage v1.Stage) v1.Job {
	out := j
	out.Capabilities = nil
	out.Stage = stage
	if stage != j.Stage {
		out.JobID = digest([]any{j.JobID, stage})
	}
	in := j.Input
	in.AccumulatedScores = nil
	in.AccumulatedTags = nil
	if len(in.PriorResults) > 0 {
		priors := make([]v1.Result, 0, len(in.PriorResults))
		for _, pr := range in.PriorResults {
			cp := pr
			if !slices.Contains([]v1.Stage{v1.Classify, v1.Extract, v1.Language, v1.Match, v1.Policy}, cp.Stage) {
				cp.Stage = v1.Policy
			}
			if cp.Proposal != nil {
				p := *cp.Proposal
				p.AddTags = nil
				p.RemoveTags = nil
				p.Weights = nil
				p.Liveness = nil
				if p.Extraction != nil {
					ex := *p.Extraction
					ex.VideoResolution = ""
					ex.VideoSource = ""
					ex.VideoCodec = ""
					ex.Video3D = ""
					ex.VideoModifier = ""
					ex.ReleaseGroup = ""
					if ex.Title == "" && ex.Year == 0 && ex.Season == nil && len(ex.Episodes) == 0 {
						p.Extraction = nil
					} else {
						p.Extraction = &ex
					}
				}
				if p.ContentType == "" && p.Extraction == nil && p.Language == nil && p.Match == nil && p.Exclusion == nil && p.Embedding == nil {
					cp.Proposal = nil
				} else {
					cp.Proposal = &p
				}
			}
			priors = append(priors, cp)
		}
		in.PriorResults = priors
	}
	out.Input = in
	return out
}

func (t *Transport) processStrictV1(ctx context.Context, endpoint, auth string, j v1.Job) (v1.Result, error) {
	caps := j.EffectiveCapabilities()
	combined := v1.Result{
		Binding:  j.Binding,
		Outcome:  v1.Abstained,
		Reasons:  []string{},
		Evidence: []v1.Evidence{},
	}
	var mergedProp v1.Proposal
	hasProp := false
	for _, st := range caps {
		sj := strictV1Job(j, st)
		var sr v1.Result
		e := t.call(ctx, endpoint, auth, "/v1/process", sj, &sr)
		if errors.Is(e, errBadRequest) {
			return combined, errStrictBadRequest
		}
		if e != nil {
			return combined, e
		}
		sr.FillDefaultBinding(sj)
		if e := sr.Validate(sj); e != nil {
			return combined, e
		}
		if sr.Outcome == v1.Failed || sr.Outcome == v1.Deferred {
			sr.Binding = j.Binding
			return sr, nil
		}
		if sr.Outcome == v1.Excluded {
			combined.Outcome = v1.Excluded
		} else if sr.Outcome == v1.Completed && combined.Outcome != v1.Excluded {
			combined.Outcome = v1.Completed
		}
		if sr.Uncertainty != nil && combined.Uncertainty == nil {
			u := *sr.Uncertainty
			combined.Uncertainty = &u
		}
		if sr.Telemetry != (v1.Telemetry{}) {
			combined.Telemetry = sr.Telemetry
		}
		combined.Trace = mergeStrictTrace(combined.Trace, sr.Trace)
		combined.RequestFeedback = combined.RequestFeedback || sr.RequestFeedback
		for _, reason := range sr.Reasons {
			if !slices.Contains(combined.Reasons, reason) && len(combined.Reasons) < 8 {
				combined.Reasons = append(combined.Reasons, reason)
			}
		}
		for _, ev := range sr.Evidence {
			if !slices.Contains(combined.Evidence, ev) && len(combined.Evidence) < 8 {
				combined.Evidence = append(combined.Evidence, ev)
			}
		}
		if sr.Proposal != nil {
			hasProp = true
			if sr.Proposal.ContentType != "" {
				mergedProp.ContentType = sr.Proposal.ContentType
			}
			if sr.Proposal.Extraction != nil {
				mergedProp.Extraction = sr.Proposal.Extraction
			}
			if sr.Proposal.Language != nil {
				mergedProp.Language = sr.Proposal.Language
			}
			if sr.Proposal.Match != nil {
				mergedProp.Match = sr.Proposal.Match
			}
			if sr.Proposal.Exclusion != nil {
				mergedProp.Exclusion = sr.Proposal.Exclusion
			}
			if sr.Proposal.Embedding != nil {
				mergedProp.Embedding = sr.Proposal.Embedding
			}
			for _, tag := range sr.Proposal.AddTags {
				if !slices.Contains(mergedProp.AddTags, tag) {
					mergedProp.AddTags = append(mergedProp.AddTags, tag)
				}
			}
			for _, tag := range sr.Proposal.RemoveTags {
				if !slices.Contains(mergedProp.RemoveTags, tag) {
					mergedProp.RemoveTags = append(mergedProp.RemoveTags, tag)
				}
			}
			if len(sr.Proposal.Weights) > 0 {
				if mergedProp.Weights == nil {
					mergedProp.Weights = make(map[string]float64, len(sr.Proposal.Weights))
				}
				for k, v := range sr.Proposal.Weights {
					mergedProp.Weights[k] = v
				}
			}
			if sr.Proposal.Liveness != nil {
				mergedProp.Liveness = sr.Proposal.Liveness
			}
		}
	}
	if hasProp && (combined.Outcome == v1.Completed || combined.Outcome == v1.Excluded) {
		combined.Proposal = &mergedProp
	}
	return combined, combined.Validate(j)
}
func mergeStrictTrace(dst, src json.RawMessage) json.RawMessage {
	s := bytes.TrimSpace(src)
	if len(s) == 0 || bytes.Equal(s, []byte("null")) {
		return dst
	}
	d := bytes.TrimSpace(dst)
	if len(d) == 0 || bytes.Equal(d, []byte("null")) {
		return bytes.Clone(s)
	}
	var dm, sm map[string]json.RawMessage
	if json.Unmarshal(d, &dm) == nil && json.Unmarshal(s, &sm) == nil && dm != nil && sm != nil {
		for k, v := range sm {
			vt := bytes.TrimSpace(v)
			if _, exists := dm[k]; !exists || (len(vt) > 0 && !bytes.Equal(vt, []byte("null"))) {
				dm[k] = bytes.Clone(vt)
			}
		}
		if merged, err := json.Marshal(dm); err == nil && len(merged) <= 65536 {
			return merged
		}
	}
	return dst
}

func (t *Transport) Feedback(ctx context.Context, endpoint, auth string, batch v1.FeedbackBatch) (v1.FeedbackAck, error) {
	var ack v1.FeedbackAck
	path := "/v1/feedback"
	if batch.Contract == v2.FeedbackContract {
		path = "/v2/feedback"
	}
	e := t.call(ctx, endpoint, auth, path, batch, &ack)
	if e == nil && batch.Contract == v2.FeedbackContract && (ack.Contract != batch.Contract || ack.Accepted < 0 || ack.Accepted > len(batch.Events)) {
		e = errors.New("invalid_v2_feedback_ack")
	}
	return ack, e
}

func containsCredentials(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			key = strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "_", ""), "-", ""))
			if strings.Contains(key, "password") || strings.Contains(key, "secret") || strings.Contains(key, "apikey") || strings.Contains(key, "credential") || key == "authorization" || key == "token" || key == "accesstoken" || key == "bearertoken" {
				return true
			}
			if containsCredentials(item) {
				return true
			}
		}
	case []any:
		for _, item := range v {
			if containsCredentials(item) {
				return true
			}
		}
	}
	return false
}
