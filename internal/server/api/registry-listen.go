package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"

	"github.com/rakunlabs/ada"

	"github.com/rakunlabs/kutu/internal/registry/common"
	"github.com/rakunlabs/kutu/internal/server/vhost"
	"github.com/rakunlabs/kutu/internal/service"
)

// registry-listen.go — dedicated per-repository registry endpoints.
//
// A registry listener publishes one {namespace, repo} pair at the
// *root* of its own endpoint (host/port, optionally scoped to a
// hostname and/or TLS-terminated). This solves the Docker CLI
// problem: docker always requests /v2/... at the host root, so the
// path-prefixed /registries/{ns}/{repo}/v2/... data plane on the main
// server is unreachable for it. It also gives operators per-repo
// firewalling and per-hostname certificates.
//
// Listeners ride the shared vhost layer (internal/server/vhost) under
// the "registry" owner key, so they can share ports with each other
// and with S3/WebDAV serve instances, split by hostname.

// registryVhostOwner is the vhost owner key for registry listeners.
const registryVhostOwner = "registry"

// reconcileRegistryListeners publishes the persisted listener list
// onto the vhost layer. Called at boot and from reloadRegistry after
// every registry mutation (repo changes may invalidate targets;
// listener-list saves obviously need it too). Best-effort: a load
// error leaves the running listeners in place.
func (a *api) reconcileRegistryListeners(ctx context.Context) {
	if a.vhostMgr == nil {
		return
	}
	cfg, err := a.svc.GetRegistryListeners(ctx)
	if err != nil {
		return
	}

	bindings := make([]vhost.Binding, 0, len(cfg.Listeners))
	for _, l := range cfg.Listeners {
		if !l.Enabled {
			continue
		}
		bindings = append(bindings, vhost.Binding{
			ID:         l.ID,
			Name:       l.Name,
			Host:       l.Host,
			Port:       l.Port,
			Hostname:   l.Hostname,
			TLSCertPEM: l.TLSCertPEM,
			TLSKeyPEM:  l.TLSKeyPEM,
			Handler:    a.registryListenerHandler(l.Namespace, l.Repo),
		})
	}
	a.vhostMgr.SetBindings(registryVhostOwner, bindings)
}

// registryListenerHandler serves one registry repo from the endpoint
// root. It mirrors serveRegistry (feature gate, CORS, token check)
// minus the path parsing: the full request path is the protocol path
// ("/v2/..." for Docker, "/lodash" for npm, ...).
//
// The registry target is resolved per request — not captured at
// build time — so a repo that is created, renamed or deleted after
// the listener was configured behaves correctly without a listener
// reconcile.
func (a *api) registryListenerHandler(ns, repo string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if a.registryMgr == nil || !a.svc.RegistryEnabled(ctx) {
			writeListenerError(w, http.StatusNotFound, "registry feature disabled")
			return
		}
		reg, found := a.registryMgr.Lookup(ns, repo)
		if !found {
			writeListenerError(w, http.StatusNotFound, fmt.Sprintf("registry %s/%s not found", ns, repo))
			return
		}
		if common.ApplyCORS(w, r, a.registryCORSOrigins(ctx, ns, repo)) {
			return
		}

		// Same token-scope convention as /registries/*: scope is the
		// repo-relative path, op derives from the method. kutu has no
		// auth (ValidateToken is a no-op) but the contract is kept so
		// the pika diff stays minimal.
		scope := "registry/" + ns + "/" + repo + r.URL.Path
		if tokenRaw := common.ExtractToken(r); tokenRaw != "" {
			if err := a.svc.ValidateToken(ctx, tokenRaw, scope, operationFor(r.Method)); err != nil {
				writeListenerError(w, http.StatusUnauthorized, err.Error())
				return
			}
		}

		// The registry lives at the endpoint root: the URL-prefix hint
		// used for absolute-URL reconstruction (npm tarball URLs,
		// Docker token realm) is empty. Overwrite unconditionally so a
		// client-supplied header cannot spoof it.
		r2 := r.Clone(ctx)
		r2.Header.Set("X-Pika-Registry-Prefix", "")

		reg.ServeHTTP(w, r2)
	})
}

func writeListenerError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, `{"message":%q}`, msg)
}

// ── admin endpoints ──

// getRegistryListeners returns the persisted listener list.
// GET /api/v1/registries/listeners.
func (a *api) getRegistryListeners(c *ada.Context) error {
	if err := a.registryFeatureGate(c); err != nil {
		return err
	}
	cfg, err := a.svc.GetRegistryListeners(c.Request.Context())
	if err != nil {
		return err
	}
	return c.SetStatus(http.StatusOK).SendJSON(cfg)
}

// updateRegistryListeners validates and persists the listener list,
// then republishes the live endpoints. PUT /api/v1/registries/listeners.
func (a *api) updateRegistryListeners(c *ada.Context) error {
	if err := a.registryFeatureGate(c); err != nil {
		return err
	}
	var cfg service.RegistryListenerSettings
	if err := c.Bind(&cfg); err != nil {
		return errors.Join(err, service.ErrBadRequest)
	}
	for i := range cfg.Listeners {
		if cfg.Listeners[i].ID == "" {
			cfg.Listeners[i].ID = newRegistryListenerID()
		}
	}
	rs := a.svc.GetRegistrySettings(c.Request.Context())
	if err := cfg.Validate(rs); err != nil {
		return err
	}
	if err := a.svc.SetRegistryListeners(c.Request.Context(), &cfg); err != nil {
		return err
	}
	a.reconcileRegistryListeners(c.Request.Context())
	return c.SetStatus(http.StatusOK).SendJSON(cfg)
}

// getRegistryListenerStatus exposes the live bind state of every
// listener. GET /api/v1/registries/listeners/status.
func (a *api) getRegistryListenerStatus(c *ada.Context) error {
	if err := a.registryFeatureGate(c); err != nil {
		return err
	}
	if a.vhostMgr == nil {
		return c.SetStatus(http.StatusOK).SendJSON([]any{})
	}
	return c.SetStatus(http.StatusOK).SendJSON(a.vhostMgr.Status(registryVhostOwner))
}

// newRegistryListenerID generates a short random id for a listener
// entry, e.g. "lst-9f2c1a".
func newRegistryListenerID() string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return "lst-" + hex.EncodeToString(b[:])
}
