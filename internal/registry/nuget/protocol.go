package nuget

import (
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
)

const (
	flatPrefix       = "/v3/flatcontainer/"
	regPrefix        = "/v3/registration/"
	searchPath       = "/v3/search"
	autocompletePath = "/v3/autocomplete"
	publishPath      = "/api/v2/package"
)

// unlistedPublished is the publish date NuGet uses to flag unlisted
// versions to older clients.
var unlistedPublished = time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)

func isServiceIndex(p string) bool {
	switch strings.TrimRight(p, "/") {
	case "", "/index.json", "/v3/index.json", "/v3":
		return true
	}
	return false
}

func serviceIndex(base string) map[string]any {
	var res []map[string]string
	add := func(id, comment string, types ...string) {
		for _, t := range types {
			res = append(res, map[string]string{"@id": id, "@type": t, "comment": comment})
		}
	}
	add(base+flatPrefix, "Base URL of where NuGet packages are stored, in the format {base}/{id-lower}/{version-lower}/{id-lower}.{version-lower}.nupkg",
		"PackageBaseAddress/3.0.0")
	add(base+regPrefix, "Base URL of the package registration (metadata) documents",
		"RegistrationsBaseUrl", "RegistrationsBaseUrl/3.0.0-rc", "RegistrationsBaseUrl/3.0.0-beta",
		"RegistrationsBaseUrl/3.4.0", "RegistrationsBaseUrl/3.6.0")
	add(base+searchPath, "Query endpoint of the NuGet search service",
		"SearchQueryService", "SearchQueryService/3.0.0-rc", "SearchQueryService/3.0.0-beta", "SearchQueryService/3.5.0")
	add(base+autocompletePath, "Autocomplete endpoint of the NuGet search service",
		"SearchAutocompleteService", "SearchAutocompleteService/3.0.0-rc", "SearchAutocompleteService/3.0.0-beta", "SearchAutocompleteService/3.5.0")
	add(base+publishPath, "Push, delete and relist endpoint", "PackagePublish/2.0.0")
	return map[string]any{"version": "3.0.0", "resources": res}
}

func regIndexURL(base, lid string) string { return base + regPrefix + lid + "/index.json" }

func leafURL(base, lid, lver string) string { return base + regPrefix + lid + "/" + lver + ".json" }

func nupkgURL(base, lid, lver string) string {
	return base + flatPrefix + lid + "/" + lver + "/" + lid + "." + lver + ".nupkg"
}

