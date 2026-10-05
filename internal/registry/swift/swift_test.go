package swift

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/rawfs/localfs"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/service"
)

func newLocal(t *testing.T, name string) *Local {
	t.Helper()
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deps := registry.Deps{MountRawFS: func(string) (rawfs.RawFS, error) { return fs, nil }}
	repo := &service.RegistryRepository{Name: name, Type: typ, Kind: service.RegistryKindLocal, Mount: "m", BasePath: "swift", AllowPush: true}
	r, err := NewLocalFactory()(context.Background(), deps, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	return r.(*Local)
}

func makeZip(t *testing.T, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	files := map[string]string{
		"LinkedList/Package.swift":              "// swift-tools-version:5.9\nimport PackageDescription\n// " + version + "\n",
		"LinkedList/Package@swift-5.7.swift":    "// swift-tools-version:5.7\nimport PackageDescription\n",
		"LinkedList/Sources/LinkedList/a.swift": "struct A {}\n",
	}
	for n, c := range files {
		f, _ := zw.Create(n)
		_, _ = f.Write([]byte(c))
	}
	_ = zw.Close()
	return buf.Bytes()
}

func do(h http.Handler, method, p string, body []byte, ct string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, p, bytes.NewReader(body))
	r.Header.Set("Accept", acceptJSON)
	if ct != "" {
		r.Header.Set("Content-Type", ct)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func publish(t *testing.T, h http.Handler, scope, name, version string, archive []byte, metadata string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	hdr := textproto.MIMEHeader{}
	hdr.Set("Content-Disposition", `form-data; name="source-archive"`)
	hdr.Set("Content-Type", "application/zip")
	pw, _ := mw.CreatePart(hdr)
	_, _ = pw.Write(archive)
	if metadata != "" {
		hdr := textproto.MIMEHeader{}
		hdr.Set("Content-Disposition", `form-data; name="metadata"`)
		hdr.Set("Content-Type", "application/json")
		pw, _ := mw.CreatePart(hdr)
		_, _ = pw.Write([]byte(metadata))
	}
	_ = mw.Close()
	return do(h, http.MethodPut, "/"+scope+"/"+name+"/"+version, body.Bytes(), mw.FormDataContentType())
}

func TestSwiftLocalRoundTrip(t *testing.T) {
	l := newLocal(t, "s")
	z1 := makeZip(t, "1.0.0")
	meta := `{"description":"One thing links to another.","repositoryURLs":["https://github.com/mona/LinkedList"]}`
	if w := publish(t, l, "mona", "LinkedList", "1.0.0", z1, meta); w.Code != http.StatusCreated || w.Header().Get("Content-Version") != "1" {
		t.Fatalf("publish: %d %s", w.Code, w.Body)
	}
	if w := publish(t, l, "mona", "LinkedList", "1.1.0", makeZip(t, "1.1.0"), meta); w.Code != http.StatusCreated {
		t.Fatalf("publish 2: %d %s", w.Code, w.Body)
	}
	if w := publish(t, l, "mona", "LinkedList", "1.0.0", z1, ""); w.Code != http.StatusConflict {
		t.Fatalf("duplicate: %d", w.Code)
	}
	if w := publish(t, l, "mona", "Bad", "1.0.0", []byte("nope"), ""); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid archive: %d", w.Code)
	}

	w := do(l, http.MethodGet, "/mona/linkedlist", nil, "")
	if w.Code != 200 || w.Header().Get("Content-Version") != "1" {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	var lst releaseList
	_ = json.Unmarshal(w.Body.Bytes(), &lst)
	if len(lst.Releases) != 2 || !strings.Contains(strings.Join(w.Header()["Link"], ","), `/1.1.0>; rel="latest-version"`) {
		t.Fatalf("list body: %s link=%v", w.Body, w.Header()["Link"])
	}

	w = do(l, http.MethodGet, "/mona/LinkedList/1.0.0", nil, "")
	var info releaseInfo
	_ = json.Unmarshal(w.Body.Bytes(), &info)
	if w.Code != 200 || info.ID != "mona.LinkedList" || info.Resources[0].Checksum != pkgbase.SHA256Hex(z1) {
		t.Fatalf("info: %d %s", w.Code, w.Body)
	}
	if !strings.Contains(strings.Join(w.Header()["Link"], ","), "successor-version") {
		t.Fatalf("info links: %v", w.Header()["Link"])
	}

	w = do(l, http.MethodGet, "/mona/LinkedList/1.0.0/Package.swift", nil, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "swift-tools-version:5.9") || !strings.Contains(w.Header().Get("Link"), `swift-tools-version="5.7"`) {
		t.Fatalf("manifest: %d %s %v", w.Code, w.Body, w.Header()["Link"])
	}
	if w := do(l, http.MethodGet, "/mona/LinkedList/1.0.0/Package.swift?swift-version=5.7", nil, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "5.7") {
		t.Fatalf("versioned manifest: %d", w.Code)
	}
	if w := do(l, http.MethodGet, "/mona/LinkedList/1.0.0/Package.swift?swift-version=4", nil, ""); w.Code != http.StatusSeeOther {
		t.Fatalf("manifest redirect: %d", w.Code)
	}

	w = do(l, http.MethodGet, "/mona/LinkedList/1.0.0.zip", nil, "")
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), z1) || !strings.HasPrefix(w.Header().Get("Digest"), "sha-256=") {
		t.Fatalf("zip: %d %v", w.Code, w.Header())
	}

	w = do(l, http.MethodGet, "/identifiers?url=git@github.com:mona/LinkedList.git", nil, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "mona.LinkedList") {
		t.Fatalf("identifiers: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodGet, "/identifiers?url=https://example.com/x", nil, ""); w.Code != 404 {
		t.Fatalf("identifiers miss: %d", w.Code)
	}
	if w := do(l, http.MethodPost, "/login", nil, ""); w.Code != 200 {
		t.Fatalf("login: %d", w.Code)
	}
	r := httptest.NewRequest(http.MethodGet, "/mona/LinkedList", nil)
	r.Header.Set("Accept", "application/vnd.swift.registry.v2+json")
	rec := httptest.NewRecorder()
	l.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("api version: %d", rec.Code)
	}

	pkgs, _ := l.ListPackages(context.Background())
	if len(pkgs) != 1 || pkgs[0].Name != "mona.linkedlist" || len(pkgs[0].Versions) != 2 {
		t.Fatalf("list packages: %+v", pkgs)
	}
	d, err := l.PackageDetail(context.Background(), "mona.LinkedList")
	if err != nil || d.Generic.LatestVersion != "1.1.0" || d.Generic.Description == "" {
		t.Fatalf("detail: %+v %v", d, err)
	}
	if ai, err := l.ArtifactInfo(context.Background(), registry.ArtifactRef{Name: "mona.linkedlist", Version: "1.0.0"}); err != nil || ai.PublishedAt.IsZero() {
		t.Fatalf("info: %+v %v", ai, err)
	}
	if ref, ok := l.ClassifyRequest(httptest.NewRequest("GET", "/mona/LinkedList/1.0.0.zip", nil)); !ok || ref.Name != "mona.linkedlist" || ref.Version != "1.0.0" {
		t.Fatalf("classify: %+v", ref)
	}

	dst := newLocal(t, "dst")
	if err := l.PromoteVersion(context.Background(), dst, "mona.LinkedList", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if w := do(dst, http.MethodGet, "/mona/LinkedList/1.0.0.zip", nil, ""); w.Code != 200 {
		t.Fatalf("promoted: %d", w.Code)
	}

	if err := l.DeleteVersion(context.Background(), "mona.LinkedList", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	w = do(l, http.MethodGet, "/mona/LinkedList", nil, "")
	if strings.Contains(w.Body.String(), `"1.0.0"`) {
		t.Fatalf("after delete: %s", w.Body)
	}
	if w := do(l, http.MethodGet, "/mona/LinkedList/1.0.0.zip", nil, ""); w.Code != 404 {
		t.Fatalf("deleted zip: %d", w.Code)
	}
}

func TestSwiftRemote(t *testing.T) {
	up := newLocal(t, "up")
	z := makeZip(t, "2.0.0")
	publish(t, up, "acme", "Kit", "2.0.0", z, `{"originalPublicationTime":"2023-02-16T04:00:00Z"}`)
	srv := httptest.NewServer(up)
	defer srv.Close()

	fs, _ := localfs.New(t.TempDir())
	deps := registry.Deps{MountRawFS: func(string) (rawfs.RawFS, error) { return fs, nil }}
	repo := &service.RegistryRepository{Name: "rem", Type: typ, Kind: service.RegistryKindRemote, Mount: "m", BasePath: "c", URL: srv.URL}
	reg, err := NewRemoteFactory()(context.Background(), deps, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	rr := reg.(*Remote)
	w := do(rr, http.MethodGet, "/acme/Kit", nil, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "http://example.com/acme/Kit/2.0.0") || strings.Contains(w.Body.String(), srv.URL) {
		t.Fatalf("remote list: %d %s", w.Code, w.Body)
	}
	if w := do(rr, http.MethodGet, "/acme/Kit/2.0.0", nil, ""); w.Code != 200 {
		t.Fatalf("remote info: %d %s", w.Code, w.Body)
	}
	if w := do(rr, http.MethodGet, "/acme/Kit/2.0.0/Package.swift", nil, ""); w.Code != 200 {
		t.Fatalf("remote manifest: %d", w.Code)
	}
	if w := do(rr, http.MethodGet, "/acme/Kit/2.0.0.zip", nil, ""); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), z) {
		t.Fatalf("remote zip: %d", w.Code)
	}
	srv.Close()
	if w := do(rr, http.MethodGet, "/acme/Kit/2.0.0.zip", nil, ""); w.Code != 200 {
		t.Fatalf("cached zip: %d", w.Code)
	}
	if ai, err := rr.ArtifactInfo(context.Background(), registry.ArtifactRef{Name: "acme.kit", Version: "2.0.0"}); err != nil || ai.PublishedAt.Year() != 2023 {
		t.Fatalf("remote info: %+v %v", ai, err)
	}
	if pkgs, _ := rr.ListPackages(context.Background()); len(pkgs) != 1 || pkgs[0].Name != "acme.kit" {
		t.Fatalf("remote list packages: %+v", pkgs)
	}
	if d, err := rr.PackageDetail(context.Background(), "acme.Kit"); err != nil || d.Generic.LatestVersion != "2.0.0" {
		t.Fatalf("remote detail: %+v %v", d, err)
	}
	if w := publish(t, rr, "acme", "Kit", "3.0.0", z, ""); w.Code != 405 {
		t.Fatalf("remote publish: %d", w.Code)
	}
}

type resolver map[string]registry.Registry

func (r resolver) Lookup(_, name string) (registry.Registry, bool) {
	reg, ok := r[name]
	return reg, ok
}

func TestSwiftVirtualMerge(t *testing.T) {
	a, b := newLocal(t, "a"), newLocal(t, "b")
	publish(t, a, "mona", "LinkedList", "1.0.0", makeZip(t, "1.0.0"), "")
	publish(t, b, "mona", "LinkedList", "1.2.0", makeZip(t, "1.2.0"), "")
	repo := &service.RegistryRepository{Name: "v", Type: typ, Kind: service.RegistryKindVirtual, Members: []string{"a", "b"}}
	reg, err := NewVirtualFactory(resolver{"a": a, "b": b})(context.Background(), registry.Deps{}, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	w := do(reg, http.MethodGet, "/mona/LinkedList", nil, "")
	var lst releaseList
	_ = json.Unmarshal(w.Body.Bytes(), &lst)
	if w.Code != 200 || len(lst.Releases) != 2 || !strings.Contains(w.Header().Get("Link"), "1.2.0") {
		t.Fatalf("virtual list: %d %s", w.Code, w.Body)
	}
	if w := do(reg, http.MethodGet, "/mona/LinkedList/1.2.0.zip", nil, ""); w.Code != 200 {
		t.Fatalf("virtual zip: %d", w.Code)
	}
	if w := publish(t, reg, "mona", "LinkedList", "9.0.0", makeZip(t, "9"), ""); w.Code != 405 {
		t.Fatalf("virtual publish: %d", w.Code)
	}
}
