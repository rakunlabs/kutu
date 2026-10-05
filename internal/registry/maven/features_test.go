package maven

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/rawfs/localfs"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/service"
)

func newLocalNamed(t *testing.T, name string) *Local {
	t.Helper()
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatalf("localfs.New: %v", err)
	}
	deps := registry.Deps{MountRawFS: func(string) (rawfs.RawFS, error) { return fs, nil }}
	repo := &service.RegistryRepository{Name: name, Type: service.RegistryTypeMaven, Kind: service.RegistryKindLocal, Mount: "m", AllowPush: true}
	r, err := NewLocalFactory()(context.Background(), deps, "default", repo)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	return r.(*Local)
}

func serve(h http.Handler, method, p string, body []byte) *httptest.ResponseRecorder {
	var rd *bytes.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	} else {
		rd = bytes.NewReader(nil)
	}
	r := httptest.NewRequest(method, p, rd)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func mustPut(t *testing.T, h http.Handler, p string, body []byte) {
	t.Helper()
	if w := serve(h, http.MethodPut, p, body); w.Code != http.StatusCreated {
		t.Fatalf("PUT %s: %d %s", p, w.Code, w.Body.String())
	}
}

func mustGet(t *testing.T, h http.Handler, p string) string {
	t.Helper()
	w := serve(h, http.MethodGet, p, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", p, w.Code, w.Body.String())
	}
	return w.Body.String()
}

func metaVersions(t *testing.T, body string) *metadataDoc {
	t.Helper()
	doc, err := parseMetadata([]byte(body))
	if err != nil {
		t.Fatalf("parse metadata: %v\n%s", err, body)
	}
	if doc.Versioning == nil {
		t.Fatalf("metadata without versioning: %s", body)
	}
	return doc
}

const pomWithLicense = `<?xml version="1.0" encoding="UTF-8"?>
<project xmlns="http://maven.apache.org/POM/4.0.0">
  <modelVersion>4.0.0</modelVersion>
  <groupId>com.example</groupId><artifactId>app</artifactId><version>1.0.0</version>
  <licenses><license><name>Apache-2.0</name></license></licenses>
</project>`

func TestLocalChecksumsGeneratedAndVerified(t *testing.T) {
	l := newLocalNamed(t, "l")
	jar := []byte("jar-bytes")
	p := "/com/example/app/1.0.0/app-1.0.0.jar"
	mustPut(t, l, p, jar)
	for _, algo := range checksumAlgos {
		if got := mustGet(t, l, p+"."+algo); got != checksumHex(algo, jar) {
			t.Fatalf("%s = %q", algo, got)
		}
	}
	// Matching client checksum accepted, mismatch rejected.
	mustPut(t, l, p+".sha1", []byte(checksumHex("sha1", jar)+"  app-1.0.0.jar\n"))
	if w := serve(l, http.MethodPut, p+".md5", []byte("deadbeef")); w.Code != http.StatusBadRequest {
		t.Fatalf("mismatched checksum: %d", w.Code)
	}
	// Missing sidecar is computed on the fly.
	if err := l.store.Delete(cleanRel(p + ".sha512")); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, l, p+".sha512"); got != checksumHex("sha512", jar) {
		t.Fatalf("computed sha512 = %q", got)
	}
	if w := serve(l, http.MethodGet, "/com/example/app/1.0.0/missing.jar.sha1", nil); w.Code != http.StatusNotFound {
		t.Fatalf("checksum of missing file: %d", w.Code)
	}
}

