// Package terraform implements the Terraform / OpenTofu module and
// provider registry protocols (v1):
//
//	GET    /.well-known/terraform.json                                 service discovery
//	GET    /v1/modules[/search][?q=]  |  /v1/modules/{ns}              module list (local)
//	GET    /v1/modules/{ns}/{name}/{system}                            latest module info
//	GET    /v1/modules/{ns}/{name}/{system}/versions                   module versions
//	GET    /v1/modules/{ns}/{name}/{system}/{version}                  module info
//	GET    /v1/modules/{ns}/{name}/{system}/{version}/download         204 + X-Terraform-Get
//	GET    /v1/modules/{ns}/{name}/{system}/download                   latest → redirect to download
//	PUT    /v1/modules/{ns}/{name}/{system}/{version}                  publish (raw .tar.gz or .zip body)
//	DELETE /v1/modules/{ns}/{name}/{system}/{version}
//	GET    /archive/modules/{ns}/{name}/{system}/{version}.tar.gz|.zip module archive
//
//	GET    /v1/providers/{ns}/{type}/versions                          provider versions + platforms
//	GET    /v1/providers/{ns}/{type}/{version}/download/{os}/{arch}    package metadata
//	PUT    /v1/providers/{ns}/{type}/{version}/{os}/{arch}[?protocols=5.0,6.0]  publish zip
//	PUT    /v1/providers/{ns}/{type}/{version}/SHA256SUMS              override generated sums
//	PUT    /v1/providers/{ns}/{type}/{version}/SHA256SUMS.sig          publisher's detached signature
//	PUT    /v1/providers/{ns}/{type}/{version}/signing-key             publisher's armored public key
//	DELETE /v1/providers/{ns}/{type}/{version}[/{os}/{arch}]
//	GET    /archive/providers/{ns}/{type}/{version}/{file}             zips, SHA256SUMS, SHA256SUMS.sig
//
// Terraform discovers services at https://{host}/.well-known/terraform.json
// — the host root — so the supported setup is a dedicated registry
// listener (Listeners → Registry) where the repository is mounted at
// "/". Module sources then look like {host}/{namespace}/{name}/{system}
// and provider sources like {host}/{namespace}/{type}. WellKnown builds
// the discovery document for servers that mount it at the host root
// themselves.
//
// Provider SHA256SUMS are generated from the stored zips. Terraform
// requires a signed SHA256SUMS: publishers may upload their own
// signature and public key (goreleaser style; upload them after every
// zip of the version, any zip change discards them). Otherwise, when
// the repository has a SigningKey, kutu signs SHA256SUMS itself and
// advertises that key.
//
// Remote repositories proxy registry.terraform.io (or any registry;
// endpoints are discovered from the upstream's terraform.json).
// Version lists are TTL-cached. Module download locations pointing at
// http(s) archives are cached and rewritten to kutu; other go-getter
// sources (git::, github.com/…) are passed through. Provider binaries,
// SHA256SUMS and signatures are cached and served from kutu; the
// upstream signing keys stay valid because bytes are identical.
//
// Virtual repositories merge version lists and serve everything else
// first-hit.
//
// Client configuration (~/.terraformrc; Terraform fetches archives via
// go-getter, which reads ~/.netrc rather than the credentials block):
//
//	credentials "{host}" { token = "<token>" }
//	# ~/.netrc: machine {host} login x password <token>
//
//	module "vpc" { source = "{host}/acme/vpc/aws" version = "1.0.0" }
//	terraform { required_providers { foo = { source = "{host}/acme/foo" } } }
//
//	curl -H "Authorization: Bearer <token>" -T vpc.tar.gz {base}/v1/modules/acme/vpc/aws/1.0.0
//	curl -H "Authorization: Bearer <token>" -T terraform-provider-foo_1.0.0_linux_amd64.zip \
//	  {base}/v1/providers/acme/foo/1.0.0/linux/amd64
package terraform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/registry/virtualbase"
	"github.com/rakunlabs/kutu/internal/service"
)

const typ = service.RegistryTypeTerraform

