// dh-waypoint-llm is an independent processor service, not a backend worker.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spencernorton/DH-Waypoint/adapters/llm"
)

func main() {
	path := flag.String("config", "", "service-local JSON configuration file")
	flag.Parse()
	cfg := llm.DefaultConfig()
	var err error
	if *path != "" {
		cfg, err = llm.LoadConfig(*path)
		if err != nil {
			log.Fatal(err)
		}
	}
	service, err := llm.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer service.Close()
	server := &http.Server{Addr: cfg.Listen, Handler: service.Handler(), ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout: 5 * time.Second, WriteTimeout: time.Duration(cfg.TimeoutMS)*time.Millisecond + 2*time.Second,
		IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		bounded, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(bounded)
	}()
	log.Print("LLM processor listening; readiness does not probe the provider")
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal("processor listener failed")
	}
}
