package processors

import (
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/spencernorton/DH-Waypoint/internal/database/dao"
	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/serving"
	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func publicationConsumer(t *testing.T, p *pgxpool.Pool, id, artifact string) search.ServingSearch {
	return publicationConsumerForConfig(t, p, serving.Config{RequiredProcessor: id, RequiredProcessorArtifactDigest: artifact})
}

func publicationConsumerForConfig(t *testing.T, p *pgxpool.Pool, cfg serving.Config) search.ServingSearch {
	t.Helper()
	db := stdlib.OpenDB(*p.Config().ConnConfig)
	t.Cleanup(func() { _ = db.Close() })
	g, err := gorm.Open(postgres.New(postgres.Config{Conn: db}), &gorm.Config{Logger: logger.Discard})
	require.NoError(t, err)
	policy, err := serving.NewPolicy(cfg, []string{"syntheticadult"}, []string{"mkv"})
	require.NoError(t, err)
	q := lazy.New(func() (*dao.Query, error) { return dao.Use(g), nil })
	consumer, err := search.New(search.Params{Query: q, ServingPolicy: policy}).ServingSearch.Get()
	require.NoError(t, err)
	return consumer
}

func policyResult(j v1.Job) v1.Result {
	return v1.Result{Binding: j.Binding, Outcome: v1.Completed, Proposal: &v1.Proposal{AddTags: []string{"synthetic-reviewed"}}}
}

func TestRequiredPublicationReviewUsesCurrentSourceAndModel(t *testing.T) {
	s, p, _ := fixture(t)
	h := seedTorrent(t, p, 220, "tv_show")
	svc := newService(t, "synthetic-reviewer", []v1.Stage{v1.Policy}, policyResult)
	svc.identity.ArtifactDigest = strings.Repeat("a", 64)
	register(t, s, svc)
	c := publicationConsumer(t, p, svc.identity.ID, svc.identity.ArtifactDigest)
	visible := func(want int) {
		t.Helper()
		r, e := c.TorrentContent(testContext, query.WithTotalCount(true))
		require.NoError(t, e)
		require.Len(t, r.Items, want)
		require.Equal(t, uint(want), r.TotalCount)
	}
	visible(0)
	n, e := s.Scan(testContext)
	require.NoError(t, e)
	require.Equal(t, 1, n)
	run(t, s)
	visible(1)
	var approved bool
	require.NoError(t, p.QueryRow(testContext, `SELECT approved FROM processor_publication_reviews WHERE info_hash=$1 AND processor_id=$2`, h, svc.identity.ID).Scan(&approved))
	require.True(t, approved)
	_, e = p.Exec(testContext, `UPDATE torrent_contents SET seeders=9,updated_at=now() WHERE info_hash=$1`, h)
	require.NoError(t, e)
	visible(1)
	_, e = p.Exec(testContext, `UPDATE torrents SET name='Fictional.Show.S01E01.exe' WHERE info_hash=$1`, h)
	require.NoError(t, e)
	visible(0)
	n, e = s.Scan(testContext)
	require.NoError(t, e)
	require.Equal(t, 1, n)
	run(t, s)
	visible(0)
	require.NoError(t, p.QueryRow(testContext, `SELECT approved FROM processor_publication_reviews WHERE info_hash=$1 AND processor_id=$2`, h, svc.identity.ID).Scan(&approved))
	require.False(t, approved, "a permissive model cannot approve an executable as TV")
	_, e = p.Exec(testContext, `UPDATE torrents SET name='Fictional.Show.S01E01.mkv' WHERE info_hash=$1`, h)
	require.NoError(t, e)
	_, e = s.Scan(testContext)
	require.NoError(t, e)
	run(t, s)
	visible(1)
	_, e = p.Exec(testContext, `UPDATE processor_registry SET state='paused' WHERE id=$1`, svc.identity.ID)
	require.NoError(t, e)
	visible(0)
	_, e = p.Exec(testContext, `UPDATE processor_registry SET state='active',mode='shadow' WHERE id=$1`, svc.identity.ID)
	require.NoError(t, e)
	visible(0)
	_, e = p.Exec(testContext, `UPDATE processor_registry SET mode='apply',execution_generation=execution_generation+1 WHERE id=$1`, svc.identity.ID)
	require.NoError(t, e)
	visible(0)
}

func TestCanonicalIdentityStillReceivesPolicyOnlyReview(t *testing.T) {
	s, p, _ := fixture(t)
	h := seedTorrent(t, p, 221, "movie")
	_, e := p.Exec(testContext, `INSERT INTO torrent_canonical_labels(info_hash,media_type,media_id,resolved_source,resolved_strength,resolved_at) VALUES($1,'movie','tmdb:990001','synthetic',60,now())`, h)
	require.NoError(t, e)
	svc := newService(t, "synthetic-reviewer", []v1.Stage{v1.Classify, v1.Policy}, func(j v1.Job) v1.Result {
		require.Equal(t, v1.Policy, j.Stage)
		require.Equal(t, []v1.Stage{v1.Policy}, j.Capabilities)
		return policyResult(j)
	})
	svc.identity.ArtifactDigest = strings.Repeat("a", 64)
	register(t, s, svc)
	n, e := s.Scan(testContext)
	require.NoError(t, e)
	require.Equal(t, 1, n)
	run(t, s)
	c := publicationConsumer(t, p, svc.identity.ID, svc.identity.ArtifactDigest)
	r, e := c.TorrentContent(testContext)
	require.NoError(t, e)
	require.Len(t, r.Items, 1)
	var media string
	require.NoError(t, p.QueryRow(testContext, `SELECT media_id FROM torrent_canonical_labels WHERE info_hash=$1`, h).Scan(&media))
	require.Equal(t, "tmdb:990001", media)
	page, e := s.Jobs(testContext, 1, "", "")
	require.NoError(t, e)
	w := reprocessWrite(t, s, page.Items[0].JobID, "c")
	reprocessing, e := s.Reprocess(testContext, page.Items[0].JobID, w)
	require.NoError(t, e, "canonical policy-only review retains audited reprocessing")
	require.Equal(t, "waiting_core", reprocessing.State)
}

func TestPolicyFailuresStayRetryableAndNeverApprove(t *testing.T) {
	s, p, _ := fixture(t)
	h := seedTorrent(t, p, 222, "movie")
	var healthy atomic.Bool
	svc := newService(t, "synthetic-reviewer", []v1.Stage{v1.Policy}, func(j v1.Job) v1.Result {
		if healthy.Load() {
			return policyResult(j)
		}
		return v1.Result{Binding: j.Binding, Outcome: v1.Failed, Reasons: []string{"synthetic_unavailable"}}
	})
	svc.identity.ArtifactDigest = strings.Repeat("a", 64)
	register(t, s, svc)
	_, e := s.Scan(testContext)
	require.NoError(t, e)
	for i := 0; i < s.cfg.MaxAttempts+2; i++ {
		run(t, s)
		_, e = p.Exec(testContext, `UPDATE processor_jobs SET available_at=now()-interval '1 second' WHERE info_hash=$1`, h)
		require.NoError(t, e)
	}
	var state string
	require.NoError(t, p.QueryRow(testContext, `SELECT state FROM processor_jobs WHERE info_hash=$1`, h).Scan(&state))
	require.Equal(t, "deferred", state)
	var approvals int
	require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM processor_publication_reviews WHERE info_hash=$1 AND approved`, h).Scan(&approvals))
	require.Zero(t, approvals)
	healthy.Store(true)
	run(t, s)
	require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM processor_publication_reviews WHERE info_hash=$1 AND approved`, h).Scan(&approvals))
	require.Equal(t, 1, approvals)
}

