package manager

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPurgeJobsRefusesAnEmptyRequest(t *testing.T) {
	// The zero value has no database, so reaching a query would panic.
	require.Error(t, manager{}.PurgeJobs(context.Background(), PurgeJobsRequest{}))
}
