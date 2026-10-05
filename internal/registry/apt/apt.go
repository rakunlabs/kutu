// Package apt implements a Debian/Ubuntu APT repository.
//
// Local repositories:
//
//	PUT    /upload/{distribution}/{component}[/{file}.deb]  upload a .deb (raw body or multipart "file")
//	PUT    /upload                                          same, distribution=stable component=main
//	POST   /api/upload?distribution=&component=             same
//	GET    /dists/{dist}/Release | InRelease | Release.gpg
//	GET    /dists/{dist}/{component}/binary-{arch}/Packages[.gz]
//	GET    /pool/{component}/{p}/{package}/{package}_{version}_{arch}.deb
//	GET    /public.key                                      armored signing key (also /public.gpg binary)
//	DELETE /pool/...deb                                     remove the file from every distribution
//
// Every upload/delete regenerates the affected distribution's
// Packages, Release and — when the repository has a SigningKey —
// InRelease (clearsigned) and Release.gpg (detached). Architecture
// "all" packages appear in every binary-{arch} index; amd64 and arm64
// indexes are always published so hosts of those architectures never
// see "doesn't support architecture" warnings.
//
// Control archives may be gzip, xz, zstd or uncompressed.
//
// Remote repositories proxy a Debian mirror: dists/ is TTL-cached
// (upstream signatures are served byte-identical), pool/ and by-hash
// files are cached forever.
//
// Virtual repositories serve pool/ first-hit and merge every member's
// Packages into a freshly generated, UNSIGNED Release (no InRelease /
// Release.gpg); clients must use [trusted=yes].
//
// Client configuration:
//
//	curl -fsSL {base}/public.key | sudo gpg --dearmor -o /etc/apt/keyrings/kutu.gpg
//	echo "deb [signed-by=/etc/apt/keyrings/kutu.gpg] {base} stable main" | sudo tee /etc/apt/sources.list.d/kutu.list
//	printf 'machine {host}\nlogin x\npassword <token>\n' | sudo tee /etc/apt/auth.conf.d/kutu.conf
//	curl -u x:<token> -T pkg.deb {base}/upload/stable/main
package apt

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
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
	"github.com/rakunlabs/kutu/internal/service"
)

const typ = service.RegistryTypeAPT

const (
	defaultDist = "stable"
	defaultComp = "main"
	indexDir    = "index"
	distsDir    = "dists"
	poolDir     = "pool"
	origin      = "kutu"
)

var defaultArches = []string{"amd64", "arm64"}

// Store wraps pkgbase.Store with the apt layout.
type Store struct{ *pkgbase.Store }

// entry is one published .deb in one distribution/component.
type entry struct {
	Control   stanza    `json:"control"`
	Filename  string    `json:"filename"`
	Size      int64     `json:"size"`
	MD5       string    `json:"md5"`
	SHA1      string    `json:"sha1"`
	SHA256    string    `json:"sha256"`
	Published time.Time `json:"published"`
}

func (e entry) name() string    { return e.Control.Get("Package") }
func (e entry) version() string { return e.Control.Get("Version") }
func (e entry) arch() string    { return e.Control.Get("Architecture") }

func (e entry) packagesStanza() stanza {
	return append(append(stanza{}, e.Control...),
		field{K: "Filename", V: e.Filename},
		field{K: "Size", V: fmt.Sprint(e.Size)},
		field{K: "MD5sum", V: e.MD5},
		field{K: "SHA1", V: e.SHA1},
		field{K: "SHA256", V: e.SHA256},
	)
}

func newEntry(ctl stanza, comp string, body []byte, published time.Time) entry {
	m := md5.Sum(body)
	s1 := sha1.Sum(body)
	return entry{
		Control:   ctl,
		Filename:  poolPath(comp, ctl.Get("Package"), ctl.Get("Version"), ctl.Get("Architecture")),
		Size:      int64(len(body)),
		MD5:       hex.EncodeToString(m[:]),
		SHA1:      hex.EncodeToString(s1[:]),
		SHA256:    pkgbase.SHA256Hex(body),
		Published: published.UTC(),
	}
}

type indexDoc struct {
	Packages []entry `json:"packages"`
}

// idxRef is the loaded index of one distribution/component.
type idxRef struct {
	dist, comp string
	entries    []entry
}

func indexRel(dist, comp string) string { return indexDir + "/" + dist + "/" + comp + ".json" }

