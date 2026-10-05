package npm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/rakunlabs/kutu/internal/hook"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/common/semver"
	"github.com/rakunlabs/kutu/internal/registry/events"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
)

var (
	_ registry.PackageLister        = (*Local)(nil)
	_ registry.VersionDeleter       = (*Local)(nil)
	_ registry.ArtifactClassifier   = (*Local)(nil)
	_ registry.ArtifactInfoProvider = (*Local)(nil)
	_ registry.VersionPromoter      = (*Local)(nil)
	_ registry.PackageLister        = (*Remote)(nil)
	_ registry.ArtifactClassifier   = (*Remote)(nil)
	_ registry.ArtifactInfoProvider = (*Remote)(nil)
	_ registry.Prefetcher           = (*Remote)(nil)
	_ registry.PackageLister        = (*Virtual)(nil)
	_ registry.ArtifactClassifier   = (*Virtual)(nil)
)

// rewritePackumentTarballs prefixes root-relative dist.tarball URLs
// ("/{pkg}/-/{file}") with publicBase. Absolute URLs are left alone.
func rewritePackumentTarballs(body []byte, publicBase string) []byte {
	if !bytes.Contains(body, []byte(`"tarball":"/`)) {
		return body
	}
	var pkg map[string]any
	if err := json.Unmarshal(body, &pkg); err != nil {
		return body
	}
	versions, _ := pkg["versions"].(map[string]any)
	base := strings.TrimRight(publicBase, "/")
	for _, vm := range versions {
		meta, _ := vm.(map[string]any)
		dist, _ := meta["dist"].(map[string]any)
		if t, ok := dist["tarball"].(string); ok && strings.HasPrefix(t, "/") {
			dist["tarball"] = base + t
		}
	}
	out, err := json.Marshal(pkg)
	if err != nil {
		return body
	}
	return out
}

// sortVersionsAsc orders versions oldest-first (semver, lenient).
func sortVersionsAsc(v []string) {
	sort.SliceStable(v, func(i, j int) bool { return semver.Compare(v[i], v[j], semver.ModeLenient) < 0 })
}

// highestRelease returns the highest non-prerelease version, falling
// back to the highest version overall.
func highestRelease(versions []string) string {
	vs := append([]string(nil), versions...)
	sortVersionsDesc(vs)
	for _, v := range vs {
		if !strings.Contains(v, "-") {
			return v
		}
	}
	if len(vs) > 0 {
		return vs[0]
	}
	return ""
}

// classifyArtifact maps an npm data-plane request to {name, version}.
// Tarballs carry the version parsed from "{bare}-{version}.tgz";
// packuments and dist-tag lookups carry the name only.
func classifyArtifact(r *http.Request) (registry.ArtifactRef, bool) {
	req := classify(r.Method, r.URL.Path)
	switch req.Op {
	case "tarball":
		return registry.ArtifactRef{Name: req.Pkg, Version: versionFromTarball(req.Pkg, req.File)}, true
	case "packument", "publish", "dist-tags", "dist-tag-set", "dist-tag-del":
		return registry.ArtifactRef{Name: req.Pkg}, true
	}
	return registry.ArtifactRef{}, false
}

// versionFromTarball extracts the version from a tarball filename
// ("lodash-4.17.21.tgz", scoped "@s/pkg" → "pkg-1.0.0.tgz").
func versionFromTarball(name, file string) string {
	bare := name
	if i := strings.LastIndexByte(bare, '/'); i >= 0 {
		bare = bare[i+1:]
	}
	f := strings.TrimSuffix(file, ".tgz")
	flat := strings.ReplaceAll(strings.TrimPrefix(name, "@"), "/", "-")
	for _, p := range []string{bare + "-", flat + "-"} {
		if v, ok := strings.CutPrefix(f, p); ok && v != "" && v[0] >= '0' && v[0] <= '9' {
			return v
		}
	}
	return ""
}

// metaFromVersion builds ArtifactMeta from a version meta blob and an
// optional packument time map.
func metaFromVersion(meta map[string]any, timeMap map[string]any, version string) registry.ArtifactMeta {
	out := registry.ArtifactMeta{License: parseLicense(meta["license"])}
	var ts string
	if t, ok := timeMap[version].(string); ok {
		ts = t
	} else if t, ok := meta["_publishedTime"].(string); ok {
		ts = t
	}
	if ts != "" {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			out.PublishedAt = t
		}
	}
	return out
}

