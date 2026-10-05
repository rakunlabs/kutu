package ops

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/service"
)

// Promote copies name@version from the local repository src into the
// local repository dst of the same type.
func Promote(ctx context.Context, src, dst registry.Registry, name, version string) error {
	if src == nil || dst == nil {
		return fmt.Errorf("%w: missing repository", ErrIncompatible)
	}
	if src.Kind() != service.RegistryKindLocal || dst.Kind() != service.RegistryKindLocal {
		return fmt.Errorf("%w: promotion requires local source and destination", ErrIncompatible)
	}
	if src.Type() != dst.Type() {
		return fmt.Errorf("%w: type %q cannot be promoted into %q", ErrIncompatible, src.Type(), dst.Type())
	}
	if src.Namespace() == dst.Namespace() && src.Name() == dst.Name() {
		return fmt.Errorf("%w: source and destination are the same repository", ErrIncompatible)
	}
	if name == "" || version == "" {
		return fmt.Errorf("%w: name and version are required", registry.ErrInvalidPackageName)
	}
	p, ok := src.(registry.VersionPromoter)
	if !ok {
		return fmt.Errorf("%w: promotion", ErrUnsupported)
	}
	err := p.PromoteVersion(ctx, dst, name, version)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, registry.ErrPackageNotFound):
		return fmt.Errorf("promote %s@%s: %w", name, version, err)
	case strings.Contains(strings.ToLower(err.Error()), "not found"):
		return fmt.Errorf("promote %s@%s: %w: %v", name, version, registry.ErrPackageNotFound, err)
	}
	return fmt.Errorf("promote %s@%s: %w", name, version, err)
}

// PrefetchAll warms reg's cache for each entry of packages ("name" or
// "name@version"). Returns one error per failed entry.
func PrefetchAll(ctx context.Context, reg registry.Registry, packages []string) []error {
	pf, ok := reg.(registry.Prefetcher)
	if !ok {
		return []error{fmt.Errorf("%w: prefetch", ErrUnsupported)}
	}
	var errs []error
	for _, spec := range packages {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		name, version := SplitPackageSpec(spec)
		if name == "" {
			continue
		}
		if err := pf.Prefetch(ctx, name, version); err != nil {
			errs = append(errs, fmt.Errorf("prefetch %s: %w", strings.TrimSpace(spec), err))
		}
	}
	return errs
}

// SplitPackageSpec splits "name@version" on the last "@" (a leading
// "@" belongs to an npm scope: "@scope/x@1.0").
func SplitPackageSpec(spec string) (name, version string) {
	spec = strings.TrimSpace(spec)
	if i := strings.LastIndexByte(spec, '@'); i > 0 {
		return spec[:i], spec[i+1:]
	}
	return spec, ""
}