func (s *Store) loadIndex(dist, comp string) ([]entry, error) {
	b, err := s.Read(indexRel(dist, comp))
	if err != nil {
		if pkgbase.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var doc indexDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("apt index %s/%s: %w", dist, comp, err)
	}
	return doc.Packages, nil
}

func (s *Store) saveIndex(dist, comp string, es []entry) error {
	if len(es) == 0 {
		return s.Delete(indexRel(dist, comp))
	}
	sort.SliceStable(es, func(i, j int) bool {
		a, b := es[i], es[j]
		if a.name() != b.name() {
			return a.name() < b.name()
		}
		if c := pkgbase.CompareVersions(a.version(), b.version()); c != 0 {
			return c < 0
		}
		return a.arch() < b.arch()
	})
	b, err := pkgbase.MarshalJSON(indexDoc{Packages: es})
	if err != nil {
		return err
	}
	return s.Write(indexRel(dist, comp), b)
}

func (s *Store) distributions() ([]string, error) { return s.ListDirs(indexDir) }

func (s *Store) components(dist string) ([]string, error) {
	files, err := s.ListFiles(indexDir + "/" + dist)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range files {
		if strings.HasSuffix(f.Name, ".json") {
			out = append(out, strings.TrimSuffix(f.Name, ".json"))
		}
	}
	return out, nil
}

func (s *Store) allIndexes() ([]idxRef, error) {
	dists, err := s.distributions()
	if err != nil {
		return nil, err
	}
	var out []idxRef
	for _, d := range dists {
		comps, err := s.components(d)
		if err != nil {
			return nil, err
		}
		for _, c := range comps {
			es, err := s.loadIndex(d, c)
			if err != nil {
				return nil, err
			}
			out = append(out, idxRef{dist: d, comp: c, entries: es})
		}
	}
	return out, nil
}

// indexFile is one generated file below dists/{dist}/.
type indexFile struct {
	path string
	data []byte
}

func gzipBytes(b []byte) []byte {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	_, _ = zw.Write(b)
	_ = zw.Close()
	return buf.Bytes()
}

func hexOf(h interface {
	io.Writer
	Sum([]byte) []byte
}, b []byte) string {
	_, _ = h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// buildRelease renders a Release document listing files.
func buildRelease(label, dist string, arches, comps []string, files []indexFile, date time.Time) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "Origin: %s\nLabel: %s\nSuite: %s\nCodename: %s\n", origin, label, dist, dist)
	fmt.Fprintf(&b, "Date: %s\n", date.UTC().Format("Mon, 02 Jan 2006 15:04:05 UTC"))
	fmt.Fprintf(&b, "Architectures: %s\nComponents: %s\n", strings.Join(arches, " "), strings.Join(comps, " "))
	sums := []struct {
		name string
		fn   func([]byte) string
	}{
		{"MD5Sum", func(d []byte) string { return hexOf(md5.New(), d) }},
		{"SHA1", func(d []byte) string { return hexOf(sha1.New(), d) }},
		{"SHA256", func(d []byte) string { return hexOf(sha256.New(), d) }},
	}
	for _, s := range sums {
		b.WriteString(s.name + ":\n")
		for _, f := range files {
			fmt.Fprintf(&b, " %s %8d %s\n", s.fn(f.data), len(f.data), f.path)
		}
	}
	return b.Bytes()
}

