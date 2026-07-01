// Package api wires the kutu HTTP surface: the artifact registry data
// plane and admin endpoints, the raw-mount file browser, and the
// user-built proxy CRUD. kutu runs without authentication — every
// route is reachable anonymously and the capability checks inherited
// from pika are satisfied by a full capability set planted on the
// request context by the server middleware.
package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/rakunlabs/ada"

	"github.com/rakunlabs/kutu/internal/hook"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/server/serve"
	"github.com/rakunlabs/kutu/internal/service"
)

// Info holds server metadata returned by the info endpoint.
type Info struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
	Date    string `json:"date,omitempty"`

	// EncryptionConfigInvalid is true when the process started with a
	// non-empty encryption.password in config but that passphrase did
	// NOT match the on-disk verifier. The server stays locked; the UI
	// renders the unlock screen with a "bad config value" warning.
	EncryptionConfigInvalid bool `json:"encryption_config_invalid,omitempty"`
}

// api carries the dependencies every handler needs. There is no auth
// manager: kutu is a plain server.
type api struct {
	svc         *service.Service
	info        Info
	rawHandler  *RawHandler
	serveMgr    *serve.Manager
	registryMgr *registry.Manager
	dispatcher  *hook.Dispatcher
	appCtx      context.Context
}

type response struct {
	Message string `json:"message,omitempty"`
}

