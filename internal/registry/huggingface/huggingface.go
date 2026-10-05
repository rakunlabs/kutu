// Package huggingface implements a Hugging Face Hub mirror usable by
// huggingface_hub, transformers, datasets and `huggingface-cli download`:
//
//	GET  /api/{models|datasets|spaces}/{repo}                       repo info (sha, siblings)
//	GET  /api/{models|datasets|spaces}/{repo}/revision/{rev}        repo info at revision
//	GET  /api/{models|datasets|spaces}/{repo}/tree/{rev}[/{path}]   file tree (?recursive=true)
//	GET  /api/whoami-v2                                             token check
//	HEAD /[datasets/|spaces/]{repo}/resolve/{rev}/{path}            file metadata (X-Repo-Commit, ETag, X-Linked-*)
//	GET  /[datasets/|spaces/]{repo}/resolve/{rev}/{path}            file download (Range supported)
//	PUT  /[datasets/|spaces/]{repo}/resolve/{branch}/{path}         raw upload (local, allow_push)
//	DELETE /[datasets/|spaces/]{repo}/resolve/{branch}/{path}       remove a file (local, allow_push)
//
// {repo} is "org/name" (or a legacy single-segment id). Remote repos
// resolve branch names to commit shas through the upstream API (cached
// for MutableTTL) and cache files keyed by commit sha forever; files are
// always served by kutu (never redirected to the upstream CDN) and are
// verified against the upstream sha256 / git blob id while streaming.
// Other GET /api/... calls are proxied with MutableTTL caching.
//
// Local repos are a minimal hub: every PUT creates a new commit on the
// branch (sha1 over parent + time + sorted file list); the huggingface_hub
// commit/preupload/LFS upload APIs are not implemented, so
// `huggingface-cli upload` does not work — upload with plain HTTP PUT.
//
// Client configuration:
//
//	export HF_ENDPOINT=https://kutu.example.com/registries/{ns}/{repo}
//	export HF_TOKEN=<kutu token>
//	huggingface-cli download org/model
//	curl -X PUT -H "Authorization: Bearer $HF_TOKEN" --data-binary @model.safetensors \
//	  $HF_ENDPOINT/org/model/resolve/main/model.safetensors
package huggingface

import (
	"context"
	"crypto/sha1" //nolint:gosec // git blob ids are sha1
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/registry/virtualbase"
	"github.com/rakunlabs/kutu/internal/service"
)

const typ = service.RegistryTypeHuggingFace

const (
	kindModels   = "models"
	kindDatasets = "datasets"
	kindSpaces   = "spaces"
)

var kinds = map[string]bool{kindModels: true, kindDatasets: true, kindSpaces: true}

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

func isSHA(s string) bool { return shaRe.MatchString(s) }

// repoRef identifies one hub repository.
type repoRef struct {
	Kind string // models | datasets | spaces
	ID   string // "org/name" or "name"
}

// key is the single-segment storage key ("models--org--name").
func (k repoRef) key() string { return k.Kind + "--" + strings.ReplaceAll(k.ID, "/", "--") }

// name is the package name shown in listings.
func (k repoRef) name() string {
	if k.Kind == kindModels {
		return k.ID
	}
	return k.Kind + "/" + k.ID
}

