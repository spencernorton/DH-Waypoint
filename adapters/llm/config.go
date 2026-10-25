// Package llm implements a standalone processor service. The backend must never
// import this package: the only shared dependency is processorapi/v1.
package llm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
)

// Config is service-local configuration, never a core registration payload.
// Inference requires both EnableInference=true and DryRun=false.
type Config struct {
	Listen             string     `json:"listen"`
	ID                 string     `json:"id"`
	Capabilities       []v1.Stage `json:"capabilities"`
	ProcessorTokenEnv  string     `json:"processor_token_env"`
	EnableInference    bool       `json:"enable_inference"`
	DryRun             bool       `json:"dry_run"`
	Endpoint           string     `json:"endpoint"` // complete chat-completions URL
	Model              string     `json:"model"`
	APIKeyEnv          string     `json:"api_key_env"`
	APIKeyFile         string     `json:"api_key_file"`
	SystemPromptFile   string     `json:"system_prompt_file"`
	JSONMode           bool       `json:"json_mode"`
	Temperature        *float64   `json:"temperature"`
	TokenLimitField    string     `json:"token_limit_field"`
	MaxOutputTokens    int        `json:"max_output_tokens"`
	MaxInputBytes      int        `json:"max_input_bytes"`
	TimeoutMS          int        `json:"timeout_ms"`
	Concurrency        int        `json:"concurrency"`
	RequestsPerMinute  int        `json:"requests_per_minute"`
	MaxRequests        int64      `json:"max_requests"`
	FeedbackFile       string     `json:"feedback_file"`
	MaxFeedbackBytes   int64      `json:"max_feedback_bytes"`
	ResultCacheEntries int        `json:"result_cache_entries"`
}

// DefaultConfig performs no inference and binds only to loopback.
func DefaultConfig() Config {
	return Config{Listen: "127.0.0.1:8091", ID: "llm", Capabilities: []v1.Stage{v1.Classify, v1.Extract},
		ProcessorTokenEnv: "LLM_PROCESSOR_TOKEN", DryRun: true, TokenLimitField: "max_tokens",
		MaxOutputTokens: 1024, MaxInputBytes: 32768, TimeoutMS: 8000, Concurrency: 1,
		RequestsPerMinute: 10, MaxRequests: 100, MaxFeedbackBytes: 16 << 20, ResultCacheEntries: 256}
}

// LoadConfig overlays a strict, bounded JSON file onto safe defaults.
func LoadConfig(path string) (Config, error) {
	c := DefaultConfig()
	b, err := readBoundedFile(path, 16384)
	if err != nil {
		return c, errors.New("configuration_read_failed")
	}
	if err := v1.Decode(b, &c); err != nil {
		return c, errors.New("invalid_service_configuration")
	}
	return c, nil
}

func (c Config) validate() error {
	if c.Concurrency < 1 || c.Concurrency > 16 || c.RequestsPerMinute < 1 || c.RequestsPerMinute > 10000 || c.MaxRequests < 1 ||
		c.MaxOutputTokens < 1 || c.MaxOutputTokens > 32768 || c.MaxInputBytes < 256 || c.MaxInputBytes > v1.MaxBody ||
		c.TimeoutMS < 100 || c.TimeoutMS > 60000 || c.MaxFeedbackBytes < 1024 || c.MaxFeedbackBytes > 1<<30 ||
		c.ResultCacheEntries < 1 || c.ResultCacheEntries > 10000 {
		return errors.New("invalid_service_bounds")
	}
	if c.TokenLimitField != "max_tokens" && c.TokenLimitField != "max_completion_tokens" {
		return errors.New("invalid_token_limit_field")
	}
	if c.Temperature != nil && (math.IsNaN(*c.Temperature) || math.IsInf(*c.Temperature, 0) || *c.Temperature < 0 || *c.Temperature > 2) {
		return errors.New("invalid_temperature")
	}
	if c.APIKeyEnv != "" && c.APIKeyFile != "" || c.APIKeyEnv != "" && c.APIKeyEnv == c.ProcessorTokenEnv {
		return errors.New("invalid_credential_configuration")
	}
	if c.EnableInference && !c.DryRun {
		u, err := url.Parse(c.Endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
			u.RawQuery != "" || u.Fragment != "" || len(c.Endpoint) > 1024 || !strings.HasSuffix(u.Path, "/chat/completions") {
			return errors.New("invalid_provider_endpoint")
		}
		if len(c.Model) == 0 || len(c.Model) > 256 {
			return errors.New("invalid_provider_model")
		}
	}
	for _, cap := range c.Capabilities {
		if !slices.Contains([]v1.Stage{v1.Classify, v1.Extract, v1.Match, v1.Language}, cap) {
			return errors.New("unsupported_capability")
		}
	}
	return nil
}

func readBoundedFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = errors.New("file_too_large")
	}
	return b, err
}

func secret(env, file string) (string, error) {
	s := os.Getenv(env)
	if file != "" {
		b, err := readBoundedFile(file, 4096)
		if err != nil {
			return "", errors.New("credential_unavailable")
		}
		s = string(b)
	}
	s = strings.TrimSpace(s)
	if len(s) > 4096 || strings.ContainsAny(s, "\r\n") || (env != "" || file != "") && s == "" {
		return "", errors.New("credential_unavailable")
	}
	return s, nil
}

func fingerprint(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (c Config) timeout() time.Duration { return time.Duration(c.TimeoutMS) * time.Millisecond }
