package ops

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/rawfs/localfs"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/service"
)

// ── fakes ──

type fakeReg struct {
	ns, name, typ, kind string

	mu       sync.Mutex
	pkgs     map[string][]string
	times    map[string]map[string]time.Time
	deleted  []string
	promoted []string
	fetched  []string
	delay    time.Duration
}

func newFake(name, typ, kind string) *fakeReg {
	return &fakeReg{ns: "default", name: name, typ: typ, kind: kind, pkgs: map[string][]string{}, times: map[string]map[string]time.Time{}}
}

func (f *fakeReg) Namespace() string { return f.ns }
func (f *fakeReg) Name() string      { return f.name }
func (f *fakeReg) Type() string      { return f.typ }
func (f *fakeReg) Kind() string      { return f.kind }
func (f *fakeReg) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNotFound)
}
func (f *fakeReg) Close() error { return nil }

func (f *fakeReg) ListPackages(ctx context.Context) ([]registry.PackageSummary, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []registry.PackageSummary
	for n, vs := range f.pkgs {
		out = append(out, registry.PackageSummary{Name: n, Versions: append([]string(nil), vs...)})
	}
	return out, nil
}

func (f *fakeReg) DeleteVersion(_ context.Context, name, version string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	vs := f.pkgs[name]
	i := slices.Index(vs, version)
	if i < 0 {
		return registry.ErrPackageNotFound
	}
	f.pkgs[name] = slices.Delete(vs, i, i+1)
	f.deleted = append(f.deleted, name+"@"+version)
	return nil
}

func (f *fakeReg) PackageDetail(_ context.Context, name string) (*registry.PackageDetail, error) {
	ts, ok := f.times[name]
	if !ok {
		return nil, registry.ErrPackageNotFound
	}
	g := &registry.GenericPackageDetail{}
	for v, t := range ts {
		g.Versions = append(g.Versions, registry.GenericVersionDetail{Version: v, PublishedAt: t.Format(time.RFC3339)})
	}
	return &registry.PackageDetail{Type: f.typ, Name: name, Generic: g}, nil
}

func (f *fakeReg) PromoteVersion(_ context.Context, dst registry.Registry, name, version string) error {
	if !slices.Contains(f.pkgs[name], version) {
		return registry.ErrPackageNotFound
	}
	d := dst.(*fakeReg)
	d.pkgs[name] = append(d.pkgs[name], version)
	f.promoted = append(f.promoted, name+"@"+version)
	return nil
}

func (f *fakeReg) Prefetch(_ context.Context, name, version string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if name == "bad" {
		return errors.New("boom")
	}
	f.fetched = append(f.fetched, name+"|"+version)
	return nil
}

// ── search ──

