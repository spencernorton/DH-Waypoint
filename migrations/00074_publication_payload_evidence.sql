-- +goose Up
CREATE TABLE processor_publication_holds (
 info_hash bytea PRIMARY KEY CHECK(octet_length(info_hash) IN(20,32)),
 held boolean NOT NULL, reason text NOT NULL CHECK(length(reason) BETWEEN 1 AND 128),
 request_id text NOT NULL, updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE processor_publication_hold_events (
 request_id text PRIMARY KEY CHECK(request_id ~ '^[a-f0-9]{64}$'),
 info_hash bytea NOT NULL, expected_source_revision bigint NOT NULL, source_revision bigint NOT NULL,
 held boolean NOT NULL, reason text NOT NULL, recorded_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TRIGGER processor_publication_holds AFTER INSERT OR UPDATE OR DELETE ON processor_publication_holds
 FOR EACH ROW EXECUTE FUNCTION processor_publication_source_changed();
CREATE TABLE processor_payload_reviews (
 info_hash bytea PRIMARY KEY, source_revision bigint NOT NULL CHECK(source_revision>0),
 verifier_artifact text NOT NULL CHECK(verifier_artifact ~ '^[a-f0-9]{64}$'),
 policy text NOT NULL, approved boolean NOT NULL, reason text NOT NULL,
 receipt jsonb NOT NULL, evaluated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 expires_at timestamptz NOT NULL
);
CREATE INDEX processor_payload_reviews_expiry ON processor_payload_reviews(expires_at,info_hash);
ALTER TABLE processor_control ADD COLUMN publication_policy text NOT NULL DEFAULT '';
ALTER TABLE processor_control ADD COLUMN payload_leases bigint NOT NULL DEFAULT 0;
CREATE TABLE processor_payload_pending (
 info_hash bytea PRIMARY KEY, source_revision bigint NOT NULL,
 priority smallint NOT NULL DEFAULT 0 CHECK(priority IN(0,1)),
 enqueued_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX processor_payload_pending_order ON processor_payload_pending(priority DESC,enqueued_at,info_hash);
CREATE TABLE processor_payload_work (
 info_hash bytea PRIMARY KEY, source_revision bigint NOT NULL,
 verifier_artifact text NOT NULL, lease_id text NOT NULL,
 lease_until timestamptz NOT NULL, available_at timestamptz NOT NULL,
 attempts bigint NOT NULL DEFAULT 1, outcome text NOT NULL DEFAULT 'leased'
);
CREATE INDEX processor_publication_reviews_order ON processor_publication_reviews(processor_id,evaluated_at,info_hash) WHERE approved;

-- +goose StatementBegin
CREATE FUNCTION processor_payload_deleted() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 DELETE FROM processor_payload_reviews WHERE info_hash=OLD.info_hash;
 DELETE FROM processor_payload_work WHERE info_hash=OLD.info_hash;
 DELETE FROM processor_payload_pending WHERE info_hash=OLD.info_hash;
 RETURN OLD;
END $$;
-- +goose StatementEnd
CREATE TRIGGER processor_payload_deleted AFTER DELETE ON torrents
 FOR EACH ROW EXECUTE FUNCTION processor_payload_deleted();

-- +goose StatementBegin
CREATE FUNCTION processor_payload_invalidate_pending() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 DELETE FROM processor_payload_pending WHERE info_hash=OLD.info_hash;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER processor_payload_invalidate_pending AFTER UPDATE ON processor_publication_sources
 FOR EACH ROW EXECUTE FUNCTION processor_payload_invalidate_pending();

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM processor_publication_hold_events) OR EXISTS(SELECT 1 FROM processor_payload_reviews) THEN
  RAISE EXCEPTION 'publication safety evidence prevents destructive downgrade';
 END IF;
END $$;
-- +goose StatementEnd
DROP INDEX processor_publication_reviews_order;
DROP TRIGGER processor_payload_deleted ON torrents;
DROP FUNCTION processor_payload_deleted();
DROP TRIGGER processor_payload_invalidate_pending ON processor_publication_sources;
DROP FUNCTION processor_payload_invalidate_pending();
DROP TABLE processor_payload_pending;
ALTER TABLE processor_control DROP COLUMN publication_policy;
ALTER TABLE processor_control DROP COLUMN payload_leases;
DROP TABLE processor_payload_work,processor_payload_reviews;
DROP TRIGGER processor_publication_holds ON processor_publication_holds;
DROP TABLE processor_publication_hold_events,processor_publication_holds;
