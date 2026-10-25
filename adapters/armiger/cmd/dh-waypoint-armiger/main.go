// dh-waypoint-armiger is an independent processor service, not a backend worker.
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

	"github.com/spencernorton/DH-Waypoint/adapters/armiger"
)

func main() {
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "dh-waypoint-armiger is configured by environment: PROCESSOR_LISTEN, ARMIGER_URL, ARMIGER_VERSION, ARMIGER_MIN_CONFIDENCE and PROCESSOR_TOKEN.")
	}
	flag.Parse()
	cfg := armiger.DefaultConfig()
	for env, dst := range map[string]*string{"PROCESSOR_LISTEN": &cfg.Listen, "ARMIGER_URL": &cfg.ArmigerURL,
		"ARMIGER_VERSION": &cfg.ArmigerVersion, "PROCESSOR_TOKEN": &cfg.Token} {
		if v := os.Getenv(env); v != "" {
			*dst = v
		}
	}
	if v := os.Getenv("ARMIGER_MIN_CONFIDENCE"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			log.Fatal("invalid ARMIGER_MIN_CONFIDENCE")
		}
		cfg.MinConfidence = f
	}
	service, err := armiger.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Addr: cfg.Listen, Handler: service.Handler(), ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout: 5 * time.Second, WriteTimeout: cfg.Timeout + 5*time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		bounded, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(bounded)
	}()
	log.Print("Armiger processor listening")
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal("processor listener failed")
	}
}
