package terraform

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/registry/signing"
	"github.com/rakunlabs/kutu/internal/service"
)

// ── Local ──

type Local struct {
	*pkgbase.Repo
	store *Store
	key   *signing.PGPKey
	mu    *sync.Mutex
}

func NewLocalFactory() registry.Factory {
	return func(_ context.Context, deps registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		base, err := pkgbase.NewLocalRepo(deps, typ, ns, r)
		if err != nil {
			return nil, err
		}
		l := &Local{Repo: base, store: &Store{Store: base.Store}, mu: lockFor(ns + "/" + r.Name)}
		if strings.TrimSpace(r.SigningKey) != "" {
			if l.key, err = signing.ParsePGPKey(r.SigningKey); err != nil {
				return nil, fmt.Errorf("terraform/local %s/%s: %w", ns, r.Name, err)
			}
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
	return detail(l.store, name)
}

func (l *Local) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) {
	return classify(r)
}

func (l *Local) ArtifactInfo(_ context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	kind, dir, ok := versionDir(ref.Name, ref.Version)
	if !ok {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	if kind == providersRoot {
		m, err := l.store.providerMeta(dir)
		if err != nil || len(m.Platforms) == 0 {
			return registry.ArtifactMeta{}, registry.ErrPackageNotFound
		}
		return registry.ArtifactMeta{PublishedAt: m.PublishedAt}, nil
	}
	f, _ := l.store.moduleArchive(dir)
	if f == "" {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	fi, err := l.store.Stat(path.Join(dir, f))
	if err != nil {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	return registry.ArtifactMeta{PublishedAt: fi.ModTime}, nil
}

// DeleteVersion removes a module or provider version.
func (l *Local) DeleteVersion(_ context.Context, name, version string) error {
	kind, dir, ok := versionDir(name, version)
	if !ok {
		return registry.ErrPackageNotFound
	}
	if kind == modulesRoot && !l.store.isModuleVersion(dir) || kind == providersRoot && !l.store.isProviderVersion(dir) {
		return registry.ErrPackageNotFound
	}
	if _, _, errs := l.store.DeleteTree(dir); len(errs) > 0 {
		return errs[0]
	}
	l.EmitDeleted(name + "@" + version)
	return nil
}

// PromoteVersion copies name@version into dst (a terraform Local).
func (l *Local) PromoteVersion(_ context.Context, dst registry.Registry, name, version string) error {
	d, ok := dst.(*Local)
	if !ok {
		return fmt.Errorf("terraform: promote target is not a local terraform repository")
	}
	kind, dir, ok := versionDir(name, version)
	if !ok {
		return registry.ErrPackageNotFound
	}
	if kind == modulesRoot && !l.store.isModuleVersion(dir) || kind == providersRoot && !l.store.isProviderVersion(dir) {
		return registry.ErrPackageNotFound
	}
	files, err := l.store.ListFiles(dir)
	if err != nil {
		return err
	}
	var total int64
	for _, f := range files {
		total += f.Size
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	exists := d.store.isModuleVersion(dir)
	if kind == providersRoot {
		exists = d.store.isProviderVersion(dir)
	}
	if _, err := d.Guard.Check(d.store.Store, exists, total); err != nil {
		return err
	}
	if exists {
		d.store.DeleteTree(dir)
	}
	for _, f := range files {
		b, err := l.store.Read(path.Join(dir, f.Name))
		if err != nil {
			return err
		}
		if err := d.store.Write(path.Join(dir, f.Name), b); err != nil {
			return err
		}
	}
	d.EmitPublished(name+"@"+version, total)
	return nil
}

func (l *Local) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if p == "/.well-known/terraform.json" {
		if !pkgbase.IsRead(r) {
			pkgbase.Error(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		serveWellKnown(w, r)
		return
	}
	switch {
	case strings.HasPrefix(p, "/v1/modules"):
		l.serveModules(w, r)
	case strings.HasPrefix(p, "/v1/providers/"):
		l.serveProviders(w, r)
	case strings.HasPrefix(p, "/archive/"):
		if !pkgbase.IsRead(r) {
			pkgbase.Error(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		l.serveArchive(w, r)
	default:
		pkgbase.NotFound(w)
	}
}

// ── modules ──

func (l *Local) serveModules(w http.ResponseWriter, r *http.Request) {
	parts, ok := splitRest(r.URL.Path, "/v1/modules")
	if !ok {
		pkgbase.NotFound(w)
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
	case http.MethodPut, http.MethodPost:
		if len(parts) != 4 {
			pkgbase.Error(w, http.StatusBadRequest, "expected PUT /v1/modules/{namespace}/{name}/{system}/{version}")
			return
		}
		l.publishModule(w, r, parts)
		return
	case http.MethodDelete:
		if len(parts) != 4 {
			pkgbase.NotFound(w)
			return
		}
		l.deleteHTTP(w, path.Join(modulesRoot, parts[0], parts[1], parts[2]), parts[3])
		return
	default:
		pkgbase.Error(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	switch {
	case len(parts) <= 1:
		l.listModules(w, r, parts)
	case len(parts) == 3:
		versions := l.store.ModuleVersions(parts[0], parts[1], parts[2])
		if len(versions) == 0 {
			pkgbase.NotFound(w)
			return
		}
		l.moduleInfo(w, r, parts, pkgbase.Latest(versions), versions)
	case len(parts) == 4 && parts[3] == "versions":
		versions := l.store.ModuleVersions(parts[0], parts[1], parts[2])
		if len(versions) == 0 {
			pkgbase.NotFound(w)
			return
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, moduleVersionsDoc(parts[0], parts[1], parts[2], versions))
	case len(parts) == 4 && parts[3] == "download":
		versions := l.store.ModuleVersions(parts[0], parts[1], parts[2])
		if len(versions) == 0 {
			pkgbase.NotFound(w)
			return
		}
		w.Header().Set("Location", pkgbase.Prefix(r)+"/v1/modules/"+strings.Join(parts[:3], "/")+"/"+pkgbase.Latest(versions)+"/download")
		w.WriteHeader(http.StatusFound)
	case len(parts) == 4:
		if !l.store.isModuleVersion(moduleDir(parts[0], parts[1], parts[2], parts[3])) {
			pkgbase.NotFound(w)
			return
		}
		l.moduleInfo(w, r, parts, parts[3], l.store.ModuleVersions(parts[0], parts[1], parts[2]))
	case len(parts) == 5 && parts[4] == "download":
		_, ext := l.store.moduleArchive(moduleDir(parts[0], parts[1], parts[2], parts[3]))
		if ext == "" {
			pkgbase.NotFound(w)
			return
		}
		w.Header().Set("X-Terraform-Get", moduleArchiveURL(pkgbase.PublicBase(r), parts[0], parts[1], parts[2], parts[3], ext))
		w.WriteHeader(http.StatusNoContent)
	default:
		pkgbase.NotFound(w)
	}
}

func moduleSummary(ns, name, system, version string, published time.Time) map[string]any {
	m := map[string]any{
		"id": ns + "/" + name + "/" + system + "/" + version, "namespace": ns, "name": name,
		"provider": system, "version": version, "verified": false,
	}
	if !published.IsZero() {
		m["published_at"] = published.UTC().Format(time.RFC3339)
	}
	return m
}

func (l *Local) modTime(dir string) time.Time {
	f, _ := l.store.moduleArchive(dir)
	if f == "" {
		return time.Time{}
	}
	fi, err := l.store.Stat(path.Join(dir, f))
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime
}

func (l *Local) moduleInfo(w http.ResponseWriter, r *http.Request, parts []string, version string, versions []string) {
	m := moduleSummary(parts[0], parts[1], parts[2], version, l.modTime(moduleDir(parts[0], parts[1], parts[2], version)))
	desc := make([]string, 0, len(versions))
	for i := len(versions) - 1; i >= 0; i-- {
		desc = append(desc, versions[i])
	}
	m["versions"] = desc
	pkgbase.WriteJSON(w, r, http.StatusOK, m)
}

func (l *Local) listModules(w http.ResponseWriter, r *http.Request, parts []string) {
	q := strings.ToLower(r.URL.Query().Get("q"))
	nsFilter := ""
	if len(parts) == 1 && parts[0] != "search" {
		nsFilter = parts[0]
	}
	pkgs, err := l.store.ListPackages(r.Context())
	if err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	mods := []map[string]any{}
	for _, p := range pkgs {
		kind, seg, ok := parseName(p.Name)
		if !ok || kind != modulesRoot {
			continue
		}
		if nsFilter != "" && seg[0] != nsFilter {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(strings.Join(seg, "/")), q) {
			continue
		}
		latest := pkgbase.Latest(p.Versions)
		mods = append(mods, moduleSummary(seg[0], seg[1], seg[2], latest, l.modTime(moduleDir(seg[0], seg[1], seg[2], latest))))
	}
	pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{
		"meta":    map[string]any{"limit": len(mods), "current_offset": 0},
		"modules": mods,
	})
}

func detectArchive(body []byte) string {
	switch {
	case len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b:
		return "tar.gz"
	case len(body) >= 4 && bytes.Equal(body[:4], []byte("PK\x03\x04")):
		return "zip"
	}
	return ""
}

func (l *Local) publishModule(w http.ResponseWriter, r *http.Request, parts []string) {
	if !l.CheckPush(w) {
		return
	}
	body, err := pkgbase.ReadBody(r.Body, l.MaxUpload)
	if err != nil {
		pkgbase.Error(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	ext := detectArchive(body)
	if ext == "" {
		pkgbase.Error(w, http.StatusBadRequest, "module body must be a .tar.gz or .zip archive")
		return
	}
	dir := moduleDirOf(parts)
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.AllowPublish(w, l.store.isModuleVersion(dir), int64(len(body))) {
		return
	}
	for _, other := range []string{"module.tar.gz", "module.zip"} {
		if other != "module."+ext {
			_ = l.store.Delete(path.Join(dir, other))
		}
	}
	if err := l.store.Write(path.Join(dir, "module."+ext), body); err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	name := path.Join(modulesRoot, parts[0], parts[1], parts[2])
	l.EmitPublished(name+"@"+parts[3], int64(len(body)))
	pkgbase.WriteJSON(w, r, http.StatusCreated, map[string]any{
		"module": strings.Join(parts[:3], "/"), "version": parts[3], "size": len(body), "sha256": pkgbase.SHA256Hex(body),
	})
}

func (l *Local) deleteHTTP(w http.ResponseWriter, name, version string) {
	if !l.CheckPush(w) {
		return
	}
	l.mu.Lock()
	err := l.DeleteVersion(context.Background(), name, version)
	l.mu.Unlock()
	if err != nil {
		if pkgbase.IsNotFound(err) {
			pkgbase.NotFound(w)
			return
		}
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── providers ──

func (l *Local) serveProviders(w http.ResponseWriter, r *http.Request) {
	parts, ok := splitRest(r.URL.Path, "/v1/providers/")
	if !ok || len(parts) < 2 {
		pkgbase.NotFound(w)
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
	case http.MethodPut, http.MethodPost:
		l.publishProvider(w, r, parts)
		return
	case http.MethodDelete:
		l.deleteProvider(w, r, parts)
		return
	default:
		pkgbase.Error(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	switch {
	case len(parts) == 3 && parts[2] == "versions":
		l.providerVersions(w, r, parts[0], parts[1])
	case len(parts) == 6 && parts[3] == "download":
		l.providerDownload(w, r, parts[0], parts[1], parts[2], parts[4], parts[5])
	default:
		pkgbase.NotFound(w)
	}
}

func (l *Local) providerVersions(w http.ResponseWriter, r *http.Request, ns, ptype string) {
	versions := l.store.ProviderVersions(ns, ptype)
	if len(versions) == 0 {
		pkgbase.NotFound(w)
		return
	}
	out := make([]map[string]any, 0, len(versions))
	for i := len(versions) - 1; i >= 0; i-- {
		m, err := l.store.providerMeta(providerDir(ns, ptype, versions[i]))
		if err != nil {
			continue
		}
		plats := make([]map[string]string, 0, len(m.Platforms))
		for _, p := range m.Platforms {
			plats = append(plats, map[string]string{"os": p.OS, "arch": p.Arch})
		}
		out = append(out, map[string]any{"version": versions[i], "protocols": m.Protocols, "platforms": plats})
	}
	pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{"id": ns + "/" + ptype, "versions": out, "warnings": nil})
}

// signature returns SHA256SUMS, its detached signature and the
// armored public key for a provider version.
func (l *Local) signature(dir string, m *providerMeta) (sums, sig, pub []byte, err error) {
	sums, err = l.store.Read(path.Join(dir, uploadedSums))
	if err != nil {
		sums = m.sums()
	}
	if sig, err = l.store.Read(path.Join(dir, uploadedSig)); err == nil {
		pub, _ = l.store.Read(path.Join(dir, uploadedKey))
		if len(pub) == 0 && l.key != nil {
			pub, _ = l.key.PublicKeyArmored()
		}
		return sums, sig, pub, nil
	}
	if l.key == nil {
		return sums, nil, nil, nil
	}
	if sig, err = l.key.DetachSignBinary(sums); err != nil {
		return nil, nil, nil, err
	}
	if pub, err = l.key.PublicKeyArmored(); err != nil {
		return nil, nil, nil, err
	}
	return sums, sig, pub, nil
}

func (l *Local) providerDownload(w http.ResponseWriter, r *http.Request, ns, ptype, ver, osName, arch string) {
	dir := providerDir(ns, ptype, ver)
	m, err := l.store.providerMeta(dir)
	if err != nil {
		pkgbase.NotFound(w)
		return
	}
	p := m.platform(osName, arch)
	if p == nil {
		pkgbase.NotFound(w)
		return
	}
	_, _, pub, err := l.signature(dir, m)
	if err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	keys := []map[string]string{}
	if len(pub) > 0 {
		id, _ := armoredKeyID(pub)
		keys = append(keys, map[string]string{"key_id": id, "ascii_armor": string(pub)})
	}
	base := providerArchiveBase(pkgbase.PublicBase(r), ns, ptype, ver)
	pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{
		"protocols":             m.Protocols,
		"os":                    p.OS,
		"arch":                  p.Arch,
		"filename":              p.Filename,
		"download_url":          base + "/" + p.Filename,
		"shasums_url":           base + "/" + sumsName(ptype, ver),
		"shasums_signature_url": base + "/" + sumsName(ptype, ver) + ".sig",
		"shasum":                p.SHASum,
		"signing_keys":          map[string]any{"gpg_public_keys": keys},
	})
}

func parseProtocols(r *http.Request) []string {
	raw := r.URL.Query().Get("protocols")
	if raw == "" {
		raw = r.Header.Get("X-Terraform-Protocols")
	}
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (l *Local) publishProvider(w http.ResponseWriter, r *http.Request, parts []string) {
	if !l.CheckPush(w) {
		return
	}
	body, err := pkgbase.ReadBody(r.Body, l.MaxUpload)
	if err != nil {
		pkgbase.Error(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	switch {
	case len(parts) == 4:
		l.publishProviderSidecar(w, r, parts, body)
	case len(parts) == 5:
		l.publishProviderZip(w, r, parts, body)
	default:
		pkgbase.Error(w, http.StatusBadRequest, "expected PUT /v1/providers/{namespace}/{type}/{version}/{os}/{arch}")
	}
}

func (l *Local) publishProviderZip(w http.ResponseWriter, r *http.Request, parts []string, body []byte) {
	ns, ptype, ver, osName, arch := parts[0], parts[1], parts[2], parts[3], parts[4]
	if _, err := zip.NewReader(bytes.NewReader(body), int64(len(body))); err != nil {
		pkgbase.Error(w, http.StatusBadRequest, "provider body must be a zip archive")
		return
	}
	dir := providerDir(ns, ptype, ver)
	l.mu.Lock()
	defer l.mu.Unlock()
	m, err := l.store.providerMeta(dir)
	if err != nil {
		m = &providerMeta{}
	}
	if !l.AllowPublish(w, m.platform(osName, arch) != nil, int64(len(body))) {
		return
	}
	file := zipName(ptype, ver, osName, arch)
	if err := l.store.Write(path.Join(dir, file), body); err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if protos := parseProtocols(r); len(protos) > 0 {
		m.Protocols = protos
	} else if len(m.Protocols) == 0 {
		m.Protocols = defaultProtocols
	}
	plat := providerPlatform{OS: osName, Arch: arch, Filename: file, SHASum: pkgbase.SHA256Hex(body), Size: int64(len(body))}
	if p := m.platform(osName, arch); p != nil {
		*p = plat
	} else {
		m.Platforms = append(m.Platforms, plat)
	}
	m.PublishedAt = time.Now().UTC()
	// The publisher's sums/signature no longer cover the new set of zips.
	_ = l.store.Delete(path.Join(dir, uploadedSums))
	_ = l.store.Delete(path.Join(dir, uploadedSig))
	if err := l.store.writeProviderMeta(dir, m); err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	l.EmitPublished(path.Join(providersRoot, ns, ptype)+"@"+ver+"/"+osName+"_"+arch, int64(len(body)))
	pkgbase.WriteJSON(w, r, http.StatusCreated, map[string]any{
		"provider": ns + "/" + ptype, "version": ver, "os": osName, "arch": arch,
		"filename": file, "shasum": plat.SHASum, "protocols": m.Protocols,
	})
}

func (l *Local) publishProviderSidecar(w http.ResponseWriter, r *http.Request, parts []string, body []byte) {
	ns, ptype, ver := parts[0], parts[1], parts[2]
	dir := providerDir(ns, ptype, ver)
	var target string
	switch parts[3] {
	case "SHA256SUMS", sumsName(ptype, ver):
		target = uploadedSums
	case "SHA256SUMS.sig", sumsName(ptype, ver) + ".sig":
		target = uploadedSig
	case "signing-key", "signing-key.asc":
		if _, err := armoredKeyID(body); err != nil {
			pkgbase.Error(w, http.StatusBadRequest, "signing-key must be an ASCII-armored OpenPGP public key: "+err.Error())
			return
		}
		target = uploadedKey
	default:
		pkgbase.Error(w, http.StatusBadRequest, "expected SHA256SUMS, SHA256SUMS.sig or signing-key")
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.store.isProviderVersion(dir) {
		pkgbase.Error(w, http.StatusNotFound, "upload the provider zips before "+parts[3])
		return
	}
	if !l.AllowPublish(w, false, int64(len(body))) {
		return
	}
	if err := l.store.Write(path.Join(dir, target), body); err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	pkgbase.WriteJSON(w, r, http.StatusCreated, map[string]any{"provider": ns + "/" + ptype, "version": ver, "file": parts[3]})
}

func (l *Local) deleteProvider(w http.ResponseWriter, r *http.Request, parts []string) {
	switch len(parts) {
	case 3:
		l.deleteHTTP(w, path.Join(providersRoot, parts[0], parts[1]), parts[2])
	case 5:
		if !l.CheckPush(w) {
			return
		}
		dir := providerDir(parts[0], parts[1], parts[2])
		l.mu.Lock()
		defer l.mu.Unlock()
		m, err := l.store.providerMeta(dir)
		if err != nil || m.platform(parts[3], parts[4]) == nil {
			pkgbase.NotFound(w)
			return
		}
		p := m.platform(parts[3], parts[4])
		_ = l.store.Delete(path.Join(dir, p.Filename))
		kept := m.Platforms[:0]
		for _, q := range m.Platforms {
			if q.OS != parts[3] || q.Arch != parts[4] {
				kept = append(kept, q)
			}
		}
		m.Platforms = kept
		_ = l.store.Delete(path.Join(dir, uploadedSums))
		_ = l.store.Delete(path.Join(dir, uploadedSig))
		if len(m.Platforms) == 0 {
			l.store.DeleteTree(dir)
		} else if err := l.store.writeProviderMeta(dir, m); err != nil {
			pkgbase.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		l.EmitDeleted(path.Join(providersRoot, parts[0], parts[1]) + "@" + parts[2] + "/" + parts[3] + "_" + parts[4])
		w.WriteHeader(http.StatusNoContent)
	default:
		pkgbase.NotFound(w)
	}
}

// ── archives ──

func (l *Local) serveArchive(w http.ResponseWriter, r *http.Request) {
	if parts, ok := splitRest(r.URL.Path, "/archive/modules/"); ok && len(parts) == 4 {
		ver, ext := splitArchiveName(parts[3])
		dir := moduleDir(parts[0], parts[1], parts[2], ver)
		if ver != "" && pkgbase.ServeStored(w, r, l.store.Store, path.Join(dir, "module."+ext), archiveType(ext)) {
			return
		}
		pkgbase.NotFound(w)
		return
	}
	parts, ok := splitRest(r.URL.Path, "/archive/providers/")
	if !ok || len(parts) != 4 {
		pkgbase.NotFound(w)
		return
	}
	ns, ptype, ver, file := parts[0], parts[1], parts[2], parts[3]
	dir := providerDir(ns, ptype, ver)
	m, err := l.store.providerMeta(dir)
	if err != nil {
		pkgbase.NotFound(w)
		return
	}
	switch file {
	case sumsName(ptype, ver), sumsName(ptype, ver) + ".sig":
		sums, sig, _, err := l.signature(dir, m)
		if err != nil {
			pkgbase.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		if strings.HasSuffix(file, ".sig") {
			if sig == nil {
				pkgbase.NotFound(w)
				return
			}
			pkgbase.WriteBytes(w, r, http.StatusOK, "application/octet-stream", sig)
			return
		}
		pkgbase.WriteBytes(w, r, http.StatusOK, "text/plain; charset=utf-8", sums)
	default:
		for _, p := range m.Platforms {
			if p.Filename == file && pkgbase.ServeStored(w, r, l.store.Store, path.Join(dir, file), "application/zip") {
				return
			}
		}
		pkgbase.NotFound(w)
	}
}

func archiveType(ext string) string {
	if ext == "zip" {
		return "application/zip"
	}
	return "application/gzip"
}
