package dht

import (
	"fmt"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnmarshalNodeAddr(t *testing.T) {
	t.Parallel()

	var na NodeAddr

	require.NoError(t, na.UnmarshalBinary([]byte("\x01\x02\x03\x04\x05\x06")))
	assert.Equal(t, "1.2.3.4", na.IP.String())
}

func TestCompactAddressRejectsMalformedWireLengths(t *testing.T) {
	for _, length := range []int{0, 1, 2, 5, 7, 17, 19, 64} {
		t.Run(fmt.Sprint(length), func(t *testing.T) {
			var addr NodeAddr
			var err error
			require.NotPanics(t, func() { err = addr.UnmarshalBinary(make([]byte, length)) })
			require.Error(t, err)
		})
	}
}

func TestMalformedCompactAddressInDHTMessageReturnsError(t *testing.T) {
	for _, length := range []int{0, 1, 2, 5, 7, 17, 19} {
		for _, field := range []string{"ip", "values"} {
			t.Run(fmt.Sprintf("%s/%d", field, length), func(t *testing.T) {
				packet := map[string]any{"t": "x", "y": "r"}
				value := string(make([]byte, length))
				if field == "ip" {
					packet["ip"] = value
				} else {
					packet["r"] = map[string]any{"values": []string{value}}
				}
				raw, err := bencode.Marshal(packet)
				require.NoError(t, err)
				var msg Msg
				require.NotPanics(t, func() { err = bencode.Unmarshal(raw, &msg) }, "UDP decoder must reject hostile addresses without a runtime panic")
				require.Error(t, err)
			})
		}
	}
}

func TestCompactAddressRetainsIPv4AndIPv6(t *testing.T) {
	for _, value := range [][]byte{
		{127, 0, 0, 1, 0x1a, 0xe1},
		{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0x1a, 0xe1},
	} {
		var addr NodeAddr
		require.NoError(t, addr.UnmarshalBinary(value))
		require.Equal(t, 6881, addr.Port)
		require.True(t, addr.ToAddrPort().IsValid())
		roundTrip, err := addr.MarshalBinary()
		require.NoError(t, err)
		require.Equal(t, value, roundTrip)
	}
}