func TestLocalMetadataRegeneratedOnPublishAndDelete(t *testing.T) {
	l := newLocalNamed(t, "l")
	for _, v := range []string{"1.0.0", "2.0.0", "1.5.0", "3.0-SNAPSHOT"} {
		mustPut(t, l, "/com/example/app/"+v+"/app-"+v+".pom", []byte("<project/>"))
	}
	body := mustGet(t, l, "/com/example/app/maven-metadata.xml")
	doc := metaVersions(t, body)
	if doc.GroupID != "com.example" || doc.ArtifactID != "app" {
		t.Fatalf("ids: %+v", doc)
	}
	if got := strings.Join(doc.versions(), ","); got != "1.0.0,1.5.0,2.0.0,3.0-SNAPSHOT" {
		t.Fatalf("versions = %s", got)
	}
	if doc.Versioning.Release != "2.0.0" || doc.Versioning.Latest != "3.0-SNAPSHOT" || len(doc.Versioning.LastUpdated) != 14 {
		t.Fatalf("versioning = %+v", doc.Versioning)
	}
	if got := mustGet(t, l, "/com/example/app/maven-metadata.xml.sha1"); got != checksumHex("sha1", []byte(body)) {
		t.Fatalf("metadata sha1 mismatch")
	}

	// Client-uploaded metadata that would drop versions is merged.
	mustPut(t, l, "/com/example/app/maven-metadata.xml", []byte(`<metadata><groupId>com.example</groupId><artifactId>app</artifactId><versioning><release>1.0.0</release><versions><version>1.0.0</version></versions></versioning></metadata>`))
	doc = metaVersions(t, mustGet(t, l, "/com/example/app/maven-metadata.xml"))
	if len(doc.versions()) != 4 || doc.Versioning.Release != "2.0.0" {
		t.Fatalf("merged metadata dropped versions: %+v", doc.Versioning)
	}

	if err := l.DeleteVersion(context.Background(), "com.example:app", "2.0.0"); err != nil {
		t.Fatalf("DeleteVersion: %v", err)
	}
	doc = metaVersions(t, mustGet(t, l, "/com/example/app/maven-metadata.xml"))
	if got := strings.Join(doc.versions(), ","); got != "1.0.0,1.5.0,3.0-SNAPSHOT" || doc.Versioning.Release != "1.5.0" {
		t.Fatalf("after delete: %s release=%s", got, doc.Versioning.Release)
	}
	if err := l.DeleteVersion(context.Background(), "com.example:app", "9.9.9"); err != registry.ErrPackageNotFound {
		t.Fatalf("delete missing: %v", err)
	}

	// HTTP DELETE of the last artifact file of a version regenerates too.
	if w := serve(l, http.MethodDelete, "/com/example/app/1.5.0/app-1.5.0.pom", nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete file: %d", w.Code)
	}
	doc = metaVersions(t, mustGet(t, l, "/com/example/app/maven-metadata.xml"))
	if got := strings.Join(doc.versions(), ","); got != "1.0.0,3.0-SNAPSHOT" {
		t.Fatalf("after file delete: %s", got)
	}
}

func TestLocalSnapshots(t *testing.T) {
	l := newLocalNamed(t, "l")
	dir := "/com/example/app/1.0-SNAPSHOT/"
	for i, ts := range []string{"20240101.120000", "20240102.120000"} {
		n := string(rune('1' + i))
		mustPut(t, l, dir+"app-1.0-"+ts+"-"+n+".jar", []byte("jar"+n))
		mustPut(t, l, dir+"app-1.0-"+ts+"-"+n+".pom", []byte("<project/>"))
		mustPut(t, l, dir+"app-1.0-"+ts+"-"+n+"-sources.jar", []byte("src"+n))
	}
	doc := metaVersions(t, mustGet(t, l, dir+"maven-metadata.xml"))
	if doc.Version != "1.0-SNAPSHOT" || doc.Versioning.Snapshot == nil || doc.Versioning.Snapshot.Timestamp != "20240102.120000" || doc.Versioning.Snapshot.BuildNumber != 2 {
		t.Fatalf("snapshot: %+v", doc.Versioning.Snapshot)
	}
	if doc.Versioning.SnapshotVersions == nil || len(doc.Versioning.SnapshotVersions.Items) != 3 {
		t.Fatalf("snapshotVersions: %+v", doc.Versioning.SnapshotVersions)
	}
	for _, sv := range doc.Versioning.SnapshotVersions.Items {
		if sv.Value != "1.0-20240102.120000-2" || sv.Updated != "20240102120000" {
			t.Fatalf("snapshotVersion: %+v", sv)
		}
	}
	if got := mustGet(t, l, dir+"app-1.0-SNAPSHOT.jar"); got != "jar2" {
		t.Fatalf("alias resolved to %q", got)
	}
	if got := mustGet(t, l, dir+"app-1.0-SNAPSHOT-sources.jar"); got != "src2" {
		t.Fatalf("classifier alias resolved to %q", got)
	}
	if got := mustGet(t, l, dir+"app-1.0-SNAPSHOT.jar.sha1"); got != checksumHex("sha1", []byte("jar2")) {
		t.Fatalf("alias checksum %q", got)
	}
	art := metaVersions(t, mustGet(t, l, "/com/example/app/maven-metadata.xml"))
	if got := strings.Join(art.versions(), ","); got != "1.0-SNAPSHOT" || art.Versioning.Release != "" {
		t.Fatalf("artifact metadata: %+v", art.Versioning)
	}
}

