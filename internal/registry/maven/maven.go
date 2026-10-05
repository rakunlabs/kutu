// Package maven implements Maven 2 repository layout registries
// (local, remote pull-through and virtual).
//
// Wire endpoints (paths relative to /registries/{ns}/{repo}):
//
//	GET|HEAD /{group/path}/{artifact}/{version}/{file}          artifact download
//	GET|HEAD /{group/path}/{artifact}/maven-metadata.xml        artifact metadata (generated)
//	GET|HEAD /{group/path}/{artifact}/{v}-SNAPSHOT/maven-metadata.xml
//	                                                            snapshot metadata (generated)
//	GET|HEAD /...{file}.{md5,sha1,sha256,sha512}                checksums (computed when missing)
//	PUT      /{path}                                            deploy (local only)
//	DELETE   /{path}                                            delete one file (local only)
//
// Local repositories store every uploaded file verbatim and generate
// .md5/.sha1/.sha256/.sha512 sidecars. Client-uploaded checksums are
// verified against the stored file (mismatch → 400). The artifact-level
// maven-metadata.xml is regenerated from the stored versions on every
// publish/delete; a client-uploaded copy is merged with it so versions
// are never dropped. Unique SNAPSHOT deployments
// (1.0-SNAPSHOT/app-1.0-20240101.120000-3.jar) maintain the
// version-level maven-metadata.xml; requests for app-1.0-SNAPSHOT.jar
// resolve to the newest timestamped build. Only the newest
// SnapshotBuildsToKeep (10) builds of each SNAPSHOT version are kept.
//
// Remote repositories pull through and cache upstream files; checksums
// the upstream lacks are computed from the cached file. Extra
// prefix-routed upstreams match on the repository path (e.g. prefix
// "com/acme/" routes com.acme.* artifacts). The Gradle plugin portal
// works as a plain remote with URL https://plugins.gradle.org/m2
// (plugin marker artifacts are ordinary POMs).
//
// Virtual repositories serve the first member hit, except for
// maven-metadata.xml (and its checksums) which is merged across members.
//
// Client configuration (Maven settings.xml / pom.xml):
//
//	<repository>
//	  <id>kutu</id>
//	  <url>https://kutu.example.com/registries/{ns}/{repo}</url>
//	</repository>
//	<server><id>kutu</id><username>any</username><password>kutu_TOKEN</password></server>
//
// Gradle:
//
//	repositories { maven { url = uri("https://kutu.example.com/registries/{ns}/{repo}")
//	    credentials { username = "any"; password = "kutu_TOKEN" } } }
package maven

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/service"
)

// Store is a path-keyed Maven repository layout rooted at BasePath.
// Maven already defines a static filesystem contract, so we preserve
// incoming paths exactly after cleaning traversal attempts.
type Store struct {
	fs       rawfs.RawFS
	basePath string
}

func NewStore(fs rawfs.RawFS, basePath string) *Store {
	return &Store{fs: fs, basePath: strings.Trim(basePath, "/")}
}

func (s *Store) RawFS() rawfs.RawFS { return s.fs }

func (s *Store) join(rel string) string {
	rel = cleanRel(rel)
	if s.basePath == "" {
		return rel
	}
	if rel == "" {
		return s.basePath
	}
	return path.Join(s.basePath, rel)
}

func cleanRel(p string) string {
	p = strings.TrimPrefix(p, "/")
	p = path.Clean("/" + p)
	p = strings.TrimPrefix(p, "/")
	if p == "." {
		return ""
	}
	return p
}

func validRel(p string) bool {
	return cleanRel(p) != "" && !strings.Contains(cleanRel(p), "../")
}

func (s *Store) Open(rel string) (rawfs.ReadSeekCloser, *rawfs.FileInfo, error) {
	rc, fi, err := s.fs.Open(s.join(rel))
	if err != nil {
		if isNotFound(err) {
			return nil, nil, registry.ErrPackageNotFound
		}
		return nil, nil, err
	}
	return rc, fi, nil
}

