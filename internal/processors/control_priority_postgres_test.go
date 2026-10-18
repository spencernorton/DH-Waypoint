package processors

import (
	"encoding/json"
	"errors"
	"testing"

	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
	"github.com/stretchr/testify/require"
)

// A priority-only RegistryControl reorders future plans: it bumps the
// registry generation (CAS) but must not invalidate the execution
// generation, and out-of-range priorities are rejected like registration.
func TestPostgresControlPriorityReordersWithoutInvalidatingExecution(t *testing.T) {
	s, _, _ := fixture(t)
	write := func(priority int) v1.RegistryWrite {
		return v1.RegistryWrite{
			Driver:        v1.DriverBuiltinFilter,
			Priority:      priority,
			Mode:          "apply",
			State:         "active",
			Configuration: json.RawMessage(`{"block_patterns":["(?i)\\bCAMRIP\\b"],"reason":"camrip_blocked"}`),
		}
	}
	first, err := s.Register(testContext, "prio-first", write(10))
	require.NoError(t, err)
	second, err := s.Register(testContext, "prio-second", write(20))
	require.NoError(t, err)

	entries, err := s.Entries(testContext)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.Equal(t, "prio-first", entries[0].ID)
	require.Equal(t, "prio-second", entries[1].ID)

	five := 5
	require.NoError(t, s.Control(testContext, "prio-second", v1.RegistryControl{
		ExpectedGeneration: second.Generation, Priority: &five,
	}))

	entries, err = s.Entries(testContext)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.Equal(t, "prio-second", entries[0].ID)
	require.Equal(t, 5, entries[0].Priority)
	require.Greater(t, entries[0].Generation, second.Generation)
	require.Equal(t, second.ExecutionGeneration, entries[0].ExecutionGeneration,
		"priority change must not invalidate execution")
	require.Equal(t, "prio-first", entries[1].ID)
	require.Equal(t, first.Generation, entries[1].Generation)

	tooBig := 1001
	err = s.Control(testContext, "prio-first", v1.RegistryControl{
		ExpectedGeneration: first.Generation, Priority: &tooBig,
	})
	require.True(t, errors.Is(err, ErrControl), "priority above registration bound: %v", err)

	negative := -1
	err = s.Control(testContext, "prio-first", v1.RegistryControl{
		ExpectedGeneration: first.Generation, Priority: &negative,
	})
	require.True(t, errors.Is(err, ErrControl), "negative priority: %v", err)

	// The pre-change generation no longer authorizes a second write.
	err = s.Control(testContext, "prio-second", v1.RegistryControl{
		ExpectedGeneration: second.Generation, Priority: &five,
	})
	require.True(t, errors.Is(err, ErrConflict), "stale generation: %v", err)
}