func TestLocalSnapshotRetention(t *testing.T) {
	l := newLocalNamed(t, "l")
	dir := "/com/example/app/1.0-SNAPSHOT/"
	total := SnapshotBuildsToKeep + 3
	for i := 1; i <= total; i++ {
		ts := "20240101.1200" + pad2(i)
		mustPut(t, l, dir+"app-1.0-"+ts+"-"+itoa(i)+".jar", []byte("jar"))
	}
	builds := l.store.snapshotBuilds("com.example", "app", "1.0-SNAPSHOT")
	if len(builds) != SnapshotBuildsToKeep {
		t.Fatalf("kept %d builds", len(builds))
	}
	for _, b := range builds {
		if b.Build <= total-SnapshotBuildsToKeep {
			t.Fatalf("old build %d kept", b.Build)
		}
	}
	if l.store.Exists("com/example/app/1.0-SNAPSHOT/app-1.0-20240101.120001-1.jar.sha1") {
		t.Fatalf("pruned build checksum left behind")
	}
}

func pad2(i int) string {
	s := itoa(i)
	if len(s) < 2 {
		s = "0" + s
	}
	return s
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func TestLocalExtensions(t *testing.T) {
	ctx := context.Background()
	src := newLocalNamed(t, "src")
	dst := newLocalNamed(t, "dst")
	mustPut(t, src, "/com/example/app/1.0.0/app-1.0.0.pom", []byte(pomWithLicense))
	mustPut(t, src, "/com/example/app/1.0.0/app-1.0.0.jar", []byte("jar"))
	mustPut(t, src, "/org/other/lib/0.1/lib-0.1.jar", []byte("lib"))

	pkgs, err := src.ListPackages(ctx)
	if err != nil || len(pkgs) != 2 || pkgs[0].Name != "com.example:app" || pkgs[0].Versions[0] != "1.0.0" || pkgs[1].Name != "org.other:lib" {
		t.Fatalf("ListPackages: %+v %v", pkgs, err)
	}

	cases := map[string]registry.ArtifactRef{
		"/com/example/app/1.0.0/app-1.0.0.jar":      {Name: "com.example:app", Version: "1.0.0"},
		"/com/example/app/1.0.0/app-1.0.0.jar.sha1": {Name: "com.example:app", Version: "1.0.0"},
		"/com/example/app/maven-metadata.xml":       {Name: "com.example:app"},
		"/com/example/app/maven-metadata.xml.md5":   {Name: "com.example:app"},
	}
	for p, want := range cases {
		got, ok := src.ClassifyRequest(httptest.NewRequest(http.MethodGet, p, nil))
		if !ok || got != want {
			t.Fatalf("classify %s = %+v %v", p, got, ok)
		}
	}
	if _, ok := src.ClassifyRequest(httptest.NewRequest(http.MethodGet, "/", nil)); ok {
		t.Fatalf("root classified")
	}

	meta, err := src.ArtifactInfo(ctx, registry.ArtifactRef{Name: "com.example:app", Version: "1.0.0"})
	if err != nil || meta.License != "Apache-2.0" || meta.PublishedAt.IsZero() {
		t.Fatalf("ArtifactInfo: %+v %v", meta, err)
	}
	if _, err := src.ArtifactInfo(ctx, registry.ArtifactRef{Name: "com.example:app", Version: "9"}); err != registry.ErrPackageNotFound {
		t.Fatalf("ArtifactInfo missing: %v", err)
	}

	if err := src.PromoteVersion(ctx, dst, "com.example:app", "1.0.0"); err != nil {
		t.Fatalf("PromoteVersion: %v", err)
	}
	if got := mustGet(t, dst, "/com/example/app/1.0.0/app-1.0.0.jar"); got != "jar" {
		t.Fatalf("promoted jar %q", got)
	}
	if got := mustGet(t, dst, "/com/example/app/1.0.0/app-1.0.0.jar.sha256"); got != checksumHex("sha256", []byte("jar")) {
		t.Fatalf("promoted checksum %q", got)
	}
	doc := metaVersions(t, mustGet(t, dst, "/com/example/app/maven-metadata.xml"))
	if doc.Versioning.Release != "1.0.0" {
		t.Fatalf("dst metadata %+v", doc.Versioning)
	}
	if err := src.PromoteVersion(ctx, dst, "com.example:app", "2.0.0"); err != registry.ErrPackageNotFound {
		t.Fatalf("promote missing: %v", err)
	}
	if err := src.PromoteVersion(ctx, &Remote{}, "com.example:app", "1.0.0"); err == nil {
		t.Fatalf("promote to remote should fail")
	}
}

// fakeUpstream serves a static file map and records requested paths.
type fakeUpstream struct {
	mu    sync.Mutex
	files map[string]string
	hits  []string
}

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.hits = append(f.hits, r.URL.Path)
	body, ok := f.files[r.URL.Path]
	f.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Last-Modified", "Mon, 01 Jan 2024 12:00:00 GMT")
	_, _ = w.Write([]byte(body))
}

