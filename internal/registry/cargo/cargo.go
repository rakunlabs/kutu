// Package cargo implements a Cargo sparse registry (RFC 2789) plus the
// registry web API used by `cargo publish/yank/owner/search`:
//
//	GET    /config.json                                  sparse index config (auth-required)
//	GET    /{prefix}/{crate}                             sparse index file (ETag / Last-Modified / 304)
//	GET    /api/v1/crates?q=&per_page=                   search
//	PUT    /api/v1/crates/new                            publish (cargo binary body)
//	GET    /api/v1/crates/{crate}/{version}/download     download .crate
//	PUT    /api/v1/crates/{crate}/{version}/download     legacy raw .crate upload (index deps empty)
//	DELETE /api/v1/crates/{crate}/{version}/yank         yank
//	PUT    /api/v1/crates/{crate}/{version}/unyank       unyank
//	GET    /api/v1/crates/{crate}/owners                 list owners
//	PUT    /api/v1/crates/{crate}/owners                 add owners
//	DELETE /api/v1/crates/{crate}/owners                 remove owners
//
// Remote repos proxy an upstream sparse index (e.g. https://index.crates.io)
// and fetch crates through the upstream config.json `dl` template. Virtual
// repos merge index files across members.
//
// Client configuration (~/.cargo/config.toml):
//
//	[registries.kutu]
//	index = "sparse+https://kutu.example.com/registries/{ns}/{repo}/"
//	credential-provider = "cargo:token"
//
// then `cargo login --registry kutu <kutu token>`.
package cargo

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
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
	"github.com/rakunlabs/kutu/internal/service"
)

const typ = service.RegistryTypeCargo

// ── Store ──

// Store wraps pkgbase.Store with the cargo layout:
//
//	index/{prefix}/{name}        sparse index file
//	crates/{name}/{ver}/{name}-{ver}.crate
//	meta/{name}/{ver}.json       publish metadata sidecar
//	meta/{name}/owners.json      owners list
//	upstream/…                   remote-only upstream config + validators
type Store struct {
	*pkgbase.Store
	mu *sync.Mutex
}

// NewStore returns a cargo store rooted at basePath on fs.
func NewStore(fs rawfs.RawFS, basePath string) *Store {
	return wrapStore(pkgbase.NewStore(fs, basePath))
}

func wrapStore(s *pkgbase.Store) *Store { return &Store{Store: s, mu: &sync.Mutex{}} }

func norm(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// prefixOf returns the sparse index directory of name (case preserved).
func prefixOf(name string) string {
	switch len(name) {
	case 0:
		return ""
	case 1:
		return "1"
	case 2:
		return "2"
	case 3:
		return "3/" + name[:1]
	default:
		return name[:2] + "/" + name[2:4]
	}
}

func indexPath(name string) string {
	n := norm(name)
	if n == "" {
		return ""
	}
	return prefixOf(n) + "/" + n
}

// parseIndexPath maps "/se/rd/serde" to "serde".
func parseIndexPath(p string) (string, bool) {
	p = strings.ToLower(strings.Trim(p, "/"))
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return "", false
	}
	name := p[i+1:]
	if !validName(name) || indexPath(name) != p {
		return "", false
	}
	return name, true
}

func crateRel(name, version string) string {
	n := norm(name)
	return path.Join("crates", n, version, n+"-"+version+".crate")
}

func indexRel(idx string) string          { return path.Join("index", idx) }
func metaRel(name, version string) string { return path.Join("meta", norm(name), version+".json") }
func ownersRel(name string) string        { return path.Join("meta", norm(name), "owners.json") }

var (
	nameRe    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)
	versionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)
)

func validName(n string) bool    { return nameRe.MatchString(n) }
func validVersion(v string) bool { return versionRe.MatchString(v) }

type indexDep struct {
	Name            string   `json:"name"`
	Req             string   `json:"req"`
	Features        []string `json:"features"`
	Optional        bool     `json:"optional"`
	DefaultFeatures bool     `json:"default_features"`
	Target          *string  `json:"target"`
	Kind            string   `json:"kind"`
	Registry        *string  `json:"registry"`
	Package         *string  `json:"package,omitempty"`
}

