package rpm

import (
	"context"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/service"
)

// Remote is a pull-through cache of an rpm-md mirror.
type Remote struct {
	*pkgbase.RemoteRepo
	store *Store

	mu       sync.Mutex
	idxHref  string
	idxCache []*remotePkg
}

func NewRemoteFactory() registry.Factory {
	return func(_ context.Context, deps registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		base, err := pkgbase.NewRemoteRepo(deps, typ, ns, r, "/"+repomdRel)
		if err != nil {
			return nil, err
		}
		return &Remote{RemoteRepo: base, store: &Store{base.Store}}, nil
	}
}

func (rr *Remote) Store() *Store { return rr.store }

var checksumNamed = regexp.MustCompile(`^[0-9a-f]{32,128}-`)

// mutablePath reports whether p (base-relative) is TTL-bound metadata.
func mutablePath(p string) bool {
	if strings.HasSuffix(p, ".rpm") {
		return false
	}
	if strings.HasPrefix(p, repodataDir+"/") && checksumNamed.MatchString(path.Base(p)) {
		return false
	}
	return true
}

func validPath(p string) bool {
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

func (rr *Remote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !pkgbase.IsRead(r) {
		pkgbase.Error(w, http.StatusMethodNotAllowed, "remote registry is read-only")
		return
	}
	p := strings.Trim(r.URL.Path, "/")
	switch {
	case p == "":
		pkgs, err := rr.ListPackages(r.Context())
		if err != nil {
			pkgbase.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{"packages": pkgs})
		return
	case isRepoFile(p):
		pkgbase.WriteBytes(w, r, http.StatusOK, "text/plain; charset=utf-8", repoFile(r, rr.NS, rr.RepoName, false))
		return
	case !validPath(p):
		pkgbase.NotFound(w)
		return
	}
	cacheRel := path.Join("cache", p)
	mutable := mutablePath(p)
	if !mutable && pkgbase.ServeStored(w, r, rr.store.Store, cacheRel, contentType(p)) {
		return
	}
	rr.ServeCached(w, r, cacheRel, "/"+p, contentType(p), mutable)
}

// index returns the parsed upstream primary.xml from cache (fetching
// when fetch is set).
func (rr *Remote) index(ctx context.Context, fetch bool) ([]*remotePkg, error) {
	var repomd []byte
	var err error
	cacheRepomd := path.Join("cache", repomdRel)
	if fetch {
		repomd, err = rr.FetchCached(ctx, cacheRepomd, "/"+repomdRel, true)
	} else {
		repomd, err = rr.store.Read(cacheRepomd)
	}
	if err != nil {
		if pkgbase.IsNotFound(err) && !fetch {
			return nil, nil
		}
		return nil, err
	}
	hrefs, err := repomdHrefs(repomd)
	if err != nil {
		return nil, err
	}
	href := strings.Trim(hrefs["primary"], "/")
	if href == "" || !validPath(href) {
		return nil, errNoPrimary
	}
	rr.mu.Lock()
	if rr.idxHref == href {
		out := rr.idxCache
		rr.mu.Unlock()
		return out, nil
	}
	rr.mu.Unlock()
	cacheRel := path.Join("cache", href)
	var raw []byte
	if fetch {
		raw, err = rr.FetchCached(ctx, cacheRel, "/"+href, mutablePath(href))
	} else {
		raw, err = rr.store.Read(cacheRel)
	}
	if err != nil {
		if pkgbase.IsNotFound(err) && !fetch {
			return nil, nil
		}
		return nil, err
	}
	rd, err := openDecompressed(raw)
	if err != nil {
		return nil, err
	}
	pkgs, err := parsePrimaryStream(rd)
	if err != nil && len(pkgs) == 0 {
		return nil, err
	}
	rr.mu.Lock()
	rr.idxHref, rr.idxCache = href, pkgs
	rr.mu.Unlock()
	return pkgs, nil
}

func (rr *Remote) ListPackages(ctx context.Context) ([]registry.PackageSummary, error) {
	pkgs, err := rr.index(ctx, false)
	if err != nil {
		return nil, err
	}
	sets := map[string]map[string]struct{}{}
	var names []string
	for _, p := range pkgs {
		if sets[p.Name] == nil {
			sets[p.Name] = map[string]struct{}{}
			names = append(names, p.Name)
		}
		sets[p.Name][p.VR] = struct{}{}
	}
	sort.Strings(names)
	out := make([]registry.PackageSummary, 0, len(names))
	for _, n := range names {
		out = append(out, registry.PackageSummary{Name: n, Versions: setToSorted(sets[n])})
	}
	return out, nil
}

func (rr *Remote) Stats(ctx context.Context) (registry.Stats, error) {
	return pkgbase.StatsOf(ctx, rr, rr.store.Store), nil
}

func (rr *Remote) byName(ctx context.Context, name string) ([]*Package, error) {
	pkgs, err := rr.index(ctx, false)
	if err != nil {
		return nil, err
	}
	var out []*Package
	for _, p := range pkgs {
		if p.Name != name {
			continue
		}
		vr := strings.SplitN(p.VR, "-", 2)
		pk := &Package{
			Name: p.Name, Arch: p.Arch, Epoch: p.Epoch, Version: vr[0], Summary: p.Summary, URL: p.URL,
			License: p.License, BuildTime: p.BuildTime, PackageSize: p.Size, SHA256: p.SHA256, Location: p.Href,
		}
		if len(vr) == 2 {
			pk.Release = vr[1]
		}
		out = append(out, pk)
	}
	sortPackages(out)
	return out, nil
}

func (rr *Remote) PackageDetail(ctx context.Context, name string) (*registry.PackageDetail, error) {
	pkgs, err := rr.byName(ctx, name)
	if err != nil {
		return nil, err
	}
	return detail(pkgs, name)
}

func (rr *Remote) ArtifactInfo(ctx context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	pkgs, err := rr.byName(ctx, ref.Name)
	if err != nil {
		return registry.ArtifactMeta{}, err
	}
	for _, p := range pkgs {
		if matchesVersion(p, ref.Version) {
			m := registry.ArtifactMeta{License: p.License}
			if p.BuildTime > 0 {
				m.PublishedAt = time.Unix(p.BuildTime, 0).UTC()
			}
			return m, nil
		}
	}
	return registry.ArtifactMeta{}, registry.ErrPackageNotFound
}

func (rr *Remote) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) {
	return classify(r)
}

