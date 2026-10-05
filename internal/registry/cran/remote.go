package cran

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

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
	_ registry.ArtifactClassifier   = (*Virtual)(nil)
)

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
		base, err := pkgbase.NewRemoteRepo(deps, typ, ns, r, "/src/contrib/PACKAGES.gz")
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

// fetch makes sure rel holds the upstream file at upath, streaming it
// into the store. Mutable files are refreshed after MutableTTL; on
// upstream failure a stale copy is kept.
func (rr *Remote) fetch(ctx context.Context, rel, upath string, mutable bool) error {
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
	if err := rr.store.WriteStream(rel, resp.Body, resp.ContentLength); err != nil {
		_ = rr.store.Delete(rel)
		return err
	}
	return nil
}

// cacheFile pulls the package file at wp. A source tarball missing
// from src/contrib upstream (moved to Archive since the client read
// PACKAGES) is retried under Archive/{pkg}/.
func (rr *Remote) cacheFile(ctx context.Context, wp wirePath, wire string) error {
	rel := rr.store.fileRel(wire)
	err := rr.fetch(ctx, rel, "/"+wire, false)
	if err != nil && pkgbase.IsNotFound(err) && wp.dir == srcDir && !strings.Contains(wire, "/Archive/") {
		name, _, _, _ := SplitFilename(wp.file)
		err = rr.fetch(ctx, rel, "/"+archivePath(name, wp.file), false)
	}
	if err != nil {
		return err
	}
	if !rr.store.Exists(rr.store.recordRel(wp.dir, wp.file)) {
		rr.recordCached(wp, wire)
	}
	return nil
}

func (rr *Remote) recordCached(wp wirePath, wire string) {
	rc, fi, err := rr.store.Open(rr.store.fileRel(wire))
	if err != nil {
		return
	}
	defer rc.Close()
	if fi.Size > maxParseInMemory {
		return
	}
	body, err := io.ReadAll(rc)
	if err != nil {
		return
	}
	desc, err := ReadDescription(body)
	if err != nil {
		return
	}
	_ = rr.store.PutRecord(newRecord(wp.dir, wp.file, wire, desc, body))
}

func (rr *Remote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !pkgbase.IsRead(r) {
		pkgbase.Error(w, http.StatusMethodNotAllowed, "remote registry is read-only")
		return
	}
	wp, ok := parseWire(r.URL.Path)
	if !ok {
		pkgbase.NotFound(w)
		return
	}
	wire := strings.Trim(r.URL.Path, "/")
	if wp.index {
		rel := path.Join(indexDir, wire)
		if err := rr.fetch(r.Context(), rel, "/"+wire, true); err != nil {
			pkgbase.UpstreamError(w, err)
			return
		}
		if !pkgbase.ServeStored(w, r, rr.store.Store, rel, indexContentType(wp.file)) {
			pkgbase.NotFound(w)
		}
		return
	}
	if serveFile(w, r, rr.store, wp) {
		return
	}
	if err := rr.cacheFile(r.Context(), wp, wire); err != nil {
		pkgbase.UpstreamError(w, err)
		return
	}
	if !pkgbase.ServeStored(w, r, rr.store.Store, rr.store.fileRel(wire), "application/octet-stream") {
		pkgbase.NotFound(w)
	}
}

func indexContentType(file string) string {
	switch {
	case strings.HasSuffix(file, ".gz"):
		return "application/x-gzip"
	case strings.HasSuffix(file, ".rds"):
		return "application/octet-stream"
	}
	return "text/plain; charset=utf-8"
}

// Prefetch warms src/contrib/PACKAGES and the source tarball of
// name@version (or the version PACKAGES currently lists).
func (rr *Remote) Prefetch(ctx context.Context, name, version string) error {
	if version == "" {
		rel := path.Join(indexDir, srcDir, "PACKAGES")
		if err := rr.fetch(ctx, rel, "/"+srcDir+"/PACKAGES", true); err != nil {
			return err
		}
		rc, _, err := rr.store.Open(rel)
		if err != nil {
			return err
		}
		stanzas, err := ParseDCF(rc)
		rc.Close()
		if err != nil {
			return err
		}
		for _, s := range stanzas {
			if s.Get("Package") == name {
				version = s.Get("Version")
			}
		}
		if version == "" {
			return registry.ErrPackageNotFound
		}
	}
	file := name + "_" + version + ".tar.gz"
	if _, _, _, ok := SplitFilename(file); !ok {
		return registry.ErrPackageNotFound
	}
	wp := wirePath{dir: srcDir, file: file}
	if rr.store.Exists(rr.store.recordRel(srcDir, file)) {
		return nil
	}
	return rr.cacheFile(ctx, wp, path.Join(srcDir, file))
}

// ── Virtual ──

// Virtual merges PACKAGES across members (highest version wins per
// package) and serves package files first-hit.
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
	wp, ok := parseWire(r.URL.Path)
	if !ok {
		pkgbase.NotFound(w)
		return
	}
	if !wp.index {
		if !v.ServeFirstHit(w, r) {
			pkgbase.NotFound(w)
		}
		return
	}
	if wp.file != "PACKAGES" && wp.file != "PACKAGES.gz" {
		// PACKAGES.rds cannot be merged; R falls back to PACKAGES.gz.
		pkgbase.NotFound(w)
		return
	}
	// Members are asked for the gzip form (remote mirrors may only
	// carry that one) and decompressed before merging.
	mr := r.Clone(r.Context())
	mr.Method = http.MethodGet
	mr.URL.Path = "/" + path.Join(wp.dir, "PACKAGES.gz")
	bodies := v.CollectMembers(mr)
	if len(bodies) == 0 {
		pkgbase.NotFound(w)
		return
	}
	body := mergePackages(bodies)
	ct := "text/plain; charset=utf-8"
	if wp.file == "PACKAGES.gz" {
		body, ct = gzipBytes(body), "application/x-gzip"
	}
	w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
	pkgbase.WriteBytes(w, r, http.StatusOK, ct, body)
}

// mergePackages unions PACKAGES documents (plain or gzip); the highest
// version of each package wins, the earliest member breaks ties.
func mergePackages(bodies [][]byte) []byte {
	best := map[string]*Stanza{}
	for _, b := range bodies {
		if bytes.HasPrefix(b, []byte{0x1f, 0x8b}) {
			zr, err := gzip.NewReader(bytes.NewReader(b))
			if err != nil {
				continue
			}
			plain, err := io.ReadAll(zr)
			if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
				continue
			}
			b = plain
		}
		stanzas, err := ParseDCF(bytes.NewReader(b))
		if err != nil {
			continue
		}
		for _, s := range stanzas {
			name := s.Get("Package")
			if name == "" {
				continue
			}
			if cur, ok := best[name]; !ok || CompareRVersions(s.Get("Version"), cur.Get("Version")) > 0 {
				best[name] = s
			}
		}
	}
	names := make([]string, 0, len(best))
	for n := range best {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]*Stanza, 0, len(names))
	for _, n := range names {
		out = append(out, best[n])
	}
	return WriteDCF(out)
}
