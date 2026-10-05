// Package cocoapods implements a CocoaPods CDN spec repository (the
// trunk CDN layout served by cdn.cocoapods.org):
//
//	GET    /CocoaPods-version.yml                              CDN marker (min/last, prefix_lengths [1,1,1])
//	GET    /all_pods.txt                                       every pod name
//	GET    /all_pods_versions_{a}_{b}_{c}.txt                  shard (md5(name)[0:3]): "Name/1.0.0/1.1.0"
//	GET    /deprecated_podspecs.txt                            deprecated podspec paths
//	GET    /Specs/{a}/{b}/{c}/{Name}/{ver}/{Name}.podspec.json podspec
//	GET    /files/{Name}/{ver}.zip                             hosted source archive
//	PUT    /pods/{Name}/{ver}                                  publish (local, allow_push): body = podspec JSON,
//	                                                           or multipart "podspec" (JSON) + "file" (source zip)
//	DELETE /pods/{Name}/{ver}                                  delete a version (local)
//
// When a source zip is uploaded the podspec "source" is rewritten to
// {"http": "{base}/files/{Name}/{ver}.zip"}; the base is resolved per
// request so it follows the host and prefix the client used. Remote
// repos proxy https://cdn.cocoapods.org (index files honour
// MutableTTL, podspecs are cached forever); virtual repos merge the
// index files across members and serve podspecs first-hit.
//
// Client configuration (Podfile):
//
//	source 'https://x:<token>@kutu.example.com/registries/{ns}/{repo}/'
package cocoapods

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/registry/virtualbase"
	"github.com/rakunlabs/kutu/internal/service"
)

const typ = service.RegistryTypeCocoaPods

// DefaultUpstream is used by remote repos that leave URL empty.
const DefaultUpstream = "https://cdn.cocoapods.org"

// baseToken is stored in hosted podspecs in place of the public base
// URL and substituted at serve time.
const baseToken = "{{kutu-base}}"

const (
	specsDir = "specs"
	filesDir = "files"
	indexDir = "index"
	metaDir  = "meta"
)

const versionYML = "---\nmin: 1.0.0\nlast: 1.16.2\nprefix_lengths:\n- 1\n- 1\n- 1\n"

const (
	ctText = "text/plain; charset=utf-8"
	ctYAML = "text/yaml; charset=utf-8"
	ctJSON = "application/json"
	ctZip  = "application/zip"
)

var shardRe = regexp.MustCompile(`^all_pods_versions_([0-9a-f])_([0-9a-f])_([0-9a-f])\.txt$`)

// Shard returns the three single-hex-char shard prefixes of name.
func Shard(name string) (string, string, string) {
	sum := md5.Sum([]byte(name))
	h := hex.EncodeToString(sum[:])
	return h[0:1], h[1:2], h[2:3]
}

// SpecPath is the CDN path of a podspec (without leading slash).
func SpecPath(name, version string) string {
	a, b, c := Shard(name)
	return path.Join("Specs", a, b, c, name, version, name+".podspec.json")
}

// ShardFile is the CDN shard file name for name.
func ShardFile(name string) string {
	a, b, c := Shard(name)
	return "all_pods_versions_" + a + "_" + b + "_" + c + ".txt"
}

func validSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		if r == '/' || r == '\\' || r <= ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

type reqKind int

const (
	reqOther reqKind = iota
	reqVersionYML
	reqAllPods
	reqDeprecated
	reqShard
	reqSpec
	reqFile
	reqPods
)

type parsed struct {
	kind    reqKind
	name    string
	version string
	shard   string
}

func parse(p string) parsed {
	trimmed := strings.Trim(p, "/")
	switch trimmed {
	case "CocoaPods-version.yml":
		return parsed{kind: reqVersionYML}
	case "all_pods.txt":
		return parsed{kind: reqAllPods}
	case "deprecated_podspecs.txt":
		return parsed{kind: reqDeprecated}
	}
	if shardRe.MatchString(trimmed) {
		return parsed{kind: reqShard, shard: trimmed}
	}
	parts := strings.Split(trimmed, "/")
	switch {
	case len(parts) == 7 && parts[0] == "Specs":
		name, ver := parts[4], parts[5]
		if !validSegment(name) || !validSegment(ver) || parts[6] != name+".podspec.json" {
			return parsed{}
		}
		a, b, c := Shard(name)
		if parts[1] != a || parts[2] != b || parts[3] != c {
			return parsed{}
		}
		return parsed{kind: reqSpec, name: name, version: ver}
	case len(parts) == 3 && parts[0] == "files" && strings.HasSuffix(parts[2], ".zip"):
		name, ver := parts[1], strings.TrimSuffix(parts[2], ".zip")
		if validSegment(name) && validSegment(ver) {
			return parsed{kind: reqFile, name: name, version: ver}
		}
	case len(parts) == 3 && parts[0] == "pods":
		if validSegment(parts[1]) && validSegment(parts[2]) {
			return parsed{kind: reqPods, name: parts[1], version: parts[2]}
		}
	}
	return parsed{}
}

