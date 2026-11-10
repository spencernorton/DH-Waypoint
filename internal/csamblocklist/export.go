package csamblocklist

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"go.uber.org/zap"
)

// Exporter is the self-export sink for banned-keyword matches. It
// double-hashes the infohash and appends it to the local JSONL log, and
// optionally POSTs the same record to a configured upstream collection
// endpoint so other operators see the observation.
//
// All public methods are safe for concurrent use.
type Exporter interface {
	// Record is called by the processor when the classifier deletes a
	// torrent. It exports only when MatchesBannedKeyword(t) holds, so other
	// delete reasons (`delete_xxx`, `delete_content_types`) do not taint
	// the export.
	//
	// Errors are logged + counted but never propagated. The processor's
	// path through this hook is non-fatal.
	Record(ctx context.Context, t model.Torrent)

	// Close flushes any pending state. Safe to call multiple times.
	Close() error
}

// NewExporter constructs an Exporter per the operator's config. When
// ExportEnabled=false, returns a NoOp exporter that silently drops
// every Record call.
//
// If ExportUpstreamURL is set + ExportUpstreamAuthHeader is set + the
// URL is not https://, the constructor scrubs the upstream URL to
// avoid sending the Authorization token in cleartext (review finding
// finding 64bf02b7). Loopback URLs (localhost / 127.0.0.1 / ::1)
// are exempt to support local-stack testing. The scrub logs a
// warning so the operator notices.
func NewExporter(cfg Config, logger *zap.SugaredLogger, metrics *Metrics) Exporter {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	if !cfg.ExportEnabled {
		return noOpExporter{}
	}
	if cfg.ExportUpstreamURL != "" && cfg.ExportUpstreamAuthHeader != "" {
		if !isSafeUpstreamURL(cfg.ExportUpstreamURL) {
			logger.Warnw(
				"refusing to attach Authorization header to non-https / non-loopback ExportUpstreamURL — disabling upstream POST",
				"url", cfg.ExportUpstreamURL,
			)
			cfg.ExportUpstreamURL = ""
		}
	}
	httpClient := &http.Client{Timeout: cfg.ExportUpstreamTimeout}
	if cfg.ExportUpstreamTimeout <= 0 {
		httpClient.Timeout = 10 * time.Second
	}
	return &exporter{
		cfg:        cfg,
		logger:     logger,
		metrics:    metrics,
		httpClient: httpClient,
	}
}

// isSafeUpstreamURL returns true iff the URL uses HTTPS, OR uses HTTP
// against loopback (localhost / 127.0.0.1 / ::1) — the latter exempt
// for local-stack testing where TLS would require self-signed certs.
func isSafeUpstreamURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme == "http" {
		host := u.Hostname()
		switch host {
		case "localhost", "127.0.0.1", "::1":
			return true
		}
	}
	return false
}

type exporter struct {
	cfg     Config
	logger  *zap.SugaredLogger
	metrics *Metrics

	mu   sync.Mutex
	file *os.File
	// closed uses atomic.Bool so the Record fast-path can read it
	// without acquiring the mutex (Jeeves finding c0885ac0).
	// Close still acquires the mutex to coordinate with appendLocal.
	closed atomic.Bool

	httpClient *http.Client
}

// observationRecord is the JSONL line and POST body shape. The
// infohash is NEVER included — only its double-hash. Keeps the
// observation log itself non-useful as a CSAM directory if it leaks.
type observationRecord struct {
	Timestamp  string `json:"ts"`
	DoubleHash string `json:"double_hash"`
	Reason     string `json:"reason"`
	// Note: title and file paths are also intentionally omitted.
	// The post-classify pipeline already logs a redacted summary
	// elsewhere; this record is purely the queryable fingerprint.
}

func (e *exporter) Record(ctx context.Context, t model.Torrent) {
	if e.closed.Load() {
		return
	}
	if !MatchesBannedKeyword(t) {
		if e.metrics != nil {
			e.metrics.ExportTotal.WithLabelValues("skipped_no_match").Inc()
		}
		return
	}
	e.export(ctx, t.InfoHash)
}

// export writes one observation without testing anything. Callers have
// already matched the torrent against the built-in banned-keyword list.
func (e *exporter) export(ctx context.Context, infoHash protocol.InfoHash) {
	if e.closed.Load() {
		return
	}

	rec := observationRecord{
		Timestamp:  time.Now().UTC().Format(time.RFC3339Nano),
		DoubleHash: DoubleHashHex(infoHash),
		Reason:     "banned_keyword",
	}

	// Local JSONL append.
	if err := e.appendLocal(rec); err != nil {
		if e.metrics != nil {
			e.metrics.ExportTotal.WithLabelValues("local_err").Inc()
		}
		e.logger.Warnw("csam export local-write failed", "err", err)
	} else {
		if e.metrics != nil {
			e.metrics.ExportTotal.WithLabelValues("local_ok").Inc()
		}
	}

	// Optional upstream POST.
	if e.cfg.ExportUpstreamURL != "" {
		if err := e.postUpstream(ctx, rec); err != nil {
			if e.metrics != nil {
				e.metrics.ExportTotal.WithLabelValues("upstream_err").Inc()
			}
			e.logger.Warnw("csam export upstream POST failed",
				"url", e.cfg.ExportUpstreamURL, "err", err)
		} else {
			if e.metrics != nil {
				e.metrics.ExportTotal.WithLabelValues("upstream_ok").Inc()
			}
		}
	}
}

func (e *exporter) appendLocal(rec observationRecord) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed.Load() {
		return errors.New("csam exporter closed")
	}
	if e.file == nil {
		path := e.cfg.ExportFilePath
		if path == "" {
			path = "data/csam-double-hashes.jsonl"
		}
		dir := filepath.Dir(path)
		if dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", dir, err)
			}
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		e.file = f
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	line = append(line, '\n')
	if _, err := e.file.Write(line); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

func (e *exporter) postUpstream(ctx context.Context, rec observationRecord) error {
	body, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.cfg.ExportUpstreamURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "dh-waypoint-csam-export/1")
	if e.cfg.ExportUpstreamAuthHeader != "" {
		req.Header.Set("Authorization", e.cfg.ExportUpstreamAuthHeader)
	}
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("upstream status %d", resp.StatusCode)
	}
	return nil
}

func (e *exporter) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed.Load() || e.file == nil {
		e.closed.Store(true)
		return nil
	}
	err := e.file.Close()
	e.file = nil
	e.closed.Store(true)
	return err
}

type noOpExporter struct{}

func (noOpExporter) Record(context.Context, model.Torrent)     {}
func (noOpExporter) export(context.Context, protocol.InfoHash) {}
func (noOpExporter) Close() error                              { return nil }
