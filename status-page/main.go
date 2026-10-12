// Command status-page checks HTTP services and serves a public status page.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/GonService/go-projects/status-page/internal/config"
	"github.com/GonService/go-projects/status-page/internal/monitor"
	"github.com/GonService/go-projects/status-page/internal/web"
)

func main() {
	configPath := flag.String("config", envOr("CONFIG", "config.json"), "path to the JSON configuration")
	addr := flag.String("addr", envOr("ADDR", ":8080"), "listen address")
	keep := flag.Int("history", 60, "checks kept per service")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, targets, err := config.Load(*configPath)
	if err != nil {
		log.Error("invalid configuration", "path", *configPath, "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mon := monitor.New(targets, *keep)
	go mon.Run(ctx)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           web.Handler(cfg.Title, mon),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	log.Info("listening", "addr", *addr, "services", len(targets))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server error", "error", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
