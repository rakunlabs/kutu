package policy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/service"
)

func TestMatchName(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"@acme/**", "@acme/foo", true},
		{"@acme/**", "@acme/foo/bar", true},
		{"@acme/**", "@other/foo", false},
		{"com/acme/**", "com/acme/x/y", true},
		{"*", "lodash", true},
		{"*", "@scope/pkg", false},
		{"**", "a/b/c", true},
		{"lod*", "lodash", true},
		{"github.com/*/x", "github.com/acme/x", true},
		{"github.com/**/x", "github.com/a/b/x", true},
		{"github.com/**/x", "github.com/a/b/y", false},
		{"[", "x", false},
		{"", "x", false},
	}
	for _, c := range cases {
		if got := MatchName(c.pattern, c.name); got != c.want {
			t.Errorf("MatchName(%q,%q)=%v want %v", c.pattern, c.name, got, c.want)
		}
	}
}

func TestNameAllowed(t *testing.T) {
	inc := []string{"@acme/**", "lodash"}
	exc := []string{"@acme/secret"}
	cases := map[string]bool{
		"@acme/a":      true,
		"lodash":       true,
		"@acme/secret": false,
		"react":        false,
	}
	for n, want := range cases {
		if got := NameAllowed(inc, exc, n); got != want {
			t.Errorf("%s: got %v", n, got)
		}
	}
	if !NameAllowed(nil, []string{"evil"}, "react") || NameAllowed(nil, []string{"evil"}, "evil") {
		t.Fatal("exclude-only")
	}
}

func TestLicense(t *testing.T) {
	cases := []struct {
		expr            string
		allowed, denied []string
		want            bool
	}{
		{"MIT", []string{"mit"}, nil, true},
		{"GPL-3.0", []string{"MIT"}, nil, false},
		{"(MIT OR GPL-3.0)", []string{"MIT"}, nil, true},
		{"MIT AND GPL-3.0", []string{"MIT"}, nil, false},
		{"MIT AND Apache-2.0", []string{"MIT", "Apache-2.0"}, nil, true},
		{"MIT OR GPL-3.0", nil, []string{"GPL-3.0"}, true},
		{"MIT AND GPL-3.0", nil, []string{"GPL-3.0"}, false},
		{"GPL-2.0-only WITH Classpath-exception-2.0", nil, []string{"GPL-2.0-only"}, false},
		{"GPL-2.0-only WITH Classpath-exception-2.0", []string{"GPL-2.0-only"}, nil, true},
		{"MIT/Apache-2.0", []string{"Apache-2.0"}, nil, true},
		{"(Apache-2.0 OR MIT) AND BSD-3-Clause", []string{"MIT", "BSD-3-Clause"}, nil, true},
		{"(Apache-2.0 OR MIT) AND BSD-3-Clause", []string{"MIT"}, nil, false},
		{"The Apache Software License, Version 2.0", []string{"Apache-2.0"}, nil, true},
		{"GPL-2.0+", nil, []string{"GPL-2.0"}, false},
	}
	for _, c := range cases {
		e := ParseLicense(c.expr)
		if got := LicensePermitted(e, c.allowed, c.denied); got != c.want {
			t.Errorf("%q allowed=%v denied=%v: got %v", c.expr, c.allowed, c.denied, got)
		}
	}
	for _, s := range []string{"", "NOASSERTION", "SEE LICENSE IN LICENSE.md"} {
		if ParseLicense(s) != nil {
			t.Errorf("%q should be unknown", s)
		}
	}
}

func TestCVSS3(t *testing.T) {
	cases := map[string]float64{
		"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H": 9.8,
		"CVSS:3.1/AV:N/AC:L/PR:L/UI:N/S:C/C:L/I:L/A:N": 6.4,
		"CVSS:3.0/AV:N/AC:L/PR:N/UI:R/S:C/C:L/I:L/A:N": 6.1,
		"CVSS:3.1/AV:L/AC:L/PR:L/UI:N/S:U/C:H/I:N/A:N": 5.5,
		"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:C/C:H/I:H/A:H": 10,
		"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:N/A:N": 0,
	}
	for v, want := range cases {
		got, ok := CVSS3BaseScore(v)
		if !ok || got != want {
			t.Errorf("%s: got %v ok=%v want %v", v, got, ok, want)
		}
	}
	if _, ok := CVSS3BaseScore("CVSS:4.0/AV:N/AC:L/AT:N/PR:N/UI:N/VC:H/VI:H/VA:H/SC:N/SI:N/SA:N"); ok {
		t.Error("v4 should not score")
	}
	if SeverityFromScore(9.8) != "critical" || SeverityFromScore(6.4) != "moderate" || SeverityFromScore(7) != "high" || SeverityFromScore(0.1) != "low" {
		t.Error("buckets")
	}
}

