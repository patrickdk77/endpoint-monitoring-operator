package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/config"
	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/rollup"
	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/server"
	"github.com/patrickdk77/endpoint-monitoring-operator/dashboard/internal/store"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to dashboard config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	st, err := store.New(cfg)
	if err != nil {
		log.Fatalf("connect valkey: %v", err)
	}
	defer st.Close()

	rollupInterval, err := cfg.RollupDuration()
	if err != nil {
		log.Fatalf("rollup interval: %v", err)
	}

	backfillPace, err := cfg.BackfillPaceDuration()
	if err != nil {
		log.Fatalf("backfill pace: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	runner := rollup.New(st, rollupInterval, backfillPace, cfg.DefaultRetentionDays)
	go runner.Start(ctx)

	srv, err := server.New(st, cfg.DefaultRetentionDays)
	if err != nil {
		log.Fatalf("server: %v", err)
	}

	httpServer := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: srv.Handler(),
	}

	go func() {
		log.Printf("listening on %s", cfg.HTTPAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = httpServer.Shutdown(shutdownCtx)
}
