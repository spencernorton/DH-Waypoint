package dhtms

import (
	"errors"
	"strings"
	"sync/atomic"
	"time"
)

// ShadowDiff records per-query parity telemetry between PostgreSQL and DH-TMS.
type ShadowDiff struct {
	PGCount      int
	TMSCount     int
	OverlapCount int
	LeakedHashes []string
	Recall       float64
	ExactSet     bool
	PGLatency    time.Duration
	TMSLatency   time.Duration
}

// ShadowStats is a point-in-time snapshot of aggregate shadow parity counters.
type ShadowStats struct {
	QueriesCompared   uint64
	ExactMatchQueries uint64
	TotalLeaks        uint64
	ErrorCount        uint64
	LeaseExpiredCount uint64
}

// ShadowComparator tracks live parity and fail-closed visibility invariants during
// shadow-read rollouts before read cutover.
type ShadowComparator struct {
	queriesCompared   atomic.Uint64
	exactMatchQueries atomic.Uint64
	totalLeaks        atomic.Uint64
	errorCount        atomic.Uint64
	leaseExpiredCount atomic.Uint64
}

// NewShadowComparator creates a zeroed ShadowComparator.
func NewShadowComparator() *ShadowComparator {
	return &ShadowComparator{}
}

// RecordError records a failed shadow call without blocking the primary PG response.
func (s *ShadowComparator) RecordError(err error) {
	if err == nil {
		return
	}
	s.errorCount.Add(1)
	if errors.Is(err, ErrLeaseExpired) {
		s.leaseExpiredCount.Add(1)
	}
}

// CompareStrict verifies that every DH-TMS hit is authorized by the PostgreSQL reference
// set (zero visibility leaks) and computes overlap recall.
// Both 40-hex v1 and 64-hex v2 identities are matched case-insensitively.
func (s *ShadowComparator) CompareStrict(
	pgEligibleHashes []string,
	tmsHits []DocHit,
	pgDur, tmsDur time.Duration,
) ShadowDiff {
	pgSet := make(map[string]struct{}, len(pgEligibleHashes))
	for _, h := range pgEligibleHashes {
		norm := strings.ToLower(strings.TrimSpace(h))
		if norm != "" {
			pgSet[norm] = struct{}{}
		}
	}

	matchedPG := make(map[string]struct{}, len(tmsHits))
	var leaked []string

	for _, hit := range tmsHits {
		v1 := strings.ToLower(strings.TrimSpace(hit.InfoHashV1))
		v2 := strings.ToLower(strings.TrimSpace(hit.InfoHashV2))

		matched := false
		if v1 != "" {
			if _, ok := pgSet[v1]; ok {
				matchedPG[v1] = struct{}{}
				matched = true
			}
		}
		if v2 != "" {
			if _, ok := pgSet[v2]; ok {
				matchedPG[v2] = struct{}{}
				matched = true
			}
		}
		if !matched {
			id := v1
			if id == "" {
				id = v2
			}
			leaked = append(leaked, id)
		}
	}

	recall := 1.0
	if len(pgSet) > 0 {
		recall = float64(len(matchedPG)) / float64(len(pgSet))
	}
	exact := len(leaked) == 0 && len(matchedPG) == len(pgSet)

	s.queriesCompared.Add(1)
	if exact {
		s.exactMatchQueries.Add(1)
	}
	if len(leaked) > 0 {
		s.totalLeaks.Add(uint64(len(leaked)))
	}

	return ShadowDiff{
		PGCount:      len(pgSet),
		TMSCount:     len(tmsHits),
		OverlapCount: len(matchedPG),
		LeakedHashes: leaked,
		Recall:       recall,
		ExactSet:     exact,
		PGLatency:    pgDur,
		TMSLatency:   tmsDur,
	}
}

// Snapshot returns a consistent copy of accumulated counters.
func (s *ShadowComparator) Snapshot() ShadowStats {
	return ShadowStats{
		QueriesCompared:   s.queriesCompared.Load(),
		ExactMatchQueries: s.exactMatchQueries.Load(),
		TotalLeaks:        s.totalLeaks.Load(),
		ErrorCount:        s.errorCount.Load(),
		LeaseExpiredCount: s.leaseExpiredCount.Load(),
	}
}
