package tracker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencernorton/DH-Waypoint/internal/importer"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/metainfo"
	"go.uber.org/zap"
)

// UploadSource is the torrent source key recorded for operator
// uploads through the private file API.
const UploadSource = "upload"

// peerRetention bounds how long an announce row survives the sweep.
// Freshness (visibility to other peers) is the much shorter
// Config.FreshnessWindow; retention only keeps the completed count
// meaningful for scrape responses.
const peerRetention = 7 * 24 * time.Hour

var ErrUnregisteredTorrent = errors.New("unregistered torrent")

// Store owns tracker peer state and stored torrent files. Torrent
// rows themselves are created through the shared importer, so CSAM,
// blocking and name-policy gates apply to uploads exactly as they
// apply to bulk imports.
type Store struct {
	cfg      Config
	pool     lazy.Lazy[*pgxpool.Pool]
	importer lazy.Lazy[importer.Importer]
	logger   *zap.SugaredLogger
}

func NewStore(cfg Config, pool lazy.Lazy[*pgxpool.Pool], imp lazy.Lazy[importer.Importer]) (*Store, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Store{cfg: cfg, pool: pool, importer: imp}, nil
}

func (s *Store) SetLogger(l *zap.SugaredLogger) { s.logger = l }

func (s *Store) log() *zap.SugaredLogger {
	if s.logger != nil {
		return s.logger
	}
	return zap.NewNop().Sugar()
}

func (s *Store) TorrentExists(ctx context.Context, hash protocol.ID) (bool, error) {
	p, err := s.pool.Get()
	if err != nil {
		return false, err
	}
	var exists bool
	err = p.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM torrents WHERE info_hash=$1)`, hash.Bytes()).Scan(&exists)
	return exists, err
}

func (s *Store) swarmCounts(ctx context.Context, hash protocol.ID) (SwarmCounts, error) {
	p, err := s.pool.Get()
	if err != nil {
		return SwarmCounts{}, err
	}
	cutoff := time.Now().UTC().Add(-s.cfg.FreshnessWindow())
	var c SwarmCounts
	err = p.QueryRow(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN left_bytes = 0 AND last_announce >= $2 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN left_bytes > 0 AND last_announce >= $2 THEN 1 ELSE 0 END), 0)
		FROM tracker_peers WHERE info_hash = $1`, hash.Bytes(), cutoff).Scan(&c.Seeders, &c.Leechers)
	if err != nil {
		return c, err
	}
	// Completed is the all-time total; it outlives peer-row retention.
	err = p.QueryRow(ctx, `SELECT COALESCE((SELECT completed_total FROM tracker_swarms WHERE info_hash = $1), 0)`, hash.Bytes()).Scan(&c.Completed)
	return c, err
}

// Announce records one peer announce and returns the response
// payload. The torrent must already exist in the catalogue.
func (s *Store) Announce(ctx context.Context, a Announce, ip string) (AnnounceResponse, error) {
	exists, err := s.TorrentExists(ctx, a.InfoHash)
	if err != nil {
		return AnnounceResponse{}, err
	}
	if !exists {
		return AnnounceResponse{}, ErrUnregisteredTorrent
	}
	p, err := s.pool.Get()
	if err != nil {
		return AnnounceResponse{}, err
	}
	if a.Event == "stopped" {
		_, err = p.Exec(ctx, `DELETE FROM tracker_peers WHERE info_hash=$1 AND peer_id=$2 AND ip=$3 AND port=$4`,
			a.InfoHash.Bytes(), a.PeerID, ip, a.Port)
		if err != nil {
			return AnnounceResponse{}, err
		}
	} else if err := s.recordPeer(ctx, a, ip); err != nil {
		return AnnounceResponse{}, err
	}

	counts, err := s.swarmCounts(ctx, a.InfoHash)
	if err != nil {
		return AnnounceResponse{}, err
	}
	limit := a.NumWant
	if limit <= 0 || limit > s.cfg.MaxPeers {
		limit = s.cfg.MaxPeers
	}
	cutoff := time.Now().UTC().Add(-s.cfg.FreshnessWindow())
	rows, err := p.Query(ctx, `
		SELECT peer_id, ip, port FROM tracker_peers
		WHERE info_hash=$1 AND last_announce >= $2
		  AND NOT (peer_id=$3 AND ip=$4 AND port=$5)
		ORDER BY last_announce DESC LIMIT $6`,
		a.InfoHash.Bytes(), cutoff, a.PeerID, ip, a.Port, limit)
	if err != nil {
		return AnnounceResponse{}, err
	}
	defer rows.Close()
	var peers []Peer
	for rows.Next() {
		var peer Peer
		if err := rows.Scan(&peer.PeerID, &peer.IP, &peer.Port); err != nil {
			return AnnounceResponse{}, err
		}
		peers = append(peers, peer)
	}
	if err := rows.Err(); err != nil {
		return AnnounceResponse{}, err
	}

	resp := AnnounceResponse{
		Interval:   int(s.cfg.AnnounceInterval.Seconds()),
		Complete:   counts.Seeders,
		Incomplete: counts.Leechers,
	}
	if a.Compact {
		resp.Peers, resp.Peers6 = CompactPeers(peers)
		if resp.Peers == nil {
			resp.Peers = []byte{}
		}
	} else {
		resp.PeerList = peers
	}
	return resp, nil
}

