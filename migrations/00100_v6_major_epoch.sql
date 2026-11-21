-- +goose Up
-- +goose StatementBegin
INSERT INTO key_values (key, value, created_at, updated_at)
VALUES ('schema_major', '6', now(), now())
ON CONFLICT (key) DO UPDATE
SET value = '6', updated_at = now();

INSERT INTO key_values (key, value, created_at, updated_at)
VALUES (
    'v6_upgrade_receipt',
    jsonb_build_object('schema_major', 6, 'recorded_at', to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'))::text,
    now(),
    now()
)
ON CONFLICT (key) DO NOTHING;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM torrents LIMIT 1) THEN
        RAISE EXCEPTION 'refusing destructive v6 -> v5 schema downgrade on a non-empty catalogue; restore a matched pre-v6 PostgreSQL backup';
    END IF;
END
$$;

DELETE FROM key_values WHERE key IN ('schema_major', 'v6_upgrade_receipt');
-- +goose StatementEnd
