package processors

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencernorton/DH-Waypoint/internal/buildidentity"
	"github.com/spencernorton/DH-Waypoint/internal/classifier"
	"github.com/spencernorton/DH-Waypoint/internal/classifier/contentfilter"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/namepolicy"
	"github.com/spencernorton/DH-Waypoint/internal/processor"
)

func NewConfiguredStore(c Config, p lazy.Lazy[*pgxpool.Pool], cc classifier.Config, cf contentfilter.Config, n *namepolicy.Policy, pc processor.Config) (*Store, error) {
	// The policy generation covers deterministic settings, not credentials.
	// The processing mode is included: it changes the deterministic facts
	// every processor input is derived from, so flipping it must stale
	// outstanding pipelines like any other core policy change.
	s, e := NewStore(c, p, digest([]any{"processor-core-policy-v1", buildidentity.CurrentBuildIdentity(), cc, cf, pc.Mode.Normalize()}))
	if e != nil {
		return nil, e
	}
	s.noiseV2 = cc.ParseNoiseV2
	s.singleEpisodeMax = cc.SingleEpisodeMaxBytes
	s.names = n
	s.filter = contentfilter.New(cf)
	s.filter.SetNamePolicy(n)
	s.forbiddenTypes = map[string]bool{}
	for _, kind := range cc.DeleteContentTypes {
		s.forbiddenTypes[kind] = true
	}
	if cc.DeleteXxx {
		s.forbiddenTypes["xxx"] = true
	}
	return s, nil
}
func (s *Store) Collectors() []prometheus.Collector { return s.metrics.Collectors() }