// recordPeer upserts one peer and, on the peer's first completion,
// increments the all-time completed total exactly once.
func (s *Store) recordPeer(ctx context.Context, a Announce, ip string) error {
	p, err := s.pool.Get()
	if err != nil {
		return err
	}
	completed := a.Left == 0 || a.Event == "completed"
	tx, err := p.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var wasCompleted bool
	err = tx.QueryRow(ctx, `SELECT ever_completed FROM tracker_peers WHERE info_hash=$1 AND peer_id=$2 AND ip=$3 AND port=$4 FOR UPDATE`,
		a.InfoHash.Bytes(), a.PeerID, ip, a.Port).Scan(&wasCompleted)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO tracker_peers(info_hash, peer_id, ip, port, uploaded, downloaded, left_bytes, ever_completed, last_announce)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8, clock_timestamp())
		ON CONFLICT(info_hash, peer_id, ip, port) DO UPDATE SET
			uploaded=$5, downloaded=$6, left_bytes=$7,
			ever_completed = tracker_peers.ever_completed OR $8,
			last_announce = clock_timestamp()`,
		a.InfoHash.Bytes(), a.PeerID, ip, a.Port, a.Uploaded, a.Downloaded, a.Left, completed); err != nil {
		return err
	}
	if completed && !wasCompleted {
		if _, err := tx.Exec(ctx, `
			INSERT INTO tracker_swarms(info_hash, completed_total) VALUES($1, 1)
			ON CONFLICT(info_hash) DO UPDATE SET completed_total = tracker_swarms.completed_total + 1`,
			a.InfoHash.Bytes()); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Scrape returns counts for each known hash; unknown hashes are
// omitted, as trackers conventionally do.
func (s *Store) Scrape(ctx context.Context, hashes []protocol.ID) (map[protocol.ID]SwarmCounts, error) {
	out := make(map[protocol.ID]SwarmCounts, len(hashes))
	for _, hash := range hashes {
		exists, err := s.TorrentExists(ctx, hash)
		if err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		counts, err := s.swarmCounts(ctx, hash)
		if err != nil {
			return nil, err
		}
		out[hash] = counts
	}
	return out, nil
}

// PutTorrentFile stores (or replaces) the raw .torrent file for a
// catalogue torrent.
func (s *Store) PutTorrentFile(ctx context.Context, hash protocol.ID, torrentFile []byte) error {
	p, err := s.pool.Get()
	if err != nil {
		return err
	}
	_, err = p.Exec(ctx, `
		INSERT INTO private_torrent_files(info_hash, torrent_file) VALUES($1,$2)
		ON CONFLICT(info_hash) DO UPDATE SET torrent_file=$2, updated_at=clock_timestamp()`,
		hash.Bytes(), torrentFile)
	return err
}

// GetTorrentFile returns the stored raw .torrent file, or pgx.ErrNoRows.
func (s *Store) GetTorrentFile(ctx context.Context, hash protocol.ID) ([]byte, error) {
	p, err := s.pool.Get()
	if err != nil {
		return nil, err
	}
	var data []byte
	err = p.QueryRow(ctx, `SELECT torrent_file FROM private_torrent_files WHERE info_hash=$1`, hash.Bytes()).Scan(&data)
	return data, err
}

// Detail is the private-API view of one torrent.
type Detail struct {
	InfoHash  string       `json:"info_hash"`
	Name      string       `json:"name"`
	Size      uint         `json:"size"`
	Private   bool         `json:"private"`
	HasFile   bool         `json:"has_torrent_file"`
	Files     []DetailFile `json:"files"`
	Seeders   int          `json:"seeders"`
	Leechers  int          `json:"leechers"`
	Completed int          `json:"completed"`
}

type DetailFile struct {
	Path string `json:"path"`
	Size uint   `json:"size"`
}

// Detail returns pgx.ErrNoRows when the torrent is unknown.
func (s *Store) Detail(ctx context.Context, hash protocol.ID) (Detail, error) {
	p, err := s.pool.Get()
	if err != nil {
		return Detail{}, err
	}
	var d Detail
	d.InfoHash = hash.String()
	err = p.QueryRow(ctx, `
		SELECT t.name, t.size, t.private, EXISTS(SELECT 1 FROM private_torrent_files f WHERE f.info_hash = t.info_hash)
		FROM torrents t WHERE t.info_hash = $1`, hash.Bytes()).Scan(&d.Name, &d.Size, &d.Private, &d.HasFile)
	if err != nil {
		return Detail{}, err
	}
	rows, err := p.Query(ctx, `SELECT path, size FROM torrent_files WHERE info_hash=$1 ORDER BY index`, hash.Bytes())
	if err != nil {
		return Detail{}, err
	}
	defer rows.Close()
	d.Files = []DetailFile{}
	for rows.Next() {
		var f DetailFile
		if err := rows.Scan(&f.Path, &f.Size); err != nil {
			return Detail{}, err
		}
		d.Files = append(d.Files, f)
	}
	if err := rows.Err(); err != nil {
		return Detail{}, err
	}
	counts, err := s.swarmCounts(ctx, hash)
	if err != nil {
		return Detail{}, err
	}
	d.Seeders, d.Leechers, d.Completed = counts.Seeders, counts.Leechers, counts.Completed
	return d, nil
}

// UploadResult reports an accepted upload.
type UploadResult struct {
	InfoHash string `json:"info_hash"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Private  bool   `json:"private"`
	Existed  bool   `json:"existed"`
}

