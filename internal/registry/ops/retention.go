package ops

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/service"
)

// RetentionPlan is the set of versions a retention run would delete.
type RetentionPlan struct {
	Deletions []RetentionItem `json:"deletions"`
}

// RetentionItem is one version scheduled for deletion.
type RetentionItem struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Reason  string `json:"reason"`
}

// PlanRetention computes which versions of a local repository violate
// pol. Per package the newest version is always kept, versions
// matching KeepPatterns are protected, versions beyond the newest
// KeepLastVersions are deleted, and versions published more than
// MaxVersionAgeDays before now are deleted (unknown publish time
// keeps the version).
func PlanRetention(ctx context.Context, reg registry.Registry, pol service.RegistryRetentionPolicy, now time.Time) (RetentionPlan, error) {
	var plan RetentionPlan
	if reg.Kind() != service.RegistryKindLocal {
		return plan, fmt.Errorf("%w: retention requires a local repository", ErrIncompatible)
	}
	lister, ok := reg.(registry.PackageLister)
	if !ok {
		return plan, fmt.Errorf("%w: listing packages", ErrUnsupported)
	}
	if _, ok := reg.(registry.VersionDeleter); !ok {
		return plan, fmt.Errorf("%w: deleting versions", ErrUnsupported)
	}
	if pol.KeepLastVersions <= 0 && pol.MaxVersionAgeDays <= 0 {
		return plan, nil
	}

	pkgs, err := lister.ListPackages(ctx)
	if err != nil {
		return plan, fmt.Errorf("list packages: %w", err)
	}

	maxAge := time.Duration(pol.MaxVersionAgeDays) * 24 * time.Hour
	for _, p := range pkgs {
		if err := ctx.Err(); err != nil {
			return plan, err
		}
		vs := append([]string(nil), p.Versions...)
		sortNewestFirst(vs)

		var times map[string]time.Time
		if pol.MaxVersionAgeDays > 0 && len(vs) > 1 {
			times = publishTimes(ctx, reg, p.Name, vs[1:])
		}

		for i, v := range vs {
			if i == 0 || keepByPattern(pol.KeepPatterns, v) {
				continue
			}
			if pol.KeepLastVersions > 0 && i >= pol.KeepLastVersions {
				plan.Deletions = append(plan.Deletions, RetentionItem{
					Name: p.Name, Version: v,
					Reason: fmt.Sprintf("beyond newest %d versions", pol.KeepLastVersions),
				})
				continue
			}
			if pol.MaxVersionAgeDays > 0 {
				if t, ok := times[v]; ok && now.Sub(t) > maxAge {
					plan.Deletions = append(plan.Deletions, RetentionItem{
						Name: p.Name, Version: v,
						Reason: fmt.Sprintf("older than %d days", pol.MaxVersionAgeDays),
					})
				}
			}
		}
	}
	return plan, nil
}

// ApplyRetention deletes every version in plan. Versions that are
// already gone are not counted as errors.
func ApplyRetention(ctx context.Context, reg registry.Registry, plan RetentionPlan) (int, []error) {
	del, ok := reg.(registry.VersionDeleter)
	if !ok {
		return 0, []error{fmt.Errorf("%w: deleting versions", ErrUnsupported)}
	}
	var (
		deleted int
		errs    []error
	)
	for _, it := range plan.Deletions {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if err := del.DeleteVersion(ctx, it.Name, it.Version); err != nil {
			if errors.Is(err, registry.ErrPackageNotFound) {
				continue
			}
			errs = append(errs, fmt.Errorf("delete %s@%s: %w", it.Name, it.Version, err))
			continue
		}
		deleted++
	}
	return deleted, errs
}

func keepByPattern(patterns []string, version string) bool {
	for _, p := range patterns {
		if p == "" {
			continue
		}
		if ok, _ := path.Match(p, version); ok {
			return true
		}
	}
	return false
}

// publishTimes resolves publish times for versions via PackageDetail,
// falling back to ArtifactInfoProvider for versions it doesn't cover.
func publishTimes(ctx context.Context, reg registry.Registry, name string, versions []string) map[string]time.Time {
	out := map[string]time.Time{}
	if d, ok := reg.(registry.PackageDetailer); ok {
		if det, err := d.PackageDetail(ctx, name); err == nil && det != nil {
			collectDetailTimes(det, out)
		}
	}
	info, ok := reg.(registry.ArtifactInfoProvider)
	if !ok {
		return out
	}
	for _, v := range versions {
		if _, ok := out[v]; ok {
			continue
		}
		meta, err := info.ArtifactInfo(ctx, registry.ArtifactRef{Name: name, Version: v})
		if err == nil && !meta.PublishedAt.IsZero() {
			out[v] = meta.PublishedAt
		}
	}
	return out
}

func collectDetailTimes(det *registry.PackageDetail, out map[string]time.Time) {
	add := func(v, ts string) {
		if t, ok := parseTime(ts); ok {
			out[v] = t
		}
	}
	if det.Generic != nil {
		for _, v := range det.Generic.Versions {
			add(v.Version, v.PublishedAt)
		}
	}
	if det.NPM != nil {
		for _, v := range det.NPM.Versions {
			add(v.Version, v.PublishedAt)
		}
	}
	if det.Go != nil {
		for _, v := range det.Go.Versions {
			add(v.Version, v.PublishedAt)
		}
	}
	if det.Helm != nil {
		for _, v := range det.Helm.Versions {
			add(v.Version, v.Created)
		}
	}
}

func parseTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", time.DateOnly} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
