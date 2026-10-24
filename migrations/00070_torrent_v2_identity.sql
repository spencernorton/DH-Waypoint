-- +goose Up
-- Existing v1 keys and all historical migrations remain unchanged. Validated
-- hybrids keep their SHA-1 key; pure v2 uses the complete SHA-256 key.
ALTER TABLE torrents ADD COLUMN info_hash_v2 bytea;
ALTER TABLE torrents ADD CONSTRAINT torrents_full_identity_check
 CHECK (octet_length(info_hash)=20 OR
        (octet_length(info_hash)=32 AND info_hash_v2 IS NOT NULL AND info_hash=info_hash_v2));
ALTER TABLE torrents ADD CONSTRAINT torrents_v2_hash_check
 CHECK (info_hash_v2 IS NULL OR octet_length(info_hash_v2)=32);
CREATE UNIQUE INDEX torrents_info_hash_v2 ON torrents(info_hash_v2) WHERE info_hash_v2 IS NOT NULL;

-- Complete identity columns only. Peer IDs, DHT keys and private tracker
-- announce keys remain exactly 20 bytes. Retired provider-work tables retain
-- their historical v1 constraints and cannot accept new v2 work.
-- +goose StatementBegin
DO $$
DECLARE tbl text;
BEGIN
 FOREACH tbl IN ARRAY ARRAY[
  'processor_jobs','processor_applications','processor_exclusions','processor_core_receipts',
  'processor_feedback_events','processor_reprocess_requests',
  'torrent_verdict_events','torrent_verdict_state','release_field_repair_journal',
  'catalogue_policy_shadow','catalogue_recovery_snapshots'
 ] LOOP
  EXECUTE format('ALTER TABLE %I DROP CONSTRAINT %I',tbl,tbl||'_info_hash_check');
  EXECUTE format('ALTER TABLE %I ADD CONSTRAINT %I CHECK (octet_length(info_hash) IN (20,32))',tbl,tbl||'_info_hash_check');
 END LOOP;
END $$;
-- +goose StatementEnd

-- +goose Down
-- Fail before removing any validated identity or permitting an older binary
-- to misread a full hash as a truncated SHA-1. Restore a matching backup.
-- +goose StatementBegin
DO $$
DECLARE tbl text; present boolean;
BEGIN
 IF EXISTS(SELECT 1 FROM torrents WHERE info_hash_v2 IS NOT NULL) THEN
  RAISE EXCEPTION 'v2 identities prevent downgrade; restore a matching backup';
 END IF;
 FOREACH tbl IN ARRAY ARRAY['label_evidence','torrent_canonical_labels','torrent_tracker_seeds','torrent_liveness',
  'torrent_grab_attempts','junkpurge_judgments','junkpurge_quarantine'] LOOP
  EXECUTE format('SELECT EXISTS(SELECT 1 FROM %I WHERE octet_length(info_hash)=32)',tbl) INTO present;
  IF present THEN RAISE EXCEPTION 'v2 evidence in % prevents downgrade; restore a matching backup',tbl; END IF;
 END LOOP;
 -- A removed hybrid still carries its full alias in retained evidence even
 -- though the canonical key is SHA-1 and its raw catalogue row is absent.
 IF EXISTS(SELECT 1 FROM catalogue_recovery_snapshots WHERE snapshot @? '$.**.info_hash_v2 ? (@ != null)')
 OR EXISTS(SELECT 1 FROM junkpurge_quarantine WHERE torrent_snapshot @? '$.**.info_hash_v2 ? (@ != null)')
 OR EXISTS(SELECT 1 FROM processor_jobs WHERE source @? '$.**.info_hash_v2 ? (@ != null)')
 OR EXISTS(SELECT 1 FROM processor_reprocess_requests WHERE before_snapshot @? '$.**.info_hash_v2 ? (@ != null)') THEN
  RAISE EXCEPTION 'v2 evidence in retained snapshots prevents downgrade; restore a matching backup';
 END IF;
 FOREACH tbl IN ARRAY ARRAY[
  'processor_jobs','processor_applications','processor_exclusions','processor_core_receipts',
  'processor_feedback_events','processor_reprocess_requests',
  'torrent_verdict_events','torrent_verdict_state','release_field_repair_journal',
  'catalogue_policy_shadow','catalogue_recovery_snapshots'
 ] LOOP
  EXECUTE format('SELECT EXISTS(SELECT 1 FROM %I WHERE octet_length(info_hash)<>20)',tbl) INTO present;
  IF present THEN RAISE EXCEPTION 'v2 evidence in % prevents downgrade; restore a matching backup',tbl; END IF;
 END LOOP;
 FOREACH tbl IN ARRAY ARRAY[
  'processor_jobs','processor_applications','processor_exclusions','processor_core_receipts',
  'processor_feedback_events','processor_reprocess_requests',
  'torrent_verdict_events','torrent_verdict_state','release_field_repair_journal',
  'catalogue_policy_shadow','catalogue_recovery_snapshots'
 ] LOOP
  EXECUTE format('ALTER TABLE %I DROP CONSTRAINT %I',tbl,tbl||'_info_hash_check');
  EXECUTE format('ALTER TABLE %I ADD CONSTRAINT %I CHECK (octet_length(info_hash)=20)',tbl,tbl||'_info_hash_check');
 END LOOP;
END $$;
-- +goose StatementEnd
DROP INDEX torrents_info_hash_v2;
ALTER TABLE torrents DROP CONSTRAINT torrents_full_identity_check;
ALTER TABLE torrents DROP CONSTRAINT torrents_v2_hash_check;
ALTER TABLE torrents DROP COLUMN info_hash_v2;
