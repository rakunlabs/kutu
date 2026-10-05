package nuget

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/rawfs/localfs"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/service"
)

const prefix = "/registries/default/nuget"

func buildNupkg(t *testing.T, id, version string) []byte {
	t.Helper()
	nuspec := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<package xmlns="http://schemas.microsoft.com/packaging/2013/05/nuspec.xsd">
  <metadata>
    <id>%s</id>
    <version>%s</version>
    <authors>Jane, John</authors>
    <description>Test package %s</description>
    <license type="expression">MIT</license>
    <licenseUrl>https://aka.ms/deprecateLicenseUrl</licenseUrl>
    <projectUrl>https://example.com/proj</projectUrl>
    <tags>json utility</tags>
    <dependencies>
      <group targetFramework="net8.0">
        <dependency id="Newtonsoft.Json" version="[13.0.1, )" exclude="Build" />
      </group>
    </dependencies>
  </metadata>
</package>`, id, version, id)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{
		id + ".nuspec":              nuspec,
		"lib/net8.0/" + id + ".dll": "dll",
		"[Content_Types].xml":       "<Types/>",
	} {
		f, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newLocal(t *testing.T, name string, policy *service.RegistryPolicy) *Local {
	t.Helper()
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deps := registry.Deps{MountRawFS: func(string) (rawfs.RawFS, error) { return fs, nil }}
	repo := &service.RegistryRepository{Name: name, Type: typ, Kind: service.RegistryKindLocal, Mount: "m", BasePath: "nuget", AllowPush: true, Policy: policy}
	r, err := NewLocalFactory()(context.Background(), deps, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	return r.(*Local)
}

func do(h http.Handler, method, p string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, p, body)
	r.Host = "kutu.test"
	r.Header.Set("X-Pika-Registry-Prefix", prefix)
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func push(t *testing.T, h http.Handler, nupkg []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("package", "package.nupkg")
	_, _ = fw.Write(nupkg)
	_ = mw.Close()
	return do(h, http.MethodPut, "/api/v2/package", &buf, mw.FormDataContentType())
}

func decode(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("decode: %v: %s", err, w.Body)
	}
}

func TestNormalizeVersion(t *testing.T) {
	for in, want := range map[string]string{
		"1":               "1.0.0",
		"1.0":             "1.0.0",
		"01.02.03":        "1.2.3",
		"1.0.0.0":         "1.0.0",
		"1.2.3.4":         "1.2.3.4",
		"1.0.0-Beta+abc":  "1.0.0-Beta",
		"2.0.0-rc.1+meta": "2.0.0-rc.1",
	} {
		if got, ok := NormalizeVersion(in); !ok || got != want {
			t.Errorf("%s: got %q,%v want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "a.b", "1.2.3.4.5", "1..2", "1.0-"} {
		if _, ok := NormalizeVersion(bad); ok {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestLocalPushReadDelete(t *testing.T) {
	l := newLocal(t, "nuget", nil)
	base := "http://kutu.test" + prefix

	var si struct {
		Resources []map[string]string `json:"resources"`
	}
	decode(t, do(l, http.MethodGet, "/v3/index.json", nil, ""), &si)
	found := map[string]string{}
	for _, r := range si.Resources {
		found[r["@type"]] = r["@id"]
	}
	if found["PackageBaseAddress/3.0.0"] != base+"/v3/flatcontainer/" || found["PackagePublish/2.0.0"] != base+"/api/v2/package" ||
		found["RegistrationsBaseUrl/3.6.0"] != base+"/v3/registration/" || found["SearchQueryService/3.5.0"] != base+"/v3/search" {
		t.Fatalf("service index: %+v", found)
	}

	pkg1 := buildNupkg(t, "Acme.Utils", "1.0")
	if w := push(t, l, pkg1); w.Code != http.StatusCreated {
		t.Fatalf("push: %d %s", w.Code, w.Body)
	}
	if w := push(t, l, buildNupkg(t, "Acme.Utils", "2.0.0-Beta.1")); w.Code != http.StatusCreated {
		t.Fatalf("push2: %d %s", w.Code, w.Body)
	}
	if w := push(t, l, []byte("not a zip")); w.Code != http.StatusBadRequest {
		t.Fatalf("bad push: %d", w.Code)
	}

	var idx struct {
		Versions []string `json:"versions"`
	}
	decode(t, do(l, http.MethodGet, "/v3/flatcontainer/ACME.UTILS/index.json", nil, ""), &idx)
	if strings.Join(idx.Versions, ",") != "1.0.0,2.0.0-beta.1" {
		t.Fatalf("versions: %v", idx.Versions)
	}
	w := do(l, http.MethodGet, "/v3/flatcontainer/acme.utils/1.0.0/acme.utils.1.0.0.nupkg", nil, "")
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), pkg1) {
		t.Fatalf("download: %d", w.Code)
	}
	if w := do(l, http.MethodGet, "/v3/flatcontainer/acme.utils/1.0/acme.utils.nuspec", nil, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "<id>Acme.Utils</id>") {
		t.Fatalf("nuspec: %d %s", w.Code, w.Body)
	}

	var reg struct {
		Count int `json:"count"`
		Items []struct {
			Items []struct {
				PackageContent string         `json:"packageContent"`
				CatalogEntry   map[string]any `json:"catalogEntry"`
			} `json:"items"`
		} `json:"items"`
	}
	decode(t, do(l, http.MethodGet, "/v3/registration/acme.utils/index.json", nil, ""), &reg)
	if reg.Count != 1 || len(reg.Items[0].Items) != 2 {
		t.Fatalf("registration: %+v", reg)
	}
	leaf := reg.Items[0].Items[0]
	if leaf.PackageContent != base+"/v3/flatcontainer/acme.utils/1.0.0/acme.utils.1.0.0.nupkg" ||
		leaf.CatalogEntry["licenseExpression"] != "MIT" || leaf.CatalogEntry["id"] != "Acme.Utils" ||
		leaf.CatalogEntry["authors"] != "Jane, John" || leaf.CatalogEntry["listed"] != true {
		t.Fatalf("leaf: %+v", leaf)
	}
	groups := leaf.CatalogEntry["dependencyGroups"].([]any)
	dep := groups[0].(map[string]any)["dependencies"].([]any)[0].(map[string]any)
	if dep["id"] != "Newtonsoft.Json" || dep["range"] != "[13.0.1, )" {
		t.Fatalf("deps: %+v", groups)
	}
	if w := do(l, http.MethodGet, "/v3/registration/acme.utils/1.0.0.json", nil, ""); w.Code != 200 {
		t.Fatalf("leaf: %d", w.Code)
	}

	var sr struct {
		TotalHits int `json:"totalHits"`
		Data      []struct {
			ID       string `json:"id"`
			Version  string `json:"version"`
			Versions []any  `json:"versions"`
		} `json:"data"`
	}
	decode(t, do(l, http.MethodGet, "/v3/search?q=utils", nil, ""), &sr)
	if sr.TotalHits != 1 || sr.Data[0].Version != "1.0.0" || len(sr.Data[0].Versions) != 1 {
		t.Fatalf("search: %+v", sr)
	}
	decode(t, do(l, http.MethodGet, "/v3/search?q=utils&prerelease=true", nil, ""), &sr)
	if sr.Data[0].Version != "2.0.0-Beta.1" {
		t.Fatalf("search pre: %+v", sr)
	}
	decode(t, do(l, http.MethodGet, "/v3/search?q=nomatch", nil, ""), &sr)
	if sr.TotalHits != 0 {
		t.Fatalf("search miss: %+v", sr)
	}
	var ac struct {
		Data []string `json:"data"`
	}
	decode(t, do(l, http.MethodGet, "/v3/autocomplete?q=acme", nil, ""), &ac)
	if len(ac.Data) != 1 || ac.Data[0] != "Acme.Utils" {
		t.Fatalf("autocomplete: %+v", ac)
	}

	info, err := l.ArtifactInfo(context.Background(), registry.ArtifactRef{Name: "acme.utils", Version: "1.0.0"})
	if err != nil || info.License != "MIT" || info.PublishedAt.IsZero() {
		t.Fatalf("info: %+v %v", info, err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v3/flatcontainer/acme.utils/1.0.0/acme.utils.1.0.0.nupkg", nil)
	if ref, ok := l.ClassifyRequest(req); !ok || ref.Name != "acme.utils" || ref.Version != "1.0.0" {
		t.Fatalf("classify: %+v %v", ref, ok)
	}
	d, err := l.PackageDetail(context.Background(), "Acme.Utils")
	if err != nil || d.Generic.LatestVersion != "2.0.0-Beta.1" || d.Generic.License != "MIT" || len(d.Generic.Versions) != 2 {
		t.Fatalf("detail: %+v %v", d, err)
	}
	pkgs, _ := l.ListPackages(context.Background())
	if len(pkgs) != 1 || pkgs[0].Name != "Acme.Utils" {
		t.Fatalf("list: %+v", pkgs)
	}

	// promote
	dst := newLocal(t, "dst", nil)
	if err := l.PromoteVersion(context.Background(), dst, "Acme.Utils", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if w := do(dst, http.MethodGet, "/v3/flatcontainer/acme.utils/index.json", nil, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "1.0.0") {
		t.Fatalf("promoted: %d %s", w.Code, w.Body)
	}

	if w := do(l, http.MethodDelete, "/api/v2/package/Acme.Utils/1.0.0", nil, ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	decode(t, do(l, http.MethodGet, "/v3/flatcontainer/acme.utils/index.json", nil, ""), &idx)
	if strings.Join(idx.Versions, ",") != "2.0.0-beta.1" {
		t.Fatalf("after delete: %v", idx.Versions)
	}
	if w := do(l, http.MethodDelete, "/api/v2/package/Acme.Utils/1.0.0", nil, ""); w.Code != http.StatusNotFound {
		t.Fatalf("delete missing: %d", w.Code)
	}
	if err := l.DeleteVersion(context.Background(), "acme.utils", "2.0.0-beta.1"); err != nil {
		t.Fatal(err)
	}
	if w := do(l, http.MethodGet, "/v3/flatcontainer/acme.utils/index.json", nil, ""); w.Code != 404 {
		t.Fatalf("empty index: %d", w.Code)
	}
}

func TestLocalImmutable(t *testing.T) {
	l := newLocal(t, "nuget", &service.RegistryPolicy{ImmutableVersions: true})
	pkg := buildNupkg(t, "Imm", "1.0.0")
	if w := push(t, l, pkg); w.Code != http.StatusCreated {
		t.Fatalf("push: %d", w.Code)
	}
	if w := push(t, l, pkg); w.Code != http.StatusConflict {
		t.Fatalf("immutable: %d", w.Code)
	}
}

// fakeUpstream serves a nuget.org-shaped feed with non-root resource
// paths backed by a local repo.
func fakeUpstream(t *testing.T, l *Local) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/v3/index.json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"version":"3.0.0","resources":[
{"@id":"%[1]s/flat2/","@type":"PackageBaseAddress/3.0.0"},
{"@id":"%[1]s/reg-gz/","@type":"RegistrationsBaseUrl/3.6.0"},
{"@id":"%[1]s/reg/","@type":"RegistrationsBaseUrl"},
{"@id":"%[1]s/q","@type":"SearchQueryService/3.5.0"}]}`, srv.URL)
	})
	forward := func(from, to string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			r2 := r.Clone(r.Context())
			r2.URL.Path = to + strings.TrimPrefix(r.URL.Path, from)
			r2.Header.Set("X-Pika-Registry-Prefix", "")
			rec := httptest.NewRecorder()
			l.ServeHTTP(rec, r2)
			body := rec.Body.String()
			body = strings.ReplaceAll(body, srv.URL+"/v3/registration/", srv.URL+"/reg-gz/")
			body = strings.ReplaceAll(body, srv.URL+"/v3/flatcontainer/", srv.URL+"/flat2/")
			w.WriteHeader(rec.Code)
			_, _ = io.WriteString(w, body)
		}
	}
	mux.HandleFunc("/flat2/", forward("/flat2/", "/v3/flatcontainer/"))
	mux.HandleFunc("/reg-gz/", forward("/reg-gz/", "/v3/registration/"))
	mux.HandleFunc("/q", forward("/q", "/v3/search"))
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newRemote(t *testing.T, url string) *Remote {
	t.Helper()
	fs, _ := localfs.New(t.TempDir())
	deps := registry.Deps{MountRawFS: func(string) (rawfs.RawFS, error) { return fs, nil }}
	repo := &service.RegistryRepository{Name: "rem", Type: typ, Kind: service.RegistryKindRemote, Mount: "m", BasePath: "c", URL: url}
	reg, err := NewRemoteFactory()(context.Background(), deps, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	return reg.(*Remote)
}

func TestRemote(t *testing.T) {
	up := newLocal(t, "up", nil)
	pkg := buildNupkg(t, "Remote.Pkg", "1.2.3")
	push(t, up, pkg)
	srv := fakeUpstream(t, up)
	rr := newRemote(t, srv.URL) // host root → /v3/index.json discovered
	base := "http://kutu.test" + prefix

	if w := do(rr, http.MethodGet, "/v3/index.json", nil, ""); w.Code != 200 || !strings.Contains(w.Body.String(), base+"/v3/flatcontainer/") {
		t.Fatalf("index: %d %s", w.Code, w.Body)
	}
	var idx struct {
		Versions []string `json:"versions"`
	}
	decode(t, do(rr, http.MethodGet, "/v3/flatcontainer/remote.pkg/index.json", nil, ""), &idx)
	if len(idx.Versions) != 1 || idx.Versions[0] != "1.2.3" {
		t.Fatalf("versions: %v", idx.Versions)
	}
	w := do(rr, http.MethodGet, "/v3/registration/remote.pkg/index.json", nil, "")
	if w.Code != 200 || strings.Contains(w.Body.String(), srv.URL) || !strings.Contains(w.Body.String(), base+"/v3/flatcontainer/remote.pkg/1.2.3/remote.pkg.1.2.3.nupkg") {
		t.Fatalf("registration not rewritten: %d %s", w.Code, w.Body)
	}
	w = do(rr, http.MethodGet, "/v3/search?q=remote", nil, "")
	if w.Code != 200 || strings.Contains(w.Body.String(), srv.URL) || !strings.Contains(w.Body.String(), `"Remote.Pkg"`) {
		t.Fatalf("search: %d %s", w.Code, w.Body)
	}
	w = do(rr, http.MethodGet, "/v3/flatcontainer/remote.pkg/1.2.3/remote.pkg.1.2.3.nupkg", nil, "")
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), pkg) {
		t.Fatalf("nupkg: %d", w.Code)
	}
	info, err := rr.ArtifactInfo(context.Background(), registry.ArtifactRef{Name: "Remote.Pkg", Version: "1.2.3"})
	if err != nil || info.License != "MIT" || info.PublishedAt.IsZero() {
		t.Fatalf("info: %+v %v", info, err)
	}

	srv.Close()
	w = do(rr, http.MethodGet, "/v3/flatcontainer/remote.pkg/1.2.3/remote.pkg.1.2.3.nupkg", nil, "")
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), pkg) {
		t.Fatalf("cached nupkg: %d", w.Code)
	}
	d, err := rr.PackageDetail(context.Background(), "remote.pkg")
	if err != nil || d.Name != "Remote.Pkg" || d.Generic.LatestVersion != "1.2.3" {
		t.Fatalf("detail: %+v %v", d, err)
	}
	if w := do(rr, http.MethodPut, "/api/v2/package", nil, ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("remote write: %d", w.Code)
	}
}