// packagesFiles renders Packages + Packages.gz for every comp × arch.
func packagesFiles(comps, arches []string, render func(comp, arch string, buf *bytes.Buffer)) []indexFile {
	var files []indexFile
	for _, c := range comps {
		for _, a := range arches {
			var buf bytes.Buffer
			render(c, a, &buf)
			raw := buf.Bytes()
			rel := c + "/binary-" + a + "/Packages"
			files = append(files, indexFile{rel, raw}, indexFile{rel + ".gz", gzipBytes(raw)})
		}
	}
	return files
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// regenerate rebuilds dists/{dist}/ from the stored index.
func (s *Store) regenerate(dist, label string, key *signing.PGPKey, now time.Time) error {
	root := distsDir + "/" + dist
	comps, err := s.components(dist)
	if err != nil {
		return err
	}
	byComp := map[string][]entry{}
	archSet := map[string]bool{}
	for _, a := range defaultArches {
		archSet[a] = true
	}
	var used []string
	for _, c := range comps {
		es, err := s.loadIndex(dist, c)
		if err != nil {
			return err
		}
		if len(es) == 0 {
			continue
		}
		byComp[c] = es
		used = append(used, c)
		for _, e := range es {
			if a := e.arch(); a != "all" {
				archSet[a] = true
			}
		}
	}
	if len(used) == 0 {
		_, _, errs := s.DeleteTree(root)
		return errors.Join(errs...)
	}
	arches := sortedKeys(archSet)
	files := packagesFiles(used, arches, func(c, a string, buf *bytes.Buffer) {
		for _, e := range byComp[c] {
			if ea := e.arch(); ea != a && ea != "all" {
				continue
			}
			e.packagesStanza().render(buf)
			buf.WriteByte('\n')
		}
	})
	release := buildRelease(label, dist, arches, used, files, now)
	keep := map[string]bool{"Release": true}
	for _, f := range files {
		keep[f.path] = true
		if err := s.Write(root+"/"+f.path, f.data); err != nil {
			return err
		}
	}
	var inRelease []byte
	if key != nil {
		sig, err := key.DetachSignArmored(release)
		if err != nil {
			return fmt.Errorf("sign Release: %w", err)
		}
		if inRelease, err = key.ClearSign(release); err != nil {
			return fmt.Errorf("sign InRelease: %w", err)
		}
		if err := s.Write(root+"/Release.gpg", sig); err != nil {
			return err
		}
		keep["Release.gpg"], keep["InRelease"] = true, true
	}
	if err := s.Write(root+"/Release", release); err != nil {
		return err
	}
	if inRelease != nil {
		if err := s.Write(root+"/InRelease", inRelease); err != nil {
			return err
		}
	}
	return s.Walk(root, func(rel string, _ rawfs.DirEntry) error {
		if !keep[strings.TrimPrefix(rel, root+"/")] {
			return s.Delete(rel)
		}
		return nil
	})
}

// poolScan groups cached/stored pool files by package and version.
func (s *Store) poolScan() (map[string]map[string][]registry.GenericFile, error) {
	out := map[string]map[string][]registry.GenericFile{}
	err := s.Walk(poolDir, func(rel string, e rawfs.DirEntry) error {
		name, ver, _, ok := parseDebFilename(rel)
		if !ok {
			return nil
		}
		if out[name] == nil {
			out[name] = map[string][]registry.GenericFile{}
		}
		out[name][ver] = append(out[name][ver], registry.GenericFile{Name: e.Name, Size: e.Size})
		return nil
	})
	return out, err
}

func summaries(m map[string]map[string]bool) []registry.PackageSummary {
	out := make([]registry.PackageSummary, 0, len(m))
	for name, set := range m {
		out = append(out, registry.PackageSummary{Name: name, Versions: sortedVersions(set)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func sortedVersions(set map[string]bool) []string {
	vs := sortedKeys(set)
	pkgbase.SortVersions(vs)
	return vs
}

func contentType(p string) string {
	switch {
	case strings.HasSuffix(p, ".deb"), strings.HasSuffix(p, ".udeb"):
		return "application/vnd.debian.binary-package"
	case strings.HasSuffix(p, ".gz"):
		return "application/gzip"
	case strings.HasSuffix(p, ".xz"):
		return "application/x-xz"
	case strings.HasSuffix(p, ".zst"):
		return "application/zstd"
	case strings.HasSuffix(p, ".gpg"):
		return "application/pgp-signature"
	case strings.HasSuffix(p, ".key"):
		return "application/pgp-keys"
	}
	return "text/plain; charset=utf-8"
}

func classify(r *http.Request) (registry.ArtifactRef, bool) {
	p := strings.Trim(r.URL.Path, "/")
	if !strings.HasPrefix(p, poolDir+"/") {
		return registry.ArtifactRef{}, false
	}
	name, ver, _, ok := parseDebFilename(p)
	if !ok {
		return registry.ArtifactRef{}, false
	}
	return registry.ArtifactRef{Name: name, Version: ver}, true
}

var repoLocks sync.Map

func lockFor(key string) *sync.Mutex {
	m, _ := repoLocks.LoadOrStore(key, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// ── Local ──

type Local struct {
	*pkgbase.Repo
	store   *Store
	key     *signing.PGPKey
	keyName string
	mu      *sync.Mutex
}

func NewLocalFactory() registry.Factory {
	return func(_ context.Context, deps registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		base, err := pkgbase.NewLocalRepo(deps, typ, ns, r)
		if err != nil {
			return nil, err
		}
		l := &Local{Repo: base, store: &Store{base.Store}, keyName: strings.Trim(r.SigningKeyName, "/"), mu: lockFor(ns + "/" + r.Name)}
		if strings.TrimSpace(r.SigningKey) != "" {
			if l.key, err = signing.ParsePGPKey(r.SigningKey); err != nil {
				return nil, fmt.Errorf("apt/local %s/%s: %w", ns, r.Name, err)
			}
		}
		return l, nil
	}
}

func (l *Local) Close() error  { return nil }
func (l *Local) Store() *Store { return l.store }

func (l *Local) ListPackages(context.Context) ([]registry.PackageSummary, error) {
	refs, err := l.store.allIndexes()
	if err != nil {
		return nil, err
	}
	m := map[string]map[string]bool{}
	for _, ref := range refs {
		for _, e := range ref.entries {
			if m[e.name()] == nil {
				m[e.name()] = map[string]bool{}
			}
			m[e.name()][e.version()] = true
		}
	}
	return summaries(m), nil
}

func (l *Local) Stats(ctx context.Context) (registry.Stats, error) {
	return pkgbase.StatsOf(ctx, l, l.store.Store), nil
}

func (l *Local) PackageDetail(_ context.Context, name string) (*registry.PackageDetail, error) {
	refs, err := l.store.allIndexes()
	if err != nil {
		return nil, err
	}
	type agg struct {
		row    registry.GenericVersionDetail
		files  map[string]bool
		dists  map[string]bool
		arches map[string]bool
		first  time.Time
		latest entry
	}
	byVer := map[string]*agg{}
	for _, ref := range refs {
		for _, e := range ref.entries {
			if e.name() != name {
				continue
			}
			a := byVer[e.version()]
			if a == nil {
				a = &agg{row: registry.GenericVersionDetail{Version: e.version()}, files: map[string]bool{}, dists: map[string]bool{}, arches: map[string]bool{}, latest: e}
				byVer[e.version()] = a
			}
			if !a.files[e.Filename] {
				a.files[e.Filename] = true
				a.row.Files = append(a.row.Files, registry.GenericFile{Name: path.Base(e.Filename), Size: e.Size, SHA256: e.SHA256})
				a.row.Size += e.Size
			}
			a.dists[ref.dist+"/"+ref.comp] = true
			a.arches[e.arch()] = true
			if a.first.IsZero() || e.Published.Before(a.first) {
				a.first = e.Published
			}
		}
	}
	if len(byVer) == 0 {
		return nil, registry.ErrPackageNotFound
	}
	vs := make([]string, 0, len(byVer))
	for v := range byVer {
		vs = append(vs, v)
	}
	pkgbase.SortVersions(vs)
	latest := byVer[vs[len(vs)-1]].latest
	d := &registry.GenericPackageDetail{
		LatestVersion: latest.version(),
		Description:   firstLine(latest.Control.Get("Description")),
		Homepage:      latest.Control.Get("Homepage"),
		Metadata:      map[string]string{},
	}
	for _, k := range []string{"Maintainer", "Section", "Priority", "Depends"} {
		if v := latest.Control.Get(k); v != "" {
			d.Metadata[strings.ToLower(k)] = firstLine(v)
		}
	}
	for i := len(vs) - 1; i >= 0; i-- {
		a := byVer[vs[i]]
		a.row.Metadata = map[string]string{
			"distributions": strings.Join(sortedKeys(a.dists), ", "),
			"architectures": strings.Join(sortedKeys(a.arches), ", "),
		}
		if !a.first.IsZero() {
			a.row.PublishedAt = a.first.UTC().Format(time.RFC3339)
		}
		d.Versions = append(d.Versions, a.row)
	}
	return &registry.PackageDetail{Type: typ, Name: name, Generic: d}, nil
}

func (l *Local) ArtifactInfo(_ context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	refs, err := l.store.allIndexes()
	if err != nil {
		return registry.ArtifactMeta{}, err
	}
	var meta registry.ArtifactMeta
	found := false
	for _, ix := range refs {
		for _, e := range ix.entries {
			if e.name() != ref.Name || (e.version() != ref.Version && stripEpoch(e.version()) != ref.Version) {
				continue
			}
			found = true
			if meta.PublishedAt.IsZero() || e.Published.Before(meta.PublishedAt) {
				meta.PublishedAt = e.Published
			}
		}
	}
	if !found {
		return meta, registry.ErrPackageNotFound
	}
	return meta, nil
}

func (l *Local) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) { return classify(r) }

func (l *Local) DeleteVersion(_ context.Context, name, version string) error {
	removed, err := l.removeWhere(func(e entry) bool {
		return e.name() == name && (e.version() == version || stripEpoch(e.version()) == version)
	})
	if err != nil {
		return err
	}
	if len(removed) == 0 {
		return registry.ErrPackageNotFound
	}
	l.EmitDeleted(name + "@" + version)
	return nil
}

// PromoteVersion copies name@version (every dist/component/arch) into
// dst, a local apt registry, and regenerates its indexes.
func (l *Local) PromoteVersion(_ context.Context, dst registry.Registry, name, version string) error {
	d, ok := dst.(*Local)
	if !ok {
		return fmt.Errorf("apt promote: destination %T is not a local apt registry", dst)
	}
	refs, err := l.store.allIndexes()
	if err != nil {
		return err
	}
	n := 0
	for _, ref := range refs {
		for _, e := range ref.entries {
			if e.name() != name || (e.version() != version && stripEpoch(e.version()) != version) {
				continue
			}
			body, err := l.store.Read(e.Filename)
			if err != nil {
				return err
			}
			if _, err := d.Guard.Check(d.store.Store, d.store.Exists(e.Filename), int64(len(body))); err != nil {
				return err
			}
			if _, err := d.addDeb(ref.dist, ref.comp, body, e.Control, e.Published); err != nil {
				return err
			}
			d.EmitPublished(name+"@"+e.version()+"/"+e.arch(), int64(len(body)))
			n++
		}
	}
	if n == 0 {
		return registry.ErrPackageNotFound
	}
	return nil
}

// addDeb stores body in the pool, records it in dist/comp and
// regenerates every distribution referencing the pool file.
func (l *Local) addDeb(dist, comp string, body []byte, ctl stanza, published time.Time) (entry, error) {
	e := newEntry(ctl, comp, body, published)
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.store.Write(e.Filename, body); err != nil {
		return e, err
	}
	refs, err := l.store.allIndexes()
	if err != nil {
		return e, err
	}
	affected := map[string]bool{dist: true}
	target := false
	for _, ref := range refs {
		changed := false
		for i, old := range ref.entries {
			if old.Filename != e.Filename {
				continue
			}
			pub := old.Published
			ref.entries[i] = e
			if ref.dist == dist && ref.comp == comp {
				target = true
			} else {
				ref.entries[i].Published = pub
			}
			changed = true
		}
		if changed {
			affected[ref.dist] = true
			if err := l.store.saveIndex(ref.dist, ref.comp, ref.entries); err != nil {
				return e, err
			}
		}
	}
	if !target {
		es, err := l.store.loadIndex(dist, comp)
		if err != nil {
			return e, err
		}
		if err := l.store.saveIndex(dist, comp, append(es, e)); err != nil {
			return e, err
		}
	}
	return e, l.regenerate(sortedKeys(affected))
}

// removeWhere drops matching entries from every index, deletes pool
// files that are no longer referenced and regenerates.
func (l *Local) removeWhere(match func(entry) bool) ([]entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	refs, err := l.store.allIndexes()
	if err != nil {
		return nil, err
	}
	var removed []entry
	affected := map[string]bool{}
	referenced := map[string]bool{}
	for _, ref := range refs {
		kept := ref.entries[:0]
		for _, e := range ref.entries {
			if match(e) {
				removed = append(removed, e)
				continue
			}
			kept = append(kept, e)
			referenced[e.Filename] = true
		}
		if len(kept) != len(ref.entries) {
			affected[ref.dist] = true
			if err := l.store.saveIndex(ref.dist, ref.comp, kept); err != nil {
				return removed, err
			}
		}
	}
	for _, e := range removed {
		if !referenced[e.Filename] {
			if err := l.store.Delete(e.Filename); err != nil {
				return removed, err
			}
		}
	}
	return removed, l.regenerate(sortedKeys(affected))
}

func (l *Local) regenerate(dists []string) error {
	now := time.Now()
	for _, d := range dists {
		if err := l.store.regenerate(d, l.RepoName, l.key, now); err != nil {
			return fmt.Errorf("regenerate %s: %w", d, err)
		}
	}
	return nil
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
		dists, err := l.store.distributions()
		if err != nil {
			pkgbase.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{"distributions": dists, "signed": l.key != nil})
		return
	case p == "public.key" || p == "public.asc" || p == "public.gpg" || (l.keyName != "" && p == l.keyName):
		if l.key == nil {
			pkgbase.NotFound(w)
			return
		}
		var body []byte
		var err error
		if p == "public.gpg" {
			body, err = l.key.PublicKeyBinary()
		} else {
			body, err = l.key.PublicKeyArmored()
		}
		if err != nil {
			pkgbase.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		pkgbase.WriteBytes(w, r, http.StatusOK, contentType(".key"), body)
		return
	case strings.HasPrefix(p, distsDir+"/"), strings.HasPrefix(p, poolDir+"/"):
		if pkgbase.ServeStored(w, r, l.store.Store, p, contentType(p)) {
			return
		}
	}
	pkgbase.NotFound(w)
}

// uploadTarget resolves distribution/component from the request.
func uploadTarget(r *http.Request) (dist, comp string, ok bool) {
	dist, comp = defaultDist, defaultComp
	p := strings.Trim(r.URL.Path, "/")
	parts := strings.Split(p, "/")
	switch {
	case p == "api/upload":
		if v := r.URL.Query().Get("distribution"); v != "" {
			dist = v
		}
		if v := r.URL.Query().Get("component"); v != "" {
			comp = v
		}
	case parts[0] == "upload":
		if len(parts) > 1 {
			dist = parts[1]
		}
		if len(parts) > 2 {
			comp = parts[2]
		}
		if len(parts) > 4 || (len(parts) == 4 && !strings.HasSuffix(parts[3], ".deb")) {
			return "", "", false
		}
	default:
		return "", "", false
	}
	return dist, comp, reSegment.MatchString(dist) && reSegment.MatchString(comp)
}

func readUpload(r *http.Request, max int64) ([]byte, error) {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt == "multipart/form-data" {
		mr, err := r.MultipartReader()
		if err != nil {
			return nil, err
		}
		for {
			part, err := mr.NextPart()
			if err != nil {
				return nil, errors.New("multipart upload has no file part")
			}
			if part.FormName() == "file" || part.FileName() != "" {
				return pkgbase.ReadBody(part, max)
			}
		}
	}
	return pkgbase.ReadBody(r.Body, max)
}

func (l *Local) upload(w http.ResponseWriter, r *http.Request) {
	if !l.CheckPush(w) {
		return
	}
	dist, comp, ok := uploadTarget(r)
	if !ok {
		pkgbase.Error(w, http.StatusBadRequest, "expected PUT /upload/{distribution}/{component}")
		return
	}
	body, err := readUpload(r, l.MaxUpload)
	if err != nil {
		pkgbase.Error(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	ctl, err := parseDeb(body)
	if err != nil {
		pkgbase.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	name, ver, arch := ctl.Get("Package"), ctl.Get("Version"), ctl.Get("Architecture")
	if !l.AllowPublish(w, l.store.Exists(poolPath(comp, name, ver, arch)), int64(len(body))) {
		return
	}
	e, err := l.addDeb(dist, comp, body, ctl, time.Now())
	if err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	l.EmitPublished(name+"@"+ver+"/"+arch, e.Size)
	pkgbase.WriteJSON(w, r, http.StatusCreated, map[string]any{
		"package": name, "version": ver, "architecture": arch,
		"distribution": dist, "component": comp,
		"filename": e.Filename, "size": e.Size, "sha256": e.SHA256,
	})
}

func (l *Local) remove(w http.ResponseWriter, r *http.Request) {
	if !l.CheckPush(w) {
		return
	}
	p := strings.Trim(path.Clean("/"+r.URL.Path), "/")
	if !strings.HasPrefix(p, poolDir+"/") || !strings.HasSuffix(p, ".deb") {
		pkgbase.Error(w, http.StatusBadRequest, "expected DELETE /pool/.../{package}_{version}_{arch}.deb")
		return
	}
	existed := l.store.Exists(p)
	removed, err := l.removeWhere(func(e entry) bool { return e.Filename == p })
	if err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(removed) == 0 {
		if !existed {
			pkgbase.NotFound(w)
			return
		}
		if err := l.store.Delete(p); err != nil {
			pkgbase.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	name, ver, arch, _ := parseDebFilename(p)
	l.EmitDeleted(name + "@" + ver + "/" + arch)
	w.WriteHeader(http.StatusNoContent)
}
