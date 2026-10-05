// Package rpm implements a YUM/DNF (rpm-md) repository:
//
//	PUT    /upload                          upload an .rpm (local, allow_push; also PUT /upload/{file}.rpm, POST /)
//	GET    /repodata/repomd.xml             repository index
//	GET    /repodata/repomd.xml.asc         armored detached signature (when a signing key is set)
//	GET    /repodata/repomd.xml.key         public key (also /RPM-GPG-KEY)
//	GET    /repodata/{sha256}-{primary|filelists|other}.xml.gz
//	GET    /Packages/{n}/{name}-{version}-{release}.{arch}.rpm
//	DELETE /Packages/{n}/{name}-{version}-{release}.{arch}.rpm
//	GET    /config.repo                     ready-to-use .repo file (also /{id}.repo)
//
// Local repos parse every uploaded RPM header, keep a JSON record per
// package under meta/ and regenerate createrepo_c-compatible repodata
// (sha256 checksums) after each publish/delete. When the repository has
// a SigningKey, repomd.xml is signed (repo_gpgcheck=1); packages keep
// whatever signature they were built with.
//
// Remote repos proxy an rpm-md mirror (e.g. https://dl.fedoraproject.org/
// pub/fedora/linux/releases/40/Everything/x86_64/os): repomd.xml and its
// signature/key are TTL-cached, checksum-named repodata and packages are
// cached forever. Virtual repos merge primary/filelists/other across
// members and serve an UNSIGNED repomd.xml (clients must use
// repo_gpgcheck=0); packages are served first-hit.
//
// Version strings used by the admin API (listing, delete, promote) are
// "version-release"; DeleteVersion also accepts "epoch:version-release".
//
// Client configuration:
//
//	sudo curl -o /etc/yum.repos.d/kutu.repo -u x:<token> https://kutu.example.com/registries/{ns}/{repo}/config.repo
//	# then replace the password=<token> placeholder in the file
//	curl -u x:<token> -T pkg-1.0-1.x86_64.rpm https://kutu.example.com/registries/{ns}/{repo}/upload
package rpm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/rakunlabs/kutu/internal/registry/signing"
	"github.com/rakunlabs/kutu/internal/service"
)

const typ = service.RegistryTypeRPM

const (
	packagesDir = "Packages"
	metaDir     = "meta"
	repodataDir = "repodata"
	repomdRel   = "repodata/repomd.xml"
	repomdAsc   = "repodata/repomd.xml.asc"
	repomdKey   = "repodata/repomd.xml.key"
)

// Store wraps pkgbase.Store with the rpm layout.
type Store struct{ *pkgbase.Store }

func packageRel(p *Package) string {
	n := strings.ToLower(p.Name[:1])
	return path.Join(packagesDir, n, p.FileName())
}

func metaRel(name, file string) string {
	return path.Join(metaDir, name, file+".json")
}

// LoadAll returns every package record, sorted by name then EVR.
func (s *Store) LoadAll() ([]*Package, error) {
	var out []*Package
	err := s.Walk(metaDir, func(rel string, _ rawfs.DirEntry) error {
		if !strings.HasSuffix(rel, ".json") {
			return nil
		}
		p, err := s.loadMeta(rel)
		if err != nil {
			return nil
		}
		out = append(out, p)
		return nil
	})
	sortPackages(out)
	return out, err
}

// LoadName returns the package records of name.
func (s *Store) LoadName(name string) ([]*Package, error) {
	if !validName(name) {
		return nil, registry.ErrInvalidPackageName
	}
	files, err := s.ListFiles(path.Join(metaDir, name))
	if err != nil {
		return nil, err
	}
	var out []*Package
	for _, f := range files {
		if p, err := s.loadMeta(path.Join(metaDir, name, f.Name)); err == nil {
			out = append(out, p)
		}
	}
	sortPackages(out)
	return out, nil
}

