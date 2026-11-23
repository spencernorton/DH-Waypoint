package dhtms

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/serving"
	"github.com/spencernorton/DH-Waypoint/internal/worker"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

const (
	syncWorkerKey    = "dhtms_sync"
	outboxBatchLimit = 2000
)

type catalogKey struct {
	contentType string
	source      string
	id          string
}

// Syncer synchronizes PostgreSQL catalogue, serving policy, metadata, and swarm
// state into a co-located dh-tmsd sidecar via streaming NDJSON generation builds,
// incremental outbox CDC, and monotonic PG-LSN governance lease renewals.
type Syncer struct {
	pool    *pgxpool.Pool
	client  *Client
	policy  *serving.Policy
	cfg     Config
	metrics *Metrics
	logger  *zap.SugaredLogger

	mu               sync.Mutex
	bootstrapped     bool
	lastSeq          int64
	lastLSN          uint64
	lastGenerationID uint64
	catalogEntity         map[catalogKey]uint32
	pendingRebuild        bool
	firstNewHashAt        time.Time
	hasLegacyTorrentFiles bool
}

// NewSyncer creates a Syncer bound to a PostgreSQL pool, DH-TMS client, and serving policy.
func NewSyncer(
	pool *pgxpool.Pool,
	client *Client,
	policy *serving.Policy,
	cfg Config,
	metrics *Metrics,
	logger *zap.SugaredLogger,
) *Syncer {
	if cfg.SyncInterval <= 0 {
		cfg.SyncInterval = defaultSyncInterval
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = defaultLeaseTTL
	}
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	return &Syncer{
		pool:          pool,
		client:        client,
		policy:        policy,
		cfg:           cfg,
		metrics:       metrics,
		logger:        logger.Named(syncWorkerKey),
		catalogEntity: make(map[catalogKey]uint32),
	}
}

// FullBuild takes a REPEATABLE READ READ ONLY snapshot of PostgreSQL, evaluates
// DH-Waypoint's canonical serving.Policy predicates inside PostgreSQL (ensuring
// exact visibility parity by construction), streams NDJSON to dh-tmsd's
// /v1/generation/build over HTTP chunked transfer, and prunes consumed outbox rows.
func (s *Syncer) FullBuild(ctx context.Context) (*BuildGenerationResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fullBuildLocked(ctx)
}