// ListPackages implements registry.PackageLister.
func (l *Local) ListPackages(_ context.Context) ([]registry.PackageSummary, error) {
	names, err := l.store.ListPackages()
	if err != nil {
		return nil, err
	}
	out := make([]registry.PackageSummary, 0, len(names))
	for _, n := range names {
		vs, err := l.store.ListVersions(n)
		if err != nil {
			return nil, err
		}
		sortVersionsAsc(vs)
		out = append(out, registry.PackageSummary{Name: n, Versions: vs})
	}
	return out, nil
}

// DeleteVersion implements registry.VersionDeleter. The "latest"
// dist-tag is recomputed when it pointed at the removed version.
func (l *Local) DeleteVersion(_ context.Context, name, version string) error {
	if ValidatePackageName(name) != nil || strings.TrimSpace(version) == "" {
		return registry.ErrPackageNotFound
	}
	if err := l.store.DeleteVersion(name, version); err != nil {
		if errors.Is(err, ErrPackageNotFound) {
			return fmt.Errorf("%s@%s: %w", name, version, registry.ErrPackageNotFound)
		}
		return err
	}
	if tags, err := l.store.ReadDistTags(name); err == nil {
		if _, ok := tags["latest"]; !ok {
			if vs, _ := l.store.ListVersions(name); len(vs) > 0 {
				tags["latest"] = highestRelease(vs)
				_ = l.store.WriteDistTags(name, tags)
			}
		}
	}
	events.EmitSafe(l.emitter, hook.Event{
		Type:     hook.EventRegistryDeleted,
		Mount:    l.namespace,
		Path:     l.name + "/" + name + "@" + version,
		Protocol: "registry-npm",
	})
	return nil
}

// ClassifyRequest implements registry.ArtifactClassifier.
func (l *Local) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) {
	return classifyArtifact(r)
}

// ArtifactInfo implements registry.ArtifactInfoProvider.
func (l *Local) ArtifactInfo(_ context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	if ValidatePackageName(ref.Name) != nil || ref.Version == "" {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	meta, err := l.store.ReadVersionMeta(ref.Name, ref.Version)
	if err != nil {
		if errors.Is(err, ErrPackageNotFound) {
			return registry.ArtifactMeta{}, registry.ErrPackageNotFound
		}
		return registry.ArtifactMeta{}, err
	}
	return metaFromVersion(meta, nil, ref.Version), nil
}

// PromoteVersion implements registry.VersionPromoter: copies the
// tarball and version metadata of name@version into dst (an npm
// *Local) and recomputes dst's "latest" dist-tag.
func (l *Local) PromoteVersion(_ context.Context, dst registry.Registry, name, version string) error {
	d, ok := dst.(*Local)
	if !ok {
		return fmt.Errorf("npm: promote target %T is not an npm local registry", dst)
	}
	if ValidatePackageName(name) != nil || version == "" {
		return registry.ErrPackageNotFound
	}
	meta, err := l.store.ReadVersionMeta(name, version)
	if err != nil {
		if errors.Is(err, ErrPackageNotFound) {
			return registry.ErrPackageNotFound
		}
		return err
	}
	if exists, err := d.store.VersionMetaExists(name, version); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("%s@%s: %w", name, version, ErrVersionExists)
	}
	file := tarballFilenameFromMeta(meta)
	if file == "" {
		return fmt.Errorf("npm: %s@%s has no tarball reference", name, version)
	}
	rc, _, err := l.store.OpenTarball(name, file)
	if err != nil {
		return err
	}
	tarball, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return fmt.Errorf("npm: read tarball %s/%s: %w", name, file, err)
	}
	if err := d.store.WriteTarball(name, file, bytes.NewReader(tarball), int64(len(tarball))); err != nil {
		return err
	}
	// Root-relative URL: the serving request's public base is applied
	// when the packument is emitted.
	_ = RewriteVersionMetaTarball(meta, name, "")
	if err := d.store.WriteVersionMeta(name, version, meta); err != nil {
		return err
	}
	if !d.store.HasReadme(name) {
		if readme, _ := l.store.ReadReadme(name); readme != "" {
			_ = d.store.WriteReadme(name, readme)
		}
	}
	tags, _ := d.store.ReadDistTags(name)
	if tags == nil {
		tags = map[string]string{}
	}
	vs, _ := d.store.ListVersions(name)
	tags["latest"] = highestRelease(vs)
	if err := d.store.WriteDistTags(name, tags); err != nil {
		return err
	}
	events.EmitSafe(d.emitter, hook.Event{
		Type:     hook.EventRegistryPublished,
		Mount:    d.namespace,
		Path:     d.name + "/" + name + "@" + version,
		Protocol: "registry-npm",
		Size:     int64(len(tarball)),
	})
	return nil
}

