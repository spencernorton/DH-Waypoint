-- +goose NO TRANSACTION
-- +goose Up
-- Covering and partial indexes for serving-policy gates and 30-day match-rate
-- telemetry:
--  1. processor_payload_reviews_serving_idx lets active payload-evidence lookups
--     run index-only without scanning expired or rejected rows.
--  2. processor_publication_sources_hash_rev_idx covers (info_hash, revision)
--     probes without random heap lookups on processor_publication_sources.
--  3. processor_publication_reviews_serving_idx covers active policy-contract
--     approvals including processor_identity for index-only verification.
--  4. torrent_contents_type_created_content_idx turns dashstats 30-day movie/TV
--     match-rate scans from full content-type scans into index-only range scans.
CREATE INDEX CONCURRENTLY IF NOT EXISTS processor_payload_reviews_serving_idx
 ON processor_payload_reviews (verifier_artifact, policy, expires_at DESC, info_hash, source_revision)
 WHERE approved;

CREATE INDEX CONCURRENTLY IF NOT EXISTS processor_publication_sources_hash_rev_idx
 ON processor_publication_sources (info_hash, revision);

CREATE INDEX CONCURRENTLY IF NOT EXISTS processor_publication_reviews_serving_idx
 ON processor_publication_reviews (processor_id, info_hash, source_revision, execution_generation)
 INCLUDE (processor_identity)
 WHERE approved AND policy_contract = 'core-publication-policy-v1';

CREATE INDEX CONCURRENTLY IF NOT EXISTS torrent_contents_type_created_content_idx
 ON torrent_contents (content_type, created_at DESC)
 INCLUDE (content_id)
 WHERE content_type IN ('movie', 'tv_show');

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS torrent_contents_type_created_content_idx;
DROP INDEX CONCURRENTLY IF EXISTS processor_publication_reviews_serving_idx;
DROP INDEX CONCURRENTLY IF EXISTS processor_publication_sources_hash_rev_idx;
DROP INDEX CONCURRENTLY IF EXISTS processor_payload_reviews_serving_idx;
