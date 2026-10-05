package gitlfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/rawfs/localfs"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/service"
)

func depsFor(t *testing.T) registry.Deps {
	t.Helper()
	fs, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return registry.Deps{MountRawFS: func(string) (rawfs.RawFS, error) { return fs, nil }}
}

func newLocal(t *testing.T, name string) *Local {
	t.Helper()
	repo := &service.RegistryRepository{Name: name, Type: typ, Kind: service.RegistryKindLocal, Mount: "m", BasePath: "lfs", AllowPush: true}
	r, err := NewLocalFactory()(context.Background(), depsFor(t), "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	return r.(*Local)
}

func do(h http.Handler, method, p string, body []byte, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, p, bytes.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func oidOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func batch(t *testing.T, h http.Handler, p, op string, objs ...batchObject) batchResponse {
	t.Helper()
	body, _ := json.Marshal(batchRequest{Operation: op, Transfers: []string{"basic"}, Objects: objs})
	w := do(h, http.MethodPost, p, body, "Authorization", "Bearer tok", "X-Pika-Registry-Prefix", "/registries/default/lfs")
	if w.Code != 200 || w.Header().Get("Content-Type") != MediaType {
		t.Fatalf("batch %s: %d %s", op, w.Code, w.Body)
	}
	var out batchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestLocalRoundTrip(t *testing.T) {
	l := newLocal(t, "lfs")
	data := []byte("large binary content")
	oid := oidOf(data)
	obj := batchObject{OID: oid, Size: int64(len(data))}

	up := batch(t, l, "/objects/batch", "upload", obj)
	a := up.Objects[0].Actions["upload"]
	if a == nil || !strings.HasSuffix(a.Href, "/registries/default/lfs/objects/"+oid) || a.Header["Authorization"] != "Bearer tok" {
		t.Fatalf("upload action: %+v", up.Objects[0])
	}
	if w := do(l, http.MethodPut, "/objects/"+oid, []byte("tampered content!!!!")); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad sha accepted: %d", w.Code)
	}
	if w := do(l, http.MethodPut, "/objects/"+oid, data); w.Code != 200 {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	vb, _ := json.Marshal(obj)
	if w := do(l, http.MethodPost, "/objects/verify", vb); w.Code != 200 {
		t.Fatalf("verify: %d", w.Code)
	}
	if up := batch(t, l, "/objects/batch", "upload", obj); up.Objects[0].Actions != nil {
		t.Fatalf("existing object should need no upload: %+v", up.Objects[0])
	}

	missing := batchObject{OID: oidOf([]byte("nope")), Size: 4}
	down := batch(t, l, "/repo.git/info/lfs/objects/batch", "download", obj, missing)
	if down.Objects[0].Actions["download"] == nil || down.Objects[1].Error == nil || down.Objects[1].Error.Code != 404 {
		t.Fatalf("download batch: %+v", down)
	}
	if w := do(l, http.MethodGet, "/info/lfs/objects/"+oid, nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatalf("get: %d", w.Code)
	}
	if st, _ := l.Stats(context.Background()); st.BlobCount != 1 {
		t.Fatalf("stats: %+v", st)
	}
	dst := newLocal(t, "dst")
	if err := l.PromoteVersion(context.Background(), dst, oid, ""); err != nil {
		t.Fatal(err)
	}
	if err := l.DeleteVersion(context.Background(), oid, ""); err != nil {
		t.Fatal(err)
	}
	if w := do(l, http.MethodGet, "/objects/"+oid, nil); w.Code != 404 {
		t.Fatalf("after delete: %d", w.Code)
	}
	if w := do(dst, http.MethodGet, "/objects/"+oid, nil); w.Code != 200 {
		t.Fatalf("promoted: %d", w.Code)
	}
}

func TestLocks(t *testing.T) {
	l := newLocal(t, "lfs")
	w := do(l, http.MethodPost, "/locks", []byte(`{"path":"a.psd"}`), "Authorization", "Basic "+basic("alice"))
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var created struct{ Lock Lock }
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if w := do(l, http.MethodPost, "/locks", []byte(`{"path":"a.psd"}`)); w.Code != http.StatusConflict {
		t.Fatalf("conflict: %d", w.Code)
	}
	w = do(l, http.MethodPost, "/locks/verify", []byte(`{}`), "Authorization", "Basic "+basic("bob"))
	if !strings.Contains(w.Body.String(), `"theirs":[{`) {
		t.Fatalf("verify: %s", w.Body)
	}
	if w := do(l, http.MethodGet, "/locks?path=a.psd", nil); !strings.Contains(w.Body.String(), created.Lock.ID) {
		t.Fatalf("list: %s", w.Body)
	}
	if w := do(l, http.MethodPost, "/locks/"+created.Lock.ID+"/unlock", []byte(`{}`), "Authorization", "Basic "+basic("bob")); w.Code != http.StatusForbidden {
		t.Fatalf("foreign unlock: %d", w.Code)
	}
	if w := do(l, http.MethodPost, "/locks/"+created.Lock.ID+"/unlock", []byte(`{}`), "Authorization", "Basic "+basic("alice")); w.Code != 200 {
		t.Fatalf("unlock: %d %s", w.Code, w.Body)
	}
}

func basic(user string) string {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.SetBasicAuth(user, "tok")
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Basic ")
}

func TestRemote(t *testing.T) {
	up := newLocal(t, "up")
	data := []byte("remote object")
	oid := oidOf(data)
	do(up, http.MethodPut, "/objects/"+oid, data)
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		up.ServeHTTP(w, r)
	}))
	defer srv.Close()

	repo := &service.RegistryRepository{Name: "rem", Type: typ, Kind: service.RegistryKindRemote, Mount: "m", BasePath: "c", URL: srv.URL,
		Auth: &service.RegistryUpstreamAuth{Type: service.RegistryAuthBearer, Token: "up-secret"}}
	reg, err := NewRemoteFactory()(context.Background(), depsFor(t), "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	rr := reg.(*Remote)
	obj := batchObject{OID: oid, Size: int64(len(data))}
	down := batch(t, rr, "/objects/batch", "download", obj, batchObject{OID: oidOf([]byte("x")), Size: 1})
	a := down.Objects[0].Actions["download"]
	if a == nil || !strings.Contains(a.Href, "/registries/default/lfs/objects/"+oid) || down.Objects[1].Error == nil {
		t.Fatalf("remote batch: %+v", down)
	}
	if gotAuth != "Bearer up-secret" {
		t.Fatalf("upstream auth: %q", gotAuth)
	}
	if w := do(rr, http.MethodGet, "/objects/"+oid+"?size="+itoa(len(data)), nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatalf("remote get: %d %s", w.Code, w.Body)
	}
	srv.Close()
	if w := do(rr, http.MethodGet, "/objects/"+oid, nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatalf("cached get: %d", w.Code)
	}
	if down := batch(t, rr, "/objects/batch", "download", obj); down.Objects[0].Actions["download"] == nil {
		t.Fatalf("cached batch: %+v", down)
	}
	body, _ := json.Marshal(batchRequest{Operation: "upload", Objects: []batchObject{obj}})
	if w := do(rr, http.MethodPost, "/objects/batch", body); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("remote upload: %d", w.Code)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

type resolver map[string]registry.Registry

func (r resolver) Lookup(_, name string) (registry.Registry, bool) {
	reg, ok := r[name]
	return reg, ok
}

func TestVirtual(t *testing.T) {
	a, b := newLocal(t, "a"), newLocal(t, "b")
	data := []byte("in b")
	oid := oidOf(data)
	do(b, http.MethodPut, "/objects/"+oid, data)
	repo := &service.RegistryRepository{Name: "v", Type: typ, Kind: service.RegistryKindVirtual, Members: []string{"a", "b"}}
	reg, err := NewVirtualFactory(resolver{"a": a, "b": b})(context.Background(), registry.Deps{}, "default", repo)
	if err != nil {
		t.Fatal(err)
	}
	down := batch(t, reg, "/objects/batch", "download", batchObject{OID: oid, Size: int64(len(data))})
	if down.Objects[0].Actions["download"] == nil {
		t.Fatalf("virtual batch: %+v", down)
	}
	if w := do(reg, http.MethodGet, "/objects/"+oid, nil); w.Code != 200 || w.Body.String() != "in b" {
		t.Fatalf("virtual get: %d", w.Code)
	}
	body, _ := json.Marshal(batchRequest{Operation: "upload", Objects: []batchObject{{OID: oid, Size: 4}}})
	if w := do(reg, http.MethodPost, "/objects/batch", body); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("virtual upload: %d", w.Code)
	}
}
