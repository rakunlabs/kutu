package conda

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/registry/virtualbase"
	"github.com/rakunlabs/kutu/internal/service"
)

var (
	_ registry.PackageLister        = (*Remote)(nil)
	_ registry.PackageDetailer      = (*Remote)(nil)
	_ registry.ArtifactClassifier   = (*Remote)(nil)
	_ registry.ArtifactInfoProvider = (*Remote)(nil)
	_ registry.Prefetcher           = (*Remote)(nil)
	_ registry.CachePurger          = (*Remote)(nil)
	_ registry.UpstreamProber       = (*Remote)(nil)
	_ registry.PackageLister        = (*Virtual)(nil)
)

// maxParseInMemory bounds how much of a cached package is buffered
// when the backend cannot provide random access for .conda parsing.
const maxParseInMemory = 512 << 20

// ── Remote ──

type Remote struct {
	*pkgbase.RemoteRepo
	store *Store

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func NewRemoteFactory() registry.Factory {
	return func(_ context.Context, deps registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		base, err := pkgbase.NewRemoteRepo(deps, typ, ns, r, "/noarch/repodata.json")
		if err != nil {
			return nil, err
		}
		return &Remote{RemoteRepo: base, store: &Store{base.Store}, locks: map[string]*sync.Mutex{}}, nil
	}
}

func (rr *Remote) Store() *Store { return rr.store }

func (rr *Remote) lock(key string) func() {
	rr.mu.Lock()
	m, ok := rr.locks[key]
	if !ok {
		m = &sync.Mutex{}
		rr.locks[key] = m
	}
	rr.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// ListPackages lists the package files pulled through the cache.
func (rr *Remote) ListPackages(ctx context.Context) ([]registry.PackageSummary, error) {
	return rr.store.ListPackages(ctx)
}

func (rr *Remote) Stats(ctx context.Context) (registry.Stats, error) {
	return pkgbase.StatsOf(ctx, rr, rr.store.Store), nil
}

func (rr *Remote) PackageDetail(_ context.Context, name string) (*registry.PackageDetail, error) {
	return detail(rr.store, name)
}

func (rr *Remote) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) { return classify(r) }

func (rr *Remote) ArtifactInfo(_ context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	return artifactInfo(rr.store, ref)
}

func (rr *Remote) PurgeCache(_ context.Context, opts registry.PurgeOptions) (registry.PurgeStats, error) {
	return rr.Purge(opts, indexDir), nil
}

// fetch makes sure rel holds the upstream document at upath, streaming
// it straight into the store. Mutable documents are refreshed after
// MutableTTL; on upstream failure a stale copy is kept.
func (rr *Remote) fetch(ctx context.Context, rel, upath string, mutable bool, rewrite bool) error {
	if rr.store.Exists(rel) && (!mutable || rr.Fresh(rel)) {
		return nil
	}
	defer rr.lock(rel)()
	if rr.store.Exists(rel) && (!mutable || rr.Fresh(rel)) {
		return nil
	}
	client := rr.Client
	if rr.Router != nil {
		client = rr.Router.For(upath)
	}
	resp, err := client.Get(ctx, upath)
	if err != nil {
		if mutable && rr.store.Exists(rel) && !pkgbase.IsNotFound(err) {
			return nil
		}
		return err
	}
	defer resp.Body.Close()
	var body io.Reader = resp.Body
	size := resp.ContentLength
	if rewrite {
		body = rewriteBaseURL(resp.Body)
		size = -1
	}
	if err := rr.store.WriteStream(rel, body, size); err != nil {
		_ = rr.store.Delete(rel)
		return err
	}
	return nil
}

var baseURLRe = regexp.MustCompile(`"base_url"\s*:\s*"[^"]*"`)

// rewriteBaseURL neutralises a CEP-15 info.base_url (which would send
// clients straight to the upstream) by pointing it at the subdir
// itself. Only the document head is inspected; info precedes packages.
func rewriteBaseURL(r io.Reader) io.Reader {
	br := bufio.NewReaderSize(r, 64<<10)
	head, _ := br.Peek(64 << 10)
	if !bytes.Contains(head, []byte(`"base_url"`)) {
		return br
	}
	h := make([]byte, len(head))
	n, _ := io.ReadFull(br, h)
	h = h[:n]
	if loc := baseURLRe.FindIndex(h); loc != nil {
		var out []byte
		out = append(out, h[:loc[0]]...)
		out = append(out, `"base_url":"./"`...)
		out = append(out, h[loc[1]:]...)
		h = out
	}
	return io.MultiReader(bytes.NewReader(h), br)
}

// cachePackage pulls a package file and records its index.json.
func (rr *Remote) cachePackage(ctx context.Context, subdir, file string) error {
	rel := rr.store.pkgRel(subdir, file)
	if err := rr.fetch(ctx, rel, "/"+subdir+"/"+file, false, false); err != nil {
		return err
	}
	if !rr.store.Exists(rr.store.recordRel(subdir, file)) {
		rr.recordCached(subdir, file)
	}
	return nil
}

func (rr *Remote) recordCached(subdir, file string) {
	_, _, _, ext, ok := SplitFilename(file)
	if !ok {
		return
	}
	rel := rr.store.pkgRel(subdir, file)
	rc, fi, err := rr.store.Open(rel)
	if err != nil {
		return
	}
	defer rc.Close()
	var ra io.ReaderAt
	size := fi.Size
	if x, ok := rc.(io.ReaderAt); ok {
		ra = x
	} else {
		if size > maxParseInMemory {
			return
		}
		b, err := io.ReadAll(rc)
		if err != nil {
			return
		}
		ra, size = bytes.NewReader(b), int64(len(b))
	}
	index, about, err := ParsePackage(ext, ra, size)
	if err != nil {
		return
	}
	md5sum, sha := mapStr(index, "md5"), mapStr(index, "sha256")
	rec := newRecord(subdir, file, index, about, md5sum, sha, size)
	if md5sum == "" {
		delete(rec.Index, "md5")
	}
	if sha == "" {
		delete(rec.Index, "sha256")
	}
	_ = rr.store.PutRecord(rec)
}

func (rr *Remote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !pkgbase.IsRead(r) {
		pkgbase.Error(w, http.StatusMethodNotAllowed, "remote registry is read-only")
		return
	}
	p := strings.Trim(r.URL.Path, "/")
	if p == "channeldata.json" {
		rr.serveFetched(w, r, path.Join(indexDir, p), "/"+p, true, true)
		return
	}
	subdir, file, ok := splitPath(p)
	if !ok {
		pkgbase.NotFound(w)
		return
	}
	switch {
	case indexDocs[file]:
		rr.serveFetched(w, r, rr.store.indexRel(subdir, file), "/"+p, true, true)
	case validFilename(file):
		if pkgbase.ServeStored(w, r, rr.store.Store, rr.store.pkgRel(subdir, file), "application/octet-stream") {
			return
		}
		if err := rr.cachePackage(r.Context(), subdir, file); err != nil {
			pkgbase.UpstreamError(w, err)
			return
		}
		if !pkgbase.ServeStored(w, r, rr.store.Store, rr.store.pkgRel(subdir, file), "application/octet-stream") {
			pkgbase.NotFound(w)
		}
	default:
		// Compressed index variants (.zst/.bz2) are not proxied: they
		// cannot be base_url-rewritten in flight, and conda falls back
		// to the plain JSON document on 404.
		pkgbase.NotFound(w)
	}
}

func (rr *Remote) serveFetched(w http.ResponseWriter, r *http.Request, rel, upath string, mutable, rewrite bool) {
	if err := rr.fetch(r.Context(), rel, upath, mutable, rewrite); err != nil {
		pkgbase.UpstreamError(w, err)
		return
	}
	if !pkgbase.ServeStored(w, r, rr.store.Store, rel, "application/json") {
		pkgbase.NotFound(w)
	}
}

type repodataEntry struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Timestamp int64  `json:"timestamp"`
}

