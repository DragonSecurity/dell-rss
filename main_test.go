package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dragonsecurity/dell-rss/internal/catalog"
	"github.com/dragonsecurity/dell-rss/internal/metrics"
	"github.com/dragonsecurity/dell-rss/internal/server"
)

func TestLoadConfig(t *testing.T) {
	t.Setenv("REFRESH_INTERVAL", "2h")
	t.Setenv("DELL_BRANDS", " PowerEdge , Precision,,")
	t.Setenv("CACHE_DIR", "off")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Interval != 2*time.Hour || cfg.CacheDir != "" || strings.Join(cfg.Brands, "|") != "PowerEdge|Precision" {
		t.Errorf("cfg = %+v", cfg)
	}
	if cfg.DefaultModel != "R640" || cfg.Port != "8088" {
		t.Errorf("defaults = %+v", cfg)
	}

	for _, bad := range []string{"soon", "-1h", "10s"} {
		t.Setenv("REFRESH_INTERVAL", bad)
		if _, err := loadConfig(); err == nil {
			t.Errorf("REFRESH_INTERVAL=%q accepted", bad)
		}
	}
}

// End to end: a refresh against a fake Dell publishes a snapshot the HTTP
// handler serves, and a second refresh is a cheap 304.
func TestRefreshPublishes(t *testing.T) {
	body, err := os.ReadFile("internal/catalog/testdata/catalog.xml")
	if err != nil {
		t.Fatal(err)
	}
	dell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write(body)
	}))
	defer dell.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := metrics.New("test")
	srv := server.New(server.Config{DefaultModel: "R640", Logger: logger, Metrics: m})
	cfg := config{DefaultModel: "R640", Brands: []string{"PowerEdge"}, Interval: time.Hour}
	r := &refresher{
		cfg:     cfg,
		log:     logger,
		fetcher: &catalog.Fetcher{URL: dell.URL, CacheDir: t.TempDir()},
		server:  srv,
		metrics: m,
	}

	h := srv.Handler()
	get := func(path string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code
	}

	if code := get("/rss.xml"); code != http.StatusServiceUnavailable {
		t.Fatalf("before refresh = %d", code)
	}
	if err := r.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if code := get("/rss.xml"); code != http.StatusOK {
		t.Fatalf("after refresh = %d", code)
	}
	if err := r.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, want := range []string{
		`dell_rss_catalog_refreshes_total{result="success"} 1`,
		`dell_rss_catalog_refreshes_total{result="not_modified"} 1`,
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}
