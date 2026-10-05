package helm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/rakunlabs/kutu/internal/hook"
	"github.com/rakunlabs/kutu/internal/rawfs"
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

// indexEntries is the slice of index.yaml the policy helpers need.
type indexEntries struct {
	Entries map[string][]struct {
		Version string   `yaml:"version"`
		Created string   `yaml:"created"`
		URLs    []string `yaml:"urls"`
	} `yaml:"entries"`
}

func listPackages(s *Store) ([]registry.PackageSummary, error) {
	charts, err := s.ListCharts()
	if err != nil {
		return nil, err
	}
	out := make([]registry.PackageSummary, 0, len(charts))
	for _, name := range charts {
		vs, err := s.ListVersions(name)
		if err != nil {
			return nil, err
		}
		pkgbase.SortVersions(vs)
		out = append(out, registry.PackageSummary{Name: name, Versions: vs})
	}
	return out, nil
}

// classify maps "/{chart}-{version}.tgz" to {chart, version}. The
// index and ChartMuseum API are not package-scoped.
func classify(r *http.Request) (registry.ArtifactRef, bool) {
	p := strings.TrimPrefix(r.URL.Path, "/")
	if strings.Contains(p, "/") || !strings.HasSuffix(p, ".tgz") {
		return registry.ArtifactRef{}, false
	}
	chart, version, err := ParseTarballFilename(p)
	if err != nil {
		return registry.ArtifactRef{}, false
	}
	return registry.ArtifactRef{Name: chart, Version: version}, true
}

func hasVersion(s *Store, name, version string) (bool, error) {
	if ValidateChartName(name) != nil || ValidateChartVersion(version) != nil {
		return false, nil
	}
	vs, err := s.ListVersions(name)
	if err != nil {
		return false, err
	}
	for _, v := range vs {
		if v == version {
			return true, nil
		}
	}
	return false, nil
}

// ListPackages implements registry.PackageLister.
func (l *Local) ListPackages(_ context.Context) ([]registry.PackageSummary, error) {
	return listPackages(l.store)
}

// ClassifyRequest implements registry.ArtifactClassifier.
func (l *Local) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) { return classify(r) }

// ArtifactInfo implements registry.ArtifactInfoProvider. PublishedAt
// is the index entry's created time (the tarball's push time).
func (l *Local) ArtifactInfo(_ context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	ok, err := hasVersion(l.store, ref.Name, ref.Version)
	if err != nil {
		return registry.ArtifactMeta{}, err
	}
	if !ok {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	var meta registry.ArtifactMeta
	if created := l.store.TarballModTime(ref.Name, ref.Version); created != "" {
		meta.PublishedAt, _ = time.Parse(time.RFC3339, created)
	}
	return meta, nil
}

// DeleteVersion implements registry.VersionDeleter. The index is
// rebuilt on the next index.yaml read.
func (l *Local) DeleteVersion(_ context.Context, name, version string) error {
	ok, err := hasVersion(l.store, name, version)
	if err != nil {
		return err
	}
	if !ok {
		return registry.ErrPackageNotFound
	}
	size := l.store.TarballSize(name, version)
	if err := l.store.DeleteVersion(name, version); err != nil {
		return err
	}
	events.EmitSafe(l.emitter, hook.Event{
		Type:     hook.EventRegistryDeleted,
		Mount:    l.namespace,
		Path:     l.name + "/" + name + "@" + version,
		Protocol: "registry-helm",
		Size:     size,
	})
	return nil
}

// PromoteVersion implements registry.VersionPromoter: copies the
// chart tarball into dst (a helm *Local) and invalidates its index.
func (l *Local) PromoteVersion(_ context.Context, dst registry.Registry, name, version string) error {
	d, ok := dst.(*Local)
	if !ok {
		return fmt.Errorf("helm: promote target %T is not a helm local registry", dst)
	}
	found, err := hasVersion(l.store, name, version)
	if err != nil {
		return err
	}
	if !found {
		return registry.ErrPackageNotFound
	}
	rc, _, err := l.store.OpenTarball(name, version)
	if err != nil {
		if errors.Is(err, ErrChartNotFound) {
			return registry.ErrPackageNotFound
		}
		return err
	}
	body, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return fmt.Errorf("helm: read %s-%s.tgz: %w", name, version, err)
	}
	extraction, err := ExtractChart(body)
	if err != nil {
		return err
	}
	if extraction.Metadata.Name != name || extraction.Metadata.Version != version {
		return fmt.Errorf("helm: %s-%s.tgz carries %s@%s: %w", name, version,
			extraction.Metadata.Name, extraction.Metadata.Version, ErrInvalidChart)
	}
	if err := d.store.WriteChart(extraction, body); err != nil {
		return err
	}
	events.EmitSafe(d.emitter, hook.Event{
		Type:     hook.EventRegistryPublished,
		Mount:    d.namespace,
		Path:     d.name + "/" + name + "@" + version,
		Protocol: "registry-helm",
		Size:     extraction.Size,
	})
	return nil
}

