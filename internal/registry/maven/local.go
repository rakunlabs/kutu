package maven

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/rakunlabs/kutu/internal/hook"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/events"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/service"
)

var (
	_ registry.PackageLister        = (*Local)(nil)
	_ registry.VersionDeleter       = (*Local)(nil)
	_ registry.ArtifactClassifier   = (*Local)(nil)
	_ registry.ArtifactInfoProvider = (*Local)(nil)
	_ registry.VersionPromoter      = (*Local)(nil)
	_ registry.StatsProvider        = (*Local)(nil)
	_ registry.PackageDetailer      = (*Local)(nil)
)

// Local is a hosted Maven repository. It supports the standard static
// GET/HEAD layout plus PUT/DELETE for deployments.
type Local struct {
	namespace string
	name      string
	store     *Store
	usage     *pkgbase.Store
	guard     pkgbase.PublishGuard
	allowPush bool
	maxUpload int64
	emitter   events.Emitter
}

func NewLocalFactory() registry.Factory {
	return func(_ context.Context, deps registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		fs, err := deps.MountRawFS(r.Mount)
		if err != nil {
			return nil, fmt.Errorf("maven/local %s/%s: %w", ns, r.Name, err)
		}
		return &Local{
			namespace: ns,
			name:      r.Name,
			store:     NewStore(fs, r.BasePath),
			usage:     pkgbase.NewStore(fs, r.BasePath),
			guard:     pkgbase.GuardFor(r),
			allowPush: r.AllowPush,
			maxUpload: r.MaxUploadSize,
			emitter:   deps.Emitter,
		}, nil
	}
}

func (l *Local) Namespace() string { return l.namespace }
func (l *Local) Name() string      { return l.name }
func (l *Local) Type() string      { return service.RegistryTypeMaven }
func (l *Local) Kind() string      { return service.RegistryKindLocal }
func (l *Local) Store() *Store     { return l.store }
func (l *Local) Close() error      { return nil }

func (l *Local) Stats(context.Context) (registry.Stats, error) {
	return storeStats(l.store), nil
}

func storeStats(s *Store) registry.Stats {
	artifacts, versions, files, bytes := s.Count()
	return registry.Stats{PackageCount: artifacts, VersionCount: versions, BlobCount: files, TotalBytes: bytes}
}

func (l *Local) PackageDetail(_ context.Context, name string) (*registry.PackageDetail, error) {
	return packageDetail(l.store, name)
}