func (f *fakeUpstream) count(p string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, h := range f.hits {
		if h == p {
			n++
		}
	}
	return n
}

func newTestRemote(t *testing.T, url string, ups []service.RegistryUpstream) *Remote {
	t.Helper()
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deps := registry.Deps{MountRawFS: func(string) (rawfs.RawFS, error) { return fs, nil }}
	repo := &service.RegistryRepository{Name: "r", Type: service.RegistryTypeMaven, Kind: service.RegistryKindRemote, Mount: "m", URL: url, Upstreams: ups}
	reg, err := NewRemoteFactory()(context.Background(), deps, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	return reg.(*Remote)
}

func TestRemotePullThroughChecksumsAndRouting(t *testing.T) {
	jar := "central-jar"
	central := &fakeUpstream{files: map[string]string{
		"/org/lib/core/1.0/core-1.0.jar":      jar,
		"/org/lib/core/1.0/core-1.0.jar.sha1": checksumHex("sha1", []byte(jar)),
		"/org/lib/core/1.0/core-1.0.pom":      pomWithLicense,
		"/org/lib/core/maven-metadata.xml":    `<metadata><groupId>org.lib</groupId><artifactId>core</artifactId><versioning><latest>1.0</latest><release>1.0</release><versions><version>1.0</version></versions></versioning></metadata>`,
	}}
	acme := &fakeUpstream{files: map[string]string{
		"/com/acme/x/1.0/x-1.0.jar": "acme-jar",
	}}
	cs, as := httptest.NewServer(central), httptest.NewServer(acme)
	defer cs.Close()
	defer as.Close()
	rr := newTestRemote(t, cs.URL, []service.RegistryUpstream{{Prefix: "com/acme/", URL: as.URL}})

	if got := mustGet(t, rr, "/org/lib/core/1.0/core-1.0.jar"); got != jar {
		t.Fatalf("jar %q", got)
	}
	mustGet(t, rr, "/org/lib/core/1.0/core-1.0.jar")
	if n := central.count("/org/lib/core/1.0/core-1.0.jar"); n != 1 {
		t.Fatalf("jar fetched %d times", n)
	}
	if got := mustGet(t, rr, "/org/lib/core/1.0/core-1.0.jar.sha1"); got != checksumHex("sha1", []byte(jar)) {
		t.Fatalf("sha1 %q", got)
	}
	if got := mustGet(t, rr, "/org/lib/core/1.0/core-1.0.jar.sha512"); got != checksumHex("sha512", []byte(jar)) {
		t.Fatalf("computed sha512 %q", got)
	}
	if got := mustGet(t, rr, "/com/acme/x/1.0/x-1.0.jar"); got != "acme-jar" {
		t.Fatalf("routed %q", got)
	}
	if central.count("/com/acme/x/1.0/x-1.0.jar") != 0 {
		t.Fatalf("prefix-routed request hit the default upstream")
	}
	if w := serve(rr, http.MethodGet, "/org/lib/core/9/core-9.jar", nil); w.Code != http.StatusNotFound {
		t.Fatalf("missing: %d", w.Code)
	}
	if w := serve(rr, http.MethodPut, "/org/lib/core/1.0/core-1.0.jar", []byte("x")); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("put on remote: %d", w.Code)
	}
	if h, _ := rr.ProbeUpstream(context.Background()); h.URL != cs.URL+"/" {
		t.Fatalf("probe url %q", h.URL)
	}

	pkgs, _ := rr.ListPackages(context.Background())
	if len(pkgs) != 2 {
		t.Fatalf("remote ListPackages %+v", pkgs)
	}
	meta, err := rr.ArtifactInfo(context.Background(), registry.ArtifactRef{Name: "org.lib:core", Version: "1.0"})
	if err != nil || meta.License != "Apache-2.0" || meta.PublishedAt.Year() != 2024 {
		t.Fatalf("remote ArtifactInfo %+v %v", meta, err)
	}
}

