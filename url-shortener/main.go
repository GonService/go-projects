// Command url-shortener serves short links with click statistics.
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

	"github.com/GonService/go-projects/url-shortener/internal/store"
	"github.com/GonService/go-projects/url-shortener/internal/web"
)

func main() {
	addr := flag.String("addr", envOr("ADDR", ":8080"), "listen address")
	data := flag.String("data", envOr("DATA_FILE", "data/links.json"), "JSON file where links are stored (empty keeps them in memory)")
	baseURL := flag.String("base-url", os.Getenv("BASE_URL"), "public base URL for short links, e.g. https://go.example.com (default: request host)")
	flushEvery := flag.Duration("flush", 5*time.Second, "how often click counters are written to disk")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	st, err := store.Open(*data)
	if err != nil {
		log.Error("cannot open data file", "path", *data, "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	flushed := make(chan struct{})
	go func() {
		st.Run(ctx, *flushEvery, func(err error) { log.Error("flush failed", "error", err) })
		close(flushed)
	}()

	srv := &http.Server{
		Addr:              *addr,
		Handler:           web.New(st, *baseURL).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	links, _ := st.Totals()
	log.Info("listening", "addr", *addr, "data", *data, "links", links)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server error", "error", err)
		os.Exit(1)
	}
	<-flushed // wait for the final write of click counters
	log.Info("stopped")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