// scanRepodata streams a repodata.json document, calling fn for every
// package entry without loading the whole document into memory.
func scanRepodata(r io.Reader, fn func(file string, e repodataEntry)) error {
	dec := json.NewDecoder(r)
	if _, err := dec.Token(); err != nil {
		return err
	}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := t.(string)
		if key != "packages" && key != "packages.conda" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return err
			}
			continue
		}
		if _, err := dec.Token(); err != nil {
			return err
		}
		for dec.More() {
			ft, err := dec.Token()
			if err != nil {
				return err
			}
			var e repodataEntry
			if err := dec.Decode(&e); err != nil {
				return err
			}
			if file, ok := ft.(string); ok {
				fn(file, e)
			}
		}
		if _, err := dec.Token(); err != nil {
			return err
		}
	}
	return nil
}

// Prefetch warms repodata for noarch plus every subdir already cached
// and pulls the files of name@version (or the latest version).
func (rr *Remote) Prefetch(ctx context.Context, name, version string) error {
	subdirs := []string{"noarch"}
	if dirs, _ := rr.store.ListDirs(indexDir); len(dirs) > 0 {
		for _, d := range dirs {
			if ValidSubdir(d) && !contains(subdirs, d) {
				subdirs = append(subdirs, d)
			}
		}
	}
	type hit struct{ subdir, file, version string }
	var hits []hit
	var firstErr error
	for _, sd := range subdirs {
		rel := rr.store.indexRel(sd, "repodata.json")
		if err := rr.fetch(ctx, rel, "/"+sd+"/repodata.json", true, true); err != nil {
			if firstErr == nil && !pkgbase.IsNotFound(err) {
				firstErr = err
			}
			continue
		}
		rc, _, err := rr.store.Open(rel)
		if err != nil {
			continue
		}
		_ = scanRepodata(rc, func(file string, e repodataEntry) {
			if e.Name == name && (version == "" || e.Version == version) {
				hits = append(hits, hit{sd, file, e.Version})
			}
		})
		rc.Close()
	}
	if len(hits) == 0 {
		if firstErr != nil {
			return firstErr
		}
		return registry.ErrPackageNotFound
	}
	if version == "" {
		vs := make([]string, 0, len(hits))
		for _, h := range hits {
			vs = append(vs, h.version)
		}
		version = pkgbase.Latest(vs)
	}
	for _, h := range hits {
		if h.version != version {
			continue
		}
		if err := rr.cachePackage(ctx, h.subdir, h.file); err != nil {
			return err
		}
	}
	return nil
}

