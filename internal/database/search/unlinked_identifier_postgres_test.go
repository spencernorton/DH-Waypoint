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

func TestStrictTitleKeyAndUnlinkedMatchingUnit(t *testing.T) {
	require.Equal(t, "the lantern", strictTitleKey("The Lantern"))
	require.Equal(t, "lantern", strictTitleKey("Lantern"))
	require.NotEqual(t, strictTitleKey("The Lantern"), strictTitleKey("Lantern"))
	require.Equal(t, "wardens creek", strictTitleKey("Warden's Creek"))
	require.Equal(t, "law and order", strictTitleKey("Law & Order"))

	solitude2015 := model.Content{
		Type:        model.ContentTypeTvShow,
		Source:      "tmdb",
		ID:          "6001",
		Title:       "Solitude",
		ReleaseYear: 2015,
	}
	lantern2022 := model.Content{
		Type:        model.ContentTypeTvShow,
		Source:      "tmdb",
		ID:          "6002",
		Title:       "The Lantern",
		ReleaseYear: 2022,
	}
	wardens := model.Content{
		Type:        model.ContentTypeTvShow,
		Source:      "tmdb",
		ID:          "6003",
		Title:       "The Warden's Creek",
		ReleaseYear: 2020,
	}

	solitudeMatchers, _, solitudeTSQ := buildUnlinkedMatchersAndTSQuery([]model.Content{solitude2015})
	require.NotEmpty(t, solitudeTSQ)
	lanternMatchers, _, lanternTSQ := buildUnlinkedMatchersAndTSQuery([]model.Content{lantern2022})
	require.NotEmpty(t, lanternTSQ)
	wardensMatchers, _, wardensTSQ := buildUnlinkedMatchersAndTSQuery([]model.Content{wardens})
	require.Contains(t, wardensTSQ, "wardens")

	mkTVItem := func(name string, eps model.Episodes) TorrentContentResultItem {
		return TorrentContentResultItem{
			TorrentContent: model.TorrentContent{
				ContentType: model.NewNullContentType(model.ContentTypeTvShow),
				Episodes:    eps,
				Torrent:     model.Torrent{Name: name},
			},
		}
	}

	s11e01 := make(model.Episodes).AddEpisode(11, 1)
	s01e01 := make(model.Episodes).AddEpisode(1, 1)
	s03e01 := make(model.Episodes).AddEpisode(3, 1)

	require.True(t, matchesAnyResolvedContent(mkTVItem("Solitude.S11E01.1080p.WEB.h264-SYNTH", s11e01), solitudeMatchers))
	require.True(t, matchesAnyResolvedContent(mkTVItem("Solitude.2015.S11E01.1080p.WEB-DL", s11e01), solitudeMatchers))
	require.False(t, matchesAnyResolvedContent(mkTVItem("Home.Solitude.S11E01.1080p.WEB.h264", s11e01), solitudeMatchers))
	require.False(t, matchesAnyResolvedContent(mkTVItem("Solitude.South.S11E01.1080p.WEB.h264", s11e01), solitudeMatchers))
	require.False(t, matchesAnyResolvedContent(mkTVItem("The.Solitude.S11E01.1080p.WEB.h264", s11e01), solitudeMatchers))
	require.False(t, matchesAnyResolvedContent(mkTVItem("Solitude.2023.S01E01.1080p.WEB.h264", s01e01), solitudeMatchers))

	require.True(t, matchesAnyResolvedContent(mkTVItem("The.Lantern.S03E01.1080p.WEB.h264", s03e01), lanternMatchers))
	require.False(t, matchesAnyResolvedContent(mkTVItem("Babies.and.the.Lantern.S03E01.1080p.WEB.h264", s03e01), lanternMatchers))
	require.False(t, matchesAnyResolvedContent(mkTVItem("Lantern.S03E01.1080p.WEB.h264", s03e01), lanternMatchers))

	// Multi-word title with apostrophe matches both apostrophe-stripped and article-dropped releases.
	require.True(t, matchesAnyResolvedContent(mkTVItem("The.Wardens.Creek.S03E01.1080p.WEB-DL", s03e01), wardensMatchers))
	require.True(t, matchesAnyResolvedContent(mkTVItem("Wardens.Creek.S03E01.1080p.WEB-DL", s03e01), wardensMatchers))
}