func (s *Store) loadMeta(rel string) (*Package, error) {
	b, err := s.Read(rel)
	if err != nil {
		return nil, err
	}
	var p Package
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Store) saveMeta(p *Package) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return s.Write(metaRel(p.Name, p.FileName()), b)
}

// ListPackages lists names with their "version-release" strings.
func (s *Store) ListPackages(context.Context) ([]registry.PackageSummary, error) {
	names, err := s.ListDirs(metaDir)
	if err != nil {
		return nil, err
	}
	var out []registry.PackageSummary
	for _, name := range names {
		files, _ := s.ListFiles(path.Join(metaDir, name))
		set := map[string]struct{}{}
		for _, f := range files {
			if _, vr, _, ok := parseFileName(strings.TrimSuffix(f.Name, ".json")); ok {
				set[vr] = struct{}{}
			}
		}
		if len(set) == 0 {
			continue
		}
		out = append(out, registry.PackageSummary{Name: name, Versions: setToSorted(set)})
	}
	return out, nil
}

func setToSorted(set map[string]struct{}) []string {
	vs := make([]string, 0, len(set))
	for v := range set {
		vs = append(vs, v)
	}
	pkgbase.SortVersions(vs)
	return vs
}

func sortPackages(ps []*Package) {
	sort.SliceStable(ps, func(i, j int) bool {
		if ps[i].Name != ps[j].Name {
			return ps[i].Name < ps[j].Name
		}
		if c := pkgbase.CompareVersions(ps[i].EVR(), ps[j].EVR()); c != 0 {
			return c < 0
		}
		return ps[i].Arch < ps[j].Arch
	})
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9._+\-]+$`)

func validName(n string) bool { return n != "" && n != "." && n != ".." && nameRe.MatchString(n) }

func matchesVersion(p *Package, version string) bool {
	return version == p.VR() || version == p.EVR() || version == p.Epoch+":"+p.VR()
}

func detail(pkgs []*Package, name string) (*registry.PackageDetail, error) {
	if len(pkgs) == 0 {
		return nil, registry.ErrPackageNotFound
	}
	byVR := map[string][]*Package{}
	var vrs []string
	for _, p := range pkgs {
		if _, ok := byVR[p.VR()]; !ok {
			vrs = append(vrs, p.VR())
		}
		byVR[p.VR()] = append(byVR[p.VR()], p)
	}
	pkgbase.SortVersions(vrs)
	latest := byVR[vrs[len(vrs)-1]][0]
	d := &registry.GenericPackageDetail{
		LatestVersion: latest.VR(), Description: latest.Summary, Homepage: latest.URL, License: latest.License,
	}
	if latest.Group != "" {
		d.Metadata = map[string]string{"group": latest.Group}
	}
	for i := len(vrs) - 1; i >= 0; i-- {
		row := registry.GenericVersionDetail{Version: vrs[i], Metadata: map[string]string{}}
		var arches []string
		var newest int64
		for _, p := range byVR[vrs[i]] {
			row.Size += p.PackageSize
			row.Files = append(row.Files, registry.GenericFile{Name: p.FileName(), Size: p.PackageSize, SHA256: p.SHA256})
			arches = append(arches, p.Arch)
			if p.Epoch != "" && p.Epoch != "0" {
				row.Metadata["epoch"] = p.Epoch
			}
			if p.FileTime > newest {
				newest = p.FileTime
			}
			if p.License != "" {
				row.Metadata["license"] = p.License
			}
		}
		row.Metadata["arch"] = strings.Join(arches, ",")
		if newest > 0 {
			row.PublishedAt = time.Unix(newest, 0).UTC().Format(time.RFC3339)
		}
		d.Versions = append(d.Versions, row)
	}
	return &registry.PackageDetail{Type: typ, Name: name, Generic: d}, nil
}

// classify maps package downloads to {name, version-release}.
func classify(r *http.Request) (registry.ArtifactRef, bool) {
	p := strings.Trim(r.URL.Path, "/")
	if !strings.HasSuffix(p, ".rpm") {
		return registry.ArtifactRef{}, false
	}
	name, vr, _, ok := parseFileName(path.Base(p))
	if !ok {
		return registry.ArtifactRef{}, false
	}
	return registry.ArtifactRef{Name: name, Version: vr}, true
}

// repoFile renders a .repo file for the request's public base.
func repoFile(r *http.Request, ns, repo string, signed bool) []byte {
	base := pkgbase.PublicBase(r)
	id := strings.TrimSuffix(path.Base(r.URL.Path), ".repo")
	if id == "" || id == "config" || id == "." || id == "/" {
		id = "kutu-" + ns + "-" + repo
	}
	if q := r.URL.Query().Get("id"); q != "" {
		id = q
	}
	id = regexp.MustCompile(`[^A-Za-z0-9._:\-]`).ReplaceAllString(id, "-")
	var b strings.Builder
	fmt.Fprintf(&b, "[%s]\n", id)
	fmt.Fprintf(&b, "name=kutu %s/%s\n", ns, repo)
	fmt.Fprintf(&b, "baseurl=%s\n", base)
	b.WriteString("enabled=1\n")
	b.WriteString("gpgcheck=0\n")
	if signed {
		b.WriteString("repo_gpgcheck=1\n")
		fmt.Fprintf(&b, "gpgkey=%s/repodata/repomd.xml.key\n", base)
	} else {
		b.WriteString("repo_gpgcheck=0\n")
	}
	b.WriteString("username=kutu\n")
	b.WriteString("password=<token>\n")
	return []byte(b.String())
}

func isRepoFile(p string) bool {
	p = strings.Trim(p, "/")
	return strings.HasSuffix(p, ".repo") && !strings.Contains(p, "/")
}

func isKeyPath(p string) bool {
	p = strings.Trim(p, "/")
	return p == repomdKey || p == "RPM-GPG-KEY"
}

// ── Local ──

// Local is a hosted rpm repository.
type Local struct {
	*pkgbase.Repo
	store  *Store
	key    *signing.PGPKey
	pubKey []byte
	mu     sync.Mutex
}

func NewLocalFactory() registry.Factory {
	return func(_ context.Context, deps registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		base, err := pkgbase.NewLocalRepo(deps, typ, ns, r)
		if err != nil {
			return nil, err
		}
		l := &Local{Repo: base, store: &Store{base.Store}}
		if strings.TrimSpace(r.SigningKey) != "" {
			k, err := signing.ParsePGPKey(r.SigningKey)
			if err != nil {
				return nil, fmt.Errorf("rpm/local %s/%s: %w", ns, r.Name, err)
			}
			pub, err := k.PublicKeyArmored()
			if err != nil {
				return nil, fmt.Errorf("rpm/local %s/%s: %w", ns, r.Name, err)
			}
			l.key, l.pubKey = k, pub
		}
		return l, nil
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
	pkgs, err := l.store.LoadName(name)
	if err != nil {
		return nil, err
	}
	return detail(pkgs, name)
}

func (l *Local) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) {
	return classify(r)
}

func (l *Local) ArtifactInfo(_ context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	pkgs, err := l.store.LoadName(ref.Name)
	if err != nil {
		return registry.ArtifactMeta{}, err
	}
	for _, p := range pkgs {
		if matchesVersion(p, ref.Version) {
			m := registry.ArtifactMeta{License: p.License}
			if p.BuildTime > 0 {
				m.PublishedAt = time.Unix(p.BuildTime, 0).UTC()
			} else if p.FileTime > 0 {
				m.PublishedAt = time.Unix(p.FileTime, 0).UTC()
			}
			return m, nil
		}
	}
	return registry.ArtifactMeta{}, registry.ErrPackageNotFound
}

// DeleteVersion removes every arch of name at version ("version-release"
// or "epoch:version-release") and regenerates repodata.
func (l *Local) DeleteVersion(_ context.Context, name, version string) error {
	pkgs, err := l.store.LoadName(name)
	if err != nil {
		return err
	}
	var hit []*Package
	for _, p := range pkgs {
		if matchesVersion(p, version) {
			hit = append(hit, p)
		}
	}
	if len(hit) == 0 {
		return registry.ErrPackageNotFound
	}
	for _, p := range hit {
		if err := l.deletePackage(p); err != nil {
			return err
		}
	}
	if err := l.Regenerate(); err != nil {
		return err
	}
	l.EmitDeleted(name + "@" + version)
	return nil
}

func (l *Local) deletePackage(p *Package) error {
	if err := l.store.Delete(p.Location); err != nil {
		return err
	}
	return l.store.Delete(metaRel(p.Name, p.FileName()))
}

// PromoteVersion copies name@version (every arch) into dst.
func (l *Local) PromoteVersion(_ context.Context, dst registry.Registry, name, version string) error {
	d, ok := dst.(*Local)
	if !ok {
		return fmt.Errorf("rpm: promote target %s/%s is not a local rpm repository", dst.Namespace(), dst.Name())
	}
	pkgs, err := l.store.LoadName(name)
	if err != nil {
		return err
	}
	n := 0
	for _, p := range pkgs {
		if !matchesVersion(p, version) {
			continue
		}
		body, err := l.store.Read(p.Location)
		if err != nil {
			return err
		}
		if code, err := d.Guard.Check(d.store.Store, d.store.Exists(p.Location), int64(len(body))); err != nil {
			return fmt.Errorf("rpm: promote rejected (%d): %w", code, err)
		}
		if err := d.store.Write(p.Location, body); err != nil {
			return err
		}
		if err := d.store.saveMeta(p); err != nil {
			return err
		}
		n++
	}
	if n == 0 {
		return registry.ErrPackageNotFound
	}
	if err := d.Regenerate(); err != nil {
		return err
	}
	d.EmitPublished(name+"@"+version, 0)
	return nil
}

// Regenerate rebuilds repodata from the package records.
func (l *Local) Regenerate() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.regenerateLocked()
}

func (l *Local) regenerateLocked() error {
	pkgs, err := l.store.LoadAll()
	if err != nil {
		return err
	}
	rd, err := generate(pkgs, time.Now())
	if err != nil {
		return err
	}
	for rel, b := range rd.Files {
		if err := l.store.Write(rel, b); err != nil {
			return err
		}
	}
	if err := l.store.Write(repomdRel, rd.Repomd); err != nil {
		return err
	}
	keep := map[string]bool{repomdRel: true}
	for rel := range rd.Files {
		keep[rel] = true
	}
	if l.key != nil {
		sig, err := l.key.DetachSignArmored(rd.Repomd)
		if err != nil {
			return fmt.Errorf("rpm: sign repomd.xml: %w", err)
		}
		if err := l.store.Write(repomdAsc, sig); err != nil {
			return err
		}
		if err := l.store.Write(repomdKey, l.pubKey); err != nil {
			return err
		}
		keep[repomdAsc], keep[repomdKey] = true, true
	}
	files, err := l.store.ListFiles(repodataDir)
	if err != nil {
		return err
	}
	for _, f := range files {
		rel := path.Join(repodataDir, f.Name)
		if !keep[rel] {
			_ = l.store.Delete(rel)
		}
	}
	return nil
}

func (l *Local) ensureRepodata() {
	if l.store.Exists(repomdRel) {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.store.Exists(repomdRel) {
		_ = l.regenerateLocked()
	}
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

func (l *Local) serveRead(w http.ResponseWriter, r *http.Request) {
	p := strings.Trim(r.URL.Path, "/")
	switch {
	case p == "":
		pkgs, err := l.store.ListPackages(r.Context())
		if err != nil {
			pkgbase.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{"packages": pkgs})
		return
	case isRepoFile(p):
		pkgbase.WriteBytes(w, r, http.StatusOK, "text/plain; charset=utf-8", repoFile(r, l.NS, l.RepoName, l.key != nil))
		return
	case isKeyPath(p):
		if l.pubKey == nil {
			pkgbase.NotFound(w)
			return
		}
		pkgbase.WriteBytes(w, r, http.StatusOK, "application/pgp-keys", l.pubKey)
		return
	case p == repomdAsc && l.key == nil:
		pkgbase.NotFound(w)
		return
	case strings.HasPrefix(p, repodataDir+"/"):
		l.ensureRepodata()
		if pkgbase.ServeStored(w, r, l.store.Store, p, contentType(p)) {
			return
		}
	case strings.HasPrefix(p, packagesDir+"/") && strings.HasSuffix(p, ".rpm"):
		if pkgbase.ServeStored(w, r, l.store.Store, p, "application/x-rpm") {
			return
		}
	}
	pkgbase.NotFound(w)
}

func contentType(p string) string {
	switch {
	case strings.HasSuffix(p, ".xml"):
		return "application/xml"
	case strings.HasSuffix(p, ".gz"):
		return "application/gzip"
	case strings.HasSuffix(p, ".asc"), strings.HasSuffix(p, ".key"):
		return "application/pgp-signature"
	case strings.HasSuffix(p, ".rpm"):
		return "application/x-rpm"
	}
	return "application/octet-stream"
}

func (l *Local) upload(w http.ResponseWriter, r *http.Request) {
	if !l.CheckPush(w) {
		return
	}
	p := strings.Trim(r.URL.Path, "/")
	if p != "" && p != "upload" && !(strings.HasPrefix(p, "upload/") && strings.HasSuffix(p, ".rpm")) {
		pkgbase.Error(w, http.StatusBadRequest, "expected PUT /upload with an .rpm body")
		return
	}
	body, err := pkgbase.ReadBody(r.Body, l.MaxUpload)
	if err != nil {
		pkgbase.Error(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	pkg, err := ParseRPM(body)
	if err != nil {
		pkgbase.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if !validName(pkg.Name) {
		pkgbase.Error(w, http.StatusBadRequest, "invalid rpm package name")
		return
	}
	pkg.SHA256 = pkgbase.SHA256Hex(body)
	pkg.PackageSize = int64(len(body))
	pkg.FileTime = time.Now().Unix()
	pkg.Location = packageRel(pkg)
	if !l.AllowPublish(w, l.store.Exists(pkg.Location), pkg.PackageSize) {
		return
	}
	if err := l.store.Write(pkg.Location, body); err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := l.store.saveMeta(pkg); err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := l.Regenerate(); err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	l.EmitPublished(pkg.Name+"@"+pkg.VR()+"/"+pkg.FileName(), pkg.PackageSize)
	pkgbase.WriteJSON(w, r, http.StatusCreated, map[string]any{
		"name": pkg.Name, "epoch": pkg.Epoch, "version": pkg.Version, "release": pkg.Release,
		"arch": pkg.Arch, "location": pkg.Location, "size": pkg.PackageSize, "sha256": pkg.SHA256,
	})
}

func (l *Local) remove(w http.ResponseWriter, r *http.Request) {
	if !l.CheckPush(w) {
		return
	}
	p := strings.Trim(r.URL.Path, "/")
	if !strings.HasPrefix(p, packagesDir+"/") || !strings.HasSuffix(p, ".rpm") {
		pkgbase.NotFound(w)
		return
	}
	var hit *Package
	if name, _, _, ok := parseFileName(path.Base(p)); ok {
		if pkgs, err := l.store.LoadName(name); err == nil {
			for _, pk := range pkgs {
				if pk.Location == p {
					hit = pk
				}
			}
		}
	}
	if hit == nil {
		pkgbase.NotFound(w)
		return
	}
	if err := l.deletePackage(hit); err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := l.Regenerate(); err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	l.EmitDeleted(hit.Name + "@" + hit.VR() + "/" + hit.FileName())
	w.WriteHeader(http.StatusNoContent)
}

var errNoPrimary = errors.New("rpm: upstream repomd.xml has no primary data")
