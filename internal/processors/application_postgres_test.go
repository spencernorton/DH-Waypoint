package processors

import (
	"encoding/hex"
	"github.com/spencernorton/DH-Waypoint/internal/classifier/contentfilter"
	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestMatchApplicationRequiresUniqueCurrentIdentityAndPreservesSearch(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		name := "unique"
		if ambiguous {
			name = "identity_added_during_inference"
		}
		t.Run(name, func(t *testing.T) {
			s, p, _ := fixture(t)
			h := seedTorrent(t, p, 51, "movie")
			_, e := p.Exec(testContext, `INSERT INTO content(type,source,id,title,release_year,tsv,created_at,updated_at) VALUES('movie','tmdb','991051','Synthetic Amber',2024,to_tsvector('simple','Synthetic Amber 2024 preservedalias'),now(),now())`)
			require.NoError(t, e)
			a := newService(t, "matcher", []v1.Stage{v1.Match}, func(j v1.Job) v1.Result {
				return v1.Result{Binding: j.Binding, Outcome: v1.Completed, Proposal: &v1.Proposal{Match: &v1.MatchRef{Source: "tmdb", ID: "991051"}}}
			})
			register(t, s, a)
			_, e = s.Scan(testContext)
			require.NoError(t, e)
			lease, e := s.Claim(testContext)
			require.NoError(t, e)
			require.NotNil(t, lease)
			job, e := s.Prepare(testContext, *lease)
			require.NoError(t, e)
			require.Len(t, job.Input.Candidates, 1)
			if ambiguous {
				_, e = p.Exec(testContext, `INSERT INTO content(type,source,id,title,release_year,tsv,created_at,updated_at) VALUES('movie','tmdb','991052','Synthetic Amber',2024,to_tsvector('simple','Synthetic Amber 2024'),now(),now())`)
				require.NoError(t, e)
			}
			r := v1.Result{Binding: job.Binding, Outcome: v1.Completed, Proposal: &v1.Proposal{Match: &v1.MatchRef{Source: "tmdb", ID: "991051"}}}
			require.NoError(t, s.Finish(testContext, *lease, job, r, false))
			var id *string
			var preserved bool
			require.NoError(t, p.QueryRow(testContext, `SELECT content_id,COALESCE(tsv @@ plainto_tsquery('simple','preservedalias'),false) FROM torrent_contents WHERE info_hash=$1`, h).Scan(&id, &preserved))
			if ambiguous {
				require.Nil(t, id)
			} else {
				require.NotNil(t, id)
				require.Equal(t, "991051", *id)
				require.True(t, preserved)
			}
		})
	}
}

