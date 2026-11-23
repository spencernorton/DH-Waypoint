-- +goose Up
-- Bounded change-data-capture outbox for the DH-TMS sidecar sync worker.
-- Database triggers capture visibility, metadata, and swarm writes from any
-- binary (dh-waypoint, external processors, or direct operator SQL) without
-- requiring application-level dual-write discipline.

CREATE TABLE IF NOT EXISTS dhtms_outbox (
    seq BIGSERIAL PRIMARY KEY,
    op TEXT NOT NULL CHECK (op IN ('upsert', 'delete', 'meta', 'policy', 'swarm')),
    info_hash BYTEA NOT NULL CHECK (octet_length(info_hash) IN (20, 32)),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX IF NOT EXISTS dhtms_outbox_created_at_idx ON dhtms_outbox (created_at);

-- +goose StatementBegin
CREATE FUNCTION dhtms_record_outbox() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    target_op TEXT;
    target_hash BYTEA;
    prev_hash BYTEA;
BEGIN
    IF TG_TABLE_NAME = 'torrents' THEN
        IF TG_OP = 'DELETE' THEN
            target_op := 'delete';
            target_hash := OLD.info_hash;
        ELSIF TG_OP = 'INSERT' THEN
            target_op := 'upsert';
            target_hash := NEW.info_hash;
        ELSE
            IF NEW.info_hash IS NOT NULL AND octet_length(NEW.info_hash) IN (20, 32) THEN
                IF NEW.private IS DISTINCT FROM OLD.private THEN
                    INSERT INTO dhtms_outbox (op, info_hash) VALUES ('policy', NEW.info_hash);
                END IF;
                IF ROW(NEW.name, NEW.size, NEW.files_status, NEW.files_count, NEW.info_hash_v2)
                   IS DISTINCT FROM ROW(OLD.name, OLD.size, OLD.files_status, OLD.files_count, OLD.info_hash_v2) THEN
                    INSERT INTO dhtms_outbox (op, info_hash) VALUES ('meta', NEW.info_hash);
                END IF;
            END IF;
            RETURN NEW;
        END IF;
    ELSIF TG_TABLE_NAME = 'torrent_contents' THEN
        IF TG_OP = 'DELETE' THEN
            target_op := 'meta';
            target_hash := OLD.info_hash;
        ELSIF TG_OP = 'INSERT' THEN
            target_op := 'meta';
            target_hash := NEW.info_hash;
        ELSE
            IF ROW(NEW.content_type, NEW.content_source, NEW.content_id, NEW.release_date, NEW.video_resolution, NEW.published_at)
               IS DISTINCT FROM ROW(OLD.content_type, OLD.content_source, OLD.content_id, OLD.release_date, OLD.video_resolution, OLD.published_at) THEN
                target_op := 'meta';
            ELSIF ROW(NEW.seeders, NEW.leechers) IS DISTINCT FROM ROW(OLD.seeders, OLD.leechers) THEN
                target_op := 'swarm';
            ELSE
                RETURN NEW;
            END IF;
            target_hash := NEW.info_hash;
        END IF;
    ELSIF TG_TABLE_NAME IN ('torrent_files', 'torrent_file_bundles') THEN
        target_op := 'meta';
        IF TG_OP = 'DELETE' THEN
            target_hash := OLD.info_hash;
        ELSE
            target_hash := NEW.info_hash;
        END IF;
    ELSE
        -- Governance / visibility tables
        IF TG_OP = 'UPDATE' THEN
            IF TG_TABLE_NAME = 'label_evidence' THEN
                IF ROW(OLD.info_hash, OLD.source, OLD.category)
                   IS NOT DISTINCT FROM ROW(NEW.info_hash, NEW.source, NEW.category) THEN
                    RETURN NEW;
                END IF;
            END IF;
            IF TG_TABLE_NAME = 'torrent_verdict_state' THEN
                IF ROW(OLD.info_hash, OLD.verdict)
                   IS NOT DISTINCT FROM ROW(NEW.info_hash, NEW.verdict) THEN
                    RETURN NEW;
                END IF;
            END IF;
        END IF;
        target_op := 'policy';
        IF TG_OP = 'DELETE' THEN
            target_hash := OLD.info_hash;
        ELSE
            target_hash := NEW.info_hash;
            IF TG_OP = 'UPDATE' AND OLD.info_hash IS DISTINCT FROM NEW.info_hash THEN
                prev_hash := OLD.info_hash;
            END IF;
        END IF;
    END IF;

    IF prev_hash IS NOT NULL AND octet_length(prev_hash) IN (20, 32) THEN
        INSERT INTO dhtms_outbox (op, info_hash) VALUES (target_op, prev_hash);
    END IF;
    IF target_hash IS NOT NULL AND octet_length(target_hash) IN (20, 32) THEN
        INSERT INTO dhtms_outbox (op, info_hash) VALUES (target_op, target_hash);
    END IF;

    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd

CREATE TRIGGER dhtms_outbox_torrents
    AFTER INSERT OR UPDATE OR DELETE ON torrents
    FOR EACH ROW EXECUTE FUNCTION dhtms_record_outbox();
CREATE TRIGGER dhtms_outbox_torrent_contents
    AFTER INSERT OR UPDATE OR DELETE ON torrent_contents
    FOR EACH ROW EXECUTE FUNCTION dhtms_record_outbox();
CREATE TRIGGER dhtms_outbox_torrent_files
    AFTER INSERT OR UPDATE OR DELETE ON torrent_files
    FOR EACH ROW EXECUTE FUNCTION dhtms_record_outbox();
CREATE TRIGGER dhtms_outbox_torrent_file_bundles
    AFTER INSERT OR UPDATE OR DELETE ON torrent_file_bundles
    FOR EACH ROW EXECUTE FUNCTION dhtms_record_outbox();
CREATE TRIGGER dhtms_outbox_torrent_tags
    AFTER INSERT OR UPDATE OR DELETE ON torrent_tags
    FOR EACH ROW EXECUTE FUNCTION dhtms_record_outbox();
CREATE TRIGGER dhtms_outbox_torrent_canonical_labels
    AFTER INSERT OR UPDATE OR DELETE ON torrent_canonical_labels
    FOR EACH ROW EXECUTE FUNCTION dhtms_record_outbox();
CREATE TRIGGER dhtms_outbox_label_evidence
    AFTER INSERT OR UPDATE OR DELETE ON label_evidence
    FOR EACH ROW EXECUTE FUNCTION dhtms_record_outbox();
CREATE TRIGGER dhtms_outbox_processor_exclusions
    AFTER INSERT OR UPDATE OR DELETE ON processor_exclusions
    FOR EACH ROW EXECUTE FUNCTION dhtms_record_outbox();
CREATE TRIGGER dhtms_outbox_junkpurge_quarantine
    AFTER INSERT OR UPDATE OR DELETE ON junkpurge_quarantine
    FOR EACH ROW EXECUTE FUNCTION dhtms_record_outbox();
CREATE TRIGGER dhtms_outbox_torrent_verdict_state
    AFTER INSERT OR UPDATE OR DELETE ON torrent_verdict_state
    FOR EACH ROW EXECUTE FUNCTION dhtms_record_outbox();
CREATE TRIGGER dhtms_outbox_processor_publication_holds
    AFTER INSERT OR UPDATE OR DELETE ON processor_publication_holds
    FOR EACH ROW EXECUTE FUNCTION dhtms_record_outbox();
CREATE TRIGGER dhtms_outbox_processor_publication_reviews
    AFTER INSERT OR UPDATE OR DELETE ON processor_publication_reviews
    FOR EACH ROW EXECUTE FUNCTION dhtms_record_outbox();
CREATE TRIGGER dhtms_outbox_processor_payload_reviews
    AFTER INSERT OR UPDATE OR DELETE ON processor_payload_reviews
    FOR EACH ROW EXECUTE FUNCTION dhtms_record_outbox();

-- +goose Down
DROP TRIGGER IF EXISTS dhtms_outbox_processor_payload_reviews ON processor_payload_reviews;
DROP TRIGGER IF EXISTS dhtms_outbox_processor_publication_reviews ON processor_publication_reviews;
DROP TRIGGER IF EXISTS dhtms_outbox_processor_publication_holds ON processor_publication_holds;
DROP TRIGGER IF EXISTS dhtms_outbox_torrent_verdict_state ON torrent_verdict_state;
DROP TRIGGER IF EXISTS dhtms_outbox_junkpurge_quarantine ON junkpurge_quarantine;
DROP TRIGGER IF EXISTS dhtms_outbox_processor_exclusions ON processor_exclusions;
DROP TRIGGER IF EXISTS dhtms_outbox_label_evidence ON label_evidence;
DROP TRIGGER IF EXISTS dhtms_outbox_torrent_canonical_labels ON torrent_canonical_labels;
DROP TRIGGER IF EXISTS dhtms_outbox_torrent_tags ON torrent_tags;
DROP TRIGGER IF EXISTS dhtms_outbox_torrent_file_bundles ON torrent_file_bundles;
DROP TRIGGER IF EXISTS dhtms_outbox_torrent_files ON torrent_files;
DROP TRIGGER IF EXISTS dhtms_outbox_torrent_contents ON torrent_contents;
DROP TRIGGER IF EXISTS dhtms_outbox_torrents ON torrents;
DROP FUNCTION IF EXISTS dhtms_record_outbox();
DROP TABLE IF EXISTS dhtms_outbox;
