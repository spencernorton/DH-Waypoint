package httpserver

import (
	"github.com/gin-gonic/gin"
	"github.com/spencernorton/DH-Waypoint/internal/importer"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/metainfo"
	"io"
)

func (b builder) handleTorrent(c *gin.Context, service importer.Importer) {
	const maxTorrentBytes = 2 * metainfo.MaxMetaInfoBytes
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, maxTorrentBytes+1))
	if err != nil {
		c.JSON(400, gin.H{"error": "torrent_body_read_failed"})
		return
	}
	if len(raw) > maxTorrentBytes {
		c.JSON(413, gin.H{"error": "torrent_body_too_large"})
		return
	}
	var expected *protocol.InfoHash
	if value := c.Query("hash"); value != "" {
		h, err := protocol.ParseInfoHash(value)
		if err != nil {
			c.JSON(400, gin.H{"error": "invalid_info_hash"})
			return
		}
		expected = &h
	}
	item, err := importer.ItemFromTorrentBytes(raw, expected, "metainfo_import")
	if err != nil {
		c.JSON(400, gin.H{"error": "invalid_torrent_metadata"})
		return
	}
	ai := service.New(c.Request.Context(), importer.Info{ID: c.GetHeader(ImportIDHeader)})
	defer ai.Close()
	if err = ai.Import(item); err == nil {
		ai.Drain()
		err = ai.Close()
	}
	if err != nil {
		c.JSON(400, gin.H{"error": "torrent_import_failed"})
		return
	}
	c.JSON(200, gin.H{"contract": "dh-waypoint.metainfo-import.v2", "info_hash": item.InfoHash.String()})
}
