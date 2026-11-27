package processors

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// These jobs are already admitted. Frozen mapping evidence and availability
// vary independently so the claim tests do not rely on how the scanner sorts.
func seedDispatchJobs(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(testContext, `INSERT INTO processor_jobs
 (job_id,info_hash,input_revision,pipeline_revision,epoch,input,source,plan,created_at,available_at)
 SELECT lpad(i::text,64,'0'),decode(lpad(i::text,40,'0'),'hex'),repeat('a',64),repeat('b',64),epoch,
 '{}'::jsonb,jsonb_build_object('contents',jsonb_build_array(jsonb_build_object('content_type','movie',
 'content_source','tmdb','content_id',CASE WHEN i>=4 THEN i::text ELSE '' END))),
 '{}'::jsonb,statement_timestamp()-interval '1 hour'+i*interval '1 second',
 statement_timestamp()-interval '1 hour'+i*interval '1 second'
 FROM processor_control CROSS JOIN generate_series(1,12) i WHERE singleton`)
	require.NoError(t, err)
}

func TestPostgresMappedTurnsRecheckDueJobsAndKeepOldestMoving(t *testing.T) {
	s, pool, _ := fixture(t)
	s.cfg.Concurrency = 16
	s.cfg.PreferMappedMedia = true
	seedDispatchJobs(t, pool)
	claim := func(want int) {
		t.Helper()
		lease, err := s.Claim(testContext)
		require.NoError(t, err)
		require.NotNil(t, lease)
		require.Equal(t, fmt.Sprintf("%064d", want), lease.ID)
	}
	claim(1)
	claim(4)
	// A mapped hint was cached previously. A deferred retry and an obsolete
	// epoch are ineligible even if they were due when the hints were read.
	_, err := pool.Exec(testContext, `UPDATE processor_jobs SET state='deferred',available_at=now()+interval '1 hour' WHERE job_id=$1;
 UPDATE processor_jobs SET epoch=epoch+1 WHERE job_id=$2`, fmt.Sprintf("%064d", 5), fmt.Sprintf("%064d", 6))
	require.NoError(t, err)
	claim(2)
	claim(7)
	claim(3)
	s.cfg.PreferMappedMedia = false
	claim(8)
}

func TestPostgresDispatchFencesAndCrossProcessCapacity(t *testing.T) {
	s, pool, _ := fixture(t)
	s.cfg.Concurrency = 2
	other, err := NewStore(s.cfg, s.pool, s.policy)
	require.NoError(t, err)
	seedDispatchJobs(t, pool)

	admin, err := pool.Begin(testContext)
	require.NoError(t, err)
	defer rollback(admin)
	_, err = admin.Exec(testContext, `SELECT epoch FROM processor_control WHERE singleton FOR UPDATE`)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(testContext, 100*time.Millisecond)
	defer cancel()
	_, err = s.Claim(ctx)
	require.Error(t, err)
	require.NoError(t, admin.Rollback(testContext))

	scan, err := pool.Begin(testContext)
	require.NoError(t, err)
	defer rollback(scan)
	_, err = scan.Exec(testContext, `SELECT scan_after FROM processor_control WHERE singleton FOR NO KEY UPDATE`)
	require.NoError(t, err)
	_, _, err = readPlan(testContext, scan, s.policy)
	require.NoError(t, err)
	type result struct {
		lease *Lease
		err   error
	}
	var wg sync.WaitGroup
	results := make(chan result, 8)
	for i := range 8 {
		worker := []*Store{s, other}[i%2]
		wg.Go(func() { lease, err := worker.Claim(testContext); results <- result{lease, err} })
	}
	wg.Wait()
	close(results)
	claimed := map[string]bool{}
	for r := range results {
		require.NoError(t, r.err)
		if r.lease != nil {
			require.False(t, claimed[r.lease.ID])
			claimed[r.lease.ID] = true
		}
	}
	require.Len(t, claimed, 2)
	var leased int
	require.NoError(t, pool.QueryRow(testContext, `SELECT count(*) FROM processor_jobs WHERE state='leased'`).Scan(&leased))
	require.Equal(t, 2, leased)
}