// Handle registers every route on m. The caller (server.Start) builds
// the service, raw handler, proxy manager and registry manager and
// passes them in already wired.
func Handle(
	m *ada.Mux,
	svc *service.Service,
	info Info,
	rawHandler *RawHandler,
	serveMgr *serve.Manager,
	registryMgr *registry.Manager,
	dispatcher *hook.Dispatcher,
) error {
	a := &api{
		svc:         svc,
		info:        info,
		rawHandler:  rawHandler,
		serveMgr:    serveMgr,
		registryMgr: registryMgr,
		dispatcher:  dispatcher,
		appCtx:      context.Background(),
	}

	m.ErrorHandler(a.errorHandler)

	// Health + info.
	m.GET("/healthz", m.Wrap(a.healthzHandler))
	m.GET("/api/v1/info", m.Wrap(a.infoHandler))

	// ── At-rest encryption key lifecycle ──
	// status + unlock are on the lockgate allowlist so the SPA can
	// drive the unlock flow while the server is locked.
	m.GET("/api/v1/key/status", m.Wrap(a.getKeyStatus))
	m.POST("/api/v1/key/initialize", m.Wrap(a.postKeyInitialize))
	m.POST("/api/v1/key/unlock", m.Wrap(a.postKeyUnlock))
	m.POST("/api/v1/key/lock", m.Wrap(a.postKeyLock))
	m.POST("/api/v1/key/rotate", m.Wrap(a.postKeyRotate))

	// ── Registry data plane (package-manager traffic) ──
	// Client tools (go, npm, docker, helm, ...) hit /registries/*.
	m.Handle("/registries/*", http.HandlerFunc(m.Wrap(a.serveRegistry)))
	m.Handle("/cdn/npm/*", http.HandlerFunc(m.Wrap(a.serveNPMCDN)))

	// ── Raw mount management (Settings → Raw mounts) ──
	// listRawMounts is the runtime summary; the /configs sibling and
	// the create/update/delete handlers drive the management UI and
	// hot-reload the live mount table after every mutation.
	m.GET("/api/v1/raw-mounts", m.Wrap(a.listRawMounts))
	m.GET("/api/v1/raw-mounts/configs", m.Wrap(a.listRawMountConfigs))
	m.POST("/api/v1/raw-mounts", m.Wrap(a.createRawMount))
	m.PUT("/api/v1/raw-mounts/{prefix}", m.Wrap(a.updateRawMount))
	m.DELETE("/api/v1/raw-mounts/{prefix}", m.Wrap(a.deleteRawMount))

	// ── File serving (FTP / SFTP / TFTP / WebDAV) ──
	// A single settings document drives the four built-in servers; the
	// status sibling reports each protocol's live bind state. Every save
	// reconciles the running servers.
	m.GET("/api/v1/serve", m.Wrap(a.getServeSettings))
	m.PUT("/api/v1/serve", m.Wrap(a.updateServeSettings))
	m.GET("/api/v1/serve/status", m.Wrap(a.getServeStatus))

	// ── Raw mount file browser / serving ──
	m.GET("/api/v1/raw/*", m.Wrap(a.getRaw))
	m.PUT("/api/v1/raw/*", m.Wrap(a.putRaw))
	m.DELETE("/api/v1/raw/*", m.Wrap(a.deleteRaw))
	m.POST("/api/v1/raw-mkdir/*", m.Wrap(a.rawHandler.mkDir))
	m.POST("/api/v1/raw-rename", m.Wrap(a.rawHandler.renameFile))
	m.POST("/api/v1/raw-copy", m.Wrap(a.rawHandler.copyFile))
	m.POST("/api/v1/raw-move", m.Wrap(a.rawHandler.moveFile))

	// ── Registry admin ──
	// GET returns the assembled namespace/repo tree; PUT toggles the
	// deployment-wide feature flag. The tree itself is managed through
	// the granular namespace/repo endpoints below.
	m.GET("/api/v1/registries", m.Wrap(a.listRegistryNamespaces))
	m.PUT("/api/v1/registries", m.Wrap(a.setRegistryFeature))
	m.GET("/api/v1/registries/repos", m.Wrap(a.listRegistryRepos))

	// Granular namespace CRUD.
	m.POST("/api/v1/registries/namespaces", m.Wrap(a.createRegistryNamespace))
	m.PUT("/api/v1/registries/namespaces/{ns}", m.Wrap(a.updateRegistryNamespace))
	m.DELETE("/api/v1/registries/namespaces/{ns}", m.Wrap(a.deleteRegistryNamespace))

	// Granular repository CRUD.
	m.POST("/api/v1/registries/namespaces/{ns}/repos", m.Wrap(a.createRegistryRepository))
	m.GET("/api/v1/registries/namespaces/{ns}/repos/{repo}", m.Wrap(a.getRegistryRepository))
	m.PUT("/api/v1/registries/namespaces/{ns}/repos/{repo}", m.Wrap(a.updateRegistryRepository))
	m.DELETE("/api/v1/registries/namespaces/{ns}/repos/{repo}", m.Wrap(a.deleteRegistryRepository))

	// Per-protocol catalogue listings.
	m.GET("/api/v1/registries/go/{ns}/{repo}/modules", m.Wrap(a.listRegistryGoModules))
	m.GET("/api/v1/registries/go/{ns}/{repo}/modules/*", m.Wrap(a.getGoModuleGoMod))
	m.GET("/api/v1/registries/npm/{ns}/{repo}/packages", m.Wrap(a.listRegistryNPMPackages))
	m.GET("/api/v1/registries/npm/{ns}/{repo}/packages/{name}/readme", m.Wrap(a.getNPMPackageReadme))
	m.GET("/api/v1/registries/docker/{ns}/{repo}/repos", m.Wrap(a.listRegistryDockerRepos))
	m.GET("/api/v1/registries/helm/{ns}/{repo}/charts", m.Wrap(a.listRegistryHelmCharts))
	m.GET("/api/v1/registries/maven/{ns}/{repo}/artifacts", m.Wrap(a.listRegistryMavenArtifacts))
	m.GET("/api/v1/registries/pypi/{ns}/{repo}/packages", m.Wrap(a.listRegistryPyPIPackages))
	m.GET("/api/v1/registries/cargo/{ns}/{repo}/crates", m.Wrap(a.listRegistryCargoCrates))

	// Docker GC (local registries only).
	m.POST("/api/v1/registries/docker/{ns}/{repo}/gc", m.Wrap(a.runDockerGC))
	m.GET("/api/v1/registries/docker/{ns}/{repo}/gc/estimate", m.Wrap(a.estimateDockerGC))

	// Generic per-repo operations. ada's router prefers a literal
	// segment over a param and does NOT backtrack to a sibling param
	// branch, so we cannot register these under a single
	// "/registries/{type}/..." pattern alongside the literal protocol
	// routes above — a request under a literal protocol would never
	// reach the param branch. Instead we register the generic ops
	// under every known protocol's literal segment. resolveRegistry
	// is called with requiredType="" so it resolves purely by ns/repo.
	for _, p := range []string{"go", "npm", "docker", "helm", "maven", "pypi", "cargo"} {
		base := "/api/v1/registries/" + p + "/{ns}/{repo}"
		m.GET(base+"/stats", m.Wrap(a.getRegistryStatsFor("")))
		m.POST(base+"/test-upstream", m.Wrap(a.runRegistryUpstreamProbeFor("")))
		m.POST(base+"/purge", m.Wrap(a.runRegistryPurgeFor("")))
		m.GET(base+"/packages/*", m.Wrap(a.getRegistryPackageDetailFor("")))
		m.DELETE(base+"/packages/*", m.Wrap(a.deleteRegistryPackageArtifactFor("")))
	}

	return nil
}

