package classifier

import (
	"context"

	"github.com/spencernorton/DH-Waypoint/internal/classifier/classification"
	"github.com/spencernorton/DH-Waypoint/internal/model"
)

type runnerSemaphore struct {
	runner    Runner
	semaphore chan struct{}
}

func (r runnerSemaphore) Run(
	ctx context.Context,
	workflow string,
	flags Flags,
	t model.Torrent,
) (classification.Result, error) {
	select {
	case <-ctx.Done():
		return classification.Result{}, ctx.Err()
	case r.semaphore <- struct{}{}:
	}

	defer func() { <-r.semaphore }()

	return r.runner.Run(ctx, workflow, flags, t)
}
