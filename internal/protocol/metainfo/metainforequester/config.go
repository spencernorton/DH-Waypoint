package metainforequester

import "time"

type Config struct {
	RequestTimeout time.Duration
	KeyMutexSize   int
	// AllowPrivateAddrs lets the requester connect to loopback, private and
	// link-local peers. Off by default: peer addresses come from strangers.
	// Only a closed test network needs it.
	AllowPrivateAddrs bool
}

func NewDefaultConfig() Config {
	return Config{
		RequestTimeout: 6 * time.Second,
		KeyMutexSize:   1000,
	}
}
