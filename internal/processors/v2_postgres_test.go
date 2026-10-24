package processors

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/cataloguerecovery"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
	v2 "github.com/spencernorton/DH-Waypoint/processorapi/v2"
	"github.com/stretchr/testify/require"
)

func TestPostgresV2ProcessorNegotiationPreservesStrictV1Services(t *testing.T) {
	s, p, _ := fixture(t)
	v1hash := seedTorrent(t, p, 93, nil)
	h := protocol.InfoHashV2(sha256.Sum256([]byte("synthetic v2 torrent")))
	_, err := p.Exec(testContext, `INSERT INTO torrents(info_hash,info_hash_v2,name,size,private,files_status,created_at,updated_at)
 VALUES($1,$1,'Synthetic.V2.txt',14,false,'single',now(),now());
 INSERT INTO torrent_contents(info_hash,size,created_at,updated_at) VALUES($1,14,now(),now())`, h.Bytes())
	require.NoError(t, err)
	legacy := newService(t, "legacy", []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result {
		require.NoError(t, j.Validate())
		require.Len(t, j.Input.InfoHash, 40)
		return result(j, "movie")
	})
	register(t, s, legacy)
	n, err := s.Scan(testContext)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	run(t, s)
	require.EqualValues(t, 1, legacy.calls.Load())
	var v2jobs int
	require.NoError(t, p.QueryRow(testContext, "SELECT count(*) FROM processor_jobs WHERE info_hash=$1", h.Bytes()).Scan(&v2jobs))
	require.Zero(t, v2jobs)
	identity := v1.Identity{ID: "modern", Version: "1.0.0", ModelVersion: "none", PolicyVersion: "synthetic", ArtifactDigest: "none"}
	var mu sync.Mutex
	var inputs []string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/manifest":
			json.NewEncoder(w).Encode(v2.Manifest{Contract: v2.Contract, Processor: identity, Capabilities: []v1.Stage{v1.Classify}, ConfigurationSchema: json.RawMessage(`{"type":"object"}`), Qualification: "synthetic"})
		case "/v1/status":
			json.NewEncoder(w).Encode(v2.ServiceStatus{Contract: v2.Contract, Processor: identity, State: "ready", InstanceID: "synthetic", StartedAt: "2026-01-01T00:00:00Z"})
		case "/v2/process":
			var j v2.Job
			require.NoError(t, json.NewDecoder(r.Body).Decode(&j))
			require.NoError(t, j.Validate())
			mu.Lock()
			inputs = append(inputs, j.Input.InfoHash)
			mu.Unlock()
			json.NewEncoder(w).Encode(v2.Result{Binding: j.Binding, Outcome: v1.Completed, Proposal: &v1.Proposal{ContentType: "movie"}, Reasons: []string{}, Evidence: []v1.Evidence{}, RequestFeedback: true})
		default:
			w.WriteHeader(404)
		}
	}))
	defer peer.Close()
	_, err = s.Register(testContext, identity.ID, v1.RegistryWrite{Endpoint: peer.URL, Mode: "apply", State: "active", Configuration: json.RawMessage(`{}`)})
	require.NoError(t, err)
	n, err = s.Scan(testContext)
	require.NoError(t, err)
	require.Equal(t, 2, n)
	for range 6 {
		worked, err := s.RunOne(testContext)
		require.NoError(t, err)
		if !worked {
			break
		}
	}
	mu.Lock()
	require.Contains(t, inputs, h.String())
	require.Contains(t, inputs, protocol.MustNewInfoHash(v1hash).String())
	mu.Unlock()
	var kind string
	require.NoError(t, p.QueryRow(testContext, "SELECT content_type FROM torrent_contents WHERE info_hash=$1", h.Bytes()).Scan(&kind))
	require.Equal(t, "movie", kind)
	feedback, err := s.Feedback(testContext, identity.ID, 0, 100, "", "")
	require.NoError(t, err)
	require.Equal(t, v2.FeedbackContract, feedback.Contract)
	var found bool
	for _, ev := range feedback.Items {
		if ev.InfoHash == h.String() {
			found = true
		}
	}
	require.True(t, found)
	legacyFeedback, err := s.Feedback(testContext, "", 0, 100, "", "")
	require.NoError(t, err)
	require.Equal(t, v1.FeedbackContract, legacyFeedback.Contract)
	for _, ev := range legacyFeedback.Items {
		require.Len(t, ev.InfoHash, 40)
	}
	require.NoError(t, s.Refresh(testContext))
	status, err := s.Status(testContext)
	require.NoError(t, err)
	for _, entry := range status.Processors {
		require.Equal(t, "compatible", entry.Compatibility)
	}
}

