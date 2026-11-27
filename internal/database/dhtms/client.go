package dhtms

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

var (
	// ErrLeaseExpired is returned when dh-tmsd fails closed because its PG governance lease expired.
	ErrLeaseExpired = errors.New("dhtms: governance lease expired")
	// ErrUnauthorized is returned when dh-tmsd rejects a missing or invalid X-DHTMS-Token (HTTP 401).
	ErrUnauthorized = errors.New("dhtms: unauthorized")
	// ErrBadRequest is returned when dh-tmsd rejects a malformed or out-of-range request (HTTP 400).
	ErrBadRequest = errors.New("dhtms: bad request")
	// ErrNotFound is returned for unknown sidecar endpoints (HTTP 404).
	ErrNotFound = errors.New("dhtms: endpoint not found")
	// ErrUnavailable is returned when the dh-tmsd transport cannot be reached.
	ErrUnavailable = errors.New("dhtms: sidecar unavailable")
)

const (
	maxResponseBytes = 32 * 1024 * 1024
	minWriteTimeout  = 5 * time.Second
)

// Client speaks HTTP/1.1 JSON over a local Unix domain socket or loopback HTTP endpoint to dh-tmsd.
type Client struct {
	cfg              Config
	httpClient       *http.Client
	writeClient      *http.Client
	streamClient     *http.Client
	transport        *http.Transport
	writeTransport   *http.Transport
	streamTransport  *http.Transport
	baseURL          string
	authToken        string
	awaitPolicyBuild atomic.Bool
}

// NewClient constructs a Client from Config.
// Even when cfg.Mode == ReadModeOff, callers may construct a Client directly for tests
// by supplying a non-empty cfg.Endpoint (unix://, http://, or https://).
func NewClient(cfg Config) (*Client, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	ep := strings.TrimSpace(cfg.Endpoint)
	if ep == "" {
		return nil, fmt.Errorf("dhtms: endpoint is required")
	}

	dialer := &net.Dialer{
		Timeout:   cfg.Timeout,
		KeepAlive: 30 * time.Second,
	}
	tr := &http.Transport{
		DisableKeepAlives:     true, // dh-tmsd responds with Connection: close
		ResponseHeaderTimeout: cfg.Timeout,
	}

	var baseURL string
	if sockPath, ok := strings.CutPrefix(ep, "unix://"); ok {
		if sockPath == "" {
			return nil, fmt.Errorf("dhtms: unix:// endpoint requires a socket path")
		}
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", sockPath)
		}
		baseURL = "http://dh-tmsd.local"
	} else if strings.HasPrefix(ep, "http://") || strings.HasPrefix(ep, "https://") {
		tr.DialContext = dialer.DialContext
		baseURL = strings.TrimRight(ep, "/")
	} else {
		return nil, fmt.Errorf("dhtms: unsupported endpoint scheme in %q", ep)
	}

	writeTimeout := cfg.Timeout
	if writeTimeout < minWriteTimeout {
		writeTimeout = minWriteTimeout
	}
	writeTr := &http.Transport{
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: writeTimeout,
		DialContext:           tr.DialContext,
	}
	streamTr := &http.Transport{
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: 0,
		DialContext:           tr.DialContext,
	}
	return &Client{
		cfg: cfg,
		httpClient: &http.Client{
			Transport: tr,
			Timeout:   cfg.Timeout,
		},
		writeClient: &http.Client{
			Transport: writeTr,
			Timeout:   writeTimeout,
		},
		streamClient: &http.Client{
			Transport: streamTr,
		},
		transport:       tr,
		writeTransport:  writeTr,
		streamTransport: streamTr,
		baseURL:         baseURL,
		authToken:       strings.TrimSpace(cfg.AuthToken),
	}, nil
}

// Close releases idle transport connections.
func (c *Client) Close() {
	if c == nil {
		return
	}
	if c.transport != nil {
		c.transport.CloseIdleConnections()
	}
	if c.writeTransport != nil {
		c.writeTransport.CloseIdleConnections()
	}
	if c.streamTransport != nil {
		c.streamTransport.CloseIdleConnections()
	}
}

// Config returns the client's runtime configuration.
func (c *Client) Config() Config {
	if c == nil {
		return DefaultConfig()
	}
	return c.cfg
}

// AwaitPolicyBuild makes consumer reads fall back to PostgreSQL until the syncer
// has replaced a retained generation with the currently configured policy.
func (c *Client) AwaitPolicyBuild() { c.awaitPolicyBuild.Store(true) }

