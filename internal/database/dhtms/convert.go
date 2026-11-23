package dhtms

import (
	"database/sql"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
)

// DocHitToTorrentContent converts a dh-tmsd hit into a model.TorrentContent
// compatible with legacy v1 Torznab and REST v1 consumers (skipping pure-v2
// hashes to preserve LegacyTorrentIdentityCriteria semantics).
func DocHitToTorrentContent(hit DocHit, contentSource, contentID string) (model.TorrentContent, bool) {
	if len(hit.InfoHashV1) != 40 {
		return model.TorrentContent{}, false
	}
	ih, err := protocol.ParseInfoHash(hit.InfoHashV1)
	if err != nil {
		return model.TorrentContent{}, false
	}
	var ihv2 *protocol.InfoHash
	if len(hit.InfoHashV2) == 64 {
		if parsed, perr := protocol.ParseInfoHash(hit.InfoHashV2); perr == nil {
			ihv2 = &parsed
		}
	}

	publishedAt := time.Unix(int64(hit.PublishedEpoch), 0).UTC()
	if hit.PublishedEpoch == 0 {
		publishedAt = time.Unix(int64(hit.CreatedEpoch), 0).UTC()
	}
	createdAt := time.Unix(int64(hit.CreatedEpoch), 0).UTC()
	if hit.CreatedEpoch == 0 {
		createdAt = publishedAt
	}
	updatedAt := publishedAt
	if hit.CheckedEpochMin > 0 {
		checked := time.Unix(int64(hit.CheckedEpochMin)*60, 0).UTC()
		if checked.After(updatedAt) {
			updatedAt = checked
		}
	}

	var seeders, leechers model.NullUint
	if hit.Seeders != nil {
		seeders = model.NewNullUint(uint(*hit.Seeders))
	}
	if hit.Leechers != nil {
		leechers = model.NewNullUint(uint(*hit.Leechers))
	}
	var filesCount model.NullUint
	if hit.FilesCount > 0 {
		filesCount = model.NewNullUint(uint(hit.FilesCount))
	}

	sources := []model.TorrentsTorrentSource{
		{
			Source:      model.SourceKeyTracker,
			InfoHash:    ih,
			Seeders:     seeders,
			Leechers:    leechers,
			PublishedAt: sql.NullTime{Time: publishedAt, Valid: true},
			CreatedAt:   createdAt,
			UpdatedAt:   updatedAt,
		},
	}

	ct := CodeToContentType(hit.ContentType)
	res := CodeToResolution(hit.Resolution)

	tc := model.TorrentContent{
		InfoHash:        ih,
		ContentType:     ct,
		VideoResolution: res,
		PublishedAt:     publishedAt,
		CreatedAt:       createdAt,
		UpdatedAt:       updatedAt,
		Size:            uint(hit.SizeBytes),
		FilesCount:      filesCount,
		Seeders:         seeders,
		Leechers:        leechers,
		Torrent: model.Torrent{
			InfoHash:   ih,
			InfoHashV2: ihv2,
			Name:       hit.Name,
			Size:       uint(hit.SizeBytes),
			Private:    false,
			CreatedAt:  createdAt,
			UpdatedAt:  updatedAt,
			FilesCount: filesCount,
			Sources:    sources,
		},
	}
	if contentSource != "" && contentID != "" {
		tc.ContentSource = model.NewNullString(contentSource)
		tc.ContentID = model.NewNullString(contentID)
	}
	return tc, true
}

// DocHitToTorrent converts a dh-tmsd hit (v1, hybrid, or pure-v2) into a
// model.Torrent for raw catalogue v2 endpoints.
func DocHitToTorrent(hit DocHit) (model.Torrent, bool) {
	var ih protocol.InfoHash
	var ihv2 *protocol.InfoHash
	switch {
	case len(hit.InfoHashV1) == 40:
		parsed, err := protocol.ParseInfoHash(hit.InfoHashV1)
		if err != nil {
			return model.Torrent{}, false
		}
		ih = parsed
		if len(hit.InfoHashV2) == 64 {
			if p2, err2 := protocol.ParseInfoHash(hit.InfoHashV2); err2 == nil {
				ihv2 = &p2
			}
		}
	case len(hit.InfoHashV2) == 64:
		parsed, err := protocol.ParseInfoHash(hit.InfoHashV2)
		if err != nil {
			return model.Torrent{}, false
		}
		ih = parsed
		cp := parsed
		ihv2 = &cp
	default:
		return model.Torrent{}, false
	}

	createdAt := time.Unix(int64(hit.CreatedEpoch), 0).UTC()
	filesStatus := model.FilesStatusSingle
	var filesCount model.NullUint
	if hit.FilesCount > 1 {
		filesStatus = model.FilesStatusMulti
		filesCount = model.NewNullUint(uint(hit.FilesCount))
	} else if hit.FilesCount == 1 {
		filesCount = model.NewNullUint(1)
	}

	return model.Torrent{
		InfoHash:    ih,
		InfoHashV2:  ihv2,
		Name:        hit.Name,
		Size:        uint(hit.SizeBytes),
		Private:     false,
		CreatedAt:   createdAt,
		UpdatedAt:   createdAt,
		FilesStatus: filesStatus,
		FilesCount:  filesCount,
	}, true
}