func TestSearch(t *testing.T) {
	a := newFake("a", "npm", "local")
	a.pkgs["lodash"] = []string{"1.0.0", "4.17.21", "2.0.0"}
	a.pkgs["lodash-es"] = []string{"4.0.0"}
	a.pkgs["my-lodash"] = []string{"0.1.0"}
	a.pkgs["react"] = []string{"18.0.0"}
	b := newFake("b", "npm", "remote")
	b.pkgs["LODASH"] = []string{"3.0.0"}
	v := newFake("v", "npm", "virtual")
	v.pkgs["lodash"] = []string{"9.9.9"}
	slow := newFake("slow", "npm", "local")
	slow.pkgs["lodash-slow"] = []string{"1.0.0"}
	slow.delay = 10 * time.Second

	start := time.Now()
	hits := Search(context.Background(), []registry.Registry{a, b, v, slow}, "Lodash", 0)
	if time.Since(start) > 8*time.Second {
		t.Fatal("search did not honor timeout")
	}

	var got []string
	for _, h := range hits {
		got = append(got, h.Repo+":"+h.Name)
		if h.Kind == "virtual" {
			t.Fatal("virtual repos must be skipped")
		}
	}
	want := []string{"b:LODASH", "a:lodash", "a:lodash-es", "a:my-lodash"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if hits[1].Latest != "4.17.21" || hits[1].Versions[0] != "4.17.21" {
		t.Fatalf("latest: %+v", hits[1])
	}

	if hits := Search(context.Background(), []registry.Registry{a}, "lodash", 2); len(hits) != 2 {
		t.Fatalf("limit: %d", len(hits))
	}
}

// ── retention ──

func TestRetentionKeepLast(t *testing.T) {
	r := newFake("l", "generic", "local")
	r.pkgs["app"] = []string{"1.0.0", "1.2.0", "1.10.0", "2.0.0", "0.9.0"}
	r.pkgs["lib"] = []string{"1.0.0"}

	plan, err := PlanRetention(context.Background(), r, service.RegistryRetentionPolicy{KeepLastVersions: 2, KeepPatterns: []string{"0.*"}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range plan.Deletions {
		got = append(got, d.Name+"@"+d.Version)
	}
	slices.Sort(got)
	want := []string{"app@1.0.0", "app@1.2.0"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}

	n, errs := ApplyRetention(context.Background(), r, plan)
	if n != 2 || len(errs) != 0 {
		t.Fatalf("apply: %d %v", n, errs)
	}
	if vs := r.pkgs["app"]; len(vs) != 3 {
		t.Fatalf("remaining %v", vs)
	}
	// Re-applying is idempotent (missing versions are not errors).
	if n, errs := ApplyRetention(context.Background(), r, plan); n != 0 || len(errs) != 0 {
		t.Fatalf("reapply: %d %v", n, errs)
	}
}

func TestRetentionMaxAge(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	r := newFake("l", "generic", "local")
	r.pkgs["app"] = []string{"1.0.0", "2.0.0", "3.0.0", "4.0.0", "5.0.0"}
	r.times["app"] = map[string]time.Time{
		"1.0.0": now.AddDate(0, 0, -100),
		"2.0.0": now.AddDate(0, 0, -50),
		"3.0.0": now.AddDate(0, 0, -5),
		// 4.0.0 unknown → kept
		"5.0.0": now.AddDate(0, 0, -400), // newest: always kept
	}
	r.pkgs["old"] = []string{"1.0.0"}
	r.times["old"] = map[string]time.Time{"1.0.0": now.AddDate(-5, 0, 0)}

	plan, err := PlanRetention(context.Background(), r, service.RegistryRetentionPolicy{MaxVersionAgeDays: 30, KeepPatterns: []string{"2.*"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Deletions) != 1 || plan.Deletions[0].Version != "1.0.0" || plan.Deletions[0].Name != "app" {
		t.Fatalf("plan %+v", plan)
	}
	if !strings.Contains(plan.Deletions[0].Reason, "30 days") {
		t.Fatalf("reason %q", plan.Deletions[0].Reason)
	}
}

func TestRetentionRejectsNonLocal(t *testing.T) {
	if _, err := PlanRetention(context.Background(), newFake("r", "npm", "remote"), service.RegistryRetentionPolicy{KeepLastVersions: 1}, time.Now()); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("err %v", err)
	}
	plan, err := PlanRetention(context.Background(), newFake("l", "npm", "local"), service.RegistryRetentionPolicy{}, time.Now())
	if err != nil || len(plan.Deletions) != 0 {
		t.Fatalf("empty policy: %v %v", plan, err)
	}
}

// ── promote / prefetch ──

func TestPromote(t *testing.T) {
	src := newFake("dev", "npm", "local")
	src.pkgs["x"] = []string{"1.0.0"}
	dst := newFake("prod", "npm", "local")

	if err := Promote(context.Background(), src, dst, "x", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(dst.pkgs["x"], "1.0.0") {
		t.Fatal("not promoted")
	}
	if err := Promote(context.Background(), src, dst, "x", "9.9.9"); !errors.Is(err, registry.ErrPackageNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if err := Promote(context.Background(), src, newFake("o", "go", "local"), "x", "1.0.0"); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("type: %v", err)
	}
	if err := Promote(context.Background(), src, newFake("o", "npm", "remote"), "x", "1.0.0"); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("kind: %v", err)
	}
	if err := Promote(context.Background(), src, src, "x", "1.0.0"); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("same: %v", err)
	}
}

func TestSplitPackageSpec(t *testing.T) {
	cases := map[string][2]string{
		"lodash":             {"lodash", ""},
		"lodash@4.17.21":     {"lodash", "4.17.21"},
		"@scope/x":           {"@scope/x", ""},
		"@scope/x@1.0":       {"@scope/x", "1.0"},
		" golang.org/x/mod ": {"golang.org/x/mod", ""},
	}
	for in, want := range cases {
		n, v := SplitPackageSpec(in)
		if n != want[0] || v != want[1] {
			t.Errorf("%q → %q %q", in, n, v)
		}
	}
}

func TestPrefetchAll(t *testing.T) {
	r := newFake("r", "npm", "remote")
	errs := PrefetchAll(context.Background(), r, []string{"a", "@s/b@2.0", "bad", ""})
	if len(errs) != 1 {
		t.Fatalf("errs %v", errs)
	}
	if !slices.Equal(r.fetched, []string{"a|", "@s/b|2.0"}) {
		t.Fatalf("fetched %v", r.fetched)
	}
	type plain struct{ registry.Registry }
	if errs := PrefetchAll(context.Background(), plain{r}, []string{"a"}); len(errs) != 1 || !errors.Is(errs[0], ErrUnsupported) {
		t.Fatalf("unsupported: %v", errs)
	}
}

// ── export / import ──

func newTree(t *testing.T) (Tree, string) {
	t.Helper()
	dir := t.TempDir()
	fs, err := localfs.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return TreeFromRawFS(fs, "repo"), filepath.Join(dir, "repo")
}

func writeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestExportImportRoundTrip(t *testing.T) {
	src, srcRoot := newTree(t)
	writeFile(t, srcRoot, "pkg/a/1.0.0/a.tgz", "alpha")
	writeFile(t, srcRoot, "pkg/b/2.0.0/b.tgz", strings.Repeat("b", 100000))
	writeFile(t, srcRoot, "index.json", `{"x":1}`)

	var buf bytes.Buffer
	if err := Export(context.Background(), src, ExportMeta{Type: "generic", Namespace: "default", Repo: "r"}, &buf); err != nil {
		t.Fatal(err)
	}

	// Manifest is the first entry.
	gz, err := gzip.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	hdr, err := tr.Next()
	if err != nil || hdr.Name != ManifestName {
		t.Fatalf("first entry %v %v", hdr, err)
	}
	var meta ExportMeta
	if err := json.NewDecoder(tr).Decode(&meta); err != nil {
		t.Fatal(err)
	}
	if meta.Type != "generic" || len(meta.Files) != 3 || meta.Files[0].SHA256 == "" {
		t.Fatalf("meta %+v", meta)
	}

	dst, dstRoot := newTree(t)
	writeFile(t, dstRoot, "index.json", "existing")

	if _, err := Import(context.Background(), dst, bytes.NewReader(buf.Bytes()), "npm", false); !errors.Is(err, ErrInvalidArchive) {
		t.Fatalf("type mismatch: %v", err)
	}

	stats, err := Import(context.Background(), dst, bytes.NewReader(buf.Bytes()), "generic", false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Files != 2 || stats.Skipped != 1 || stats.Bytes != 100005 {
		t.Fatalf("stats %+v", stats)
	}
	if readFile(t, dstRoot, "pkg/a/1.0.0/a.tgz") != "alpha" || readFile(t, dstRoot, "index.json") != "existing" {
		t.Fatal("content mismatch")
	}

	stats, err = Import(context.Background(), dst, bytes.NewReader(buf.Bytes()), "generic", true)
	if err != nil || stats.Files != 3 || stats.Skipped != 0 {
		t.Fatalf("overwrite %+v %v", stats, err)
	}
	if readFile(t, dstRoot, "index.json") != `{"x":1}` {
		t.Fatal("not overwritten")
	}
}

func TestExportEmptyTree(t *testing.T) {
	src, _ := newTree(t)
	var buf bytes.Buffer
	if err := Export(context.Background(), src, ExportMeta{Type: "go"}, &buf); err != nil {
		t.Fatal(err)
	}
	dst, _ := newTree(t)
	stats, err := Import(context.Background(), dst, &buf, "go", false)
	if err != nil || stats.Files != 0 {
		t.Fatalf("%+v %v", stats, err)
	}
}

// craft builds an archive from a manifest and raw entries.
func craft(t *testing.T, meta ExportMeta, entries map[string]string, order []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	m, _ := json.Marshal(meta)
	tw.WriteHeader(&tar.Header{Name: ManifestName, Mode: 0o644, Size: int64(len(m))})
	tw.Write(m)
	for _, name := range order {
		body := entries[name]
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestImportRejectsTraversal(t *testing.T) {
	const sha = "8ed3f6ad685b959ead7022518e1af76cd816f8e8ec7ccdda1ed4018e8f2223f8" // sha256("evil")
	for _, p := range []string{"../escape", "a/../../escape", "/etc/passwd", "a/./b", "a\\..\\b"} {
		arc := craft(t, ExportMeta{Type: "generic", Files: []ExportFile{{Path: p, Size: 4, SHA256: sha}}},
			map[string]string{"data/" + p: "evil"}, []string{"data/" + p})
		dst, _ := newTree(t)
		if _, err := Import(context.Background(), dst, bytes.NewReader(arc), "generic", true); !errors.Is(err, ErrInvalidArchive) {
			t.Errorf("%q accepted: %v", p, err)
		}
	}

	// Entry path not in manifest.
	arc := craft(t, ExportMeta{Type: "generic", Files: []ExportFile{{Path: "ok", Size: 4, SHA256: sha}}},
		map[string]string{"data/../x": "evil"}, []string{"data/../x"})
	dst, root := newTree(t)
	if _, err := Import(context.Background(), dst, bytes.NewReader(arc), "generic", true); !errors.Is(err, ErrInvalidArchive) {
		t.Fatalf("entry traversal accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "x")); err == nil {
		t.Fatal("escaped file written")
	}
}

func TestImportRejectsChecksumMismatch(t *testing.T) {
	const sha = "8ed3f6ad685b959ead7022518e1af76cd816f8e8ec7ccdda1ed4018e8f2223f8"
	arc := craft(t, ExportMeta{Type: "generic", Files: []ExportFile{{Path: "a.bin", Size: 4, SHA256: sha}}},
		map[string]string{"data/a.bin": "good"}, []string{"data/a.bin"})
	dst, root := newTree(t)
	if _, err := Import(context.Background(), dst, bytes.NewReader(arc), "generic", true); !errors.Is(err, ErrInvalidArchive) || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("sha mismatch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "a.bin")); err == nil {
		t.Fatal("corrupt file written")
	}

	// Missing file listed in manifest.
	arc = craft(t, ExportMeta{Type: "generic", Files: []ExportFile{{Path: "a.bin", Size: 4, SHA256: sha}}}, nil, nil)
	if _, err := Import(context.Background(), dst, bytes.NewReader(arc), "generic", true); !errors.Is(err, ErrInvalidArchive) {
		t.Fatalf("missing: %v", err)
	}

	if _, err := Import(context.Background(), dst, strings.NewReader("not gzip"), "", true); !errors.Is(err, ErrInvalidArchive) {
		t.Fatalf("garbage: %v", err)
	}
}

func TestExportSince(t *testing.T) {
	src, root := newTree(t)
	writeFile(t, root, "old.txt", "old")
	writeFile(t, root, "new.txt", "new")
	cut := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(root, "old.txt"), cut.Add(-time.Hour), cut.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := ExportSince(context.Background(), src, ExportMeta{Type: "generic"}, cut, &buf); err != nil {
		t.Fatal(err)
	}
	dst, dstRoot := newTree(t)
	stats, err := Import(context.Background(), dst, &buf, "generic", false)
	if err != nil || stats.Files != 1 {
		t.Fatalf("%+v %v", stats, err)
	}
	if readFile(t, dstRoot, "new.txt") != "new" {
		t.Fatal("new missing")
	}
	if _, err := os.Stat(filepath.Join(dstRoot, "old.txt")); err == nil {
		t.Fatal("old file exported")
	}
}

type onlyRead struct{ rawfs.RawFS }

func TestTreeReadOnly(t *testing.T) {
	fs, _ := localfs.New(t.TempDir())
	tree := TreeFromRawFS(onlyRead{fs}, "x")
	if err := tree.Write("a", strings.NewReader("a"), 1); err == nil {
		t.Fatal("write on read-only fs succeeded")
	}
}

// ── replication ──

func TestReplicate(t *testing.T) {
	src, srcRoot := newTree(t)
	writeFile(t, srcRoot, "a.txt", "A")
	writeFile(t, srcRoot, "sub/b.txt", "B")

	var gotAuth, gotSince atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		gotSince.Store(r.URL.Query().Get("since"))
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var since time.Time
		if s := r.URL.Query().Get("since"); s != "" {
			since, _ = time.Parse(time.RFC3339, s)
		}
		w.Header().Set("Content-Type", "application/gzip")
		if err := ExportSince(r.Context(), src, ExportMeta{Type: "generic"}, since, w); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()

	dst, dstRoot := newTree(t)
	writeFile(t, dstRoot, "a.txt", "stale")

	rep := service.RegistryReplication{SourceURL: srv.URL + "/export?x=1", Token: "tok"}
	stats, err := Replicate(context.Background(), srv.Client(), rep, dst, "generic", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Files != 2 || readFile(t, dstRoot, "a.txt") != "A" || readFile(t, dstRoot, "sub/b.txt") != "B" {
		t.Fatalf("stats %+v", stats)
	}
	if gotAuth.Load() != "Bearer tok" || gotSince.Load() != "" {
		t.Fatalf("auth=%v since=%v", gotAuth.Load(), gotSince.Load())
	}

	since := time.Now().Add(time.Hour)
	stats, err = Replicate(context.Background(), srv.Client(), rep, dst, "generic", since)
	if err != nil || stats.Files != 0 {
		t.Fatalf("incremental %+v %v", stats, err)
	}
	if gotSince.Load() != since.UTC().Format(time.RFC3339) {
		t.Fatalf("since=%v", gotSince.Load())
	}

	rep.Token = "wrong"
	if _, err := Replicate(context.Background(), srv.Client(), rep, dst, "generic", time.Time{}); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("auth err: %v", err)
	}
	rep.Token = "tok"
	if _, err := Replicate(context.Background(), srv.Client(), rep, dst, "npm", time.Time{}); !errors.Is(err, ErrInvalidArchive) {
		t.Fatalf("type err: %v", err)
	}
}

// ── scheduler ──

func TestScheduler(t *testing.T) {
	s := NewScheduler()
	var a, b, c atomic.Int32
	s.Set("a", 20*time.Millisecond, func(context.Context) { a.Add(1) })
	s.Set("b", 20*time.Millisecond, func(context.Context) { b.Add(1) })
	s.Set("p", 20*time.Millisecond, func(context.Context) { panic("x") })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()

	waitFor(t, func() bool { return a.Load() >= 2 && b.Load() >= 2 })

	s.Remove("b")
	s.Set("a", 0, nil)
	time.Sleep(30 * time.Millisecond)
	an, bn := a.Load(), b.Load()

	s.Set("c", 10*time.Millisecond, func(context.Context) { c.Add(1) })
	waitFor(t, func() bool { return c.Load() >= 2 })
	time.Sleep(60 * time.Millisecond)
	if a.Load() != an || b.Load() != bn {
		t.Fatalf("removed jobs kept running: a %d→%d b %d→%d", an, a.Load(), bn, b.Load())
	}

	// Replacing a job swaps the function.
	var d atomic.Int32
	s.Set("c", 10*time.Millisecond, func(context.Context) { d.Add(1) })
	waitFor(t, func() bool { return d.Load() >= 1 })
	cn := c.Load()
	time.Sleep(50 * time.Millisecond)
	if c.Load() != cn {
		t.Fatal("replaced job kept running")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}
	if keys := s.Keys(); len(keys) != 2 {
		t.Fatalf("keys %v", keys)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}
