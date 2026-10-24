package v2

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
	"github.com/stretchr/testify/require"
)

func TestExplicitV2ContractRetainsV1StrictValidation(t *testing.T) {
	raw, err := os.ReadFile("../../docs/processors/fixtures/job.json")
	require.NoError(t, err)
	var legacy v1.Job
	require.NoError(t, v1.Decode(raw, &legacy))
	require.NoError(t, legacy.Validate())
	j := Job(legacy)
	j.Contract = Contract
	j.Input.InfoHash = strings.Repeat("ab", 32)
	require.NoError(t, j.Validate())
	encoded, err := json.Marshal(j)
	require.NoError(t, err)
	var decoded Job
	require.NoError(t, v1.Decode(encoded, &decoded))
	require.NoError(t, decoded.Validate())
	require.Equal(t, j.Input.InfoHash, decoded.Input.InfoHash)
	require.Error(t, v1.Job(j).Validate(), "v1 never accepts a v2 contract implicitly")
	wrong := j
	wrong.Contract = v1.Contract
	require.Error(t, wrong.Validate())
	wrong = j
	wrong.Input.InfoHash = wrong.Input.InfoHash[:62]
	require.Error(t, wrong.Validate())
	wrong = j
	wrong.Input.InfoHash = strings.ToUpper(wrong.Input.InfoHash)
	require.Error(t, wrong.Validate())
	wrong = j
	wrong.Input.Name = ""
	require.Error(t, wrong.Validate(), "all v1 resource and input checks still apply")
	r := Result{Binding: j.Binding, Outcome: v1.Abstained, Reasons: []string{}, Evidence: []v1.Evidence{}}
	require.NoError(t, r.Validate(j))
	r.Contract = v1.Contract
	require.Error(t, r.Validate(j))
	// No new field is slipped into a v1 message or its strict decoder.
	require.Error(t, v1.Decode([]byte(`{"contract":"dh-waypoint.processor.v1","unexpected_identity":true}`), &legacy))
}
