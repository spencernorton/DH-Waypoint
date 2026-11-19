package prowlarrsync

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSyncOnceForwardsAndDeduplicates(t *testing.T) {
	feedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed">
  <channel>
    <item>
      <title>Synthetic.Movie.2026.1080p.WEB-DL-TEST</title>
      <size>4294967296</size>
      <torznab:attr name="magneturl" value="magnet:?xt=urn:btih:abcdef0123456789abcdef0123456789abcdef01"/>
    </item>
  </channel>
</rss>`))
	}))
	defer feedSrv.Close()

	var importedBodies []string
	var gotQuery string
	wpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		raw, _ := io.ReadAll(r.Body)
		importedBodies = append(importedBodies, string(raw))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("1 items imported\nimport complete\n"))
	}))
	defer wpSrv.Close()

	s := NewSyncer(Config{FeedURL: feedSrv.URL, WaypointURL: wpSrv.URL})
	n1, err := s.SyncOnce(context.Background())
	if err != nil || n1 != 1 {
		t.Fatalf("first sync = %d, %v", n1, err)
	}
	if gotQuery != "on_error=skip" || len(importedBodies) != 1 || !strings.Contains(importedBodies[0], "abcdef0123456789abcdef0123456789abcdef01") {
		t.Fatalf("unexpected import payload: query=%q bodies=%v", gotQuery, importedBodies)
	}
	n2, err := s.SyncOnce(context.Background())
	if err != nil || n2 != 0 {
		t.Fatalf("second sync should deduplicate: %d, %v", n2, err)
	}
}
