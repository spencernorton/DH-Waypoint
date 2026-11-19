// Package prowlarrsync polls a Torznab/Prowlarr RSS feed and forwards newly
// discovered infohashes to DH-Waypoint's fault-isolated `/import?on_error=skip`
// endpoint using only the Go standard library.
package prowlarrsync

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Config struct {
	FeedURL      string
	FeedAPIKey   string
	WaypointURL  string
	ImportSource string
	MaxSeen      int
}

type importRecord struct {
	InfoHash string `json:"infoHash"`
	Name     string `json:"name"`
	Size     int64  `json:"size,omitempty"`
	Source   string `json:"source,omitempty"`
}

type rssFeed struct {
	Channel struct {
		Items []struct {
			Title string `xml:"title"`
			Size  int64  `xml:"size"`
			Attrs []struct {
				Name  string `xml:"name,attr"`
				Value string `xml:"value,attr"`
			} `xml:"attr"`
		} `xml:"item"`
	} `xml:"channel"`
}

type Syncer struct {
	cfg    Config
	client *http.Client
	mu     sync.Mutex
	seen   map[string]struct{}
	order  []string
}

func NewSyncer(cfg Config) *Syncer {
	if cfg.ImportSource == "" {
		cfg.ImportSource = "prowlarr-sync"
	}
	if cfg.MaxSeen <= 0 {
		cfg.MaxSeen = 16384
	}
	return &Syncer{
		cfg:    cfg,
		client: &http.Client{Timeout: 30 * time.Second},
		seen:   make(map[string]struct{}, 1024),
	}
}

func (s *Syncer) markSeen(hash string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.seen[hash]; exists {
		return false
	}
	if len(s.order) >= s.cfg.MaxSeen {
		evict := s.order[0]
		s.order = s.order[1:]
		delete(s.seen, evict)
	}
	s.seen[hash] = struct{}{}
	s.order = append(s.order, hash)
	return true
}

func extractHashFromMagnet(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "magnet" {
		return ""
	}
	for _, xt := range u.Query()["xt"] {
		if strings.HasPrefix(strings.ToLower(xt), "urn:btih:") {
			h := strings.ToLower(strings.TrimPrefix(strings.ToLower(xt), "urn:btih:"))
			if len(h) == 40 {
				return h
			}
		}
	}
	return ""
}

// SyncOnce fetches the configured Torznab/Prowlarr RSS feed and posts unseen
// releases to DH-Waypoint `/import?on_error=skip`.
func (s *Syncer) SyncOnce(ctx context.Context) (int, error) {
	feedURL, err := url.Parse(s.cfg.FeedURL)
	if err != nil {
		return 0, err
	}
	if s.cfg.FeedAPIKey != "" {
		q := feedURL.Query()
		q.Set("apikey", s.cfg.FeedAPIKey)
		feedURL.RawQuery = q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feedURL.String(), nil)
	if err != nil {
		return 0, err
	}
	res, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("feed returned status %d", res.StatusCode)
	}
	rawXML, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return 0, err
	}
	var parsed rssFeed
	if err := xml.Unmarshal(rawXML, &parsed); err != nil {
		return 0, err
	}

	var ndjson bytes.Buffer
	sent := 0
	for _, item := range parsed.Channel.Items {
		var hash string
		for _, a := range item.Attrs {
			switch a.Name {
			case "infohash":
				hash = strings.ToLower(strings.TrimSpace(a.Value))
			case "magneturl":
				if hash == "" {
					hash = extractHashFromMagnet(a.Value)
				}
			}
		}
		if len(hash) != 40 || strings.TrimSpace(item.Title) == "" || !s.markSeen(hash) {
			continue
		}
		line, err := json.Marshal(importRecord{
			InfoHash: hash,
			Name:     item.Title,
			Size:     item.Size,
			Source:   s.cfg.ImportSource,
		})
		if err != nil {
			continue
		}
		ndjson.Write(line)
		ndjson.WriteByte('\n')
		sent++
	}
	if sent == 0 {
		return 0, nil
	}

	target := strings.TrimRight(s.cfg.WaypointURL, "/") + "/import?on_error=skip"
	postReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, &ndjson)
	if err != nil {
		return 0, err
	}
	postReq.Header.Set("Content-Type", "application/x-ndjson")
	postReq.Header.Set("X-Import-Id", s.cfg.ImportSource)
	postRes, err := s.client.Do(postReq)
	if err != nil {
		return 0, err
	}
	defer postRes.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(postRes.Body, 64<<10))
	if postRes.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("waypoint import status %d", postRes.StatusCode)
	}
	return sent, nil
}