// Read returns the full contents of rel.
func (s *Store) Read(rel string) ([]byte, error) {
	rc, _, err := s.Open(rel)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// Stat stats rel.
func (s *Store) Stat(rel string) (*rawfs.FileInfo, error) {
	fi, err := s.fs.Stat(s.join(rel))
	if err != nil {
		if isNotFound(err) {
			return nil, registry.ErrPackageNotFound
		}
		return nil, err
	}
	return fi, nil
}

// Exists reports whether rel is a stored regular file.
func (s *Store) Exists(rel string) bool {
	fi, err := s.fs.Stat(s.join(rel))
	return err == nil && !fi.IsDir
}

func (s *Store) Write(rel string, body []byte) error {
	wfs, ok := s.fs.(rawfs.WritableRawFS)
	if !ok {
		return fmt.Errorf("maven: backend read-only")
	}
	return wfs.Write(s.join(rel), bytes.NewReader(body), int64(len(body)))
}

func (s *Store) Delete(rel string) error {
	wfs, ok := s.fs.(rawfs.WritableRawFS)
	if !ok {
		return fmt.Errorf("maven: backend read-only")
	}
	return wfs.Delete(s.join(rel))
}

func (s *Store) readDir(rel string) []rawfs.DirEntry {
	entries, err := s.fs.ReadDir(s.join(rel))
	if err != nil {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries
}

// WriteWithChecksums stores body at rel plus md5/sha1/sha256/sha512 sidecars.
func (s *Store) WriteWithChecksums(rel string, body []byte) error {
	if err := s.Write(rel, body); err != nil {
		return err
	}
	return s.writeChecksums(rel, body)
}

func (s *Store) writeChecksums(rel string, body []byte) error {
	for _, algo := range checksumAlgos {
		if err := s.Write(rel+"."+algo, []byte(checksumHex(algo, body))); err != nil {
			return err
		}
	}
	return nil
}

// deleteWithChecksums removes rel and its checksum/signature sidecars.
func (s *Store) deleteWithChecksums(rel string) {
	_ = s.Delete(rel)
	for _, algo := range checksumAlgos {
		_ = s.Delete(rel + "." + algo)
	}
}

// Checksum returns the algo checksum of rel, reading the stored
// sidecar or computing (and persisting) it from the file.
func (s *Store) Checksum(rel, algo string) ([]byte, error) {
	if b, err := s.Read(rel + "." + algo); err == nil {
		return b, nil
	}
	body, err := s.Read(rel)
	if err != nil {
		return nil, err
	}
	sum := []byte(checksumHex(algo, body))
	_ = s.Write(rel+"."+algo, sum)
	return sum, nil
}

// belongs reports whether name is a primary artifact file of
// artifact:version (including unique snapshot builds).
func belongs(artifact, version, name string) bool {
	if !isPrimary(name) {
		return false
	}
	if strings.HasPrefix(name, artifact+"-"+version) {
		return true
	}
	_, ok := parseSnapshotFile(artifact, version, name)
	return ok
}

// artifactVersions lists the versions under group/artifact that hold
// at least one primary file.
func (s *Store) artifactVersions(group, artifact string) []string {
	dir := path.Join(groupPath(group), artifact)
	var out []string
	for _, e := range s.readDir(dir) {
		if !e.IsDir {
			continue
		}
		for _, f := range s.readDir(path.Join(dir, e.Name)) {
			if !f.IsDir && belongs(artifact, e.Name, f.Name) {
				out = append(out, e.Name)
				break
			}
		}
	}
	pkgbase.SortVersions(out)
	return out
}

func (s *Store) generateArtifactMetadata(group, artifact string) (*metadataDoc, bool) {
	vs := s.artifactVersions(group, artifact)
	if len(vs) == 0 {
		return nil, false
	}
	return &metadataDoc{
		GroupID:    group,
		ArtifactID: artifact,
		Versioning: &versioning{
			Latest:      pkgbase.Latest(vs),
			Release:     latestRelease(vs),
			Versions:    &versionList{Version: vs},
			LastUpdated: nowStamp(),
		},
	}, true
}

// snapshotBuilds parses the unique snapshot builds stored for a
// SNAPSHOT version (primary files only).
func (s *Store) snapshotBuilds(group, artifact, version string) []snapshotFile {
	var out []snapshotFile
	for _, f := range s.readDir(path.Join(groupPath(group), artifact, version)) {
		if f.IsDir || !isPrimary(f.Name) {
			continue
		}
		if sf, ok := parseSnapshotFile(artifact, version, f.Name); ok {
			out = append(out, sf)
		}
	}
	return out
}

func newerBuild(a, b snapshotFile) bool {
	if a.Build != b.Build {
		return a.Build > b.Build
	}
	return a.Timestamp > b.Timestamp
}

func (s *Store) generateSnapshotMetadata(group, artifact, version string) (*metadataDoc, bool) {
	builds := s.snapshotBuilds(group, artifact, version)
	if len(builds) == 0 {
		return nil, false
	}
	latest := builds[0]
	perKey := map[string]snapshotFile{}
	for _, b := range builds {
		if newerBuild(b, latest) {
			latest = b
		}
		key := b.Classifier + "\x00" + b.Ext
		if cur, ok := perKey[key]; !ok || newerBuild(b, cur) {
			perKey[key] = b
		}
	}
	keys := make([]string, 0, len(perKey))
	for k := range perKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	list := &snapshotVersionList{}
	for _, k := range keys {
		b := perKey[k]
		list.Items = append(list.Items, snapshotVersion{Classifier: b.Classifier, Extension: b.Ext, Value: b.Value, Updated: compactTimestamp(b.Timestamp)})
	}
	return &metadataDoc{
		GroupID:    group,
		ArtifactID: artifact,
		Version:    version,
		Versioning: &versioning{
			Snapshot:         &snapshotInfo{Timestamp: latest.Timestamp, BuildNumber: latest.Build},
			LastUpdated:      nowStamp(),
			SnapshotVersions: list,
		},
	}, true
}

// RegenerateMetadata rewrites group/artifact/maven-metadata.xml (and
// checksums) from the stored versions, removing it when none remain.
func (s *Store) RegenerateMetadata(group, artifact string) error {
	rel := path.Join(groupPath(group), artifact, metadataFile)
	doc, ok := s.generateArtifactMetadata(group, artifact)
	if !ok {
		s.deleteWithChecksums(rel)
		return nil
	}
	return s.WriteWithChecksums(rel, renderMetadata(doc))
}

// RegenerateSnapshotMetadata rewrites the version-level metadata of a
// SNAPSHOT version from its stored unique builds.
func (s *Store) RegenerateSnapshotMetadata(group, artifact, version string) error {
	if !isSnapshot(version) {
		return nil
	}
	rel := path.Join(groupPath(group), artifact, version, metadataFile)
	doc, ok := s.generateSnapshotMetadata(group, artifact, version)
	if !ok {
		s.deleteWithChecksums(rel)
		return nil
	}
	return s.WriteWithChecksums(rel, renderMetadata(doc))
}

// PruneSnapshots deletes every file of all but the newest keep unique
// builds of a SNAPSHOT version and returns the number of builds removed.
func (s *Store) PruneSnapshots(group, artifact, version string, keep int) int {
	if keep <= 0 || !isSnapshot(version) {
		return 0
	}
	nums := map[int]struct{}{}
	for _, b := range s.snapshotBuilds(group, artifact, version) {
		nums[b.Build] = struct{}{}
	}
	if len(nums) <= keep {
		return 0
	}
	sorted := make([]int, 0, len(nums))
	for n := range nums {
		sorted = append(sorted, n)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(sorted)))
	drop := map[int]struct{}{}
	for _, n := range sorted[keep:] {
		drop[n] = struct{}{}
	}
	dir := path.Join(groupPath(group), artifact, version)
	for _, f := range s.readDir(dir) {
		if f.IsDir {
			continue
		}
		name, _ := splitChecksum(f.Name)
		name = strings.TrimSuffix(name, ".asc")
		if sf, ok := parseSnapshotFile(artifact, version, name); ok {
			if _, gone := drop[sf.Build]; gone {
				_ = s.Delete(path.Join(dir, f.Name))
			}
		}
	}
	return len(drop)
}

// resolveSnapshot maps a non-unique snapshot request to the newest
// stored unique build with the same classifier and extension.
func (s *Store) resolveSnapshot(a aliasRef) (string, bool) {
	var best *snapshotFile
	for _, b := range s.snapshotBuilds(a.Group, a.Artifact, a.Version) {
		if b.Classifier != a.Classifier || b.Ext != a.Ext {
			continue
		}
		if best == nil || newerBuild(b, *best) {
			cp := b
			best = &cp
		}
	}
	if best == nil {
		return "", false
	}
	return a.target(best.Value), true
}

// resolveFile returns rel when stored, else the unique snapshot build
// it aliases.
func (s *Store) resolveFile(rel string) (string, bool) {
	rel = cleanRel(rel)
	if s.Exists(rel) {
		return rel, true
	}
	if a, ok := parseSnapshotAlias(rel); ok {
		return s.resolveSnapshot(a)
	}
	return "", false
}

// DeleteVersion removes all files under one Maven GAV version
// directory and invalidates adjacent maven-metadata files. Maven's
// repository layout is path-defined, so this stays within the
// group/artifact/version prefix instead of deleting arbitrary paths.
// The returned count covers artifact files (checksums excluded).
func (s *Store) DeleteVersion(groupID, artifactID, version string) (int, error) {
	groupID = strings.TrimSpace(groupID)
	artifactID = strings.TrimSpace(artifactID)
	version = strings.TrimSpace(version)
	if groupID == "" || artifactID == "" || version == "" || strings.ContainsAny(artifactID+version, "/\\") || version == ".." || version == "." {
		return 0, registry.ErrInvalidPackageName
	}
	versionPrefix := path.Join(groupPath(groupID), artifactID, version)
	files, err := s.ListFiles(versionPrefix)
	if err != nil {
		return 0, err
	}
	if len(files) == 0 {
		return 0, registry.ErrPackageNotFound
	}

	var deleted int
	var firstErr error
	for _, f := range files {
		if err := s.Delete(f.Path); err != nil && !isNotFound(err) {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if isPrimary(path.Base(f.Path)) {
			deleted++
		}
	}

	artifactPrefix := path.Join(groupPath(groupID), artifactID)
	for _, f := range s.readDir(artifactPrefix) {
		if !f.IsDir && isMutablePath(f.Name) {
			_ = s.Delete(path.Join(artifactPrefix, f.Name))
		}
	}
	return deleted, firstErr
}

type File struct {
	Path    string
	Size    int64
	ModTime time.Time
}

func (s *Store) ListFiles(prefix string) ([]File, error) {
	root := s.join(prefix)
	var out []File
	var walk func(abs, rel string) error
	walk = func(abs, rel string) error {
		entries, err := s.fs.ReadDir(abs)
		if err != nil {
			if isNotFound(err) {
				return nil
			}
			return err
		}
		for _, e := range entries {
			childAbs := path.Join(abs, e.Name)
			childRel := path.Join(rel, e.Name)
			if e.IsDir {
				if err := walk(childAbs, childRel); err != nil {
					return err
				}
				continue
			}
			out = append(out, File{Path: childRel, Size: e.Size})
		}
		return nil
	}
	if err := walk(root, cleanRel(prefix)); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

type Artifact struct {
	GroupID    string   `json:"group_id"`
	ArtifactID string   `json:"artifact_id"`
	Versions   []string `json:"versions"`
}

func (s *Store) ListArtifacts() ([]Artifact, error) {
	files, err := s.ListFiles("")
	if err != nil {
		return nil, err
	}
	versions := map[string]map[string]struct{}{}
	meta := map[string]Artifact{}
	for _, f := range files {
		c, ok := parseArtifactPath(f.Path)
		if !ok || !belongs(c.Artifact, c.Version, c.File) {
			continue
		}
		key := c.Name()
		if versions[key] == nil {
			versions[key] = map[string]struct{}{}
			meta[key] = Artifact{GroupID: c.Group, ArtifactID: c.Artifact}
		}
		versions[key][c.Version] = struct{}{}
	}
	out := make([]Artifact, 0, len(meta))
	for key, a := range meta {
		for v := range versions[key] {
			a.Versions = append(a.Versions, v)
		}
		pkgbase.SortVersions(a.Versions)
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GroupID == out[j].GroupID {
			return out[i].ArtifactID < out[j].ArtifactID
		}
		return out[i].GroupID < out[j].GroupID
	})
	return out, nil
}

func (s *Store) Count() (artifacts, versions, files int, bytes int64) {
	arts, _ := s.ListArtifacts()
	artifacts = len(arts)
	for _, a := range arts {
		versions += len(a.Versions)
	}
	all, _ := s.ListFiles("")
	files = len(all)
	for _, f := range all {
		bytes += f.Size
	}
	return
}

func (s *Store) PurgeAll() (int, int64, []error) {
	return s.purge(func(File) bool { return true })
}

func (s *Store) PurgeMutable() (int, int64, []error) {
	return s.purge(func(f File) bool { return isMutablePath(f.Path) })
}

func (s *Store) purge(match func(File) bool) (int, int64, []error) {
	wfs, ok := s.fs.(rawfs.WritableRawFS)
	if !ok {
		return 0, 0, []error{fmt.Errorf("maven: backend read-only")}
	}
	files, err := s.ListFiles("")
	if err != nil {
		return 0, 0, []error{err}
	}
	var count int
	var bytes int64
	var errs []error
	for _, f := range files {
		if !match(f) {
			continue
		}
		if err := wfs.Delete(s.join(f.Path)); err != nil && !isNotFound(err) {
			errs = append(errs, err)
			continue
		}
		count++
		bytes += f.Size
	}
	return count, bytes, errs
}

func serveFile(w http.ResponseWriter, r *http.Request, s *Store, rel string) bool {
	rc, fi, err := s.Open(rel)
	if err != nil {
		return false
	}
	defer rc.Close()
	writeFileResponse(w, r, rel, rc, fi)
	return true
}

func writeFileResponse(w http.ResponseWriter, r *http.Request, rel string, rc io.Reader, fi *rawfs.FileInfo) {
	setContentType(w, rel)
	if fi != nil && fi.Size >= 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", fi.Size))
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, rc)
	}
}

