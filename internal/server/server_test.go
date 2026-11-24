package server

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/dragonsecurity/dell-rss/internal/catalog"
	"github.com/dragonsecurity/dell-rss/internal/feed"
	"github.com/dragonsecurity/dell-rss/internal/metrics"
)

func newServer(t *testing.T, loaded bool) (*Server, http.Handler) {
	t.Helper()
	s := New(Config{
		DefaultModel: "R640",
		Version:      "test",
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:      metrics.New("test"),
	})
	if loaded {
		data, err := os.ReadFile("../catalog/testdata/catalog.xml")
		if err != nil {
			t.Fatal(err)
		}
		c, err := catalog.Parse(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		info := catalog.Info{SHA256: "abc", LastModified: "Mon, 23 Nov 2026 09:46:27 GMT"}
		snap, err := feed.Build(c, info, feed.DefaultBrands, "test")
		if err != nil {
			t.Fatal(err)
		}
		s.SetSnapshot(snap)
	}
	return s, s.Handler()
}

func do(h http.Handler, method, path string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRoutes(t *testing.T) {
	_, h := newServer(t, true)

	cases := []struct {
		path, contentType string
		code              int
		contains          string
	}{
		{"/", "text/html", 200, "PowerEdge R640"},
		{"/rss.xml", "application/rss+xml", 200, "<title>Dell PowerEdge R640 Updates</title>"},
		{"/r640/rss.xml", "application/rss+xml", 200, "<title>Dell PowerEdge R640 Updates</title>"},
		{"/R640/rss.xml", "application/rss+xml", 200, "R640"},
		{"/r740/rss.xml", "application/rss+xml", 200, "<title>Dell PowerEdge R740 Updates</title>"},
		{"/r640/bios/rss.xml", "application/rss+xml", 200, "BIOS Updates</title>"},
		{"/r640/idrac/rss.xml", "application/rss+xml", 200, "iDRAC Updates</title>"},
		{"/r640/urgent/rss.xml", "application/rss+xml", 200, "Urgent Updates</title>"},
		{"/r740/idrac/rss.xml", "application/rss+xml", 200, "<channel>"}, // valid filter, no items
		{"/r640/", "text/html", 200, "Broadcom NIC firmware"},
		{"/r640/bios/", "text/html", 200, "BIOS"},
		{"/api/models", "application/json", 200, `"slug": "r750xa"`},
		{"/api/r640", "application/json", 200, `"model": "r640"`},
		{"/api/r640/urgent", "application/json", 200, `"filter": "urgent"`},
		{"/healthz", "application/json", 200, `"status":"ok"`},
		{"/metrics", "text/plain", 200, "dell_rss_models 4"},
		{"/nope/rss.xml", "text/plain", 404, ""},
		{"/r640/nope/rss.xml", "text/plain", 404, ""},
		{"/api/nope", "text/plain", 404, ""},
		{"/api/r640/nope", "text/plain", 404, ""},
		{"/r640/bios/extra/rss.xml", "text/plain", 404, ""},
		{"/r640/feed.json", "text/plain", 404, ""},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			rec := do(h, http.MethodGet, tc.path)
			if rec.Code != tc.code {
				t.Fatalf("status = %d, want %d\n%s", rec.Code, tc.code, rec.Body)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, tc.contentType) {
				t.Errorf("Content-Type = %q, want %s", ct, tc.contentType)
			}
			if !strings.Contains(rec.Body.String(), tc.contains) {
				t.Errorf("body does not contain %q:\n%.600s", tc.contains, rec.Body)
			}
			if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Error("missing nosniff")
			}
		})
	}
}

func TestRedirects(t *testing.T) {
	_, h := newServer(t, true)
	for path, want := range map[string]string{"/r640": "/r640/", "/r640/bios": "/r640/bios/"} {
		rec := do(h, http.MethodGet, path)
		if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != want {
			t.Errorf("%s -> %d %q, want 301 %q", path, rec.Code, rec.Header().Get("Location"), want)
		}
	}
	// Never a protocol-relative redirect to another host.
	for _, path := range []string{"//evil.example", "//evil.example/x"} {
		rec := do(h, http.MethodGet, path)
		if loc := rec.Header().Get("Location"); strings.HasPrefix(loc, "//") {
			t.Errorf("%s redirected to %q", path, loc)
		}
	}
}

