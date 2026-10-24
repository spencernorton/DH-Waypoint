package processors

import (
	"context"
	"encoding/json"
	"testing"

	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
	"github.com/stretchr/testify/require"
)

func TestRetiredLLMCannotRegisterOrExecute(t *testing.T) {
	w := v1.RegistryWrite{Driver: v1.DriverBuiltinLLM, Mode: "shadow", State: "active", Configuration: json.RawMessage(`{}`)}
	require.EqualError(t, validateWrite(w), retiredLLMReason)
	s := &Store{} // no database, transport or credential lookup is available
	_, _, err := s.inspectBuiltin(context.Background(), "retired", w)
	require.EqualError(t, err, retiredLLMReason)
	_, err = s.processBuiltin(context.Background(), Entry{Driver: v1.DriverBuiltinLLM}, v1.Job{})
	require.EqualError(t, err, retiredLLMReason)
	require.Equal(t, retiredLLMReason, failureReason(err))
}

func TestStaticRulesNeverLearnInCore(t *testing.T) {
	learn := &v1.LearningConfig{Enabled: true, Mode: "online_calibrate"}
	w := v1.RegistryWrite{Driver: v1.DriverBuiltinRegex, Learning: learn}
	require.EqualError(t, validateWrite(w), retiredLearningReason)
	s := &Store{}
	_, err := s.processBuiltin(context.Background(), Entry{Driver: v1.DriverBuiltinRegex, Learning: learn}, v1.Job{})
	require.EqualError(t, err, retiredLearningReason)
	// Static weights are a pure function of the supplied configuration, with
	// no database from which a retained learned_state could be read.
	j := v1.Job{Input: v1.Input{Name: "Synthetic"}, Configuration: json.RawMessage(`{"rules":[{"id":"fixture","pattern":"Synthetic","weight":1.5}]}`)}
	r, err := s.runRegexWeights(j)
	require.NoError(t, err)
	require.Equal(t, 1.5, r.Proposal.Weights["score"])
}
