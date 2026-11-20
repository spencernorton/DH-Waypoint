package metainforequester

import "time"

type Config struct {
	RequestTimeout time.Duration
	KeyMutexSize   int
	// AllowPrivateAddrs lets the requester connect to loopback, private and
	// link-local peers. Off by default: peer addresses come from strangers.
	// Only a closed test network needs it.
	AllowPrivateAddrs bool
	// EnableMSE allows a single bounded TCP MSE/PE (BEP-3 protocol encryption)
	// fallback within the same RequestTimeout budget when a connected peer
	// rejects a plaintext BitTorrent handshake. Off by default.
	EnableMSE bool
}

func NewDefaultConfig() Config {
	return Config{
		RequestTimeout: 6 * time.Second,
		KeyMutexSize:   1000,
	}
}
