package pypi

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/rawfs/localfs"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/service"
)

const testMetadata = "Metadata-Version: 2.1\nName: demo\nVersion: 1.0.0\nSummary: A demo\nLicense: MIT\nRequires-Python: >=3.8\n\nlong description\n"

func makeWheel(t *testing.T, metadata string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{
		"demo/__init__.py":                "",
		"demo-1.0.0.dist-info/METADATA":   metadata,
		"demo-1.0.0.dist-info/WHEEL":      "Wheel-Version: 1.0\n",
		"demo-1.0.0.dist-info/RECORD":     "",
		"other/demo-1.0.0.dist-info/META": "x",
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func twineUpload(t *testing.T, h http.Handler, filename string, body []byte, fields map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	fw, _ := mw.CreateFormFile("content", filename)
	_, _ = fw.Write(body)
	_ = mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/legacy/", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func get(h http.Handler, p, accept string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, p, nil)
	r.Header.Set("X-Pika-Registry-Prefix", "/registries/default/pypi-local")
	if accept != "" {
		r.Header.Set("Accept", accept)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func postForm(h http.Handler, p string, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, p, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const (
	whlName   = "demo-1.0.0-py3-none-any.whl"
	sdistName = "demo-2.0.0.tar.gz"
)

type simpleFile = struct {
	Filename       string            `json:"filename"`
	URL            string            `json:"url"`
	Hashes         map[string]string `json:"hashes"`
	RequiresPython string            `json:"requires-python"`
	Yanked         any               `json:"yanked"`
	CoreMetadata   any               `json:"core-metadata"`
	DistInfo       any               `json:"dist-info-metadata"`
	Size           int64             `json:"size"`
	UploadTime     string            `json:"upload-time"`
}

func (p simpleJSON) file(t *testing.T, name string) simpleFile {
	t.Helper()
	for _, f := range p.Files {
		if f.Filename == name {
			return f
		}
	}
	t.Fatalf("file %s not in %+v", name, p.Files)
	return simpleFile{}
}

type simpleJSON struct {
	Meta     map[string]string `json:"meta"`
	Name     string            `json:"name"`
	Projects []struct {
		Name string `json:"name"`
	} `json:"projects"`
	Files    []simpleFile `json:"files"`
	Versions []string     `json:"versions"`
}

func decodeSimple(t *testing.T, w *httptest.ResponseRecorder) simpleJSON {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != mediaJSON {
		t.Fatalf("content-type %q", ct)
	}
	var out simpleJSON
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v: %s", err, w.Body)
	}
	if out.Meta["api-version"] != apiVersion {
		t.Fatalf("meta: %+v", out.Meta)
	}
	return out
}

func publishDemo(t *testing.T, l *Local) []byte {
	t.Helper()
	whl := makeWheel(t, testMetadata)
	w := twineUpload(t, l, "demo-1.0.0-py3-none-any.whl", whl, map[string]string{
		":action": "file_upload", "name": "demo", "version": "1.0.0", "requires_python": ">=3.8",
		"sha256_digest": pkgbase.SHA256Hex(whl), "metadata_version": "2.1", "summary": "A demo", "license": "MIT", "filetype": "bdist_wheel",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodPut, "/packages/demo/demo-2.0.0.tar.gz", bytes.NewReader([]byte("sdist2"))); w.Code != http.StatusCreated {
		t.Fatalf("put: %d", w.Code)
	}
	return whl
}

func TestPyPIContentNegotiation(t *testing.T) {
	cases := []struct {
		accept, query string
		want          format
	}{
		{"", "", fmtHTML},
		{"text/html", "", fmtHTML},
		{mediaJSON, "", fmtJSON},
		{mediaV1HTML, "", fmtV1HTML},
		{acceptAll, "", fmtJSON},
		{"text/html, " + mediaJSON + ";q=0.5", "", fmtHTML},
		{mediaJSON, "text/html", fmtHTML},
		{"", mediaJSON, fmtJSON},
		{"application/xml", "", fmtHTML},
	}
	for _, c := range cases {
		target := "/simple/"
		if c.query != "" {
			target += "?format=" + url.QueryEscape(c.query)
		}
		r := httptest.NewRequest(http.MethodGet, target, nil)
		r.Header.Set("Accept", c.accept)
		if got := negotiate(r); got != c.want {
			t.Errorf("accept=%q query=%q: got %v want %v", c.accept, c.query, got, c.want)
		}
	}
}

func TestPyPILocalPEP691AndPEP658(t *testing.T) {
	l := newTestLocal(t)
	whl := publishDemo(t, l)

	root := decodeSimple(t, get(l, "/simple/", mediaJSON))
	if len(root.Projects) != 1 || root.Projects[0].Name != "demo" {
		t.Fatalf("root: %+v", root)
	}
	w := get(l, "/simple/", "")
	if !strings.Contains(w.Body.String(), `href="/registries/default/pypi-local/simple/demo/"`) {
		t.Fatalf("root html: %s", w.Body)
	}

	page := decodeSimple(t, get(l, "/simple/Demo/", mediaJSON))
	if page.Name != "demo" || len(page.Files) != 2 || len(page.Versions) != 2 || page.Versions[1] != "2.0.0" {
		t.Fatalf("page: %+v", page)
	}
	f := page.file(t, whlName)
	if f.Hashes["sha256"] != pkgbase.SHA256Hex(whl) || f.RequiresPython != ">=3.8" || f.Size != int64(len(whl)) || f.UploadTime == "" {
		t.Fatalf("wheel entry: %+v", f)
	}
	cm, ok := f.CoreMetadata.(map[string]any)
	if !ok || cm["sha256"] != pkgbase.SHA256Hex([]byte(testMetadata)) || f.DistInfo == nil {
		t.Fatalf("core-metadata: %+v", f.CoreMetadata)
	}
	if page.file(t, sdistName).CoreMetadata != nil {
		t.Fatalf("sdist should not advertise metadata: %+v", page.Files)
	}

	w = get(l, "/simple/demo/", "text/html")
	body := w.Body.String()
	if w.Header().Get("Vary") != "Accept" || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("headers: %v", w.Header())
	}
	for _, want := range []string{
		"#sha256=" + pkgbase.SHA256Hex(whl),
		`data-requires-python="&gt;=3.8"`,
		`data-core-metadata="sha256=` + pkgbase.SHA256Hex([]byte(testMetadata)) + `"`,
		`data-dist-info-metadata="sha256=`,
		"/registries/default/pypi-local/packages/demo/demo-1.0.0-py3-none-any.whl",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("html missing %q:\n%s", want, body)
		}
	}
	if w := get(l, "/simple/demo/", mediaV1HTML); w.Header().Get("Content-Type") != mediaV1HTML {
		t.Fatalf("v1 html content-type: %q", w.Header().Get("Content-Type"))
	}
	if w := get(l, "/simple/demo/?format="+url.QueryEscape(mediaJSON), ""); w.Header().Get("Content-Type") != mediaJSON {
		t.Fatalf("format override: %q", w.Header().Get("Content-Type"))
	}

	w = get(l, "/packages/demo/demo-1.0.0-py3-none-any.whl.metadata", "")
	if w.Code != http.StatusOK || w.Body.String() != testMetadata {
		t.Fatalf("metadata: %d %q", w.Code, w.Body)
	}

	bad := twineUpload(t, l, "demo-3.0.0.tar.gz", []byte("x"), map[string]string{"name": "demo", "sha256_digest": "00"})
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("digest mismatch accepted: %d", bad.Code)
	}
}

func TestPyPIYank(t *testing.T) {
	l := newTestLocal(t)
	publishDemo(t, l)
	if w := postForm(l, "/yank", url.Values{"name": {"demo"}, "version": {"1.0.0"}, "reason": {"broken"}}); w.Code != http.StatusOK {
		t.Fatalf("yank: %d %s", w.Code, w.Body)
	}
	page := decodeSimple(t, get(l, "/simple/demo/", mediaJSON))
	if page.file(t, whlName).Yanked != "broken" || page.file(t, sdistName).Yanked != nil {
		t.Fatalf("yanked: %+v", page.Files)
	}
	if body := get(l, "/simple/demo/", "").Body.String(); !strings.Contains(body, `data-yanked="broken"`) {
		t.Fatalf("html yank: %s", body)
	}
	if w := get(l, "/packages/demo/demo-1.0.0-py3-none-any.whl", ""); w.Code != http.StatusOK {
		t.Fatalf("yanked download: %d", w.Code)
	}
	if w := postForm(l, "/yank", url.Values{"name": {"demo"}, "version": {"9.9"}}); w.Code != http.StatusNotFound {
		t.Fatalf("yank missing: %d", w.Code)
	}
	if w := postForm(l, "/unyank", url.Values{"name": {"demo"}, "version": {"1.0.0"}}); w.Code != http.StatusOK {
		t.Fatalf("unyank: %d", w.Code)
	}
	page = decodeSimple(t, get(l, "/simple/demo/", mediaJSON))
	if page.file(t, whlName).Yanked != nil {
		t.Fatalf("still yanked: %+v", page.Files)
	}
}

func TestPyPILocalJSONAPI(t *testing.T) {
	l := newTestLocal(t)
	publishDemo(t, l)
	postForm(l, "/yank", url.Values{"name": {"demo"}, "version": {"2.0.0"}})

	w := get(l, "/pypi/demo/json", "")
	var doc jsonDoc
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil || w.Code != 200 {
		t.Fatalf("json api: %d %v %s", w.Code, err, w.Body)
	}
	if doc.Info.Version != "1.0.0" || doc.Info.License != "MIT" || doc.Info.Summary != "A demo" || doc.Info.RequiresPython == nil || *doc.Info.RequiresPython != ">=3.8" {
		t.Fatalf("info: %+v", doc.Info)
	}
	if len(doc.Releases) != 2 || len(doc.URLs) != 1 || !strings.HasSuffix(doc.URLs[0].URL, "/registries/default/pypi-local/packages/demo/demo-1.0.0-py3-none-any.whl") {
		t.Fatalf("doc: %+v", doc)
	}
	w = get(l, "/pypi/demo/2.0.0/json", "")
	doc = jsonDoc{}
	_ = json.Unmarshal(w.Body.Bytes(), &doc)
	if w.Code != 200 || doc.Info.Version != "2.0.0" || !doc.Info.Yanked || doc.Releases != nil {
		t.Fatalf("version json: %d %+v", w.Code, doc)
	}
	if w := get(l, "/pypi/demo/9.0/json", ""); w.Code != http.StatusNotFound {
		t.Fatalf("missing version: %d", w.Code)
	}
}

func TestPyPILocalInterfaces(t *testing.T) {
	ctx := context.Background()
	l := newTestLocal(t)
	publishDemo(t, l)

	pkgs, err := l.ListPackages(ctx)
	if err != nil || len(pkgs) != 1 || pkgs[0].Name != "demo" || len(pkgs[0].Versions) != 2 {
		t.Fatalf("list: %+v %v", pkgs, err)
	}

	cls := []struct {
		path string
		ref  registry.ArtifactRef
		ok   bool
	}{
		{"/packages/demo/demo-1.0.0-py3-none-any.whl", registry.ArtifactRef{Name: "demo", Version: "1.0.0"}, true},
		{"/packages/demo/demo-1.0.0-py3-none-any.whl.metadata", registry.ArtifactRef{Name: "demo"}, true},
		{"/simple/Demo_Pkg/", registry.ArtifactRef{Name: "demo-pkg"}, true},
		{"/pypi/demo/json", registry.ArtifactRef{Name: "demo"}, true},
		{"/simple/", registry.ArtifactRef{}, false},
	}
	for _, c := range cls {
		ref, ok := l.ClassifyRequest(httptest.NewRequest(http.MethodGet, c.path, nil))
		if ok != c.ok || ref != c.ref {
			t.Errorf("classify %s: %+v %v", c.path, ref, ok)
		}
	}

	info, err := l.ArtifactInfo(ctx, registry.ArtifactRef{Name: "demo", Version: "1.0.0"})
	if err != nil || info.License != "MIT" || info.PublishedAt.IsZero() {
		t.Fatalf("info: %+v %v", info, err)
	}
	if _, err := l.ArtifactInfo(ctx, registry.ArtifactRef{Name: "demo", Version: "5"}); err != registry.ErrPackageNotFound {
		t.Fatalf("info missing: %v", err)
	}

	dst := newTestLocal(t)
	if err := l.PromoteVersion(ctx, dst, "demo", "1.0.0"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	page := decodeSimple(t, get(dst, "/simple/demo/", mediaJSON))
	if len(page.Files) != 1 || page.Files[0].RequiresPython != ">=3.8" || page.Files[0].CoreMetadata == nil {
		t.Fatalf("promoted page: %+v", page)
	}
	if w := get(dst, "/packages/demo/demo-1.0.0-py3-none-any.whl.metadata", ""); w.Code != 200 {
		t.Fatalf("promoted metadata: %d", w.Code)
	}
	if err := l.PromoteVersion(ctx, &Remote{}, "demo", "1.0.0"); err == nil {
		t.Fatal("promote to non-local should fail")
	}

	if err := l.DeleteVersion(ctx, "demo", "1.0.0"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := l.DeleteVersion(ctx, "demo", "1.0.0"); err != registry.ErrPackageNotFound {
		t.Fatalf("delete again: %v", err)
	}
	page = decodeSimple(t, get(l, "/simple/demo/", mediaJSON))
	if len(page.Files) != 1 || len(page.Versions) != 1 || page.Versions[0] != "2.0.0" {
		t.Fatalf("after delete: %+v", page)
	}
	if w := get(l, "/packages/demo/demo-1.0.0-py3-none-any.whl.metadata", ""); w.Code != http.StatusNotFound {
		t.Fatalf("metadata should be gone: %d", w.Code)
	}
}

func newTestRemote(t *testing.T, upstreamURL string) *Remote {
	t.Helper()
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deps := registry.Deps{MountRawFS: func(string) (rawfs.RawFS, error) { return fs, nil }}
	repo := &service.RegistryRepository{Name: "pypi-remote", Type: service.RegistryTypePyPI, Kind: service.RegistryKindRemote, Mount: "m", BasePath: "c", URL: upstreamURL}
	r, err := NewRemoteFactory()(context.Background(), deps, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	return r.(*Remote)
}

func TestPyPIRemoteJSONUpstream(t *testing.T) {
	ctx := context.Background()
	up := newTestLocal(t)
	whl := publishDemo(t, up)
	postForm(up, "/yank", url.Values{"name": {"demo"}, "version": {"2.0.0"}, "reason": {"bad"}})
	var accepts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accepts = append(accepts, r.Header.Get("Accept"))
		up.ServeHTTP(w, r)
	}))
	defer srv.Close()
	rr := newTestRemote(t, srv.URL)

	page := decodeSimple(t, get(rr, "/simple/demo/", mediaJSON))
	if len(accepts) == 0 || !strings.Contains(accepts[0], mediaJSON) {
		t.Fatalf("upstream accept: %v", accepts)
	}
	if len(page.Files) != 2 {
		t.Fatalf("remote page: %+v", page)
	}
	f := page.file(t, whlName)
	if !strings.HasPrefix(f.URL, "/registries/default/pypi-local/_remote/") || strings.Contains(f.URL, srv.URL) || !strings.HasSuffix(f.URL, "/demo-1.0.0-py3-none-any.whl") {
		t.Fatalf("url not rewritten: %s", f.URL)
	}
	if f.Hashes["sha256"] != pkgbase.SHA256Hex(whl) || f.RequiresPython != ">=3.8" || f.CoreMetadata == nil || page.file(t, sdistName).Yanked != "bad" {
		t.Fatalf("remote file meta lost: %+v", page.Files)
	}
	html := get(rr, "/simple/demo/", "").Body.String()
	if !strings.Contains(html, `data-yanked="bad"`) || !strings.Contains(html, "#sha256="+pkgbase.SHA256Hex(whl)) || strings.Contains(html, srv.URL) {
		t.Fatalf("remote html: %s", html)
	}

	rel := strings.TrimPrefix(f.URL, "/registries/default/pypi-local")
	if w := get(rr, rel, ""); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), whl) {
		t.Fatalf("remote file: %d", w.Code)
	}
	if w := get(rr, rel+".metadata", ""); w.Code != 200 || w.Body.String() != testMetadata {
		t.Fatalf("remote metadata: %d %q", w.Code, w.Body)
	}
	if ref, ok := rr.ClassifyRequest(httptest.NewRequest(http.MethodGet, rel, nil)); !ok || ref.Name != "demo" || ref.Version != "1.0.0" {
		t.Fatalf("remote classify: %+v %v", ref, ok)
	}
	if ref, ok := rr.ClassifyRequest(httptest.NewRequest(http.MethodGet, rel+".metadata", nil)); !ok || ref.Version != "" {
		t.Fatalf("remote classify metadata: %+v %v", ref, ok)
	}

	w := get(rr, "/pypi/demo/json", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), srv.URL) || strings.Contains(w.Body.String(), "/packages/demo/") || !strings.Contains(w.Body.String(), "/_remote/") {
		t.Fatalf("remote json api: %d %s", w.Code, w.Body)
	}
	info, err := rr.ArtifactInfo(ctx, registry.ArtifactRef{Name: "demo", Version: "1.0.0"})
	if err != nil || info.License != "MIT" || info.PublishedAt.IsZero() {
		t.Fatalf("remote info: %+v %v", info, err)
	}

	pkgs, _ := rr.ListPackages(ctx)
	if len(pkgs) != 1 || pkgs[0].Name != "demo" {
		t.Fatalf("remote list: %+v", pkgs)
	}
	if d, err := rr.PackageDetail(ctx, "demo"); err != nil || d.PyPI.LatestVersion != "2.0.0" {
		t.Fatalf("remote detail: %+v %v", d, err)
	}

	// Prefetch latest (non-yanked → 1.0.0) then serve offline.
	rr2 := newTestRemote(t, srv.URL)
	if err := rr2.Prefetch(ctx, "demo", ""); err != nil {
		t.Fatalf("prefetch: %v", err)
	}
	srv.Close()
	page = decodeSimple(t, get(rr2, "/simple/demo/", mediaJSON))
	rel = strings.TrimPrefix(page.file(t, whlName).URL, "/registries/default/pypi-local")
	if w := get(rr2, rel, ""); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), whl) {
		t.Fatalf("prefetched file: %d", w.Code)
	}
	if w := get(rr2, rel+".metadata", ""); w.Code != 200 {
		t.Fatalf("prefetched metadata: %d", w.Code)
	}
}

