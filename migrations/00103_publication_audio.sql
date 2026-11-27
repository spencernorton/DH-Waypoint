-- +goose Up
-- Preserve the decision with its exact publication review. NULL marks reviews
-- predating this contract. Backfill them in operator-selected, resumable batches.
ALTER TABLE processor_publication_reviews ADD COLUMN release_audio_v1 text;

-- +goose StatementBegin
CREATE FUNCTION publication_track_claim_v1(label text) RETURNS text
LANGUAGE plpgsql IMMUTABLE STRICT PARALLEL SAFE AS $$
DECLARE
    s text := lower(regexp_replace(label, '[._-]+', ' ', 'g'));
    en constant text := '\m(en|eng|english)\M';
    other_lang constant text := '\m(french|truefrench|fra|fre|german|deu|ger|spanish|spa|italian|ita|russian|rus|japanese|jpn|korean|kor|hindi|hin|tamil|tam|telugu|tel|portuguese|por|chinese|zho|mandarin|cantonese|arabic|ara|polish|pol|turkish|tur|ukrainian|ukr)\M';
    scoped_list text;
    technical_tail text;
    ambiguous boolean;
    segment text;
    kept text[] := '{}';
    preceding integer;
BEGIN
    IF s ~ ('\m(no|not|without|except|excluding|non)\M[[:space:]]+' || en)
        OR s ~ (en || '[[:space:]]+(absent|unavailable|excluded)\M') THEN RETURN 'conflict'; END IF;
    -- Commentary can be separated from its language by arbitrary channel,
    -- codec and edition labels. Discard the entire labelled track segment.
    -- Only an independently delimited main-audio claim can resolve this case.
    IF s ~ '\mcommentar(y|ies)\M' THEN
        FOREACH segment IN ARRAY regexp_split_to_array(s,'[\[\]|;]') LOOP
            IF btrim(segment)='' THEN CONTINUE; END IF;
            IF segment !~ '\mcommentar(y|ies)\M' THEN
                kept := array_append(kept,segment);
            ELSIF segment !~ en AND segment !~ other_lang THEN
                -- A role in its own brackets qualifies the preceding track.
                -- Drop intervening codec/channel fragments with that track.
                preceding := cardinality(kept);
                WHILE preceding>0 AND kept[preceding] !~ en AND kept[preceding] !~ other_lang LOOP
                    preceding := preceding-1;
                END LOOP;
                IF preceding=0 THEN RETURN 'conflict'; END IF;
                kept := kept[1:preceding-1];
            END IF;
        END LOOP;
        s := array_to_string(kept,' ');
        IF s !~ en THEN RETURN 'conflict'; END IF;
    END IF;
    -- An English commentary or subtitle may coexist with a main English track.
    -- Strip its own claim before looking for independent main-audio evidence.
    ambiguous := s ~ (en || '[[:space:]]+((audio|dub(bed)?|aac|ac3|ddp|dts|truehd|flac)[[:space:]]+)?(commentary|(hard|soft)?[[:space:]]*sub(s|bed|titles?)?)\M')
        OR s ~ ('\m(commentary|subs?|subtitles?)\M[[:space:]:=]+' || en)
        OR s ~ '\m(vostfr|subbed|esubs?|subs? only|raws)\M';
    s := regexp_replace(s, en || '[[:space:]]+((audio|dub(bed)?|aac|ac3|ddp|dts|truehd|flac)[[:space:]]+)?((director.?s?[[:space:]]+)?commentary|(hard|soft)?[[:space:]]*sub(s|bed|titles?)?)\M', ' ', 'g');
    s := regexp_replace(s, '\m(commentary|subtitles?|subs?)\M[[:space:]:=]+' || en, ' ', 'g');
    IF s ~ (en || '[[:space:]]+(audio|dub(bed)?|aac|ac3|ddp|dts|truehd|flac)\M')
        OR s ~ ('\m(audio|dub(bed)?)\M[[:space:]:=]+' || en) THEN RETURN 'english'; END IF;
    FOR scoped_list IN SELECT m[1] FROM regexp_matches(s,
        '\m(?:audio|tracks?)\M[[:space:]:=\[\(\]]*([a-z]{2,12}(?:[[:space:]]*[,/+][[:space:]]*[a-z]{2,12})+)', 'g') AS m LOOP
        IF scoped_list ~ en THEN RETURN 'english'; END IF;
        ambiguous := true;
    END LOOP;
    -- Bare language codes in a technical release suffix describe the edition.
    -- Catalogue title words and explicitly labelled subtitles carry no such role.
    technical_tail := substring(s FROM '\m(?:(?:19|20)[0-9]{2}|s[0-9]{1,2}(?:e[0-9]{1,3})?|[0-9]{3,4}[pi]|web[ ]?(?:dl|rip)|blu[ ]?ray|hdtv|dvdrip)\M.*');
    technical_tail := regexp_replace(coalesce(technical_tail,''), other_lang || '[[:space:]]+(subtitles?|subs?|commentary)\M', ' ', 'g');
    technical_tail := regexp_replace(technical_tail, '\m(subtitles?|subs?|commentary)\M[[:space:]:=]+' || other_lang, ' ', 'g');
    IF technical_tail ~ other_lang THEN ambiguous := true; END IF;
    IF s ~ ('(?:' || other_lang || ')[[:space:]]+(audio|dub(bed)?|aac|ac3|dts|only)\M')
        OR s ~ ('\m(audio|dub(bed)?)\M[[:space:]:=]+' || other_lang)
        OR s ~ '\m(vff|vfq|truefrench|pldub)\M' THEN
        ambiguous := true;
    END IF;
    IF ambiguous THEN RETURN 'conflict'; END IF;
    IF s ~ '\m(dual|multi)\M' THEN RETURN 'ambiguous'; END IF;
    RETURN 'unspecified';
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION publication_release_audio_v1(receipt jsonb, release_name text, media_names text[]) RETURNS text
LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $$
DECLARE
    root_claim text;
    child text;
    claim text;
    component text;
    episode_english boolean;
    all_files_english boolean := cardinality(media_names)>0;
    is_anime boolean;
    mixed_scope boolean;
