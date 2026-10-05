package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/rakunlabs/ada"

	"github.com/rakunlabs/kutu/internal/registry/common"
	"github.com/rakunlabs/kutu/internal/server/authx"
	"github.com/rakunlabs/kutu/internal/service"
)

// errNoCredentials marks a data-plane request that carried no credentials
// at all, so callers can answer with the protocol's auth challenge.
var errNoCredentials = errors.New("authentication required")

// dataPrincipal resolves the caller of a data-plane request (registry or
// raw). Accepted, in order:
//
//  1. A short-lived registry bearer token minted by /v2/token.
//  2. A kutu API token, as Bearer, X-Kutu-Token, or the Basic password.
//  3. Local username + password as Basic auth (users with TOTP must use
//     a token instead).
//  4. The browser session cookie.
//
// Returns (nil, nil) for an anonymous request and an error wrapping
// ErrUnauthorized when credentials were presented but are invalid.
func (a *api) dataPrincipal(r *http.Request) (*authx.Principal, error) {
	ctx := r.Context()

	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		raw := strings.TrimSpace(auth[len("Bearer "):])
		if claims, err := a.registryTokens.Verify(raw); err == nil {
			p := a.principalFromRegistryToken(r, claims)
			if p == nil {
				return nil, fmt.Errorf("registry token no longer valid: %w", service.ErrUnauthorized)
			}
			return p, nil
		}
	}

	if raw := common.ExtractToken(r); strings.HasPrefix(raw, service.TokenPrefix) {
		p, err := a.mgr.PrincipalForToken(ctx, raw)
		if err != nil {
			return nil, err
		}
		return p, nil
	}

	if user, pass, ok := r.BasicAuth(); ok {
		if p := a.mgr.PrincipalForPassword(ctx, user, pass); p != nil {
			return p, nil
		}
		return nil, fmt.Errorf("invalid credentials: %w", service.ErrUnauthorized)
	}

	if raw := common.ExtractToken(r); raw != "" {
		return nil, fmt.Errorf("invalid token: %w", service.ErrUnauthorized)
	}

	if p := a.mgr.SessionPrincipal(r); p != nil {
		return p, nil
	}
	return nil, nil
}

// authorizeRegistry decides whether the caller may perform op on the
// registry path "<ns>/<repo><rest>". Anonymous reads are allowed when the
// operator turned on registry_anonymous_read.
func (a *api) authorizeRegistry(r *http.Request, ns, repo, rest, op string) (*authx.Principal, error) {
	p, err := a.dataPrincipal(r)
	if err != nil {
		return nil, err
	}
	capKey := capForOp(op)
	if p == nil {
		if op == common.OpRead && a.svc.GetAuthSettings(r.Context()).RegistryAnonymousReadEnabled() {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: %w", errNoCredentials, service.ErrUnauthorized)
	}
	path := ns + "/" + repo + rest
	if !p.Allows(capKey, path) {
		return p, fmt.Errorf("%s on %s/%s requires %q: %w", op, ns, repo, capKey, service.ErrForbidden)
	}
	return p, nil
}

// authorizeRaw is the data-plane gate for /api/v1/raw/*: capability capKey
// on "<mount>/<path>". Listing a directory may pass through ancestors of a
// permitted path.
func (a *api) authorizeRaw(c *ada.Context, capKey, path string, ancestor bool) (*authx.Principal, error) {
	p, err := a.dataPrincipal(c.Request)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, fmt.Errorf("%w: %w", errNoCredentials, service.ErrUnauthorized)
	}
	if !p.Caps.Has(capKey) {
		return p, fmt.Errorf("capability %q required: %w", capKey, service.ErrForbidden)
	}
	allowed := p.Patterns.Allows(capKey, path)
	if !allowed && ancestor {
		allowed = p.Patterns.AllowsAncestor(capKey, path)
	}
	if !allowed {
		return p, fmt.Errorf("path %q not permitted for %q: %w", path, capKey, service.ErrForbidden)
	}
	return p, nil
}

// withPrincipal stamps the principal on the request context so storage
// audit columns and hook events attribute the change.
func withPrincipal(r *http.Request, p *authx.Principal) *http.Request {
	if p == nil {
		return r
	}
	ctx := service.WithActor(r.Context(), p.Username)
	ctx = service.WithCapabilities(ctx, p.Caps)
	ctx = service.WithCapabilityPatterns(ctx, p.Patterns)
	if p.Kind == "user" {
		ctx = service.WithUserInfo(ctx, p.Username, p.ID)
	}
	return r.WithContext(ctx)
}
