package tracker

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
)

// Key identifies this HTTP option (see httpserver.Options config).
func (s *Store) Key() string { return "tracker" }

// Apply mounts the tracker announce/scrape endpoints and the
// private torrent-file API. Routes always exist so clients get a
// clear signal (tracker failure reason / 503) instead of a bare 404
// when the feature is not configured.
func (s *Store) Apply(e *gin.Engine) error {
	e.GET("/announce/:passkey", s.handleAnnounce)
	e.GET("/scrape/:passkey", s.handleScrape)

	g := e.Group("/api/private/v1", s.adminAuth)
	g.POST("/torrents", s.handleUpload)
	g.GET("/torrents/:hash", s.handleDetail)
	// Download accepts a valid passkey instead of the admin token
	// (see handleDownload), so it is registered outside the group.
	e.GET("/api/private/v1/torrents/:hash/download", s.handleDownload)
	return nil
}

func (s *Store) adminAuth(c *gin.Context) {
	if s.cfg.AdminToken == "" {
		c.AbortWithStatusJSON(503, gin.H{"error": "private_api_disabled"})
		return
	}
	if !s.acceptsAdmin(c) {
		c.AbortWithStatusJSON(401, gin.H{"error": "unauthorized"})
		return
	}
	c.Next()
}

func (s *Store) acceptsAdmin(c *gin.Context) bool {
	expected := "Bearer " + string(s.cfg.AdminToken)
	return subtle.ConstantTimeCompare([]byte(c.GetHeader("Authorization")), []byte(expected)) == 1
}

func (s *Store) writeFailure(c *gin.Context, reason string) {
	raw, err := EncodeFailure(reason)
	if err != nil {
		c.Status(500)
		return
	}
	c.Data(200, "text/plain", raw)
}

func (s *Store) handleAnnounce(c *gin.Context) {
	if !s.cfg.TrackerActive() {
		s.writeFailure(c, "tracker not enabled")
		return
	}
	if !s.cfg.AcceptsPasskey(c.Param("passkey")) {
		s.writeFailure(c, "invalid passkey")
		return
	}
	a, err := ParseAnnounce(c.Request.URL.Query())
	if err != nil {
		s.writeFailure(c, err.Error())
		return
	}
	resp, err := s.Announce(c.Request.Context(), a, clientIP(c, a.IP))
	if errors.Is(err, ErrUnregisteredTorrent) {
		s.writeFailure(c, "unregistered torrent")
		return
	}
	if err != nil {
		s.log().Warnw("tracker announce", "error", err)
		s.writeFailure(c, "tracker unavailable")
		return
	}
	raw, err := EncodeAnnounce(resp, a.Compact)
	if err != nil {
		s.writeFailure(c, "tracker unavailable")
		return
	}
	c.Data(200, "text/plain", raw)
}

func (s *Store) handleScrape(c *gin.Context) {
	if !s.cfg.TrackerActive() {
		s.writeFailure(c, "tracker not enabled")
		return
	}
	if !s.cfg.AcceptsPasskey(c.Param("passkey")) {
		s.writeFailure(c, "invalid passkey")
		return
	}
	var hashes []protocol.ID
	for _, raw := range c.Request.URL.Query()["info_hash"] {
		if len(raw) != 20 || len(hashes) >= 74 {
			continue
		}
		var h protocol.ID
		copy(h[:], raw)
		hashes = append(hashes, h)
	}
	swarms, err := s.Scrape(c.Request.Context(), hashes)
	if err != nil {
		s.log().Warnw("tracker scrape", "error", err)
		s.writeFailure(c, "tracker unavailable")
		return
	}
	raw, err := EncodeScrape(swarms)
	if err != nil {
		s.writeFailure(c, "tracker unavailable")
		return
	}
	c.Data(200, "text/plain", raw)
}