type indexEntry struct {
	Name        string              `json:"name"`
	Version     string              `json:"vers"`
	Deps        []indexDep          `json:"deps"`
	CKSum       string              `json:"cksum"`
	Features    map[string][]string `json:"features"`
	Features2   map[string][]string `json:"features2,omitempty"`
	Yanked      bool                `json:"yanked"`
	Links       *string             `json:"links"`
	V           int                 `json:"v,omitempty"`
	RustVersion string              `json:"rust_version,omitempty"`
	PubTime     string              `json:"pubtime,omitempty"`
}

// versionMeta is the publish-metadata sidecar of one version.
type versionMeta struct {
	Name          string    `json:"name"`
	Version       string    `json:"vers"`
	Description   string    `json:"description,omitempty"`
	License       string    `json:"license,omitempty"`
	LicenseFile   string    `json:"license_file,omitempty"`
	Homepage      string    `json:"homepage,omitempty"`
	Documentation string    `json:"documentation,omitempty"`
	Repository    string    `json:"repository,omitempty"`
	Keywords      []string  `json:"keywords,omitempty"`
	Categories    []string  `json:"categories,omitempty"`
	Authors       []string  `json:"authors,omitempty"`
	PublishedAt   time.Time `json:"published_at"`
}

type owner struct {
	ID    int    `json:"id"`
	Login string `json:"login"`
	Name  string `json:"name,omitempty"`
}

func parseEntries(body []byte) []indexEntry {
	var out []indexEntry
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var ent indexEntry
		if json.Unmarshal(line, &ent) == nil && ent.Name != "" && ent.Version != "" {
			out = append(out, ent)
		}
	}
	return out
}

func sortEntries(entries []indexEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		return pkgbase.CompareVersions(entries[i].Version, entries[j].Version) < 0
	})
}