func classify(r *http.Request) (registry.ArtifactRef, bool) {
	p := parse(r.URL.Path)
	switch p.kind {
	case reqSpec, reqFile:
		return registry.ArtifactRef{Name: p.name, Version: p.version}, true
	}
	return registry.ArtifactRef{}, false
}

// ── Store ──

// Store wraps pkgbase.Store with the CocoaPods layout:
// specs/{Name}/{ver}/{Name}.podspec.json, files/{Name}/{ver}.zip and
// generated index/*.txt files.
type Store struct{ *pkgbase.Store }

func (s *Store) specRel(name, ver string) string {
	return path.Join(specsDir, name, ver, name+".podspec.json")
}

func (s *Store) zipRel(name, ver string) string {
	return path.Join(filesDir, name, ver+".zip")
}

// Versions returns the stored versions of name (ascending).
func (s *Store) Versions(name string) []string {
	dirs, _ := s.ListDirs(path.Join(specsDir, name))
	var out []string
	for _, d := range dirs {
		if s.Exists(s.specRel(name, d)) {
			out = append(out, d)
		}
	}
	pkgbase.SortVersions(out)
	return out
}

// ListPackages returns every pod with at least one podspec.
func (s *Store) ListPackages(context.Context) ([]registry.PackageSummary, error) {
	names, err := s.ListDirs(specsDir)
	if err != nil {
		return nil, err
	}
	out := make([]registry.PackageSummary, 0, len(names))
	for _, n := range names {
		if vs := s.Versions(n); len(vs) > 0 {
			out = append(out, registry.PackageSummary{Name: n, Versions: vs})
		}
	}
	return out, nil
}

func (s *Store) readSpec(name, ver string) (map[string]any, error) {
	b, err := s.Read(s.specRel(name, ver))
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func isDeprecated(spec map[string]any) bool {
	if b, ok := spec["deprecated"].(bool); ok && b {
		return true
	}
	if s, ok := spec["deprecated_in_favor_of"].(string); ok && s != "" {
		return true
	}
	return false
}

// rebuild regenerates the shard of name, all_pods.txt and the
// deprecated list entries of name.
func (s *Store) rebuild(name string) error {
	pkgs, err := s.ListPackages(context.Background())
	if err != nil {
		return err
	}
	shard := ShardFile(name)
	var all, lines bytes.Buffer
	for _, p := range pkgs {
		all.WriteString(p.Name + "\n")
		if ShardFile(p.Name) == shard {
			lines.WriteString(p.Name + "/" + strings.Join(p.Versions, "/") + "\n")
		}
	}
	if err := s.Write(path.Join(indexDir, "all_pods.txt"), all.Bytes()); err != nil {
		return err
	}
	if err := s.Write(path.Join(indexDir, shard), lines.Bytes()); err != nil {
		return err
	}

	var kept []string
	if old, err := s.Read(path.Join(indexDir, "deprecated_podspecs.txt")); err == nil {
		prefix := path.Dir(path.Dir(SpecPath(name, "x"))) + "/"
		for _, l := range strings.Split(string(old), "\n") {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, prefix) {
				kept = append(kept, l)
			}
		}
	}
	for _, v := range s.Versions(name) {
		if spec, err := s.readSpec(name, v); err == nil && isDeprecated(spec) {
			kept = append(kept, SpecPath(name, v))
		}
	}
	sort.Strings(kept)
	body := strings.Join(kept, "\n")
	if body != "" {
		body += "\n"
	}
	return s.Write(path.Join(indexDir, "deprecated_podspecs.txt"), []byte(body))
}

