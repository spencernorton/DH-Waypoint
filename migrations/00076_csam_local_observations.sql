-- +goose Up
-- Double hashes (SHA-256 of the infohash) of torrents this instance dropped
-- because their name or file paths matched the built-in banned-keyword list.
-- No infohash, name or path is stored.
CREATE TABLE csam_local_observations (
 double_hash bytea PRIMARY KEY CHECK(octet_length(double_hash)=32),
 observed_at timestamptz NOT NULL DEFAULT now(),
 source text NOT NULL CHECK(length(source) BETWEEN 1 AND 64)
);

-- +goose Down
DROP TABLE csam_local_observations;