func TestPublicationPayloadRequiresCompleteSafeMedia(t *testing.T) {
	count := int64(2)
	for _, ext := range []string{"exe", "scr", "zipx", "7z", "lnk", "ps1", "EXE", "mkv.exe"} {
		reason := publicationPayloadReason(source{Name: "Fictional.Show." + ext, Size: 100, FilesStatus: "single"}, "tv_show")
		require.NotEmpty(t, reason, ext)
	}
	require.Empty(t, publicationPayloadReason(source{Name: "Fictional.Show.mkv", Size: 100, FilesStatus: "single"}, "tv_show"))
	require.Equal(t, "incomplete_file_evidence", publicationPayloadReason(source{Size: 100, FilesStatus: "multi", FileCount: &count, Files: []sourceFile{{Path: "video.mkv", Size: 100}}}, "movie"))
	require.Equal(t, "unsafe_media_payload", publicationPayloadReason(source{Size: 100, FilesStatus: "multi", FileCount: &count, Files: []sourceFile{{Path: "video.mkv", Size: 99}, {Path: "codec.exe", Size: 1}}}, "movie"))
	require.Empty(t, publicationPayloadReason(source{Name: "Fictional.Installer.exe", Size: 100, FilesStatus: "single"}, "software"))
	for _, path := range []string{"../video.mkv", "/video.mkv", "drive:video.mkv", "folder/../video.mkv", "hidden\u202evideo.mkv"} {
		require.Equal(t, "unsafe_media_path", publicationPayloadReason(source{Name: path, Size: 100, FilesStatus: "single"}, "tv_show"))
	}
	require.Equal(t, "inconsistent_file_evidence", publicationPayloadReason(source{Size: 100, FilesStatus: "multi", FileCount: &count, Files: []sourceFile{{Path: "video.mkv", Size: 100}, {Path: "notes.nfo", Size: 1}}}, "movie"))
	require.Equal(t, "unsupported_media_sidecar", publicationPayloadReason(source{Size: 100, FilesStatus: "multi", FileCount: &count, Files: []sourceFile{{Path: "video.mkv", Size: 1}, {Path: "payload.bin", Size: 99}}}, "movie"))
}

