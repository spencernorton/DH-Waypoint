-- +goose Up
-- Audited operator exclusion retraction trail, pruning-churn fence on
-- processor_publication_source_changed(), episode GIN key extractor, HOT
-- fillfactor on torrents_torrent_sources, and hash-length statistics.
CREATE TABLE IF NOT EXISTS processor_exclusion_events (
 request_id text PRIMARY KEY,
 info_hash bytea NOT NULL,
 expected_source_revision bigint NOT NULL,
 source_revision bigint NOT NULL,
 removed_count integer NOT NULL,
 reason text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX IF NOT EXISTS processor_exclusion_events_hash_idx
 ON processor_exclusion_events (info_hash, created_at DESC);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION processor_publication_source_changed() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
 h bytea;
 previous_hash bytea;
 before_data jsonb;
 after_data jsonb;
 ws constant text := E'\t\n\x0b\f\r \u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000';
 old_private boolean;
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
 IF TG_OP='DELETE' AND TG_TABLE_NAME='label_evidence' THEN
  IF OLD.source_kind='qb_state_observation' AND OLD.info_hash IS NOT NULL THEN
   old_private := (lower(btrim(coalesce(OLD.source,''),ws))='qbittorrent' AND lower(btrim(coalesce(OLD.category,''),ws)) IN ('private','bitgrab'));
   IF EXISTS (
    SELECT 1 FROM label_evidence n
    WHERE n.info_hash=OLD.info_hash AND n.source_kind='qb_state_observation'
     AND (NOT old_private OR (lower(btrim(coalesce(n.source,''),ws))='qbittorrent' AND lower(btrim(coalesce(n.category,''),ws)) IN ('private','bitgrab')))
   ) THEN
    RETURN OLD;
   END IF;
  END IF;
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

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION torrent_contents_episode_keys(ct text, cs text, cid text, ep jsonb)
RETURNS text[]
LANGUAGE plpgsql
IMMUTABLE STRICT PARALLEL SAFE
AS $$
DECLARE
 keys text[] := ARRAY[]::text[];
 prefix text;
 s_key text;
 s_val jsonb;
 e_key text;
BEGIN
 IF jsonb_typeof(ep) <> 'object' THEN
  RETURN keys;
 END IF;
 prefix := ct || ':' || cs || ':' || cid;
 FOR s_key, s_val IN SELECT key, value FROM jsonb_each(ep) LOOP
  keys := array_append(keys, prefix || ':s:' || s_key);
  IF s_val = '{}'::jsonb THEN
   keys := array_append(keys, prefix || ':s_all:' || s_key);
  ELSIF jsonb_typeof(s_val) = 'object' THEN
   FOR e_key IN SELECT jsonb_object_keys(s_val) LOOP
    keys := array_append(keys, prefix || ':e:' || s_key || ':' || e_key);
   END LOOP;
  END IF;
 END LOOP;
 RETURN keys;
END $$;
-- +goose StatementEnd

ALTER TABLE torrents_torrent_sources SET (fillfactor = 90);

CREATE STATISTICS IF NOT EXISTS torrent_contents_hash_len_stats
 ON (octet_length(info_hash)) FROM torrent_contents;

-- +goose Down
DROP STATISTICS IF EXISTS torrent_contents_hash_len_stats;
ALTER TABLE torrents_torrent_sources RESET (fillfactor);
DROP FUNCTION IF EXISTS torrent_contents_episode_keys(text, text, text, jsonb);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION processor_publication_source_changed() RETURNS trigger LANGUAGE plpgsql AS $$
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

DROP TABLE IF EXISTS processor_exclusion_events;