// ── Virtual ──

// Virtual merges repodata.json / channeldata.json across members and
// serves package files first-hit.
type Virtual struct {
	*pkgbase.Virtual
}

func NewVirtualFactory(resolver virtualbase.Resolver) registry.Factory {
	return func(_ context.Context, _ registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		v, err := pkgbase.NewVirtual(typ, resolver, ns, r)
		if err != nil {
			return nil, err
		}
		return &Virtual{Virtual: v}, nil
	}
}

func (v *Virtual) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) { return classify(r) }

func (v *Virtual) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !pkgbase.IsRead(r) {
		pkgbase.Error(w, http.StatusMethodNotAllowed, "virtual repositories are read-only; publish to a local member")
		return
	}
	p := strings.Trim(r.URL.Path, "/")
	if p == "channeldata.json" {
		body, err := mergeChannelData(v.CollectMembers(r))
		if err != nil {
			pkgbase.Error(w, http.StatusBadGateway, err.Error())
			return
		}
		pkgbase.WriteBytes(w, r, http.StatusOK, "application/json", body)
		return
	}
	subdir, file, ok := splitPath(p)
	if !ok {
		pkgbase.NotFound(w)
		return
	}
	if indexDocs[file] {
		bodies := v.CollectMembers(r)
		if len(bodies) == 0 {
			pkgbase.NotFound(w)
			return
		}
		body, err := mergeRepodata(subdir, bodies)
		if err != nil {
			pkgbase.Error(w, http.StatusBadGateway, err.Error())
			return
		}
		pkgbase.WriteBytes(w, r, http.StatusOK, "application/json", body)
		return
	}
	if !validFilename(file) {
		// Compressed index variants would bypass the merge.
		pkgbase.NotFound(w)
		return
	}
	if !v.ServeFirstHit(w, r) {
		pkgbase.NotFound(w)
	}
}

// mergeRepodata unions member repodata documents; the first member
// wins on an identical filename.
func mergeRepodata(subdir string, bodies [][]byte) ([]byte, error) {
	out := emptyRepodata(subdir)
	removed := map[string]bool{}
	for _, b := range bodies {
		var rd Repodata
		if err := json.Unmarshal(b, &rd); err != nil {
			return nil, errors.New("member returned invalid repodata.json")
		}
		for k, val := range rd.Packages {
			if _, ok := out.Packages[k]; !ok {
				out.Packages[k] = val
			}
		}
		for k, val := range rd.PackagesConda {
			if _, ok := out.PackagesConda[k]; !ok {
				out.PackagesConda[k] = val
			}
		}
		for _, f := range rd.Removed {
			if !removed[f] {
				removed[f] = true
				out.Removed = append(out.Removed, f)
			}
		}
	}
	return pkgbase.MarshalJSON(out)
}

func mergeChannelData(bodies [][]byte) ([]byte, error) {
	pkgs := map[string]json.RawMessage{}
	subdirs := map[string]bool{}
	for _, b := range bodies {
		var cd struct {
			Packages map[string]json.RawMessage `json:"packages"`
			Subdirs  []string                   `json:"subdirs"`
		}
		if json.Unmarshal(b, &cd) != nil {
			continue
		}
		for k, val := range cd.Packages {
			if _, ok := pkgs[k]; !ok {
				pkgs[k] = val
			}
		}
		for _, s := range cd.Subdirs {
			subdirs[s] = true
		}
	}
	sd := make([]string, 0, len(subdirs))
	for s := range subdirs {
		sd = append(sd, s)
	}
	sort.Strings(sd)
	return pkgbase.MarshalJSON(map[string]any{"channeldata_version": 1, "packages": pkgs, "subdirs": sd})
}
