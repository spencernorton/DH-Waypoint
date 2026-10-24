package dhtcrawler

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencernorton/DH-Waypoint/internal/database/dao"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/processor"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"gorm.io/gen"
	"gorm.io/gorm/clause"
)

// runPersistTorrents waits on the persistTorrents channel, and persists torrents to the database in batches.
// After persisting each batch it will publish a message to the classifier,
// and forward the hash on the scrape channel to attempt finding the seeders/leechers.
func (c *crawler) runPersistTorrents(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case is := <-c.persistTorrents.Out():
			torrentsToPersist := make([]*model.Torrent, 0, len(is))

			var torrentFilesToPersist []*model.TorrentFile

			var torrentSourcesToPersist []*model.TorrentsTorrentSource

			var torrentPiecesToPersist []*model.TorrentPieces

			var queueJobsToPersist []*model.QueueJob

			hashMap := make(map[protocol.InfoHash]infoHashWithMetaInfo, len(is))

			var hashesToClassify []protocol.InfoHash

			flushHashesToClassify := func() {
				if len(hashesToClassify) > 0 {
					job, err := processor.NewQueueJob(processor.MessageParams{
						InfoHashes: hashesToClassify,
					},
						// delay the classifier by a minute to allow time for the S/L scrape:
						model.QueueJobDelayBy(time.Minute),
					)
					if err != nil {
						c.logger.Errorf("error creating queue job: %s", err.Error())
					} else {
						queueJobsToPersist = append(queueJobsToPersist, &job)
					}
				}

				hashesToClassify = make([]protocol.InfoHash, 0, classifyBatchSize)
			}
			flushHashesToClassify()

			for _, i := range is {
				if i.validated.Identities.V1 == nil && i.validated.Identities.V2 == nil {
					continue
				}
				identity := i.validated.Identities.Canonical()
				d := c.namePolicy.Evaluate(identity, i.validated.Info.BestName())
				if !d.Eligible {
					c.namePolicy.Observe("crawler_acquisition", d)
					continue
				}
				if _, ok := hashMap[identity]; ok {
					continue
				}

				i.nodeHasPeersForHash.identity = &identity
				hashMap[identity] = i

				t := model.TorrentFromMetadata(i.validated, "dht", c.savePieces, c.saveFilesThreshold)
				for _, f := range t.Files {
					fc := f
					torrentFilesToPersist = append(torrentFilesToPersist, &fc)
				}

				t.Files = nil
				for _, s := range t.Sources {
					sc := s
					torrentSourcesToPersist = append(torrentSourcesToPersist, &sc)
				}

				t.Sources = nil
				if c.savePieces && len(t.Pieces.Pieces) > 0 {
					pc := t.Pieces
					torrentPiecesToPersist = append(torrentPiecesToPersist, &pc)
					t.Pieces = model.TorrentPieces{}
				}

				torrentsToPersist = append(torrentsToPersist, &t)

				hashesToClassify = append(hashesToClassify, identity)
				if len(hashesToClassify) >= classifyBatchSize {
					flushHashesToClassify()
				}
			}

			flushHashesToClassify()
			if len(torrentsToPersist) == 0 {
				continue
			}

			if persistErr := c.dao.Transaction(func(tx *dao.Query) error {
				if err := tx.WithContext(ctx).Torrent.Clauses(clause.OnConflict{
					Columns: []clause.Column{{Name: string(c.dao.Torrent.InfoHash.ColumnName())}},
					DoUpdates: clause.AssignmentColumns([]string{
						string(c.dao.Torrent.Name.ColumnName()),
						string(c.dao.Torrent.InfoHashV2.ColumnName()),
						string(c.dao.Torrent.FilesStatus.ColumnName()),
						string(c.dao.Torrent.FilesCount.ColumnName()),
						string(c.dao.Torrent.UpdatedAt.ColumnName()),
					}),
				}).CreateInBatches(torrentsToPersist, 100); err != nil {
					return err
				}
				if len(torrentFilesToPersist) > 0 {
					if err := tx.WithContext(ctx).TorrentFile.Clauses(clause.OnConflict{
						DoNothing: true,
					}).CreateInBatches(torrentFilesToPersist, 100); err != nil {
						return err
					}
				}
				if err := tx.WithContext(ctx).TorrentsTorrentSource.Clauses(clause.OnConflict{
					DoNothing: true,
				}).CreateInBatches(torrentSourcesToPersist, 100); err != nil {
					return err
				}
				if c.savePieces && len(torrentPiecesToPersist) > 0 {
					if err := tx.WithContext(ctx).TorrentPieces.Clauses(clause.OnConflict{
						DoNothing: true,
					}).CreateInBatches(torrentPiecesToPersist, 10); err != nil {
						return err
					}
				}
				return tx.WithContext(ctx).QueueJob.CreateInBatches(queueJobsToPersist, 10)
			}); persistErr != nil {
				c.logger.Errorf("error persisting torrents: %s", persistErr)
			} else {
				c.persistedTotal.With(prometheus.Labels{"entity": "Torrent"}).Add(float64(len(torrentsToPersist)))
				c.logger.Debugw("persisted torrents", "count", len(torrentsToPersist))

				for _, i := range hashMap {
					select {
					case <-ctx.Done():
						return
					case c.scrape.In() <- i.nodeHasPeersForHash:
						continue
					}
				}
			}
		}
	}
}

