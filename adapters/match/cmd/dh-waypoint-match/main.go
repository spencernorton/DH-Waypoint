// dh-waypoint-match is an independent metadata match processor service
// supporting AniList, MusicBrainz, IGDB, and OpenLibrary over processorapi/v1.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/spencernorton/DH-Waypoint/adapters/match"
)

func main() {
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "dh-waypoint-match is configured by environment: MATCH_PROVIDER (anilist|musicbrainz|igdb|openlibrary), PROCESSOR_LISTEN, PROCESSOR_TOKEN, MATCH_ENDPOINT, MATCH_MIN_CONFIDENCE, MATCH_USER_AGENT, MATCH_CLIENT_ID, MATCH_API_TOKEN.")
	}
	flag.Parse()

	provider := os.Getenv("MATCH_PROVIDER")
	if provider == "" {
		provider = "anilist"
	}
	cfg := match.DefaultConfig(provider)
	for env, dst := range map[string]*string{
		"PROCESSOR_LISTEN": &cfg.Listen,
		"MATCH_ENDPOINT":   &cfg.Endpoint,
		"PROCESSOR_TOKEN":  &cfg.Token,
		"MATCH_USER_AGENT": &cfg.UserAgent,
		"MATCH_CLIENT_ID":  &cfg.APIClientID,
		"MATCH_API_TOKEN":  &cfg.APIBearerToken,
	} {
		if v := os.Getenv(env); v != "" {
			*dst = v
		}
	}
	if v := os.Getenv("MATCH_MIN_CONFIDENCE"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			log.Fatal("invalid MATCH_MIN_CONFIDENCE")
		}
		cfg.MinConfidence = f
	}

	service, err := match.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{
		Addr:              cfg.Listen,
		Handler:           service.Handler(),
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      cfg.Timeout + 5*time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    8192,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		bounded, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(bounded)
	}()
	log.Printf("match processor listening (provider=%s)", cfg.Provider)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal("match processor listener failed")
	}
}