func TestTypeProposalHonorsExistingFileAndLanguagePolicy(t *testing.T) {
	for _, music := range []bool{false, true} {
		name := "language"
		kind := "movie"
		if music {
			name = "lossy_files"
			kind = "music"
		}
		t.Run(name, func(t *testing.T) {
			s, p, _ := fixture(t)
			h := seedTorrent(t, p, 52, nil)
			s.filter = contentfilter.New(contentfilter.Config{Enabled: true, Enforce: true, RequireEnglishLanguage: true, AllowedLanguages: []string{"en"}, DropLossyAudioOnly: true})
			var e error
			if music {
				_, e = p.Exec(testContext, `UPDATE torrents SET name='Synthetic.Album.mp3' WHERE info_hash=$1`, h)
			} else {
				_, e = p.Exec(testContext, `UPDATE torrent_contents SET languages='["fr"]' WHERE info_hash=$1`, h)
			}
			require.NoError(t, e)
			a := newService(t, "type", []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result { return result(j, kind) })
			register(t, s, a)
			_, e = s.Scan(testContext)
			require.NoError(t, e)
			run(t, s)
			var got *string
			require.NoError(t, p.QueryRow(testContext, `SELECT content_type FROM torrent_contents WHERE info_hash=$1`, h).Scan(&got))
			require.Nil(t, got)
			var count int
			require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM torrents WHERE info_hash=$1`, h).Scan(&count))
			require.Equal(t, 1, count)
		})
	}
}


func TestPluginTypedRowContinuesToUniqueLocalIdentityAndAbstainsOnAmbiguity(t *testing.T) {
	t.Run("unique_movie_and_tv_alias", func(t *testing.T) {
		s, p, _ := fixture(t)
		s.noiseV2 = true
		s.altTitleMatch = true

		// Seed a movie and a TV show (matched by alt_title alias).
		_, e := p.Exec(testContext, `INSERT INTO content(type,source,id,title,release_year,tsv,created_at,updated_at) VALUES
			('movie','tmdb','880001','Iron Lantern',2023,to_tsvector('simple','Iron Lantern 2023 secretkeyword'),now(),now()),
			('tv_show','tmdb','880002','Starbound Chronicles',2022,to_tsvector('simple','Starbound Chronicles Starbound Alt'),now(),now())`)
		require.NoError(t, e)
		_, e = p.Exec(testContext, `INSERT INTO content_attributes(content_type,content_source,content_id,source,key,value,created_at,updated_at)
			VALUES('tv_show','tmdb','880002','tmdb','alt_title:en','Starbound Alt',now(),now())`)
		require.NoError(t, e)

		hMovie := seedTorrent(t, p, 71, nil)
		_, e = p.Exec(testContext, `UPDATE torrents SET name='Iron.Lantern.2023.1080p.BluRay.x265-GRP.mkv' WHERE info_hash=$1`, hMovie)
		require.NoError(t, e)

		hTV := seedTorrent(t, p, 72, nil)
		_, e = p.Exec(testContext, `UPDATE torrents SET name='Starbound.Alt.S02E05.1080p.WEB-DL.x264-GRP.mkv' WHERE info_hash=$1`, hTV)
		require.NoError(t, e)

		a := newService(t, "typer", []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result {
			if j.Input.InfoHash == hex.EncodeToString(hMovie) {
				return result(j, "movie")
			}
			return result(j, "tv_show")
		})
		register(t, s, a)
		_, e = s.Scan(testContext)
		require.NoError(t, e)
		run(t, s)
		run(t, s)

		var mID, mRes *string
		var hasKeyword bool
		require.NoError(t, p.QueryRow(testContext, `SELECT content_id, video_resolution, COALESCE(tsv @@ plainto_tsquery('simple','secretkeyword'), false) FROM torrent_contents WHERE info_hash=$1`, hMovie).Scan(&mID, &mRes, &hasKeyword))
		require.NotNil(t, mID)
		require.Equal(t, "880001", *mID)
		require.NotNil(t, mRes)
		require.Equal(t, "V1080p", *mRes)
		require.True(t, hasKeyword, "attached local content tsv must be merged into torrent_contents.tsv")

		var tagCount int
		require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM torrent_tags WHERE info_hash=$1 AND name='type-local-enriched'`, hMovie).Scan(&tagCount))
		require.Equal(t, 1, tagCount)

		var tvID *string
		var eps string
		require.NoError(t, p.QueryRow(testContext, `SELECT content_id, COALESCE(episodes::text, '') FROM torrent_contents WHERE info_hash=$1`, hTV).Scan(&tvID, &eps))
		require.NotNil(t, tvID)
		require.Equal(t, "880002", *tvID)
		require.Contains(t, eps, `"2"`)
	})

	t.Run("ambiguous_local_titles_abstain", func(t *testing.T) {
		s, p, _ := fixture(t)
		_, e := p.Exec(testContext, `INSERT INTO content(type,source,id,title,release_year,tsv,created_at,updated_at) VALUES
			('movie','tmdb','880010','Twin Horizon',2024,to_tsvector('simple','Twin Horizon 2024'),now(),now()),
			('movie','tmdb','880011','Twin Horizon',2024,to_tsvector('simple','Twin Horizon 2024'),now(),now())`)
		require.NoError(t, e)

		hAmb := seedTorrent(t, p, 73, nil)
		_, e = p.Exec(testContext, `UPDATE torrents SET name='Twin.Horizon.2024.1080p.WEBDL.x264-GRP.mkv' WHERE info_hash=$1`, hAmb)
		require.NoError(t, e)

		a := newService(t, "typer", []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result { return result(j, "movie") })
		register(t, s, a)
		_, e = s.Scan(testContext)
		require.NoError(t, e)
		run(t, s)

		var cType, cID, res *string
		require.NoError(t, p.QueryRow(testContext, `SELECT content_type, content_id, video_resolution FROM torrent_contents WHERE info_hash=$1`, hAmb).Scan(&cType, &cID, &res))
		require.NotNil(t, cType)
		require.Equal(t, "movie", *cType)
		require.Nil(t, cID, "ambiguous local titles must strictly abstain from attaching content_id")
		require.NotNil(t, res)
		require.Equal(t, "V1080p", *res)
	})
}
