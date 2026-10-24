package processors

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
	"github.com/stretchr/testify/require"
)

func TestPostgresRetainedLLMRegistrationFailsClosed(t *testing.T) {
	s, p, _ := fixture(t)
	var calls atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer provider.Close()
	entry, err := s.Register(testContext, "retained", v1.RegistryWrite{Driver: v1.DriverBuiltinFilter,
		Mode: "shadow", State: "active", Configuration: json.RawMessage(`{"block_patterns":["fixture"]}`)})
	require.NoError(t, err)
	// Represent the durable registration a previous binary could create.
	_, err = p.Exec(testContext, `UPDATE processor_registry SET driver='builtin/llm',endpoint=$1,configuration='{"model":"fixture"}' WHERE id='retained'`, provider.URL)
	require.NoError(t, err)
	require.NoError(t, s.Refresh(testContext))
	status, err := s.Status(testContext)
	require.NoError(t, err)
	require.Equal(t, "ready", status.CoreReadiness)
	require.Len(t, status.Processors, 1)
	proc := status.Processors[0]
	require.False(t, proc.Enabled)
	require.Equal(t, "incompatible", proc.Compatibility)
	require.Equal(t, "unavailable", proc.Health)
	require.Equal(t, retiredLLMReason, proc.LastError)
	require.Zero(t, calls.Load())
	// Recovery can pause the retired entry, but cannot reactivate it.
	paused := "paused"
	require.NoError(t, s.Control(testContext, entry.ID, v1.RegistryControl{ExpectedGeneration: entry.Generation, State: &paused}))
	es, err := s.Entries(testContext)
	require.NoError(t, err)
	active := "active"
	require.ErrorContains(t, s.Control(testContext, entry.ID, v1.RegistryControl{ExpectedGeneration: es[0].Generation, State: &active}), retiredLLMReason)
	require.Zero(t, calls.Load())
}
