package maven

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/common"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/registry/upstream"
	"github.com/rakunlabs/kutu/internal/service"
)

var (
	_ registry.PackageLister        = (*Remote)(nil)
	_ registry.ArtifactClassifier   = (*Remote)(nil)
	_ registry.ArtifactInfoProvider = (*Remote)(nil)
	_ registry.Prefetcher           = (*Remote)(nil)
	_ registry.CachePurger          = (*Remote)(nil)
	_ registry.UpstreamProber       = (*Remote)(nil)
)

// Remote is a pull-through cache of an upstream Maven repository
// (Maven Central, the Gradle plugin portal, a corporate Nexus, …).
type Remote struct {
	namespace  string
	name       string
	store      *Store
	router     *upstream.Router
	mutableTTL time.Duration
	sf         *common.Singleflight
	// published caches upstream Last-Modified times by path.
	published sync.Map
}

func NewRemoteFactory() registry.Factory {
	return func(_ context.Context, deps registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		b, err := upstream.BuildRemote(deps, "maven/remote", ns, r, 5*time.Minute)
		if err != nil {
			return nil, err
		}
		return &Remote{
			namespace:  ns,
			name:       r.Name,
			store:      NewStore(b.FS, b.BasePath),
			router:     upstream.NewRouter(b.Client, b.Upstreams),
			mutableTTL: b.MutableTTL,
			sf:         common.NewSingleflight(),
		}, nil
	}
}

func (rr *Remote) Namespace() string { return rr.namespace }
func (rr *Remote) Name() string      { return rr.name }
func (rr *Remote) Type() string      { return service.RegistryTypeMaven }
func (rr *Remote) Kind() string      { return service.RegistryKindRemote }
func (rr *Remote) Store() *Store     { return rr.store }
func (rr *Remote) Close() error {
	if rr.router != nil {
		return rr.router.Close()
	}
	return nil
}

func (rr *Remote) Stats(context.Context) (registry.Stats, error) {
	return storeStats(rr.store), nil
}

func (rr *Remote) PackageDetail(_ context.Context, name string) (*registry.PackageDetail, error) {
	return packageDetail(rr.store, name)
}

func (rr *Remote) ProbeUpstream(ctx context.Context) (registry.UpstreamHealth, error) {
	return upstream.Probe(ctx, rr.router.Default(), "/"), nil
}

func (rr *Remote) PurgeCache(_ context.Context, opts registry.PurgeOptions) (registry.PurgeStats, error) {
	count, bytes, errs := rr.store.PurgeMutable()
	if opts.All {
		count, bytes, errs = rr.store.PurgeAll()
	}
	out := registry.PurgeStats{PurgedFiles: count, PurgedBytes: bytes}
	for _, err := range errs {
		out.Errors = append(out.Errors, err.Error())
	}
	return out, nil
}

func (rr *Remote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "remote registry is read-only", http.StatusMethodNotAllowed)
		return
	}
	rel := cleanRel(r.URL.Path)
	if rel == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	body, err := rr.fetch(r.Context(), rel)
	if err != nil {
		if errors.Is(err, upstream.ErrNotFound) || errors.Is(err, registry.ErrPackageNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeBody(w, r, rel, body)
}

// fresh reports whether the cached rel can be served without
// revalidating against the upstream.
func (rr *Remote) fresh(rel string) bool {
	fi, err := rr.store.Stat(rel)
	if err != nil || fi.IsDir {
		return false
	}
	if !isMutablePath(rel) {
		return true
	}
	return rr.mutableTTL > 0 && time.Since(fi.ModTime) < rr.mutableTTL
}

// fetch returns rel from cache or upstream (caching the result). A
// missing upstream checksum is computed from the artifact itself.
func (rr *Remote) fetch(ctx context.Context, rel string) ([]byte, error) {
	if rr.fresh(rel) {
		if b, err := rr.store.Read(rel); err == nil {
			return b, nil
		}
	}
	v, err, _ := rr.sf.Do(rel, func() (any, error) {
		return rr.fetchUpstream(ctx, rel)
	})
	if err != nil {
		base, algo := splitChecksum(rel)
		if algo == "" || !errors.Is(err, upstream.ErrNotFound) {
			return nil, err
		}
		content, cerr := rr.fetch(ctx, base)
		if cerr != nil {
			return nil, err
		}
		sum := []byte(checksumHex(algo, content))
		_ = rr.store.Write(rel, sum)
		return sum, nil
	}
	return v.([]byte), nil
}

func (rr *Remote) fetchUpstream(ctx context.Context, rel string) ([]byte, error) {
	resp, err := rr.router.For(rel).Get(ctx, "/"+rel)
	if err != nil {
		// Serve a stale mutable copy when the upstream flakes.
		if !errors.Is(err, upstream.ErrNotFound) {
			if b, rerr := rr.store.Read(rel); rerr == nil {
				return b, nil
			}
		}
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read upstream: %w", err)
	}
	_ = rr.store.Write(rel, body)
	rr.notePublished(rel, resp.LastModified)
	return body, nil
}

func (rr *Remote) notePublished(rel, lastModified string) {
	if lastModified == "" {
		return
	}
	if t, err := http.ParseTime(lastModified); err == nil {
		rr.published.Store(rel, t)
	}
}

// publishedAt returns the upstream Last-Modified of rel (HEAD on a
// cache miss), or zero when unknown. The cache mtime is deliberately
// not used: it is the fetch time, not the publish time.
func (rr *Remote) publishedAt(ctx context.Context, rel string) time.Time {
	if t, ok := rr.published.Load(rel); ok {
		return t.(time.Time)
	}
	resp, err := rr.router.For(rel).Head(ctx, "/"+rel)
	if err != nil {
		return time.Time{}
	}
	resp.Body.Close()
	rr.notePublished(rel, resp.LastModified)
	if t, ok := rr.published.Load(rel); ok {
		return t.(time.Time)
	}
	return time.Time{}
}

// ListPackages implements registry.PackageLister over the cache.
func (rr *Remote) ListPackages(context.Context) ([]registry.PackageSummary, error) {
	return listPackages(rr.store)
}

// ClassifyRequest implements registry.ArtifactClassifier.
func (rr *Remote) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) {
	return classify(r)
}

