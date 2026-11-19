package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	prowlarrsync "github.com/spencernorton/DH-Waypoint/adapters/prowlarr-sync"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	feedURL := flag.String("feed-url", os.Getenv("PROWLARR_TORZNAB_FEED_URL"), "Torznab RSS feed URL to poll")
	waypointURL := flag.String("waypoint-url", envOr("DH_WAYPOINT_URL", "http://127.0.0.1:3333"), "DH-Waypoint HTTP base URL")
	source := flag.String("source", envOr("PROWLARR_IMPORT_SOURCE", "prowlarr-sync"), "X-Import-Id header value")
	interval := flag.Duration("interval", 5*time.Minute, "Poll interval (0 runs once and exits)")
	flag.Parse()

	if *feedURL == "" {
		log.Fatal("feed-url or PROWLARR_TORZNAB_FEED_URL is required")
	}

	syncer := prowlarrsync.NewSyncer(prowlarrsync.Config{
		FeedURL:      *feedURL,
		FeedAPIKey:   os.Getenv("PROWLARR_API_KEY"),
		WaypointURL:  *waypointURL,
		ImportSource: *source,
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	run := func() {
		n, err := syncer.SyncOnce(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sync error: %v\n", err)
			return
		}
		fmt.Fprintf(os.Stderr, "synced %d new items\n", n)
	}
	run()
	if *interval <= 0 {
		return
	}
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}
