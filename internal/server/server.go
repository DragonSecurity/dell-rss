// Package server exposes catalog snapshots over HTTP: an HTML dashboard,
// RSS feeds, a JSON API, a health check and Prometheus metrics.
package server

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dragonsecurity/dell-rss/internal/feed"
	"github.com/dragonsecurity/dell-rss/internal/metrics"
)

//go:embed templates/*.html
var templateFS embed.FS

// Config configures a Server.
type Config struct {
	DefaultModel string // model served at /rss.xml, e.g. "R640"
	PublicURL    string // optional, e.g. https://dell-rss.example.com; enables atom:link self
	Version      string
	Logger       *slog.Logger
	Metrics      *metrics.Metrics
}

// Server is safe for concurrent use. Snapshots are swapped atomically.
type Server struct {
	cfg     Config
	log     *slog.Logger
	metrics *metrics.Metrics
	tmpl    *template.Template

	snapshot atomic.Pointer[feed.Snapshot]
	checked  atomic.Int64 // unix nanos of the last successful upstream check
}

// New builds a Server.
func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Metrics == nil {
		cfg.Metrics = metrics.New(cfg.Version)
	}
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")

	tmpl := template.Must(template.New("").Funcs(template.FuncMap{
		"date": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.UTC().Format("2006-01-02")
		},
		"datetime": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.UTC().Format("2006-01-02 15:04 UTC")
		},
		"lower": strings.ToLower,
	}).ParseFS(templateFS, "templates/*.html"))

	return &Server{cfg: cfg, log: cfg.Logger, metrics: cfg.Metrics, tmpl: tmpl}
}

// SetSnapshot publishes a new catalog snapshot.
func (s *Server) SetSnapshot(snap *feed.Snapshot) {
	s.snapshot.Store(snap)
	s.MarkChecked(time.Now())
	s.metrics.Catalog(snap.Modified, snap.Components, len(snap.Models))
}

// MarkChecked records that upstream was checked and found unchanged.
func (s *Server) MarkChecked(t time.Time) { s.checked.Store(t.UnixNano()) }

// Handler returns the HTTP handler with routing and instrumentation.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /metrics", s.metricsHandler)
	mux.HandleFunc("GET /rss.xml", s.defaultRSS)
	mux.HandleFunc("GET /api/models", s.apiModels)
	mux.HandleFunc("GET /api/{model}", s.apiModel)
	mux.HandleFunc("GET /api/{model}/{filter}", s.apiModel)
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		setRoute(w, "favicon")
		w.WriteHeader(http.StatusNoContent)
	})
	// Everything else is /{model}/..., which ServeMux cannot express
	// alongside /api/{model} without conflicting patterns.
	mux.HandleFunc("GET /", s.pages)

	return s.instrument(mux)
}

// pages routes:
//
//	/                          dashboard
//	/{model}/                  model dashboard
//	/{model}/{filter}/         filtered model dashboard
//	/{model}/rss.xml           model feed
//	/{model}/{filter}/rss.xml  filtered feed
func (s *Server) pages(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		setRoute(w, "dashboard")
		s.dashboard(w, r)
		return
	}

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	last := parts[len(parts)-1]

	switch {
	case parts[0] == "":
		// Never let "//host" become a protocol-relative redirect.
		s.notFound(w, r)
	case len(parts) == 2 && last == "rss.xml":
		setRoute(w, "model_rss")
		s.modelRSS(w, r, parts[0], "")
	case len(parts) == 3 && last == "rss.xml":
		setRoute(w, "filter_rss")
		s.modelRSS(w, r, parts[0], parts[1])
	case len(parts) == 2 && last == "":
		setRoute(w, "model_dashboard")
		s.modelPage(w, r, parts[0], "")
	case len(parts) == 3 && last == "":
		setRoute(w, "model_dashboard")
		s.modelPage(w, r, parts[0], parts[1])
	case (len(parts) == 1 || len(parts) == 2) && last != "" && !strings.Contains(last, "."):
		// /r640 -> /r640/, /r640/bios -> /r640/bios/
		setRoute(w, "redirect")
		http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
	default:
		s.notFound(w, r)
	}
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) (*feed.Snapshot, bool) {
	snap := s.snapshot.Load()
	if snap == nil {
		w.Header().Set("Retry-After", "30")
		if wantsHTML(r) {
			s.page(w, http.StatusServiceUnavailable, "loading", nil)
		} else {
			http.Error(w, "catalog not yet loaded, try again shortly", http.StatusServiceUnavailable)
		}
		return nil, false
	}
	return snap, true
}

func (s *Server) resolve(w http.ResponseWriter, r *http.Request, snap *feed.Snapshot, modelName, filterName string) (*feed.Model, feed.Filter, bool) {
	model, ok := snap.Model(modelName)
	if !ok {
		s.notFound(w, r)
		return nil, feed.Filter{}, false
	}
	filter := feed.All
	if filterName != "" {
		if filter, ok = snap.Filter(filterName); !ok {
			s.notFound(w, r)
			return nil, feed.Filter{}, false
		}
	}
	return model, filter, true
}

func (s *Server) defaultRSS(w http.ResponseWriter, r *http.Request) {
	setRoute(w, "rss")
	s.modelRSS(w, r, s.cfg.DefaultModel, "")
}