func (s *Syncer) fullBuildLocked(ctx context.Context) (*BuildGenerationResponse, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		s.metrics.RecordSync("full_build", "error")
		return nil, fmt.Errorf("dhtms sync: begin snapshot tx: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var lsnText string
	var snapshotMaxSeq int64
	if err := tx.QueryRow(ctx, `SELECT pg_current_wal_lsn()::text, COALESCE((SELECT MAX(seq) FROM dhtms_outbox), 0)`).
		Scan(&lsnText, &snapshotMaxSeq); err != nil {
		s.metrics.RecordSync("full_build", "error")
		return nil, fmt.Errorf("dhtms sync: read snapshot lsn/outbox: %w", err)
	}
	lsnU64, err := ParsePGLSN(lsnText)
	if err != nil {
		lsnU64 = s.lastLSN + 1
	}
	if lsnU64 <= s.lastLSN {
		lsnU64 = s.lastLSN + 1
	}

	// Load content catalog + alternate identifiers first so docs resolve entity_id.
	altMap := make(map[catalogKey][][2]string)
	attrRows, err := tx.Query(ctx, `SELECT content_type, content_source, content_id, source, value
FROM content_attributes
WHERE key = 'id' AND value <> ''`)
	if err != nil {
		s.metrics.RecordSync("full_build", "error")
		return nil, fmt.Errorf("dhtms sync: query content_attributes: %w", err)
	}
	for attrRows.Next() {
		var ct, csrc, cid, src, val string
		if err := attrRows.Scan(&ct, &csrc, &cid, &src, &val); err != nil {
			attrRows.Close()
			s.metrics.RecordSync("full_build", "error")
			return nil, fmt.Errorf("dhtms sync: scan content_attributes: %w", err)
		}
		k := catalogKey{contentType: ct, source: csrc, id: cid}
		altMap[k] = append(altMap[k], [2]string{src, val})
	}
	if err := attrRows.Err(); err != nil {
		attrRows.Close()
		s.metrics.RecordSync("full_build", "error")
		return nil, fmt.Errorf("dhtms sync: iterate content_attributes: %w", err)
	}
	attrRows.Close()

	entityMap := make(map[catalogKey]uint32)
	var catalogLines []BuildCatalogLine
	catRows, err := tx.Query(ctx, `SELECT type, source, id, title, COALESCE(original_title, ''), COALESCE(release_year, 0), COALESCE(adult, false)
FROM content
ORDER BY type, source, id`)
	if err != nil {
		s.metrics.RecordSync("full_build", "error")
		return nil, fmt.Errorf("dhtms sync: query content catalog: %w", err)
	}
	var nextEntityID uint32 = 1
	for catRows.Next() {
		var ct, src, id, title, origTitle string
		var year int32
		var adult bool
		if err := catRows.Scan(&ct, &src, &id, &title, &origTitle, &year, &adult); err != nil {
			catRows.Close()
			s.metrics.RecordSync("full_build", "error")
			return nil, fmt.Errorf("dhtms sync: scan content catalog: %w", err)
		}
		k := catalogKey{contentType: ct, source: src, id: id}
		eid := nextEntityID
		nextEntityID++
		entityMap[k] = eid
		var yr uint16
		if year > 0 && year <= 65535 {
			yr = uint16(year)
		}
		catalogLines = append(catalogLines, BuildCatalogLine{
			Kind:           "catalog",
			EntityID:       eid,
			ContentType:    ContentTypeToCode(ct),
			Source:         src,
			ID:             id,
			Title:          title,
			OriginalTitle:  origTitle,
			Year:           yr,
			Adult:          adult,
			AltIdentifiers: altMap[k],
		})
	}
	if err := catRows.Err(); err != nil {
		catRows.Close()
		s.metrics.RecordSync("full_build", "error")
		return nil, fmt.Errorf("dhtms sync: iterate content catalog: %w", err)
	}
	catRows.Close()

	var hasLegacyFiles bool
	var estTorrents int64
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM torrent_files LIMIT 1), COALESCE((SELECT reltuples::bigint FROM pg_class WHERE relname = 'torrents'), 0)`).
		Scan(&hasLegacyFiles, &estTorrents); err != nil {
		s.metrics.RecordSync("full_build", "error")
		return nil, fmt.Errorf("dhtms sync: inspect torrent_files/estimate: %w", err)
	}
	s.hasLegacyTorrentFiles = hasLegacyFiles
	indexTrigrams := true
	switch strings.ToLower(strings.TrimSpace(s.cfg.IndexTrigrams)) {
	case "true":
		indexTrigrams = true
	case "false":
		indexTrigrams = false
	default: // "auto"
		indexTrigrams = estTorrents < 2_000_000
	}
	estDocs := 0
	if estTorrents > 0 {
		estDocs = int(estTorrents)
	}

	// Build authoritative PostgreSQL visibility + metadata + file stream query.
	docSQL, docArgs := s.buildTorrentExportSQL(false, hasLegacyFiles)
	docRows, err := tx.Query(ctx, docSQL, docArgs...)
	if err != nil {
		s.metrics.RecordSync("full_build", "error")
		return nil, fmt.Errorf("dhtms sync: query torrents snapshot: %w", err)
	}
	defer docRows.Close()

	pr, pw := io.Pipe()
	streamErrCh := make(chan error, 1)
	go func() {
		bw := bufio.NewWriterSize(pw, 256*1024)
		enc := json.NewEncoder(bw)
		enc.SetEscapeHTML(false)

		writeErr := func(e error) {
			_ = pw.CloseWithError(e)
			streamErrCh <- e
		}

		leaseMS := uint64(s.cfg.LeaseTTL / time.Millisecond)
		if leaseMS == 0 {
			leaseMS = uint64(defaultLeaseTTL / time.Millisecond)
		}
		if err := enc.Encode(BuildHeaderLine{
			Kind:          "header",
			PGLSN:         lsnU64,
			LeaseTTLMS:    leaseMS,
			PGLSNText:     lsnText,
			IndexTrigrams: &indexTrigrams,
			UniqueHashes:  true,
			EstimatedDocs: estDocs,
		}); err != nil {
			writeErr(err)
			return
		}
		for i := range catalogLines {
			if err := enc.Encode(&catalogLines[i]); err != nil {
				writeErr(err)
				return
			}
		}

		for docRows.Next() {
			doc, err := scanBuildDocRow(docRows, entityMap)
			if err != nil {
				writeErr(err)
				return
			}
			if err := enc.Encode(&doc); err != nil {
				writeErr(err)
				return
			}
		}
		if err := docRows.Err(); err != nil {
			writeErr(err)
			return
		}
		if err := bw.Flush(); err != nil {
			writeErr(err)
			return
		}
		_ = pw.Close()
		streamErrCh <- nil
	}()

	resp, buildErr := s.client.BuildGeneration(ctx, pr, -1)
	_ = pr.Close()
	streamErr := <-streamErrCh
	if buildErr != nil {
		s.metrics.RecordSync("full_build", "error")
		if streamErr != nil {
			return nil, fmt.Errorf("dhtms sync: build generation: %w (stream: %v)", buildErr, streamErr)
		}
		return nil, fmt.Errorf("dhtms sync: build generation: %w", buildErr)
	}
	if streamErr != nil {
		s.metrics.RecordSync("full_build", "error")
		return nil, fmt.Errorf("dhtms sync: stream snapshot: %w", streamErr)
	}

	_ = tx.Rollback(context.Background())

	s.bootstrapped = true
	if resp.PGLSN > s.lastLSN {
		s.lastLSN = resp.PGLSN
	} else if lsnU64 > s.lastLSN {
		s.lastLSN = lsnU64
	}
	s.lastSeq = snapshotMaxSeq
	s.lastGenerationID = resp.GenerationID
	s.catalogEntity = entityMap
	s.pendingRebuild = false
	s.firstNewHashAt = time.Time{}

	if snapshotMaxSeq > 0 {
		if _, delErr := s.pool.Exec(ctx, `DELETE FROM dhtms_outbox WHERE seq <= $1`, snapshotMaxSeq); delErr != nil {
			s.logger.Warnw("prune applied outbox after full build failed", "seq", snapshotMaxSeq, "error", delErr)
		}
	}

	s.metrics.RecordSync("full_build", "ok")
	s.metrics.SetGeneration(resp.GenerationID)
	s.logger.Infow("dh-tmsd generation built and swapped",
		"generation_id", resp.GenerationID,
		"num_docs", resp.NumDocs,
		"pg_lsn", lsnText,
	)
	return resp, nil
}

// buildTorrentExportSQL constructs the authoritative PostgreSQL export query
// using serving.Policy's canonical SQL predicates verbatim.
func (s *Syncer) buildTorrentExportSQL(filterByHashes bool, includeLegacyTorrentFiles bool) (string, []any) {
	var args []any
	nextParam := 1

	baseSQL := "TRUE"
	if s.policy != nil {
		baseExpr := s.policy.BaseCondition("torrents")
		converted, convertedArgs, nxt := GormExprToPG(baseExpr.SQL, baseExpr.Vars, nextParam)
		baseSQL = converted
		args = append(args, convertedArgs...)
		nextParam = nxt
	}

	pubSQL := "TRUE"
	if s.policy != nil {
		if pubExpr, ok := s.policy.PublicationGateCondition("torrents"); ok {
			converted, convertedArgs, nxt := GormExprToPG(pubExpr.SQL, pubExpr.Vars, nextParam)
			pubSQL = converted
			args = append(args, convertedArgs...)
			nextParam = nxt
		}
	}

	payloadSQL := "0::bigint"
	if s.policy != nil {
		if payloadExpr, ok := s.policy.PayloadExpirySQL("torrents"); ok {
			converted, convertedArgs, nxt := GormExprToPG(payloadExpr.SQL, payloadExpr.Vars, nextParam)
			payloadSQL = converted
			args = append(args, convertedArgs...)
			nextParam = nxt
		}
	}

	whereClause := ""
	if filterByHashes {
		whereClause = fmt.Sprintf("WHERE torrents.info_hash = ANY($%d::bytea[]) OR torrents.info_hash_v2 = ANY($%d::bytea[])", nextParam, nextParam)
	}

	filePathsExpr := `COALESCE(fb.paths, '{}'::text[])`
	fileSizesExpr := `COALESCE(fb.sizes, '{}'::bigint[])`
	fileExtsExpr := `COALESCE(fb.extensions, '{}'::text[])`
	if includeLegacyTorrentFiles {
		filePathsExpr = `COALESCE(fb.paths, ARRAY(SELECT f.path FROM torrent_files f WHERE f.info_hash = torrents.info_hash ORDER BY f."index"))`
		fileSizesExpr = `COALESCE(fb.sizes, ARRAY(SELECT f.size FROM torrent_files f WHERE f.info_hash = torrents.info_hash ORDER BY f."index"))`
		fileExtsExpr = `COALESCE(fb.extensions, ARRAY(SELECT COALESCE(f.extension, '') FROM torrent_files f WHERE f.info_hash = torrents.info_hash ORDER BY f."index"))`
	}

	querySQL := fmt.Sprintf(`SELECT
	torrents.info_hash,
	torrents.info_hash_v2,
	torrents.name,
	torrents.size,
	torrents.private,
	COALESCE(EXTRACT(EPOCH FROM torrents.created_at)::bigint, 0) AS created_epoch,
	COALESCE(tc.content_type, '') AS content_type,
	COALESCE(tc.content_source, '') AS content_source,
	COALESCE(tc.content_id, '') AS content_id,
	COALESCE(tc.video_resolution, '') AS video_resolution,
	COALESCE(EXTRACT(EPOCH FROM COALESCE(tc.published_at, torrents.created_at))::bigint, 0) AS published_epoch,
	COALESCE(tc.seeders, -1) AS seeders,
	COALESCE(tc.leechers, -1) AS leechers,
	COALESCE(c.release_year, 0) AS release_year,
	%s AS file_paths,
	%s AS file_sizes,
	%s AS file_exts,
	(%s) AS base_ok,
	(%s) AS pub_ok,
	(%s) AS payload_exp
FROM torrents
LEFT JOIN LATERAL (
	SELECT tc0.content_type, tc0.content_source, tc0.content_id, tc0.video_resolution,
	       tc0.published_at, tc0.seeders, tc0.leechers
	FROM torrent_contents tc0
	WHERE tc0.info_hash = torrents.info_hash
	ORDER BY (tc0.content_id IS NOT NULL) DESC, tc0.updated_at DESC
	LIMIT 1
) tc ON TRUE
LEFT JOIN content c
	ON c.type = tc.content_type AND c.source = tc.content_source AND c.id = tc.content_id
LEFT JOIN torrent_file_bundles fb
	ON fb.info_hash = torrents.info_hash
%s`, filePathsExpr, fileSizesExpr, fileExtsExpr, baseSQL, pubSQL, payloadSQL, whereClause)

	return querySQL, args
}

type exportedTorrentRow struct {
	infoHash   []byte
	infoHashV2 []byte
	doc        BuildDocLine
	baseOK     bool
	pubOK      bool
	payloadExp uint32
	isPrivate  bool
	filePaths  []string
	seeders    int32
	leechers   int32
	checkedMin uint32
}

func scanExportedRow(rows pgx.Rows, entityMap map[catalogKey]uint32) (exportedTorrentRow, error) {
	var (
		ih             []byte
		ihv2           []byte
		name           string
		sizeBytes      int64
		isPrivate      bool
		createdEpoch   int64
		contentType    string
		contentSource  string
		contentID      string
		videoRes       string
		publishedEpoch int64
		seeders        int32
		leechers       int32
		releaseYear    int32
		filePaths      []string
		fileSizes      []int64
		fileExts       []string
		baseOK         bool
		pubOK          bool
		payloadExp     int64
	)
	if err := rows.Scan(
		&ih, &ihv2, &name, &sizeBytes, &isPrivate, &createdEpoch,
		&contentType, &contentSource, &contentID, &videoRes,
		&publishedEpoch, &seeders, &leechers, &releaseYear,
		&filePaths, &fileSizes, &fileExts,
		&baseOK, &pubOK, &payloadExp,
	); err != nil {
		return exportedTorrentRow{}, err
	}

	var v1Hex, v2Hex string
	if len(ih) == 20 {
		v1Hex = hex.EncodeToString(ih)
	} else if len(ih) == 32 {
		v2Hex = hex.EncodeToString(ih)
	}
	if len(ihv2) == 32 {
		v2Hex = hex.EncodeToString(ihv2)
	}

	var eid uint32
	if contentType != "" && contentSource != "" && contentID != "" {
		eid = entityMap[catalogKey{contentType: contentType, source: contentSource, id: contentID}]
	}

	var yr uint16
	if releaseYear > 0 && releaseYear <= 65535 {
		yr = uint16(releaseYear)
	}

	files := make([]BuildFileInput, 0, len(filePaths))
	for i, fp := range filePaths {
		var fsz uint64
		if i < len(fileSizes) && fileSizes[i] > 0 {
			fsz = uint64(fileSizes[i])
		}
		var fext string
		if i < len(fileExts) {
			fext = fileExts[i]
		}
		files = append(files, BuildFileInput{
			Path:      fp,
			Size:      fsz,
			Extension: fext,
		})
	}

	var sz uint64
	if sizeBytes > 0 {
		sz = uint64(sizeBytes)
	}
	var pubEp, crEp, payEp uint32
	if publishedEpoch > 0 {
		pubEp = uint32(publishedEpoch)
	}
	if createdEpoch > 0 {
		crEp = uint32(createdEpoch)
	}
	if payloadExp > 0 {
		payEp = uint32(payloadExp)
	}

	doc := BuildDocLine{
		Kind:                "doc",
		InfoHashV1:          v1Hex,
		InfoHashV2:          v2Hex,
		Name:                name,
		SizeBytes:           sz,
		ContentType:         ContentTypeToCode(contentType),
		EntityID:            eid,
		Year:                yr,
		PublishedEpoch:      pubEp,
		CreatedEpoch:        crEp,
		Resolution:          ResolutionToCode(videoRes),
		Files:               files,
		IsPrivate:           isPrivate,
		Excluded:            !baseOK && !isPrivate,
		AuthoritativePolicy: true,
		PubApproved:         baseOK && pubOK,
		PayloadExpiresEpoch: payEp,
		Seeders:             seeders,
		Leechers:            leechers,
		CheckedEpochMin:     uint32(time.Now().Unix() / 60),
	}

	return exportedTorrentRow{
		infoHash:   ih,
		infoHashV2: ihv2,
		doc:        doc,
		baseOK:     baseOK,
		pubOK:      pubOK,
		payloadExp: payEp,
		isPrivate:  isPrivate,
		filePaths:  filePaths,
		seeders:    seeders,
		leechers:   leechers,
		checkedMin: doc.CheckedEpochMin,
	}, nil
}

func scanBuildDocRow(rows pgx.Rows, entityMap map[catalogKey]uint32) (BuildDocLine, error) {
	rec, err := scanExportedRow(rows, entityMap)
	if err != nil {
		return BuildDocLine{}, err
	}
	return rec.doc, nil
}

type outboxChangeFlags struct {
	rawHash []byte
	deleted bool
	upsert  bool
	policy  bool
	meta    bool
	swarm   bool
}

// SyncOnce executes one bounded sync cycle:
//  1. Checks dh-tmsd health; triggers FullBuild automatically on empty volume (gen 0) or first boot.
//  2. Drains pending dhtms_outbox rows and pushes immediate fail-closed policy revocations,
//     L0 metadata overlays, and swarm updates to dh-tmsd.
//  3. Triggers a debounced zero-downtime FullBuild when newly discovered hashes appear.
//  4. Renews dh-tmsd's monotonic PG-LSN governance lease.
func (s *Syncer) SyncOnce(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	health, err := s.client.Health(ctx)
	if err != nil {
		s.metrics.RecordSync("incremental", "error")
		return fmt.Errorf("dhtms sync: health check: %w", err)
	}
	if !s.bootstrapped || health.GenerationID == 0 {
		_, err := s.fullBuildLocked(ctx)
		return err
	}
	s.metrics.SetGeneration(health.GenerationID)

	var lsnText string
	if err := s.pool.QueryRow(ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&lsnText); err != nil {
		s.metrics.RecordSync("incremental", "error")
		return fmt.Errorf("dhtms sync: read pg_current_wal_lsn: %w", err)
	}
	nextLSN, err := ParsePGLSN(lsnText)
	if err != nil {
		nextLSN = s.lastLSN + 1
	}
	if nextLSN <= health.PGLSN {
		nextLSN = health.PGLSN + 1
	}
	if nextLSN <= s.lastLSN {
		nextLSN = s.lastLSN + 1
	}

	rows, err := s.pool.Query(ctx, `SELECT seq, op, info_hash
FROM dhtms_outbox
WHERE seq > $1
ORDER BY seq
LIMIT $2`, s.lastSeq, outboxBatchLimit)
	if err != nil {
		s.metrics.RecordSync("incremental", "error")
		return fmt.Errorf("dhtms sync: query dhtms_outbox: %w", err)
	}

	changes := make(map[string]*outboxChangeFlags)
	var maxSeq int64
	rowCount := 0
	for rows.Next() {
		var seq int64
		var op string
		var ih []byte
		if err := rows.Scan(&seq, &op, &ih); err != nil {
			rows.Close()
			s.metrics.RecordSync("incremental", "error")
			return fmt.Errorf("dhtms sync: scan dhtms_outbox: %w", err)
		}
		rowCount++
		if seq > maxSeq {
			maxSeq = seq
		}
		hexHash := hex.EncodeToString(ih)
		f := changes[hexHash]
		if f == nil {
			f = &outboxChangeFlags{rawHash: append([]byte(nil), ih...)}
			changes[hexHash] = f
		}
		switch op {
		case "delete":
			f.deleted = true
		case "upsert":
			f.upsert = true
			f.policy = true
			f.meta = true
		case "policy":
			f.policy = true
		case "meta":
			f.meta = true
			f.policy = true // name or content_type change can affect adult/namepolicy visibility
		case "swarm":
			f.swarm = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		s.metrics.RecordSync("incremental", "error")
		return fmt.Errorf("dhtms sync: iterate dhtms_outbox: %w", err)
	}
	rows.Close()

	if rowCount > 0 {
		if err := s.applyOutboxBatchLocked(ctx, changes, nextLSN); err != nil {
			s.metrics.RecordSync("incremental", "error")
			return err
		}
		s.lastSeq = maxSeq
		s.lastLSN = nextLSN
		if _, delErr := s.pool.Exec(ctx, `DELETE FROM dhtms_outbox WHERE seq <= $1`, maxSeq); delErr != nil {
			s.logger.Warnw("delete processed dhtms_outbox rows failed", "seq", maxSeq, "error", delErr)
		}
		s.metrics.AddOutboxApplied(rowCount)
		s.metrics.RecordSync("incremental", "ok")
		nextLSN++
	}

	if s.pendingRebuild && time.Since(s.firstNewHashAt) >= s.cfg.RebuildDebounce {
		if _, err := s.fullBuildLocked(ctx); err != nil {
			return err
		}
		return nil
	}

	leaseMS := uint64(s.cfg.LeaseTTL / time.Millisecond)
	if leaseMS == 0 {
		leaseMS = uint64(defaultLeaseTTL / time.Millisecond)
	}
	leaseResp, err := s.client.PolicyLease(ctx, PolicyLeaseRequest{
		PGLSN: nextLSN,
		TTLMS: leaseMS,
	})
	if err != nil {
		s.metrics.RecordSync("lease", "error")
		return fmt.Errorf("dhtms sync: renew governance lease: %w", err)
	}
	if leaseResp.PGLSN > s.lastLSN {
		s.lastLSN = leaseResp.PGLSN
	} else {
		s.lastLSN = nextLSN
	}
	s.metrics.RecordSync("lease", "ok")
	return nil
}

func (s *Syncer) applyOutboxBatchLocked(
	ctx context.Context,
	changes map[string]*outboxChangeFlags,
	pgLSN uint64,
) error {
	hashSlice := make([][]byte, 0, len(changes))
	for _, f := range changes {
		hashSlice = append(hashSlice, f.rawHash)
	}

	querySQL, baseArgs := s.buildTorrentExportSQL(true, s.hasLegacyTorrentFiles)
	queryArgs := append(baseArgs, hashSlice)
	rows, err := s.pool.Query(ctx, querySQL, queryArgs...)
	if err != nil {
		return fmt.Errorf("dhtms sync: query changed torrents: %w", err)
	}
	present := make(map[string]exportedTorrentRow, len(changes))
	for rows.Next() {
		rec, err := scanExportedRow(rows, s.catalogEntity)
		if err != nil {
			rows.Close()
			return fmt.Errorf("dhtms sync: scan changed torrent: %w", err)
		}
		present[hex.EncodeToString(rec.infoHash)] = rec
		if len(rec.infoHashV2) == 32 {
			present[hex.EncodeToString(rec.infoHashV2)] = rec
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("dhtms sync: iterate changed torrents: %w", err)
	}
	rows.Close()

	var (
		policyOps  []PolicyBatchOp
		l0Items    []L0UpsertItem
		newL0Items []L0UpsertItem
		newRecords []exportedTorrentRow
		swarmItems []SwarmUpdateItem
		seenDocID  = make(map[uint32]struct{}, len(changes))
		seenNew    = make(map[string]struct{})
	)
	markRebuild := func() {
		if !s.pendingRebuild {
			s.pendingRebuild = true
			s.firstNewHashAt = time.Now()
		}
	}

	// Ensure dh-tmsd lease is active during hash lookups if it momentarily lapsed.
	for hexHash, f := range changes {
		hl, err := s.client.HashLookup(ctx, HashLookupRequest{
			InfoHash: hexHash,
			Tier:     TierBase,
		})
		if errors.Is(err, ErrLeaseExpired) {
			leaseMS := uint64(s.cfg.LeaseTTL / time.Millisecond)
			if leaseMS == 0 {
				leaseMS = uint64(defaultLeaseTTL / time.Millisecond)
			}
			if _, lerr := s.client.PolicyLease(ctx, PolicyLeaseRequest{PGLSN: pgLSN, TTLMS: leaseMS}); lerr != nil {
				return fmt.Errorf("dhtms sync: renew lease before outbox lookup: %w", lerr)
			}
			hl, err = s.client.HashLookup(ctx, HashLookupRequest{
				InfoHash: hexHash,
				Tier:     TierBase,
			})
		}
		if err != nil {
			return fmt.Errorf("dhtms sync: hash lookup %s: %w", hexHash, err)
		}

		rec, existsInPG := present[hexHash]
		if !existsInPG {
			if hl.Found && hl.DocID != nil {
				docID := *hl.DocID
				if _, dup := seenDocID[docID]; !dup {
					seenDocID[docID] = struct{}{}
					policyOps = append(policyOps, PolicyBatchOp{
						Op:     "revoke",
						DocID:  &docID,
						Reason: ReasonCsamDelete,
					})
				}
			}
			continue
		}

		if !hl.Found || hl.DocID == nil {
			// Brand-new torrent not yet in active generation -> ingest directly into live L0
			// (avoids full-catalogue rebuild loops during continuous DHT crawling!).
			canonicalKey := hex.EncodeToString(rec.infoHash)
			if _, dup := seenNew[canonicalKey]; dup {
				continue
			}
			seenNew[canonicalKey] = struct{}{}
			newL0Items = append(newL0Items, L0UpsertItem{
				InfoHashV1:     rec.doc.InfoHashV1,
				InfoHashV2:     rec.doc.InfoHashV2,
				Name:           rec.doc.Name,
				SizeBytes:      rec.doc.SizeBytes,
				ContentType:    rec.doc.ContentType,
				Resolution:     rec.doc.Resolution,
				EntityID:       rec.doc.EntityID,
				Year:           rec.doc.Year,
				PublishedEpoch: rec.doc.PublishedEpoch,
				CreatedEpoch:   rec.doc.CreatedEpoch,
				FilesCount:     uint32(len(rec.doc.Files)),
				FilePaths:      rec.filePaths,
				Files:          rec.doc.Files,
			})
			newRecords = append(newRecords, rec)
			continue
		}

		docID := *hl.DocID
		if _, dup := seenDocID[docID]; dup {
			continue
		}
		seenDocID[docID] = struct{}{}

		if f.policy {
			if !rec.baseOK {
				reason := ReasonExcluded
				if rec.isPrivate {
					reason = ReasonPrivacy
				}
				policyOps = append(policyOps, PolicyBatchOp{
					Op:     "revoke",
					DocID:  &docID,
					Reason: reason,
				})
			} else {
				policyOps = append(policyOps,
					PolicyBatchOp{Op: "grant_base", DocID: &docID},
					PolicyBatchOp{Op: "unrevoke", DocID: &docID, Reason: ReasonExcluded},
					PolicyBatchOp{Op: "unrevoke", DocID: &docID, Reason: ReasonPrivacy},
				)
				if rec.pubOK {
					policyOps = append(policyOps, PolicyBatchOp{Op: "grant_pub", DocID: &docID})
				} else {
					policyOps = append(policyOps, PolicyBatchOp{Op: "revoke_pub", DocID: &docID})
				}
				if rec.payloadExp > 0 {
					policyOps = append(policyOps, PolicyBatchOp{
						Op:           "grant_payload",
						DocID:        &docID,
						ExpiresEpoch: rec.payloadExp,
					})
				} else {
					policyOps = append(policyOps, PolicyBatchOp{Op: "revoke_payload", DocID: &docID})
				}
			}
		}

		if f.meta {
			idCopy := docID
			l0Items = append(l0Items, L0UpsertItem{
				DocID:          &idCopy,
				InfoHashV1:     rec.doc.InfoHashV1,
				InfoHashV2:     rec.doc.InfoHashV2,
				Name:           rec.doc.Name,
				SizeBytes:      rec.doc.SizeBytes,
				ContentType:    rec.doc.ContentType,
				Resolution:     rec.doc.Resolution,
				EntityID:       rec.doc.EntityID,
				Year:           rec.doc.Year,
				PublishedEpoch: rec.doc.PublishedEpoch,
				CreatedEpoch:   rec.doc.CreatedEpoch,
				FilesCount:     uint32(len(rec.doc.Files)),
				FilePaths:      rec.filePaths,
				Files:          rec.doc.Files,
			})
		}

		if f.swarm || f.meta {
			var sPtr, lPtr *uint32
			if rec.seeders >= 0 {
				v := uint32(rec.seeders)
				sPtr = &v
			}
			if rec.leechers >= 0 {
				v := uint32(rec.leechers)
				lPtr = &v
			}
			if sPtr != nil || lPtr != nil {
				swarmItems = append(swarmItems, SwarmUpdateItem{
					DocID:           &docID,
					Seeders:         sPtr,
					Leechers:        lPtr,
					CheckedEpochMin: rec.checkedMin,
				})
			}
		}
	}

	// 1. Insert brand-new torrents into live L0 first so we obtain their assigned dynamic doc_ids
	// before applying policy grants/revocations and swarm counts in the same cycle.
	if len(newL0Items) > 0 {
		newResp, err := s.client.L0Upsert(ctx, L0UpsertRequest{
			Docs: newL0Items,
			Sync: true,
		})
		if err != nil {
			// If dynamic L0 capacity is exhausted, schedule compaction rebuild cleanly.
			if strings.Contains(err.Error(), "capacity exhausted") {
				markRebuild()
			} else {
				return fmt.Errorf("dhtms sync: apply new l0 insert (%d docs): %w", len(newL0Items), err)
			}
		} else {
			if s.cfg.CompactDocs > 0 && newResp.L0Count >= s.cfg.CompactDocs {
				markRebuild()
			}
			for i, assignedID := range newResp.DocIDs {
				if i >= len(newRecords) {
					break
				}
				rec := newRecords[i]
				idCopy := assignedID
				if !rec.baseOK {
					reason := ReasonExcluded
					if rec.isPrivate {
						reason = ReasonPrivacy
					}
					policyOps = append(policyOps, PolicyBatchOp{
						Op:     "revoke",
						DocID:  &idCopy,
						Reason: reason,
					})
				} else {
					policyOps = append(policyOps,
						PolicyBatchOp{Op: "grant_base", DocID: &idCopy},
						PolicyBatchOp{Op: "unrevoke", DocID: &idCopy, Reason: ReasonExcluded},
						PolicyBatchOp{Op: "unrevoke", DocID: &idCopy, Reason: ReasonPrivacy},
					)
					if rec.pubOK {
						policyOps = append(policyOps, PolicyBatchOp{Op: "grant_pub", DocID: &idCopy})
					} else {
						policyOps = append(policyOps, PolicyBatchOp{Op: "revoke_pub", DocID: &idCopy})
					}
					if rec.payloadExp > 0 {
						policyOps = append(policyOps, PolicyBatchOp{
							Op:           "grant_payload",
							DocID:        &idCopy,
							ExpiresEpoch: rec.payloadExp,
						})
					} else {
						policyOps = append(policyOps, PolicyBatchOp{Op: "revoke_payload", DocID: &idCopy})
					}
				}
				var sPtr, lPtr *uint32
				if rec.seeders >= 0 {
					v := uint32(rec.seeders)
					sPtr = &v
				}
				if rec.leechers >= 0 {
					v := uint32(rec.leechers)
					lPtr = &v
				}
				if sPtr != nil || lPtr != nil {
					swarmItems = append(swarmItems, SwarmUpdateItem{
						DocID:           &idCopy,
						Seeders:         sPtr,
						Leechers:        lPtr,
						CheckedEpochMin: rec.checkedMin,
					})
				}
			}
		}
	}

	if len(policyOps) > 0 {
		if _, err := s.client.PolicyBatch(ctx, PolicyBatchRequest{
			Ops:             policyOps,
			PGLSN:           pgLSN,
			Sync:            true,
			RebuildSnapshot: true,
		}); err != nil {
			return fmt.Errorf("dhtms sync: apply policy batch (%d ops): %w", len(policyOps), err)
		}
	}
	if len(l0Items) > 0 {
		l0Resp, err := s.client.L0Upsert(ctx, L0UpsertRequest{
			Docs: l0Items,
			Sync: true,
		})
		if err != nil {
			return fmt.Errorf("dhtms sync: apply l0 upsert (%d docs): %w", len(l0Items), err)
		}
		if s.cfg.CompactDocs > 0 && l0Resp.L0Count >= s.cfg.CompactDocs {
			markRebuild()
		}
	}
	if len(swarmItems) > 0 {
		if _, err := s.client.SwarmUpdate(ctx, SwarmUpdateRequest{
			Updates: swarmItems,
			Sync:    false,
		}); err != nil {
			return fmt.Errorf("dhtms sync: apply swarm update (%d items): %w", len(swarmItems), err)
		}
	}

	return nil
}

// SyncWorkerParams injects dependencies for the background dhtms_sync fx worker.
type SyncWorkerParams struct {
	fx.In
	Config  Config
	Client  *Client `optional:"true"`
	Pool    lazy.Lazy[*pgxpool.Pool]
	Policy  *serving.Policy
	Metrics *Metrics
	Logger  *zap.SugaredLogger
}

// SyncWorkerResult exports the background dhtms_sync worker to the worker registry.
type SyncWorkerResult struct {
	fx.Out
	Worker worker.Worker `group:"workers"`
}

// NewSyncWorker registers the dhtms_sync worker. When DH_TMS_ENDPOINT is unset or
// read mode is off, start() is a zero-overhead no-op.
func NewSyncWorker(p SyncWorkerParams) SyncWorkerResult {
	var (
		cancel context.CancelFunc
		wg     sync.WaitGroup
	)
	log := p.Logger.Named(syncWorkerKey)
	return SyncWorkerResult{
		Worker: worker.NewWorker(syncWorkerKey, fx.Hook{
			OnStart: func(context.Context) error {
				if p.Client == nil || !p.Config.Enabled() || strings.TrimSpace(p.Config.Endpoint) == "" {
					return nil
				}
				ctx, c := context.WithCancel(context.Background())
				cancel = c
				wg.Add(1)
				go func() {
					defer wg.Done()
					pool, err := p.Pool.Get()
					if err != nil {
						log.Warnw("acquire postgres pool for dhtms sync failed", "error", err)
						return
					}
					syncer := NewSyncer(pool, p.Client, p.Policy, p.Config, p.Metrics, p.Logger)
					ticker := time.NewTicker(p.Config.SyncInterval)
					defer ticker.Stop()

					if err := syncer.SyncOnce(ctx); err != nil && ctx.Err() == nil {
						log.Warnw("initial dh-tmsd sync cycle failed (will retry)", "error", err)
					}
					for {
						select {
						case <-ctx.Done():
							return
						case <-ticker.C:
							if err := syncer.SyncOnce(ctx); err != nil && ctx.Err() == nil {
								log.Warnw("dh-tmsd sync cycle failed", "error", err)
							}
						}
					}
				}()
				return nil
			},
			OnStop: func(context.Context) error {
				if cancel != nil {
					cancel()
				}
				wg.Wait()
				if p.Client != nil {
					p.Client.Close()
				}
				return nil
			},
		}),
	}
}
