package vagrant

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	repo := &service.RegistryRepository{Name: name, Type: typ, Kind: service.RegistryKindLocal, Mount: "m", BasePath: "vagrant", AllowPush: true}
	r, err := NewLocalFactory()(context.Background(), deps, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	return r.(*Local)
}

func do(h http.Handler, method, p string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, p, bytes.NewReader(body))
	r.Header.Set("X-Pika-Registry-Prefix", "/registries/default/v")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func meta(t *testing.T, w *httptest.ResponseRecorder) Metadata {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("metadata: %d %s", w.Code, w.Body)
	}
	var m Metadata
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestVagrantLocalRoundTrip(t *testing.T) {
	l := newLocal(t, "v")
	ctx := context.Background()
	if w := do(l, http.MethodPut, "/acme/devbox/1.0.0/virtualbox/amd64", []byte("box-amd64")); w.Code != http.StatusCreated {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodPut, "/acme/devbox/1.0.0/virtualbox/arm64", []byte("box-arm64")); w.Code != http.StatusCreated {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	if w := do(l, http.MethodPut, "/acme/devbox/1.10.0/libvirt?description=Dev", []byte("lv")); w.Code != http.StatusCreated {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}

	m := meta(t, do(l, http.MethodGet, "/acme/devbox", nil))
	if m.Name != "acme/devbox" || len(m.Versions) != 2 || m.Versions[0].Version != "1.10.0" || m.Description != "Dev" {
		t.Fatalf("metadata: %+v", m)
	}
	v1 := m.Versions[1]
	if len(v1.Providers) != 2 {
		t.Fatalf("providers: %+v", v1)
	}
	p := v1.Providers[0]
	if p.URL != "http://example.com/registries/default/v/acme/devbox/1.0.0/virtualbox/amd64.box" ||
		p.Checksum != pkgbase.SHA256Hex([]byte("box-amd64")) || p.ChecksumType != "sha256" || !p.DefaultArchitecture || v1.Providers[1].DefaultArchitecture {
		t.Fatalf("provider: %+v", v1.Providers)
	}
	if m.Versions[0].Providers[0].URL != "http://example.com/registries/default/v/acme/devbox/1.10.0/libvirt.box" {
		t.Fatalf("no-arch url: %+v", m.Versions[0].Providers[0])
	}
	if mj := meta(t, do(l, http.MethodGet, "/acme/devbox.json", nil)); len(mj.Versions) != 2 {
		t.Fatalf(".json: %+v", mj)
	}
	if w := do(l, http.MethodGet, "/acme/devbox/1.0.0/virtualbox/arm64.box", nil); w.Body.String() != "box-arm64" {
		t.Fatalf("download: %d %q", w.Code, w.Body)
	}
	if w := do(l, http.MethodGet, "/acme/devbox/1.10.0/libvirt.box", nil); w.Body.String() != "lv" {
		t.Fatalf("download: %d %q", w.Code, w.Body)
	}
	if w := do(l, http.MethodGet, "/api/v2/box/acme/devbox", nil); !strings.Contains(w.Body.String(), `"current_version":{`) {
		t.Fatalf("api box: %s", w.Body)
	}
	if w := do(l, http.MethodGet, "/api/v2/search?q=dev", nil); !strings.Contains(w.Body.String(), `"tag":"acme/devbox"`) {
		t.Fatalf("search: %s", w.Body)
	}

	pkgs, _ := l.ListPackages(ctx)
	if len(pkgs) != 1 || pkgs[0].Name != "acme/devbox" || len(pkgs[0].Versions) != 2 {
		t.Fatalf("list: %+v", pkgs)
	}
	d, err := l.PackageDetail(ctx, "acme/devbox")
	if err != nil || d.Generic.LatestVersion != "1.10.0" || len(d.Generic.Versions[1].Files) != 2 {
		t.Fatalf("detail: %+v %v", d, err)
	}
	if ref, ok := l.ClassifyRequest(httptest.NewRequest(http.MethodGet, "/acme/devbox/1.0.0/virtualbox/amd64.box", nil)); !ok || ref.Version != "1.0.0" || ref.Name != "acme/devbox" {
		t.Fatalf("classify: %+v", ref)
	}

	dst := newLocal(t, "dst")
	if err := l.PromoteVersion(ctx, dst, "acme/devbox", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if w := do(dst, http.MethodGet, "/acme/devbox/1.0.0/virtualbox/amd64.box", nil); w.Body.String() != "box-amd64" {
		t.Fatalf("promoted: %d", w.Code)
	}

	if w := do(l, http.MethodDelete, "/acme/devbox/1.0.0/virtualbox/arm64", nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete provider: %d", w.Code)
	}
	if err := l.DeleteVersion(ctx, "acme/devbox", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	m = meta(t, do(l, http.MethodGet, "/acme/devbox", nil))
	if len(m.Versions) != 1 {
		t.Fatalf("after delete: %+v", m)
	}
	if w := do(l, http.MethodGet, "/acme/devbox/1.0.0/virtualbox/amd64.box", nil); w.Code != 404 {
		t.Fatalf("deleted download: %d", w.Code)
	}
	if err := l.DeleteVersion(ctx, "acme/devbox", "9.9"); err != registry.ErrPackageNotFound {
		t.Fatalf("missing: %v", err)
	}
}

func TestVagrantRemote(t *testing.T) {
	var srvURL string
	var boxHits int
	mux := http.NewServeMux()
	mux.HandleFunc("/hashicorp/bionic64", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/json" {
			http.Error(w, "html", 406)
			return
		}
		_, _ = w.Write([]byte(`{"name":"hashicorp/bionic64","description":"Ubuntu","versions":[
			{"version":"1.0.282","providers":[{"name":"virtualbox","url":"` + srvURL + `/dl/vb.box","checksum_type":"sha256","checksum":"x"}]},
			{"version":"1.0.100","providers":[]}]}`))
	})
	mux.HandleFunc("/dl/vb.box", func(w http.ResponseWriter, _ *http.Request) {
		boxHits++
		_, _ = w.Write([]byte("BOXDATA"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL

	fs, _ := localfs.New(t.TempDir())
	deps := registry.Deps{MountRawFS: func(string) (rawfs.RawFS, error) { return fs, nil }}
	repo := &service.RegistryRepository{Name: "rem", Type: typ, Kind: service.RegistryKindRemote, Mount: "m", BasePath: "c", URL: srv.URL}
	reg, err := NewRemoteFactory()(context.Background(), deps, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	rr := reg.(*Remote)
	m := meta(t, do(rr, http.MethodGet, "/hashicorp/bionic64", nil))
	if m.Versions[0].Version != "1.0.282" || m.Versions[0].Providers[0].URL != "http://example.com/registries/default/v/hashicorp/bionic64/1.0.282/virtualbox.box" {
		t.Fatalf("rewritten: %+v", m)
	}
	for i := 0; i < 2; i++ {
		if w := do(rr, http.MethodGet, "/hashicorp/bionic64/1.0.282/virtualbox.box", nil); w.Code != 200 || w.Body.String() != "BOXDATA" {
			t.Fatalf("box: %d %q", w.Code, w.Body)
		}
	}
	if boxHits != 1 {
		t.Fatalf("box fetched %d times", boxHits)
	}
	pkgs, _ := rr.ListPackages(context.Background())
	if len(pkgs) != 1 || pkgs[0].Name != "hashicorp/bionic64" {
		t.Fatalf("list: %+v", pkgs)
	}
	if err := rr.Prefetch(context.Background(), "hashicorp/bionic64", ""); err != nil {
		t.Fatalf("prefetch: %v", err)
	}
	if w := do(rr, http.MethodPut, "/hashicorp/bionic64/1/vb", []byte("x")); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("put: %d", w.Code)
	}
	srv.Close()
	if w := do(rr, http.MethodGet, "/hashicorp/bionic64/1.0.282/virtualbox.box", nil); w.Body.String() != "BOXDATA" {
		t.Fatalf("cached: %d", w.Code)
	}
	if w := do(rr, http.MethodGet, "/hashicorp/bionic64", nil); w.Code != 200 {
		t.Fatalf("stale metadata: %d", w.Code)
	}
}

type resolver map[string]registry.Registry

func (r resolver) Lookup(_, name string) (registry.Registry, bool) {
	reg, ok := r[name]
	return reg, ok
}

func TestVagrantVirtual(t *testing.T) {
	a, b := newLocal(t, "a"), newLocal(t, "b")
	do(a, http.MethodPut, "/acme/devbox/1.0.0/virtualbox", []byte("a1"))
	do(b, http.MethodPut, "/acme/devbox/2.0.0/virtualbox", []byte("b2"))
	do(b, http.MethodPut, "/acme/devbox/1.0.0/libvirt", []byte("b1"))

	repo := &service.RegistryRepository{Name: "v", Type: typ, Kind: service.RegistryKindVirtual, Members: []string{"a", "b"}}
	reg, err := NewVirtualFactory(resolver{"a": a, "b": b})(context.Background(), registry.Deps{}, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	m := meta(t, do(reg, http.MethodGet, "/acme/devbox", nil))
	if len(m.Versions) != 2 || m.Versions[0].Version != "2.0.0" || len(m.Versions[1].Providers) != 2 {
		t.Fatalf("merged: %+v", m)
	}
	if w := do(reg, http.MethodGet, "/acme/devbox/1.0.0/libvirt.box", nil); w.Body.String() != "b1" {
		t.Fatalf("download: %d %q", w.Code, w.Body)
	}
	if w := do(reg, http.MethodGet, "/api/v2/box/acme/devbox/version/2.0.0", nil); !strings.Contains(w.Body.String(), `"version":"2.0.0"`) {
		t.Fatalf("api version: %s", w.Body)
	}
}
