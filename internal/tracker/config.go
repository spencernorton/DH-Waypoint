// Package tracker implements DH-Waypoint's optional private-torrent
// support: operator upload and personalised download of .torrent
// files, plus a minimal HTTP announce/scrape tracker (BEP-3/15-style
// semantics over HTTP, BEP-23 compact peers). It deliberately has no
// user accounts, ratio economy, or invite system — passkeys gate
// tracker access, a single admin token gates the private file API,
// and anything richer belongs to the site layer an operator runs in
// front of the core.
package tracker

import (
	"crypto/subtle"
	"errors"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/namepolicy"
)

// Config is the `tracker` configuration section. The tracker and
// private file API are off until configured: announce/scrape need
// Enabled plus at least one passkey; the file API needs AdminToken.
type Config struct {
	Enabled bool
	// AdminToken gates /api/private/v1 (upload, download, detail).
	// Unset returns 503 on that API only.
	AdminToken namepolicy.SecretToken
	// Passkeys are the tracker credentials embedded in announce URLs
	// (/announce/<passkey>). Typically one per member or per client.
	Passkeys []string
	// PublicBaseURL is the externally reachable base used to build
	// personalised announce URLs on download (for example
	// https://tracker.example.com). Empty derives scheme+host from
	// the download request itself.
	PublicBaseURL string
	// AnnounceInterval is the interval returned to clients; peers
	// are considered gone after twice this without an announce.
	AnnounceInterval time.Duration
	// MaxPeers caps the peer list returned per announce.
	MaxPeers int
}

const DefaultNumWant = 50

func NewDefaultConfig() Config {
	return Config{
		AnnounceInterval: 30 * time.Minute,
		MaxPeers:         50,
	}
}

func (c Config) Validate() error {
	if c.MaxPeers < 1 || c.MaxPeers > 200 {
		return errors.New("tracker max_peers must be between 1 and 200")
	}
	if c.AnnounceInterval < time.Minute || c.AnnounceInterval > 6*time.Hour {
		return errors.New("tracker announce_interval must be between 1m and 6h")
	}
	if len(c.Passkeys) > 4096 {
		return errors.New("tracker passkeys exceeds 4096 entries")
	}
	for _, k := range c.Passkeys {
		if len(k) < 8 || len(k) > 256 {
			return errors.New("tracker passkeys must be 8-256 characters")
		}
	}
	return nil
}

// TrackerActive reports whether announce/scrape should answer.
func (c Config) TrackerActive() bool {
	return c.Enabled && len(c.Passkeys) > 0
}

// AcceptsPasskey reports whether key is a configured passkey.
// Comparisons are constant-time per candidate and do not
// short-circuit on the matching position's index.
func (c Config) AcceptsPasskey(key string) bool {
	if key == "" {
		return false
	}
	accepted := 0
	for _, candidate := range c.Passkeys {
		accepted |= subtle.ConstantTimeCompare([]byte(key), []byte(candidate))
	}
	return accepted == 1
}

// FreshnessWindow is how long a peer stays visible without
// re-announcing.
func (c Config) FreshnessWindow() time.Duration {
	return 2 * c.AnnounceInterval
}