const (
	modulesRoot   = "modules"
	providersRoot = "providers"
	metaDir       = "meta"

	providerMetaFile = "meta.json"
	uploadedSums     = "SHA256SUMS.upload"
	uploadedSig      = "SHA256SUMS.sig.upload"
	uploadedKey      = "signing-key.asc"
	locationFile     = "location"
)

var defaultProtocols = []string{"5.0"}

// WellKnown returns the service discovery document for a registry
// mounted at prefix ("" on a dedicated listener).
func WellKnown(prefix string) []byte {
	prefix = strings.TrimRight(prefix, "/")
	b, _ := pkgbase.MarshalJSON(map[string]string{
		"modules.v1":   prefix + "/v1/modules/",
		"providers.v1": prefix + "/v1/providers/",
	})
	return b
}

func serveWellKnown(w http.ResponseWriter, r *http.Request) {
	pkgbase.WriteBytes(w, r, http.StatusOK, "application/json", WellKnown(pkgbase.Prefix(r)))
}

// Store wraps pkgbase.Store with the terraform layout:
//
//	modules/{ns}/{name}/{system}/{version}/module.tar.gz|module.zip|location
//	providers/{ns}/{type}/{version}/{zips, meta.json | download_{os}_{arch}.json}
//	meta/…  (remote, TTL-bound upstream documents)
type Store struct {
	*pkgbase.Store
	remote bool
}

func moduleDir(ns, name, system string, ver ...string) string {
	return path.Join(append([]string{modulesRoot, ns, name, system}, ver...)...)
}

// moduleDirOf is moduleDir for a {ns, name, system[, version]} slice.
func moduleDirOf(parts []string) string {
	return path.Join(append([]string{modulesRoot}, parts...)...)
}

func providerDir(ns, ptype string, ver ...string) string {
	return path.Join(append([]string{providersRoot, ns, ptype}, ver...)...)
}

func validSegment(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\\\x00")
}

// splitRest returns the segments of p after prefix, validating each.
func splitRest(p, prefix string) ([]string, bool) {
	if !strings.HasPrefix(p, prefix) {
		return nil, false
	}
	rest := strings.Trim(strings.TrimPrefix(p, prefix), "/")
	if rest == "" {
		return nil, true
	}
	parts := strings.Split(rest, "/")
	for _, s := range parts {
		if !validSegment(s) {
			return nil, false
		}
	}
	return parts, true
}

func sumsName(ptype, ver string) string {
	return "terraform-provider-" + ptype + "_" + ver + "_SHA256SUMS"
}

func zipName(ptype, ver, osName, arch string) string {
	return "terraform-provider-" + ptype + "_" + ver + "_" + osName + "_" + arch + ".zip"
}

// moduleArchive returns the stored archive file name and extension of
// a module version ("" when absent).
func (s *Store) moduleArchive(dir string) (string, string) {
	for _, ext := range []string{"tar.gz", "zip"} {
		if s.Exists(path.Join(dir, "module."+ext)) {
			return "module." + ext, ext
		}
	}
	return "", ""
}

func (s *Store) isModuleVersion(dir string) bool {
	if s.remote {
		return s.Exists(path.Join(dir, locationFile))
	}
	f, _ := s.moduleArchive(dir)
	return f != ""
}

func (s *Store) isProviderVersion(dir string) bool {
	if !s.remote {
		m, err := s.providerMeta(dir)
		return err == nil && len(m.Platforms) > 0
	}
	files, _ := s.ListFiles(dir)
	for _, f := range files {
		if strings.HasPrefix(f.Name, "download_") && strings.HasSuffix(f.Name, ".json") {
			return true
		}
	}
	return false
}

func (s *Store) versionsIn(dir string, ok func(string) bool) []string {
	dirs, _ := s.ListDirs(dir)
	var out []string
	for _, d := range dirs {
		if ok(path.Join(dir, d)) {
			out = append(out, d)
		}
	}
	pkgbase.SortVersions(out)
	return out
}

// ModuleVersions returns the versions of a module (ascending).
func (s *Store) ModuleVersions(ns, name, system string) []string {
	return s.versionsIn(moduleDir(ns, name, system), s.isModuleVersion)
}