func TestPostgresIdentityMigrationPreservesV1AndGuardsDowngrade(t *testing.T) {
	_, p, provider := fixtureAt(t, 69)
	h := seedTorrent(t, p, 94, nil)
	_, err := provider.Up(testContext)
	require.NoError(t, err)
	var length int
	var alias []byte
	require.NoError(t, p.QueryRow(testContext, "SELECT octet_length(info_hash),info_hash_v2 FROM torrents WHERE info_hash=$1", h).Scan(&length, &alias))
	require.Equal(t, 20, length)
	require.Nil(t, alias)
	_, err = provider.Down(testContext)
	require.NoError(t, err) // concurrent name index
	_, err = provider.Down(testContext)
	require.NoError(t, err) // identity schema, v1-only data
	require.NoError(t, p.QueryRow(testContext, "SELECT octet_length(info_hash) FROM torrents WHERE info_hash=$1", h).Scan(&length))
	require.Equal(t, 20, length)
	_, err = provider.Up(testContext)
	require.NoError(t, err)
	v2hash := protocol.InfoHashV2(sha256.Sum256([]byte("synthetic hybrid alias")))
	_, err = p.Exec(testContext, "UPDATE torrents SET info_hash_v2=$2 WHERE info_hash=$1", h, v2hash.Bytes())
	require.NoError(t, err)
	_, err = provider.Down(testContext)
	require.NoError(t, err)
	_, err = provider.Down(testContext)
	require.ErrorContains(t, err, "v2 identities prevent downgrade")
	require.NoError(t, p.QueryRow(testContext, "SELECT info_hash_v2 FROM torrents WHERE info_hash=$1", h).Scan(&alias))
	require.Equal(t, v2hash.Bytes(), alias)
	_, err = p.Exec(testContext, "DELETE FROM torrents")
	require.NoError(t, err)
	_, err = p.Exec(testContext, `INSERT INTO processor_core_receipts(info_hash,source_digest,content_id,core_policy,processing_outcome,outcome)
 VALUES($1,$2,'synthetic','synthetic','not_attempted','not_attempted')`, v2hash.Bytes(), make([]byte, 32))
	require.NoError(t, err)
	_, err = provider.Down(testContext)
	require.ErrorContains(t, err, "v2 evidence")
}

func TestPostgresRemovedHybridRetainsAliasAndPreventsUnsafeDowngrade(t *testing.T) {
	_, pool, m := fixture(t)
	h := seedTorrent(t, pool, 95, "movie")
	v2hash := protocol.InfoHashV2(sha256.Sum256([]byte("synthetic removed hybrid")))
	_, err := pool.Exec(testContext, `UPDATE torrents SET info_hash_v2=$2 WHERE info_hash=$1`, h, v2hash.Bytes())
	require.NoError(t, err)
	recovery, err := cataloguerecovery.NewStore(pool, cataloguerecovery.Config{Enabled: true, Retention: 24 * time.Hour, MaxPayloadBytes: 64 << 20, MaxSnapshotBytes: 8 << 20, MaxSnapshots: 128, MaxStorageBytes: 128 << 20, MaxRowsPerSnapshot: 1024})
	require.NoError(t, err)
	snapshot, err := recovery.Remove(testContext, h, "blocking", "synthetic", time.Now())
	require.NoError(t, err)
	_, err = m.Down(testContext)
	require.NoError(t, err)
	_, err = m.Down(testContext)
	require.ErrorContains(t, err, "v2 evidence in retained snapshots")
	restored, err := recovery.Restore(testContext, snapshot.ID, time.Now())
	require.NoError(t, err)
	require.True(t, restored)
	var alias []byte
	require.NoError(t, pool.QueryRow(testContext, `SELECT info_hash_v2 FROM torrents WHERE info_hash=$1`, h).Scan(&alias))
	require.Equal(t, v2hash.Bytes(), alias)
}
