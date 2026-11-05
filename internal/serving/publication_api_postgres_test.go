package serving_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/99designs/gqlgen/graphql"
	"github.com/gin-gonic/gin"
	"github.com/spencernorton/DH-Waypoint/internal/database/dao"
	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/gql"
	gqlhttp "github.com/spencernorton/DH-Waypoint/internal/gql/httpserver"
	"github.com/spencernorton/DH-Waypoint/internal/gql/resolvers"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/namepolicy"
	namehttp "github.com/spencernorton/DH-Waypoint/internal/namepolicy/httpserver"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/restapi"
	"github.com/spencernorton/DH-Waypoint/internal/serving"
	"github.com/spencernorton/DH-Waypoint/internal/torznab"
	"github.com/spencernorton/DH-Waypoint/internal/torznab/adapter"
	torznabhttp "github.com/spencernorton/DH-Waypoint/internal/torznab/httpserver"
	"github.com/stretchr/testify/require"
)

// Every public handler uses real SQL and the same mandatory consumer policy.
// The manually inserted proof is deliberately varied to exercise its authority;
// processors' integration tests separately cover transactional proof creation.
func TestRequiredPublicationReviewAcrossNativeConsumerAPIs(t *testing.T) {
	for _, payload := range []bool{false, true} {
		name := "model"
		if payload {
			name = "model_and_payload"
		}
		t.Run(name, func(t *testing.T) { requiredPublicationAPIs(t, payload) })
	}
}