func publishedOf(m *Meta) string {
	t := m.Published
	switch {
	case !m.Listed:
		t = unlistedPublished
	case t.IsZero():
		t = time.Unix(0, 0)
	}
	return t.UTC().Format(time.RFC3339)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func catalogEntry(base string, m *Meta) map[string]any {
	lid, lver := strings.ToLower(m.ID), strings.ToLower(m.Version)
	groups := make([]map[string]any, 0, len(m.DependencyGroups))
	for i, g := range m.DependencyGroups {
		gid := leafURL(base, lid, lver) + "#dependencygroup/" + strconv.Itoa(i)
		deps := make([]map[string]any, 0, len(g.Dependencies))
		for _, d := range g.Dependencies {
			deps = append(deps, map[string]any{
				"@id":          gid + "/" + strings.ToLower(d.ID),
				"@type":        "PackageDependency",
				"id":           d.ID,
				"range":        d.Range,
				"registration": regIndexURL(base, strings.ToLower(d.ID)),
			})
		}
		grp := map[string]any{"@id": gid, "@type": "PackageDependencyGroup", "dependencies": deps}
		if g.TargetFramework != "" {
			grp["targetFramework"] = g.TargetFramework
		}
		groups = append(groups, grp)
	}
	e := map[string]any{
		"@id":                      leafURL(base, lid, lver),
		"@type":                    "PackageDetails",
		"id":                       m.ID,
		"version":                  m.Version,
		"authors":                  m.Authors,
		"description":              m.Description,
		"summary":                  m.Summary,
		"title":                    m.Title,
		"tags":                     nonNil(m.Tags),
		"iconUrl":                  m.IconURL,
		"licenseUrl":               m.LicenseURL,
		"licenseExpression":        m.LicenseExpression,
		"projectUrl":               m.ProjectURL,
		"language":                 m.Language,
		"requireLicenseAcceptance": m.RequireLicenseAcceptance,
		"listed":                   m.Listed,
		"published":                publishedOf(m),
		"packageContent":           nupkgURL(base, lid, lver),
		"dependencyGroups":         groups,
	}
	if m.MinClientVersion != "" {
		e["minClientVersion"] = m.MinClientVersion
	}
	if m.SHA512 != "" {
		e["packageHash"] = m.SHA512
		e["packageHashAlgorithm"] = "SHA512"
	}
	if m.Size > 0 {
		e["packageSize"] = m.Size
	}
	return e
}

func registrationLeafItem(base string, m *Meta) map[string]any {
	lid, lver := strings.ToLower(m.ID), strings.ToLower(m.Version)
	return map[string]any{
		"@id":             leafURL(base, lid, lver),
		"@type":           "Package",
		"commitId":        "00000000-0000-0000-0000-000000000000",
		"commitTimeStamp": publishedOf(m),
		"catalogEntry":    catalogEntry(base, m),
		"packageContent":  nupkgURL(base, lid, lver),
		"registration":    regIndexURL(base, lid),
	}
}

// registrationIndex builds a registration index with one inlined page
// (metas sorted ascending).
func registrationIndex(base string, metas []*Meta) map[string]any {
	lid := strings.ToLower(metas[0].ID)
	idx := regIndexURL(base, lid)
	items := make([]map[string]any, 0, len(metas))
	for _, m := range metas {
		items = append(items, registrationLeafItem(base, m))
	}
	lower, upper := metas[0].Version, metas[len(metas)-1].Version
	page := map[string]any{
		"@id":    idx + "#page/" + strings.ToLower(lower) + "/" + strings.ToLower(upper),
		"@type":  "catalog:CatalogPage",
		"count":  len(items),
		"lower":  lower,
		"upper":  upper,
		"parent": idx,
		"items":  items,
	}
	return map[string]any{
		"@id":   idx,
		"@type": []string{"catalog:CatalogRoot", "PackageRegistration", "catalog:Permalink"},
		"count": 1,
		"items": []any{page},
	}
}

func registrationLeaf(base string, m *Meta) map[string]any {
	lid, lver := strings.ToLower(m.ID), strings.ToLower(m.Version)
	return map[string]any{
		"@id":            leafURL(base, lid, lver),
		"@type":          []string{"Package", "http://schema.nuget.org/catalog#Permalink"},
		"catalogEntry":   leafURL(base, lid, lver),
		"listed":         m.Listed,
		"published":      publishedOf(m),
		"packageContent": nupkgURL(base, lid, lver),
		"registration":   regIndexURL(base, lid),
	}
}

// pkgVersions is one package id with its versions (ascending).
type pkgVersions struct {
	ID    string
	Metas []*Meta
}

func intParam(q url.Values, key string, def, max int) int {
	n, err := strconv.Atoi(q.Get(key))
	if err != nil || n < 0 {
		return def
	}
	if max > 0 && n > max {
		return max
	}
	return n
}

func boolParam(q url.Values, key string) bool {
	b, _ := strconv.ParseBool(q.Get(key))
	return b
}

func skipTake(q url.Values) (int, int) {
	return intParam(q, "skip", 0, 0), intParam(q, "take", 20, 1000)
}

func page[T any](in []T, skip, take int) []T {
	if skip >= len(in) {
		return []T{}
	}
	end := len(in)
	if skip+take < end {
		end = skip + take
	}
	return in[skip:end]
}

// visible returns the listed versions matching the prerelease filter.
func visible(metas []*Meta, prerelease bool) []*Meta {
	var out []*Meta
	for _, m := range metas {
		if !m.Listed || (!prerelease && isPrerelease(m.Version)) {
			continue
		}
		out = append(out, m)
	}
	return out
}

func matches(m *Meta, q string) bool {
	if q == "" {
		return true
	}
	if v, ok := strings.CutPrefix(q, "packageid:"); ok {
		return strings.EqualFold(m.ID, strings.TrimSpace(v))
	}
	if v, ok := strings.CutPrefix(q, "id:"); ok {
		q = strings.TrimSpace(v)
		return strings.Contains(strings.ToLower(m.ID), q)
	}
	for _, term := range strings.Fields(q) {
		hit := strings.Contains(strings.ToLower(m.ID), term) || strings.Contains(strings.ToLower(m.Title), term)
		for _, t := range m.Tags {
			hit = hit || strings.Contains(strings.ToLower(t), term)
		}
		if !hit {
			return false
		}
	}
	return true
}

// searchPackages filters pkgs by the search query parameters and
// returns the filtered set (before paging) with the latest meta.
func searchPackages(pkgs []pkgVersions, q url.Values) []pkgVersions {
	term := strings.ToLower(strings.TrimSpace(q.Get("q")))
	prerelease := boolParam(q, "prerelease")
	var out []pkgVersions
	for _, p := range pkgs {
		vs := visible(p.Metas, prerelease)
		if len(vs) == 0 || !matches(vs[len(vs)-1], term) {
			continue
		}
		out = append(out, pkgVersions{ID: vs[len(vs)-1].ID, Metas: vs})
	}
	sort.SliceStable(out, func(i, j int) bool {
		ei, ej := strings.EqualFold(out[i].ID, term), strings.EqualFold(out[j].ID, term)
		if ei != ej {
			return ei
		}
		return strings.ToLower(out[i].ID) < strings.ToLower(out[j].ID)
	})
	return out
}

func searchResponse(base string, pkgs []pkgVersions, q url.Values) map[string]any {
	hits := searchPackages(pkgs, q)
	skip, take := skipTake(q)
	data := make([]map[string]any, 0)
	for _, p := range page(hits, skip, take) {
		latest := p.Metas[len(p.Metas)-1]
		lid := strings.ToLower(latest.ID)
		versions := make([]map[string]any, 0, len(p.Metas))
		for _, m := range p.Metas {
			versions = append(versions, map[string]any{
				"version":   m.Version,
				"downloads": 0,
				"@id":       leafURL(base, lid, strings.ToLower(m.Version)),
			})
		}
		var authors []string
		for _, a := range strings.Split(latest.Authors, ",") {
			if a = strings.TrimSpace(a); a != "" {
				authors = append(authors, a)
			}
		}
		data = append(data, map[string]any{
			"@id":            regIndexURL(base, lid),
			"@type":          "Package",
			"registration":   regIndexURL(base, lid),
			"id":             latest.ID,
			"version":        latest.Version,
			"description":    latest.Description,
			"summary":        latest.Summary,
			"title":          latest.Title,
			"iconUrl":        latest.IconURL,
			"licenseUrl":     latest.LicenseURL,
			"projectUrl":     latest.ProjectURL,
			"tags":           nonNil(latest.Tags),
			"authors":        nonNil(authors),
			"owners":         nonNil(splitTags(latest.Owners)),
			"totalDownloads": 0,
			"verified":       false,
			"packageTypes":   []map[string]string{{"name": "Dependency"}},
			"versions":       versions,
		})
	}
	return map[string]any{"totalHits": len(hits), "data": data}
}

func autocompleteResponse(pkgs []pkgVersions, q url.Values) map[string]any {
	prerelease := boolParam(q, "prerelease")
	if id := strings.TrimSpace(q.Get("id")); id != "" {
		data := []string{}
		for _, p := range pkgs {
			if !strings.EqualFold(p.ID, id) {
				continue
			}
			for _, m := range visible(p.Metas, prerelease) {
				data = append(data, m.Version)
			}
		}
		return map[string]any{"totalHits": len(data), "data": data}
	}
	hits := searchPackages(pkgs, q)
	ids := make([]string, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.ID)
	}
	skip, take := skipTake(q)
	return map[string]any{"totalHits": len(ids), "data": page(ids, skip, take)}
}

func serveServiceIndex(w http.ResponseWriter, r *http.Request) {
	pkgbase.WriteJSON(w, r, http.StatusOK, serviceIndex(pkgbase.PublicBase(r)))
}

// splitFlat splits the path after flatPrefix into its segments.
func splitFlat(p string) []string {
	rest := strings.Trim(strings.TrimPrefix(p, flatPrefix), "/")
	if rest == "" {
		return nil
	}
	return strings.Split(rest, "/")
}
