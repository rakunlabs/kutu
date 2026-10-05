package nuget

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/service"
)

// Mutable cache directories of a remote repo.
const (
	cacheFlat = "flat"
	cacheReg  = "reg"
	cacheMeta = "meta"
)

// discovery holds the resource URLs of the upstream service index.
type discovery struct {
	pba          string
	reg          string
	regAll       []string
	search       string
	autocomplete string
	fetched      time.Time
}

// Remote is a pull-through proxy of an upstream NuGet V3 feed.
type Remote struct {
	*pkgbase.RemoteRepo
	store *Store

	indexURLs []string

	mu   sync.Mutex
	disc *discovery
}

// serviceIndexCandidates returns the upstream service index URLs to try.
func serviceIndexCandidates(raw string) []string {
	u := strings.TrimRight(strings.TrimSpace(raw), "/")
	if strings.HasSuffix(strings.ToLower(u), ".json") {
		return []string{u}
	}
	if strings.HasSuffix(u, "/v3") {
		return []string{u + "/index.json"}
	}
	return []string{u + "/v3/index.json", u + "/index.json"}
}

// NewRemoteFactory returns the remote NuGet factory.
func NewRemoteFactory() registry.Factory {
	return func(_ context.Context, deps registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		cands := serviceIndexCandidates(r.URL)
		base, err := pkgbase.NewRemoteRepo(deps, typ, ns, r, cands[0])
		if err != nil {
			return nil, err
		}
		return &Remote{RemoteRepo: base, store: &Store{base.Store}, indexURLs: cands}, nil
	}
}

func (rr *Remote) Store() *Store { return rr.store }

func withSlash(s string) string {
	if s == "" || strings.HasSuffix(s, "/") {
		return s
	}
	return s + "/"
}

func pickResource(res []struct {
	ID   string `json:"@id"`
	Type any    `json:"@type"`
}, prefs ...string) string {
	for _, want := range prefs {
		for _, r := range res {
			for _, t := range typeStrings(r.Type) {
				if t == want {
					return r.ID
				}
			}
		}
	}
	return ""
}

