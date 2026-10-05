package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	defaultOSVBase  = "https://api.osv.dev"
	defaultCacheTTL = 6 * time.Hour
	maxCacheEntries = 10000
	maxOSVPages     = 10
)

// Vuln is one OSV advisory affecting a package version.
type Vuln struct {
	ID       string   `json:"id"`
	Summary  string   `json:"summary,omitempty"`
	Severity string   `json:"severity"` // low|moderate|high|critical|unknown
	Aliases  []string `json:"aliases,omitempty"`
	FixedIn  []string `json:"fixed_in,omitempty"`
	URL      string   `json:"url,omitempty"`
}

type osvClient struct {
	base   string
	client *http.Client
	ttl    time.Duration
	now    func() time.Time

	mu     sync.Mutex
	cache  map[string]cacheEntry
	warned map[string]struct{}
}

type cacheEntry struct {
	vulns   []Vuln
	expires time.Time
}

func newOSVClient(opts Options) *osvClient {
	base := strings.TrimRight(opts.OSVBaseURL, "/")
	if base == "" {
		base = defaultOSVBase
	}
	c := opts.HTTPClient
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	ttl := opts.CacheTTL
	if ttl <= 0 {
		ttl = defaultCacheTTL
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &osvClient{
		base:   base,
		client: c,
		ttl:    ttl,
		now:    now,
		cache:  make(map[string]cacheEntry),
		warned: make(map[string]struct{}),
	}
}

// query returns the advisories for (eco, name, version), cached. On
// error the result is not cached and a warning is logged once per key.
func (c *osvClient) query(ctx context.Context, eco, name, version string) ([]Vuln, error) {
	name = OSVPackageName(eco, name)
	version = OSVVersion(eco, version)
	key := eco + "\x00" + name + "\x00" + version
	now := c.now()

	c.mu.Lock()
	if e, ok := c.cache[key]; ok && now.Before(e.expires) {
		c.mu.Unlock()
		return e.vulns, nil
	}
	c.mu.Unlock()

	vulns, err := c.fetch(ctx, eco, name, version)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		if _, ok := c.warned[key]; !ok {
			if len(c.warned) >= maxCacheEntries {
				clear(c.warned)
			}
			c.warned[key] = struct{}{}
			slog.Warn("policy: OSV query failed, allowing", "ecosystem", eco, "package", name, "version", version, "error", err)
		}
		return nil, err
	}
	delete(c.warned, key)
	if len(c.cache) >= maxCacheEntries {
		c.evict(now)
	}
	c.cache[key] = cacheEntry{vulns: vulns, expires: now.Add(c.ttl)}
	return vulns, nil
}

// evict drops expired entries, then arbitrary ones until 10% headroom
// exists. Caller holds mu.
func (c *osvClient) evict(now time.Time) {
	for k, e := range c.cache {
		if !now.Before(e.expires) {
			delete(c.cache, k)
		}
	}
	target := maxCacheEntries * 9 / 10
	for k := range c.cache {
		if len(c.cache) <= target {
			break
		}
		delete(c.cache, k)
	}
}

type osvQuery struct {
	Package   osvPackage `json:"package"`
	Version   string     `json:"version"`
	PageToken string     `json:"page_token,omitempty"`
}

type osvPackage struct {
	Name      string `json:"name"`
	Ecosystem string `json:"ecosystem"`
}

type osvResponse struct {
	Vulns         []osvVuln `json:"vulns"`
	NextPageToken string    `json:"next_page_token"`
}

type osvVuln struct {
	ID       string   `json:"id"`
	Summary  string   `json:"summary"`
	Details  string   `json:"details"`
	Aliases  []string `json:"aliases"`
	Severity []struct {
		Type  string `json:"type"`
		Score string `json:"score"`
	} `json:"severity"`
	Affected []struct {
		Package osvPackage `json:"package"`
		Ranges  []struct {
			Events []map[string]string `json:"events"`
		} `json:"ranges"`
		DatabaseSpecific  map[string]any `json:"database_specific"`
		EcosystemSpecific map[string]any `json:"ecosystem_specific"`
	} `json:"affected"`
	DatabaseSpecific map[string]any `json:"database_specific"`
}

