package apt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp/clearsign"

	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/service"
)

// ── Remote ──

type Remote struct {
	*pkgbase.RemoteRepo
	store *Store
}

func NewRemoteFactory() registry.Factory {
	return func(_ context.Context, deps registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		base, err := pkgbase.NewRemoteRepo(deps, typ, ns, r, "/dists/")
		if err != nil {
			return nil, err
		}
		return &Remote{RemoteRepo: base, store: &Store{base.Store}}, nil
	}
}

func (rr *Remote) Store() *Store { return rr.store }

func (rr *Remote) ListPackages(context.Context) ([]registry.PackageSummary, error) {
	scan, err := rr.store.poolScan()
	if err != nil {
		return nil, err
	}
	m := map[string]map[string]bool{}
	for name, vers := range scan {
		m[name] = map[string]bool{}
		for v := range vers {
			m[name][v] = true
		}
	}
	return summaries(m), nil
}

func (rr *Remote) Stats(ctx context.Context) (registry.Stats, error) {
	return pkgbase.StatsOf(ctx, rr, rr.store.Store), nil
}

func (rr *Remote) PackageDetail(_ context.Context, name string) (*registry.PackageDetail, error) {
	scan, err := rr.store.poolScan()
	if err != nil {
		return nil, err
	}
	vers := scan[name]
	if len(vers) == 0 {
		return nil, registry.ErrPackageNotFound
	}
	set := map[string]bool{}
	for v := range vers {
		set[v] = true
	}
	vs := sortedVersions(set)
	d := &registry.GenericPackageDetail{LatestVersion: vs[len(vs)-1]}
	for i := len(vs) - 1; i >= 0; i-- {
		row := registry.GenericVersionDetail{Version: vs[i], Files: vers[vs[i]]}
		for _, f := range row.Files {
			row.Size += f.Size
		}
		d.Versions = append(d.Versions, row)
	}
	return &registry.PackageDetail{Type: typ, Name: name, Generic: d}, nil
}

func (rr *Remote) PurgeCache(_ context.Context, opts registry.PurgeOptions) (registry.PurgeStats, error) {
	return rr.Purge(opts, distsDir), nil
}

func (rr *Remote) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) { return classify(r) }