// Upload ingests one .torrent file: validate, create the catalogue
// row through the shared importer (all ingestion gates apply),
// record its file inventory, and store the raw bytes for download.
func (s *Store) Upload(ctx context.Context, torrentFile []byte) (UploadResult, error) {
	hash, info, err := ParseTorrentFile(torrentFile)
	if err != nil {
		return UploadResult{}, fmt.Errorf("%w: %v", ErrInvalidTorrentFile, err)
	}
	existed, err := s.TorrentExists(ctx, hash)
	if err != nil {
		return UploadResult{}, err
	}

	private := info.Private != nil && *info.Private
	// Re-uploading a known hash only refreshes the stored file and
	// inventory: the first upload already created the catalogue row
	// and its processing job (a duplicate job insert would collide
	// on the queue fingerprint while the first is still pending).
	if !existed {
		imp, err := s.importer.Get()
		if err != nil {
			return UploadResult{}, err
		}
		ai := imp.New(ctx, importer.Info{ID: "private-upload"})
		if err := ai.Import(importer.Item{
			Source:   UploadSource,
			InfoHash: hash,
			Name:     info.BestName(),
			Size:     uint(info.TotalLength()),
			Private:  private,
		}); err != nil {
			_ = ai.Close()
			return UploadResult{}, err
		}
		ai.Drain()
		if err := ai.Close(); err != nil {
			return UploadResult{}, err
		}
		if err := ai.Err(); err != nil {
			return UploadResult{}, err
		}

		// A hash dropped by an ingestion gate never lands. Refuse
		// without naming the gate.
		nowExists, err := s.TorrentExists(ctx, hash)
		if err != nil {
			return UploadResult{}, err
		}
		if !nowExists {
			return UploadResult{}, ErrRejectedByPolicy
		}
	}

	if err := s.recordFiles(ctx, hash, info); err != nil {
		return UploadResult{}, err
	}
	if err := s.PutTorrentFile(ctx, hash, torrentFile); err != nil {
		return UploadResult{}, err
	}
	return UploadResult{
		InfoHash: hash.String(),
		Name:     info.BestName(),
		Size:     info.TotalLength(),
		Private:  private,
		Existed:  existed,
	}, nil
}

var (
	ErrInvalidTorrentFile = errors.New("invalid torrent file")
	ErrRejectedByPolicy   = errors.New("rejected by ingestion policy")
)

// recordFiles mirrors the crawler's file-inventory persistence for
// an uploaded torrent (the importer alone leaves files_status at
// no-info).
func (s *Store) recordFiles(ctx context.Context, hash protocol.ID, info metainfo.Info) error {
	p, err := s.pool.Get()
	if err != nil {
		return err
	}
	status := model.FilesStatusSingle
	var filesCount *uint
	if len(info.Files) > 0 {
		status = model.FilesStatusMulti
		n := uint(len(info.Files))
		filesCount = &n
	}
	tx, err := p.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for i, f := range info.Files {
		if _, err := tx.Exec(ctx, `
			INSERT INTO torrent_files(info_hash, index, path, size, created_at, updated_at)
			VALUES($1,$2,$3,$4, clock_timestamp(), clock_timestamp())
			ON CONFLICT DO NOTHING`, hash.Bytes(), i, f.DisplayPath(&info), f.Length); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE torrents SET files_status=$2, files_count=$3, updated_at=clock_timestamp() WHERE info_hash=$1`,
		hash.Bytes(), string(status), filesCount); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Sweep deletes peer rows beyond the retention window. Called by
// the tracker worker; safe to call any time.
func (s *Store) Sweep(ctx context.Context) (int64, error) {
	p, err := s.pool.Get()
	if err != nil {
		return 0, err
	}
	tag, err := p.Exec(ctx, `DELETE FROM tracker_peers WHERE last_announce < $1`,
		time.Now().UTC().Add(-peerRetention))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