func TestPyPIRemoteHTMLUpstream(t *testing.T) {
	sum := pkgbase.SHA256Hex([]byte("legacy"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/simple/legacy/":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><body>
<a href="../../files/legacy-0.1.tar.gz#sha256=` + sum + `" data-requires-python="&gt;=3.6" data-yanked="">legacy-0.1.tar.gz</a>
<a href='/files/legacy-0.2-py3-none-any.whl' data-dist-info-metadata="true">legacy-0.2-py3-none-any.whl</a>
</body></html>`))
		case "/files/legacy-0.1.tar.gz":
			_, _ = w.Write([]byte("legacy"))
		case "/files/legacy-0.2-py3-none-any.whl.metadata":
			_, _ = w.Write([]byte("Name: legacy\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	rr := newTestRemote(t, srv.URL)

	page := decodeSimple(t, get(rr, "/simple/legacy/", mediaJSON))
	if len(page.Files) != 2 || page.Files[0].Hashes["sha256"] != sum || page.Files[0].RequiresPython != ">=3.6" || page.Files[0].Yanked != true || page.Files[1].CoreMetadata != true {
		t.Fatalf("html upstream page: %+v", page)
	}
	rel := strings.TrimPrefix(page.Files[0].URL, "/registries/default/pypi-local")
	if w := get(rr, rel, ""); w.Code != 200 || w.Body.String() != "legacy" {
		t.Fatalf("file: %d %q", w.Code, w.Body)
	}
	rel = strings.TrimPrefix(page.Files[1].URL, "/registries/default/pypi-local")
	if w := get(rr, rel+".metadata", ""); w.Code != 200 || w.Body.String() != "Name: legacy\n" {
		t.Fatalf("metadata: %d %q", w.Code, w.Body)
	}
	if w := get(rr, "/simple/missing/", ""); w.Code != http.StatusNotFound {
		t.Fatalf("missing: %d", w.Code)
	}
}

type testResolver map[string]registry.Registry

func (r testResolver) Lookup(_, name string) (registry.Registry, bool) {
	reg, ok := r[name]
	return reg, ok
}

func TestPyPIVirtualMerge(t *testing.T) {
	a, b := newTestLocal(t), newTestLocal(t)
	do(a, http.MethodPut, "/packages/demo/demo-1.0.0.tar.gz", bytes.NewReader([]byte("from-a")))
	do(b, http.MethodPut, "/packages/demo/demo-1.0.0.tar.gz", bytes.NewReader([]byte("from-b")))
	do(b, http.MethodPut, "/packages/demo/demo-2.0.0.tar.gz", bytes.NewReader([]byte("b2")))
	do(b, http.MethodPut, "/packages/other/other-1.0.tar.gz", bytes.NewReader([]byte("o")))

	repo := &service.RegistryRepository{Name: "virt", Type: service.RegistryTypePyPI, Kind: service.RegistryKindVirtual, Members: []string{"a", "b"}}
	reg, err := NewVirtualFactory(testResolver{"a": a, "b": b})(context.Background(), registry.Deps{}, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	v := reg.(*Virtual)

	page := decodeSimple(t, get(v, "/simple/demo/", mediaJSON))
	if len(page.Files) != 2 || len(page.Versions) != 2 || page.Files[0].Hashes["sha256"] != pkgbase.SHA256Hex([]byte("from-a")) {
		t.Fatalf("merged page: %+v", page)
	}
	html := get(v, "/simple/demo/", "").Body.String()
	if !strings.Contains(html, "demo-2.0.0.tar.gz") || !strings.Contains(html, "#sha256="+pkgbase.SHA256Hex([]byte("from-a"))) {
		t.Fatalf("merged html: %s", html)
	}
	root := decodeSimple(t, get(v, "/simple/", mediaJSON))
	if len(root.Projects) != 2 {
		t.Fatalf("merged root: %+v", root)
	}
	if w := get(v, "/packages/demo/demo-1.0.0.tar.gz", ""); w.Body.String() != "from-a" {
		t.Fatalf("first-hit file: %q", w.Body)
	}
	if w := get(v, "/simple/nope/", ""); w.Code != http.StatusNotFound {
		t.Fatalf("missing: %d", w.Code)
	}
	pkgs, _ := v.ListPackages(context.Background())
	if len(pkgs) != 2 {
		t.Fatalf("virtual list: %+v", pkgs)
	}
}