func (c *Client) consumerJSON(ctx context.Context, path string, request, result any) error {
	if c.awaitPolicyBuild.Load() {
		return ErrLeaseExpired
	}
	return c.doJSON(ctx, http.MethodPost, path, request, result)
}

func (c *Client) doJSON(ctx context.Context, method, path string, reqBody any, respBody any) error {
	return c.doJSONWith(ctx, c.httpClient, method, path, reqBody, respBody)
}

func (c *Client) doWriteJSON(ctx context.Context, method, path string, reqBody any, respBody any) error {
	cli := c.writeClient
	if cli == nil {
		cli = c.httpClient
	}
	return c.doJSONWith(ctx, cli, method, path, reqBody, respBody)
}

func (c *Client) doJSONWith(ctx context.Context, cli *http.Client, method, path string, reqBody any, respBody any) error {
	var bodyReader io.Reader
	if reqBody != nil {
		raw, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("dhtms: marshal %s: %w", path, err)
		}
		bodyReader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("dhtms: build request %s: %w", path, err)
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.authToken != "" {
		req.Header.Set("X-DHTMS-Token", c.authToken)
	}

	resp, err := cli.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s %s: %v", ErrUnavailable, method, path, err)
	}
	defer resp.Body.Close()

	limited := io.LimitReader(resp.Body, maxResponseBytes)
	data, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("%w: read %s: %v", ErrUnavailable, path, err)
	}

	if resp.StatusCode != http.StatusOK {
		apiErr := &APIError{StatusCode: resp.StatusCode}
		_ = json.Unmarshal(data, apiErr)
		if apiErr.Message == "" {
			apiErr.Message = strings.TrimSpace(string(data))
		}
		switch {
		case resp.StatusCode == http.StatusServiceUnavailable && apiErr.Code == "lease_expired":
			return fmt.Errorf("%w: %v", ErrLeaseExpired, apiErr)
		case resp.StatusCode == http.StatusUnauthorized:
			return fmt.Errorf("%w: %v", ErrUnauthorized, apiErr)
		case resp.StatusCode == http.StatusBadRequest:
			return fmt.Errorf("%w: %v", ErrBadRequest, apiErr)
		case resp.StatusCode == http.StatusNotFound:
			return fmt.Errorf("%w: %v", ErrNotFound, apiErr)
		default:
			return apiErr
		}
	}

	if respBody != nil {
		if err := json.Unmarshal(data, respBody); err != nil {
			return fmt.Errorf("dhtms: decode %s response: %w", path, err)
		}
	}
	return nil
}