func requiredPublicationAPIs(t *testing.T, payload bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	p, q := newServingPostgresDatabase(t, ctx)
	h := protocol.InfoHash{0x45}
	v2 := strings.Repeat("67", 32)
	artifact := strings.Repeat("a", 64)
	identity := `{"id":"synthetic-reviewer","version":"1.0.0","artifact_digest":"` + artifact + `"}`
	exec := func(sql string, args ...any) { t.Helper(); _, e := p.Exec(ctx, sql, args...); require.NoError(t, e) }
	exec(`INSERT INTO torrents(info_hash,info_hash_v2,name,size,private,files_status,files_count,created_at,updated_at) VALUES($1,decode($2,'hex'),'Fictional.Release.mkv',4096,false,'multi',1,now(),now())`, h.Bytes(), v2)
	exec(`INSERT INTO torrent_files(info_hash,"index",path,size,created_at,updated_at) VALUES($1,0,'Fictional.Release.mkv',4096,now(),now())`, h.Bytes())
	exec(`INSERT INTO torrent_contents(info_hash,content_type,size,published_at,created_at,updated_at) VALUES($1,'movie',4096,now(),now(),now())`, h.Bytes())
	exec(`INSERT INTO processor_registry(id,generation,execution_generation,endpoint,mode,state,priority,manifest,configuration)
 VALUES('synthetic-reviewer',1,1,'http://synthetic.invalid','apply','active',1,jsonb_build_object('processor',$1::jsonb,'capabilities',jsonb_build_array('policy')),'{}')`, identity)
	cfg := serving.Config{RequiredProcessor: "synthetic-reviewer", RequiredProcessorArtifactDigest: artifact}
	if payload {
		cfg.RequiredPayloadVerifierArtifactDigest = strings.Repeat("c", 64)
	}
	policy, e := serving.NewPolicy(cfg, []string{"syntheticadult"}, []string{"mkv"})
	require.NoError(t, e)
	consumer := search.New(search.Params{Query: lazy.New(func() (*dao.Query, error) { return q, nil }), ServingPolicy: policy}).ServingSearch
	engine := gin.New()
	s, e := consumer.Get()
	require.NoError(t, e)
	es := gql.NewExecutableSchema(gql.Config{Resolvers: &resolvers.Resolver{Search: s, Dao: q}})
	require.NoError(t, gqlhttp.New(gqlhttp.Params{Schema: lazy.New(func() (graphql.ExecutableSchema, error) { return es, nil })}).Option.Apply(engine))
	require.NoError(t, restapi.New(consumer, nil).Apply(engine))
	client := adapter.NewWithFilters(s, nil, false, nil, adapter.FreshnessConfig{})
	require.NoError(t, torznabhttp.NewWithReview(lazy.New(func() (torznab.Client, error) { return client, nil }), torznab.NewDefaultConfig(), nil, cfg).Apply(engine))
	token := strings.Repeat("synthetic", 8)
	names, e := namepolicy.New(namepolicy.Config{Enabled: true, InternalCheckToken: namepolicy.SecretToken(token)})
	require.NoError(t, e)
	require.NoError(t, namehttp.NewPublicHashWithReview(consumer, names, cfg).Apply(engine))
	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, bytes.NewBufferString(body)).WithContext(ctx)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, r)
		return w
	}
	check := func(visible bool) {
		t.Helper()
		w := request(http.MethodPost, "/graphql", `{"query":"{torrentContent{search(input:{totalCount:true}){totalCount items{infoHash}}} torrent{files(input:{totalCount:true}){totalCount items{path}}}}"}`)
		require.Equal(t, 200, w.Code)
		require.NotContains(t, w.Body.String(), `"errors"`)
		var doc struct {
			Data map[string]map[string]struct {
				TotalCount int
				Items      []any
			}
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &doc))
		want := 0
		code := 404
		if visible {
			want = 1
			code = 200
		}
		for _, field := range []struct{ root, key string }{{"torrentContent", "search"}, {"torrent", "files"}} {
			require.Equal(t, want, doc.Data[field.root][field.key].TotalCount)
			require.Len(t, doc.Data[field.root][field.key].Items, want)
		}
		for _, path := range []string{"/api/v1/torrents/" + h.String(), "/api/v2/torrents/" + v2} {
			require.Equal(t, code, request(http.MethodGet, path, "").Code, path)
		}
		w = request(http.MethodGet, "/api/v1/torrents/"+h.String()+"/files", "")
		require.Equal(t, 200, w.Code)
		var files struct {
			Items []any `json:"files"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &files))
		require.Len(t, files.Items, want)
		w = request(http.MethodGet, "/torznab/api?t=search", "")
		require.Equal(t, 200, w.Code)
		require.Equal(t, serving.PublicationReviewContract, w.Header().Get("X-Publication-Review"))
		if payload {
			require.Equal(t, serving.PayloadEvidenceContract, w.Header().Get("X-Publication-Payload"))
		}
		require.Equal(t, visible, strings.Contains(w.Body.String(), "<item>"))
		w = request(http.MethodPost, "/internal/name-policy/check-public", `{"infoHashes":["`+h.String()+`","`+v2+`"]}`)
		require.Equal(t, 200, w.Code)
		require.Equal(t, serving.PublicationReviewContract, w.Header().Get("X-Publication-Review"))
		if payload {
			require.Equal(t, serving.PayloadEvidenceContract, w.Header().Get("X-Publication-Payload"))
		}
		require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
		var grants struct{ Results []struct{ Eligible bool } }
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &grants))
		require.Len(t, grants.Results, 2)
		for _, grant := range grants.Results {
			require.Equal(t, visible, grant.Eligible)
		}
	}
	approve := func() {
		t.Helper()
		exec(`UPDATE processor_registry SET mode='apply',state='active',execution_generation=1 WHERE id='synthetic-reviewer'`)
		exec(`INSERT INTO processor_publication_reviews(info_hash,processor_id,source_revision,execution_generation,processor_identity,approved,reason,job_id)
 SELECT info_hash,'synthetic-reviewer',revision,1,$2::jsonb,true,'approved',repeat('a',64) FROM processor_publication_sources WHERE info_hash=$1
 ON CONFLICT(info_hash,processor_id) DO UPDATE SET source_revision=EXCLUDED.source_revision,execution_generation=1,processor_identity=EXCLUDED.processor_identity,approved=true`, h.Bytes(), identity)
		if payload {
			exec(`INSERT INTO processor_payload_reviews(info_hash,source_revision,verifier_artifact,policy,approved,reason,receipt,expires_at)
 SELECT info_hash,revision,$2,$3,true,'synthetic_fixture','{}',now()+interval '1 day' FROM processor_publication_sources WHERE info_hash=$1
 ON CONFLICT(info_hash) DO UPDATE SET source_revision=EXCLUDED.source_revision,verifier_artifact=EXCLUDED.verifier_artifact,policy=EXCLUDED.policy,approved=true,expires_at=EXCLUDED.expires_at`, h.Bytes(), cfg.RequiredPayloadVerifierArtifactDigest, serving.PayloadEvidenceContract)
		}
	}
	check(false)
	approve()
	check(true)
	if payload {
		for _, mutation := range []string{
			`DELETE FROM processor_payload_reviews`,
			`UPDATE processor_payload_reviews SET approved=false`,
			`UPDATE processor_payload_reviews SET expires_at=now()-interval '1 second'`,
			`UPDATE processor_payload_reviews SET source_revision=source_revision+1`,
			`UPDATE processor_payload_reviews SET verifier_artifact=repeat('d',64)`,
			`UPDATE processor_payload_reviews SET policy='retired-policy'`,
		} {
			exec(mutation)
			check(false)
			approve()
			check(true)
		}
	}
	for _, mutation := range []string{
		`UPDATE processor_publication_reviews SET approved=false`,
		`UPDATE processor_publication_reviews SET execution_generation=2`,
		`UPDATE processor_publication_reviews SET processor_identity=jsonb_set(processor_identity,'{artifact_digest}',to_jsonb(repeat('b',64)))`,
		`UPDATE processor_registry SET state='paused'`,
		`UPDATE processor_registry SET mode='shadow'`,
		`UPDATE processor_registry SET execution_generation=2`,
		`UPDATE torrent_files SET path='Replacement.mkv'`,
		`UPDATE torrent_contents SET release_group='changed'`,
		`INSERT INTO torrent_tags(info_hash,name,created_at,updated_at) SELECT info_hash,'changed',now(),now() FROM torrents`,
	} {
		exec(mutation)
		check(false)
		approve()
		check(true)
	}
	exec(`INSERT INTO processor_exclusions(info_hash,job_id,processor_id,binding,reason) VALUES($1,'synthetic','synthetic-reviewer','{}','synthetic_hold')`, h.Bytes())
	approve() // Even a positive proof cannot override a separate negative hold.
	check(false)
	exec(`DELETE FROM processor_exclusions`)
	check(false) // Removing a hold requires a fresh review.
	approve()
	check(true)
}
