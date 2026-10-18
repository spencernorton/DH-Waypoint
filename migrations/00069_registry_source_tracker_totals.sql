-- +goose Up
-- +goose StatementBegin

-- Where a processor registration came from: the operator API or a
-- plugins-directory file. Directory sync only ever modifies or
-- removes 'file' rows, and the column makes that ownership survive
-- restarts (5.5.0 tracked it in process memory, so a file deleted
-- while the core was stopped left its registration behind).
ALTER TABLE processor_registry
  ADD COLUMN source text NOT NULL DEFAULT 'api' CHECK (source IN ('api','file'));

-- All-time completion totals for the built-in tracker. Peer rows
-- expire with the retention sweep; without a separate counter the
-- scrape "downloaded" value would decrease as peers age out. No
-- foreign key to torrents, matching tracker_peers (catalogue
-- recovery retained-evidence class).
CREATE TABLE tracker_swarms
(
  info_hash       bytea primary key,
  completed_total bigint not null default 0 check (completed_total >= 0)
);

INSERT INTO tracker_swarms(info_hash, completed_total)
  SELECT info_hash, count(*) FROM tracker_peers WHERE ever_completed GROUP BY info_hash
  ON CONFLICT (info_hash) DO NOTHING;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS tracker_swarms;
ALTER TABLE processor_registry DROP COLUMN IF EXISTS source;

-- +goose StatementEnd