// cachedPackument decodes the Remote's cached packument.
func (rr *Remote) cachedPackument(name string) (map[string]any, bool) {
	body, ok, _ := rr.store.ReadCachedPackument(name)
	if !ok || len(body) == 0 {
		return nil, false
	}
	var pkg map[string]any
	if err := json.Unmarshal(body, &pkg); err != nil {
		return nil, false
	}
	return pkg, true
}

// ListPackages implements registry.PackageLister over the cache.
// Versions are those whose tarball is cached.
func (rr *Remote) ListPackages(_ context.Context) ([]registry.PackageSummary, error) {
	var out []registry.PackageSummary
	walkNPMPackages(rr.store.RawFS(), rr.store.join("packages"), "", func(name string) {
		var vs []string
		if pkg, ok := rr.cachedPackument(name); ok {
			versions, _ := pkg["versions"].(map[string]any)
			for v, vm := range versions {
				meta, _ := vm.(map[string]any)
				if f := tarballFilenameFromMeta(meta); f != "" && rr.store.TarballSize(name, f) > 0 {
					vs = append(vs, v)
				}
			}
		}
		sortVersionsAsc(vs)
		out = append(out, registry.PackageSummary{Name: name, Versions: vs})
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ClassifyRequest implements registry.ArtifactClassifier.
func (rr *Remote) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) {
	return classifyArtifact(r)
}

// ArtifactInfo implements registry.ArtifactInfoProvider from the
// upstream packument (fetched when not cached).
func (rr *Remote) ArtifactInfo(ctx context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	if ValidatePackageName(ref.Name) != nil || ref.Version == "" {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	pkg, ok := rr.cachedPackument(ref.Name)
	if ok {
		if versions, _ := pkg["versions"].(map[string]any); versions[ref.Version] == nil {
			ok = false
		}
	}
	if !ok {
		_, _, _ = rr.sf.Do("packument:"+ref.Name, func() (any, error) {
			return nil, rr.refetchPackumentCtx(ctx, ref.Name)
		})
		if pkg, ok = rr.cachedPackument(ref.Name); !ok {
			return registry.ArtifactMeta{}, registry.ErrPackageNotFound
		}
	}
	versions, _ := pkg["versions"].(map[string]any)
	meta, ok := versions[ref.Version].(map[string]any)
	if !ok {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	timeMap, _ := pkg["time"].(map[string]any)
	return metaFromVersion(meta, timeMap, ref.Version), nil
}

// Prefetch implements registry.Prefetcher: refreshes the packument,
// then caches the tarball of version (or dist-tags.latest).
func (rr *Remote) Prefetch(ctx context.Context, name, version string) error {
	if err := ValidatePackageName(name); err != nil {
		return fmt.Errorf("%w: %w", err, registry.ErrInvalidPackageName)
	}
	_, err, _ := rr.sf.Do("packument:"+name, func() (any, error) {
		return nil, rr.refetchPackumentCtx(ctx, name)
	})
	pkg, ok := rr.cachedPackument(name)
	if !ok {
		if err != nil {
			return err
		}
		return registry.ErrPackageNotFound
	}
	if version == "" {
		tags, _ := pkg["dist-tags"].(map[string]any)
		version, _ = tags["latest"].(string)
	}
	versions, _ := pkg["versions"].(map[string]any)
	meta, ok := versions[version].(map[string]any)
	if !ok {
		return fmt.Errorf("%s@%s: %w", name, version, registry.ErrPackageNotFound)
	}
	file := tarballFilenameFromMeta(meta)
	if file == "" {
		return fmt.Errorf("npm: %s@%s has no tarball reference", name, version)
	}
	if rr.store.TarballSize(name, file) > 0 {
		return nil
	}
	_, err, _ = rr.sf.Do("tarball:"+name+"/"+file, func() (any, error) {
		return nil, rr.refetchTarball(ctx, name, file)
	})
	return err
}

// ListPackages unions the members' listings.
func (v *Virtual) ListPackages(ctx context.Context) ([]registry.PackageSummary, error) {
	return (&pkgbase.Virtual{Base: v.Base, RepoType: v.Type()}).ListPackages(ctx)
}

// ClassifyRequest implements registry.ArtifactClassifier.
func (v *Virtual) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) {
	return classifyArtifact(r)
}