func TestOSVPackageName(t *testing.T) {
	if got := OSVPackageName("PyPI", "Foo_Bar.baz"); got != "foo-bar-baz" {
		t.Error(got)
	}
	if got := OSVPackageName("Maven", "org.apache:commons"); got != "org.apache:commons" {
		t.Error(got)
	}
	if got := OSVPackageName("Maven", "org/apache/commons"); got != "org.apache:commons" {
		t.Error(got)
	}
}

// fake registry

type fakeReg struct {
	typ    string
	ref    registry.ArtifactRef
	ok     bool
	meta   registry.ArtifactMeta
	signed bool
}

func (f *fakeReg) Namespace() string                            { return "ns" }
func (f *fakeReg) Name() string                                 { return "repo" }
func (f *fakeReg) Type() string                                 { return f.typ }
func (f *fakeReg) Kind() string                                 { return "remote" }
func (f *fakeReg) ServeHTTP(http.ResponseWriter, *http.Request) {}
func (f *fakeReg) Close() error                                 { return nil }
func (f *fakeReg) ClassifyRequest(*http.Request) (registry.ArtifactRef, bool) {
	return f.ref, f.ok
}
func (f *fakeReg) ArtifactInfo(context.Context, registry.ArtifactRef) (registry.ArtifactMeta, error) {
	return f.meta, nil
}
func (f *fakeReg) HasSignature(context.Context, string, string) (bool, error) { return f.signed, nil }