func TestRemotePrefetch(t *testing.T) {
	up := &fakeUpstream{files: map[string]string{
		"/org/lib/core/maven-metadata.xml": `<metadata><groupId>org.lib</groupId><artifactId>core</artifactId><versioning><latest>2.0</latest><release>2.0</release><versions><version>1.0</version><version>2.0</version></versions></versioning></metadata>`,
		"/org/lib/core/2.0/core-2.0.pom":   "<project/>",
		"/org/lib/core/2.0/core-2.0.jar":   "jar2",
		"/org/lib/core/1.0/core-1.0.pom":   "<project><packaging>pom</packaging></project>",
	}}
	s := httptest.NewServer(up)
	defer s.Close()
	rr := newTestRemote(t, s.URL, nil)
	if err := rr.Prefetch(context.Background(), "org.lib:core", ""); err != nil {
		t.Fatalf("Prefetch: %v", err)
	}
	for _, p := range []string{"org/lib/core/maven-metadata.xml", "org/lib/core/2.0/core-2.0.pom", "org/lib/core/2.0/core-2.0.jar"} {
		if !rr.store.Exists(p) {
			t.Fatalf("%s not cached", p)
		}
	}
	if err := rr.Prefetch(context.Background(), "org.lib:core", "1.0"); err != nil {
		t.Fatalf("Prefetch pom-only: %v", err)
	}
	if err := rr.Prefetch(context.Background(), "org.lib:core", "3.0"); err == nil {
		t.Fatalf("Prefetch of missing version should fail")
	}
}

