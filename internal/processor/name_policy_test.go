package processor

import (
	"context"
	"encoding/json"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/spencernorton/DH-Waypoint/internal/classifier/classification"
	llmwork "github.com/spencernorton/DH-Waypoint/internal/legacyresults"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/namepolicy"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestStoredNameDenialDoesNotHydrateClassifyBlockOrPersist(t *testing.T) {
	h := protocol.InfoHash{1}
	p, mock, _ := newProcessTestProcessor(t, nil, processRunnerStub{run: func(model.Torrent) (classification.Result, error) {
		t.Fatal("classifier called after denial")
		return classification.Result{}, nil
	}})
	p.namePolicy, _ = namepolicy.New(namepolicy.Config{Enabled: true})
	mock.ExpectQuery(`SELECT .*name.* FROM "torrents"`).WillReturnRows(sqlmock.NewRows([]string{"info_hash", "name"}).AddRow(h.Bytes(), "Synthetic.电影.ENG.mkv"))
	require.NoError(t, p.Process(context.Background(), MessageParams{InfoHashes: []protocol.InfoHash{h}}))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPersistReusesSingleBatchedForUpdateAcrossGuardAndRestore(t *testing.T) {
	h1 := processTestHash(12)
	h2 := processTestHash(24)
	n1 := "First.Film.2025.1080p.WEB-DL-GRP"
	n2 := "Second.Film.2025.2160p.HEVC.HDR10-GRP"
	attrs2 := model.InferReleaseAttributes(n2, "2160p.HEVC.HDR10")
	raw2, err := json.Marshal(attrs2)
	require.NoError(t, err)

	p, mock, _ := newProcessTestProcessor(t, nil, processRunnerStub{})
	p.namePolicy, err = namepolicy.New(namepolicy.Config{Enabled: true})
	require.NoError(t, err)

	mock.ExpectBegin()
	// Exactly 2 batched FOR UPDATE queries cover BOTH guardNameApplications and restoreReleaseAttributes.
	mock.ExpectQuery(`SELECT .* FROM "torrents".*WHERE "torrents"\."info_hash" IN \(\$1,\$2\).*ORDER BY "torrents"\."info_hash".*FOR UPDATE`).
		WithArgs(h1.Bytes(), h2.Bytes()).
		WillReturnRows(sqlmock.NewRows([]string{"info_hash", "name"}).
			AddRow(h1.Bytes(), n1).
			AddRow(h2.Bytes(), n2))
	mock.ExpectQuery(`SELECT .* FROM "torrent_contents".*WHERE "torrent_contents"\."info_hash" IN \(\$1,\$2\).*ORDER BY "torrent_contents"\."info_hash","torrent_contents"\."id".*FOR UPDATE`).
		WithArgs(h1.Bytes(), h2.Bytes()).
		WillReturnRows(sqlmock.NewRows([]string{"info_hash", "content_type", "release_attributes"}).
			AddRow(h2.Bytes(), "movie", raw2))
	mock.ExpectQuery(`SELECT .*english_audio_source.* FROM "torrent_contents"`).
		WillReturnRows(sqlmock.NewRows([]string{"info_hash", "english_audio", "english_audio_source"}))
	mock.ExpectQuery(`INSERT INTO "torrent_contents"`).
		WillReturnRows(sqlmock.NewRows([]string{"published_at"}).AddRow(nil).AddRow(nil))
	mock.ExpectCommit()

	err = p.persist(context.Background(), persistPayload{
		torrentContents: []model.TorrentContent{
			{InfoHash: h2, Torrent: model.Torrent{InfoHash: h2, Name: n2}, ContentType: model.NewNullContentType(model.ContentTypeMovie)},
			{InfoHash: h1, Torrent: model.Torrent{InfoHash: h1, Name: n1}, ContentType: model.NewNullContentType(model.ContentTypeMovie)},
		},
	})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGuardNameRowsBatchedRejectsAdultRow(t *testing.T) {
	h1 := processTestHash(31)
	h2 := processTestHash(32)
	d, mock := newProcessTestDAO(t)
	pol, err := namepolicy.New(namepolicy.Config{Enabled: true})
	require.NoError(t, err)

	mock.ExpectQuery(`SELECT .* FROM "torrents".*WHERE "torrents"\."info_hash" IN \(\$1,\$2\).*ORDER BY "torrents"\."info_hash".*FOR UPDATE`).
		WithArgs(h1.Bytes(), h2.Bytes()).
		WillReturnRows(sqlmock.NewRows([]string{"info_hash", "name"}).
			AddRow(h1.Bytes(), "Clean.Movie.2025.mkv").
			AddRow(h2.Bytes(), "Another.Movie.2025.mkv"))
	mock.ExpectQuery(`SELECT .* FROM "torrent_contents".*WHERE "torrent_contents"\."info_hash" IN \(\$1,\$2\).*ORDER BY "torrent_contents"\."info_hash","torrent_contents"\."id".*FOR UPDATE`).
		WithArgs(h1.Bytes(), h2.Bytes()).
		WillReturnRows(sqlmock.NewRows([]string{"info_hash", "content_type", "release_attributes"}).
			AddRow(h2.Bytes(), "xxx", nil))

	err = GuardNameRows(context.Background(), d, pol, []*model.TorrentContent{
		{InfoHash: h2, Torrent: model.Torrent{InfoHash: h2, Name: "Another.Movie.2025.mkv"}},
		{InfoHash: h1, Torrent: model.Torrent{InfoHash: h1, Name: "Clean.Movie.2025.mkv"}},
	})
	require.ErrorIs(t, err, llmwork.ErrHeld)
	require.NoError(t, mock.ExpectationsWereMet())
}