func osvServer(t *testing.T, calls *atomic.Int32, fail *atomic.Bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if fail != nil && fail.Load() {
			http.Error(w, "boom", 500)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/query" {
			http.NotFound(w, r)
			return
		}
		var q osvQuery
		_ = json.NewDecoder(r.Body).Decode(&q)
		if q.Package.Name == "lodash" && q.Package.Ecosystem == "npm" && q.Version == "4.17.20" {
			_, _ = w.Write([]byte(`{"vulns":[
			 {"id":"GHSA-aaaa","summary":"proto pollution","aliases":["CVE-2021-1"],
			  "database_specific":{"severity":"HIGH"},
			  "affected":[{"package":{"name":"lodash","ecosystem":"npm"},"ranges":[{"events":[{"introduced":"0"},{"fixed":"4.17.21"}]}]}]},
			 {"id":"GHSA-bbbb","summary":"low thing",
			  "severity":[{"type":"CVSS_V3","score":"CVSS:3.1/AV:L/AC:H/PR:H/UI:R/S:U/C:L/I:N/A:N"}]},
			 {"id":"OSV-cccc","details":"no severity\nmore"}
			]}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
}

func TestVulnerabilities(t *testing.T) {
	var calls atomic.Int32
	srv := osvServer(t, &calls, nil)
	defer srv.Close()
	e := New(nil, Options{OSVBaseURL: srv.URL})
	vs, err := e.Vulnerabilities(context.Background(), "npm", "lodash", "4.17.20")
	if err != nil || len(vs) != 3 {
		t.Fatalf("%v %v", vs, err)
	}
	if vs[0].Severity != "high" || vs[0].FixedIn[0] != "4.17.21" || vs[1].Severity != "low" || vs[2].Severity != "unknown" || vs[2].Summary != "no severity" {
		t.Fatalf("%+v", vs)
	}
	_, _ = e.Vulnerabilities(context.Background(), "npm", "lodash", "4.17.20")
	if calls.Load() != 1 {
		t.Fatalf("cache miss: %d calls", calls.Load())
	}
	if vs, _ := e.Vulnerabilities(context.Background(), "docker", "x", "1"); vs != nil {
		t.Fatal("docker has no ecosystem")
	}
}

func TestBlocking(t *testing.T) {
	vs := []Vuln{{ID: "A", Severity: "high", Aliases: []string{"CVE-1", "GHSA-x"}}, {ID: "B", Severity: "low"}, {ID: "C", Severity: "unknown"}}
	if got := strings.Join(blocking(vs, ""), ","); got != "A,CVE-1,B,C" {
		t.Error(got)
	}
	if got := strings.Join(blocking(vs, "medium"), ","); got != "A,CVE-1" {
		t.Error(got)
	}
	if got := blocking(vs, "critical"); len(got) != 0 {
		t.Error(got)
	}
}

func gate(e *Engine, reg registry.Registry, method string) (int, string, bool) {
	return e.Gate(reg, httptest.NewRequest(method, "/x", nil))
}

func TestGate(t *testing.T) {
	var calls atomic.Int32
	var fail atomic.Bool
	srv := osvServer(t, &calls, &fail)
	defer srv.Close()

	now := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	var pol *service.RegistryPolicy
	lookup := func(_ context.Context, ns, repo string) *service.RegistryRepository {
		if ns != "ns" || repo != "repo" {
			return nil
		}
		return &service.RegistryRepository{Name: repo, Policy: pol}
	}
	e := New(lookup, Options{OSVBaseURL: srv.URL, Now: func() time.Time { return now }})

	reg := &fakeReg{typ: "npm", ok: true, ref: registry.ArtifactRef{Name: "lodash", Version: "4.17.20"}}

	// nil policy allows
	if _, _, ok := gate(e, reg, http.MethodGet); !ok {
		t.Fatal("nil policy")
	}

	// include/exclude
	pol = &service.RegistryPolicy{Include: []string{"@acme/**"}}
	if st, msg, ok := gate(e, reg, http.MethodGet); ok || st != 403 || !strings.Contains(msg, "blocked by repository policy") {
		t.Fatalf("include: %d %s", st, msg)
	}
	if _, _, ok := gate(e, reg, http.MethodPut); ok {
		t.Fatal("include must apply to writes")
	}
	reg.ok = false
	if _, _, ok := gate(e, reg, http.MethodGet); !ok {
		t.Fatal("unclassified request must pass")
	}
	reg.ok = true

	// vulnerabilities
	pol = &service.RegistryPolicy{BlockVulnerable: true}
	st, msg, ok := gate(e, reg, http.MethodGet)
	if ok || st != 403 || !strings.Contains(msg, "GHSA-aaaa") || !strings.Contains(msg, "CVE-2021-1") {
		t.Fatalf("vuln: %d %s", st, msg)
	}
	if _, _, ok := gate(e, reg, http.MethodPut); !ok {
		t.Fatal("writes are not vuln-gated")
	}
	pol.MinSeverity = "critical"
	if _, _, ok := gate(e, reg, http.MethodGet); !ok {
		t.Fatal("min severity critical should allow")
	}
	pol.MinSeverity = ""
	reg.ref.Version = "4.17.21"
	if _, _, ok := gate(e, reg, http.MethodGet); !ok {
		t.Fatal("clean version")
	}
	// fail open
	fail.Store(true)
	reg.ref.Version = "9.9.9"
	if _, _, ok := gate(e, reg, http.MethodGet); !ok {
		t.Fatal("OSV errors must fail open")
	}
	fail.Store(false)

	// metadata request (no version) skips version checks
	reg.ref.Version = ""
	if _, _, ok := gate(e, reg, http.MethodGet); !ok {
		t.Fatal("metadata request")
	}
	reg.ref.Version = "1.0.0"

	// quarantine
	pol = &service.RegistryPolicy{QuarantineDays: 7}
	reg.meta = registry.ArtifactMeta{PublishedAt: now.Add(-48 * time.Hour)}
	if st, msg, ok := gate(e, reg, http.MethodGet); ok || !strings.Contains(msg, "quarantined until 2026-10-10") {
		t.Fatalf("quarantine: %d %s", st, msg)
	}
	reg.meta.PublishedAt = now.Add(-8 * 24 * time.Hour)
	if _, _, ok := gate(e, reg, http.MethodGet); !ok {
		t.Fatal("old version")
	}
	reg.meta.PublishedAt = time.Time{}
	if _, _, ok := gate(e, reg, http.MethodGet); !ok {
		t.Fatal("unknown time allows")
	}

	// license
	pol = &service.RegistryPolicy{AllowedLicenses: []string{"MIT"}}
	reg.meta = registry.ArtifactMeta{License: "GPL-3.0"}
	if _, msg, ok := gate(e, reg, http.MethodGet); ok || !strings.Contains(msg, "GPL-3.0") {
		t.Fatalf("license: %s", msg)
	}
	reg.meta.License = "MIT OR GPL-3.0"
	if _, _, ok := gate(e, reg, http.MethodGet); !ok {
		t.Fatal("OR")
	}
	reg.meta.License = ""
	if _, _, ok := gate(e, reg, http.MethodGet); !ok {
		t.Fatal("unknown passes by default")
	}
	pol.BlockUnknownLicense = true
	if _, msg, ok := gate(e, reg, http.MethodGet); ok || !strings.Contains(msg, "unknown") {
		t.Fatalf("unknown: %s", msg)
	}

	// signatures
	dreg := &fakeReg{typ: "docker", ok: true, ref: registry.ArtifactRef{Name: "library/nginx", Version: "latest"}}
	pol = &service.RegistryPolicy{RequireSignature: true}
	if _, msg, ok := gate(e, dreg, http.MethodGet); ok || !strings.Contains(msg, "not signed") {
		t.Fatalf("sig: %s", msg)
	}
	dreg.signed = true
	if _, _, ok := gate(e, dreg, http.MethodGet); !ok {
		t.Fatal("signed")
	}
	dreg.signed = false
	dreg.ref.Version = "sha256:abc"
	if _, _, ok := gate(e, dreg, http.MethodGet); !ok {
		t.Fatal("digest pulls are not signature-gated")
	}

	// Evaluate outside HTTP
	pol = &service.RegistryPolicy{Exclude: []string{"lodash"}}
	if d := e.Evaluate(context.Background(), reg, registry.ArtifactRef{Name: "lodash"}); d.Allowed || d.Status != 403 {
		t.Fatalf("%+v", d)
	}
}
