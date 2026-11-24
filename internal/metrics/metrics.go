// Package metrics is a small, dependency-free Prometheus text exposition of
// the handful of series this service needs.
package metrics

import (
	"fmt"
	"io"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

var buckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}

type histogram struct {
	counts []uint64 // per bucket, non-cumulative
	sum    float64
	count  uint64
}

type requestKey struct{ route, code string }

// Metrics holds every series. The zero value is not usable; call New.
type Metrics struct {
	mu sync.Mutex

	start           time.Time
	refreshes       map[string]uint64 // result -> count
	downloadedBytes uint64
	refreshSeconds  float64
	lastAttempt     time.Time
	lastSuccess     time.Time
	catalogModified time.Time
	components      int
	models          int
	requests        map[requestKey]uint64
	durations       map[string]*histogram
	version         string
}

// New creates an empty registry.
func New(version string) *Metrics {
	return &Metrics{
		start:     time.Now(),
		refreshes: map[string]uint64{"success": 0, "not_modified": 0, "error": 0},
		requests:  make(map[requestKey]uint64),
		durations: make(map[string]*histogram),
		version:   version,
	}
}

// Refresh records one catalog refresh attempt. result is success,
// not_modified or error.
func (m *Metrics) Refresh(result string, took time.Duration, downloaded int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	m.refreshes[result]++
	m.refreshSeconds = took.Seconds()
	m.lastAttempt = now
	if result != "error" {
		m.lastSuccess = now
	}
	if downloaded > 0 {
		m.downloadedBytes += uint64(downloaded)
	}
}

// Catalog records the shape of the catalog being served.
func (m *Metrics) Catalog(modified time.Time, components, models int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.catalogModified = modified
	m.components = components
	m.models = models
}

// Request records one HTTP request. route must come from a fixed set so
// label cardinality stays bounded.
func (m *Metrics) Request(route string, code int, took time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[requestKey{route, fmt.Sprint(code)}]++

	h := m.durations[route]
	if h == nil {
		h = &histogram{counts: make([]uint64, len(buckets))}
		m.durations[route] = h
	}
	s := took.Seconds()
	h.sum += s
	h.count++
	for i, b := range buckets {
		if s <= b {
			h.counts[i]++
			break
		}
	}
}

// WriteTo writes the Prometheus text exposition format.
func (m *Metrics) WriteTo(w io.Writer) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var b strings.Builder
	header := func(name, kind, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
	}
	unix := func(t time.Time) float64 {
		if t.IsZero() {
			return 0
		}
		return float64(t.UnixNano()) / 1e9
	}

	header("dell_rss_build_info", "gauge", "Build information.")
	fmt.Fprintf(&b, "dell_rss_build_info{version=%q,goversion=%q} 1\n", m.version, runtime.Version())

	header("dell_rss_catalog_refreshes_total", "counter", "Catalog refresh attempts by result.")
	for _, result := range sortedKeys(m.refreshes) {
		fmt.Fprintf(&b, "dell_rss_catalog_refreshes_total{result=%q} %d\n", result, m.refreshes[result])
	}

	header("dell_rss_catalog_downloaded_bytes_total", "counter", "Compressed catalog bytes downloaded from Dell.")
	fmt.Fprintf(&b, "dell_rss_catalog_downloaded_bytes_total %d\n", m.downloadedBytes)

	header("dell_rss_catalog_refresh_duration_seconds", "gauge", "Duration of the most recent refresh attempt.")
	fmt.Fprintf(&b, "dell_rss_catalog_refresh_duration_seconds %g\n", m.refreshSeconds)

	header("dell_rss_catalog_last_attempt_timestamp_seconds", "gauge", "Unix time of the most recent refresh attempt.")
	fmt.Fprintf(&b, "dell_rss_catalog_last_attempt_timestamp_seconds %g\n", unix(m.lastAttempt))

	header("dell_rss_catalog_last_success_timestamp_seconds", "gauge", "Unix time of the most recent successful refresh (including not-modified).")
	fmt.Fprintf(&b, "dell_rss_catalog_last_success_timestamp_seconds %g\n", unix(m.lastSuccess))

	header("dell_rss_catalog_modified_timestamp_seconds", "gauge", "Last-Modified time of the catalog being served.")
	fmt.Fprintf(&b, "dell_rss_catalog_modified_timestamp_seconds %g\n", unix(m.catalogModified))

	header("dell_rss_catalog_components", "gauge", "Server update packages in the catalog being served.")
	fmt.Fprintf(&b, "dell_rss_catalog_components %d\n", m.components)

	header("dell_rss_models", "gauge", "Server models discovered in the catalog.")
	fmt.Fprintf(&b, "dell_rss_models %d\n", m.models)

	header("dell_rss_http_requests_total", "counter", "HTTP requests by route and status code.")
	keys := make([]requestKey, 0, len(m.requests))
	for k := range m.requests {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b requestKey) int {
		if c := strings.Compare(a.route, b.route); c != 0 {
			return c
		}
		return strings.Compare(a.code, b.code)
	})
	for _, k := range keys {
		fmt.Fprintf(&b, "dell_rss_http_requests_total{route=%q,code=%q} %d\n", k.route, k.code, m.requests[k])
	}

	header("dell_rss_http_request_duration_seconds", "histogram", "HTTP request latency by route.")
	for _, route := range sortedKeys(m.durations) {
		h := m.durations[route]
		var cumulative uint64
		for i, le := range buckets {
			cumulative += h.counts[i]
			fmt.Fprintf(&b, "dell_rss_http_request_duration_seconds_bucket{route=%q,le=\"%g\"} %d\n", route, le, cumulative)
		}
		fmt.Fprintf(&b, "dell_rss_http_request_duration_seconds_bucket{route=%q,le=\"+Inf\"} %d\n", route, h.count)
		fmt.Fprintf(&b, "dell_rss_http_request_duration_seconds_sum{route=%q} %g\n", route, h.sum)
		fmt.Fprintf(&b, "dell_rss_http_request_duration_seconds_count{route=%q} %d\n", route, h.count)
	}

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	header("process_start_time_seconds", "gauge", "Start time of the process since unix epoch in seconds.")
	fmt.Fprintf(&b, "process_start_time_seconds %g\n", unix(m.start))
	header("go_goroutines", "gauge", "Number of goroutines that currently exist.")
	fmt.Fprintf(&b, "go_goroutines %d\n", runtime.NumGoroutine())
	header("go_memstats_heap_alloc_bytes", "gauge", "Number of heap bytes allocated and still in use.")
	fmt.Fprintf(&b, "go_memstats_heap_alloc_bytes %d\n", mem.HeapAlloc)
	header("go_memstats_sys_bytes", "gauge", "Number of bytes obtained from system.")
	fmt.Fprintf(&b, "go_memstats_sys_bytes %d\n", mem.Sys)

	n, err := io.WriteString(w, b.String())
	return int64(n), err
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
