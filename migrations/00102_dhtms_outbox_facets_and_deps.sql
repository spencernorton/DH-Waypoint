-- +goose Up
-- Expand DH-TMS change-data-capture triggers to cover new facet columns
-- (is_anime, video_3d, video_source, video_codec), serving-policy dependency
-- tables (processor_publication_sources, torrent_hints, content.adult), and
-- catalog entity / alternate-ID changes so the sidecar never retains stale
-- visibility or metadata after PostgreSQL updates.

ALTER TABLE dhtms_outbox DROP CONSTRAINT IF EXISTS dhtms_outbox_op_check;
ALTER TABLE dhtms_outbox ADD CONSTRAINT dhtms_outbox_op_check
    CHECK (op IN ('upsert', 'delete', 'meta', 'policy', 'swarm', 'catalog'));

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION dhtms_record_outbox() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    target_op TEXT;
    target_hash BYTEA;
    prev_hash BYTEA;
    catalog_sentinel CONSTANT BYTEA := decode('0000000000000000000000000000000000000000', 'hex');
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
            IF ROW(
                NEW.content_type, NEW.content_source, NEW.content_id,
                NEW.release_date, NEW.video_resolution, NEW.published_at,
                NEW.is_anime, NEW.video_3d, NEW.video_source, NEW.video_codec
            ) IS DISTINCT FROM ROW(
                OLD.content_type, OLD.content_source, OLD.content_id,
                OLD.release_date, OLD.video_resolution, OLD.published_at,
                OLD.is_anime, OLD.video_3d, OLD.video_source, OLD.video_codec
            ) THEN
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
    ELSIF TG_TABLE_NAME = 'content' THEN
        IF TG_OP = 'DELETE' THEN
            INSERT INTO dhtms_outbox (op, info_hash)
            SELECT DISTINCT 'policy', tc.info_hash
            FROM torrent_contents tc
            WHERE tc.content_type = OLD.type
              AND tc.content_source = OLD.source
              AND tc.content_id = OLD.id
              AND octet_length(tc.info_hash) IN (20, 32);
            INSERT INTO dhtms_outbox (op, info_hash) VALUES ('catalog', catalog_sentinel);
            RETURN OLD;
        ELSIF TG_OP = 'INSERT' THEN
            INSERT INTO dhtms_outbox (op, info_hash) VALUES ('catalog', catalog_sentinel);
            RETURN NEW;
        ELSE
            IF NEW.adult IS DISTINCT FROM OLD.adult THEN
                INSERT INTO dhtms_outbox (op, info_hash)
                SELECT DISTINCT 'policy', tc.info_hash
                FROM torrent_contents tc
                WHERE tc.content_type = NEW.type
                  AND tc.content_source = NEW.source
                  AND tc.content_id = NEW.id
                  AND octet_length(tc.info_hash) IN (20, 32);
            END IF;
            IF ROW(NEW.title, NEW.original_title, NEW.release_year, NEW.adult)
               IS DISTINCT FROM ROW(OLD.title, OLD.original_title, OLD.release_year, OLD.adult) THEN
                INSERT INTO dhtms_outbox (op, info_hash) VALUES ('catalog', catalog_sentinel);
            END IF;
            RETURN NEW;
        END IF;
    ELSIF TG_TABLE_NAME = 'content_attributes' THEN
        IF TG_OP = 'DELETE' THEN
            IF OLD.key = 'id' AND COALESCE(OLD.value, '') <> '' THEN
                INSERT INTO dhtms_outbox (op, info_hash) VALUES ('catalog', catalog_sentinel);
            END IF;
            RETURN OLD;
        ELSIF TG_OP = 'INSERT' THEN
            IF NEW.key = 'id' AND COALESCE(NEW.value, '') <> '' THEN
                INSERT INTO dhtms_outbox (op, info_hash) VALUES ('catalog', catalog_sentinel);
            END IF;
            RETURN NEW;
        ELSE
            IF (NEW.key = 'id' OR OLD.key = 'id')
               AND ROW(NEW.content_type, NEW.content_source, NEW.content_id, NEW.source, NEW.key, NEW.value)
                   IS DISTINCT FROM ROW(OLD.content_type, OLD.content_source, OLD.content_id, OLD.source, OLD.key, OLD.value) THEN
                INSERT INTO dhtms_outbox (op, info_hash) VALUES ('catalog', catalog_sentinel);
            END IF;
            RETURN NEW;
        END IF;
    ELSE
        -- Governance / visibility / source-revision tables
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
            IF TG_TABLE_NAME = 'processor_publication_sources' THEN
                IF ROW(OLD.info_hash, OLD.revision)
                   IS NOT DISTINCT FROM ROW(NEW.info_hash, NEW.revision) THEN
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

DROP TRIGGER IF EXISTS dhtms_outbox_processor_publication_sources ON processor_publication_sources;
CREATE TRIGGER dhtms_outbox_processor_publication_sources
    AFTER INSERT OR UPDATE OR DELETE ON processor_publication_sources
    FOR EACH ROW EXECUTE FUNCTION dhtms_record_outbox();

DROP TRIGGER IF EXISTS dhtms_outbox_torrent_hints ON torrent_hints;
CREATE TRIGGER dhtms_outbox_torrent_hints
    AFTER INSERT OR UPDATE OR DELETE ON torrent_hints
    FOR EACH ROW EXECUTE FUNCTION dhtms_record_outbox();

DROP TRIGGER IF EXISTS dhtms_outbox_content ON content;
CREATE TRIGGER dhtms_outbox_content
    AFTER INSERT OR UPDATE OR DELETE ON content
    FOR EACH ROW EXECUTE FUNCTION dhtms_record_outbox();

DROP TRIGGER IF EXISTS dhtms_outbox_content_attributes ON content_attributes;
CREATE TRIGGER dhtms_outbox_content_attributes
    AFTER INSERT OR UPDATE OR DELETE ON content_attributes
    FOR EACH ROW EXECUTE FUNCTION dhtms_record_outbox();

-- +goose Down
DROP TRIGGER IF EXISTS dhtms_outbox_content_attributes ON content_attributes;
DROP TRIGGER IF EXISTS dhtms_outbox_content ON content;
DROP TRIGGER IF EXISTS dhtms_outbox_torrent_hints ON torrent_hints;
DROP TRIGGER IF EXISTS dhtms_outbox_processor_publication_sources ON processor_publication_sources;
DELETE FROM dhtms_outbox WHERE op = 'catalog';
ALTER TABLE dhtms_outbox DROP CONSTRAINT IF EXISTS dhtms_outbox_op_check;
ALTER TABLE dhtms_outbox ADD CONSTRAINT dhtms_outbox_op_check
    CHECK (op IN ('upsert', 'delete', 'meta', 'policy', 'swarm'));