func (rr *Remote) PurgeCache(_ context.Context, opts registry.PurgeOptions) (registry.PurgeStats, error) {
	rr.mu.Lock()
	rr.idxHref, rr.idxCache = "", nil
	rr.mu.Unlock()
	if opts.All {
		return rr.Purge(opts), nil
	}
	var out registry.PurgeStats
	_ = rr.store.Walk("cache", func(rel string, e rawfs.DirEntry) error {
		if !mutablePath(strings.TrimPrefix(rel, "cache/")) {
			out.Skipped++
			return nil
		}
		if err := rr.store.Delete(rel); err != nil {
			out.Errors = append(out.Errors, err.Error())
			return nil
		}
		out.PurgedFiles++
		out.PurgedBytes += e.Size
		return nil
	})
	return out, nil
}

// Prefetch refreshes repodata and caches the requested (or latest)
// version of name for every architecture.
func (rr *Remote) Prefetch(ctx context.Context, name, version string) error {
	pkgs, err := rr.index(ctx, true)
	if err != nil {
		return err
	}
	var hit []*remotePkg
	var vrs []string
	for _, p := range pkgs {
		if p.Name == name {
			hit = append(hit, p)
			vrs = append(vrs, p.VR)
		}
	}
	if len(hit) == 0 {
		return registry.ErrPackageNotFound
	}
	if version == "" {
		version = pkgbase.Latest(vrs)
	}
	n := 0
	for _, p := range hit {
		if p.VR != version && p.EVR() != version && p.Epoch+":"+p.VR != version {
			continue
		}
		href := strings.Trim(p.Href, "/")
		if !validPath(href) {
			continue
		}
		if _, err := rr.FetchCached(ctx, path.Join("cache", href), "/"+href, false); err != nil {
			return err
		}
		n++
	}
	if n == 0 {
		return registry.ErrPackageNotFound
	}
	return nil
}
