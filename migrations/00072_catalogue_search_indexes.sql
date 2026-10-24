-- +goose NO TRANSACTION
-- +goose Up
-- Match a canonical identity and its complete published order, plus selective
-- external-ID lookup. Wider browse indexes were not retained by qualification.
-- Existing source facts, identity keys and serving predicates are unchanged.
-- Build/disk/write costs must be qualified on a restored catalogue before rollout.
CREATE INDEX CONCURRENTLY torrent_contents_identity_published_page_idx
    ON torrent_contents (content_type, content_source, content_id,
                        published_at DESC, info_hash DESC, id DESC)
    WHERE content_id IS NOT NULL;
CREATE INDEX CONCURRENTLY content_attributes_identifier_lookup_idx
    ON content_attributes (source, value, content_type, content_source, content_id)
    WHERE key = 'id';

-- +goose Down
DROP INDEX CONCURRENTLY content_attributes_identifier_lookup_idx;
DROP INDEX CONCURRENTLY torrent_contents_identity_published_page_idx;
