package search

import (
	"context"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/stretchr/testify/require"
)

func TestEpisodeKeysGINAutoActivationAndTorznabLeanHydratePostgres(t *testing.T) {
	f := newAdultServingPostgresFixture(t)
	ctx := context.Background()

	_, err := f.pool.Exec(ctx, `
ALTER TABLE content ADD COLUMN IF NOT EXISTS original_title text;
ALTER TABLE content ADD COLUMN IF NOT EXISTS release_year integer;
ALTER TABLE torrent_contents ADD COLUMN IF NOT EXISTS episodes jsonb;
CREATE OR REPLACE FUNCTION torrent_contents_episode_keys(ct text, cs text, cid text, ep jsonb)
RETURNS text[] LANGUAGE plpgsql IMMUTABLE STRICT PARALLEL SAFE AS $$
DECLARE
 keys text[] := ARRAY[]::text[];
 prefix text;
 s_key text;
 s_val jsonb;
 e_key text;
BEGIN
 IF jsonb_typeof(ep) <> 'object' THEN RETURN keys; END IF;
 prefix := ct || ':' || cs || ':' || cid;
 FOR s_key, s_val IN SELECT key, value FROM jsonb_each(ep) LOOP
  keys := array_append(keys, prefix || ':s:' || s_key);
  IF s_val = '{}'::jsonb THEN
   keys := array_append(keys, prefix || ':s_all:' || s_key);
  ELSIF jsonb_typeof(s_val) = 'object' THEN
   FOR e_key IN SELECT jsonb_object_keys(s_val) LOOP
    keys := array_append(keys, prefix || ':e:' || s_key || ':' || e_key);
   END LOOP;
  END IF;
 END LOOP;
 RETURN keys;
END $$;
CREATE INDEX torrent_contents_episode_keys_idx
 ON torrent_contents USING gin (torrent_contents_episode_keys(content_type, content_source, content_id, episodes))
 WHERE content_id IS NOT NULL AND episodes IS NOT NULL;
INSERT INTO metadata_sources (key, name) VALUES ('tmdb', 'TMDB'), ('imdb', 'IMDb') ON CONFLICT DO NOTHING;
INSERT INTO torrent_sources (key, name) VALUES ('dht', 'DHT') ON CONFLICT DO NOTHING;
INSERT INTO content (type, source, id, title, release_year, adult) VALUES
  ('tv_show', 'tmdb', '7100', 'Synthetic Odyssey', 2024, false);
INSERT INTO content_attributes (content_type, content_source, content_id, source, key, value) VALUES
  ('tv_show', 'tmdb', '7100', 'imdb', 'id', 'tt7100001');
`)
	require.NoError(t, err)
	ResetDBCapabilitiesCache()

	mkHash := func(n int) protocol.InfoHash {
		raw, decErr := hex.DecodeString(fmt.Sprintf("%040x", 0xa100+n))
		require.NoError(t, decErr)
		var h protocol.InfoHash
		copy(h[:], raw)
		return h
	}
	baseTime := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	insertLinked := func(n int, name, epJSON string) protocol.InfoHash {
		h := mkHash(n)
		pub := baseTime.Add(time.Duration(n) * time.Minute)
		_, e := f.pool.Exec(ctx, `INSERT INTO torrents (info_hash, name, size) VALUES ($1, $2, 1073741824)`, h.Bytes(), name)
		require.NoError(t, e)
		_, e = f.pool.Exec(ctx, `INSERT INTO torrents_torrent_sources (source, info_hash, seeders, leechers) VALUES ('dht', $1, 15, 2)`, h.Bytes())
		require.NoError(t, e)
		_, e = f.pool.Exec(ctx, `INSERT INTO torrent_tags (info_hash, name) VALUES ($1, 'synthetic-tag')`, h.Bytes())
		require.NoError(t, e)
		_, e = f.pool.Exec(ctx, `INSERT INTO torrent_contents (id, info_hash, content_type, content_source, content_id, episodes, seeders, published_at, search_string)
VALUES ($1, $2, 'tv_show', 'tmdb', '7100', $3::jsonb, 15, $4, $5)`,
			fmt.Sprintf("epkey-%d", n), h.Bytes(), epJSON, pub, name)
		require.NoError(t, e)
		return h
	}

	hEp := insertLinked(1, "Synthetic.Odyssey.S02E05.1080p.WEB-DL", `{"2":{"5":{}}}`)
	hPack := insertLinked(2, "Synthetic.Odyssey.S02.COMPLETE.1080p.WEB-DL", `{"2":{}}`)
	_ = insertLinked(3, "Synthetic.Odyssey.S02E06.1080p.WEB-DL", `{"2":{"6":{}}}`)

	s := f.serving(t, true)
	eps := make(model.Episodes).AddEpisode(2, 5)
	hintCtx := WithEpisodesHint(ctx, eps)

	res, err := s.TorrentContentByIdentifiersWithUnlinked(
		hintCtx,
		[]model.ContentRef{{Type: model.ContentTypeTvShow, Source: "imdb", ID: "tt7100001"}},
		20, 0,
		TorrentContentTorznabOption(),
		query.WithTotalCount(false),
		query.Where(TorrentContentTypeCriteria(model.ContentTypeTvShow)),
		query.Where(TorrentContentEpisodesCriteria(eps)),
	)
	require.NoError(t, err)
	require.Len(t, res.Items, 2)
	gotHashes := []protocol.InfoHash{res.Items[0].InfoHash, res.Items[1].InfoHash}
	require.ElementsMatch(t, []protocol.InfoHash{hEp, hPack}, gotHashes)

	// Verify Torznab lean hydrate loaded Sources + Attributes while skipping Tags.
	require.NotEmpty(t, res.Items[0].Torrent.Sources)
	imdbID, ok := res.Items[0].Content.Identifier("imdb")
	require.True(t, ok)
	require.Equal(t, "tt7100001", imdbID)
	require.Empty(t, res.Items[0].Torrent.Tags)
}
