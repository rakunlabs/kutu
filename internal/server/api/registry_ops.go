package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rakunlabs/ada"

	"github.com/rakunlabs/kutu/internal/hook"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/ops"
	"github.com/rakunlabs/kutu/internal/registry/policy"
	"github.com/rakunlabs/kutu/internal/service"
)

// registry_ops.go — cross-protocol registry operations: search,
// retention, promotion, export/import, replication, prefetch,
// vulnerability lookup, plus the policy gate and background schedulers.

var (
	policyEngine *policy.Engine
	opsScheduler = ops.NewScheduler()
)

// startRegistryRuntime installs the request policy gate and starts the
// prefetch / replication scheduler. Called once from Handle.
func (a *api) startRegistryRuntime(ctx context.Context) {
	if a.registryMgr == nil {
		return
	}
	mgr := a.registryMgr
	policyEngine = policy.New(func(_ context.Context, ns, repo string) *service.RegistryRepository {
		return mgr.Repository(ns, repo)
	}, policy.Options{})
	registry.SetRequestGate(policyEngine.Gate)
	go opsScheduler.Run(ctx)
	a.syncRegistrySchedules()
}

// syncRegistrySchedules (re)registers prefetch and replication jobs
// from the live routing table. Safe to call on every reload.
func (a *api) syncRegistrySchedules() {
	if a.registryMgr == nil {
		return
	}
	want := map[string]bool{}
	for _, reg := range a.registryMgr.List() {
		row := a.registryMgr.Repository(reg.Namespace(), reg.Name())
		if row == nil {
			continue
		}
		ns, name := reg.Namespace(), reg.Name()
		if row.Prefetch != nil && len(row.Prefetch.Packages) > 0 && row.Kind == service.RegistryKindRemote {
			key := "prefetch/" + ns + "/" + name
			want[key] = true
			pkgs := append([]string(nil), row.Prefetch.Packages...)
			opsScheduler.Set(key, durationOr(row.Prefetch.Interval, 6*time.Hour), func(ctx context.Context) {
				reg, ok := a.registryMgr.Lookup(ns, name)
				if !ok {
					return
				}
				for _, err := range ops.PrefetchAll(ctx, reg, pkgs) {
					slog.Warn("registry prefetch", "repo", ns+"/"+name, "error", err)
				}
			})
		}
		if row.Replication != nil && !row.Replication.Disabled && row.Kind == service.RegistryKindLocal {
			key := "replicate/" + ns + "/" + name
			want[key] = true
			opsScheduler.Set(key, durationOr(row.Replication.Interval, time.Hour), func(ctx context.Context) {
				if _, err := a.replicateRepo(ctx, ns, name); err != nil {
					slog.Warn("registry replication", "repo", ns+"/"+name, "error", err)
				}
			})
		}
	}
	for _, k := range opsScheduler.Keys() {
		if !want[k] {
			opsScheduler.Remove(k)
		}
	}
}

func durationOr(s string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return def
}

// ── search ──

