// Package alpine implements an Alpine Linux apk repository:
//
//	GET    /{branch}/{repository}/{arch}/APKINDEX.tar.gz   package index (signed when a key is set)
//	GET    /{branch}/{repository}/{arch}/{name}-{ver}.apk  package download
//	GET    /keys/{keyname}.rsa.pub                         index signing public key (local)
//	PUT    /upload/{branch}/{repository}                   upload an .apk (local, allow_push)
//	DELETE /{branch}/{repository}/{arch}/{name}-{ver}.apk  delete a package (local)
//
// Uploaded packages are placed by the arch field of their .PKGINFO;
// "noarch" packages are copied into every arch directory that already
// exists under {branch}/{repository} (x86_64 and aarch64 when none
// exist yet). APKINDEX.tar.gz is regenerated after every change and,
// when the repository has a PEM RSA SigningKey, prefixed with an
// abuild-compatible ".SIGN.RSA.{keyname}.rsa.pub" signature
// (keyname = SigningKeyName, default "kutu").
//
// Remote repos proxy a mirror such as https://dl-cdn.alpinelinux.org/alpine
// (indexes honour MutableTTL, packages are cached forever). Virtual
// repos merge member indexes into one unsigned APKINDEX, so clients
// need --allow-untrusted (or a trusted local member addressed directly).
//
// Client configuration:
//
//	wget -O /etc/apk/keys/kutu.rsa.pub {base}/keys/kutu.rsa.pub
//	echo "{scheme}://x:<token>@{host}{prefix}/v3.20/main" >> /etc/apk/repositories
//	apk update
//	curl -u x:<token> -T pkg.apk {base}/upload/v3.20/main
package alpine

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/registry/signing"
	"github.com/rakunlabs/kutu/internal/registry/virtualbase"
	"github.com/rakunlabs/kutu/internal/service"
)

const typ = service.RegistryTypeAlpine

const (
	indexFile = "APKINDEX.tar.gz"
	metaDir   = "_meta"
	keysDir   = "keys"
)

// defaultArches receive noarch packages when no arch directory exists yet.
var defaultArches = []string{"x86_64", "aarch64"}

// Store wraps pkgbase.Store with the alpine layout.
type Store struct{ *pkgbase.Store }

func validSegment(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\\\x00") && !strings.HasPrefix(s, "_")
}

// parsePath splits "/{branch}/{repository}/{arch}/{file}".
func parsePath(p string) (branch, repo, arch, file string, ok bool) {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) != 4 {
		return "", "", "", "", false
	}
	for _, s := range parts {
		if !validSegment(s) {
			return "", "", "", "", false
		}
	}
	return parts[0], parts[1], parts[2], parts[3], true
}

// located is an index record plus its repository location.
type located struct {
	record
	Branch string
	Repo   string
	Dir    string
}

func (l located) rel() string { return path.Join(l.Branch, l.Repo, l.Dir, l.fileName()) }

func metaRel(branch, repo, arch, file string) string {
	return path.Join(metaDir, branch, repo, arch, file+".json")
}

func (s *Store) readMeta(branch, repo, arch, file string) (record, error) {
	b, err := s.Read(metaRel(branch, repo, arch, file))
	if err != nil {
		return record{}, err
	}
	var r record
	if err := json.Unmarshal(b, &r); err != nil {
		return record{}, err
	}
	return r, nil
}

// records returns every stored package record (local layout).
func (s *Store) records() ([]located, error) {
	var out []located
	err := s.Walk(metaDir, func(rel string, _ rawfs.DirEntry) error {
		parts := strings.Split(strings.TrimPrefix(rel, metaDir+"/"), "/")
		if len(parts) != 4 || !strings.HasSuffix(parts[3], ".apk.json") {
			return nil
		}
		b, err := s.Read(rel)
		if err != nil {
			return nil
		}
		var r record
		if json.Unmarshal(b, &r) != nil || r.Name == "" {
			return nil
		}
		out = append(out, located{record: r, Branch: parts[0], Repo: parts[1], Dir: parts[2]})
		return nil
	})
	return out, err
}