// splitDist splits "dists/{dist}/{rest}".
func splitDist(p string) (dist, rest string, ok bool) {
	parts := strings.SplitN(strings.TrimPrefix(p, distsDir+"/"), "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// releaseText returns the Release document body from a Release or
// clearsigned InRelease file.
func releaseText(b []byte) []byte {
	if bytes.HasPrefix(bytes.TrimSpace(b), []byte("-----BEGIN PGP SIGNED MESSAGE-----")) {
		if blk, _ := clearsign.Decode(b); blk != nil {
			return blk.Plaintext
		}
		return nil
	}
	return b
}

// cachedReleaseHashes returns the SHA256 list of the cached Release
// (or InRelease) of dist.
func (rr *Remote) cachedReleaseHashes(dist string) map[string]string {
	for _, n := range []string{"InRelease", "Release"} {
		if b, err := rr.store.Read(distsDir + "/" + dist + "/" + n); err == nil {
			if txt := releaseText(b); txt != nil {
				return releaseFiles(txt)
			}
		}
	}
	return nil
}

// fetch returns the bytes for a base-relative path, applying the
// apt-specific cache rules.
func (rr *Remote) fetch(ctx context.Context, p string) ([]byte, error) {
	upath := "/" + p
	switch {
	case strings.HasPrefix(p, poolDir+"/"), strings.Contains(p, "/by-hash/"):
		return rr.FetchCached(ctx, p, upath, false)
	case strings.HasPrefix(p, distsDir+"/"):
	default:
		return nil, fmt.Errorf("apt remote: %w", errNotServed)
	}
	dist, rest, ok := splitDist(p)
	if !ok {
		return nil, errNotServed
	}
	switch rest {
	case "InRelease":
		return rr.FetchCached(ctx, p, upath, true)
	case "Release":
		old, _ := rr.store.Read(p)
		b, err := rr.FetchCached(ctx, p, upath, true)
		if err == nil && !bytes.Equal(old, b) {
			_ = rr.store.Delete(distsDir + "/" + dist + "/Release.gpg")
		}
		return b, err
	case "Release.gpg":
		// The detached signature follows its Release: it is only
		// refreshed together with it.
		relRel := distsDir + "/" + dist + "/Release"
		if _, err := rr.fetch(ctx, relRel); err != nil {
			return nil, err
		}
		if b, err := rr.store.Read(p); err == nil {
			return b, nil
		}
		return rr.fetchUpstream(ctx, p, upath)
	}
	// Index files are pinned to the hash listed by the cached Release
	// so apt never sees a Packages file that disagrees with it.
	if want, listed := rr.cachedReleaseHashes(dist)[rest]; listed {
		if b, err := rr.store.Read(p); err == nil && pkgbase.SHA256Hex(b) == want {
			return b, nil
		}
		b, err := rr.fetchUpstream(ctx, p, upath)
		if err == nil || pkgbase.IsNotFound(err) {
			return b, err
		}
		if b, rerr := rr.store.Read(p); rerr == nil {
			return b, nil
		}
		return nil, err
	}
	return rr.FetchCached(ctx, p, upath, true)
}

// fetchUpstream always downloads upath and caches it at rel.
func (rr *Remote) fetchUpstream(ctx context.Context, rel, upath string) ([]byte, error) {
	client := rr.Client
	if rr.Router != nil {
		client = rr.Router.For(upath)
	}
	resp, err := client.Get(ctx, upath)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	_ = rr.store.Write(rel, b)
	return b, nil
}

var errNotServed = errors.New("not found")

func (rr *Remote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !pkgbase.IsRead(r) {
		pkgbase.Error(w, http.StatusMethodNotAllowed, "remote registry is read-only")
		return
	}
	p := strings.Trim(path.Clean("/"+r.URL.Path), "/")
	if !strings.HasPrefix(p, distsDir+"/") && !strings.HasPrefix(p, poolDir+"/") {
		pkgbase.NotFound(w)
		return
	}
	if strings.HasPrefix(p, poolDir+"/") && pkgbase.ServeStored(w, r, rr.store.Store, p, contentType(p)) {
		return
	}
	b, err := rr.fetch(r.Context(), p)
	if err != nil {
		if errors.Is(err, errNotServed) {
			pkgbase.NotFound(w)
			return
		}
		pkgbase.UpstreamError(w, err)
		return
	}
	pkgbase.WriteBytes(w, r, http.StatusOK, contentType(p), b)
}

// cachedPackages yields every cached Packages index (any compression).
func (rr *Remote) cachedPackages(fn func(st []stanza)) {
	_ = rr.store.Walk(distsDir, func(rel string, _ rawfs.DirEntry) error {
		base := path.Base(rel)
		if !strings.HasPrefix(base, "Packages") || strings.Contains(rel, "/by-hash/") {
			return nil
		}
		b, err := rr.store.Read(rel)
		if err != nil {
			return nil
		}
		if raw, err := decompressAll(base, b); err == nil {
			fn(parseStanzas(raw))
		}
		return nil
	})
}

// Prefetch refreshes the cached Release files and downloads the .deb
// files of name@version (latest when empty) listed in any cached
// Packages index. At least one `apt update` through the repository is
// required so a Packages index is cached.
func (rr *Remote) Prefetch(ctx context.Context, name, version string) error {
	dists, _ := rr.store.ListDirs(distsDir)
	for _, d := range dists {
		for _, f := range []string{"InRelease", "Release"} {
			if rr.store.Exists(distsDir + "/" + d + "/" + f) {
				_, _ = rr.fetch(ctx, distsDir+"/"+d+"/"+f)
			}
		}
	}
	files := map[string]string{} // filename → version
	rr.cachedPackages(func(st []stanza) {
		for _, s := range st {
			if s.Get("Package") == name && s.Get("Filename") != "" {
				files[s.Get("Filename")] = s.Get("Version")
			}
		}
	})
	if len(files) == 0 {
		return fmt.Errorf("apt prefetch %s: %w (no cached Packages index lists it)", name, registry.ErrPackageNotFound)
	}
	if version == "" {
		var vs []string
		for _, v := range files {
			vs = append(vs, v)
		}
		version = pkgbase.Latest(vs)
	}
	n := 0
	for fn, v := range files {
		if v != version && stripEpoch(v) != version {
			continue
		}
		fn = strings.Trim(path.Clean("/"+fn), "/")
		if !strings.HasPrefix(fn, poolDir+"/") {
			continue
		}
		if _, err := rr.fetch(ctx, fn); err != nil {
			return err
		}
		n++
	}
	if n == 0 {
		return fmt.Errorf("apt prefetch %s@%s: %w", name, version, registry.ErrPackageNotFound)
	}
	return nil
}
