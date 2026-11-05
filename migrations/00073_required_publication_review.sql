-- +goose Up
-- Approvals are explicit and revision-bound. Existing applications are never
-- grandfathered into publication authority by this migration.
CREATE TABLE processor_publication_sources (
 info_hash bytea PRIMARY KEY, revision bigint NOT NULL DEFAULT 1
);
CREATE TABLE processor_publication_reviews (
 info_hash bytea NOT NULL,
 processor_id text NOT NULL,
 source_revision bigint NOT NULL,
 execution_generation bigint NOT NULL,
 processor_identity jsonb NOT NULL,
 approved boolean NOT NULL,
 policy_contract text NOT NULL DEFAULT 'core-publication-policy-v1',
 reason text NOT NULL,
 job_id text NOT NULL,
 evaluated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(info_hash,processor_id)
);
CREATE INDEX processor_publication_reviews_approved ON processor_publication_reviews(processor_id,execution_generation,info_hash) WHERE approved;
CREATE TABLE processor_publication_pending (
 info_hash bytea PRIMARY KEY,
 source_revision bigint NOT NULL,
 enqueued_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX processor_publication_pending_order ON processor_publication_pending(enqueued_at,info_hash);
ALTER TABLE processor_jobs ADD COLUMN publication_revision bigint NOT NULL DEFAULT 0;
-- Revisit existing source after the new review contract; this is a bounded
-- catch-up cursor reset, not an unbounded data backfill or approval grant.
UPDATE processor_control SET scan_after=''::bytea WHERE singleton;

-- +goose StatementBegin
CREATE FUNCTION processor_enqueue_publication(h bytea) RETURNS void LANGUAGE plpgsql AS $$
DECLARE rev bigint; target bytea;
BEGIN
 FOR target IN SELECT info_hash FROM torrents WHERE info_hash=h OR info_hash_v2=h LOOP
 INSERT INTO processor_publication_sources(info_hash,revision)
 VALUES(target,1)
 ON CONFLICT(info_hash) DO UPDATE SET revision=processor_publication_sources.revision+1
 RETURNING revision INTO rev;
 INSERT INTO processor_publication_pending(info_hash,source_revision) VALUES(target,rev)
 ON CONFLICT(info_hash) DO UPDATE SET source_revision=EXCLUDED.source_revision;
 END LOOP;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION processor_publication_torrent_pending() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN
  DELETE FROM processor_publication_reviews WHERE info_hash=OLD.info_hash;
  DELETE FROM processor_publication_pending WHERE info_hash=OLD.info_hash;
  -- Keep the small monotonic identity fence across delete/recrawl. Reusing a
  -- previous revision could collide with retained jobs or replay old proofs.
  UPDATE processor_publication_sources SET revision=revision+1 WHERE info_hash=OLD.info_hash;
  RETURN OLD;
 END IF;
 IF TG_OP='INSERT' OR ROW(NEW.name,NEW.size,NEW.private,NEW.files_status,NEW.files_count,NEW.info_hash_v2)
 IS DISTINCT FROM ROW(OLD.name,OLD.size,OLD.private,OLD.files_status,OLD.files_count,OLD.info_hash_v2) THEN
  PERFORM processor_enqueue_publication(NEW.info_hash);
 END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER processor_publication_torrent_pending AFTER INSERT OR UPDATE OR DELETE ON torrents
 FOR EACH ROW EXECUTE FUNCTION processor_publication_torrent_pending();

-- +goose StatementBegin
CREATE FUNCTION processor_publication_source_changed() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE h bytea; previous_hash bytea; before_data jsonb; after_data jsonb;
BEGIN
 IF TG_OP='UPDATE' THEN
  IF TG_TABLE_NAME='label_evidence' THEN
   IF ROW(OLD.info_hash,OLD.source,OLD.category) IS NOT DISTINCT FROM ROW(NEW.info_hash,NEW.source,NEW.category) THEN RETURN NEW; END IF;
  END IF;
  IF TG_TABLE_NAME='torrent_verdict_state' THEN
   IF ROW(OLD.info_hash,OLD.verdict) IS NOT DISTINCT FROM ROW(NEW.info_hash,NEW.verdict) THEN RETURN NEW; END IF;
  END IF;
  before_data=to_jsonb(OLD)-'created_at'-'updated_at'-'seeders'-'leechers'-'published_at'-'tsv'-'resolved_at';
  after_data=to_jsonb(NEW)-'created_at'-'updated_at'-'seeders'-'leechers'-'published_at'-'tsv'-'resolved_at';
  IF before_data=after_data THEN RETURN NEW; END IF;
 END IF;
 IF TG_OP='DELETE' THEN h=OLD.info_hash; ELSE h=NEW.info_hash; END IF;
 IF TG_OP='UPDATE' AND OLD.info_hash IS DISTINCT FROM NEW.info_hash THEN
  previous_hash=OLD.info_hash;
  PERFORM processor_enqueue_publication(previous_hash);
 END IF;
 PERFORM processor_enqueue_publication(h);
 IF TG_OP='DELETE' THEN RETURN OLD; END IF;
 RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER processor_publication_files AFTER INSERT OR UPDATE OR DELETE ON torrent_files
 FOR EACH ROW EXECUTE FUNCTION processor_publication_source_changed();
CREATE TRIGGER processor_publication_contents AFTER INSERT OR UPDATE OR DELETE ON torrent_contents
 FOR EACH ROW EXECUTE FUNCTION processor_publication_source_changed();
CREATE TRIGGER processor_publication_hints AFTER INSERT OR UPDATE OR DELETE ON torrent_hints
 FOR EACH ROW EXECUTE FUNCTION processor_publication_source_changed();
CREATE TRIGGER processor_publication_canonical AFTER INSERT OR UPDATE OR DELETE ON torrent_canonical_labels
 FOR EACH ROW EXECUTE FUNCTION processor_publication_source_changed();
CREATE TRIGGER processor_publication_tags AFTER INSERT OR UPDATE OR DELETE ON torrent_tags
 FOR EACH ROW EXECUTE FUNCTION processor_publication_source_changed();
CREATE TRIGGER processor_publication_evidence AFTER INSERT OR UPDATE OR DELETE ON label_evidence
 FOR EACH ROW EXECUTE FUNCTION processor_publication_source_changed();
CREATE TRIGGER processor_publication_exclusions AFTER INSERT OR UPDATE OR DELETE ON processor_exclusions
 FOR EACH ROW EXECUTE FUNCTION processor_publication_source_changed();
CREATE TRIGGER processor_publication_quarantine AFTER INSERT OR UPDATE OR DELETE ON junkpurge_quarantine
 FOR EACH ROW EXECUTE FUNCTION processor_publication_source_changed();
CREATE TRIGGER processor_publication_verdict AFTER INSERT OR UPDATE OR DELETE ON torrent_verdict_state
 FOR EACH ROW EXECUTE FUNCTION processor_publication_source_changed();

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM processor_publication_reviews) THEN
  RAISE EXCEPTION 'publication review evidence prevents destructive downgrade';
 END IF;
END $$;
-- +goose StatementEnd
DROP TRIGGER processor_publication_verdict ON torrent_verdict_state;
DROP TRIGGER processor_publication_quarantine ON junkpurge_quarantine;
DROP TRIGGER processor_publication_exclusions ON processor_exclusions;
DROP TRIGGER processor_publication_canonical ON torrent_canonical_labels;
DROP TRIGGER processor_publication_tags ON torrent_tags;
DROP TRIGGER processor_publication_evidence ON label_evidence;
DROP TRIGGER processor_publication_hints ON torrent_hints;
DROP TRIGGER processor_publication_contents ON torrent_contents;
DROP TRIGGER processor_publication_files ON torrent_files;
DROP TRIGGER processor_publication_torrent_pending ON torrents;
DROP FUNCTION processor_publication_source_changed();
DROP FUNCTION processor_publication_torrent_pending();
DROP FUNCTION processor_enqueue_publication(bytea);
ALTER TABLE processor_jobs DROP COLUMN publication_revision;
DROP TABLE processor_publication_pending,processor_publication_reviews,processor_publication_sources;
