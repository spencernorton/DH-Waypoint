package processors

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPayloadDeferredBackoffPolicy(t *testing.T) {
	for _, reason := range []string{"proof_capacity", "decoder_timeout", "lease_expiring", "decoder_unavailable"} {
		for _, att := range []int{1, 2, 5, 20} {
			require.Equal(t, "5 minutes", payloadDeferredBackoff(reason, att), "local contention %s attempt %d", reason, att)
		}
	}

	require.Equal(t, "1 hour", payloadDeferredBackoff("claim_not_available", 1))
	require.Equal(t, "6 hours", payloadDeferredBackoff("claim_not_available", 2))
	require.Equal(t, "24 hours", payloadDeferredBackoff("claim_not_available", 3))
	require.Equal(t, "24 hours", payloadDeferredBackoff("claim_not_available", 9))

	require.Equal(t, "5 minutes", payloadDeferredBackoff("verified_pieces_unavailable", 1))
	require.Equal(t, "15 minutes", payloadDeferredBackoff("verified_pieces_unavailable", 2))
	require.Equal(t, "1 hour", payloadDeferredBackoff("verified_pieces_unavailable", 3))
	require.Equal(t, "4 hours", payloadDeferredBackoff("verified_pieces_unavailable", 4))
	require.Equal(t, "24 hours", payloadDeferredBackoff("verified_pieces_unavailable", 5))
	require.Equal(t, "24 hours", payloadDeferredBackoff("claim_unavailable", 12))
}
