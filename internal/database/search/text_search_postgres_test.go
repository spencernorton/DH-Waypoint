package search

import (
	"context"
	"testing"

	"github.com/spencernorton/DH-Waypoint/internal/database/fts"
	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/stretchr/testify/require"
)

func TestTextSearchPostgresBareSeasonMatchesGluedEpisode(t *testing.T) {
	f := newAdultServingPostgresFixture(t)
	ctx := context.Background()
	for i, name := range map[int]string{
		3: "Shrek S51E01 1080p",
		4: "Shrek S51 Complete",
		5: "Shrek S510E01",
		6: "Shrek S05E01",
	} {
		_, err := f.pool.Exec(ctx, `UPDATE torrent_contents SET search_string=$2 WHERE info_hash=$1`, f.hashes[i].Bytes(), name)
		require.NoError(t, err)
	}
	s := f.serving(t, false)
	for _, q := range []string{"Shrek s51", `"Shrek s51"`, "shrek S51"} {
		result, err := s.TorrentContent(ctx, append(adultServingExactOptions(model.TableNameTorrentContent), query.SearchString(q))...)
		require.NoError(t, err, q)
		require.Equal(t, f.hashes[3:5], adultServingContentHashes(result.Items), q)
	}

	// Production rows carry a Go-built tsvector rather than the fixture's generated one.
	tsv := fts.Tsvector{}
	tsv.AddText("Shrek.S51E01.1080p.mkv", fts.TsvectorWeightA)
	for q, want := range map[string]bool{"Shrek s51": true, "Shrek s51*": true, "Shrek s5": false, "Shrek s510": false} {
		var got bool
		require.NoError(t, f.pool.QueryRow(ctx, `SELECT $1::tsvector @@ $2::tsquery`, tsv.String(), fts.AppQueryToTsquery(q)).Scan(&got))
		require.Equal(t, want, got, q)
	}
}
