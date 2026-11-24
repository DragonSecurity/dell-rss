package catalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"golang.org/x/text/encoding/unicode"
)

// dell imitates downloads.dell.com: ETag + Last-Modified, honouring
// conditional requests, with a switchable body.
type dell struct {
	body     atomic.Pointer[[]byte]
	etag     atomic.Pointer[string]
	status   atomic.Int32
	requests atomic.Int32
	lastINM  atomic.Pointer[string]
}

func newDell(t *testing.T) (*dell, *httptest.Server) {
	d := &dell{}
	body := gz(t, utf16(t, fixture(t), unicode.LittleEndian, unicode.UseBOM))
	etag := `"v1"`
	d.body.Store(&body)
	d.etag.Store(&etag)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.requests.Add(1)
		inm := r.Header.Get("If-None-Match")
		d.lastINM.Store(&inm)
		if code := d.status.Load(); code != 0 {
			w.WriteHeader(int(code))
			return
		}
		etag := *d.etag.Load()
		w.Header().Set("ETag", etag)
		w.Header().Set("Last-Modified", "Mon, 23 Nov 2026 09:46:27 GMT")
		if inm == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/x-gzip")
		_, _ = w.Write(*d.body.Load())
	}))
	t.Cleanup(srv.Close)
	return d, srv
}

func TestFetchConditionalAndPersist(t *testing.T) {
	d, srv := newDell(t)
	dir := t.TempDir()
	f := &Fetcher{URL: srv.URL, CacheDir: dir}
	ctx := context.Background()

	first, err := f.Fetch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.NotModified || first.Catalog == nil || len(first.Catalog.Components) != 5 {
		t.Fatalf("first fetch = %+v", first)
	}
	if got := *d.lastINM.Load(); got != "" {
		t.Errorf("first request sent If-None-Match %q; it has nothing to validate", got)
	}
	if first.Info.ETag != `"v1"` || first.Downloaded == 0 || first.Info.SHA256 == "" {
		t.Errorf("info = %+v", first.Info)
	}
	if mod := first.Info.Modified(); mod.Format(http.TimeFormat) != "Mon, 23 Nov 2026 09:46:27 GMT" {
		t.Errorf("Modified() = %v", mod)
	}

	second, err := f.Fetch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !second.NotModified || second.Catalog != nil || second.Downloaded != 0 {
		t.Fatalf("second fetch should be a 304: %+v", second)
	}
	if got := *d.lastINM.Load(); got != `"v1"` {
		t.Errorf("If-None-Match = %q", got)
	}

	// A new process picks the catalog up from disk, then revalidates it.
	restarted := &Fetcher{URL: srv.URL, CacheDir: dir}
	cached, info, err := restarted.LoadCached()
	if err != nil {
		t.Fatalf("LoadCached: %v", err)
	}
	if len(cached.Components) != 5 || info.SHA256 != first.Info.SHA256 {
		t.Errorf("cached = %d components, info %+v", len(cached.Components), info)
	}
	third, err := restarted.Fetch(ctx)
	if err != nil || !third.NotModified {
		t.Fatalf("revalidation after restart = %+v, %v", third, err)
	}

	// Reset makes the next request unconditional.
	restarted.Reset()
	fourth, err := restarted.Fetch(ctx)
	if err != nil || fourth.NotModified {
		t.Fatalf("after Reset = %+v, %v", fourth, err)
	}
}

func TestFetchBadDownloadKeepsCache(t *testing.T) {
	d, srv := newDell(t)
	dir := t.TempDir()
	f := &Fetcher{URL: srv.URL, CacheDir: dir}
	ctx := context.Background()

	if _, err := f.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	good, err := os.ReadFile(filepath.Join(dir, cacheFile))
	if err != nil {
		t.Fatal(err)
	}

	// Upstream changes to something unparseable.
	junk := []byte("\x1f\x8b not really gzip")
	etag := `"v2"`
	d.body.Store(&junk)
	d.etag.Store(&etag)

	if _, err := f.Fetch(ctx); err == nil {
		t.Fatal("expected a parse error")
	}
	after, _ := os.ReadFile(filepath.Join(dir, cacheFile))
	if string(after) != string(good) {
		t.Fatal("a corrupt download replaced the good cache")
	}
	if _, _, err := (&Fetcher{URL: srv.URL, CacheDir: dir}).LoadCached(); err != nil {
		t.Fatalf("cache unusable after a bad download: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("temporary files left behind: %v", entries)
	}

	d.status.Store(http.StatusBadGateway)
	if _, err := f.Fetch(ctx); err == nil {
		t.Fatal("expected an error for HTTP 502")
	}
}

func TestLoadCachedRejectsTamperedFile(t *testing.T) {
	_, srv := newDell(t)
	dir := t.TempDir()
	if _, err := (&Fetcher{URL: srv.URL, CacheDir: dir}).Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Same structure, different bytes: the checksum must catch it.
	other := gz(t, fixture(t))
	if err := os.WriteFile(filepath.Join(dir, cacheFile), other, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (&Fetcher{URL: srv.URL, CacheDir: dir}).LoadCached(); err == nil {
		t.Fatal("expected a checksum error")
	}
}

func TestLoadCachedIgnoresOtherURL(t *testing.T) {
	_, srv := newDell(t)
	dir := t.TempDir()
	if _, err := (&Fetcher{URL: srv.URL, CacheDir: dir}).Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (&Fetcher{URL: srv.URL + "/other", CacheDir: dir}).LoadCached(); err == nil {
		t.Fatal("cache for one URL must not be served for another")
	}
	if _, _, err := (&Fetcher{URL: srv.URL}).LoadCached(); err != ErrNoCache {
		t.Fatalf("no cache dir: err = %v, want ErrNoCache", err)
	}
}

func TestFetchWithoutCacheDir(t *testing.T) {
	_, srv := newDell(t)
	f := &Fetcher{URL: srv.URL}
	res, err := f.Fetch(context.Background())
	if err != nil || res.Catalog == nil {
		t.Fatalf("Fetch = %+v, %v", res, err)
	}
}