func (s *Store) handleUpload(c *gin.Context) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, MaxTorrentFileBytes+1))
	if err != nil {
		c.JSON(400, gin.H{"error": "read_failed"})
		return
	}
	result, err := s.Upload(c.Request.Context(), body)
	switch {
	case errors.Is(err, ErrInvalidTorrentFile):
		c.JSON(400, gin.H{"error": "invalid_torrent_file"})
	case errors.Is(err, ErrRejectedByPolicy):
		c.JSON(422, gin.H{"error": "rejected_by_ingestion_policy"})
	case err != nil:
		s.log().Warnw("private upload", "error", err)
		c.JSON(500, gin.H{"error": "upload_failed"})
	default:
		c.JSON(201, result)
	}
}

func parseHashParam(c *gin.Context) (protocol.ID, bool) {
	id, err := protocol.ParseID(c.Param("hash"))
	if err != nil {
		c.JSON(400, gin.H{"error": "invalid_info_hash"})
		return protocol.ID{}, false
	}
	return id, true
}

func (s *Store) handleDetail(c *gin.Context) {
	hash, ok := parseHashParam(c)
	if !ok {
		return
	}
	detail, err := s.Detail(c.Request.Context(), hash)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(404, gin.H{"error": "not_found"})
		return
	}
	if err != nil {
		s.log().Warnw("private detail", "error", err)
		c.JSON(500, gin.H{"error": "detail_failed"})
		return
	}
	c.JSON(200, detail)
}

// handleDownload serves the stored .torrent file. Authorized by the
// admin token or by a tracker passkey (`?passkey=`); when a passkey
// is supplied and the tracker is active, the announce URL is
// personalised to this tracker + passkey. The info dictionary is
// never modified, so the info hash is stable.
func (s *Store) handleDownload(c *gin.Context) {
	if s.cfg.AdminToken == "" && !s.cfg.TrackerActive() {
		c.JSON(503, gin.H{"error": "private_api_disabled"})
		return
	}
	passkey := c.Query("passkey")
	passkeyOK := s.cfg.AcceptsPasskey(passkey)
	if !passkeyOK && (s.cfg.AdminToken == "" || !s.acceptsAdmin(c)) {
		c.JSON(401, gin.H{"error": "unauthorized"})
		return
	}
	hash, ok := parseHashParam(c)
	if !ok {
		return
	}
	// Stored files deliberately survive catalogue removal (retained
	// evidence class), so downloads re-check catalogue membership.
	exists, err := s.TorrentExists(c.Request.Context(), hash)
	if err != nil {
		c.JSON(500, gin.H{"error": "download_failed"})
		return
	}
	if !exists {
		c.JSON(404, gin.H{"error": "not_found"})
		return
	}
	data, err := s.GetTorrentFile(c.Request.Context(), hash)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(404, gin.H{"error": "torrent_file_not_stored"})
		return
	}
	if err != nil {
		s.log().Warnw("private download", "error", err)
		c.JSON(500, gin.H{"error": "download_failed"})
		return
	}
	if passkeyOK {
		data, err = RewriteAnnounce(data, s.announceURL(c, passkey))
		if err != nil {
			s.log().Warnw("private download rewrite", "error", err)
			c.JSON(500, gin.H{"error": "download_failed"})
			return
		}
	}
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", hash.String()+".torrent"))
	c.Data(200, "application/x-bittorrent", data)
}

func (s *Store) announceURL(c *gin.Context, passkey string) string {
	base := strings.TrimRight(s.cfg.PublicBaseURL, "/")
	if base == "" {
		scheme := "http"
		if c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https") {
			scheme = "https"
		}
		base = scheme + "://" + c.Request.Host
	}
	return base + "/announce/" + url.PathEscape(passkey)
}

// clientIP resolves the announcing peer's address: connection
// remote address first, explicit ip parameter as fallback. The
// result identifies a peer for swarm purposes only; it is never an
// authentication input.
func clientIP(c *gin.Context, param string) string {
	if ip := c.ClientIP(); ip != "" {
		return ip
	}
	if param != "" {
		if _, err := net.ResolveIPAddr("ip", param); err == nil {
			return param
		}
	}
	return "0.0.0.0"
}
