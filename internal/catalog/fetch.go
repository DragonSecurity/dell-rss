package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// DefaultURL is Dell's enterprise catalog.
	DefaultURL = "https://downloads.dell.com/catalog/Catalog.xml.gz"

	// MaxSize bounds how much is read from the network (compressed).
	MaxSize = 256 << 20

	cacheFile = "Catalog.xml.gz"
	metaFile  = "catalog.json"
)

// ErrNoCache is returned by LoadCached when there is nothing usable on disk.
var ErrNoCache = errors.New("no cached catalog")

// Info identifies the catalog bytes a Catalog was parsed from.
type Info struct {
	URL          string    `json:"url"`
	ETag         string    `json:"etag,omitempty"`
	LastModified string    `json:"last_modified,omitempty"`
	SHA256       string    `json:"sha256"`
	Size         int64     `json:"size"`
	FetchedAt    time.Time `json:"fetched_at"`
}

// Modified is the best available "this catalog changed at" time: the
// upstream Last-Modified header, falling back to when it was downloaded.
func (i Info) Modified() time.Time {
	if t, err := http.ParseTime(i.LastModified); err == nil {
		return t.UTC()
	}
	return i.FetchedAt.UTC()
}

// Result is the outcome of a Fetch.
type Result struct {
	Catalog     *Catalog // nil when NotModified
	Info        Info
	NotModified bool
	Downloaded  int64 // bytes read from the network
}

// Fetcher downloads the catalog with conditional requests and keeps the last
// good copy on disk so a restart can serve immediately.
type Fetcher struct {
	URL       string
	Client    *http.Client
	CacheDir  string // empty disables persistence
	UserAgent string

	mu      sync.Mutex
	current *Info // set only once a catalog has been parsed successfully
}

// LoadCached parses the catalog persisted by a previous run.
func (f *Fetcher) LoadCached() (*Catalog, Info, error) {
	if f.CacheDir == "" {
		return nil, Info{}, ErrNoCache
	}

	data, err := os.ReadFile(filepath.Join(f.CacheDir, metaFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, Info{}, ErrNoCache
	}
	if err != nil {
		return nil, Info{}, err
	}

	var info Info
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, Info{}, fmt.Errorf("read cache metadata: %w", err)
	}
	if info.URL != f.URL {
		return nil, Info{}, fmt.Errorf("%w: cache was for %s", ErrNoCache, info.URL)
	}

	file, err := os.Open(filepath.Join(f.CacheDir, cacheFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, Info{}, ErrNoCache
	}
	if err != nil {
		return nil, Info{}, err
	}
	defer file.Close()

	hash := sha256.New()
	catalog, err := Parse(io.TeeReader(file, hash))
	if err != nil {
		return nil, Info{}, fmt.Errorf("parse cached catalog: %w", err)
	}
	// Drain so the hash covers the whole file, then make sure it is the file
	// the metadata describes.
	if _, err := io.Copy(hash, file); err != nil {
		return nil, Info{}, err
	}
	if sum := hex.EncodeToString(hash.Sum(nil)); sum != info.SHA256 {
		return nil, Info{}, fmt.Errorf("cached catalog checksum mismatch")
	}

	f.mu.Lock()
	f.current = &info
	f.mu.Unlock()

	return catalog, info, nil
}

// Reset forgets the current catalog so the next Fetch is unconditional.
func (f *Fetcher) Reset() {
	f.mu.Lock()
	f.current = nil
	f.mu.Unlock()
}

// Fetch downloads the catalog. Once a catalog has been loaded it sends
// If-None-Match / If-Modified-Since, and an unchanged upstream returns a
// Result with NotModified set and no Catalog.
func (f *Fetcher) Fetch(ctx context.Context) (Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return Result{}, err
	}
	if f.UserAgent != "" {
		req.Header.Set("User-Agent", f.UserAgent)
	}

	f.mu.Lock()
	var previous *Info
	if f.current != nil {
		p := *f.current
		previous = &p
	}
	f.mu.Unlock()

	if previous != nil {
		if previous.ETag != "" {
			req.Header.Set("If-None-Match", previous.ETag)
		}
		if previous.LastModified != "" {
			req.Header.Set("If-Modified-Since", previous.LastModified)
		}
	}

	client := f.Client
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotModified && previous != nil:
		return Result{Info: *previous, NotModified: true}, nil
	case resp.StatusCode != http.StatusOK:
		return Result{}, fmt.Errorf("catalog download: HTTP %d", resp.StatusCode)
	}

	catalog, info, n, err := f.store(resp)
	if err != nil {
		return Result{Downloaded: n}, err
	}

	f.mu.Lock()
	f.current = &info
	f.mu.Unlock()

	return Result{Catalog: catalog, Info: info, Downloaded: n}, nil
}

// store streams the body to a temporary file, parses it, and only then
// replaces the persisted copy, so a truncated or corrupt download never
// overwrites a good cache.
func (f *Fetcher) store(resp *http.Response) (*Catalog, Info, int64, error) {
	dir := f.CacheDir
	if dir == "" {
		dir = os.TempDir()
	}

	tmp, err := os.CreateTemp(dir, ".catalog-*.tmp")
	if err != nil {
		return nil, Info{}, 0, err
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name()) // no-op after a successful rename
	}()

	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(resp.Body, MaxSize+1))
	if err != nil {
		return nil, Info{}, n, fmt.Errorf("catalog download: %w", err)
	}
	if n > MaxSize {
		return nil, Info{}, n, fmt.Errorf("catalog download: larger than %d bytes", MaxSize)
	}

	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, Info{}, n, err
	}
	catalog, err := Parse(tmp)
	if err != nil {
		return nil, Info{}, n, err
	}

	info := Info{
		URL:          f.URL,
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		SHA256:       hex.EncodeToString(hash.Sum(nil)),
		Size:         n,
		FetchedAt:    time.Now().UTC(),
	}

	if f.CacheDir == "" {
		return catalog, info, n, nil
	}

	if err := tmp.Sync(); err != nil {
		return nil, Info{}, n, err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(f.CacheDir, cacheFile)); err != nil {
		return nil, Info{}, n, err
	}
	if err := writeJSON(filepath.Join(f.CacheDir, metaFile), info); err != nil {
		return nil, Info{}, n, err
	}

	return catalog, info, n, nil
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