func encodeEntries(entries []indexEntry) []byte {
	var b bytes.Buffer
	for _, ent := range entries {
		if ent.Deps == nil {
			ent.Deps = []indexDep{}
		}
		if ent.Features == nil {
			ent.Features = map[string][]string{}
		}
		line, _ := pkgbase.MarshalJSON(ent)
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

func findEntry(entries []indexEntry, version string) (indexEntry, bool) {
	for _, e := range entries {
		if e.Version == version {
			return e, true
		}
	}
	return indexEntry{}, false
}

// ReadIndex returns the parsed entries of the index file at idx.
func (s *Store) ReadIndex(idx string) ([]indexEntry, error) {
	body, err := s.Read(indexRel(idx))
	if err != nil {
		if pkgbase.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return parseEntries(body), nil
}

// ReadIndexByName returns the parsed entries of crate name.
func (s *Store) ReadIndexByName(name string) ([]indexEntry, error) {
	idx := indexPath(name)
	if idx == "" {
		return nil, nil
	}
	return s.ReadIndex(idx)
}

func (s *Store) writeEntries(name string, entries []indexEntry) error {
	rel := indexRel(indexPath(name))
	if len(entries) == 0 {
		return s.Delete(rel)
	}
	sortEntries(entries)
	return s.Write(rel, encodeEntries(entries))
}

func (s *Store) hasVersion(name, version string) bool {
	ents, _ := s.ReadIndexByName(name)
	_, ok := findEntry(ents, version)
	return ok
}

// put stores crate bytes, the optional metadata sidecar and upserts
// the index entry.
func (s *Store) put(ent indexEntry, vm *versionMeta, crate []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.Write(crateRel(ent.Name, ent.Version), crate); err != nil {
		return err
	}
	if vm != nil {
		b, _ := pkgbase.MarshalJSON(vm)
		if err := s.Write(metaRel(ent.Name, ent.Version), b); err != nil {
			return err
		}
	}
	entries, err := s.ReadIndexByName(ent.Name)
	if err != nil {
		return err
	}
	replaced := false
	for i := range entries {
		if entries[i].Version == ent.Version {
			entries[i] = ent
			replaced = true
		}
	}
	if !replaced {
		entries = append(entries, ent)
	}
	return s.writeEntries(ent.Name, entries)
}

// WriteCrate stores a raw .crate and indexes it with empty deps.
func (s *Store) WriteCrate(name, version string, body []byte) error {
	return s.put(rawEntry(name, version, body), &versionMeta{Name: name, Version: version, PublishedAt: time.Now().UTC()}, body)
}

func rawEntry(name, version string, body []byte) indexEntry {
	return indexEntry{
		Name: name, Version: version, CKSum: pkgbase.SHA256Hex(body),
		Deps: []indexDep{}, Features: map[string][]string{},
		PubTime: time.Now().UTC().Format(time.RFC3339),
	}
}

// OpenCrate opens a stored .crate.
func (s *Store) OpenCrate(name, version string) (rawfs.ReadSeekCloser, *rawfs.FileInfo, error) {
	return s.Open(crateRel(name, version))
}

func (s *Store) setYanked(name, version string, yanked bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.ReadIndexByName(name)
	if err != nil {
		return err
	}
	found := false
	for i := range entries {
		if entries[i].Version == version {
			entries[i].Yanked = yanked
			found = true
		}
	}
	if !found {
		return registry.ErrPackageNotFound
	}
	return s.writeEntries(name, entries)
}

// DeleteVersion removes a crate archive, its metadata and its index row.
func (s *Store) DeleteVersion(name, version string) error {
	name = strings.TrimSpace(name)
	version = strings.TrimSpace(version)
	if indexPath(name) == "" || version == "" {
		return registry.ErrInvalidPackageName
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.ReadIndexByName(name)
	if err != nil {
		return err
	}
	next := entries[:0]
	found := false
	for _, ent := range entries {
		if ent.Version == version {
			found = true
			continue
		}
		next = append(next, ent)
	}
	if !found {
		return registry.ErrPackageNotFound
	}
	if err := s.Delete(crateRel(name, version)); err != nil {
		return err
	}
	_ = s.Delete(metaRel(name, version))
	if len(next) == 0 {
		_ = s.Delete(ownersRel(name))
	}
	return s.writeEntries(name, next)
}

func (s *Store) readMeta(name, version string) *versionMeta {
	b, err := s.Read(metaRel(name, version))
	if err != nil {
		return nil
	}
	var vm versionMeta
	if json.Unmarshal(b, &vm) != nil {
		return nil
	}
	return &vm
}

func (s *Store) readOwners(name string) []owner {
	b, err := s.Read(ownersRel(name))
	if err != nil {
		return nil
	}
	var out []owner
	_ = json.Unmarshal(b, &out)
	return out
}

func (s *Store) writeOwners(name string, owners []owner) error {
	for i := range owners {
		owners[i].ID = i + 1
	}
	b, _ := pkgbase.MarshalJSON(owners)
	return s.Write(ownersRel(name), b)
}

// ListCrates returns every crate name present in the index (sorted).
func (s *Store) ListCrates() ([]string, error) {
	var out []string
	err := s.Walk("index", func(rel string, _ rawfs.DirEntry) error {
		body, err := s.Read(rel)
		if err != nil {
			return nil
		}
		if ents := parseEntries(body); len(ents) > 0 {
			out = append(out, ents[0].Name)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// ListVersions returns the versions of name (ascending).
func (s *Store) ListVersions(name string) ([]string, error) {
	entries, err := s.ReadIndexByName(name)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, ent := range entries {
		out = append(out, ent.Version)
	}
	pkgbase.SortVersions(out)
	return out, nil
}

func (s *Store) listPackages() ([]registry.PackageSummary, error) {
	names, err := s.ListCrates()
	if err != nil {
		return nil, err
	}
	out := make([]registry.PackageSummary, 0, len(names))
	for _, n := range names {
		vs, _ := s.ListVersions(n)
		out = append(out, registry.PackageSummary{Name: n, Versions: vs})
	}
	return out, nil
}

type searchCrate struct {
	Name        string `json:"name"`
	MaxVersion  string `json:"max_version"`
	Description string `json:"description"`
}

type searchResult struct {
	Crates []searchCrate `json:"crates"`
	Meta   struct {
		Total int `json:"total"`
	} `json:"meta"`
}

func maxVersion(entries []indexEntry) string {
	var all, live []string
	for _, e := range entries {
		all = append(all, e.Version)
		if !e.Yanked {
			live = append(live, e.Version)
		}
	}
	if v := pkgbase.Latest(live); v != "" {
		return v
	}
	return pkgbase.Latest(all)
}

func (s *Store) search(q string, limit int) searchResult {
	q = strings.ToLower(strings.TrimSpace(q))
	var res searchResult
	res.Crates = []searchCrate{}
	names, _ := s.ListCrates()
	for _, n := range names {
		ents, _ := s.ReadIndexByName(n)
		if len(ents) == 0 {
			continue
		}
		mv := maxVersion(ents)
		var desc string
		if vm := s.readMeta(n, mv); vm != nil {
			desc = vm.Description
		}
		if q != "" && !strings.Contains(strings.ToLower(n), q) && !strings.Contains(strings.ToLower(desc), q) {
			continue
		}
		res.Meta.Total++
		if len(res.Crates) < limit {
			res.Crates = append(res.Crates, searchCrate{Name: n, MaxVersion: mv, Description: desc})
		}
	}
	return res
}

func perPage(r *http.Request) int {
	n := 10
	if v := r.URL.Query().Get("per_page"); v != "" {
		var x int
		for _, c := range v {
			if c < '0' || c > '9' {
				x = -1
				break
			}
			x = x*10 + int(c-'0')
			if x > 1000 {
				break
			}
		}
		if x > 0 {
			n = x
		}
	}
	if n > 100 {
		n = 100
	}
	return n
}

func packageDetail(s *Store, name string) (*registry.PackageDetail, error) {
	ents, _ := s.ReadIndexByName(name)
	if len(ents) == 0 {
		return nil, registry.ErrPackageNotFound
	}
	detail := &registry.CargoCrateDetail{}
	var vs []string
	for _, ent := range ents {
		row := registry.CargoVersionDetail{Version: ent.Version, Yanked: ent.Yanked, CKSum: ent.CKSum}
		if fi, err := s.Stat(crateRel(ent.Name, ent.Version)); err == nil {
			row.Size = fi.Size
		}
		detail.Versions = append(detail.Versions, row)
		vs = append(vs, ent.Version)
	}
	detail.LatestVersion = pkgbase.Latest(vs)
	return &registry.PackageDetail{Type: typ, Name: ents[0].Name, Cargo: detail}, nil
}

// ── HTTP helpers ──

func cargoError(w http.ResponseWriter, code int, msg string) {
	body, _ := pkgbase.MarshalJSON(map[string]any{"errors": []map[string]string{{"detail": msg}}})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

func serveConfig(w http.ResponseWriter, r *http.Request) {
	base := pkgbase.PublicBase(r)
	pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{
		"dl":            base + "/api/v1/crates/{crate}/{version}/download",
		"api":           base,
		"auth-required": true,
	})
}

// serveIndexBytes writes a sparse index file with ETag/Last-Modified
// validators; conditional requests get 304.
func serveIndexBytes(w http.ResponseWriter, r *http.Request, body []byte, mod time.Time) {
	sum := sha256.Sum256(body)
	w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:16])+`"`)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	http.ServeContent(w, r, "", mod, bytes.NewReader(body))
}

func serveStoredIndex(w http.ResponseWriter, r *http.Request, s *Store, idx string) {
	rc, fi, err := s.Open(indexRel(idx))
	if err != nil {
		pkgbase.NotFound(w)
		return
	}
	body, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || len(bytes.TrimSpace(body)) == 0 {
		pkgbase.NotFound(w)
		return
	}
	var mod time.Time
	if fi != nil {
		mod = fi.ModTime
	}
	serveIndexBytes(w, r, body, mod)
}

func parseDownload(p string) (string, string, bool) {
	parts := strings.Split(strings.TrimPrefix(p, "/api/v1/crates/"), "/")
	if len(parts) != 3 || parts[2] != "download" || !validName(parts[0]) || !validVersion(parts[1]) {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// apiCrateRoute splits "/api/v1/crates/{crate}/{rest...}".
func apiCrateRoute(p string) (string, []string, bool) {
	rest, ok := strings.CutPrefix(p, "/api/v1/crates/")
	if !ok {
		return "", nil, false
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) < 2 || !validName(parts[0]) {
		return "", nil, false
	}
	return parts[0], parts[1:], true
}

func classify(r *http.Request) (registry.ArtifactRef, bool) {
	p := r.URL.Path
	if name, ver, ok := parseDownload(p); ok {
		return registry.ArtifactRef{Name: norm(name), Version: ver}, true
	}
	if strings.HasPrefix(p, "/api/") || p == "/config.json" {
		return registry.ArtifactRef{}, false
	}
	if name, ok := parseIndexPath(p); ok {
		return registry.ArtifactRef{Name: name}, true
	}
	return registry.ArtifactRef{}, false
}