// ProviderVersions returns the versions of a provider (ascending).
func (s *Store) ProviderVersions(ns, ptype string) []string {
	return s.versionsIn(providerDir(ns, ptype), s.isProviderVersion)
}

// ListPackages lists "modules/{ns}/{name}/{system}" and
// "providers/{ns}/{type}" entries.
func (s *Store) ListPackages(context.Context) ([]registry.PackageSummary, error) {
	var out []registry.PackageSummary
	nss, err := s.ListDirs(modulesRoot)
	if err != nil {
		return nil, err
	}
	for _, ns := range nss {
		names, _ := s.ListDirs(path.Join(modulesRoot, ns))
		for _, name := range names {
			systems, _ := s.ListDirs(path.Join(modulesRoot, ns, name))
			for _, sys := range systems {
				if vs := s.ModuleVersions(ns, name, sys); len(vs) > 0 {
					out = append(out, registry.PackageSummary{Name: path.Join(modulesRoot, ns, name, sys), Versions: vs})
				}
			}
		}
	}
	nss, err = s.ListDirs(providersRoot)
	if err != nil {
		return nil, err
	}
	for _, ns := range nss {
		types, _ := s.ListDirs(path.Join(providersRoot, ns))
		for _, t := range types {
			if vs := s.ProviderVersions(ns, t); len(vs) > 0 {
				out = append(out, registry.PackageSummary{Name: path.Join(providersRoot, ns, t), Versions: vs})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// parseName splits a PackageLister name into its kind and segments.
func parseName(name string) (kind string, parts []string, ok bool) {
	parts = strings.Split(strings.Trim(name, "/"), "/")
	for _, p := range parts {
		if !validSegment(p) {
			return "", nil, false
		}
	}
	switch {
	case parts[0] == modulesRoot && len(parts) == 4:
		return modulesRoot, parts[1:], true
	case parts[0] == providersRoot && len(parts) == 3:
		return providersRoot, parts[1:], true
	}
	return "", nil, false
}

// versionDir maps a package name + version to its store directory.
func versionDir(name, version string) (string, string, bool) {
	kind, parts, ok := parseName(name)
	if !ok || !validSegment(version) {
		return "", "", false
	}
	if kind == modulesRoot {
		return kind, moduleDir(parts[0], parts[1], parts[2], version), true
	}
	return kind, providerDir(parts[0], parts[1], version), true
}

// ── provider metadata (local) ──

type providerPlatform struct {
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Filename string `json:"filename"`
	SHASum   string `json:"shasum"`
	Size     int64  `json:"size"`
}

type providerMeta struct {
	Protocols   []string           `json:"protocols"`
	Platforms   []providerPlatform `json:"platforms"`
	PublishedAt time.Time          `json:"published_at"`
}

func (m *providerMeta) platform(osName, arch string) *providerPlatform {
	for i := range m.Platforms {
		if m.Platforms[i].OS == osName && m.Platforms[i].Arch == arch {
			return &m.Platforms[i]
		}
	}
	return nil
}

func (m *providerMeta) sums() []byte {
	ps := append([]providerPlatform(nil), m.Platforms...)
	sort.Slice(ps, func(i, j int) bool { return ps[i].Filename < ps[j].Filename })
	var buf bytes.Buffer
	for _, p := range ps {
		fmt.Fprintf(&buf, "%s  %s\n", p.SHASum, p.Filename)
	}
	return buf.Bytes()
}

func (s *Store) providerMeta(dir string) (*providerMeta, error) {
	b, err := s.Read(path.Join(dir, providerMetaFile))
	if err != nil {
		return nil, err
	}
	var m providerMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *Store) writeProviderMeta(dir string, m *providerMeta) error {
	sort.Slice(m.Platforms, func(i, j int) bool {
		if m.Platforms[i].OS != m.Platforms[j].OS {
			return m.Platforms[i].OS < m.Platforms[j].OS
		}
		return m.Platforms[i].Arch < m.Platforms[j].Arch
	})
	b, err := pkgbase.MarshalJSON(m)
	if err != nil {
		return err
	}
	return s.Write(path.Join(dir, providerMetaFile), b)
}

// ── detail ──

func detail(s *Store, name string) (*registry.PackageDetail, error) {
	kind, parts, ok := parseName(name)
	if !ok {
		return nil, registry.ErrInvalidPackageName
	}
	name = path.Join(append([]string{kind}, parts...)...)
	var versions []string
	var dir string
	if kind == modulesRoot {
		dir = moduleDir(parts[0], parts[1], parts[2])
		versions = s.ModuleVersions(parts[0], parts[1], parts[2])
	} else {
		dir = providerDir(parts[0], parts[1])
		versions = s.ProviderVersions(parts[0], parts[1])
	}
	if len(versions) == 0 {
		return nil, registry.ErrPackageNotFound
	}
	d := &registry.GenericPackageDetail{
		LatestVersion: pkgbase.Latest(versions),
		Metadata:      map[string]string{"kind": strings.TrimSuffix(kind, "s"), "source": strings.Join(parts, "/")},
	}
	for i := len(versions) - 1; i >= 0; i-- {
		v := versions[i]
		vdir := path.Join(dir, v)
		row := registry.GenericVersionDetail{Version: v}
		files, _ := s.ListFiles(vdir)
		var newest time.Time
		for _, f := range files {
			row.Files = append(row.Files, registry.GenericFile{Name: f.Name, Size: f.Size})
			row.Size += f.Size
			if fi, err := s.Stat(path.Join(vdir, f.Name)); err == nil && fi.ModTime.After(newest) {
				newest = fi.ModTime
			}
		}
		if kind == providersRoot && !s.remote {
			if m, err := s.providerMeta(vdir); err == nil {
				var plats []string
				for _, p := range m.Platforms {
					plats = append(plats, p.OS+"_"+p.Arch)
					for j := range row.Files {
						if row.Files[j].Name == p.Filename {
							row.Files[j].SHA256 = p.SHASum
						}
					}
				}
				row.Metadata = map[string]string{"protocols": strings.Join(m.Protocols, ","), "platforms": strings.Join(plats, ",")}
				if !m.PublishedAt.IsZero() {
					newest = m.PublishedAt
				}
			}
		}
		if !newest.IsZero() {
			row.PublishedAt = newest.UTC().Format(time.RFC3339)
		}
		d.Versions = append(d.Versions, row)
	}
	return &registry.PackageDetail{Type: typ, Name: name, Generic: d}, nil
}

// classify maps data-plane requests to {package, version}.
func classify(r *http.Request) (registry.ArtifactRef, bool) {
	p := r.URL.Path
	if parts, ok := splitRest(p, "/v1/modules/"); ok && len(parts) >= 3 {
		ref := registry.ArtifactRef{Name: path.Join(modulesRoot, parts[0], parts[1], parts[2])}
		if len(parts) == 5 && parts[4] == "download" {
			ref.Version = parts[3]
		}
		return ref, true
	}
	if parts, ok := splitRest(p, "/v1/providers/"); ok && len(parts) >= 2 {
		ref := registry.ArtifactRef{Name: path.Join(providersRoot, parts[0], parts[1])}
		if len(parts) == 6 && parts[3] == "download" {
			ref.Version = parts[2]
		}
		return ref, true
	}
	if parts, ok := splitRest(p, "/archive/modules/"); ok && len(parts) == 4 {
		ver, _ := splitArchiveName(parts[3])
		if ver == "" {
			return registry.ArtifactRef{}, false
		}
		return registry.ArtifactRef{Name: path.Join(modulesRoot, parts[0], parts[1], parts[2]), Version: ver}, true
	}
	if parts, ok := splitRest(p, "/archive/providers/"); ok && len(parts) == 4 {
		return registry.ArtifactRef{Name: path.Join(providersRoot, parts[0], parts[1]), Version: parts[2]}, true
	}
	return registry.ArtifactRef{}, false
}

// splitArchiveName splits "1.0.0.tar.gz" into ("1.0.0", "tar.gz").
func splitArchiveName(f string) (string, string) {
	for _, ext := range []string{"tar.gz", "zip"} {
		if v := strings.TrimSuffix(f, "."+ext); v != f && v != "" {
			return v, ext
		}
	}
	return "", ""
}

func moduleArchiveURL(base, ns, name, system, ver, ext string) string {
	return base + "/archive/modules/" + ns + "/" + name + "/" + system + "/" + ver + "." + ext
}

func providerArchiveBase(base, ns, ptype, ver string) string {
	return base + "/archive/providers/" + ns + "/" + ptype + "/" + ver
}

func moduleVersionsDoc(ns, name, system string, versions []string) map[string]any {
	vs := make([]map[string]any, 0, len(versions))
	for i := len(versions) - 1; i >= 0; i-- {
		vs = append(vs, map[string]any{"version": versions[i]})
	}
	return map[string]any{"modules": []map[string]any{{"source": ns + "/" + name + "/" + system, "versions": vs}}}
}

func armoredKeyID(armored []byte) (string, error) {
	list, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(armored))
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		return "", fmt.Errorf("no key in armored block")
	}
	return fmt.Sprintf("%016X", list[0].PrimaryKey.KeyId), nil
}

var repoLocks sync.Map

func lockFor(key string) *sync.Mutex {
	m, _ := repoLocks.LoadOrStore(key, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// ── Virtual ──

// Virtual merges version lists across members and serves everything
// else first-hit.
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
	if !pkgbase.IsRead(r) {
		v.Virtual.ServeHTTP(w, r)
		return
	}
	p := r.URL.Path
	if p == "/.well-known/terraform.json" {
		serveWellKnown(w, r)
		return
	}
	if parts, ok := splitRest(p, "/v1/modules/"); ok && len(parts) == 4 && parts[3] == "versions" {
		v.mergeModuleVersions(w, r, parts)
		return
	}
	if parts, ok := splitRest(p, "/v1/providers/"); ok && len(parts) == 3 && parts[2] == "versions" {
		v.mergeProviderVersions(w, r)
		return
	}
	v.Virtual.ServeHTTP(w, r)
}

func (v *Virtual) mergeModuleVersions(w http.ResponseWriter, r *http.Request, parts []string) {
	bodies := v.CollectMembers(r)
	if len(bodies) == 0 {
		pkgbase.NotFound(w)
		return
	}
	seen := map[string]bool{}
	var versions []string
	for _, b := range bodies {
		var doc struct {
			Modules []struct {
				Versions []struct {
					Version string `json:"version"`
				} `json:"versions"`
			} `json:"modules"`
		}
		if json.Unmarshal(b, &doc) != nil {
			continue
		}
		for _, m := range doc.Modules {
			for _, ver := range m.Versions {
				if ver.Version != "" && !seen[ver.Version] {
					seen[ver.Version] = true
					versions = append(versions, ver.Version)
				}
			}
		}
	}
	pkgbase.SortVersions(versions)
	pkgbase.WriteJSON(w, r, http.StatusOK, moduleVersionsDoc(parts[0], parts[1], parts[2], versions))
}

func (v *Virtual) mergeProviderVersions(w http.ResponseWriter, r *http.Request) {
	bodies := v.CollectMembers(r)
	if len(bodies) == 0 {
		pkgbase.NotFound(w)
		return
	}
	seen := map[string]bool{}
	var out []json.RawMessage
	var order []string
	byVer := map[string]json.RawMessage{}
	for _, b := range bodies {
		var doc struct {
			Versions []json.RawMessage `json:"versions"`
		}
		if json.Unmarshal(b, &doc) != nil {
			continue
		}
		for _, raw := range doc.Versions {
			var head struct {
				Version string `json:"version"`
			}
			if json.Unmarshal(raw, &head) != nil || head.Version == "" || seen[head.Version] {
				continue
			}
			seen[head.Version] = true
			order = append(order, head.Version)
			byVer[head.Version] = raw
		}
	}
	pkgbase.SortVersions(order)
	for i := len(order) - 1; i >= 0; i-- {
		out = append(out, byVer[order[i]])
	}
	pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{"versions": out})
}
