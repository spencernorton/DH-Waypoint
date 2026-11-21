package metainforequester

import "time"

type Config struct {
	RequestTimeout time.Duration
	KeyMutexSize   int
	// AllowPrivateAddrs lets the requester connect to loopback, private and
	// link-local peers. Off by default: peer addresses come from strangers.
	// Only a closed test network needs it.
	AllowPrivateAddrs bool
	// EnableMSE allows a single bounded MSE/PE (BEP-3 protocol encryption)
	// fallback within the same RequestTimeout budget when a connected peer
	// rejects a plaintext BitTorrent handshake. Off by default.
	EnableMSE bool
	// EnableUTP allows a bounded uTP (BEP 29) transport fallback within the same
	// RequestTimeout budget when a peer is unreachable or fails metadata exchange
	// over TCP. When combined with EnableMSE, falls back sequentially:
	// tcp_plain -> tcp_mse -> utp_plain -> utp_mse. Off by default.
	EnableUTP bool
}

func NewDefaultConfig() Config {
	return Config{
		RequestTimeout: 6 * time.Second,
		KeyMutexSize:   1000,
	}
}