func (l *Local) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		l.get(w, r)
	case http.MethodPut:
		l.put(w, r)
	case http.MethodDelete:
		l.delete(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (l *Local) emit(typ hook.EventType, subject string, size int64) {
	events.EmitSafe(l.emitter, hook.Event{Type: typ, Mount: l.namespace, Path: l.name + "/" + subject, Protocol: "registry-maven", Size: size})
}

func (l *Local) get(w http.ResponseWriter, r *http.Request) {
	rel := cleanRel(r.URL.Path)
	if rel == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if serveFile(w, r, l.store, rel) {
		return
	}
	base, algo := splitChecksum(rel)
	if algo != "" {
		l.ensureMetadata(base)
		if target, ok := l.store.resolveFile(base); ok {
			if sum, err := l.store.Checksum(target, algo); err == nil {
				writeBody(w, r, rel, sum)
				return
			}
		}
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if l.ensureMetadata(rel) && serveFile(w, r, l.store, rel) {
		return
	}
	if target, ok := l.store.resolveFile(rel); ok && serveFile(w, r, l.store, target) {
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

// ensureMetadata lazily generates a missing metadata document (e.g.
// after a raw Store.DeleteVersion invalidated it).
func (l *Local) ensureMetadata(rel string) bool {
	c, ok := parseMetadataPath(rel)
	if !ok || l.store.Exists(rel) {
		return false
	}
	if c.Version != "" {
		_ = l.store.RegenerateSnapshotMetadata(c.Group, c.Artifact, c.Version)
	} else {
		_ = l.regenerate(c.Group, c.Artifact)
	}
	return l.store.Exists(rel)
}

// regenerate rebuilds the artifact metadata, carrying over a <plugins>
// section from the existing document.
func (l *Local) regenerate(group, artifact string) error {
	rel := path.Join(groupPath(group), artifact, metadataFile)
	var plugins *pluginList
	if b, err := l.store.Read(rel); err == nil {
		if doc, err := parseMetadata(b); err == nil {
			plugins = doc.Plugins
		}
	}
	doc, ok := l.store.generateArtifactMetadata(group, artifact)
	if !ok {
		if plugins != nil {
			return nil
		}
		l.store.deleteWithChecksums(rel)
		return nil
	}
	doc.Plugins = plugins
	return l.store.WriteWithChecksums(rel, renderMetadata(doc))
}

func (l *Local) put(w http.ResponseWriter, r *http.Request) {
	if !l.allowPush {
		http.Error(w, "push disabled", http.StatusMethodNotAllowed)
		return
	}
	if !validRel(r.URL.Path) {
		http.Error(w, "invalid maven path", http.StatusBadRequest)
		return
	}
	rel := cleanRel(r.URL.Path)
	max := l.maxUpload
	if max == 0 {
		max = 512 * 1024 * 1024
	}
	body, err := pkgbase.ReadBody(r.Body, max)
	if err != nil {
		code := http.StatusBadRequest
		if strings.Contains(err.Error(), "exceeds") {
			code = http.StatusRequestEntityTooLarge
		}
		http.Error(w, err.Error(), code)
		return
	}

	if base, algo := splitChecksum(rel); algo != "" {
		l.putChecksum(w, base, algo, body)
		return
	}
	if mc, ok := parseMetadataPath(rel); ok {
		l.putMetadata(w, rel, mc, body)
		return
	}

	c, isArtifact := parseArtifactPath(rel)
	exists := isArtifact && !isSnapshot(c.Version) && l.store.Exists(rel)
	if !l.guard.Allow(w, l.usage, exists, int64(len(body))) {
		return
	}
	if err := l.store.WriteWithChecksums(rel, body); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if isArtifact && isPrimary(c.File) {
		if _, ok := parseSnapshotFile(c.Artifact, c.Version, c.File); ok {
			l.store.PruneSnapshots(c.Group, c.Artifact, c.Version, SnapshotBuildsToKeep)
			_ = l.store.RegenerateSnapshotMetadata(c.Group, c.Artifact, c.Version)
		}
		_ = l.regenerate(c.Group, c.Artifact)
	}
	l.emit(hook.EventRegistryPublished, rel, int64(len(body)))
	w.WriteHeader(http.StatusCreated)
}

// putChecksum verifies a client-uploaded checksum against the stored
// file. Checksums of generated metadata are ignored (ours are kept).
func (l *Local) putChecksum(w http.ResponseWriter, base, algo string, body []byte) {
	if _, ok := parseMetadataPath(base); ok {
		w.WriteHeader(http.StatusCreated)
		return
	}
	got := strings.ToLower(strings.TrimSpace(string(body)))
	if f := strings.Fields(got); len(f) > 0 {
		got = f[0]
	}
	if content, err := l.store.Read(base); err == nil {
		if want := checksumHex(algo, content); got != want {
			http.Error(w, fmt.Sprintf("%s checksum mismatch for %s: got %s, want %s", algo, path.Base(base), got, want), http.StatusBadRequest)
			return
		}
		got = checksumHex(algo, content)
	}
	if err := l.store.Write(base+"."+algo, []byte(got)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// putMetadata merges a client-uploaded metadata document with the one
// generated from storage so no stored version is ever dropped.
func (l *Local) putMetadata(w http.ResponseWriter, rel string, c coords, body []byte) {
	var gen *metadataDoc
	var ok bool
	if c.Version != "" {
		gen, ok = l.store.generateSnapshotMetadata(c.Group, c.Artifact, c.Version)
	} else {
		gen, ok = l.store.generateArtifactMetadata(c.Group, c.Artifact)
	}
	out := body
	if ok {
		if c.Version != "" {
			// Version-level snapshot metadata is fully derived from storage.
			out = renderMetadata(gen)
		} else if merged, mok := mergeMetadata([][]byte{renderMetadata(gen), body}); mok {
			out = merged
		} else {
			out = renderMetadata(gen)
		}
	} else if _, err := parseMetadata(body); err != nil {
		http.Error(w, "invalid maven-metadata.xml: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := l.store.WriteWithChecksums(rel, out); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (l *Local) delete(w http.ResponseWriter, r *http.Request) {
	if !l.allowPush {
		http.Error(w, "delete disabled", http.StatusMethodNotAllowed)
		return
	}
	rel := cleanRel(r.URL.Path)
	if rel == "" || !l.store.Exists(rel) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if _, algo := splitChecksum(rel); algo != "" {
		_ = l.store.Delete(rel)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	l.store.deleteWithChecksums(rel)
	if c, ok := parseArtifactPath(rel); ok && isPrimary(c.File) {
		_ = l.store.RegenerateSnapshotMetadata(c.Group, c.Artifact, c.Version)
		_ = l.regenerate(c.Group, c.Artifact)
	}
	l.emit(hook.EventRegistryDeleted, rel, 0)
	w.WriteHeader(http.StatusNoContent)
}

// ListPackages implements registry.PackageLister ("groupId:artifactId").
func (l *Local) ListPackages(context.Context) ([]registry.PackageSummary, error) {
	return listPackages(l.store)
}

func listPackages(s *Store) ([]registry.PackageSummary, error) {
	arts, err := s.ListArtifacts()
	if err != nil {
		return nil, err
	}
	out := make([]registry.PackageSummary, 0, len(arts))
	for _, a := range arts {
		out = append(out, registry.PackageSummary{Name: a.GroupID + ":" + a.ArtifactID, Versions: a.Versions})
	}
	return out, nil
}

// DeleteVersion implements registry.VersionDeleter.
func (l *Local) DeleteVersion(_ context.Context, name, version string) error {
	group, artifact := splitName(name)
	if group == "" || artifact == "" {
		return registry.ErrPackageNotFound
	}
	if _, err := l.store.DeleteVersion(group, artifact, version); err != nil {
		return err
	}
	if err := l.regenerate(group, artifact); err != nil {
		return err
	}
	l.emit(hook.EventRegistryDeleted, group+":"+artifact+"@"+version, 0)
	return nil
}

// ClassifyRequest implements registry.ArtifactClassifier.
func (l *Local) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) {
	return classify(r)
}

func classify(r *http.Request) (registry.ArtifactRef, bool) {
	rel, _ := splitChecksum(cleanRel(r.URL.Path))
	rel = strings.TrimSuffix(rel, ".asc")
	if c, ok := parseMetadataPath(rel); ok {
		return registry.ArtifactRef{Name: c.Name()}, true
	}
	if c, ok := parseArtifactPath(rel); ok {
		return registry.ArtifactRef{Name: c.Name(), Version: c.Version}, true
	}
	return registry.ArtifactRef{}, false
}

// ArtifactInfo implements registry.ArtifactInfoProvider: license from
// the POM, publish time from the stored file's mtime.
func (l *Local) ArtifactInfo(_ context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	group, artifact := splitName(ref.Name)
	if group == "" || artifact == "" || ref.Version == "" {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	pomRel, ok := l.store.resolveFile(versionFile(group, artifact, ref.Version, ".pom"))
	if !ok {
		if c, cok := l.store.firstPrimary(group, artifact, ref.Version); cok {
			fi, err := l.store.Stat(c)
			if err != nil {
				return registry.ArtifactMeta{}, err
			}
			return registry.ArtifactMeta{PublishedAt: fi.ModTime}, nil
		}
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	var meta registry.ArtifactMeta
	if fi, err := l.store.Stat(pomRel); err == nil {
		meta.PublishedAt = fi.ModTime
	}
	if b, err := l.store.Read(pomRel); err == nil {
		meta.License = parsePOM(b).license()
	}
	return meta, nil
}

func (s *Store) firstPrimary(group, artifact, version string) (string, bool) {
	dir := path.Join(groupPath(group), artifact, version)
	for _, f := range s.readDir(dir) {
		if !f.IsDir && belongs(artifact, version, f.Name) {
			return path.Join(dir, f.Name), true
		}
	}
	return "", false
}

// PromoteVersion implements registry.VersionPromoter: copies the whole
// version directory (checksums included) into dst and regenerates
// dst's metadata.
func (l *Local) PromoteVersion(_ context.Context, dst registry.Registry, name, version string) error {
	d, ok := dst.(*Local)
	if !ok {
		return fmt.Errorf("maven: promote target %T is not a maven local registry", dst)
	}
	group, artifact := splitName(name)
	if group == "" || artifact == "" || version == "" || strings.ContainsAny(version, "/\\") {
		return registry.ErrPackageNotFound
	}
	if _, ok := l.store.firstPrimary(group, artifact, version); !ok {
		return registry.ErrPackageNotFound
	}
	dir := path.Join(groupPath(group), artifact, version)
	files, err := l.store.ListFiles(dir)
	if err != nil {
		return err
	}
	var total int64
	for _, f := range files {
		total += f.Size
	}
	_, exists := d.store.firstPrimary(group, artifact, version)
	if code, err := d.guard.Check(d.usage, exists && !isSnapshot(version), total); err != nil {
		return fmt.Errorf("maven: promote %s@%s (status %d): %w", name, version, code, err)
	}
	for _, f := range files {
		body, err := l.store.Read(f.Path)
		if err != nil {
			return fmt.Errorf("maven: read %s: %w", f.Path, err)
		}
		if err := d.store.Write(f.Path, body); err != nil {
			return err
		}
		if _, algo := splitChecksum(f.Path); algo == "" {
			for _, a := range checksumAlgos {
				if !d.store.Exists(f.Path + "." + a) {
					_ = d.store.Write(f.Path+"."+a, []byte(checksumHex(a, body)))
				}
			}
		}
	}
	if isSnapshot(version) {
		_ = d.store.RegenerateSnapshotMetadata(group, artifact, version)
	}
	if err := d.regenerate(group, artifact); err != nil {
		return err
	}
	d.emit(hook.EventRegistryPublished, group+":"+artifact+"@"+version, total)
	return nil
}
