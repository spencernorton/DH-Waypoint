package serving_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/serving"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm/clause"
)

func TestReleaseAudioContract(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pool, _ := newServingPostgresDatabase(t, ctx)
	for _, tc := range []struct {
		name, audio, want string
		anime             bool
		files             []string
	}{
		{"Fictional.Series.S01E01.1080p", "english", "english_main", false, nil},
		{"Only.Fictional.Tales.S01E01.1080p", "english", "english_main", false, nil},
		{"Fictional.English.Dub", "foreign", "foreign", true, nil},
		{"Fictional.English.Dub", "english", "english_dub", true, nil},
		{"Fictional.Dual.Audio.[JPN+ENG]", "english", "english_dub", true, nil},
		{"Fictional.Dual.Audio.[TAM+TEL]", "english", "conflict", true, nil},
		{"Fictional.English.Subs", "english", "conflict", true, nil},
		{"Fictional.English.AAC.Commentary", "english", "conflict", true, nil},
		{"Fictional.French.River.2024.1080p", "english", "english_main", false, nil},
		{"Fictional.2024.French.1080p", "english", "conflict", false, nil},
		{"Fictional.2024.Spanish.Subs", "english", "english_main", false, nil},
		{"Fictional.Series.S01E01.English.Dub.Only.mkv", "english", "english_dub", true, nil},
		{"Fictional.Not.English.Audio", "english", "conflict", true, nil},
		{"Fictional.1080p.Japanese.Audio.English.AAC.2.0.Commentary.mkv", "english", "conflict", true, nil},
		{"Fictional.[English Audio][English AAC 2.0 Commentary]", "english", "english_dub", true, nil},
		{"Fictional.English.Dub.Commentary", "english", "conflict", true, nil},
		{"Fictional.Dual.Audio", "english", "english_dub", true, []string{"01.English.Dub.mkv", "02.English.Dub.mkv"}},
		{"Fictional.Complete.English.Dub.Episodes.1-2.Only", "english", "unknown_pack_audio", true, []string{"01.mkv", "02.mkv", "03.mkv"}},
		{"Fictional.No-English.Audio", "english", "conflict", false, nil},
		{"Fictional.English.Dub.English.Subs", "english", "english_dub", true, nil},
		{"Fictional.Release", "english", "unknown_dub", true, nil},
		{"Fictional.Complete.English.Dub", "english", "conflict", true, []string{"English Dub/01.mkv", "English Dub/Japanese Audio/02.flv"}},
		{"Fictional.Season.Complete.English.Dub", "english", "conflict", true, []string{"01.English.Dub.mkv", "02.Japanese.Audio.mkv"}},
		{"Fictional.Season.Complete.English.Dub", "english", "english_dub", true, []string{"01.mkv", "02.mkv"}},
		{"Fictional.Season", "english", "unknown_pack_audio", true, []string{"01.English.Dub.mkv", "02.mkv"}},
		{"Fictional.Season", "english", "english_dub", true, []string{"01.English.Dub.mkv", "02.English.Dub.mkv"}},
	} {
		t.Run(tc.name+"_"+tc.want, func(t *testing.T) {
			result, err := json.Marshal(map[string]any{"outcome": "completed", "trace": map[string]any{
				"audio_language": tc.audio, "anime": tc.anime, "neural_heads": map[string]string{"category": "tv_show"}},
				"proposal": map[string]any{"language": map[string]any{"audio": []string{"en"}, "evidence": "metadata_claim"}}})
			require.NoError(t, err)
			var got string
			require.NoError(t, pool.QueryRow(ctx, "SELECT publication_release_audio_v1($1,$2,$3)", result, tc.name, tc.files).Scan(&got))
			require.Equal(t, tc.want, got)
		})
	}
}

