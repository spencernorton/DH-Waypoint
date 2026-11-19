-- +goose Up
-- Opt-in ParadeDB BM25 support on stock PostgreSQL: two immutable helper
-- functions with zero storage cost until an operator enables BM25 with
-- `dh-waypoint search bm25 enable`, plus publication-trigger handling so the
-- generated search_text/ep_text columns that command may add can never churn
-- processor source revisions or approval snapshots.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION torrent_contents_tsv_text(tsvector) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$ SELECT array_to_string(tsvector_to_array($1), ' ') $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION torrent_contents_ep_tokens(jsonb) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
SELECT COALESCE(string_agg(tok, ' ' ORDER BY tok), '')
FROM (
 SELECT 's' || s.key || 'all' AS tok
 FROM jsonb_each(CASE WHEN jsonb_typeof($1)='object' THEN $1 ELSE '{}'::jsonb END) AS s(key, value)
 WHERE s.value='{}'::jsonb
 UNION ALL
 SELECT 's' || s.key || 'e' || e.key
 FROM jsonb_each(CASE WHEN jsonb_typeof($1)='object' THEN $1 ELSE '{}'::jsonb END) AS s(key, value)
 CROSS JOIN LATERAL jsonb_object_keys(CASE WHEN jsonb_typeof(s.value)='object' THEN s.value ELSE '{}'::jsonb END) AS e(key)
 WHERE s.value<>'{}'::jsonb
) t $$;
-- +goose StatementEnd

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
 IF current_setting('dh_waypoint.suppress_publication_trigger', true)='on' THEN
  IF TG_OP='DELETE' THEN RETURN OLD; END IF;
  RETURN NEW;
 END IF;
 IF TG_OP='UPDATE' THEN
  IF TG_TABLE_NAME='label_evidence' THEN
   IF ROW(OLD.info_hash,OLD.source,OLD.category) IS NOT DISTINCT FROM ROW(NEW.info_hash,NEW.source,NEW.category) THEN RETURN NEW; END IF;
  END IF;
  IF TG_TABLE_NAME='torrent_verdict_state' THEN
   IF ROW(OLD.info_hash,OLD.verdict) IS NOT DISTINCT FROM ROW(NEW.info_hash,NEW.verdict) THEN RETURN NEW; END IF;
  END IF;
  before_data=to_jsonb(OLD)-'created_at'-'updated_at'-'seeders'-'leechers'-'published_at'-'tsv'-'resolved_at'-'search_text'-'ep_text';
  after_data=to_jsonb(NEW)-'created_at'-'updated_at'-'seeders'-'leechers'-'published_at'-'tsv'-'resolved_at'-'search_text'-'ep_text';
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

-- +goose Down
DROP FUNCTION IF EXISTS torrent_contents_ep_tokens(jsonb);
DROP FUNCTION IF EXISTS torrent_contents_tsv_text(tsvector);

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