// TestRemoteGradlePluginPortalMarker covers the plugin-portal layout:
// plugin marker artifacts are plain POM pulls.
func TestRemoteGradlePluginPortalMarker(t *testing.T) {
	marker := `<project><groupId>com.example.hello</groupId><artifactId>com.example.hello.gradle.plugin</artifactId><version>1.2</version><packaging>pom</packaging><dependencies><dependency><groupId>com.example</groupId><artifactId>hello-plugin</artifactId><version>1.2</version></dependency></dependencies></project>`
	up := &fakeUpstream{files: map[string]string{
		"/m2/com/example/hello/com.example.hello.gradle.plugin/1.2/com.example.hello.gradle.plugin-1.2.pom": marker,
	}}
	s := httptest.NewServer(up)
	defer s.Close()
	rr := newTestRemote(t, s.URL+"/m2", nil)
	p := "/com/example/hello/com.example.hello.gradle.plugin/1.2/com.example.hello.gradle.plugin-1.2.pom"
	if got := mustGet(t, rr, p); got != marker {
		t.Fatalf("marker %q", got)
	}
	ref, ok := rr.ClassifyRequest(httptest.NewRequest(http.MethodGet, p, nil))
	if !ok || ref.Name != "com.example.hello:com.example.hello.gradle.plugin" || ref.Version != "1.2" {
		t.Fatalf("classify marker %+v", ref)
	}
}

type mapResolver map[string]registry.Registry

func (m mapResolver) Lookup(_, repo string) (registry.Registry, bool) {
	r, ok := m[repo]
	return r, ok
}

func TestVirtualMergesMetadata(t *testing.T) {
	a := newLocalNamed(t, "a")
	b := newLocalNamed(t, "b")
	mustPut(t, a, "/com/example/app/1.0/app-1.0.jar", []byte("a1"))
	mustPut(t, b, "/com/example/app/2.0/app-2.0.jar", []byte("b2"))
	mustPut(t, b, "/com/example/app/1.0/app-1.0.jar", []byte("b1"))
	mustPut(t, b, "/com/example/app/3.0-SNAPSHOT/app-3.0-20240101.120000-1.jar", []byte("snap"))

	reg, err := NewVirtualFactory(mapResolver{"a": a, "b": b})(context.Background(), registry.Deps{}, "default",
		&service.RegistryRepository{Name: "v", Members: []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	v := reg.(*Virtual)
	body := mustGet(t, v, "/com/example/app/maven-metadata.xml")
	doc := metaVersions(t, body)
	if got := strings.Join(doc.versions(), ","); got != "1.0,2.0,3.0-SNAPSHOT" || doc.Versioning.Release != "2.0" || doc.Versioning.Latest != "3.0-SNAPSHOT" {
		t.Fatalf("merged: %s %+v", got, doc.Versioning)
	}
	if got := mustGet(t, v, "/com/example/app/maven-metadata.xml.sha1"); got != checksumHex("sha1", []byte(body)) {
		t.Fatalf("merged checksum mismatch")
	}
	if got := mustGet(t, v, "/com/example/app/1.0/app-1.0.jar"); got != "a1" {
		t.Fatalf("first hit %q", got)
	}
	if got := mustGet(t, v, "/com/example/app/3.0-SNAPSHOT/app-3.0-SNAPSHOT.jar"); got != "snap" {
		t.Fatalf("snapshot via virtual %q", got)
	}
	if w := serve(v, http.MethodGet, "/com/example/none/maven-metadata.xml", nil); w.Code != http.StatusNotFound {
		t.Fatalf("missing metadata: %d", w.Code)
	}
	if w := serve(v, http.MethodPut, "/com/example/app/1.0/app-1.0.jar", []byte("x")); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("virtual put: %d", w.Code)
	}
	pkgs, _ := v.ListPackages(context.Background())
	if len(pkgs) != 1 || len(pkgs[0].Versions) != 3 {
		t.Fatalf("virtual ListPackages %+v", pkgs)
	}
}