func typeStrings(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func parseServiceIndex(body []byte) (*discovery, error) {
	var doc struct {
		Resources []struct {
			ID   string `json:"@id"`
			Type any    `json:"@type"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	d := &discovery{
		pba: withSlash(pickResource(doc.Resources, "PackageBaseAddress/3.0.0")),
		reg: withSlash(pickResource(doc.Resources, "RegistrationsBaseUrl/3.6.0", "RegistrationsBaseUrl/3.4.0",
			"RegistrationsBaseUrl/Versioned", "RegistrationsBaseUrl/3.0.0-rc", "RegistrationsBaseUrl/3.0.0-beta", "RegistrationsBaseUrl")),
		search: pickResource(doc.Resources, "SearchQueryService/3.5.0", "SearchQueryService/3.0.0-rc",
			"SearchQueryService/3.0.0-beta", "SearchQueryService"),
		autocomplete: pickResource(doc.Resources, "SearchAutocompleteService/3.5.0", "SearchAutocompleteService/3.0.0-rc",
			"SearchAutocompleteService/3.0.0-beta", "SearchAutocompleteService"),
	}
	for _, r := range doc.Resources {
		for _, t := range typeStrings(r.Type) {
			if strings.HasPrefix(t, "RegistrationsBaseUrl") && r.ID != "" {
				d.regAll = append(d.regAll, withSlash(r.ID))
				break
			}
		}
	}
	if d.pba == "" {
		return nil, errors.New("upstream service index has no PackageBaseAddress/3.0.0 resource")
	}
	return d, nil
}

// discover returns the upstream resource URLs (memory cached for
// MutableTTL, at least one minute; disk cached under meta/).
func (rr *Remote) discover(ctx context.Context) (*discovery, error) {
	ttl := rr.MutableTTL
	if ttl < time.Minute {
		ttl = time.Minute
	}
	rr.mu.Lock()
	d := rr.disc
	rr.mu.Unlock()
	if d != nil && time.Since(d.fetched) < ttl {
		return d, nil
	}
	var lastErr error
	for _, u := range rr.indexURLs {
		body, err := rr.FetchCached(ctx, path.Join(cacheMeta, "service-index.json"), u, true)
		if err != nil {
			lastErr = err
			continue
		}
		nd, err := parseServiceIndex(body)
		if err != nil {
			lastErr = err
			_ = rr.store.Delete(path.Join(cacheMeta, "service-index.json"))
			continue
		}
		nd.fetched = time.Now()
		rr.mu.Lock()
		rr.disc = nd
		rr.mu.Unlock()
		return nd, nil
	}
	if d != nil {
		return d, nil
	}
	return nil, lastErr
}

// rewrite maps upstream registration / package URLs onto kutu.
func (d *discovery) rewrite(body []byte, base string) []byte {
	for _, reg := range d.regAll {
		body = bytes.ReplaceAll(body, []byte(reg), []byte(base+regPrefix))
	}
	return bytes.ReplaceAll(body, []byte(d.pba), []byte(base+flatPrefix))
}

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
	rr.mu.Lock()
	rr.disc = nil
	rr.mu.Unlock()
	return rr.Purge(opts, cacheFlat, cacheReg, cacheMeta), nil
}

func (rr *Remote) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) { return classify(r) }

// catalogEntryFor finds the upstream registration catalog entry of lid@lver.
func (rr *Remote) catalogEntryFor(ctx context.Context, lid, lver string) (map[string]any, error) {
	d, err := rr.discover(ctx)
	if err != nil {
		return nil, err
	}
	if d.reg == "" {
		return nil, registry.ErrPackageNotFound
	}
	body, err := rr.FetchCached(ctx, path.Join(cacheReg, lid, "index.json"), d.reg+lid+"/index.json", true)
	if err != nil {
		return nil, err
	}
	var idx struct {
		Items []struct {
			ID    string           `json:"@id"`
			Lower string           `json:"lower"`
			Upper string           `json:"upper"`
			Items []map[string]any `json:"items"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &idx); err != nil {
		return nil, err
	}
	for _, pg := range idx.Items {
		if pg.Lower != "" && pkgbase.CompareVersions(lver, strings.ToLower(pg.Lower)) < 0 {
			continue
		}
		if pg.Upper != "" && pkgbase.CompareVersions(lver, strings.ToLower(pg.Upper)) > 0 {
			continue
		}
		items := pg.Items
		if items == nil && strings.HasPrefix(pg.ID, d.reg) {
			pb, err := rr.FetchCached(ctx, path.Join(cacheReg, strings.TrimPrefix(pg.ID, d.reg)), pg.ID, true)
			if err != nil {
				return nil, err
			}
			var page struct {
				Items []map[string]any `json:"items"`
			}
			if err := json.Unmarshal(pb, &page); err != nil {
				return nil, err
			}
			items = page.Items
		}
		for _, leaf := range items {
			ce, _ := leaf["catalogEntry"].(map[string]any)
			if ce == nil {
				continue
			}
			v, _ := ce["version"].(string)
			if lv, ok := lowerVersion(v); ok && lv == lver {
				return ce, nil
			}
		}
	}
	return nil, registry.ErrPackageNotFound
}

func metaFromCatalog(ce map[string]any) registry.ArtifactMeta {
	var out registry.ArtifactMeta
	if s, _ := ce["published"].(string); s != "" {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil && t.Year() > 1900 {
			out.PublishedAt = t
		}
	}
	if s, _ := ce["licenseExpression"].(string); s != "" {
		out.License = s
	} else if s, _ := ce["licenseUrl"].(string); s != "" && s != deprecatedLicenseURL {
		out.License = s
	}
	return out
}

// ArtifactInfo reads the upstream registration (falling back to the
// cached package metadata).
func (rr *Remote) ArtifactInfo(ctx context.Context, ref registry.ArtifactRef) (registry.ArtifactMeta, error) {
	lid := strings.ToLower(ref.Name)
	lver, ok := lowerVersion(ref.Version)
	if !ok || !ValidID(lid) {
		return registry.ArtifactMeta{}, registry.ErrPackageNotFound
	}
	if ce, err := rr.catalogEntryFor(ctx, lid, lver); err == nil {
		return metaFromCatalog(ce), nil
	}
	return artifactInfo(rr.store, ref)
}

// fetchNupkg returns the cached nupkg of lid@lver, downloading and
// indexing it on first use.
func (rr *Remote) fetchNupkg(ctx context.Context, lid, lver string) ([]byte, error) {
	if b, err := rr.store.Read(rr.store.nupkgRel(lid, lver)); err == nil && rr.store.Exists(rr.store.metaRel(lid, lver)) {
		return b, nil
	}
	d, err := rr.discover(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := rr.Client.Get(ctx, d.pba+lid+"/"+lver+"/"+lid+"."+lver+".nupkg")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var published time.Time
	if ce, err := rr.catalogEntryFor(ctx, lid, lver); err == nil {
		published = metaFromCatalog(ce).PublishedAt
	}
	_, _ = rr.store.storeNupkg(body, published, lid, lver)
	return body, nil
}

// Prefetch warms the version list, registration and the nupkg of
// version (or the latest version).
func (rr *Remote) Prefetch(ctx context.Context, name, version string) error {
	lid := strings.ToLower(strings.TrimSpace(name))
	if !ValidID(lid) {
		return registry.ErrInvalidPackageName
	}
	d, err := rr.discover(ctx)
	if err != nil {
		return err
	}
	body, err := rr.FetchCached(ctx, path.Join(cacheFlat, lid, "index.json"), d.pba+lid+"/index.json", true)
	if err != nil {
		return err
	}
	if version == "" {
		var idx struct {
			Versions []string `json:"versions"`
		}
		if err := json.Unmarshal(body, &idx); err != nil {
			return err
		}
		var stable []string
		for _, v := range idx.Versions {
			if !isPrerelease(v) {
				stable = append(stable, v)
			}
		}
		if version = pkgbase.Latest(stable); version == "" {
			version = pkgbase.Latest(idx.Versions)
		}
		if version == "" {
			return registry.ErrPackageNotFound
		}
	}
	lver, ok := lowerVersion(version)
	if !ok {
		return registry.ErrPackageNotFound
	}
	_, err = rr.fetchNupkg(ctx, lid, lver)
	return err
}

func (rr *Remote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !pkgbase.IsRead(r) {
		pkgbase.Error(w, http.StatusMethodNotAllowed, "remote registry is read-only")
		return
	}
	p := r.URL.Path
	if isServiceIndex(p) {
		serveServiceIndex(w, r)
		return
	}
	d, err := rr.discover(r.Context())
	if err != nil {
		if strings.HasPrefix(p, flatPrefix) || strings.HasPrefix(p, regPrefix) {
			serveRead(w, r, rr.store)
			return
		}
		pkgbase.UpstreamError(w, err)
		return
	}
	base := pkgbase.PublicBase(r)
	switch {
	case strings.HasPrefix(p, flatPrefix):
		rr.serveFlat(w, r, d)
	case strings.HasPrefix(p, regPrefix):
		sub := strings.TrimPrefix(p, regPrefix)
		id, _, _ := strings.Cut(sub, "/")
		if d.reg == "" || !ValidID(id) || strings.Contains(sub, "..") {
			serveRead(w, r, rr.store)
			return
		}
		lsub := strings.ToLower(sub)
		body, err := rr.FetchCached(r.Context(), path.Join(cacheReg, lsub), d.reg+lsub, true)
		if err != nil {
			pkgbase.UpstreamError(w, err)
			return
		}
		pkgbase.WriteBytes(w, r, http.StatusOK, "application/json", d.rewrite(body, base))
	case strings.TrimRight(p, "/") == searchPath:
		rr.proxySearch(w, r, d.search, d, base, false)
	case strings.TrimRight(p, "/") == autocompletePath:
		rr.proxySearch(w, r, d.autocomplete, d, base, true)
	default:
		pkgbase.NotFound(w)
	}
}

func (rr *Remote) serveFlat(w http.ResponseWriter, r *http.Request, d *discovery) {
	parts := splitFlat(r.URL.Path)
	if len(parts) == 0 || !ValidID(parts[0]) {
		pkgbase.NotFound(w)
		return
	}
	lid := strings.ToLower(parts[0])
	switch {
	case len(parts) == 2 && parts[1] == "index.json":
		rr.ServeCached(w, r, path.Join(cacheFlat, lid, "index.json"), d.pba+lid+"/index.json", "application/json", true)
	case len(parts) == 3:
		lver, ok := lowerVersion(parts[1])
		if !ok {
			pkgbase.NotFound(w)
			return
		}
		file := strings.ToLower(parts[2])
		switch {
		case strings.HasSuffix(file, ".nupkg"):
			if rr.store.Exists(rr.store.metaRel(lid, lver)) &&
				pkgbase.ServeStored(w, r, rr.store.Store, rr.store.nupkgRel(lid, lver), "application/octet-stream") {
				return
			}
			body, err := rr.fetchNupkg(r.Context(), lid, lver)
			if err != nil {
				pkgbase.UpstreamError(w, err)
				return
			}
			pkgbase.WriteBytes(w, r, http.StatusOK, "application/octet-stream", body)
		case strings.HasSuffix(file, ".nuspec"):
			rr.ServeCached(w, r, rr.store.nuspecRel(lid, lver), d.pba+lid+"/"+lver+"/"+lid+".nuspec", "application/xml", false)
		case file == "icon" || file == "readme" || file == "license":
			rr.ServeCached(w, r, path.Join(rr.store.versionDir(lid, lver), "extra-"+file), d.pba+lid+"/"+lver+"/"+file, "", false)
		default:
			pkgbase.NotFound(w)
		}
	default:
		pkgbase.NotFound(w)
	}
}

// proxySearch forwards a search / autocomplete query upstream, falling
// back to the cached packages when the upstream is unavailable.
func (rr *Remote) proxySearch(w http.ResponseWriter, r *http.Request, target string, d *discovery, base string, auto bool) {
	if target != "" {
		u := target
		if r.URL.RawQuery != "" {
			u += "?" + r.URL.RawQuery
		}
		resp, err := rr.Client.Get(r.Context(), u)
		if err == nil {
			body, rerr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if rerr == nil {
				pkgbase.WriteBytes(w, r, http.StatusOK, "application/json", d.rewrite(body, base))
				return
			}
		}
	}
	pkgs, err := rr.store.all()
	if err != nil {
		pkgbase.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	if auto {
		pkgbase.WriteJSON(w, r, http.StatusOK, autocompleteResponse(pkgs, r.URL.Query()))
		return
	}
	pkgbase.WriteJSON(w, r, http.StatusOK, searchResponse(base, pkgs, r.URL.Query()))
}
