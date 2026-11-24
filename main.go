// Command dell-rss monitors Dell's enterprise update catalog and publishes
// per-model RSS feeds, a JSON API and an HTML dashboard.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/dragonsecurity/dell-rss/internal/catalog"
	"github.com/dragonsecurity/dell-rss/internal/feed"
	"github.com/dragonsecurity/dell-rss/internal/metrics"
	"github.com/dragonsecurity/dell-rss/internal/server"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

type config struct {
	Port         string
	DefaultModel string
	Interval     time.Duration
	CatalogURL   string
	CacheDir     string
	Brands       []string
	PublicURL    string
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			os.Exit(healthcheck())
		case "version", "-version", "--version":
			fmt.Println(version)
			return
		}
	}

	logger := newLogger()
	slog.SetDefault(logger)

	cfg, err := loadConfig()
	if err != nil {
		logger.Error("invalid configuration", "err", err)
		os.Exit(2)
	}

	if err := run(cfg, logger); err != nil {
		logger.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func loadConfig() (config, error) {
	cfg := config{
		Port:         env("PORT", "8088"),
		DefaultModel: env("DELL_MODEL", "R640"),
		CatalogURL:   env("CATALOG_URL", catalog.DefaultURL),
		PublicURL:    env("PUBLIC_URL", ""),
		Brands:       feed.DefaultBrands,
	}

	interval, err := time.ParseDuration(env("REFRESH_INTERVAL", "6h"))
	if err != nil || interval < time.Minute {
		return cfg, fmt.Errorf("REFRESH_INTERVAL must be a duration of at least 1m, got %q", os.Getenv("REFRESH_INTERVAL"))
	}
	cfg.Interval = interval

	if brands := env("DELL_BRANDS", ""); brands != "" {
		cfg.Brands = nil
		for _, b := range strings.Split(brands, ",") {
			if b = strings.TrimSpace(b); b != "" {
				cfg.Brands = append(cfg.Brands, b)
			}
		}
	}

	// CACHE_DIR="" (explicitly empty is indistinguishable from unset, so use
	// "off") disables persistence.
	cfg.CacheDir = env("CACHE_DIR", "")
	if cfg.CacheDir == "" {
		if dir, err := os.UserCacheDir(); err == nil {
			cfg.CacheDir = filepath.Join(dir, "dell-rss")
		}
	}
	if cfg.CacheDir == "off" {
		cfg.CacheDir = ""
	}

	return cfg, nil
}

func newLogger() *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(env("LOG_LEVEL", "info"))); err != nil {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}

	if strings.EqualFold(env("LOG_FORMAT", "json"), "text") {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}

func run(cfg config, logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.CacheDir != "" {
		if err := os.MkdirAll(cfg.CacheDir, 0o755); err != nil {
			logger.Warn("catalog cache disabled", "dir", cfg.CacheDir, "err", err)
			cfg.CacheDir = ""
		}
	}

	m := metrics.New(version)
	srv := server.New(server.Config{
		DefaultModel: cfg.DefaultModel,
		PublicURL:    cfg.PublicURL,
		Version:      version,
		Logger:       logger,
		Metrics:      m,
	})

	fetcher := &catalog.Fetcher{
		URL:       cfg.CatalogURL,
		Client:    &http.Client{Timeout: 3 * time.Minute},
		CacheDir:  cfg.CacheDir,
		UserAgent: "dell-rss/" + version,
	}

	r := &refresher{cfg: cfg, log: logger, fetcher: fetcher, server: srv, metrics: m}
	r.loadCache()

	httpServer := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	logger.Info("starting",
		"version", version,
		"port", cfg.Port,
		"default_model", cfg.DefaultModel,
		"refresh_interval", cfg.Interval.String(),
		"cache_dir", cfg.CacheDir,
		"brands", cfg.Brands,
	)

	refreshDone := make(chan struct{})
	go func() {
		defer close(refreshDone)
		r.loop(ctx)
	}()

	serveErr := make(chan error, 1)
	go func() { serveErr <- httpServer.ListenAndServe() }()

	select {
	case err := <-serveErr:
		stop()
		<-refreshDone
		return err
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	err := httpServer.Shutdown(shutdownCtx)
	<-refreshDone
	if err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	logger.Info("stopped")
	return nil
}

type refresher struct {
	cfg     config
	log     *slog.Logger
	fetcher *catalog.Fetcher
	server  *server.Server
	metrics *metrics.Metrics
	loaded  bool
}

func (r *refresher) loadCache() {
	start := time.Now()
	cat, info, err := r.fetcher.LoadCached()
	switch {
	case errors.Is(err, catalog.ErrNoCache):
		r.log.Info("no cached catalog; waiting for first download")
		return
	case err != nil:
		r.log.Warn("ignoring cached catalog", "err", err)
		return
	}
	if err := r.publish(cat, info); err != nil {
		r.fetcher.Reset()
		r.log.Warn("ignoring cached catalog", "err", err)
		return
	}
	r.log.Info("serving cached catalog",
		"catalog_version", cat.Manifest.Version,
		"fetched_at", info.FetchedAt,
		"took_ms", time.Since(start).Milliseconds())
}

func (r *refresher) publish(cat *catalog.Catalog, info catalog.Info) error {
	snap, err := feed.Build(cat, info, r.cfg.Brands, version)
	if err != nil {
		return err
	}
	if _, ok := snap.Model(r.cfg.DefaultModel); !ok {
		r.log.Warn("default model not found in catalog; /rss.xml will 404", "model", r.cfg.DefaultModel)
	}
	r.server.SetSnapshot(snap)
	r.loaded = true
	return nil
}

// loop refreshes immediately, then every interval. Failures retry with
// exponential backoff (capped at the interval) so a failed first download
// does not leave the service empty for hours.
func (r *refresher) loop(ctx context.Context) {
	const minBackoff = 30 * time.Second
	backoff := minBackoff

	for {
		wait := r.cfg.Interval
		if err := r.refresh(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			wait = min(backoff, r.cfg.Interval)
			backoff = min(backoff*2, r.cfg.Interval)
			r.log.Error("catalog refresh failed", "err", err, "retry_in", wait.String())
		} else {
			backoff = minBackoff
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (r *refresher) refresh(ctx context.Context) error {
	start := time.Now()
	res, err := r.fetcher.Fetch(ctx)
	took := time.Since(start)

	if err != nil {
		r.metrics.Refresh("error", took, res.Downloaded)
		return err
	}

	if res.NotModified {
		r.metrics.Refresh("not_modified", took, 0)
		r.server.MarkChecked(time.Now())
		r.log.Info("catalog unchanged", "took_ms", took.Milliseconds())
		return nil
	}

	if err := r.publish(res.Catalog, res.Info); err != nil {
		if !r.loaded {
			// Nothing is being served, so the next attempt must not be a
			// conditional request that Dell answers with 304.
			r.fetcher.Reset()
		}
		r.metrics.Refresh("error", took, res.Downloaded)
		return err
	}
	r.metrics.Refresh("success", took, res.Downloaded)
	r.log.Info("catalog refreshed",
		"catalog_version", res.Catalog.Manifest.Version,
		"components", len(res.Catalog.Components),
		"bytes", res.Downloaded,
		"etag", res.Info.ETag,
		"took_ms", took.Milliseconds())
	return nil
}

// healthcheck lets the distroless image (no shell, no curl) probe itself.
func healthcheck() int {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + env("PORT", "8088") + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthz:", resp.Status)
		return 1
	}
	return 0
}
