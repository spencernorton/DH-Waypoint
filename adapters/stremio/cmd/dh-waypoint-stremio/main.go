package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/spencernorton/DH-Waypoint/adapters/stremio"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	listen := flag.String("listen", envOr("STREMIO_LISTEN", "127.0.0.1:7000"), "HTTP listen address")
	torznabURL := flag.String("torznab-url", envOr("DH_WAYPOINT_TORZNAB_URL", "http://127.0.0.1:3333"), "DH-Waypoint base or Torznab URL")
	name := flag.String("name", envOr("STREMIO_ADDON_NAME", "DH-Waypoint"), "Stremio addon display name")
	maxStreamsDefault := 25
	if raw := os.Getenv("STREMIO_MAX_STREAMS"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			maxStreamsDefault = n
		}
	}
	maxStreams := flag.Int("max-streams", maxStreamsDefault, "Maximum streams returned per query")
	flag.Parse()

	svc := stremio.NewService(stremio.Config{
		ListenAddr: *listen,
		TorznabURL: *torznabURL,
		TorznabKey: os.Getenv("DH_WAYPOINT_TORZNAB_KEY"),
		AddonName:  *name,
		MaxStreams: *maxStreams,
	})
	srv := &http.Server{
		Addr:              *listen,
		Handler:           svc.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	fmt.Fprintf(os.Stderr, "dh-waypoint-stremio listening on %s\n", *listen)
	log.Fatal(srv.ListenAndServe())
}
