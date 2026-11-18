-- +goose NO TRANSACTION
-- +goose Up
-- Partial queue picker index, composite identity+episode GIN index, and removal
-- of four unread secondary indexes on torrents_torrent_sources so seeder/leecher
-- scrape updates stay HOT on heap pages.
CREATE INDEX CONCURRENTLY IF NOT EXISTS queue_jobs_pick_idx
 ON queue_jobs (queue, (status = 'retry'), priority, run_after)
 WHERE status IN ('pending', 'retry');

CREATE INDEX CONCURRENTLY IF NOT EXISTS torrent_contents_episode_keys_idx
 ON torrent_contents USING gin (torrent_contents_episode_keys(content_type, content_source, content_id, episodes))
 WHERE content_id IS NOT NULL AND episodes IS NOT NULL;

DROP INDEX CONCURRENTLY IF EXISTS torrents_torrent_sources_seeders_idx;
DROP INDEX CONCURRENTLY IF EXISTS torrents_torrent_sources_leechers_idx;
DROP INDEX CONCURRENTLY IF EXISTS torrents_torrent_sources_created_at_idx;
DROP INDEX CONCURRENTLY IF EXISTS torrents_torrent_sources_updated_at_idx;

-- +goose Down
CREATE INDEX CONCURRENTLY IF NOT EXISTS torrents_torrent_sources_updated_at_idx
 ON torrents_torrent_sources (updated_at);
CREATE INDEX CONCURRENTLY IF NOT EXISTS torrents_torrent_sources_created_at_idx
 ON torrents_torrent_sources (created_at);
CREATE INDEX CONCURRENTLY IF NOT EXISTS torrents_torrent_sources_leechers_idx
 ON torrents_torrent_sources (leechers);
CREATE INDEX CONCURRENTLY IF NOT EXISTS torrents_torrent_sources_seeders_idx
 ON torrents_torrent_sources (seeders);
DROP INDEX CONCURRENTLY IF EXISTS torrent_contents_episode_keys_idx;
DROP INDEX CONCURRENTLY IF EXISTS queue_jobs_pick_idx;
