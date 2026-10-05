package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/rakunlabs/ada"
	"github.com/rakunlabs/ada/middleware/auth/identity"
	"github.com/rakunlabs/kutu/internal/service"
)

// listPermissions returns all defined permissions.
func (a *api) listPermissions(c *ada.Context) error {
	perms, err := a.svc.ListPermissions(c.Request.Context())
	if err != nil {
		return err
	}

	return c.SetStatus(http.StatusOK).SendJSON(perms)
}

// createPermission creates a new permission.
func (a *api) createPermission(c *ada.Context) error {
	var req service.CreatePermissionRequest
	if err := c.Bind(&req); err != nil {
		return errors.Join(err, service.ErrBadRequest)
	}

	perm, err := a.svc.CreatePermission(c.Request.Context(), &req)
	if err != nil {
		return err
	}

	return c.SetStatus(http.StatusCreated).SendJSON(perm)
}

// updatePermission updates a permission.
func (a *api) updatePermission(c *ada.Context) error {
	id := c.Request.PathValue("*")

	var req service.UpdatePermissionRequest
	if err := c.Bind(&req); err != nil {
		return errors.Join(err, service.ErrBadRequest)
	}

	if err := a.svc.UpdatePermission(c.Request.Context(), id, &req); err != nil {
		return err
	}

	return c.SetStatus(http.StatusOK).SendJSON(response{Message: "permission updated"})
}

// deletePermission deletes a permission.
func (a *api) deletePermission(c *ada.Context) error {
	id := c.Request.PathValue("*")

	if err := a.svc.DeletePermission(c.Request.Context(), id); err != nil {
		return err
	}

	return c.SendNoContent()
}

// getUserPermissions returns permissions assigned to a user.
func (a *api) getUserPermissions(c *ada.Context) error {
	userID := c.Request.PathValue("*")

	perms, err := a.svc.GetUserPermissions(c.Request.Context(), userID)
	if err != nil {
		return err
	}

	return c.SetStatus(http.StatusOK).SendJSON(perms)
}

// setUserPermissions replaces all permissions for a user.
func (a *api) setUserPermissions(c *ada.Context) error {
	userID := c.Request.PathValue("*")

	var req struct {
		PermissionIDs []string `json:"permission_ids"`
	}
	if err := c.Bind(&req); err != nil {
		return errors.Join(err, service.ErrBadRequest)
	}

	if err := a.svc.SetUserPermissions(c.Request.Context(), userID, req.PermissionIDs); err != nil {
		return err
	}

	return c.SetStatus(http.StatusOK).SendJSON(response{Message: "user permissions updated"})
}

// withPerm wraps a handler with a capability check against the resolved
// capability set in the request context (set by authx.CapMiddleware).
func (a *api) withPerm(need string, handler func(*ada.Context) error) func(*ada.Context) error {
	return func(c *ada.Context) error {
		caps := service.CapabilitiesFromContext(c.Request.Context())
		if !caps.Has(need) {
			return fmt.Errorf("capability %q required: %w", need, service.ErrForbidden)
		}
		return handler(c)
	}
}

// isSuperadmin reports whether the caller is a superadmin, either through
// the operator allowlist or the users.is_superadmin column.
func (a *api) isSuperadmin(ctx context.Context) bool {
	settings := a.svc.GetAuthSettings(ctx)
	if id := identity.FromContext(ctx); id != nil && settings != nil {
		for _, subject := range settings.Capabilities.Superadmins {
			if subject == id.Subject {
				return true
			}
		}
	}
	if userID := service.UserIDFromContext(ctx); userID != "" {
		if user, err := a.svc.GetUserByID(ctx, userID); err == nil && user != nil {
			return user.IsSuperadmin
		}
	}
	return false
}

// withAccountSecurityAccess applies the runtime policy for passkey and TOTP
// self-service. The UI mirrors this decision, but this gate is authoritative.
func (a *api) withAccountSecurityAccess(handler func(*ada.Context) error) func(*ada.Context) error {
	return func(c *ada.Context) error {
		ctx := c.Request.Context()
		settings := a.svc.GetAuthSettings(ctx)
		if !settings.AccountSecurityAllowed(a.isSuperadmin(ctx)) {
			return fmt.Errorf("account security is restricted to superadmins: %w", service.ErrForbidden)
		}
		return handler(c)
	}
}

// withRawPerm checks capability need on the raw path in the wildcard
// ("<mount>/<path>"). ancestor allows listing directories that lead to a
// permitted path.
func (a *api) withRawPerm(need string, ancestor bool, handler func(*ada.Context) error) func(*ada.Context) error {
	return func(c *ada.Context) error {
		ctx := c.Request.Context()
		if !service.CapabilitiesFromContext(ctx).Has(need) {
			return fmt.Errorf("capability %q required: %w", need, service.ErrForbidden)
		}
		path := c.Request.PathValue("*")
		patterns := service.CapabilityPatternsFromContext(ctx)
		allowed := patterns.Allows(need, path)
		if !allowed && ancestor {
			allowed = patterns.AllowsAncestor(need, path)
		}
		if !allowed {
			return fmt.Errorf("path %q not permitted for %q: %w", path, need, service.ErrForbidden)
		}
		return handler(c)
	}
}

// withRepoPerm checks capability need scoped to the {ns}/{repo} path
// params, so registry patterns like "team-a/**" apply to admin routes too.
func (a *api) withRepoPerm(need string, handler func(*ada.Context) error) func(*ada.Context) error {
	return func(c *ada.Context) error {
		ctx := c.Request.Context()
		if !service.CapabilitiesFromContext(ctx).Has(need) {
			return fmt.Errorf("capability %q required: %w", need, service.ErrForbidden)
		}
		path := c.Request.PathValue("ns") + "/" + c.Request.PathValue("repo")
		if !service.CapabilityPatternsFromContext(ctx).AllowsAncestor(need, path) {
			return fmt.Errorf("repository %q not permitted for %q: %w", path, need, service.ErrForbidden)
		}
		return handler(c)
	}
}
