package server

import "time"

type Config struct {
	Port         uint16
	QueryTimeout time.Duration

	// EnableIPv6 enables opt-in dual-stack BEP-32 IPv6 DHT discovery alongside
	// IPv4 on a separate IPV6_V6ONLY UDP socket. Off by default so IPv4-only
	// environments never fail startup or emit unreachable IPv6 probes.
	EnableIPv6 bool

	// IPv6Port overrides the UDP port bound for the IPv6 DHT socket when
	// EnableIPv6 is true. Zero reuses Port (safe because IPV6_V6ONLY is set).
	IPv6Port uint16

	// QueryLimiterRatePerSec is the sustained outbound-query rate per
	// remote peer (keyed by IP) in tokens/second.
	QueryLimiterRatePerSec float64

	// QueryLimiterBurst is the burst capacity of the per-remote-peer
	// token bucket.
	QueryLimiterBurst int
}

func NewDefaultConfig() Config {
	return Config{
		Port:                   3334,
		QueryTimeout:           time.Second * 4,
		QueryLimiterRatePerSec: 1.0,
		QueryLimiterBurst:      16,
	}
}