func (k repoRef) escID() string {
	parts := strings.Split(k.ID, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// webPrefix is the resolve URL prefix of the repo.
func (k repoRef) webPrefix() string {
	if k.Kind == kindModels {
		return "/" + k.escID()
	}
	return "/" + k.Kind + "/" + k.escID()
}

func (k repoRef) apiPath() string { return "/api/" + k.Kind + "/" + k.escID() }

func validIDSeg(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.Contains(s, "--") && !strings.ContainsAny(s, "/\\\x00")
}

func newRef(kind string, segs []string) (repoRef, bool) {
	if !kinds[kind] || len(segs) < 1 || len(segs) > 2 {
		return repoRef{}, false
	}
	for _, s := range segs {
		if !validIDSeg(s) {
			return repoRef{}, false
		}
	}
	return repoRef{Kind: kind, ID: strings.Join(segs, "/")}, true
}

func parseKey(s string) (repoRef, bool) {
	parts := strings.Split(s, "--")
	if len(parts) < 2 {
		return repoRef{}, false
	}
	return newRef(parts[0], parts[1:])
}

// parseName parses a listing name ("org/name", "datasets/org/name").
func parseName(name string) (repoRef, bool) {
	parts := strings.Split(strings.Trim(name, "/"), "/")
	kind := kindModels
	if len(parts) > 1 && (parts[0] == kindDatasets || parts[0] == kindSpaces) {
		kind, parts = parts[0], parts[1:]
	}
	return newRef(kind, parts)
}

const (
	opInfo = iota + 1
	opTree
	opResolve
	opAPI
	opWhoami
)

type route struct {
	op   int
	ref  repoRef
	rev  string
	file string
}

func validFile(p string) bool {
	if p == "" {
		return false
	}
	for _, s := range strings.Split(p, "/") {
		if s == "" || s == "." || s == ".." || strings.ContainsAny(s, "\\\x00") {
			return false
		}
	}
	return true
}

// parseRoute classifies an escaped request path.
func parseRoute(escaped string) (route, bool) {
	raw := strings.Split(strings.Trim(escaped, "/"), "/")
	segs := make([]string, 0, len(raw))
	for _, s := range raw {
		u, err := url.PathUnescape(s)
		if err != nil {
			return route{}, false
		}
		segs = append(segs, u)
	}
	if len(segs) == 0 || segs[0] == "" {
		return route{}, false
	}
	if segs[0] == "api" {
		if len(segs) == 2 && segs[1] == "whoami-v2" {
			return route{op: opWhoami}, true
		}
		if len(segs) < 3 || !kinds[segs[1]] {
			return route{op: opAPI}, true
		}
		rest := segs[2:]
		n := 2
		if len(rest) == 1 || ((rest[1] == "revision" || rest[1] == "tree") && len(rest) > 2) {
			n = 1
		}
		ref, ok := newRef(segs[1], rest[:n])
		if !ok {
			return route{op: opAPI}, true
		}
		tail := rest[n:]
		switch {
		case len(tail) == 0:
			return route{op: opInfo, ref: ref}, true
		case tail[0] == "revision" && len(tail) == 2 && tail[1] != "":
			return route{op: opInfo, ref: ref, rev: tail[1]}, true
		case tail[0] == "tree" && len(tail) >= 2 && tail[1] != "":
			return route{op: opTree, ref: ref, rev: tail[1], file: strings.Join(tail[2:], "/")}, true
		}
		return route{op: opAPI, ref: ref}, true
	}
	kind, rest := kindModels, segs
	if segs[0] == kindDatasets || segs[0] == kindSpaces {
		kind, rest = segs[0], segs[1:]
	}
	for _, i := range []int{2, 1} {
		if len(rest) >= i+3 && rest[i] == "resolve" && rest[i+1] != "" {
			ref, ok := newRef(kind, rest[:i])
			file := strings.Join(rest[i+2:], "/")
			if !ok || !validFile(file) {
				return route{}, false
			}
			return route{op: opResolve, ref: ref, rev: rest[i+1], file: file}, true
		}
	}
	return route{}, false
}

func escFile(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// fileMeta describes one file of a commit.
type fileMeta struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
	BlobID string `json:"blob_id,omitempty"`
	LFS    bool   `json:"lfs,omitempty"`
}

func (f fileMeta) etag() string {
	if f.LFS {
		return f.SHA256
	}
	return f.BlobID
}

func setFileHeaders(w http.ResponseWriter, commit string, f fileMeta) {
	h := w.Header()
	h.Set("X-Repo-Commit", commit)
	if e := f.etag(); e != "" {
		h.Set("ETag", `"`+e+`"`)
	}
	if f.LFS {
		h.Set("X-Linked-Etag", `"`+f.SHA256+`"`)
		h.Set("X-Linked-Size", strconv.FormatInt(f.Size, 10))
	}
	h.Set("Accept-Ranges", "bytes")
}

// hfError writes an error with the X-Error-Code header huggingface_hub
// maps to RepositoryNotFound / RevisionNotFound / EntryNotFound.
func hfError(w http.ResponseWriter, code int, errCode, msg string) {
	if errCode != "" {
		w.Header().Set("X-Error-Code", errCode)
	}
	w.Header().Set("X-Error-Message", msg)
	pkgbase.Error(w, code, msg)
}

func whoami(w http.ResponseWriter, r *http.Request) {
	pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{
		"type": "user", "name": "kutu", "fullname": "kutu", "orgs": []any{},
		"auth": map[string]any{"type": "access_token", "accessToken": map[string]any{"role": "read"}},
	})
}

// lfsPointer returns the git-lfs pointer text of a file.
func lfsPointer(sha string, size int64) string {
	return fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", sha, size)
}

func gitBlobID(b []byte) string {
	h := sha1.New() //nolint:gosec
	fmt.Fprintf(h, "blob %d\x00", len(b))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// verifier hashes streamed content and checks it against f.
type verifier struct {
	f    fileMeta
	n    int64
	h256 hash.Hash
	h1   hash.Hash
}

func newVerifier(f fileMeta) *verifier {
	v := &verifier{f: f}
	if f.SHA256 != "" {
		v.h256 = sha256.New()
	} else if f.BlobID != "" {
		v.h1 = sha1.New() //nolint:gosec
		fmt.Fprintf(v.h1, "blob %d\x00", f.Size)
	}
	return v
}

func (v *verifier) Write(p []byte) (int, error) {
	v.n += int64(len(p))
	if v.h256 != nil {
		v.h256.Write(p)
	}
	if v.h1 != nil {
		v.h1.Write(p)
	}
	return len(p), nil
}

func (v *verifier) check() error {
	if v.n != v.f.Size {
		return fmt.Errorf("size mismatch for %s: got %d, want %d", v.f.Path, v.n, v.f.Size)
	}
	if v.h256 != nil {
		if got := hex.EncodeToString(v.h256.Sum(nil)); got != v.f.SHA256 {
			return fmt.Errorf("sha256 mismatch for %s", v.f.Path)
		}
	}
	if v.h1 != nil {
		if got := hex.EncodeToString(v.h1.Sum(nil)); got != v.f.BlobID {
			return fmt.Errorf("blob id mismatch for %s", v.f.Path)
		}
	}
	return nil
}

// lenientWriter forwards to w until the first error, then drops data
// so a disconnected client never aborts a cache fill.
type lenientWriter struct {
	w      io.Writer
	failed bool
}

func (l *lenientWriter) Write(p []byte) (int, error) {
	if !l.failed {
		if _, err := l.w.Write(p); err != nil {
			l.failed = true
		}
	}
	return len(p), nil
}

// inflight tracks paths currently being filled.
type inflight struct{ m sync.Map }

func (i *inflight) acquire(k string) bool {
	_, loaded := i.m.LoadOrStore(k, struct{}{})
	return !loaded
}

func (i *inflight) release(k string) { i.m.Delete(k) }

func (i *inflight) busy(k string) bool {
	_, ok := i.m.Load(k)
	return ok
}

// readmeLicense extracts "license:" from a model card's YAML front matter.
func readmeLicense(b []byte) string {
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") {
		return ""
	}
	for _, line := range strings.Split(s[4:], "\n") {
		if strings.TrimSpace(line) == "---" {
			break
		}
		if v, ok := strings.CutPrefix(line, "license:"); ok {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

func classify(r *http.Request) (registry.ArtifactRef, bool) {
	rt, ok := parseRoute(r.URL.EscapedPath())
	if !ok {
		return registry.ArtifactRef{}, false
	}
	switch rt.op {
	case opResolve:
		return registry.ArtifactRef{Name: rt.ref.name(), Version: rt.rev}, true
	case opInfo, opTree:
		return registry.ArtifactRef{Name: rt.ref.name()}, true
	}
	return registry.ArtifactRef{}, false
}

// ── Virtual ──

// Virtual is first-hit, except file downloads which are probed with
// HEAD and then streamed straight from the winning member (never
// buffered in memory).
type Virtual struct{ *pkgbase.Virtual }

func NewVirtualFactory(resolver virtualbase.Resolver) registry.Factory {
	return func(_ context.Context, _ registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		v, err := pkgbase.NewVirtual(typ, resolver, ns, r)
		if err != nil {
			return nil, err
		}
		return &Virtual{Virtual: v}, nil
	}
}

func (v *Virtual) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if rt, ok := parseRoute(r.URL.EscapedPath()); ok && rt.op == opResolve {
			if !streamFirstHit(v.Virtual, w, r) {
				hfError(w, http.StatusNotFound, "EntryNotFound", "entry not found")
			}
			return
		}
	}
	v.Virtual.ServeHTTP(w, r)
}

func streamFirstHit(v *pkgbase.Virtual, w http.ResponseWriter, r *http.Request) bool {
	served := false
	v.ForEachMember(func(reg registry.Registry) bool {
		if _, _, ok := registry.CheckGate(reg, r); !ok {
			return false
		}
		probe := r.Clone(r.Context())
		probe.Method = http.MethodHead
		for _, h := range []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since"} {
			probe.Header.Del(h)
		}
		rec := httptest.NewRecorder()
		reg.ServeHTTP(rec, probe)
		if rec.Code < 200 || rec.Code >= 300 {
			return false
		}
		reg.ServeHTTP(w, r)
		served = true
		return true
	})
	return served
}

func filesRel(ref repoRef, commit, file string) string {
	return path.Join("files", ref.key(), commit, file)
}