func (c *osvClient) fetch(ctx context.Context, eco, name, version string) ([]Vuln, error) {
	q := osvQuery{Package: osvPackage{Name: name, Ecosystem: eco}, Version: version}
	var out []Vuln
	for page := 0; page < maxOSVPages; page++ {
		body, err := json.Marshal(q)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/query", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		resp, err := c.client.Do(req)
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("osv: %s", resp.Status)
		}
		var r osvResponse
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, fmt.Errorf("osv: decode: %w", err)
		}
		for i := range r.Vulns {
			out = append(out, convertVuln(&r.Vulns[i], name))
		}
		if r.NextPageToken == "" {
			break
		}
		q.PageToken = r.NextPageToken
	}
	return out, nil
}

func convertVuln(v *osvVuln, name string) Vuln {
	out := Vuln{
		ID:      v.ID,
		Summary: v.Summary,
		Aliases: v.Aliases,
		URL:     "https://osv.dev/vulnerability/" + v.ID,
	}
	if out.Summary == "" {
		out.Summary = firstLine(v.Details)
	}
	out.Severity = vulnSeverity(v)
	seen := map[string]bool{}
	for _, a := range v.Affected {
		if a.Package.Name != "" && !strings.EqualFold(a.Package.Name, name) {
			continue
		}
		for _, r := range a.Ranges {
			for _, ev := range r.Events {
				if f := ev["fixed"]; f != "" && !seen[f] {
					seen[f] = true
					out.FixedIn = append(out.FixedIn, f)
				}
			}
		}
	}
	return out
}

func vulnSeverity(v *osvVuln) string {
	if s, ok := v.DatabaseSpecific["severity"].(string); ok && severityRank(s) > 0 {
		return NormalizeSeverity(s)
	}
	best := 0.0
	for _, s := range v.Severity {
		if score, ok := CVSS3BaseScore(s.Score); ok && score > best {
			best = score
		}
	}
	if best > 0 {
		return SeverityFromScore(best)
	}
	for _, a := range v.Affected {
		for _, m := range []map[string]any{a.DatabaseSpecific, a.EcosystemSpecific} {
			if s, ok := m["severity"].(string); ok && severityRank(s) > 0 {
				return NormalizeSeverity(s)
			}
		}
	}
	return SeverityUnknown
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// blocking returns the advisory ids (plus CVE aliases) that reach
// minSeverity. With an empty (or unrecognized) minSeverity every
// advisory blocks, including ones of unknown severity; otherwise
// unknown-severity advisories are ignored.
func blocking(vulns []Vuln, minSeverity string) []string {
	floor := severityRank(minSeverity)
	var ids []string
	seen := map[string]bool{}
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, v := range vulns {
		if floor > 0 && severityRank(v.Severity) < floor {
			continue
		}
		add(v.ID)
		for _, a := range v.Aliases {
			if strings.HasPrefix(a, "CVE-") {
				add(a)
			}
		}
	}
	return ids
}

var pypiNormRe = regexp.MustCompile(`[-_.]+`)

// OSVPackageName maps a kutu package name to the name OSV expects in
// the given ecosystem.
func OSVPackageName(eco, name string) string {
	switch eco {
	case "PyPI":
		return pypiNormRe.ReplaceAllString(strings.ToLower(name), "-")
	case "Maven":
		if !strings.Contains(name, ":") {
			if i := strings.LastIndexByte(name, '/'); i > 0 {
				return strings.ReplaceAll(name[:i], "/", ".") + ":" + name[i+1:]
			}
		}
	case "Packagist":
		return strings.ToLower(name)
	}
	return name
}

// OSVVersion maps a kutu version string to OSV's form (Go module
// versions drop their "v" prefix).
func OSVVersion(eco, version string) string {
	if eco == "Go" {
		return strings.TrimPrefix(version, "v")
	}
	return version
}
