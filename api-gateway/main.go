// Command api-gateway is a small API gateway: path-based routing to
// upstreams with API keys, per-client rate limits, health checks and metrics.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/GonService/go-projects/api-gateway/internal/config"
	"github.com/GonService/go-projects/api-gateway/internal/gateway"
)

func main() {
	path := flag.String("config", envOr("CONFIG", "config.json"), "path to the JSON configuration")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.Load(*path)
	if err != nil {
		log.Error("invalid configuration", "path", *path, "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	gw := gateway.New(cfg, log)
	go gw.RunHealthChecks(ctx)

	public := &http.Server{Addr: cfg.Listen, Handler: gw, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	admin := &http.Server{Addr: cfg.AdminListen, Handler: gw.AdminHandler(), ReadHeaderTimeout: 5 * time.Second}

	var wg sync.WaitGroup
	failed := make(chan struct{}, 2)
	for _, srv := range []*http.Server{public, admin} {
		wg.Add(1)
		go func(srv *http.Server) {
			defer wg.Done()
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("server error", "addr", srv.Addr, "error", err)
				failed <- struct{}{}
			}
		}(srv)
	}
	log.Info("listening", "addr", cfg.Listen, "admin", cfg.AdminListen, "routes", len(cfg.Routes))

	select {
	case <-ctx.Done():
	case <-failed:
		stop()
	}
	// Let in-flight requests finish.
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = public.Shutdown(shutdown)
	_ = admin.Shutdown(shutdown)
	wg.Wait()
	log.Info("stopped")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