func TestConditionalRequests(t *testing.T) {
	_, h := newServer(t, true)

	for _, path := range []string{"/r640/rss.xml", "/r640/bios/rss.xml", "/api/r640", "/api/models"} {
		t.Run(path, func(t *testing.T) {
			first := do(h, http.MethodGet, path)
			etag := first.Header().Get("ETag")
			lastMod := first.Header().Get("Last-Modified")
			if etag == "" || lastMod != "Mon, 23 Nov 2026 09:46:27 GMT" {
				t.Fatalf("ETag %q, Last-Modified %q", etag, lastMod)
			}

			if rec := do(h, http.MethodGet, path, "If-None-Match", etag); rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
				t.Errorf("If-None-Match: %d, %d bytes", rec.Code, rec.Body.Len())
			}
			if rec := do(h, http.MethodGet, path, "If-None-Match", `"other", W/`+etag); rec.Code != http.StatusNotModified {
				t.Errorf("weak match in a list: %d", rec.Code)
			}
			if rec := do(h, http.MethodGet, path, "If-Modified-Since", lastMod); rec.Code != http.StatusNotModified {
				t.Errorf("If-Modified-Since: %d", rec.Code)
			}
			if rec := do(h, http.MethodGet, path, "If-Modified-Since", "Sun, 22 Nov 2026 00:00:00 GMT"); rec.Code != http.StatusOK {
				t.Errorf("older If-Modified-Since: %d", rec.Code)
			}
			// If-None-Match wins over If-Modified-Since.
			if rec := do(h, http.MethodGet, path, "If-None-Match", `"stale"`, "If-Modified-Since", lastMod); rec.Code != http.StatusOK {
				t.Errorf("mismatched ETag with fresh date: %d", rec.Code)
			}
			if rec := do(h, http.MethodHead, path); rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get("ETag") != etag {
				t.Errorf("HEAD: %d, %d bytes", rec.Code, rec.Body.Len())
			}
		})
	}

	// Different representations never share an ETag.
	a := do(h, http.MethodGet, "/r640/rss.xml").Header().Get("ETag")
	b := do(h, http.MethodGet, "/r640/bios/rss.xml").Header().Get("ETag")
	c := do(h, http.MethodGet, "/api/r640").Header().Get("ETag")
	if a == b || a == c || b == c {
		t.Errorf("ETags collide: %s %s %s", a, b, c)
	}
}

func TestNotReady(t *testing.T) {
	_, h := newServer(t, false)
	for _, path := range []string{"/healthz", "/rss.xml", "/r640/rss.xml", "/api/models"} {
		if rec := do(h, http.MethodGet, path); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s = %d, want 503", path, rec.Code)
		}
	}
	rec := do(h, http.MethodGet, "/", "Accept", "text/html")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "Loading") {
		t.Errorf("dashboard before load = %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/metrics"); rec.Code != http.StatusOK {
		t.Errorf("metrics must work before load: %d", rec.Code)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	_, h := newServer(t, true)
	for _, path := range []string{"/", "/rss.xml", "/api/models"} {
		if rec := do(h, http.MethodPost, path); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d", path, rec.Code)
		}
	}
}

func TestDashboardEscapesCatalogText(t *testing.T) {
	_, h := newServer(t, true)
	for _, path := range []string{"/", "/r640/"} {
		rec := do(h, http.MethodGet, path)
		body := rec.Body.String()
		if strings.Contains(body, "<script>") {
			t.Errorf("%s contains raw <script>", path)
		}
		if strings.Contains(body, "javascript:") {
			t.Errorf("%s contains a javascript: URL", path)
		}
		if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
			t.Errorf("%s CSP = %q", path, csp)
		}
	}
}

func TestMetricsLabelsAreBounded(t *testing.T) {
	_, h := newServer(t, true)
	for i := range 20 {
		do(h, http.MethodGet, "/random-"+strings.Repeat("x", i)+"/rss.xml")
	}
	do(h, http.MethodGet, "/r640/rss.xml")

	body := do(h, http.MethodGet, "/metrics").Body.String()
	if strings.Contains(body, "random-") {
		t.Error("request paths leaked into metric labels")
	}
	for _, want := range []string{
		`dell_rss_http_requests_total{route="not_found",code="404"} 20`,
		`dell_rss_http_requests_total{route="model_rss",code="200"} 1`,
		`dell_rss_http_request_duration_seconds_count{route="model_rss"} 1`,
		`dell_rss_catalog_components 3`,
		`dell_rss_build_info{version="test"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

func TestPublicURLAddsSelfLink(t *testing.T) {
	s, _ := newServer(t, true)
	s.cfg.PublicURL = "https://feeds.example"
	rec := do(s.Handler(), http.MethodGet, "/r640/bios/rss.xml")
	if !strings.Contains(rec.Body.String(), `href="https://feeds.example/r640/bios/rss.xml"`) {
		t.Errorf("no atom:link self:\n%.400s", rec.Body)
	}
}