// archRecords returns the records of one arch directory, parsing (and
// back-filling metadata for) apks that lack a sidecar.
func (s *Store) archRecords(branch, repo, arch string) ([]record, error) {
	files, err := s.ListFiles(path.Join(branch, repo, arch))
	if err != nil {
		return nil, err
	}
	var out []record
	for _, f := range files {
		if !strings.HasSuffix(f.Name, ".apk") {
			continue
		}
		r, err := s.readMeta(branch, repo, arch, f.Name)
		if err != nil {
			body, rerr := s.Read(path.Join(branch, repo, arch, f.Name))
			if rerr != nil {
				continue
			}
			if r, err = parseAPK(body); err != nil {
				continue
			}
			if mb, err := json.Marshal(r); err == nil {
				_ = s.Write(metaRel(branch, repo, arch, f.Name), mb)
			}
		}
		out = append(out, r)
	}
	sortRecords(out)
	return out, nil
}

func summaries(recs []located) []registry.PackageSummary {
	set := map[string]map[string]struct{}{}
	for _, r := range recs {
		if set[r.Name] == nil {
			set[r.Name] = map[string]struct{}{}
		}
		set[r.Name][r.Version] = struct{}{}
	}
	out := make([]registry.PackageSummary, 0, len(set))
	for name, vs := range set {
		list := make([]string, 0, len(vs))
		for v := range vs {
			list = append(list, v)
		}
		pkgbase.SortVersions(list)
		out = append(out, registry.PackageSummary{Name: name, Versions: list})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func detail(name string, recs []located, stat func(rel string) (*rawfs.FileInfo, error)) (*registry.PackageDetail, error) {
	var mine []located
	for _, r := range recs {
		if r.Name == name {
			mine = append(mine, r)
		}
	}
	if len(mine) == 0 {
		return nil, registry.ErrPackageNotFound
	}
	byVer := map[string][]located{}
	var versions []string
	for _, r := range mine {
		if _, ok := byVer[r.Version]; !ok {
			versions = append(versions, r.Version)
		}
		byVer[r.Version] = append(byVer[r.Version], r)
	}
	pkgbase.SortVersions(versions)
	latest := byVer[versions[len(versions)-1]][0]
	d := &registry.GenericPackageDetail{
		LatestVersion: latest.Version,
		Description:   latest.Description,
		Homepage:      latest.URL,
		License:       latest.License,
		Metadata:      map[string]string{},
	}
	if latest.Origin != "" {
		d.Metadata["origin"] = latest.Origin
	}
	if latest.Maintainer != "" {
		d.Metadata["maintainer"] = latest.Maintainer
	}
	for i := len(versions) - 1; i >= 0; i-- {
		rows := byVer[versions[i]]
		row := registry.GenericVersionDetail{Version: versions[i], Metadata: map[string]string{}}
		var arches, repos []string
		for _, r := range rows {
			row.Files = append(row.Files, registry.GenericFile{Name: r.rel(), Size: r.Size})
			row.Size += r.Size
			arches = appendUnique(arches, r.Dir)
			repos = appendUnique(repos, r.Branch+"/"+r.Repo)
			if row.PublishedAt == "" {
				if r.BuildDate > 0 {
					row.PublishedAt = time.Unix(r.BuildDate, 0).UTC().Format(time.RFC3339)
				} else if stat != nil {
					if fi, err := stat(r.rel()); err == nil {
						row.PublishedAt = fi.ModTime.UTC().Format(time.RFC3339)
					}
				}
			}
		}
		row.Metadata["arch"] = strings.Join(arches, ", ")
		row.Metadata["repository"] = strings.Join(repos, ", ")
		if len(rows[0].Depends) > 0 {
			row.Metadata["depends"] = strings.Join(rows[0].Depends, " ")
		}
		if rows[0].Checksum != "" {
			row.Metadata["checksum"] = rows[0].Checksum
		}
		d.Versions = append(d.Versions, row)
	}
	return &registry.PackageDetail{Type: typ, Name: name, Generic: d}, nil
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

func artifactInfo(recs []located, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	for _, r := range recs {
		if r.Name == ref.Name && (ref.Version == "" || r.Version == ref.Version) {
			m := registry.ArtifactMeta{License: r.License}
			if r.BuildDate > 0 {
				m.PublishedAt = time.Unix(r.BuildDate, 0).UTC()
			}
			return m, nil
		}
	}
	return registry.ArtifactMeta{}, registry.ErrPackageNotFound
}

func classify(r *http.Request) (registry.ArtifactRef, bool) {
	_, _, _, file, ok := parsePath(r.URL.Path)
	if !ok {
		return registry.ArtifactRef{}, false
	}
	name, ver, ok := splitFilename(file)
	if !ok {
		return registry.ArtifactRef{}, false
	}
	return registry.ArtifactRef{Name: name, Version: ver}, true
}

func contentType(file string) string {
	if strings.HasSuffix(file, ".tar.gz") || strings.HasSuffix(file, ".apk") {
		return "application/gzip"
	}
	return "application/octet-stream"
}

// ── Local ──

type Local struct {
	*pkgbase.Repo
	store   *Store
	key     *rsa.PrivateKey
	keyName string
	mu      sync.Mutex
}

// keyNameOf normalises SigningKeyName ("kutu", "kutu.rsa.pub" → "kutu").
func keyNameOf(r *service.RegistryRepository) string {
	n := strings.TrimSpace(r.SigningKeyName)
	n = strings.TrimSuffix(strings.TrimSuffix(n, ".pub"), ".rsa")
	if n == "" || strings.ContainsAny(n, "/\\\x00") {
		return "kutu"
	}
	return n
}

func NewLocalFactory() registry.Factory {
	return func(_ context.Context, deps registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		base, err := pkgbase.NewLocalRepo(deps, typ, ns, r)
		if err != nil {
			return nil, err
		}
		l := &Local{Repo: base, store: &Store{base.Store}, keyName: keyNameOf(r)}
		if strings.TrimSpace(r.SigningKey) != "" {
			if l.key, err = signing.ParseRSAKey(r.SigningKey); err != nil {
				return nil, fmt.Errorf("alpine/local %s/%s: %w", ns, r.Name, err)
			}
		}
		return l, nil
	}
}

func (l *Local) Close() error  { return nil }
func (l *Local) Store() *Store { return l.store }

func (l *Local) ListPackages(context.Context) ([]registry.PackageSummary, error) {
	recs, err := l.store.records()
	if err != nil {
		return nil, err
	}
	return summaries(recs), nil
}

func (l *Local) Stats(ctx context.Context) (registry.Stats, error) {
	return pkgbase.StatsOf(ctx, l, l.store.Store), nil
}

func (l *Local) PackageDetail(_ context.Context, name string) (*registry.PackageDetail, error) {
	recs, err := l.store.records()
	if err != nil {
		return nil, err
	}
	return detail(name, recs, l.store.Stat)
}

func (l *Local) ArtifactInfo(_ context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	recs, err := l.store.records()
	if err != nil {
		return registry.ArtifactMeta{}, err
	}
	return artifactInfo(recs, ref)
}

func (l *Local) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) {
	return classify(r)
}

// DeleteVersion removes name@version from every branch/repository/arch.
func (l *Local) DeleteVersion(_ context.Context, name, version string) error {
	recs, err := l.store.records()
	if err != nil {
		return err
	}
	var hit []located
	for _, r := range recs {
		if r.Name == name && r.Version == version {
			hit = append(hit, r)
		}
	}
	if len(hit) == 0 {
		return registry.ErrPackageNotFound
	}
	for _, r := range hit {
		if err := l.deleteFile(r.Branch, r.Repo, r.Dir, r.fileName()); err != nil {
			return err
		}
	}
	return nil
}

func (l *Local) deleteFile(branch, repo, arch, file string) error {
	if err := l.store.Delete(path.Join(branch, repo, arch, file)); err != nil {
		return err
	}
	if err := l.store.Delete(metaRel(branch, repo, arch, file)); err != nil {
		return err
	}
	if err := l.regenerate(branch, repo, arch); err != nil {
		return err
	}
	l.EmitDeleted(path.Join(branch, repo, arch, file))
	return nil
}

// PromoteVersion copies name@version into dst (same branch/repository/arch).
func (l *Local) PromoteVersion(_ context.Context, dst registry.Registry, name, version string) error {
	d, ok := dst.(*Local)
	if !ok {
		return fmt.Errorf("alpine: promote target %s/%s is not a local alpine repository", dst.Namespace(), dst.Name())
	}
	recs, err := l.store.records()
	if err != nil {
		return err
	}
	found := false
	for _, r := range recs {
		if r.Name != name || r.Version != version {
			continue
		}
		found = true
		body, err := l.store.Read(r.rel())
		if err != nil {
			return err
		}
		if code, err := d.Guard.Check(d.store.Store, d.store.Exists(r.rel()), int64(len(body))); code != 0 {
			return err
		}
		if err := d.store.Write(r.rel(), body); err != nil {
			return err
		}
		mb, _ := json.Marshal(r.record)
		if err := d.store.Write(metaRel(r.Branch, r.Repo, r.Dir, r.fileName()), mb); err != nil {
			return err
		}
		if err := d.regenerate(r.Branch, r.Repo, r.Dir); err != nil {
			return err
		}
		d.EmitPublished(r.rel(), int64(len(body)))
	}
	if !found {
		return registry.ErrPackageNotFound
	}
	return nil
}

func (l *Local) description(branch, repo string) string {
	return fmt.Sprintf("kutu %s/%s %s/%s", l.NS, l.RepoName, branch, repo)
}

// buildIndex renders the APKINDEX.tar.gz of one arch directory.
func (l *Local) buildIndex(branch, repo, arch string) ([]byte, error) {
	recs, err := l.store.archRecords(branch, repo, arch)
	if err != nil {
		return nil, err
	}
	return buildIndexArchive(renderIndex(recs), l.description(branch, repo), l.key, l.keyName)
}

func (l *Local) regenerate(branch, repo, arch string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, err := l.buildIndex(branch, repo, arch)
	if err != nil {
		return err
	}
	return l.store.Write(path.Join(branch, repo, arch, indexFile), b)
}

func (l *Local) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		l.serveRead(w, r)
	case http.MethodPut, http.MethodPost:
		l.upload(w, r)
	case http.MethodDelete:
		l.remove(w, r)
	default:
		pkgbase.Error(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (l *Local) publicKey() ([]byte, bool) {
	if l.key == nil {
		return nil, false
	}
	b, err := signing.RSAPublicPEM(l.key)
	return b, err == nil
}

func (l *Local) serveRead(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.Trim(r.URL.Path, "/")
	if trimmed == keysDir+"/"+l.keyName+".rsa.pub" {
		if b, ok := l.publicKey(); ok {
			pkgbase.WriteBytes(w, r, http.StatusOK, "application/x-pem-file", b)
			return
		}
		pkgbase.NotFound(w)
		return
	}
	branch, repo, arch, file, ok := parsePath(trimmed)
	if !ok {
		pkgbase.NotFound(w)
		return
	}
	rel := path.Join(branch, repo, arch, file)
	if pkgbase.ServeStored(w, r, l.store.Store, rel, contentType(file)) {
		return
	}
	if file == indexFile {
		b, err := l.buildIndex(branch, repo, arch)
		if err != nil {
			pkgbase.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		pkgbase.WriteBytes(w, r, http.StatusOK, contentType(file), b)
		return
	}
	pkgbase.NotFound(w)
}

func (l *Local) upload(w http.ResponseWriter, r *http.Request) {
	if !l.CheckPush(w) {
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "upload" || !validSegment(parts[1]) || !validSegment(parts[2]) {
		pkgbase.Error(w, http.StatusBadRequest, "expected PUT /upload/{branch}/{repository}")
		return
	}
	branch, repo := parts[1], parts[2]
	body, err := pkgbase.ReadBody(r.Body, l.MaxUpload)
	if err != nil {
		pkgbase.Error(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	rec, err := parseAPK(body)
	if err != nil {
		pkgbase.Error(w, http.StatusBadRequest, "invalid apk: "+err.Error())
		return
	}
	if !validSegment(rec.Arch) || !validSegment(rec.fileName()) {
		pkgbase.Error(w, http.StatusBadRequest, "invalid pkgname, pkgver or arch")
		return
	}
	arches := []string{rec.Arch}
	if rec.Arch == "noarch" {
		dirs, _ := l.store.ListDirs(path.Join(branch, repo))
		arches = arches[:0]
		for _, d := range dirs {
			if validSegment(d) && d != "noarch" {
				arches = append(arches, d)
			}
		}
		if len(arches) == 0 {
			arches = defaultArches
		}
	}
	file := rec.fileName()
	exists := false
	for _, a := range arches {
		exists = exists || l.store.Exists(path.Join(branch, repo, a, file))
	}
	if !l.AllowPublish(w, exists, int64(len(body))*int64(len(arches))) {
		return
	}
	meta, _ := json.Marshal(rec)
	var stored []string
	for _, a := range arches {
		rel := path.Join(branch, repo, a, file)
		if err := l.store.Write(rel, body); err != nil {
			pkgbase.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := l.store.Write(metaRel(branch, repo, a, file), meta); err != nil {
			pkgbase.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := l.regenerate(branch, repo, a); err != nil {
			pkgbase.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		stored = append(stored, rel)
		l.EmitPublished(rel, int64(len(body)))
	}
	pkgbase.WriteJSON(w, r, http.StatusCreated, map[string]any{
		"name": rec.Name, "version": rec.Version, "arch": rec.Arch, "checksum": rec.Checksum,
		"size": len(body), "files": stored,
	})
}

func (l *Local) remove(w http.ResponseWriter, r *http.Request) {
	if !l.CheckPush(w) {
		return
	}
	branch, repo, arch, file, ok := parsePath(r.URL.Path)
	if !ok || !strings.HasSuffix(file, ".apk") || !l.store.Exists(path.Join(branch, repo, arch, file)) {
		pkgbase.NotFound(w)
		return
	}
	if err := l.deleteFile(branch, repo, arch, file); err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Remote ──

type Remote struct {
	*pkgbase.RemoteRepo
	store *Store

	mu    sync.Mutex
	cache map[string]cachedIndex
}

type cachedIndex struct {
	mod  time.Time
	size int64
	recs []record
}

func NewRemoteFactory() registry.Factory {
	return func(_ context.Context, deps registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		base, err := pkgbase.NewRemoteRepo(deps, typ, ns, r, "/")
		if err != nil {
			return nil, err
		}
		return &Remote{RemoteRepo: base, store: &Store{base.Store}, cache: map[string]cachedIndex{}}, nil
	}
}

func (rr *Remote) Store() *Store { return rr.store }

// cachedFiles lists cached .apk files as located records (name and
// version only, from the file name).
func (rr *Remote) cachedFiles() ([]located, error) {
	var out []located
	err := rr.store.Walk("", func(rel string, e rawfs.DirEntry) error {
		branch, repo, arch, file, ok := parsePath(rel)
		if !ok {
			return nil
		}
		name, ver, ok := splitFilename(file)
		if !ok {
			return nil
		}
		out = append(out, located{record: record{Name: name, Version: ver, Arch: arch, Size: e.Size}, Branch: branch, Repo: repo, Dir: arch})
		return nil
	})
	return out, err
}

// indexRecords parses one cached index (memoised by mod time + size).
func (rr *Remote) indexRecords(rel string) []record {
	fi, err := rr.store.Stat(rel)
	if err != nil {
		return nil
	}
	rr.mu.Lock()
	c, ok := rr.cache[rel]
	rr.mu.Unlock()
	if ok && c.mod.Equal(fi.ModTime) && c.size == fi.Size {
		return c.recs
	}
	b, err := rr.store.Read(rel)
	if err != nil {
		return nil
	}
	text, err := readIndex(b)
	if err != nil {
		return nil
	}
	recs := parseIndexText(text)
	rr.mu.Lock()
	rr.cache[rel] = cachedIndex{mod: fi.ModTime, size: fi.Size, recs: recs}
	rr.mu.Unlock()
	return recs
}

// indexed returns every record of every cached index.
func (rr *Remote) indexed() []located {
	var out []located
	_ = rr.store.Walk("", func(rel string, _ rawfs.DirEntry) error {
		branch, repo, arch, file, ok := parsePath(rel)
		if !ok || file != indexFile {
			return nil
		}
		for _, r := range rr.indexRecords(rel) {
			out = append(out, located{record: r, Branch: branch, Repo: repo, Dir: arch})
		}
		return nil
	})
	return out
}

func (rr *Remote) ListPackages(context.Context) ([]registry.PackageSummary, error) {
	files, err := rr.cachedFiles()
	if err != nil {
		return nil, err
	}
	return summaries(files), nil
}

func (rr *Remote) Stats(ctx context.Context) (registry.Stats, error) {
	return pkgbase.StatsOf(ctx, rr, rr.store.Store), nil
}

// PackageDetail reports cached packages, enriched from cached indexes.
func (rr *Remote) PackageDetail(_ context.Context, name string) (*registry.PackageDetail, error) {
	files, err := rr.cachedFiles()
	if err != nil {
		return nil, err
	}
	meta := map[string]record{}
	for _, r := range rr.indexed() {
		if r.Name == name {
			meta[r.rel()] = r.record
		}
	}
	for i := range files {
		if m, ok := meta[files[i].rel()]; ok {
			m.Size = files[i].Size
			files[i].record = m
		}
	}
	return detail(name, files, rr.store.Stat)
}

func (rr *Remote) ArtifactInfo(_ context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	return artifactInfo(rr.indexed(), ref)
}

func (rr *Remote) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) {
	return classify(r)
}

// PurgeCache drops cached indexes (or everything with opts.All).
func (rr *Remote) PurgeCache(_ context.Context, opts registry.PurgeOptions) (registry.PurgeStats, error) {
	if opts.All {
		return rr.Purge(opts), nil
	}
	var st registry.PurgeStats
	err := rr.store.Walk("", func(rel string, e rawfs.DirEntry) error {
		if path.Base(rel) != indexFile {
			st.Skipped++
			return nil
		}
		if err := rr.store.Delete(rel); err != nil {
			st.Errors = append(st.Errors, err.Error())
			return nil
		}
		st.PurgedFiles++
		st.PurgedBytes += e.Size
		return nil
	})
	if err != nil {
		st.Errors = append(st.Errors, err.Error())
	}
	return st, nil
}

// Prefetch warms an index and a package. name is either
// "{branch}/{repository}/{arch}/{pkgname}" or a bare pkgname looked
// up in already-cached indexes.
func (rr *Remote) Prefetch(ctx context.Context, name, version string) error {
	var cands []located
	if parts := strings.Split(strings.Trim(name, "/"), "/"); len(parts) == 4 {
		rel := path.Join(parts[0], parts[1], parts[2], indexFile)
		if _, _, _, _, ok := parsePath(rel); !ok {
			return fmt.Errorf("alpine prefetch: invalid name %q", name)
		}
		if _, err := rr.FetchCached(ctx, rel, "/"+rel, true); err != nil {
			return err
		}
		for _, r := range rr.indexRecords(rel) {
			if r.Name == parts[3] {
				cands = append(cands, located{record: r, Branch: parts[0], Repo: parts[1], Dir: parts[2]})
			}
		}
	} else {
		seen := map[string]bool{}
		for _, r := range rr.indexed() {
			if r.Name != name {
				continue
			}
			idx := path.Join(r.Branch, r.Repo, r.Dir, indexFile)
			if !seen[idx] {
				seen[idx] = true
				_, _ = rr.FetchCached(ctx, idx, "/"+idx, true)
			}
			cands = append(cands, r)
		}
	}
	if len(cands) == 0 {
		return registry.ErrPackageNotFound
	}
	if version == "" {
		vs := make([]string, 0, len(cands))
		for _, c := range cands {
			vs = append(vs, c.Version)
		}
		version = pkgbase.Latest(vs)
	}
	fetched := false
	for _, c := range cands {
		if c.Version != version {
			continue
		}
		if _, err := rr.FetchCached(ctx, c.rel(), "/"+c.rel(), false); err != nil {
			return err
		}
		fetched = true
	}
	if !fetched {
		return registry.ErrPackageNotFound
	}
	return nil
}

func (rr *Remote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !pkgbase.IsRead(r) {
		pkgbase.Error(w, http.StatusMethodNotAllowed, "remote registry is read-only")
		return
	}
	branch, repo, arch, file, ok := parsePath(r.URL.Path)
	if !ok {
		pkgbase.NotFound(w)
		return
	}
	rel := path.Join(branch, repo, arch, file)
	mutable := file == indexFile || !strings.HasSuffix(file, ".apk")
	if !mutable && pkgbase.ServeStored(w, r, rr.store.Store, rel, contentType(file)) {
		return
	}
	rr.ServeCached(w, r, rel, "/"+rel, contentType(file), mutable)
}

// ── Virtual ──

// Virtual merges member APKINDEX files; everything else is first-hit.
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

func (v *Virtual) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	branch, repo, _, file, ok := parsePath(r.URL.Path)
	if !pkgbase.IsRead(r) || !ok || file != indexFile {
		v.Virtual.ServeHTTP(w, r)
		return
	}
	bodies := v.CollectMembers(r)
	if len(bodies) == 0 {
		pkgbase.NotFound(w)
		return
	}
	merged, err := mergeIndexes(bodies)
	if err != nil {
		pkgbase.Error(w, http.StatusBadGateway, err.Error())
		return
	}
	b, err := buildIndexArchive(renderIndex(merged), fmt.Sprintf("kutu %s/%s %s/%s (virtual)", v.Namespace(), v.Name(), branch, repo), nil, "")
	if err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	pkgbase.WriteBytes(w, r, http.StatusOK, contentType(file), b)
}

// mergeIndexes unions APKINDEX records; the first member wins for a
// duplicate name+version+arch.
func mergeIndexes(bodies [][]byte) ([]record, error) {
	seen := map[string]bool{}
	var out []record
	var firstErr error
	for _, b := range bodies {
		text, err := readIndex(b)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, r := range parseIndexText(text) {
			k := r.Name + "\x00" + r.Version + "\x00" + r.Arch
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, r)
		}
	}
	if out == nil && firstErr != nil {
		return nil, errors.Join(errors.New("alpine virtual: no readable member index"), firstErr)
	}
	sortRecords(out)
	return out, nil
}