func TestReleaseAudioBoundReviewAndRetry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pool, q := newServingPostgresDatabase(t, ctx)
	h := []byte(strings.Repeat("h", 20))
	artifact := strings.Repeat("a", 64)
	identity := `{"id":"synthetic-reviewer","version":"1","artifact_digest":"` + artifact + `"}`
	exec := func(q string, args ...any) { t.Helper(); _, err := pool.Exec(ctx, q, args...); require.NoError(t, err) }
	exec(`INSERT INTO torrents(info_hash,name,size,private,files_status,created_at,updated_at) VALUES($1,'Fictional.Release.mkv',100,false,'single',now(),now())`, h)
	exec(`INSERT INTO torrent_contents(info_hash,content_type,size,published_at,created_at,updated_at) VALUES($1,'movie',100,now(),now(),now())`, h)
	exec(`INSERT INTO processor_registry(id,generation,execution_generation,endpoint,mode,state,priority,manifest,configuration) VALUES('synthetic-reviewer',1,1,'http://processor.invalid','apply','active',1,jsonb_build_object('processor',$1::jsonb,'capabilities','["policy"]'::jsonb),'{}')`, identity)
	exec(`INSERT INTO processor_applications(info_hash,job_id,input_revision,applied_revision,pipeline_revision,results)
	VALUES($1,'other-job','a','a','a',jsonb_build_array(jsonb_build_object('result',jsonb_build_object('processor',$2::jsonb,'outcome','completed','trace','{"audio_language":"english","anime":false,"neural_heads":{"category":"movie"}}'::jsonb,
	'proposal','{"language":{"audio":["en"],"evidence":"metadata_claim"}}'::jsonb))))`, h, identity)
	exec(`INSERT INTO processor_publication_reviews(info_hash,processor_id,source_revision,execution_generation,processor_identity,approved,policy_contract,reason,job_id)
	SELECT $1,'synthetic-reviewer',revision,1,$2,true,$3,'approved','review-job' FROM processor_publication_sources WHERE info_hash=$1`, h, identity, serving.PublicationReviewPolicy)
	policy, err := serving.NewPolicy(serving.Config{RequiredProcessor: "synthetic-reviewer", RequiredProcessorArtifactDigest: artifact, RequireEnglishMainAudio: true}, []string{"syntheticadult"}, []string{"mkv"})
	require.NoError(t, err)
	probe := func(expected string, visible bool) {
		t.Helper()
		var got string
		require.NoError(t, pool.QueryRow(ctx, "SELECT release_audio_v1 FROM processor_publication_reviews WHERE info_hash=$1", h).Scan(&got))
		require.Equal(t, expected, got)
		for _, predicate := range []func(string) (clause.Expr, bool){policy.PublicationGateCondition, policy.PublicationApprovedSQL} {
			gate, ok := predicate("torrents")
			require.True(t, ok)
			var count int64
			err := q.Torrent.UnderlyingDB().WithContext(ctx).Table("torrents").Where("info_hash = ?", h).Where(gate).Count(&count).Error
			require.NoError(t, err)
			require.Equal(t, visible, count == 1)
		}
	}
	probe("missing_receipt", false)
	var requests int
	retry := "SELECT publication_retry_missing_audio_v1('synthetic-reviewer',$1,1)"
	require.NoError(t, pool.QueryRow(ctx, retry, artifact).Scan(&requests))
	require.Equal(t, 1, requests)
	require.NoError(t, pool.QueryRow(ctx, retry, artifact).Scan(&requests))
	require.Zero(t, requests)
	probe("retry_requested", false)
	exec("UPDATE processor_applications SET job_id='review-job' WHERE info_hash=$1", h)
	exec("UPDATE processor_publication_reviews SET release_audio_v1=NULL WHERE info_hash=$1", h)
	var n int
	batch := `SELECT coalesce(sum(n),0) FROM publication_reconcile_audio_v1('synthetic-reviewer',$1,1)`
	require.NoError(t, pool.QueryRow(ctx, batch, artifact).Scan(&n))
	require.Equal(t, 1, n)
	probe("english_main", true)
	require.NoError(t, pool.QueryRow(ctx, batch, artifact).Scan(&n))
	require.Zero(t, n)
	// A legacy NULL extension or alternate playable container cannot hide an
	// episode from the same released manifest used by publication review.
	exec("UPDATE torrents SET files_status='multi',files_count=2 WHERE info_hash=$1", h)
	exec(`INSERT INTO torrent_files(info_hash,"index",path,size,created_at,updated_at) VALUES
        ($1,0,'English Dub/01.mkv',50,now(),now()),($1,1,'Japanese Audio/02.ogm',50,now(),now())`, h)
	var mixed string
	require.NoError(t, pool.QueryRow(ctx, "SELECT publication_audio_for_review_v1($1,'review-job',$2)", h, identity).Scan(&mixed))
	require.Equal(t, "conflict", mixed)
	exec("UPDATE torrent_files SET path='Japanese Audio/02.flv' WHERE info_hash=$1 AND \"index\"=1", h)
	require.NoError(t, pool.QueryRow(ctx, "SELECT publication_audio_for_review_v1($1,'review-job',$2)", h, identity).Scan(&mixed))
	require.Equal(t, "conflict", mixed)
	exec("UPDATE torrent_contents SET content_type='game' WHERE info_hash=$1", h)
	require.NoError(t, pool.QueryRow(ctx, "SELECT publication_audio_for_review_v1($1,'review-job',$2)", h, identity).Scan(&mixed))
	require.Equal(t, "unresolved_media_type", mixed)

	exec("UPDATE processor_publication_sources SET revision=revision+1 WHERE info_hash=$1", h)
	probe("english_main", false)
}
