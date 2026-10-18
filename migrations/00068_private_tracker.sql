-- +goose Up
-- +goose StatementBegin

-- Raw .torrent files for operator-uploaded torrents. The crawler stores
-- parsed metainfo facts but not the original file bytes, so a faithful
-- .torrent cannot be reconstructed from torrents/torrent_files alone.
-- Uploads keep their exact bytes here; downloads serve them with the
-- announce URL rewritten outside the info dictionary, which leaves the
-- info hash unchanged.
-- Neither table carries a foreign key to torrents, deliberately:
-- catalogue recovery snapshots enumerate every FK relation of
-- torrents and fail closed on unknown ones, and these rows belong to
-- the retained-evidence class that recovery leaves untouched (like
-- label evidence and liveness). A recovery removal therefore keeps
-- the stored file and peer rows, and a restore reattaches them.
-- Membership is enforced at write time by the tracker store, and
-- reads always join back to torrents.
create table private_torrent_files
(
  info_hash    bytea primary key,
  torrent_file bytea not null check (octet_length(torrent_file) > 0 and octet_length(torrent_file) <= 12582912),
  created_at   timestamptz not null default now(),
  updated_at   timestamptz not null default now()
);

-- Peers announced to the built-in tracker. One row per
-- (torrent, peer_id, ip, port); last_announce drives freshness and the
-- periodic sweep. Counts are derived at read time: seeders are peers
-- with left_bytes = 0, completed counts peers that ever announced
-- left_bytes = 0 or event=completed.
create table tracker_peers
(
  info_hash      bytea not null,
  peer_id        bytea not null check (octet_length(peer_id) = 20),
  ip             text  not null check (char_length(ip) between 3 and 45),
  port           integer not null check (port between 1 and 65535),
  uploaded       bigint not null default 0 check (uploaded >= 0),
  downloaded     bigint not null default 0 check (downloaded >= 0),
  left_bytes     bigint not null default 0 check (left_bytes >= 0),
  ever_completed boolean not null default false,
  last_announce  timestamptz not null default now(),
  primary key (info_hash, peer_id, ip, port)
);

create index tracker_peers_last_announce_idx
  on tracker_peers (last_announce);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

drop table if exists tracker_peers;
drop table if exists private_torrent_files;

-- +goose StatementEnd
