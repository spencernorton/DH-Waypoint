package migrations

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v2"
)

func TestComputeDBFingerprintDeterministicAndScoped(t *testing.T) {
	t.Parallel()
	a := ComputeDBFingerprint("7710079708787920945", "bitmagnet", 83)
	b := ComputeDBFingerprint("7710079708787920945", "bitmagnet", 83)
	c := ComputeDBFingerprint("7710079708787920945", "bitmagnet", 82)
	d := ComputeDBFingerprint("7541432310887014434", "bitmagnet", 83)

	require.Len(t, a, 12)
	require.Equal(t, a, b)
	require.NotEqual(t, a, c)
	require.NotEqual(t, a, d)
}

func TestIncompatibleSchemaErrorImplementsExitCode78(t *testing.T) {
	t.Parallel()
	err := &IncompatibleSchemaError{
		DetectedMajor:        6,
		DetectedGooseVersion: 100,
		Reason:               "database already upgraded to v6",
		Guidance:             "run v6 binary",
	}
	var exitCoder cli.ExitCoder
	require.True(t, errors.As(err, &exitCoder))
	require.Equal(t, ExitCodeIncompatibleSchema, exitCoder.ExitCode())
	require.Contains(t, err.Error(), "exit 78")
	require.Contains(t, err.Error(), "database already upgraded to v6")
	require.Contains(t, err.Error(), "run v6 binary")
}
