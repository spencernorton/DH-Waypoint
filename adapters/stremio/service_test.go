package stremio

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStremioManifestAndSeriesStream(t *testing.T) {
	var gotQuery string
	torznabSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed">
  <channel>
    <item>
      <title>Synthetic.Show.S02E05.1080p.WEB-DL.x265-TEST</title>
      <size>2147483648</size>
      <torznab:attr name="infohash" value="0123456789abcdef0123456789abcdef01234567"/>
      <torznab:attr name="resolution" value="1080p"/>
      <torznab:attr name="video" value="x265"/>
      <torznab:attr name="seeders" value="42"/>
    </item>
  </channel>
</rss>`))
	}))
	defer torznabSrv.Close()

	svc := NewService(Config{TorznabURL: torznabSrv.URL, AddonName: "WaypointTest"})
	h := svc.Handler()

	mRec := httptest.NewRecorder()
	h.ServeHTTP(mRec, httptest.NewRequest(http.MethodGet, "/manifest.json", nil))
	if mRec.Code != http.StatusOK {
		t.Fatalf("manifest status = %d", mRec.Code)
	}

	sRec := httptest.NewRecorder()
	h.ServeHTTP(sRec, httptest.NewRequest(http.MethodGet, "/stream/series/tt6002000:2:5.json", nil))
	if sRec.Code != http.StatusOK {
		t.Fatalf("stream status = %d", sRec.Code)
	}
	var sr StreamResponse
	if err := json.Unmarshal(sRec.Body.Bytes(), &sr); err != nil {
		t.Fatal(err)
	}
	if len(sr.Streams) != 1 || sr.Streams[0].InfoHash != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("unexpected streams: %+v", sr.Streams)
	}
	if gotQuery == "" {
		t.Fatal("expected torznab request")
	}
}
