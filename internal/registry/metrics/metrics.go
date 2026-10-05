// Package metrics keeps in-process counters for the registry data
// plane (requests, bytes served, per-package downloads) and renders
// them in the Prometheus text exposition format.
//
// Counters are process-local and reset on restart; they're meant for
// scraping, not for billing.
package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type repoKey struct{ NS, Repo, Type string }

type repoCounters struct {
	requests [6]atomic.Int64 // by status class 0..5
	bytes    atomic.Int64
	upstream atomic.Int64 // cache misses forwarded to an upstream
}

type downloadKey struct {
	repoKey
	Package string
}

// Registry is the metrics sink.
type Registry struct {
	mu        sync.RWMutex
	repos     map[repoKey]*repoCounters
	downloads map[downloadKey]*atomic.Int64
	started   time.Time
}

// Default is the process-wide sink.
var Default = New()

// New returns an empty sink.
func New() *Registry {
	return &Registry{repos: map[repoKey]*repoCounters{}, downloads: map[downloadKey]*atomic.Int64{}, started: time.Now()}
}

func (m *Registry) repo(k repoKey) *repoCounters {
	m.mu.RLock()
	c := m.repos[k]
	m.mu.RUnlock()
	if c != nil {
		return c
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if c = m.repos[k]; c == nil {
		c = &repoCounters{}
		m.repos[k] = c
	}
	return c
}

// Observe records one served request.
func (m *Registry) Observe(ns, repo, typ string, status int, bytes int64) {
	c := m.repo(repoKey{ns, repo, typ})
	class := status / 100
	if class < 0 || class > 5 {
		class = 0
	}
	c.requests[class].Add(1)
	c.bytes.Add(bytes)
}

// Download records one artifact download of pkg.
func (m *Registry) Download(ns, repo, typ, pkg string) {
	if pkg == "" {
		return
	}
	k := downloadKey{repoKey{ns, repo, typ}, pkg}
	m.mu.RLock()
	c := m.downloads[k]
	m.mu.RUnlock()
	if c == nil {
		m.mu.Lock()
		if c = m.downloads[k]; c == nil {
			c = &atomic.Int64{}
			m.downloads[k] = c
		}
		m.mu.Unlock()
	}
	c.Add(1)
}

// PackageDownloads is one row of TopDownloads.
type PackageDownloads struct {
	Package   string `json:"package"`
	Downloads int64  `json:"downloads"`
}

// RepoSummary is the per-repo counters snapshot.
type RepoSummary struct {
	Requests     int64              `json:"requests"`
	Errors       int64              `json:"errors"`
	BytesServed  int64              `json:"bytes_served"`
	Since        time.Time          `json:"since"`
	TopDownloads []PackageDownloads `json:"top_downloads,omitempty"`
}

// Summary returns the counters for one repo with up to limit top packages.
func (m *Registry) Summary(ns, repo string, limit int) RepoSummary {
	out := RepoSummary{Since: m.started}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for k, c := range m.repos {
		if k.NS != ns || k.Repo != repo {
			continue
		}
		for i := range c.requests {
			n := c.requests[i].Load()
			out.Requests += n
			if i >= 4 {
				out.Errors += n
			}
		}
		out.BytesServed += c.bytes.Load()
	}
	for k, c := range m.downloads {
		if k.NS == ns && k.Repo == repo {
			out.TopDownloads = append(out.TopDownloads, PackageDownloads{Package: k.Package, Downloads: c.Load()})
		}
	}
	sort.Slice(out.TopDownloads, func(i, j int) bool {
		if out.TopDownloads[i].Downloads != out.TopDownloads[j].Downloads {
			return out.TopDownloads[i].Downloads > out.TopDownloads[j].Downloads
		}
		return out.TopDownloads[i].Package < out.TopDownloads[j].Package
	})
	if limit > 0 && len(out.TopDownloads) > limit {
		out.TopDownloads = out.TopDownloads[:limit]
	}
	return out
}

func esc(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

// WritePrometheus renders every counter.
func (m *Registry) WritePrometheus(w io.Writer) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	keys := make([]repoKey, 0, len(m.repos))
	for k := range m.repos {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].NS != keys[j].NS {
			return keys[i].NS < keys[j].NS
		}
		return keys[i].Repo < keys[j].Repo
	})
	fmt.Fprintln(w, "# HELP kutu_registry_requests_total Registry data-plane requests by status class.")
	fmt.Fprintln(w, "# TYPE kutu_registry_requests_total counter")
	for _, k := range keys {
		c := m.repos[k]
		for i := 1; i <= 5; i++ {
			fmt.Fprintf(w, "kutu_registry_requests_total{namespace=\"%s\",repo=\"%s\",type=\"%s\",code=\"%dxx\"} %d\n", esc(k.NS), esc(k.Repo), esc(k.Type), i, c.requests[i].Load())
		}
	}
	fmt.Fprintln(w, "# HELP kutu_registry_response_bytes_total Bytes written by the registry data plane.")
	fmt.Fprintln(w, "# TYPE kutu_registry_response_bytes_total counter")
	for _, k := range keys {
		fmt.Fprintf(w, "kutu_registry_response_bytes_total{namespace=\"%s\",repo=\"%s\",type=\"%s\"} %d\n", esc(k.NS), esc(k.Repo), esc(k.Type), m.repos[k].bytes.Load())
	}
	dkeys := make([]downloadKey, 0, len(m.downloads))
	for k := range m.downloads {
		dkeys = append(dkeys, k)
	}
	sort.Slice(dkeys, func(i, j int) bool {
		a, b := dkeys[i], dkeys[j]
		if a.NS != b.NS {
			return a.NS < b.NS
		}
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		return a.Package < b.Package
	})
	fmt.Fprintln(w, "# HELP kutu_registry_package_downloads_total Artifact downloads per package.")
	fmt.Fprintln(w, "# TYPE kutu_registry_package_downloads_total counter")
	for _, k := range dkeys {
		fmt.Fprintf(w, "kutu_registry_package_downloads_total{namespace=\"%s\",repo=\"%s\",type=\"%s\",package=\"%s\"} %d\n", esc(k.NS), esc(k.Repo), esc(k.Type), esc(k.Package), m.downloads[k].Load())
	}
}

// StatusWriter wraps a ResponseWriter to capture status and bytes.
type StatusWriter struct {
	http.ResponseWriter
	Status int
	Bytes  int64
}

func (s *StatusWriter) WriteHeader(code int) {
	if s.Status == 0 {
		s.Status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *StatusWriter) Write(b []byte) (int, error) {
	if s.Status == 0 {
		s.Status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.Bytes += int64(n)
	return n, err
}

func (s *StatusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *StatusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }
