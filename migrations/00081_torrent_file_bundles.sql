-- +goose Up
-- Compact per-torrent file bundles: one row per torrent holding parallel
-- arrays of index/path/size/extension, replacing torrent_files rows behind
-- the torrent_files_effective view. torrent_files stays intact until legacy
-- rows are pruned by the backfill command.
CREATE TABLE IF NOT EXISTS torrent_file_bundles (
 info_hash bytea PRIMARY KEY REFERENCES torrents(info_hash) ON DELETE CASCADE,
 indexes integer[] NOT NULL,
 paths text[] NOT NULL,
 sizes bigint[] NOT NULL,
 extensions text[] NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK (cardinality(indexes) = cardinality(paths)
  AND cardinality(paths) = cardinality(sizes)
  AND cardinality(sizes) = cardinality(extensions))
);

ALTER TABLE torrent_file_bundles ALTER COLUMN paths SET STORAGE EXTENDED;
ALTER TABLE torrent_file_bundles ALTER COLUMN extensions SET STORAGE EXTENDED;

-- +goose StatementBegin
CREATE FUNCTION torrent_files_for_hash(target_hash bytea)
RETURNS TABLE(info_hash bytea, "index" integer, path text, size bigint, extension text, created_at timestamptz, updated_at timestamptz)
LANGUAGE sql
STABLE
AS $$
 SELECT b.info_hash, u.file_index, u.file_path, u.file_size, u.file_extension, b.created_at, b.updated_at
 FROM torrent_file_bundles b
 CROSS JOIN LATERAL unnest(b.indexes, b.paths, b.sizes, b.extensions) AS u(file_index, file_path, file_size, file_extension)
 WHERE b.info_hash = target_hash
 UNION ALL
 SELECT f.info_hash, f."index", f.path, f.size, f.extension, f.created_at, f.updated_at
 FROM torrent_files f
 WHERE f.info_hash = target_hash
  AND NOT EXISTS (SELECT 1 FROM torrent_file_bundles b2 WHERE b2.info_hash = target_hash)
$$;
-- +goose StatementEnd

CREATE VIEW torrent_files_effective AS
SELECT b.info_hash AS info_hash, u.file_index AS "index", u.file_path AS path, u.file_size AS size, u.file_extension AS extension, b.created_at AS created_at, b.updated_at AS updated_at
FROM torrent_file_bundles b
CROSS JOIN LATERAL unnest(b.indexes, b.paths, b.sizes, b.extensions) AS u(file_index, file_path, file_size, file_extension)
UNION ALL
SELECT f.info_hash, f."index", f.path, f.size, f.extension, f.created_at, f.updated_at
FROM torrent_files f
WHERE NOT EXISTS (SELECT 1 FROM torrent_file_bundles b2 WHERE b2.info_hash = f.info_hash);

CREATE TRIGGER processor_publication_file_bundles
 AFTER INSERT OR UPDATE OR DELETE ON torrent_file_bundles
 FOR EACH ROW EXECUTE FUNCTION processor_publication_source_changed();

-- +goose Down
DROP TRIGGER IF EXISTS processor_publication_file_bundles ON torrent_file_bundles;
DROP VIEW IF EXISTS torrent_files_effective;
DROP FUNCTION IF EXISTS torrent_files_for_hash(bytea);
DROP TABLE IF EXISTS torrent_file_bundles;
