package plugincmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
	v2 "github.com/spencernorton/DH-Waypoint/processorapi/v2"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v2"
)

func TestPluginVerifyCommandV1AndV2(t *testing.T) {
	for _, contract := range []string{v1.Contract, v2.Contract} {
		t.Run(contract, func(t *testing.T) {
			procID := v1.Identity{
				ID:            "synthetic-verifier",
				Version:       "1.0.0",
				ModelVersion:  "1.0.0",
				PolicyVersion: "1.0.0",
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1/manifest":
					_ = json.NewEncoder(w).Encode(v1.Manifest{
						Contract:            contract,
						Processor:           procID,
						Capabilities:        []v1.Stage{v1.Classify},
						Qualification:       "synthetic",
						ConfigurationSchema: json.RawMessage(`{"type":"object"}`),
					})
				case "/v1/status":
					_ = json.NewEncoder(w).Encode(v1.ServiceStatus{
						Contract:  contract,
						Processor: procID,
						State:     "ready",
					})
				case "/v1/process", "/v2/process":
					var job v1.Job
					require.NoError(t, json.NewDecoder(r.Body).Decode(&job))
					_ = json.NewEncoder(w).Encode(v1.Result{
						Binding:  job.Binding,
						Outcome:  v1.Abstained,
						Reasons:  []string{"synthetic_ok"},
						Evidence: []v1.Evidence{},
					})
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			res, err := New(Params{})
			require.NoError(t, err)
			var out bytes.Buffer
			app := &cli.App{
				Writer:   &out,
				Commands: []*cli.Command{res.Command},
			}
			require.NoError(t, app.Run([]string{"dh-waypoint", "plugin", "verify", srv.URL}))
			require.Contains(t, out.String(), "verified: synthetic-verifier 1.0.0")
			require.Contains(t, out.String(), "contract="+contract)
			require.Contains(t, out.String(), "outcome=abstained")
		})
	}
}
