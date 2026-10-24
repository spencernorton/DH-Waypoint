-- +goose NO TRANSACTION
-- +goose Up
-- The v2 raw catalogue remains searchable when no classifier/plugins run.
-- Concurrent build avoids taking the catalogue's ingestion write lock.
-- Qualify build time, I/O and disk headroom on a restored catalogue first.
CREATE INDEX CONCURRENTLY torrents_name_trgm_v2_idx ON torrents USING gin(name gin_trgm_ops);

-- +goose Down
DROP INDEX CONCURRENTLY torrents_name_trgm_v2_idx;
