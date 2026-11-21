-- +goose Up
-- Opt-in LEAN MODE catalog projection and tombstone tables.
-- Neither table stores torrent names or file paths, and neither table triggers
-- processor_publication_sources revision bumps.
CREATE TABLE IF NOT EXISTS lean_winners (
 profile_id text NOT NULL CHECK (profile_id ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
 slot_key text NOT NULL CHECK (length(slot_key) BETWEEN 1 AND 256),
 rank smallint NOT NULL CHECK (rank >= 1 AND rank <= 10),
 info_hash bytea NOT NULL REFERENCES torrents(info_hash) ON DELETE CASCADE,
 content_type text NOT NULL,
 content_source text NOT NULL,
 content_id text NOT NULL,
 config_digest text NOT NULL CHECK (config_digest ~ '^[a-f0-9]{64}$'),
 preferred_score double precision NOT NULL,
 tiebreak_score double precision NOT NULL,
 incumbent_since timestamptz NOT NULL DEFAULT clock_timestamp(),
 evaluated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (profile_id, slot_key, rank),
 UNIQUE (profile_id, slot_key, info_hash)
);

CREATE INDEX IF NOT EXISTS lean_winners_profile_rank_hash_idx
 ON lean_winners (profile_id, rank, info_hash);

CREATE INDEX IF NOT EXISTS lean_winners_info_hash_idx
 ON lean_winners (info_hash);

CREATE INDEX IF NOT EXISTS lean_winners_content_idx
 ON lean_winners (profile_id, content_type, content_source, content_id);

CREATE TABLE IF NOT EXISTS lean_tombstones (
 info_hash bytea PRIMARY KEY CHECK (octet_length(info_hash) IN (20, 32)),
 profile_id text NOT NULL CHECK (profile_id ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
 config_digest text NOT NULL CHECK (config_digest ~ '^[a-f0-9]{64}$'),
 slot_key text NOT NULL CHECK (length(slot_key) BETWEEN 1 AND 256),
 reason_code text NOT NULL CHECK (length(reason_code) BETWEEN 1 AND 64),
 pruned_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 expires_at timestamptz
);

CREATE INDEX IF NOT EXISTS lean_tombstones_slot_idx
 ON lean_tombstones (profile_id, slot_key);

CREATE INDEX IF NOT EXISTS lean_tombstones_expiry_idx
 ON lean_tombstones (expires_at)
 WHERE expires_at IS NOT NULL;

CREATE TABLE IF NOT EXISTS lean_state (
 profile_id text PRIMARY KEY CHECK (profile_id ~ '^[a-z0-9][a-z0-9._-]{0,63}$'),
 mode text NOT NULL CHECK (mode IN ('observe', 'serve', 'prune')),
 config_digest text NOT NULL CHECK (config_digest ~ '^[a-f0-9]{64}$'),
 last_content_cursor text NOT NULL DEFAULT '',
 slots_evaluated bigint NOT NULL DEFAULT 0,
 winners_kept bigint NOT NULL DEFAULT 0,
 runner_ups_kept bigint NOT NULL DEFAULT 0,
 pruned_total bigint NOT NULL DEFAULT 0,
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- +goose Down
DROP TABLE IF EXISTS lean_state;
DROP TABLE IF EXISTS lean_tombstones;
DROP TABLE IF EXISTS lean_winners;