// Health queries GET /v1/health.
func (c *Client) Health(ctx context.Context) (*HealthResponse, error) {
	var out HealthResponse
	if err := c.doJSON(ctx, http.MethodGet, "/v1/health", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Search executes POST /v1/search.
func (c *Client) Search(ctx context.Context, req SearchRequest) (*SearchResponse, error) {
	var out SearchResponse
	if err := c.consumerJSON(ctx, "/v1/search", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Lookup executes POST /v1/lookup (entity ID + unlinked fallback).
func (c *Client) Lookup(ctx context.Context, req LookupRequest) (*LookupResponse, error) {
	var out LookupResponse
	if err := c.consumerJSON(ctx, "/v1/lookup", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RSS executes POST /v1/rss.
func (c *Client) RSS(ctx context.Context, req RSSRequest) (*RSSResponse, error) {
	var out RSSResponse
	if err := c.consumerJSON(ctx, "/v1/rss", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Dashstats executes POST /v1/dashstats.
func (c *Client) Dashstats(ctx context.Context, req DashstatsRequest) (*DashstatsResponse, error) {
	var out DashstatsResponse
	if err := c.consumerJSON(ctx, "/v1/dashstats", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Substring executes POST /v1/substring.
func (c *Client) Substring(ctx context.Context, req SubstringRequest) (*RSSResponse, error) {
	var out RSSResponse
	if err := c.consumerJSON(ctx, "/v1/substring", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Hydrate executes POST /v1/hydrate to retrieve full file trees.
func (c *Client) Hydrate(ctx context.Context, req HydrateRequest) (*RSSResponse, error) {
	var out RSSResponse
	if err := c.consumerJSON(ctx, "/v1/hydrate", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// HashLookup executes POST /v1/hash/lookup for a 40-hex v1 or 64-hex v2 infohash.
func (c *Client) HashLookup(ctx context.Context, req HashLookupRequest) (*HashLookupResponse, error) {
	var out HashLookupResponse
	if err := c.consumerJSON(ctx, "/v1/hash/lookup", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SyncResolve executes authenticated POST /v1/sync/resolve to map infohashes to doc_ids
// without requiring or renewing an active governance lease.
func (c *Client) SyncResolve(ctx context.Context, req SyncResolveRequest) (*SyncResolveResponse, error) {
	var out SyncResolveResponse
	if err := c.doWriteJSON(ctx, http.MethodPost, "/v1/sync/resolve", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PolicyRevoke executes POST /v1/policy/revoke for immediate fail-closed denial.
func (c *Client) PolicyRevoke(ctx context.Context, req PolicyRevokeRequest) (*PolicyRevokeResponse, error) {
	var out PolicyRevokeResponse
	if err := c.doWriteJSON(ctx, http.MethodPost, "/v1/policy/revoke", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PolicyBatch executes POST /v1/policy/batch for batched governance updates.
func (c *Client) PolicyBatch(ctx context.Context, req PolicyBatchRequest) (*PolicyBatchResponse, error) {
	var out PolicyBatchResponse
	if err := c.doWriteJSON(ctx, http.MethodPost, "/v1/policy/batch", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PolicyLease executes POST /v1/policy/lease to renew or expire the PG governance lease.
func (c *Client) PolicyLease(ctx context.Context, req PolicyLeaseRequest) (*PolicyLeaseResponse, error) {
	var out PolicyLeaseResponse
	if err := c.doWriteJSON(ctx, http.MethodPost, "/v1/policy/lease", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SwarmUpdate executes POST /v1/swarm/update.
func (c *Client) SwarmUpdate(ctx context.Context, req SwarmUpdateRequest) (*SwarmUpdateResponse, error) {
	var out SwarmUpdateResponse
	if err := c.doWriteJSON(ctx, http.MethodPost, "/v1/swarm/update", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SwarmSelectStale executes POST /v1/swarm/select-stale.
func (c *Client) SwarmSelectStale(ctx context.Context, req SwarmSelectStaleRequest) (*SwarmSelectStaleResponse, error) {
	var out SwarmSelectStaleResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/swarm/select-stale", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SwarmCheckpoint executes POST /v1/swarm/checkpoint.
func (c *Client) SwarmCheckpoint(ctx context.Context) (*SwarmCheckpointResponse, error) {
	var out SwarmCheckpointResponse
	if err := c.doWriteJSON(ctx, http.MethodPost, "/v1/swarm/checkpoint", struct{}{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// L0Upsert executes POST /v1/l0/upsert for live metadata reclassifications.
func (c *Client) L0Upsert(ctx context.Context, req L0UpsertRequest) (*L0UpsertResponse, error) {
	var out L0UpsertResponse
	if err := c.doWriteJSON(ctx, http.MethodPost, "/v1/l0/upsert", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// BuildGeneration streams an NDJSON generation payload to POST /v1/generation/build
// and atomically swaps dh-tmsd onto the newly built generation. Pass contentLen < 0
// to stream using HTTP/1.1 chunked transfer encoding without buffering in memory.
func (c *Client) BuildGeneration(ctx context.Context, body io.Reader, contentLen int64) (*BuildGenerationResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/generation/build", body)
	if err != nil {
		return nil, fmt.Errorf("dhtms: build request /v1/generation/build: %w", err)
	}
	if contentLen >= 0 {
		req.ContentLength = contentLen
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("Accept", "application/json")
	if c.authToken != "" {
		req.Header.Set("X-DHTMS-Token", c.authToken)
	}

	resp, err := c.streamClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: POST /v1/generation/build: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("%w: read /v1/generation/build: %v", ErrUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		apiErr := &APIError{StatusCode: resp.StatusCode}
		_ = json.Unmarshal(data, apiErr)
		if apiErr.Message == "" {
			apiErr.Message = strings.TrimSpace(string(data))
		}
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			return nil, fmt.Errorf("%w: %v", ErrUnauthorized, apiErr)
		case http.StatusBadRequest:
			return nil, fmt.Errorf("%w: %v", ErrBadRequest, apiErr)
		default:
			return nil, apiErr
		}
	}
	var out BuildGenerationResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("dhtms: decode /v1/generation/build response: %w", err)
	}
	return &out, nil
}

// Reload executes POST /v1/reload to hot-swap dh-tmsd onto the latest CURRENT.json generation.
func (c *Client) Reload(ctx context.Context, req ReloadRequest) (*ReloadResponse, error) {
	var out ReloadResponse
	if err := c.doWriteJSON(ctx, http.MethodPost, "/v1/reload", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