BEGIN
    IF receipt->>'outcome' IS DISTINCT FROM 'completed'
       OR receipt#>>'{trace,neural_heads,category}' IS NULL
       OR receipt#>>'{trace,neural_heads,category}' NOT IN ('movie','tv_show')
       OR jsonb_typeof(receipt#>'{trace,anime}') IS DISTINCT FROM 'boolean'
       OR jsonb_typeof(receipt#>'{proposal,language,audio}') IS DISTINCT FROM 'array' THEN
        RETURN 'unknown';
    END IF;
    IF receipt#>>'{trace,audio_language}'='foreign' THEN RETURN 'foreign'; END IF;
    IF receipt#>>'{trace,audio_language}' IS DISTINCT FROM 'english'
       OR NOT jsonb_exists(receipt#>'{proposal,language,audio}', 'en')
       OR coalesce(receipt#>>'{proposal,language,evidence}','') NOT IN ('metadata_claim','verified_reference')
       OR coalesce(receipt#>>'{proposal,language,english_audio}','') IN ('sub','none') THEN
        RETURN 'unknown';
    END IF;
    is_anime := (receipt#>>'{trace,anime}')::boolean;
    root_claim := publication_track_claim_v1(release_name);
    mixed_scope := regexp_replace(lower(release_name),'[._-]+',' ','g') ~ '\m(mixed (audio|dub)|partial(ly)? (dub|english)|except episodes?|s[0-9]+e[0-9]+([ ]+e?[0-9]+)? only|only (ep(isode)?s? |s[0-9])|ep(isode)?s? [0-9]+.{0,18}only)\M';
    FOREACH child IN ARRAY coalesce(media_names,ARRAY[]::text[]) LOOP
        episode_english := false;
        FOREACH component IN ARRAY string_to_array(replace(child,chr(92),'/'),'/') LOOP
            claim := publication_track_claim_v1(component);
            IF claim='conflict' THEN RETURN 'conflict'; END IF;
            episode_english := episode_english OR claim='english';
        END LOOP;
        all_files_english := all_files_english AND episode_english;
    END LOOP;
    IF root_claim='conflict' THEN RETURN 'conflict'; END IF;
    IF root_claim='ambiguous' AND NOT coalesce(all_files_english,false) THEN RETURN 'unknown'; END IF;
    IF (mixed_scope OR (is_anime AND cardinality(media_names)>1 AND
        (root_claim IS DISTINCT FROM 'english' OR regexp_replace(lower(release_name),'[._-]+',' ','g') !~ '\m(complete|all episodes)\M')))
        AND NOT all_files_english THEN
        RETURN 'unknown_pack_audio';
    END IF;
    IF is_anime THEN
        IF root_claim='english' OR all_files_english OR receipt#>>'{proposal,language,english_audio}'='dub'
        THEN RETURN 'english_dub'; END IF;
        RETURN 'unknown_dub';
    END IF;
    RETURN 'english_main';
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION publication_audio_for_review_v1(h bytea, job text, identity jsonb) RETURNS text
LANGUAGE plpgsql STABLE AS $$
DECLARE
    receipt jsonb;
    root_name text;
    fs text;
    media text[] := '{}';
BEGIN
    -- Require the complete bound identity and exact job/hash. Another step,
    -- plugin or a later application cannot stand in for the reviewed result.
    SELECT CASE WHEN count(*)=1 THEN (array_agg(step->'result'))[1] END INTO receipt
      FROM processor_applications a CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(a.results)='array' THEN a.results ELSE '[]'::jsonb END) step
      WHERE a.info_hash=h AND a.job_id=job AND step#>'{result,processor}'=identity;
    IF receipt IS NULL THEN RETURN 'missing_receipt'; END IF;
    IF NOT EXISTS (SELECT 1 FROM torrent_contents tc WHERE tc.info_hash=h AND tc.content_type IN ('movie','tv_show'))
       OR EXISTS (SELECT 1 FROM torrent_contents tc WHERE tc.info_hash=h AND tc.content_type NOT IN ('movie','tv_show')) THEN
        RETURN 'unresolved_media_type';
    END IF;
    IF receipt#>>'{trace,audio_language}' IS DISTINCT FROM 'english' THEN
        RETURN publication_release_audio_v1(receipt,'',media);
    END IF;
    SELECT t.name,t.files_status::text INTO root_name,fs FROM torrents t WHERE t.info_hash=h;
    IF fs IS NULL OR fs NOT IN ('single','multi') THEN RETURN 'unknown'; END IF;
    IF fs='single' AND rtrim(btrim(root_name),'. ') !~* '[.](mkv|webm|mp4|m4v|mov|avi|ts|m2ts|mpg|mpeg|vob|wmv|flv|ogm)$' THEN RETURN 'unknown'; END IF;
    IF fs='multi' THEN
        SELECT array_agg(f.path) INTO media FROM torrent_files_for_hash(h) f
          WHERE rtrim(btrim(f.path),'. ') ~* '[.](mkv|webm|mp4|m4v|mov|avi|ts|m2ts|mpg|mpeg|vob|wmv|flv|ogm)$';
        IF coalesce(cardinality(media),0)=0 THEN RETURN 'unknown'; END IF;
    END IF;
    RETURN publication_release_audio_v1(receipt,root_name,media);
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION publication_record_audio_v1() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    NEW.release_audio_v1 := CASE WHEN NEW.approved
        THEN publication_audio_for_review_v1(NEW.info_hash,NEW.job_id,NEW.processor_identity) END;
    RETURN NEW;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER publication_record_audio_v1 BEFORE INSERT OR UPDATE OF approved,job_id,info_hash,processor_id,processor_identity,source_revision,execution_generation
  ON processor_publication_reviews FOR EACH ROW EXECUTE FUNCTION publication_record_audio_v1();

CREATE INDEX processor_publication_audio_pending ON processor_publication_reviews
 (processor_id,info_hash) WHERE approved AND release_audio_v1 IS NULL;

CREATE INDEX processor_publication_english_audio ON processor_publication_reviews
 (processor_id,execution_generation,info_hash) WHERE approved AND release_audio_v1 IN ('english_main','english_dub');

-- +goose StatementBegin
CREATE FUNCTION publication_reconcile_audio_v1(reviewer text, artifact text, batch_size integer)
RETURNS TABLE(decision text, n bigint) LANGUAGE plpgsql AS $$
BEGIN
    IF batch_size IS NULL OR batch_size NOT BETWEEN 1 AND 10000 THEN
        RAISE EXCEPTION 'audio reconciliation requires a batch size of 1..10000';
    END IF;
    RETURN QUERY WITH work AS MATERIALIZED (
        SELECT r.info_hash,r.processor_id FROM processor_publication_reviews r
         JOIN processor_registry p ON p.id=r.processor_id AND p.mode='apply' AND p.state='active'
           AND p.execution_generation=r.execution_generation AND p.manifest->'processor'=r.processor_identity
         JOIN processor_publication_sources s ON s.info_hash=r.info_hash AND s.revision=r.source_revision
         WHERE r.processor_id=reviewer AND r.approved AND r.release_audio_v1 IS NULL
           AND r.policy_contract='core-publication-policy-v1'
           AND r.processor_identity->>'artifact_digest'=artifact
         ORDER BY r.info_hash LIMIT batch_size FOR UPDATE OF r SKIP LOCKED
    ), updated AS (
        UPDATE processor_publication_reviews r SET release_audio_v1=
          publication_audio_for_review_v1(r.info_hash,r.job_id,r.processor_identity)
          FROM work w WHERE w.info_hash=r.info_hash AND w.processor_id=r.processor_id AND r.release_audio_v1 IS NULL
          RETURNING r.release_audio_v1
    ) SELECT u.release_audio_v1,count(*) FROM updated u GROUP BY u.release_audio_v1;
END
$$;
-- +goose StatementEnd

-- Queue only specifically budgeted lost results. A retry is an audit event for
-- the review, separate from a source revision or a fresh physical manifest.
-- +goose StatementBegin
CREATE FUNCTION publication_retry_missing_audio_v1(reviewer text, artifact text, batch_size integer)
RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE
    queued bigint;
BEGIN
    IF batch_size IS NULL OR batch_size NOT BETWEEN 1 AND 10000 THEN
        RAISE EXCEPTION 'audio re-review requires a batch size of 1..10000';
    END IF;
    WITH work AS MATERIALIZED (
        SELECT r.info_hash,r.processor_id FROM processor_publication_reviews r
         JOIN processor_registry p ON p.id=r.processor_id AND p.mode='apply' AND p.state='active'
           AND p.execution_generation=r.execution_generation AND p.manifest->'processor'=r.processor_identity
         JOIN processor_publication_sources s ON s.info_hash=r.info_hash AND s.revision=r.source_revision
         WHERE r.processor_id=reviewer AND r.approved AND r.release_audio_v1='missing_receipt'
           AND r.policy_contract='core-publication-policy-v1' AND r.processor_identity->>'artifact_digest'=artifact
         ORDER BY r.info_hash LIMIT batch_size FOR UPDATE OF r SKIP LOCKED
    ), request AS (
        UPDATE processor_publication_reviews r SET release_audio_v1='retry_requested' FROM work w
        WHERE w.info_hash=r.info_hash AND w.processor_id=r.processor_id RETURNING r.info_hash,r.source_revision
    ), pushed AS (
        INSERT INTO processor_publication_pending(info_hash,source_revision)
          SELECT info_hash,source_revision FROM request
          ON CONFLICT(info_hash) DO UPDATE SET source_revision=greatest(processor_publication_pending.source_revision,excluded.source_revision)
          RETURNING info_hash
    ) SELECT count(*) INTO queued FROM pushed;
    RETURN queued;
END
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION publication_retry_missing_audio_v1(text,text,integer);
DROP FUNCTION publication_reconcile_audio_v1(text,text,integer);
DROP TRIGGER publication_record_audio_v1 ON processor_publication_reviews;
DROP FUNCTION publication_record_audio_v1();
DROP INDEX processor_publication_audio_pending;
DROP INDEX processor_publication_english_audio;
ALTER TABLE processor_publication_reviews DROP COLUMN release_audio_v1;
DROP FUNCTION publication_audio_for_review_v1(bytea,text,jsonb);
DROP FUNCTION publication_release_audio_v1(jsonb,text,text[]);
DROP FUNCTION publication_track_claim_v1(text);
