package processors

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/serving"
	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
	"github.com/stretchr/testify/require"
)

func TestPostgresStrictFallbackPreservesReleaseAudioAndRegisterClearsStrictCache(t *testing.T) {
	s, p, _ := fixture(t)
	s.transport.strictTTL = 15 * time.Minute

	var strictMode atomic.Bool
	strictMode.Store(true)
	var multiCapCalls atomic.Int32
	identity := v1.Identity{
		ID:             "synthetic-strict-audio",
		Version:        "1.0.0",
		ModelVersion:   "none",
		PolicyVersion:  "test-v1",
		ArtifactDigest: strings.Repeat("a", 64),
	}
	var currentIdentity atomic.Value
	currentIdentity.Store(identity)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id := currentIdentity.Load().(v1.Identity)
		switch r.URL.Path {
		case "/v1/manifest":
			_ = json.NewEncoder(w).Encode(v1.Manifest{
				Contract:            v1.Contract,
				Processor:           id,
				Capabilities:        []v1.Stage{v1.Classify, v1.Language, v1.Policy},
				ConfigurationSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
				Qualification:       "synthetic",
			})
		case "/v1/status":
			_ = json.NewEncoder(w).Encode(v1.ServiceStatus{
				Contract:   v1.Contract,
				Processor:  id,
				State:      "ready",
				InstanceID: "test-strict-audio",
				StartedAt:  "2026-01-01T00:00:00Z",
			})
		case "/v1/process":
			var j v1.Job
			require.NoError(t, json.NewDecoder(r.Body).Decode(&j))
			if strictMode.Load() && len(j.Capabilities) > 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"bad_request"}`))
				return
			}
			if len(j.Capabilities) > 1 {
				multiCapCalls.Add(1)
			}
			res := v1.Result{
				Binding:  j.Binding,
				Outcome:  v1.Completed,
				Reasons:  []string{"clean"},
				Evidence: []v1.Evidence{},
			}
			if len(j.Capabilities) > 1 {
				res.Proposal = &v1.Proposal{
					ContentType: "movie",
					Language:    &v1.LanguageClaim{Audio: []string{"en"}, Evidence: "metadata_claim"},
					AddTags:     []string{"clean"},
				}
				res.Trace = json.RawMessage(`{"audio_language":"english","anime":false,"neural_heads":{"category":"movie"}}`)
			} else {
				switch j.Stage {
				case v1.Classify:
					res.Proposal = &v1.Proposal{ContentType: "movie"}
					res.Trace = json.RawMessage(`{"audio_language":"english","anime":false,"neural_heads":{"category":"movie"}}`)
				case v1.Language:
					res.Proposal = &v1.Proposal{Language: &v1.LanguageClaim{Audio: []string{"en"}, Evidence: "metadata_claim"}}
				default:
					res.Proposal = &v1.Proposal{AddTags: []string{"clean"}}
				}
			}
			_ = json.NewEncoder(w).Encode(res)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	entry, err := s.Register(testContext, identity.ID, v1.RegistryWrite{
		Endpoint:      srv.URL,
		Mode:          "apply",
		State:         "active",
		Configuration: json.RawMessage(`{}`),
	})
	require.NoError(t, err)
	require.NoError(t, s.SetPublicationConfig(serving.Config{
		RequiredProcessor:               identity.ID,
		RequiredProcessorArtifactDigest: identity.ArtifactDigest,
		RequireEnglishMainAudio:         true,
	}))

	h1 := seedTorrent(t, p, 41, "movie")
	_, err = s.Scan(testContext)
	require.NoError(t, err)
	run(t, s)

	// Strict fallback must preserve Trace so release_audio_v1 reconciles to english_main (not unknown).
	var releaseAudio string
	require.NoError(t, p.QueryRow(testContext, `SELECT release_audio_v1 FROM processor_publication_reviews WHERE info_hash=$1 AND processor_id=$2`, h1, identity.ID).Scan(&releaseAudio))
	require.Equal(t, "english_main", releaseAudio)
	_, cached := s.transport.strictEndpoints.Load(srv.URL)
	require.True(t, cached)

	// Upgrading endpoint + re-registering must clear strictEndpoints so next job uses multi-cap immediately.
	strictMode.Store(false)
	upgraded := identity
	upgraded.Version = "2.0.0"
	upgraded.ArtifactDigest = strings.Repeat("b", 64)
	currentIdentity.Store(upgraded)
	_, err = s.Register(testContext, identity.ID, v1.RegistryWrite{
		ExpectedGeneration: entry.Generation,
		Endpoint:           srv.URL,
		Mode:               "apply",
		State:              "active",
		Configuration:      json.RawMessage(`{}`),
	})
	require.NoError(t, err)
	require.NoError(t, s.SetPublicationConfig(serving.Config{
		RequiredProcessor:               upgraded.ID,
		RequiredProcessorArtifactDigest: upgraded.ArtifactDigest,
		RequireEnglishMainAudio:         true,
	}))
	_, cached = s.transport.strictEndpoints.Load(srv.URL)
	require.False(t, cached, "Register must clear cached strict endpoint state")

	h2 := seedTorrent(t, p, 42, "movie")
	_, err = s.Scan(testContext)
	require.NoError(t, err)
	for n := 0; n < 10; n++ {
		worked, runErr := s.RunOne(testContext)
		require.NoError(t, runErr)
		if !worked {
			break
		}
	}
	require.GreaterOrEqual(t, multiCapCalls.Load(), int32(2), "post-Register jobs must use multi-capability requests")
	require.NoError(t, p.QueryRow(testContext, `SELECT release_audio_v1 FROM processor_publication_reviews WHERE info_hash=$1 AND processor_id=$2`, h2, identity.ID).Scan(&releaseAudio))
	require.Equal(t, "english_main", releaseAudio)
}
