-- +goose NO TRANSACTION
-- +goose Up
-- Match the complete published order and the selective external-ID semijoin.
-- Existing source facts, identity keys and serving predicates are unchanged.
-- Build/disk/write costs must be qualified on a restored catalogue before rollout.
CREATE INDEX CONCURRENTLY torrent_contents_published_page_idx
    ON torrent_contents (published_at DESC, info_hash DESC, id DESC);
CREATE INDEX CONCURRENTLY torrent_contents_type_published_page_idx
    ON torrent_contents (content_type, published_at DESC, info_hash DESC, id DESC);
CREATE INDEX CONCURRENTLY content_attributes_identifier_lookup_idx
    ON content_attributes (source, value, content_type, content_source, content_id)
    WHERE key = 'id';

-- +goose Down
DROP INDEX CONCURRENTLY content_attributes_identifier_lookup_idx;
DROP INDEX CONCURRENTLY torrent_contents_type_published_page_idx;
DROP INDEX CONCURRENTLY torrent_contents_published_page_idx;
