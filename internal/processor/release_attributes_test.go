package processor

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/spencernorton/DH-Waypoint/internal/database/dao"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func newReleaseAttrMockProcessor(t *testing.T) (processor, sqlmock.Sqlmock) {
	t.Helper()
	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	gdb, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	return processor{dao: dao.Use(gdb)}, mock
}

func TestRestoreReleaseAttributesAllowsSupersededTorrentName(t *testing.T) {
	p, mock := newReleaseAttrMockProcessor(t)
	hash := processTestHash(41)
	oldName := "Imported.Stub.Name.2025.1080p.WEB-DL-OLD"
	newName := "Authoritative.Metadata.Name.2025.2160p.HEVC.HDR10-NEW"
	oldAttrs := model.InferReleaseAttributes(oldName, "1080p.WEB-DL")
	newAttrs := model.InferReleaseAttributes(newName, "2160p.HEVC.HDR10")
	oldRaw, err := json.Marshal(oldAttrs)
	if err != nil {
		t.Fatalf("marshal oldAttrs: %v", err)
	}

	mock.ExpectQuery(`SELECT .* FROM "torrents".*FOR UPDATE`).
		WithArgs(hash.Bytes(), 1).
		WillReturnRows(sqlmock.NewRows([]string{"info_hash", "name"}).AddRow(hash.Bytes(), newName))
	mock.ExpectQuery(`SELECT .* FROM "torrent_contents".*FOR UPDATE`).
		WithArgs(hash.Bytes()).
		WillReturnRows(sqlmock.NewRows([]string{"info_hash", "release_attributes"}).AddRow(hash.Bytes(), oldRaw))

	tc := &model.TorrentContent{
		InfoHash:          hash,
		ReleaseAttributes: newAttrs,
	}
	if err := p.restoreReleaseAttributes(context.Background(), p.dao, []*model.TorrentContent{tc}, map[protocol.InfoHash]string{hash: newName}); err != nil {
		t.Fatalf("restoreReleaseAttributes returned unexpected error for superseded name: %v", err)
	}
	if tc.ReleaseAttributes == nil || tc.ReleaseAttributes.SourceNameSHA256 != newAttrs.SourceNameSHA256 {
		t.Fatalf("expected freshly parsed release attributes for current name, got %+v", tc.ReleaseAttributes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestRestoreReleaseAttributesRejectsConcurrentRenameAndPreservesUnchanged(t *testing.T) {
	t.Run("concurrent rename rejected", func(t *testing.T) {
		p, mock := newReleaseAttrMockProcessor(t)
		hash := processTestHash(42)
		mock.ExpectQuery(`SELECT .* FROM "torrents".*FOR UPDATE`).
			WithArgs(hash.Bytes(), 1).
			WillReturnRows(sqlmock.NewRows([]string{"info_hash", "name"}).AddRow(hash.Bytes(), "Changed.During.Classify.2025.mkv"))

		tc := &model.TorrentContent{InfoHash: hash}
		err := p.restoreReleaseAttributes(context.Background(), p.dao, []*model.TorrentContent{tc}, map[protocol.InfoHash]string{hash: "Original.Name.2025.mkv"})
		if !errors.Is(err, errReleaseAttributesChanged) {
			t.Fatalf("err = %v, want errReleaseAttributesChanged", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("SQL expectations: %v", err)
		}
	})

	t.Run("unchanged name retains existing claims when incoming is nil", func(t *testing.T) {
		p, mock := newReleaseAttrMockProcessor(t)
		hash := processTestHash(43)
		name := "Amber.Signal.2025.2160p.HEVC.HDR10-GROUP.mkv"
		attrs := model.InferReleaseAttributes(name, "2160p.HEVC.HDR10")
		raw, err := json.Marshal(attrs)
		if err != nil {
			t.Fatalf("marshal attrs: %v", err)
		}

		mock.ExpectQuery(`SELECT .* FROM "torrents".*FOR UPDATE`).
			WithArgs(hash.Bytes(), 1).
			WillReturnRows(sqlmock.NewRows([]string{"info_hash", "name"}).AddRow(hash.Bytes(), name))
		mock.ExpectQuery(`SELECT .* FROM "torrent_contents".*FOR UPDATE`).
			WithArgs(hash.Bytes()).
			WillReturnRows(sqlmock.NewRows([]string{"info_hash", "release_attributes"}).AddRow(hash.Bytes(), raw))

		tc := &model.TorrentContent{InfoHash: hash, ReleaseAttributes: nil}
		if err := p.restoreReleaseAttributes(context.Background(), p.dao, []*model.TorrentContent{tc}, map[protocol.InfoHash]string{hash: name}); err != nil {
			t.Fatalf("restoreReleaseAttributes: %v", err)
		}
		if tc.ReleaseAttributes == nil || tc.ReleaseAttributes.SourceNameSHA256 != attrs.SourceNameSHA256 {
			t.Fatalf("expected retained release attributes, got %+v", tc.ReleaseAttributes)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("SQL expectations: %v", err)
		}
	})
}