func (a *api) errorHandler(c *ada.Context, err error) {
	switch {
	case errors.Is(err, service.ErrNotFound):
		c.SetStatus(http.StatusNotFound)
	case errors.Is(err, service.ErrBadRequest):
		c.SetStatus(http.StatusBadRequest)
	case errors.Is(err, service.ErrUnauthorized):
		c.SetStatus(http.StatusUnauthorized)
	case errors.Is(err, service.ErrForbidden):
		c.SetStatus(http.StatusForbidden)
	case errors.Is(err, service.ErrConflict):
		c.SetStatus(http.StatusConflict)
	case errors.Is(err, service.ErrInternal):
		c.SetStatus(http.StatusInternalServerError)
	default:
		c.SetStatus(http.StatusInternalServerError)
	}
	_ = c.SendJSON(response{Message: err.Error()})
}

func (a *api) healthzHandler(c *ada.Context) error {
	return c.SetStatus(http.StatusOK).SendString("OK")
}

// infoHandler returns server metadata, the known capability set and the
// at-rest key status. kutu has no auth, so it always reports the caller
// as fully capable. The SPA reads key_initialized/key_unlocked to decide
// whether to render the unlock screen. This endpoint is on the lockgate
// allowlist so it answers even while the server is locked.
func (a *api) infoHandler(c *ada.Context) error {
	initialized, unlocked := false, true
	if st, err := a.svc.GetKeyStatus(c.Request.Context()); err == nil {
		initialized = st.Initialized
		unlocked = st.Unlocked
	}
	return c.SetStatus(http.StatusOK).SendJSON(struct {
		Info
		AuthEnabled    bool     `json:"auth_enabled"`
		Capabilities   []string `json:"capabilities"`
		KeyInitialized bool     `json:"key_initialized"`
		KeyUnlocked    bool     `json:"key_unlocked"`
	}{
		Info:           a.info,
		AuthEnabled:    false,
		Capabilities:   service.AllCapabilities,
		KeyInitialized: initialized,
		KeyUnlocked:    unlocked,
	})
}

// listRawMounts returns the configured raw mounts for the UI.
func (a *api) listRawMounts(c *ada.Context) error {
	if a.rawHandler == nil {
		return c.SetStatus(http.StatusOK).SendJSON([]MountInfo{})
	}
	return c.SetStatus(http.StatusOK).SendJSON(a.rawHandler.MountsInfo())
}

// authBearerOrSession is the auth gate inherited from pika's raw
// handlers. kutu does not authenticate, so it always allows the
// request. Kept as a method (rather than deleting the call sites) so
// the raw handlers stay a verbatim copy that's easy to diff against
// pika.
func (a *api) authBearerOrSession(c *ada.Context, tokenScope, op, capability, subKey string) error {
	return nil
}