// ListPackages implements registry.PackageLister over the cache.
func (rr *Remote) ListPackages(_ context.Context) ([]registry.PackageSummary, error) {
	return listPackages(rr.store)
}

// ClassifyRequest implements registry.ArtifactClassifier.
func (rr *Remote) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) { return classify(r) }

// loadIndex returns the cached upstream index.yaml, fetching (and
// caching) it when missing or stale.
func (rr *Remote) loadIndex(ctx context.Context) ([]byte, error) {
	cachePath := rr.store.indexPath()
	if rc, fi, err := rr.store.fs.Open(cachePath); err == nil {
		fresh := rr.mutableTTL > 0 && time.Since(fi.ModTime) < rr.mutableTTL
		if fresh {
			defer rc.Close()
			return io.ReadAll(rc)
		}
		rc.Close()
	}
	resp, err := rr.client.Get(ctx, "/index.yaml")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if wfs, ok := rr.store.fs.(rawfs.WritableRawFS); ok {
		_ = wfs.Write(cachePath, strings.NewReader(string(body)), int64(len(body)))
	}
	return body, nil
}

func (rr *Remote) parsedIndex(ctx context.Context) (*indexEntries, error) {
	body, err := rr.loadIndex(ctx)
	if err != nil {
		return nil, err
	}
	var idx indexEntries
	if err := yaml.Unmarshal(body, &idx); err != nil {
		return nil, fmt.Errorf("helm: parse upstream index: %w", err)
	}
	return &idx, nil
}

// ArtifactInfo implements registry.ArtifactInfoProvider using the
// upstream index entry's created time.
func (rr *Remote) ArtifactInfo(ctx context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	if ValidateChartName(ref.Name) != nil || ValidateChartVersion(ref.Version) != nil {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	idx, err := rr.parsedIndex(ctx)
	if err != nil {
		return registry.ArtifactMeta{}, err
	}
	for _, e := range idx.Entries[ref.Name] {
		if e.Version != ref.Version {
			continue
		}
		var meta registry.ArtifactMeta
		if e.Created != "" {
			meta.PublishedAt, _ = time.Parse(time.RFC3339Nano, e.Created)
		}
		return meta, nil
	}
	return registry.ArtifactMeta{}, registry.ErrPackageNotFound
}

// Prefetch implements registry.Prefetcher: refreshes index.yaml and
// caches the chart tarball (latest version when version == "").
func (rr *Remote) Prefetch(ctx context.Context, name, version string) error {
	if err := ValidateChartName(name); err != nil {
		return fmt.Errorf("%w: %w", err, registry.ErrInvalidPackageName)
	}
	idx, err := rr.parsedIndex(ctx)
	if err != nil {
		return err
	}
	entries := idx.Entries[name]
	if len(entries) == 0 {
		return registry.ErrPackageNotFound
	}
	if version == "" {
		vs := make([]string, 0, len(entries))
		for _, e := range entries {
			vs = append(vs, e.Version)
		}
		version = pkgbase.Latest(vs)
	}
	if err := ValidateChartVersion(version); err != nil {
		return fmt.Errorf("%w: %w", err, registry.ErrInvalidPackageName)
	}
	var urls []string
	found := false
	for _, e := range entries {
		if e.Version == version {
			found = true
			urls = e.URLs
			break
		}
	}
	if !found {
		return registry.ErrPackageNotFound
	}
	if _, err := rr.store.ReadMetadata(name, version); err == nil {
		return nil
	}
	// The data plane fetches tarballs as "/{filename}" relative to
	// the upstream; mirror that, then fall back to the index URL.
	candidates := []string{"/" + TarballFilename(name, version)}
	for _, u := range urls {
		if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
			candidates = append(candidates, u)
		} else if u != "" {
			candidates = append(candidates, "/"+strings.TrimPrefix(u, "/"))
		}
	}
	var lastErr error
	for _, c := range candidates {
		body, err := rr.fetch(ctx, c)
		if err != nil {
			lastErr = err
			continue
		}
		extraction, err := ExtractChart(body)
		if err != nil {
			lastErr = err
			continue
		}
		return rr.store.WriteChart(extraction, body)
	}
	return lastErr
}

func (rr *Remote) fetch(ctx context.Context, pathOrURL string) ([]byte, error) {
	resp, err := rr.client.Get(ctx, pathOrURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// ListPackages unions the members' listings.
func (v *Virtual) ListPackages(ctx context.Context) ([]registry.PackageSummary, error) {
	out, err := (&pkgbase.Virtual{Base: v.Base, RepoType: v.Type()}).ListPackages(ctx)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ClassifyRequest implements registry.ArtifactClassifier.
func (v *Virtual) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) { return classify(r) }