const classifyBatchSize = 100

// runPersistSources waits on the persistSources channel for scraped torrents, and persists sources
// (which includes discovery date, seeders and leechers) to the database in batches.
func (c *crawler) runPersistSources(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case scrapes := <-c.persistSources.Out():
			srcs := make([]*model.TorrentsTorrentSource, 0, len(scrapes))

			hashSet := make(map[protocol.ID]struct{}, len(scrapes))
			for _, s := range scrapes {
				if _, ok := hashSet[s.infoHash]; ok {
					continue
				}

				hashSet[s.infoHash] = struct{}{}

				if src, err := createTorrentSourceModel(s); err != nil {
					c.logger.Errorf("error creating torrent source model: %s", err.Error())
				} else {
					srcs = append(srcs, &src)
				}
			}

			if persistErr := c.dao.WithContext(ctx).TorrentsTorrentSource.Clauses(
				clause.OnConflict{
					Columns: []clause.Column{
						{Name: string(c.dao.TorrentsTorrentSource.InfoHash.ColumnName())},
						{Name: string(c.dao.TorrentsTorrentSource.Source.ColumnName())},
					},
					DoUpdates: clause.AssignmentColumns([]string{
						string(c.dao.TorrentsTorrentSource.Seeders.ColumnName()),
						string(c.dao.TorrentsTorrentSource.Leechers.ColumnName()),
						// sets to null, fixes torrents indexed before 0.8.0 with published_at
						// 0001-01-01 00:00:00+00:
						string(c.dao.TorrentsTorrentSource.PublishedAt.ColumnName()),
						string(c.dao.TorrentsTorrentSource.UpdatedAt.ColumnName()),
					}),
				},
			).Where(
				// check that the torrent record hasn't been deleted:
				gen.Exists(c.dao.WithContext(ctx).Torrent.Where(
					c.dao.Torrent.InfoHash.EqCol(c.dao.TorrentsTorrentSource.InfoHash),
				)),
			).CreateInBatches(srcs, 100); persistErr != nil {
				c.logger.Errorf("error persisting torrent sources: %s", persistErr.Error())
			} else {
				c.persistedTotal.With(prometheus.Labels{"entity": "TorrentsTorrentSource"}).Add(float64(len(srcs)))
				c.logger.Debugw("persisted torrent sources", "count", len(srcs))
			}
		}
	}
}

func createTorrentSourceModel(
	result infoHashWithScrape,
) (model.TorrentsTorrentSource, error) {
	identity := protocol.InfoHashV1(result.infoHash)
	if result.identity != nil {
		identity = *result.identity
	}
	seeders := model.NewNullUint(uint(result.bfsd.ApproximatedSize()))
	leechers := model.NewNullUint(uint(result.bfpe.ApproximatedSize()))

	return model.TorrentsTorrentSource{
		Source:   "dht",
		InfoHash: identity,
		Seeders:  seeders,
		Leechers: leechers,
	}, nil
}