func TestPublicationMigrationNeverApprovesExistingApplications(t *testing.T) {
	s, p, migrations := fixtureAt(t, 72)
	h := seedTorrent(t, p, 225, "movie")
	_, err := p.Exec(testContext, `INSERT INTO processor_applications(info_hash,job_id,input_revision,applied_revision,pipeline_revision,results) VALUES($1,repeat('a',64),repeat('b',64),repeat('b',64),repeat('c',64),'[]')`, h)
	require.NoError(t, err)
	_, err = migrations.Up(testContext)
	require.NoError(t, err)
	svc := newService(t, "synthetic-reviewer", []v1.Stage{v1.Policy}, policyResult)
	svc.identity.ArtifactDigest = strings.Repeat("a", 64)
	register(t, s, svc)
	c := publicationConsumer(t, p, svc.identity.ID, svc.identity.ArtifactDigest)
	page, err := c.TorrentContent(testContext)
	require.NoError(t, err)
	require.Empty(t, page.Items)
	_, err = s.Scan(testContext)
	require.NoError(t, err)
	run(t, s)
	page, err = c.TorrentContent(testContext)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
}

func TestNewHighHashGetsPriorityWithoutMovingBackfill(t *testing.T) {
	s, p, _ := fixture(t)
	seedTorrent(t, p, 2, "movie")
	high := seedTorrent(t, p, 250, "movie")
	svc := newService(t, "synthetic-reviewer", []v1.Stage{v1.Policy}, policyResult)
	register(t, s, svc)
	_, e := p.Exec(testContext, `UPDATE processor_control SET scan_after=decode('8000000000000000000000000000000000000000','hex');DELETE FROM processor_publication_pending WHERE info_hash<>$1`, high)
	require.NoError(t, e)
	s.cfg.ScanBatch = 1
	n, e := s.Scan(testContext)
	require.NoError(t, e)
	require.Equal(t, 1, n)
	var hash, cursor []byte
	require.NoError(t, p.QueryRow(testContext, `SELECT info_hash FROM processor_jobs`).Scan(&hash))
	require.Equal(t, high, hash)
	require.NoError(t, p.QueryRow(testContext, `SELECT scan_after FROM processor_control`).Scan(&cursor))
	require.Equal(t, byte(128), cursor[0])
}

func TestChangedAndRestoredSourceCannotReuseInflightReview(t *testing.T) {
	s, p, _ := fixture(t)
	h := seedTorrent(t, p, 223, "movie")
	var mutate atomic.Bool
	mutate.Store(true)
	svc := newService(t, "synthetic-reviewer", []v1.Stage{v1.Policy}, func(j v1.Job) v1.Result {
		if mutate.Swap(false) {
			// The stable metadata ends identical, but this result predates a
			// material source transition and cannot authorize the new revision.
			_, err := p.Exec(testContext, `UPDATE torrents SET name='Changed.mkv' WHERE info_hash=$1; UPDATE torrents SET name=$2 WHERE info_hash=$1`, h, j.Input.Name)
			require.NoError(t, err)
		}
		return policyResult(j)
	})
	register(t, s, svc)
	_, err := s.Scan(testContext)
	require.NoError(t, err)
	worked, err := s.RunOne(testContext)
	require.True(t, worked)
	require.NoError(t, err)
	var state string
	require.NoError(t, p.QueryRow(testContext, `SELECT state FROM processor_jobs`).Scan(&state))
	require.Equal(t, "stale", state)
	var count int
	require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM processor_publication_reviews WHERE approved`).Scan(&count))
	require.Zero(t, count)
	_, err = s.Scan(testContext)
	require.NoError(t, err)
	run(t, s)
	require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM processor_publication_reviews WHERE approved`).Scan(&count))
	require.Equal(t, 1, count)
}

func TestTerminalHistoryCannotStarveNewReview(t *testing.T) {
	s, p, _ := fixture(t)
	s.cfg.RetainedJobs = 1
	windowJob(t, p, "e")
	h := seedTorrent(t, p, 224, "movie")
	svc := newService(t, "synthetic-reviewer", []v1.Stage{v1.Policy}, policyResult)
	register(t, s, svc)
	n, err := s.Scan(testContext)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	var hash []byte
	require.NoError(t, p.QueryRow(testContext, `SELECT info_hash FROM processor_jobs`).Scan(&hash))
	require.Equal(t, h, hash)
	run(t, s)
}

func TestOversizedSourceCannotBlockNewReview(t *testing.T) {
	s, p, _ := fixture(t)
	bad := seedTorrent(t, p, 226, "movie")
	_, err := p.Exec(testContext, `INSERT INTO torrent_files(info_hash,"index",path,size,created_at,updated_at)
 SELECT $1,i,repeat('x',1024)||i::text||'/video.mkv',1,now(),now() FROM generate_series(1,300) i`, bad)
	require.NoError(t, err)
	good := seedTorrent(t, p, 227, "movie")
	svc := newService(t, "synthetic-reviewer", []v1.Stage{v1.Policy}, policyResult)
	register(t, s, svc)
	n, err := s.Scan(testContext)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	var hash []byte
	require.NoError(t, p.QueryRow(testContext, `SELECT info_hash FROM processor_jobs`).Scan(&hash))
	require.Equal(t, good, hash)
	run(t, s)
	var pending int
	require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM processor_publication_pending WHERE info_hash=$1`, bad).Scan(&pending))
	require.Zero(t, pending)
}