func TestUnlinkedIdentifierFallbackPostgres(t *testing.T) {
	f := newAdultServingPostgresFixture(t)
	ctx := context.Background()

	_, err := f.pool.Exec(ctx, `
ALTER TABLE content ADD COLUMN IF NOT EXISTS original_title text;
ALTER TABLE content ADD COLUMN IF NOT EXISTS release_year integer;
ALTER TABLE torrent_contents ADD COLUMN IF NOT EXISTS episodes jsonb;
INSERT INTO metadata_sources (key, name) VALUES ('tmdb', 'TMDB'), ('imdb', 'IMDb'), ('tvdb', 'TVDB') ON CONFLICT DO NOTHING;
INSERT INTO content (type, source, id, title, release_year, adult) VALUES
  ('tv_show', 'tmdb', '6002', 'The Lantern', 2022, false),
  ('tv_show', 'tmdb', '6099', 'Flagged Series', 2021, true);
INSERT INTO content_attributes (content_type, content_source, content_id, source, key, value) VALUES
  ('tv_show', 'tmdb', '6002', 'imdb', 'id', 'tt6002000'),
  ('tv_show', 'tmdb', '6002', 'tvdb', 'id', '406002'),
  ('tv_show', 'tmdb', '6099', 'imdb', 'id', 'tt6099000');
`)
	require.NoError(t, err)

	mkHash := func(n int) protocol.InfoHash {
		raw, decErr := hex.DecodeString(fmt.Sprintf("%040x", 0x9000+n))
		require.NoError(t, decErr)
		var h protocol.InfoHash
		copy(h[:], raw)
		return h
	}

	baseTime := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	insertRow := func(idx int, name, searchStr string, cSource, cID *string, episodesJSON string, pubOffsetMin int, held bool) protocol.InfoHash {
		h := mkHash(idx)
		pub := baseTime.Add(time.Duration(pubOffsetMin) * time.Minute)
		_, execErr := f.pool.Exec(ctx, `INSERT INTO torrents (info_hash, name) VALUES ($1, $2)`, h.Bytes(), name)
		require.NoError(t, execErr)
		rowID := fmt.Sprintf("synth-%d", idx)
		_, execErr = f.pool.Exec(ctx, `INSERT INTO torrent_contents
  (id, info_hash, content_type, content_source, content_id, episodes, seeders, published_at, search_string)
  VALUES ($1, $2, 'tv_show', $3, $4, $5::jsonb, 25, $6, $7)`,
			rowID, h.Bytes(), cSource, cID, episodesJSON, pub, searchStr)
		require.NoError(t, execErr)
		if held {
			_, execErr = f.pool.Exec(ctx, `INSERT INTO processor_publication_holds (info_hash, held) VALUES ($1, true)`, h.Bytes())
			require.NoError(t, execErr)
		}
		return h
	}

	tmdbSrc := "tmdb"
	lanternID := "6002"
	// Linked rows have OLDER published_at than unlinked rows; linked must still come first.
	hLinkedEp := insertRow(1, "The.Lantern.S03E01.720p.WEB.h264-SYNTH", "the lantern s03e01 720p web h264", &tmdbSrc, &lanternID, `{"3":{"1":{}}}`, 1, false)
	hUnlinkedEp := insertRow(2, "The.Lantern.S03E01.1080p.WEB.h264-SYNTH", "the lantern s03e01 1080p web h264", nil, nil, `{"3":{"1":{}}}`, 30, false)
	hUnlinkedPack := insertRow(3, "The.Lantern.S03.COMPLETE.2160p.WEB-DL-SYNTH", "the lantern s03 complete 2160p web dl", nil, nil, `{"3":{}}`, 20, false)
	// Must NOT match: different show containing substring "the lantern".
	_ = insertRow(4, "Babies.and.the.Lantern.S03E01.1080p.WEB.h264-SYNTH", "babies and the lantern s03e01 1080p web h264", nil, nil, `{"3":{"1":{}}}`, 40, false)
	// Must NOT match: unrelated season pack whose internal episode file path contains "the lantern" in tsv.
	_ = insertRow(8, "Kingdom.of.Crowns.S03.COMPLETE.1080p.BluRay.x265-SYNTH", "kingdom of crowns s03 complete 1080p bluray s03e07 the lantern and the maiden mkv", nil, nil, `{"3":{}}`, 45, false)
	// MUST match: tracker domain prefix stripped by NoiseV2 parser.
	hSitePrefixed := insertRow(9, "www.ExampleIndex.org - The.Lantern.S03E01.PROPER.1080p.WEB-DL-SYNTH", "www exampleindex org the lantern s03e01 proper 1080p web dl", nil, nil, `{"3":{"1":{}}}`, 35, false)
	// Must NOT match: wrong episode S03E02.
	_ = insertRow(5, "The.Lantern.S03E02.1080p.WEB.h264-SYNTH", "the lantern s03e02 1080p web h264", nil, nil, `{"3":{"2":{}}}`, 50, false)
	// Must NOT match: held by publication hold (serving policy).
	_ = insertRow(6, "The.Lantern.S03E01.REMUX.1080p-SYNTH", "the lantern s03e01 remux 1080p", nil, nil, `{"3":{"1":{}}}`, 60, true)
	// Must NOT match when ExcludeAdult=true: adult-flagged series.
	_ = insertRow(7, "Flagged.Series.S01E01.1080p.WEB-DL", "flagged series s01e01 1080p web dl", nil, nil, `{"1":{"1":{}}}`, 70, false)

	s := f.serving(t, true)
	epFilter := make(model.Episodes).AddEpisode(3, 1)
	baseOpts := []query.Option{
		TorrentContentDefaultOption(),
		query.WithTotalCount(false),
		query.Where(TorrentContentTypeCriteria(model.ContentTypeTvShow)),
		query.Where(TorrentContentEpisodesCriteria(epFilter)),
	}

	wantOrder := []protocol.InfoHash{hLinkedEp, hSitePrefixed, hUnlinkedEp, hUnlinkedPack}

	for _, ref := range []model.ContentRef{
		{Type: model.ContentTypeTvShow, Source: "tmdb", ID: "6002"},
		{Type: model.ContentTypeTvShow, Source: "imdb", ID: "tt6002000"},
		{Type: model.ContentTypeTvShow, Source: "tvdb", ID: "406002"},
	} {
		res, qErr := s.TorrentContentByIdentifiersWithUnlinked(ctx, []model.ContentRef{ref}, 10, 0, baseOpts...)
		require.NoError(t, qErr, ref)
		require.Equal(t, wantOrder, adultServingContentHashes(res.Items), "linked first then newest unlinked matches for %v", ref)
	}

	// Pagination across linked -> unlinked boundary (limit=1 and limit=2).
	var pagedByOne []protocol.InfoHash
	for off := uint(0); off < 5; off++ {
		page, pErr := s.TorrentContentByIdentifiersWithUnlinked(
			ctx,
			[]model.ContentRef{{Type: model.ContentTypeTvShow, Source: "imdb", ID: "tt6002000"}},
			1,
			off,
			baseOpts...,
		)
		require.NoError(t, pErr)
		if int(off) < len(wantOrder) {
			require.Len(t, page.Items, 1)
			pagedByOne = append(pagedByOne, page.Items[0].InfoHash)
		} else {
			require.Empty(t, page.Items)
		}
	}
	require.Equal(t, wantOrder, pagedByOne)

	// Unresolved identifier keeps today's empty behavior.
	unresolved, uErr := s.TorrentContentByIdentifiersWithUnlinked(
		ctx,
		[]model.ContentRef{{Type: model.ContentTypeTvShow, Source: "tmdb", ID: "999999"}},
		10,
		0,
		baseOpts...,
	)
	require.NoError(t, uErr)
	require.Empty(t, unresolved.Items)

	// Adult-flagged content does not rescue unlinked rows when ExcludeAdult=true.
	s01e01 := make(model.Episodes).AddEpisode(1, 1)
	adultOpts := []query.Option{
		TorrentContentDefaultOption(),
		query.WithTotalCount(false),
		query.Where(TorrentContentTypeCriteria(model.ContentTypeTvShow)),
		query.Where(TorrentContentEpisodesCriteria(s01e01)),
	}
	adultRes, aErr := s.TorrentContentByIdentifiersWithUnlinked(
		ctx,
		[]model.ContentRef{{Type: model.ContentTypeTvShow, Source: "imdb", ID: "tt6099000"}},
		10,
		0,
		adultOpts...,
	)
	require.NoError(t, aErr)
	require.Empty(t, adultRes.Items)
}
