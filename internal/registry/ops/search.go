package ops

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/service"
)

const (
	searchTimeout     = 5 * time.Second
	searchConcurrency = 8
)

// SearchHit is one package matching a cross-registry search.
type SearchHit struct {
	Namespace string   `json:"namespace"`
	Repo      string   `json:"repo"`
	Type      string   `json:"type"`
	Kind      string   `json:"kind"`
	Name      string   `json:"name"`
	Versions  []string `json:"versions,omitempty"` // newest first
	Latest    string   `json:"latest,omitempty"`
}

// Search lists packages of every non-virtual registry implementing
// PackageLister and returns those whose name contains query
// (case-insensitive). Exact matches sort first, then prefix matches,
// then by name. limit <= 0 means unlimited. Registries that fail or
// exceed the per-registry timeout are skipped.
func Search(ctx context.Context, regs []registry.Registry, query string, limit int) []SearchHit {
	q := strings.ToLower(strings.TrimSpace(query))

	var (
		mu   sync.Mutex
		hits []SearchHit
		wg   sync.WaitGroup
		sem  = make(chan struct{}, searchConcurrency)
	)

	for _, reg := range regs {
		if reg == nil || reg.Kind() == service.RegistryKindVirtual {
			continue
		}
		lister, ok := reg.(registry.PackageLister)
		if !ok {
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()

			pkgs, ok := listWithTimeout(ctx, lister)
			if !ok {
				return
			}

			var local []SearchHit
			for _, p := range pkgs {
				if !strings.Contains(strings.ToLower(p.Name), q) {
					continue
				}
				vs := append([]string(nil), p.Versions...)
				sortNewestFirst(vs)
				var latest string
				if len(vs) > 0 {
					latest = vs[0]
				}
				local = append(local, SearchHit{
					Namespace: reg.Namespace(),
					Repo:      reg.Name(),
					Type:      reg.Type(),
					Kind:      reg.Kind(),
					Name:      p.Name,
					Versions:  vs,
					Latest:    latest,
				})
			}

			mu.Lock()
			hits = append(hits, local...)
			mu.Unlock()
		}()
	}
	wg.Wait()

	rank := func(name string) int {
		n := strings.ToLower(name)
		switch {
		case n == q:
			return 0
		case strings.HasPrefix(n, q):
			return 1
		}
		return 2
	}
	sort.SliceStable(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
		if ra, rb := rank(a.Name), rank(b.Name); ra != rb {
			return ra < rb
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Repo < b.Repo
	})

	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

// listWithTimeout runs ListPackages under a per-registry deadline,
// abandoning implementations that ignore their context.
func listWithTimeout(ctx context.Context, l registry.PackageLister) ([]registry.PackageSummary, bool) {
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()

	type result struct {
		pkgs []registry.PackageSummary
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		pkgs, err := l.ListPackages(ctx)
		ch <- result{pkgs, err}
	}()

	select {
	case res := <-ch:
		return res.pkgs, res.err == nil
	case <-ctx.Done():
		return nil, false
	}
}

func sortNewestFirst(vs []string) {
	sort.SliceStable(vs, func(i, j int) bool { return pkgbase.CompareVersions(vs[i], vs[j]) > 0 })
}