// searchRegistryPackages: GET /api/v1/registries/search?q=&limit=.
// Results are filtered to repositories the caller may read.
func (a *api) searchRegistryPackages(c *ada.Context) error {
	if err := a.registryFeatureGate(c); err != nil {
		return err
	}
	if a.registryMgr == nil {
		return c.SetStatus(http.StatusOK).SendJSON([]ops.SearchHit{})
	}
	q := strings.TrimSpace(c.Request.URL.Query().Get("q"))
	if len(q) < 2 {
		return fmt.Errorf("query must be at least 2 characters: %w", service.ErrBadRequest)
	}
	limit, _ := strconv.Atoi(c.Request.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	ctx := c.Request.Context()
	caps := service.CapabilitiesFromContext(ctx)
	pats := service.CapabilityPatternsFromContext(ctx)
	var regs []registry.Registry
	for _, reg := range a.registryMgr.List() {
		if !caps.Has(service.CapRegistryRead) || !pats.AllowsAncestor(service.CapRegistryRead, reg.Namespace()+"/"+reg.Name()) {
			continue
		}
		regs = append(regs, reg)
	}
	hits := ops.Search(ctx, regs, q, limit)
	if hits == nil {
		hits = []ops.SearchHit{}
	}
	return c.SetStatus(http.StatusOK).SendJSON(hits)
}

// ── vulnerabilities ──

// getRegistryVulnerabilities: GET …/vulnerabilities/{name...}?version=.
func (a *api) getRegistryVulnerabilitiesFor(typ string) func(*ada.Context) error {
	return func(c *ada.Context) error {
		reg, _, _, err := a.resolveRegistry(c, typ)
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(c.Request.PathValue("*"), "/")
		version, err := requiredQuery(c.Request.URL.Query().Get("version"), "version")
		if err != nil {
			return err
		}
		if registry.Ecosystem(reg.Type()) == "" {
			return fmt.Errorf("no vulnerability database for %s packages: %w", reg.Type(), service.ErrBadRequest)
		}
		if policyEngine == nil {
			return fmt.Errorf("policy engine not running: %w", service.ErrNotFound)
		}
		vulns, err := policyEngine.Vulnerabilities(c.Request.Context(), reg.Type(), name, version)
		if err != nil {
			return fmt.Errorf("osv lookup: %w", err)
		}
		if vulns == nil {
			vulns = []policy.Vuln{}
		}
		return c.SetStatus(http.StatusOK).SendJSON(vulns)
	}
}

// ── retention ──

// runRegistryRetention: POST …/retention?dry_run=true|false.
func (a *api) runRegistryRetentionFor(typ string) func(*ada.Context) error {
	return func(c *ada.Context) error {
		reg, ns, repo, err := a.resolveRegistry(c, typ)
		if err != nil {
			return err
		}
		if reg.Kind() != service.RegistryKindLocal {
			return fmt.Errorf("retention applies to local repositories only: %w", service.ErrBadRequest)
		}
		row := a.registryMgr.Repository(ns, repo)
		if row == nil || row.Policy == nil || row.Policy.Retention == nil {
			return fmt.Errorf("repository has no retention policy: %w", service.ErrBadRequest)
		}
		ctx := c.Request.Context()
		plan, err := ops.PlanRetention(ctx, reg, *row.Policy.Retention, time.Now())
		if err != nil {
			return fmt.Errorf("plan retention: %w", err)
		}
		out := map[string]any{"deletions": nonNil(plan.Deletions)}
		if c.Request.URL.Query().Get("dry_run") != "false" {
			return c.SetStatus(http.StatusOK).SendJSON(out)
		}
		deleted, errs := ops.ApplyRetention(ctx, reg, plan)
		out["deleted"] = deleted
		if len(errs) > 0 {
			msgs := make([]string, len(errs))
			for i, e := range errs {
				msgs[i] = e.Error()
			}
			out["errors"] = msgs
		}
		for _, it := range plan.Deletions {
			a.emitRegistryEvent(hook.Event{Type: hook.EventRegistryDeleted, Mount: ns, Path: repo + "/" + it.Name + "@" + it.Version, Protocol: "registry-" + reg.Type()})
		}
		return c.SetStatus(http.StatusOK).SendJSON(out)
	}
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// ── promotion ──

// promoteRegistryVersion: POST …/promote {name, version, target_namespace?, target_repo}.
func (a *api) promoteRegistryVersionFor(typ string) func(*ada.Context) error {
	return func(c *ada.Context) error {
		src, ns, repo, err := a.resolveRegistry(c, typ)
		if err != nil {
			return err
		}
		var req struct {
			Name            string `json:"name"`
			Version         string `json:"version"`
			TargetNamespace string `json:"target_namespace"`
			TargetRepo      string `json:"target_repo"`
		}
		if err := c.Bind(&req); err != nil {
			return errors.Join(err, service.ErrBadRequest)
		}
		if req.Name == "" || req.Version == "" || req.TargetRepo == "" {
			return fmt.Errorf("name, version and target_repo are required: %w", service.ErrBadRequest)
		}
		tns := req.TargetNamespace
		if tns == "" {
			tns = ns
		}
		ctx := c.Request.Context()
		if !service.CapabilityPatternsFromContext(ctx).AllowsAncestor(service.CapRegistryWrite, tns+"/"+req.TargetRepo) ||
			!service.CapabilitiesFromContext(ctx).Has(service.CapRegistryWrite) {
			return fmt.Errorf("write to %s/%s not permitted: %w", tns, req.TargetRepo, service.ErrForbidden)
		}
		dst, ok := a.registryMgr.Lookup(tns, req.TargetRepo)
		if !ok {
			return fmt.Errorf("target %s/%s not found: %w", tns, req.TargetRepo, service.ErrNotFound)
		}
		if err := ops.Promote(ctx, src, dst, req.Name, req.Version); err != nil {
			switch {
			case errors.Is(err, registry.ErrPackageNotFound):
				return fmt.Errorf("%s@%s: %w", req.Name, req.Version, service.ErrNotFound)
			case errors.Is(err, ops.ErrIncompatible):
				return fmt.Errorf("%v: %w", err, service.ErrBadRequest)
			}
			return fmt.Errorf("promote: %w", err)
		}
		a.emitRegistryEvent(hook.Event{Type: hook.EventRegistryPublished, Mount: tns, Path: req.TargetRepo + "/" + req.Name + "@" + req.Version, Protocol: "registry-" + dst.Type()})
		slog.Info("registry promote", "from", ns+"/"+repo, "to", tns+"/"+req.TargetRepo, "package", req.Name, "version", req.Version)
		return c.SetStatus(http.StatusOK).SendJSON(map[string]bool{"ok": true})
	}
}

// ── export / import / replication ──

func (a *api) repoTree(ns, repo string) (ops.Tree, *service.RegistryRepository, error) {
	row := a.registryMgr.Repository(ns, repo)
	if row == nil {
		return nil, nil, fmt.Errorf("registry %s/%s not found: %w", ns, repo, service.ErrNotFound)
	}
	if row.Kind == service.RegistryKindVirtual {
		return nil, nil, fmt.Errorf("virtual repositories have no storage of their own: %w", service.ErrBadRequest)
	}
	fs, ok := a.rawHandler.MountFS(row.Mount)
	if !ok {
		return nil, nil, fmt.Errorf("raw mount %q not found: %w", row.Mount, service.ErrNotFound)
	}
	return ops.TreeFromRawFS(fs, row.BasePath), row, nil
}

// exportRegistryRepo: GET …/export[?since=RFC3339] → tar.gz stream.
func (a *api) exportRegistryRepoFor(typ string) func(*ada.Context) error {
	return func(c *ada.Context) error {
		_, ns, repo, err := a.resolveRegistry(c, typ)
		if err != nil {
			return err
		}
		tree, row, err := a.repoTree(ns, repo)
		if err != nil {
			return err
		}
		meta := ops.ExportMeta{Type: row.Type, Namespace: ns, Repo: repo, CreatedAt: time.Now().UTC()}
		var since time.Time
		if s := c.Request.URL.Query().Get("since"); s != "" {
			if since, err = time.Parse(time.RFC3339, s); err != nil {
				return fmt.Errorf("since must be RFC3339: %w", service.ErrBadRequest)
			}
		}
		w := c.Response
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s-%s.tar.gz"`, ns, repo, time.Now().UTC().Format("20060102-150405")))
		w.WriteHeader(http.StatusOK)
		if since.IsZero() {
			err = ops.Export(c.Request.Context(), tree, meta, w)
		} else {
			err = ops.ExportSince(c.Request.Context(), tree, meta, since, w)
		}
		if err != nil {
			slog.Warn("registry export aborted", "repo", ns+"/"+repo, "error", err)
		}
		return nil
	}
}

// importRegistryRepo: POST …/import?overwrite= (body: export tar.gz).
func (a *api) importRegistryRepoFor(typ string) func(*ada.Context) error {
	return func(c *ada.Context) error {
		reg, ns, repo, err := a.resolveRegistry(c, typ)
		if err != nil {
			return err
		}
		if reg.Kind() != service.RegistryKindLocal {
			return fmt.Errorf("import targets local repositories only: %w", service.ErrBadRequest)
		}
		tree, row, err := a.repoTree(ns, repo)
		if err != nil {
			return err
		}
		overwrite := c.Request.URL.Query().Get("overwrite") == "true"
		stats, err := ops.Import(c.Request.Context(), tree, c.Request.Body, row.Type, overwrite)
		if err != nil {
			return fmt.Errorf("import: %v: %w", err, service.ErrBadRequest)
		}
		a.reloadRegistry(c.Request.Context())
		return c.SetStatus(http.StatusOK).SendJSON(stats)
	}
}

func (a *api) replicateRepo(ctx context.Context, ns, repo string) (ops.ImportStats, error) {
	tree, row, err := a.repoTree(ns, repo)
	if err != nil {
		return ops.ImportStats{}, err
	}
	if row.Replication == nil || row.Replication.SourceURL == "" {
		return ops.ImportStats{}, fmt.Errorf("repository has no replication source: %w", service.ErrBadRequest)
	}
	sinceT := a.svc.ReplicationWatermark(ctx, ns, repo)
	started := time.Now().UTC()
	stats, err := ops.Replicate(ctx, &http.Client{Timeout: 30 * time.Minute}, *row.Replication, tree, row.Type, sinceT)
	if err != nil {
		return stats, err
	}
	_ = a.svc.SetReplicationWatermark(ctx, ns, repo, started.Add(-time.Minute))
	if stats.Files > 0 {
		a.reloadRegistry(ctx)
	}
	return stats, nil
}

// replicateRegistryRepo: POST …/replicate — run the pull now.
func (a *api) replicateRegistryRepoFor(typ string) func(*ada.Context) error {
	return func(c *ada.Context) error {
		_, ns, repo, err := a.resolveRegistry(c, typ)
		if err != nil {
			return err
		}
		stats, err := a.replicateRepo(c.Request.Context(), ns, repo)
		if err != nil {
			return err
		}
		return c.SetStatus(http.StatusOK).SendJSON(stats)
	}
}

// prefetchRegistryRepo: POST …/prefetch {packages?: []} — warm now.
func (a *api) prefetchRegistryRepoFor(typ string) func(*ada.Context) error {
	return func(c *ada.Context) error {
		reg, ns, repo, err := a.resolveRegistry(c, typ)
		if err != nil {
			return err
		}
		if _, ok := reg.(registry.Prefetcher); !ok {
			return fmt.Errorf("prefetch not supported for %s/%s: %w", ns, repo, service.ErrBadRequest)
		}
		var req struct {
			Packages []string `json:"packages"`
		}
		_ = c.Bind(&req)
		pkgs := req.Packages
		if len(pkgs) == 0 {
			if row := a.registryMgr.Repository(ns, repo); row != nil && row.Prefetch != nil {
				pkgs = row.Prefetch.Packages
			}
		}
		if len(pkgs) == 0 {
			return fmt.Errorf("no packages to prefetch: %w", service.ErrBadRequest)
		}
		errs := ops.PrefetchAll(c.Request.Context(), reg, pkgs)
		msgs := make([]string, 0, len(errs))
		for _, e := range errs {
			msgs = append(msgs, e.Error())
		}
		return c.SetStatus(http.StatusOK).SendJSON(map[string]any{"errors": msgs})
	}
}
