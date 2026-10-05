package goproxy

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

	"github.com/rakunlabs/kutu/internal/hook"
	"github.com/rakunlabs/kutu/internal/registry"
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

// listPackages returns every module with its versions (ascending).
func listPackages(s *Store) ([]registry.PackageSummary, error) {
	mods, err := s.ListModules()
	if err != nil {
		return nil, err
	}
	out := make([]registry.PackageSummary, 0, len(mods))
	for _, mod := range mods {
		vs, err := s.ListVersions(mod)
		if err != nil {
			return nil, err
		}
		sort.SliceStable(vs, func(i, j int) bool { return CompareVersions(vs[i], vs[j]) < 0 })
		out = append(out, registry.PackageSummary{Name: mod, Versions: vs})
	}
	return out, nil
}

// classify maps a goproxy request path to {module, version}. Version
// files carry the version; @v/list and @latest carry the module only.
func classify(r *http.Request) (registry.ArtifactRef, bool) {
	req, ok := parsePath(r.URL.Path)
	if !ok {
		return registry.ArtifactRef{}, false
	}
	if req.IsList || req.IsLatest {
		return registry.ArtifactRef{Name: req.Module}, true
	}
	return registry.ArtifactRef{Name: req.Module, Version: req.Version}, true
}

// artifactInfo reports the .info Time as the publish time.
func artifactInfo(s *Store, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	if ValidateModulePath(ref.Name) != nil || ValidateVersion(ref.Version) != nil {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	info, err := s.ReadVersionInfo(ref.Name, ref.Version)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return registry.ArtifactMeta{}, registry.ErrPackageNotFound
		}
		return registry.ArtifactMeta{}, err
	}
	return registry.ArtifactMeta{PublishedAt: info.Time}, nil
}

// ListPackages implements registry.PackageLister.
func (l *Local) ListPackages(_ context.Context) ([]registry.PackageSummary, error) {
	return listPackages(l.store)
}

// ClassifyRequest implements registry.ArtifactClassifier.
func (l *Local) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) { return classify(r) }

// ArtifactInfo implements registry.ArtifactInfoProvider.
func (l *Local) ArtifactInfo(_ context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	return artifactInfo(l.store, ref)
}

// DeleteVersion implements registry.VersionDeleter.
func (l *Local) DeleteVersion(_ context.Context, module, version string) error {
	if ValidateModulePath(module) != nil || ValidateVersion(version) != nil {
		return registry.ErrPackageNotFound
	}
	if _, err := l.store.StatVersionFile(module, version, "info"); err != nil {
		if errors.Is(err, ErrNotFound) {
			return registry.ErrPackageNotFound
		}
		return err
	}
	if err := l.store.DeleteVersion(module, version); err != nil {
		return err
	}
	events.EmitSafe(l.emitter, hook.Event{
		Type:     hook.EventRegistryDeleted,
		Mount:    l.namespace,
		Path:     l.name + "/" + module + "@" + version,
		Protocol: "registry-go",
	})
	return nil
}

// PromoteVersion implements registry.VersionPromoter: copies the
// .info/.mod/.zip of module@version into dst (a goproxy *Local).
func (l *Local) PromoteVersion(_ context.Context, dst registry.Registry, module, version string) error {
	d, ok := dst.(*Local)
	if !ok {
		return fmt.Errorf("goproxy: promote target %T is not a go local registry", dst)
	}
	if ValidateModulePath(module) != nil || ValidateVersion(version) != nil {
		return registry.ErrPackageNotFound
	}
	if _, err := l.store.StatVersionFile(module, version, "info"); err != nil {
		if errors.Is(err, ErrNotFound) {
			return registry.ErrPackageNotFound
		}
		return err
	}
	var zipSize int64
	for _, ext := range versionFileExts {
		rc, _, err := l.store.OpenVersionFile(module, version, ext)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return err
		}
		body, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return fmt.Errorf("goproxy: read %s@%s.%s: %w", module, version, ext, err)
		}
		if err := d.store.WriteVersionFile(module, version, ext, bytes.NewReader(body), int64(len(body))); err != nil {
			return err
		}
		if ext == "zip" {
			zipSize = int64(len(body))
		}
	}
	events.EmitSafe(d.emitter, hook.Event{
		Type:     hook.EventRegistryPublished,
		Mount:    d.namespace,
		Path:     d.name + "/" + module + "@" + version,
		Protocol: "registry-go",
		Size:     zipSize,
	})
	return nil
}

// ListPackages implements registry.PackageLister over the cache.
func (rr *Remote) ListPackages(_ context.Context) ([]registry.PackageSummary, error) {
	return listPackages(rr.store)
}

// ClassifyRequest implements registry.ArtifactClassifier.
func (rr *Remote) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) { return classify(r) }

// ArtifactInfo implements registry.ArtifactInfoProvider. Uncached
// versions are fetched (.info only) so the upstream publish time is
// available to policy checks before the download proceeds.
func (rr *Remote) ArtifactInfo(ctx context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	if ValidateModulePath(ref.Name) != nil || ValidateVersion(ref.Version) != nil {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	if _, err := rr.store.StatVersionFile(ref.Name, ref.Version, "info"); err != nil {
		_, _, _ = rr.sf.Do("info:"+ref.Name+"@"+ref.Version, func() (any, error) {
			return nil, rr.fetchAndStoreVersion(ctx, ref.Name, ref.Version, "info")
		})
	}
	return artifactInfo(rr.store, ref)
}

// Prefetch implements registry.Prefetcher. version == "" resolves the
// upstream @latest (falling back to the highest @v/list entry).
func (rr *Remote) Prefetch(ctx context.Context, module, version string) error {
	if err := ValidateModulePath(module); err != nil {
		return fmt.Errorf("%w: %w", err, registry.ErrInvalidPackageName)
	}
	if version == "" {
		_, listErr, _ := rr.sf.Do("list:"+module, func() (any, error) {
			return nil, rr.refetchList(ctx, module)
		})
		_, latestErr, _ := rr.sf.Do("latest:"+module, func() (any, error) {
			return nil, rr.refetchLatest(ctx, module)
		})
		if latestErr == nil {
			if body, err := rr.cachedLatestRaw(module); err == nil {
				var info VersionInfo
				if json.Unmarshal(body, &info) == nil {
					version = strings.TrimSpace(info.Version)
				}
			}
		}
		if version == "" && listErr == nil {
			if vs, err := rr.store.cachedListVersions(module); err == nil && len(vs) > 0 {
				SortVersionsDesc(vs)
				version = vs[0]
			}
		}
		if version == "" {
			if latestErr != nil {
				return latestErr
			}
			if listErr != nil {
				return listErr
			}
			return registry.ErrPackageNotFound
		}
	}
	if err := rr.WarmVersionFile(ctx, module, version, "info"); err != nil {
		return err
	}
	for _, ext := range versionFileExts {
		if _, err := rr.store.StatVersionFile(module, version, ext); err != nil {
			return fmt.Errorf("goproxy: prefetch %s@%s.%s: %w", module, version, ext, err)
		}
	}
	return nil
}

// ListPackages unions the members' listings.
func (v *Virtual) ListPackages(ctx context.Context) ([]registry.PackageSummary, error) {
	return (&pkgbase.Virtual{Base: v.Base, RepoType: v.Type()}).ListPackages(ctx)
}

// ClassifyRequest implements registry.ArtifactClassifier.
func (v *Virtual) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) { return classify(r) }
