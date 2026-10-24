package processor

import (
	"context"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/spencernorton/DH-Waypoint/internal/classifier/classification"
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
