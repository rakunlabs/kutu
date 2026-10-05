package nuget

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/registry/virtualbase"
	"github.com/rakunlabs/kutu/internal/service"
)

// Virtual aggregates NuGet members: version lists, registration
// indexes, search and autocomplete are merged; everything else is
// served first-hit.
type Virtual struct {
	*pkgbase.Virtual
}

// NewVirtualFactory returns the virtual NuGet factory.
func NewVirtualFactory(resolver virtualbase.Resolver) registry.Factory {
	return func(_ context.Context, _ registry.Deps, ns string, r *service.RegistryRepository) (registry.Registry, error) {
		v, err := pkgbase.NewVirtual(typ, resolver, ns, r)
		if err != nil {
			return nil, err
		}
		return &Virtual{Virtual: v}, nil
	}
}

func (v *Virtual) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) { return classify(r) }

func (v *Virtual) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !pkgbase.IsRead(r) {
		pkgbase.Error(w, http.StatusMethodNotAllowed, "virtual repositories are read-only; publish to a local member")
		return
	}
	p := r.URL.Path
	switch {
	case isServiceIndex(p):
		serveServiceIndex(w, r)
		return
	case strings.HasPrefix(p, flatPrefix):
		if parts := splitFlat(p); len(parts) == 2 && parts[1] == "index.json" {
			v.mergeFlatIndex(w, r)
			return
		}
	case strings.HasPrefix(p, regPrefix) && strings.HasSuffix(p, "/index.json") && strings.Count(strings.TrimPrefix(p, regPrefix), "/") == 1:
		v.mergeRegistration(w, r)
		return
	case strings.TrimRight(p, "/") == searchPath:
		v.mergeSearch(w, r, false)
		return
	case strings.TrimRight(p, "/") == autocompletePath:
		v.mergeSearch(w, r, true)
		return
	}
	if !v.ServeFirstHit(w, r) {
		pkgbase.NotFound(w)
	}
}

func (v *Virtual) mergeFlatIndex(w http.ResponseWriter, r *http.Request) {
	seen := map[string]struct{}{}
	var out []string
	for _, body := range v.CollectMembers(r) {
		var idx struct {
			Versions []string `json:"versions"`
		}
		if json.Unmarshal(body, &idx) != nil {
			continue
		}
		for _, ver := range idx.Versions {
			ver = strings.ToLower(ver)
			if _, ok := seen[ver]; ok {
				continue
			}
			seen[ver] = struct{}{}
			out = append(out, ver)
		}
	}
	if len(out) == 0 {
		pkgbase.NotFound(w)
		return
	}
	pkgbase.SortVersions(out)
	pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{"versions": out})
}

func (v *Virtual) mergeRegistration(w http.ResponseWriter, r *http.Request) {
	bodies := v.CollectMembers(r)
	if len(bodies) == 0 {
		pkgbase.NotFound(w)
		return
	}
	var root map[string]any
	seen := map[string]struct{}{}
	var pages []any
	for _, body := range bodies {
		var doc map[string]any
		if json.Unmarshal(body, &doc) != nil {
			continue
		}
		if root == nil {
			root = doc
		}
		items, _ := doc["items"].([]any)
		for _, pi := range items {
			pg, ok := pi.(map[string]any)
			if !ok {
				continue
			}
			leaves, inlined := pg["items"].([]any)
			if !inlined {
				pages = append(pages, pg)
				continue
			}
			var kept []any
			for _, li := range leaves {
				leaf, _ := li.(map[string]any)
				ce, _ := leaf["catalogEntry"].(map[string]any)
				ver, _ := ce["version"].(string)
				key, ok := lowerVersion(ver)
				if !ok {
					key = strings.ToLower(ver)
				}
				if _, dup := seen[key]; dup {
					continue
				}
				seen[key] = struct{}{}
				kept = append(kept, leaf)
			}
			if len(kept) == 0 {
				continue
			}
			pg["items"] = kept
			pg["count"] = len(kept)
			pages = append(pages, pg)
		}
	}
	if root == nil {
		pkgbase.NotFound(w)
		return
	}
	sort.SliceStable(pages, func(i, j int) bool {
		li, _ := pages[i].(map[string]any)["lower"].(string)
		lj, _ := pages[j].(map[string]any)["lower"].(string)
		return pkgbase.CompareVersions(li, lj) < 0
	})
	root["items"] = pages
	root["count"] = len(pages)
	pkgbase.WriteJSON(w, r, http.StatusOK, root)
}

// mergeSearch queries every member for the first skip+take hits and
// merges them by package id (first member wins).
func (v *Virtual) mergeSearch(w http.ResponseWriter, r *http.Request, auto bool) {
	q := r.URL.Query()
	skip, take := skipTake(q)
	mq := url.Values{}
	for k, vs := range q {
		mq[k] = vs
	}
	mq.Set("skip", "0")
	mq.Set("take", strconv.Itoa(skip+take))
	mr := r.Clone(r.Context())
	mr.URL.RawQuery = mq.Encode()

	total := 0
	if auto {
		seen := map[string]struct{}{}
		data := []string{}
		for _, body := range v.CollectMembers(mr) {
			var res struct {
				TotalHits int      `json:"totalHits"`
				Data      []string `json:"data"`
			}
			if json.Unmarshal(body, &res) != nil {
				continue
			}
			total = max(total, res.TotalHits)
			for _, s := range res.Data {
				if _, ok := seen[strings.ToLower(s)]; ok {
					continue
				}
				seen[strings.ToLower(s)] = struct{}{}
				data = append(data, s)
			}
		}
		if q.Get("id") != "" {
			pkgbase.SortVersions(data)
			pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{"totalHits": len(data), "data": data})
			return
		}
		pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{"totalHits": max(total, len(data)), "data": page(data, skip, take)})
		return
	}
	seen := map[string]struct{}{}
	data := []map[string]any{}
	for _, body := range v.CollectMembers(mr) {
		var res struct {
			TotalHits int              `json:"totalHits"`
			Data      []map[string]any `json:"data"`
		}
		if json.Unmarshal(body, &res) != nil {
			continue
		}
		total = max(total, res.TotalHits)
		for _, d := range res.Data {
			id, _ := d["id"].(string)
			if _, ok := seen[strings.ToLower(id)]; ok {
				continue
			}
			seen[strings.ToLower(id)] = struct{}{}
			data = append(data, d)
		}
	}
	pkgbase.WriteJSON(w, r, http.StatusOK, map[string]any{"totalHits": max(total, len(data)), "data": page(data, skip, take)})
}