func TestRemotePrefetchServiceIndexURL(t *testing.T) {
	up := newLocal(t, "up", nil)
	push(t, up, buildNupkg(t, "P", "1.0.0"))
	push(t, up, buildNupkg(t, "P", "2.0.0"))
	push(t, up, buildNupkg(t, "P", "3.0.0-rc1"))
	srv := fakeUpstream(t, up)
	rr := newRemote(t, srv.URL+"/v3/index.json")
	if err := rr.Prefetch(context.Background(), "P", ""); err != nil {
		t.Fatal(err)
	}
	pkgs, _ := rr.ListPackages(context.Background())
	if len(pkgs) != 1 || len(pkgs[0].Versions) != 1 || pkgs[0].Versions[0] != "2.0.0" {
		t.Fatalf("prefetched: %+v", pkgs)
	}
}

type stubResolver map[string]registry.Registry

func (s stubResolver) Lookup(_, repo string) (registry.Registry, bool) {
	r, ok := s[repo]
	return r, ok
}

func TestVirtual(t *testing.T) {
	a, b := newLocal(t, "a", nil), newLocal(t, "b", nil)
	push(t, a, buildNupkg(t, "Shared", "1.0.0"))
	push(t, b, buildNupkg(t, "Shared", "2.0.0"))
	push(t, b, buildNupkg(t, "OnlyB", "0.1.0"))
	repo := &service.RegistryRepository{Name: "v", Type: typ, Kind: service.RegistryKindVirtual, Members: []string{"a", "b"}}
	reg, err := NewVirtualFactory(stubResolver{"a": a, "b": b})(context.Background(), registry.Deps{}, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	v := reg.(*Virtual)

	var idx struct {
		Versions []string `json:"versions"`
	}
	decode(t, do(v, http.MethodGet, "/v3/flatcontainer/shared/index.json", nil, ""), &idx)
	if strings.Join(idx.Versions, ",") != "1.0.0,2.0.0" {
		t.Fatalf("merged versions: %v", idx.Versions)
	}
	var regDoc struct {
		Count int `json:"count"`
		Items []struct {
			Items []any `json:"items"`
		} `json:"items"`
	}
	decode(t, do(v, http.MethodGet, "/v3/registration/shared/index.json", nil, ""), &regDoc)
	if regDoc.Count != 2 || len(regDoc.Items[0].Items)+len(regDoc.Items[1].Items) != 2 {
		t.Fatalf("merged registration: %+v", regDoc)
	}
	if w := do(v, http.MethodGet, "/v3/flatcontainer/shared/2.0.0/shared.2.0.0.nupkg", nil, ""); w.Code != 200 {
		t.Fatalf("first-hit download: %d", w.Code)
	}
	var sr struct {
		TotalHits int              `json:"totalHits"`
		Data      []map[string]any `json:"data"`
	}
	decode(t, do(v, http.MethodGet, "/v3/search?q=", nil, ""), &sr)
	if len(sr.Data) != 2 {
		t.Fatalf("merged search: %+v", sr)
	}
	if w := do(v, http.MethodPut, "/api/v2/package", nil, ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("virtual write: %d", w.Code)
	}
	pkgs, _ := v.ListPackages(context.Background())
	if len(pkgs) != 2 {
		t.Fatalf("virtual list: %+v", pkgs)
	}
}