func writeBody(w http.ResponseWriter, r *http.Request, rel string, body []byte) {
	if _, algo := splitChecksum(rel); algo != "" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	} else {
		setContentType(w, rel)
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

func setContentType(w http.ResponseWriter, rel string) {
	if ct := mime.TypeByExtension(path.Ext(rel)); ct != "" {
		w.Header().Set("Content-Type", ct)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
}

// isMutablePath reports whether a cached path can change upstream:
// metadata documents and non-unique SNAPSHOT aliases (plus checksums).
func isMutablePath(p string) bool {
	base := path.Base(p)
	if base == metadataFile || strings.HasPrefix(base, metadataFile+".") {
		return true
	}
	b, _ := splitChecksum(p)
	_, ok := parseSnapshotAlias(b)
	return ok
}

func packageDetail(s *Store, name string) (*registry.PackageDetail, error) {
	group, artifact := splitName(name)
	if group == "" || artifact == "" {
		return nil, registry.ErrPackageNotFound
	}
	arts, err := s.ListArtifacts()
	if err != nil {
		return nil, err
	}
	for _, a := range arts {
		if a.GroupID != group || a.ArtifactID != artifact {
			continue
		}
		detail := &registry.MavenArtifactDetail{GroupID: group, ArtifactID: artifact}
		for _, v := range a.Versions {
			detail.Versions = append(detail.Versions, registry.MavenVersionDetail{
				Version: v,
				JarSize: fileSize(s, group, artifact, v, ".jar"),
				PomSize: fileSize(s, group, artifact, v, ".pom"),
			})
		}
		if len(detail.Versions) > 0 {
			detail.LatestVersion = detail.Versions[len(detail.Versions)-1].Version
		}
		return &registry.PackageDetail{Type: service.RegistryTypeMaven, Name: group + ":" + artifact, Maven: detail}, nil
	}
	return nil, registry.ErrPackageNotFound
}

func splitName(name string) (string, string) {
	if strings.Contains(name, ":") {
		parts := strings.SplitN(name, ":", 2)
		return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	}
	parts := strings.Split(cleanRel(name), "/")
	if len(parts) < 2 {
		return "", ""
	}
	return strings.Join(parts[:len(parts)-1], "."), parts[len(parts)-1]
}

func versionFile(group, artifact, version, ext string) string {
	return path.Join(groupPath(group), artifact, version, artifact+"-"+version+ext)
}

func fileSize(s *Store, group, artifact, version, ext string) int64 {
	rel, ok := s.resolveFile(versionFile(group, artifact, version, ext))
	if !ok {
		return 0
	}
	fi, err := s.fs.Stat(s.join(rel))
	if err != nil {
		return 0
	}
	return fi.Size
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	low := strings.ToLower(err.Error())
	return strings.Contains(low, "not found") || strings.Contains(low, "no such file") || strings.Contains(low, "does not exist")
}
