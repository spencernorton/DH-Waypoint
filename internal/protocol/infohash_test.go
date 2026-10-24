package protocol

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInfoHashFullDigestAndWireKeyAreDistinct(t *testing.T) {
	v1 := InfoHashV1(ID{1, 2, 3})
	sum := [32]byte{1, 2, 3}
	v2 := InfoHashV2(sum)
	require.Equal(t, v1.WireID(), v2.WireID())
	require.NotEqual(t, v1, v2)
	require.Len(t, v1.Bytes(), 20)
	require.Len(t, v2.Bytes(), 32)
	require.Len(t, RandomNodeID(), 20)
	require.Len(t, RandomPeerID(), 20)
	for _, h := range []InfoHash{v1, v2} {
		parsed, err := ParseInfoHash(h.String())
		require.NoError(t, err)
		require.Equal(t, h, parsed)
		var scanned InfoHash
		require.NoError(t, scanned.Scan(h.Bytes()))
		require.Equal(t, h, scanned)
		b, err := json.Marshal(h)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(b, &scanned))
		require.Equal(t, h, scanned)
	}
	for _, n := range []int{0, 19, 21, 31, 33} {
		_, err := NewInfoHash(make([]byte, n))
		require.Error(t, err)
	}
	bad := v1
	bad[20] = 1
	_, err := bad.Value()
	require.Error(t, err)
}

func TestMagnetIdentityRoundTripsAndConflicts(t *testing.T) {
	v1 := InfoHashV1(ID{1})
	v2 := InfoHashV2(sha256.Sum256([]byte("synthetic")))
	for _, ids := range []TorrentIdentities{{V1: &v1}, {V2: &v2}, {V1: &v1, V2: &v2}} {
		parsed, err := ParseMagnetIdentities(ids.Magnet("Synthetic file + name"))
		require.NoError(t, err)
		require.Equal(t, ids, parsed)
	}
	parsed, err := ParseMagnetIdentities("magnet:?xt=urn:btih:" + base32.StdEncoding.EncodeToString(v1.Bytes()))
	require.NoError(t, err)
	require.Equal(t, v1, *parsed.V1)
	for _, s := range []string{
		"magnet:?xt=urn:btmh:1220" + v2.String()[:40],
		"magnet:?xt=urn:btih:" + v2.String(),
		"magnet:?xt=urn:btmh:1320" + v2.String(),
		"magnet:?xt=urn:btih:" + v1.String() + "&xt=urn:btih:" + strings.Repeat("a", 40),
	} {
		_, err := ParseMagnetIdentities(s)
		require.Error(t, err, s)
	}
}
