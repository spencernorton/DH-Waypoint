package config

import (
	"github.com/spencernorton/DH-Waypoint/internal/processors"
	"github.com/spencernorton/DH-Waypoint/internal/serving"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestServingAdultExclusionIsExplicitAndDefaultsOff(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		env := map[string]string{}
		if enabled {
			env["SERVING_EXCLUDE_ADULT"] = "true"
		}
		cfg := resolveSectionFromEnv(t, "serving", serving.Config{}, env).(serving.Config)
		require.Equal(t, enabled, cfg.ExcludeAdult)
	}
}

func TestRequiredPublicationReviewEnvironmentIsPinned(t *testing.T) {
	cfg := resolveSectionFromEnv(t, "serving", serving.Config{}, map[string]string{
		"SERVING_REQUIRED_PROCESSOR":                        "synthetic-reviewer",
		"SERVING_REQUIRED_PROCESSOR_ARTIFACT_DIGEST":        strings.Repeat("a", 64),
		"SERVING_REQUIRED_PAYLOAD_VERIFIER_ARTIFACT_DIGEST": strings.Repeat("b", 64),
	}).(serving.Config)
	require.Equal(t, "synthetic-reviewer", cfg.RequiredProcessor)
	require.Equal(t, strings.Repeat("a", 64), cfg.RequiredProcessorArtifactDigest)
	require.Equal(t, strings.Repeat("b", 64), cfg.RequiredPayloadVerifierArtifactDigest)
	processorCfg := resolveSectionFromEnv(t, "processors", processors.NewDefaultConfig(), map[string]string{"PROCESSORS_PAYLOAD_VERIFIER_TOKEN": strings.Repeat("s", 48)}).(processors.Config)
	require.Equal(t, strings.Repeat("s", 48), string(processorCfg.PayloadVerifierToken))
	_, err := serving.NewPolicy(cfg, []string{"syntheticadult"}, []string{"mkv"})
	require.NoError(t, err)
	for _, broken := range []serving.Config{{RequiredProcessor: "synthetic-reviewer"}, {RequiredProcessorArtifactDigest: strings.Repeat("a", 64)}, {RequiredProcessor: "synthetic-reviewer", RequiredProcessorArtifactDigest: "latest"}} {
		_, err = serving.NewPolicy(broken, []string{"syntheticadult"}, []string{"mkv"})
		require.Error(t, err)
	}
}