// ArtifactInfo implements registry.ArtifactInfoProvider. The POM is
// fetched (and cached) when missing so the license is known before
// the download proceeds; publish time is the upstream Last-Modified
// of the POM (zero when the upstream does not send one).
func (rr *Remote) ArtifactInfo(ctx context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	group, artifact := splitName(ref.Name)
	if group == "" || artifact == "" || ref.Version == "" || strings.ContainsAny(ref.Version, "/\\") {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	pomRel := versionFile(group, artifact, ref.Version, ".pom")
	body, err := rr.fetch(ctx, pomRel)
	if err != nil {
		if errors.Is(err, upstream.ErrNotFound) {
			return registry.ArtifactMeta{}, registry.ErrPackageNotFound
		}
		return registry.ArtifactMeta{}, err
	}
	return registry.ArtifactMeta{License: parsePOM(body).license(), PublishedAt: rr.publishedAt(ctx, pomRel)}, nil
}

// Prefetch implements registry.Prefetcher: warms maven-metadata.xml,
// then the POM and main artifact of version (or the metadata's
// release / latest when version is "").
func (rr *Remote) Prefetch(ctx context.Context, name, version string) error {
	group, artifact := splitName(name)
	if group == "" || artifact == "" {
		return fmt.Errorf("maven: invalid artifact %q: %w", name, registry.ErrInvalidPackageName)
	}
	metaRel := path.Join(groupPath(group), artifact, metadataFile)
	metaBody, metaErr := rr.fetch(ctx, metaRel)
	if version == "" {
		if metaErr != nil {
			return metaErr
		}
		doc, err := parseMetadata(metaBody)
		if err != nil {
			return fmt.Errorf("maven: parse %s: %w", metaRel, err)
		}
		if doc.Versioning != nil {
			version = doc.Versioning.Release
			if version == "" {
				version = doc.Versioning.Latest
			}
		}
		if version == "" {
			version = latestRelease(doc.versions())
		}
		if version == "" {
			version = pkgbase.Latest(doc.versions())
		}
		if version == "" {
			return registry.ErrPackageNotFound
		}
	}
	if strings.ContainsAny(version, "/\\") {
		return registry.ErrPackageNotFound
	}
	pom, err := rr.fetch(ctx, versionFile(group, artifact, version, ".pom"))
	if err != nil {
		return fmt.Errorf("maven: prefetch %s@%s pom: %w", name, version, err)
	}
	if isSnapshot(version) {
		_, _ = rr.fetch(ctx, path.Join(groupPath(group), artifact, version, metadataFile))
	}
	if ext := mainExt(parsePOM(pom).Packaging); ext != "" {
		if _, err := rr.fetch(ctx, versionFile(group, artifact, version, "."+ext)); err != nil && !(errors.Is(err, upstream.ErrNotFound) && ext != "jar") {
			return fmt.Errorf("maven: prefetch %s@%s %s: %w", name, version, ext, err)
		}
	}
	return nil
}