func (s *Server) modelRSS(w http.ResponseWriter, r *http.Request, modelName, filterName string) {
	snap, ok := s.ready(w, r)
	if !ok {
		return
	}
	model, filter, ok := s.resolve(w, r, snap, modelName, filterName)
	if !ok {
		return
	}

	self := ""
	if s.cfg.PublicURL != "" {
		self = s.cfg.PublicURL + r.URL.Path
	}
	s.serveCached(w, r, snap, "rss|"+r.URL.Path, "application/rss+xml; charset=utf-8", func() ([]byte, error) {
		return snap.RSS(model, filter, self)
	})
}

func (s *Server) apiModels(w http.ResponseWriter, r *http.Request) {
	setRoute(w, "api_models")
	snap, ok := s.ready(w, r)
	if !ok {
		return
	}
	s.serveCached(w, r, snap, "models", "application/json", func() ([]byte, error) {
		return snap.ModelsJSON(feed.ModelSlug(s.cfg.DefaultModel))
	})
}

func (s *Server) apiModel(w http.ResponseWriter, r *http.Request) {
	setRoute(w, "api_model")
	snap, ok := s.ready(w, r)
	if !ok {
		return
	}
	model, filter, ok := s.resolve(w, r, snap, r.PathValue("model"), r.PathValue("filter"))
	if !ok {
		return
	}
	s.serveCached(w, r, snap, "api|"+model.Slug+"|"+filter.Slug, "application/json", func() ([]byte, error) {
		return snap.ModelJSON(model, filter)
	})
}

// serveCached serves a representation that is a pure function of the
// snapshot and key, so its ETag can be computed without rendering it and
// conditional requests are answered for free.
func (s *Server) serveCached(w http.ResponseWriter, r *http.Request, snap *feed.Snapshot, key, contentType string, render func() ([]byte, error)) {
	sum := sha256.Sum256([]byte(snap.ID + "|" + key + "|" + s.cfg.PublicURL))
	etag := `"` + hex.EncodeToString(sum[:12]) + `"`
	modified := snap.Modified

	h := w.Header()
	h.Set("ETag", etag)
	h.Set("Cache-Control", "public, max-age=300")
	if !modified.IsZero() {
		h.Set("Last-Modified", modified.UTC().Format(http.TimeFormat))
	}

	if notModified(r, etag, modified) {
		h.Del("Content-Type")
		w.WriteHeader(http.StatusNotModified)
		return
	}

	body, err := render()
	if err != nil {
		s.log.Error("render failed", "key", key, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.Set("Content-Type", contentType)
	http.ServeContent(w, r, "", modified, bytes.NewReader(body))
}

// notModified implements RFC 9110 conditional GET: If-None-Match takes
// precedence, If-Modified-Since is consulted only without it.
func notModified(r *http.Request, etag string, modified time.Time) bool {
	if inm := r.Header.Get("If-None-Match"); inm != "" {
		for _, candidate := range strings.Split(inm, ",") {
			candidate = strings.TrimSpace(candidate)
			if candidate == "*" || strings.TrimPrefix(candidate, "W/") == etag {
				return true
			}
		}
		return false
	}
	if ims := r.Header.Get("If-Modified-Since"); ims != "" && !modified.IsZero() {
		if t, err := http.ParseTime(ims); err == nil {
			return !modified.Truncate(time.Second).After(t)
		}
	}
	return false
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	setRoute(w, "healthz")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")

	snap := s.snapshot.Load()
	if snap == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "starting"})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":          "ok",
		"version":         s.cfg.Version,
		"catalog_version": snap.Manifest.Version,
		"catalog_date":    snap.Modified,
		"last_checked":    time.Unix(0, s.checked.Load()).UTC(),
		"models":          len(snap.Models),
		"components":      snap.Components,
	})
}

func (s *Server) metricsHandler(w http.ResponseWriter, r *http.Request) {
	setRoute(w, "metrics")
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = s.metrics.WriteTo(w)
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	setRoute(w, "not_found")
	if wantsHTML(r) {
		s.page(w, http.StatusNotFound, "notfound", nil)
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

func wantsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// instrument adds security headers, metrics and an access log.
func (s *Server) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w, code: http.StatusOK, route: "other"}

		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")

		next.ServeHTTP(rec, r)

		took := time.Since(start)
		s.metrics.Request(rec.route, rec.code, took)
		level := slog.LevelInfo
		if rec.route == "healthz" || rec.route == "metrics" {
			level = slog.LevelDebug
		}
		s.log.Log(r.Context(), level, "request",
			"method", r.Method,
			"path", r.URL.Path,
			"route", rec.route,
			"status", rec.code,
			"bytes", rec.bytes,
			"duration_ms", float64(took.Microseconds())/1000,
			"user_agent", r.UserAgent(),
		)
	})
}

type recorder struct {
	http.ResponseWriter
	code        int
	bytes       int
	route       string
	wroteHeader bool
}

func (r *recorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.code = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.wroteHeader = true
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func setRoute(w http.ResponseWriter, route string) {
	if rec, ok := w.(*recorder); ok {
		rec.route = route
	}
}
