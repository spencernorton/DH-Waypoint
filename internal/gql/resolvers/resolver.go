package resolvers

import (
	"github.com/spencernorton/DH-Waypoint/internal/blocking"
	"github.com/spencernorton/DH-Waypoint/internal/database/dao"
	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/evidence"
	"github.com/spencernorton/DH-Waypoint/internal/health"
	"github.com/spencernorton/DH-Waypoint/internal/metrics/queuemetrics"
	"github.com/spencernorton/DH-Waypoint/internal/metrics/torrentmetrics"
	"github.com/spencernorton/DH-Waypoint/internal/processor"
	"github.com/spencernorton/DH-Waypoint/internal/queue/manager"
	"github.com/spencernorton/DH-Waypoint/internal/worker"
)

// This file will not be regenerated automatically.
//
// It serves as dependency injection for your app, add any dependencies you require here.

type Resolver struct {
	Dao                  *dao.Query
	Search               search.Search
	Workers              worker.Registry
	Checker              health.Checker
	QueueMetricsClient   queuemetrics.Client
	QueueManager         manager.Manager
	TorrentMetricsClient torrentmetrics.Client
	Processor            processor.Processor
	BlockingManager      blocking.Manager
	EvidenceStore        *evidence.Store
}