// DeleteVersion removes name@version and regenerates the indexes.
func (s *Store) DeleteVersion(_ context.Context, name, version string) error {
	if !validSegment(name) || !validSegment(version) || !s.Exists(s.specRel(name, version)) {
		return registry.ErrPackageNotFound
	}
	if err := s.Delete(s.specRel(name, version)); err != nil {
		return err
	}
	if err := s.Delete(s.zipRel(name, version)); err != nil {
		return err
	}
	return s.rebuild(name)
}

func specLicense(spec map[string]any) string {
	switch l := spec["license"].(type) {
	case string:
		return l
	case map[string]any:
		if t, ok := l["type"].(string); ok {
			return t
		}
	}
	return ""
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func detail(s *Store, name string) (*registry.PackageDetail, error) {
	if !validSegment(name) {
		return nil, registry.ErrPackageNotFound
	}
	versions := s.Versions(name)
	if len(versions) == 0 {
		return nil, registry.ErrPackageNotFound
	}
	latest := pkgbase.Latest(versions)
	d := &registry.GenericPackageDetail{LatestVersion: latest, Metadata: map[string]string{}}
	if spec, err := s.readSpec(name, latest); err == nil {
		d.Description = str(spec["summary"])
		if d.Description == "" {
			d.Description = str(spec["description"])
		}
		d.Homepage = str(spec["homepage"])
		d.License = specLicense(spec)
		if src, ok := spec["source"]; ok {
			if b, err := json.Marshal(src); err == nil {
				d.Metadata["source"] = strings.ReplaceAll(string(b), baseToken, "")
			}
		}
		if a, ok := spec["authors"]; ok {
			switch x := a.(type) {
			case string:
				d.Metadata["authors"] = x
			case map[string]any:
				var names []string
				for k := range x {
					names = append(names, k)
				}
				sort.Strings(names)
				d.Metadata["authors"] = strings.Join(names, ", ")
			}
		}
	}
	for i := len(versions) - 1; i >= 0; i-- {
		v := versions[i]
		row := registry.GenericVersionDetail{Version: v}
		if fi, err := s.Stat(s.specRel(name, v)); err == nil {
			row.PublishedAt = fi.ModTime.UTC().Format(time.RFC3339)
			row.Size += fi.Size
			row.Files = append(row.Files, registry.GenericFile{Name: name + ".podspec.json", Size: fi.Size})
		}
		if fi, err := s.Stat(s.zipRel(name, v)); err == nil {
			row.Size += fi.Size
			row.Files = append(row.Files, registry.GenericFile{Name: v + ".zip", Size: fi.Size})
		}
		if spec, err := s.readSpec(name, v); err == nil && isDeprecated(spec) {
			row.Yanked = true
		}
		d.Versions = append(d.Versions, row)
	}
	return &registry.PackageDetail{Type: typ, Name: name, Generic: d}, nil
}

func artifactInfo(s *Store, ref registry.ArtifactRef, withTime bool) (registry.ArtifactMeta, error) {
	spec, err := s.readSpec(ref.Name, ref.Version)
	if err != nil {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	meta := registry.ArtifactMeta{License: specLicense(spec)}
	if withTime {
		if fi, err := s.Stat(s.specRel(ref.Name, ref.Version)); err == nil {
			meta.PublishedAt = fi.ModTime
		}
	}
	return meta, nil
}

func serveIndex(w http.ResponseWriter, r *http.Request, s *Store, file string) {
	if pkgbase.ServeStored(w, r, s.Store, path.Join(indexDir, file), ctText) {
		return
	}
	pkgbase.WriteBytes(w, r, http.StatusOK, ctText, nil)
}

// ── Local ──

type Local struct {
	*pkgbase.Repo
	store *Store
}

func NewLocalFactory() registry.Factory {
	return func(_ context.Context, deps registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		base, err := pkgbase.NewLocalRepo(deps, typ, ns, r)
		if err != nil {
			return nil, err
		}
		return &Local{Repo: base, store: &Store{base.Store}}, nil
	}
}

func (l *Local) Close() error  { return nil }
func (l *Local) Store() *Store { return l.store }

func (l *Local) ListPackages(ctx context.Context) ([]registry.PackageSummary, error) {
	return l.store.ListPackages(ctx)
}

func (l *Local) Stats(ctx context.Context) (registry.Stats, error) {
	return pkgbase.StatsOf(ctx, l, l.store.Store), nil
}

func (l *Local) PackageDetail(_ context.Context, name string) (*registry.PackageDetail, error) {
	return detail(l.store, name)
}

func (l *Local) DeleteVersion(ctx context.Context, name, version string) error {
	if err := l.store.DeleteVersion(ctx, name, version); err != nil {
		return err
	}
	l.EmitDeleted(name + "@" + version)
	return nil
}

func (l *Local) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) { return classify(r) }

func (l *Local) ArtifactInfo(_ context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	return artifactInfo(l.store, ref, true)
}

// PromoteVersion copies name@version (podspec + hosted zip) into dst.
func (l *Local) PromoteVersion(_ context.Context, dst registry.Registry, name, version string) error {
	d, ok := dst.(*Local)
	if !ok {
		return fmt.Errorf("cocoapods: promote target %s/%s is not a cocoapods local repository", dst.Namespace(), dst.Name())
	}
	spec, err := l.store.Read(l.store.specRel(name, version))
	if err != nil {
		return registry.ErrPackageNotFound
	}
	zip, zerr := l.store.Read(l.store.zipRel(name, version))
	size := int64(len(spec) + len(zip))
	if _, err := d.Guard.Check(d.store.Store, d.store.Exists(d.store.specRel(name, version)), size); err != nil {
		return err
	}
	if zerr == nil {
		if err := d.store.Write(d.store.zipRel(name, version), zip); err != nil {
			return err
		}
	}
	if err := d.store.Write(d.store.specRel(name, version), spec); err != nil {
		return err
	}
	if err := d.store.rebuild(name); err != nil {
		return err
	}
	d.EmitPublished(name+"@"+version, size)
	return nil
}

func (l *Local) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := parse(r.URL.Path)
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		serveLocalRead(w, r, l.store, p)
	case http.MethodPut, http.MethodPost:
		if p.kind != reqPods {
			pkgbase.Error(w, http.StatusBadRequest, "expected PUT /pods/{Name}/{version}")
			return
		}
		l.publish(w, r, p.name, p.version)
	case http.MethodDelete:
		if !l.CheckPush(w) {
			return
		}
		if p.kind != reqPods {
			pkgbase.NotFound(w)
			return
		}
		if err := l.DeleteVersion(r.Context(), p.name, p.version); err != nil {
			if pkgbase.IsNotFound(err) {
				pkgbase.NotFound(w)
				return
			}
			pkgbase.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		pkgbase.Error(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func serveLocalRead(w http.ResponseWriter, r *http.Request, s *Store, p parsed) {
	switch p.kind {
	case reqVersionYML:
		pkgbase.WriteBytes(w, r, http.StatusOK, ctYAML, []byte(versionYML))
	case reqAllPods:
		serveIndex(w, r, s, "all_pods.txt")
	case reqDeprecated:
		serveIndex(w, r, s, "deprecated_podspecs.txt")
	case reqShard:
		serveIndex(w, r, s, p.shard)
	case reqSpec:
		b, err := s.Read(s.specRel(p.name, p.version))
		if err != nil {
			pkgbase.NotFound(w)
			return
		}
		b = bytes.ReplaceAll(b, []byte(baseToken), []byte(pkgbase.PublicBase(r)))
		pkgbase.WriteBytes(w, r, http.StatusOK, ctJSON, b)
	case reqFile:
		if !pkgbase.ServeStored(w, r, s.Store, s.zipRel(p.name, p.version), ctZip) {
			pkgbase.NotFound(w)
		}
	default:
		pkgbase.NotFound(w)
	}
}

func (l *Local) publish(w http.ResponseWriter, r *http.Request, name, ver string) {
	if !l.CheckPush(w) {
		return
	}
	specBody, zipBody, err := readPublish(r, l.MaxUpload)
	if err != nil {
		pkgbase.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	var spec map[string]any
	if err := json.Unmarshal(specBody, &spec); err != nil {
		pkgbase.Error(w, http.StatusBadRequest, "invalid podspec JSON: "+err.Error())
		return
	}
	if n, ok := spec["name"]; ok && n != name {
		pkgbase.Error(w, http.StatusBadRequest, fmt.Sprintf("podspec name %v does not match %q", n, name))
		return
	}
	if v, ok := spec["version"]; ok && v != ver {
		pkgbase.Error(w, http.StatusBadRequest, fmt.Sprintf("podspec version %v does not match %q", v, ver))
		return
	}
	spec["name"], spec["version"] = name, ver
	if zipBody != nil {
		if !bytes.HasPrefix(zipBody, []byte("PK")) {
			pkgbase.Error(w, http.StatusBadRequest, "file is not a zip archive")
			return
		}
		spec["source"] = map[string]any{"http": baseToken + "/files/" + name + "/" + ver + ".zip"}
	} else if _, ok := spec["source"]; !ok {
		pkgbase.Error(w, http.StatusBadRequest, "podspec has no source; upload a source zip as multipart \"file\"")
		return
	}
	out, err := pkgbase.MarshalJSON(spec)
	if err != nil {
		pkgbase.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	size := int64(len(out) + len(zipBody))
	if !l.AllowPublish(w, l.store.Exists(l.store.specRel(name, ver)), size) {
		return
	}
	if zipBody != nil {
		if err := l.store.Write(l.store.zipRel(name, ver), zipBody); err != nil {
			pkgbase.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
	} else if err := l.store.Delete(l.store.zipRel(name, ver)); err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := l.store.Write(l.store.specRel(name, ver), out); err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := l.store.rebuild(name); err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	l.EmitPublished(name+"@"+ver, size)
	pkgbase.WriteJSON(w, r, http.StatusCreated, map[string]any{"name": name, "version": ver, "path": SpecPath(name, ver)})
}

func readPublish(r *http.Request, max int64) (spec, zip []byte, err error) {
	if max <= 0 {
		max = pkgbase.DefaultMaxUpload
	}
	ct := r.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "multipart/form-data") {
		spec, err = pkgbase.ReadBody(r.Body, max)
		return spec, nil, err
	}
	r.Body = http.MaxBytesReader(nil, r.Body, max)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		return nil, nil, err
	}
	if vs := r.MultipartForm.Value["podspec"]; len(vs) > 0 {
		spec = []byte(vs[0])
	} else if f, _, ferr := r.FormFile("podspec"); ferr == nil {
		spec, err = pkgbase.ReadBody(f, max)
		f.Close()
		if err != nil {
			return nil, nil, err
		}
	} else {
		return nil, nil, errors.New("multipart field \"podspec\" is required")
	}
	if f, _, ferr := r.FormFile("file"); ferr == nil {
		zip, err = pkgbase.ReadBody(f, max)
		f.Close()
		if err != nil {
			return nil, nil, err
		}
	}
	return spec, zip, nil
}

// ── Remote ──

type Remote struct {
	*pkgbase.RemoteRepo
	store *Store
}

func NewRemoteFactory() registry.Factory {
	return func(_ context.Context, deps registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		if r.URL == "" {
			cp := *r
			cp.URL = DefaultUpstream
			r = &cp
		}
		base, err := pkgbase.NewRemoteRepo(deps, typ, ns, r, "/CocoaPods-version.yml")
		if err != nil {
			return nil, err
		}
		return &Remote{RemoteRepo: base, store: &Store{base.Store}}, nil
	}
}

func (rr *Remote) Store() *Store { return rr.store }

func (rr *Remote) ListPackages(ctx context.Context) ([]registry.PackageSummary, error) {
	return rr.store.ListPackages(ctx)
}

func (rr *Remote) Stats(ctx context.Context) (registry.Stats, error) {
	return pkgbase.StatsOf(ctx, rr, rr.store.Store), nil
}

func (rr *Remote) PackageDetail(_ context.Context, name string) (*registry.PackageDetail, error) {
	return detail(rr.store, name)
}

func (rr *Remote) PurgeCache(_ context.Context, opts registry.PurgeOptions) (registry.PurgeStats, error) {
	return rr.Purge(opts, metaDir), nil
}

func (rr *Remote) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) { return classify(r) }

func (rr *Remote) ArtifactInfo(_ context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	return artifactInfo(rr.store, ref, false)
}

func (rr *Remote) fetchSpec(ctx context.Context, name, ver string) ([]byte, error) {
	return rr.FetchCached(ctx, rr.store.specRel(name, ver), "/"+SpecPath(name, ver), false)
}

// Prefetch warms the shard of name and the podspec of version (or
// the newest version listed in the shard).
func (rr *Remote) Prefetch(ctx context.Context, name, version string) error {
	if !validSegment(name) {
		return registry.ErrInvalidPackageName
	}
	if version == "" {
		shard := ShardFile(name)
		body, err := rr.FetchCached(ctx, path.Join(metaDir, shard), "/"+shard, true)
		if err != nil {
			return err
		}
		version = pkgbase.Latest(shardVersions(body)[name])
		if version == "" {
			return registry.ErrPackageNotFound
		}
	}
	_, err := rr.fetchSpec(ctx, name, version)
	return err
}

func (rr *Remote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !pkgbase.IsRead(r) {
		pkgbase.Error(w, http.StatusMethodNotAllowed, "remote registry is read-only")
		return
	}
	p := parse(r.URL.Path)
	switch p.kind {
	case reqVersionYML:
		rr.ServeCached(w, r, path.Join(metaDir, "CocoaPods-version.yml"), "/CocoaPods-version.yml", ctYAML, true)
	case reqAllPods:
		rr.ServeCached(w, r, path.Join(metaDir, "all_pods.txt"), "/all_pods.txt", ctText, true)
	case reqDeprecated:
		rr.ServeCached(w, r, path.Join(metaDir, "deprecated_podspecs.txt"), "/deprecated_podspecs.txt", ctText, true)
	case reqShard:
		rr.ServeCached(w, r, path.Join(metaDir, p.shard), "/"+p.shard, ctText, true)
	case reqSpec:
		body, err := rr.fetchSpec(r.Context(), p.name, p.version)
		if err != nil {
			pkgbase.UpstreamError(w, err)
			return
		}
		// Sources hosted by an upstream kutu point back at it; route
		// them through this repo instead.
		up := strings.TrimRight(rr.Client.BaseURL(), "/") + "/files/"
		body = bytes.ReplaceAll(body, []byte(up), []byte(pkgbase.PublicBase(r)+"/files/"))
		pkgbase.WriteBytes(w, r, http.StatusOK, ctJSON, body)
	case reqFile:
		rel := rr.store.zipRel(p.name, p.version)
		if pkgbase.ServeStored(w, r, rr.store.Store, rel, ctZip) {
			return
		}
		rr.ServeCached(w, r, rel, "/"+strings.Trim(r.URL.Path, "/"), ctZip, false)
	default:
		pkgbase.NotFound(w)
	}
}

func shardVersions(body []byte) map[string][]string {
	out := map[string][]string{}
	for _, line := range strings.Split(string(body), "\n") {
		parts := strings.Split(strings.TrimSpace(line), "/")
		if len(parts) == 0 || parts[0] == "" {
			continue
		}
		out[parts[0]] = append(out[parts[0]], parts[1:]...)
	}
	return out
}

// ── Virtual ──

// Virtual merges the CDN index files across members and serves
// podspecs / archives first-hit.
type Virtual struct{ *pkgbase.Virtual }

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
	if !pkgbase.IsRead(r) {
		pkgbase.Error(w, http.StatusMethodNotAllowed, "virtual repositories are read-only; publish to a local member")
		return
	}
	switch p := parse(r.URL.Path); p.kind {
	case reqVersionYML:
		pkgbase.WriteBytes(w, r, http.StatusOK, ctYAML, []byte(versionYML))
	case reqAllPods, reqDeprecated:
		lines := v.CollectListLines(r)
		sort.Strings(lines)
		pkgbase.WriteBytes(w, r, http.StatusOK, ctText, joinLines(lines))
	case reqShard:
		pkgbase.WriteBytes(w, r, http.StatusOK, ctText, mergeShards(v.CollectMembers(r)))
	default:
		if !v.ServeFirstHit(w, r) {
			pkgbase.NotFound(w)
		}
	}
}

func joinLines(lines []string) []byte {
	if len(lines) == 0 {
		return nil
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

func mergeShards(bodies [][]byte) []byte {
	merged := map[string]map[string]struct{}{}
	for _, b := range bodies {
		for name, vs := range shardVersions(b) {
			set, ok := merged[name]
			if !ok {
				set = map[string]struct{}{}
				merged[name] = set
			}
			for _, v := range vs {
				if v != "" {
					set[v] = struct{}{}
				}
			}
		}
	}
	names := make([]string, 0, len(merged))
	for n := range merged {
		names = append(names, n)
	}
	sort.Strings(names)
	lines := make([]string, 0, len(names))
	for _, n := range names {
		vs := make([]string, 0, len(merged[n]))
		for v := range merged[n] {
			vs = append(vs, v)
		}
		pkgbase.SortVersions(vs)
		lines = append(lines, strings.Join(append([]string{n}, vs...), "/"))
	}
	return joinLines(lines)
}
