// Package api wires the kutu HTTP surface: the artifact registry data
// plane and admin endpoints, the raw-mount file browser, file serving,
// and user / permission / token administration.
//
// Routes live on three muxes: Public (no auth: info, health, key status,
// login), Protected (session or API token required; capabilities
// resolved per request) and Data (registry data plane, which runs its own
// protocol-aware authentication so package managers get the right
// challenge).
package api

import (
	"context"
	"errors"
	"net"
	"net/http"

	"github.com/rakunlabs/ada"

	"github.com/rakunlabs/kutu/internal/hook"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/server/authx"
	"github.com/rakunlabs/kutu/internal/server/serve"
	"github.com/rakunlabs/kutu/internal/server/vhost"
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

// api carries the dependencies every handler needs.
type api struct {
	svc            *service.Service
	info           Info
	mgr            *authx.Manager
	registryTokens *registryTokenSigner
	rawHandler     *RawHandler
	serveMgr       *serve.Manager
	registryMgr    *registry.Manager
	vhostMgr       *vhost.Manager
	dispatcher     *hook.Dispatcher
	appCtx         context.Context
}

type response struct {
	Message string `json:"message,omitempty"`
}

// Muxes groups the route trees the API registers on.
type Muxes struct {
	// Public is reachable without authentication.
	Public *ada.Mux
	// Protected requires a session or API token; capabilities are
	// resolved into the request context.
	Protected *ada.Mux
	// Data carries the registry data plane; handlers authenticate.
	Data *ada.Mux
}

// Deps carries the collaborators the handlers need.
type Deps struct {
	Svc         *service.Service
	Info        Info
	Mgr         *authx.Manager
	RawHandler  *RawHandler
	ServeMgr    *serve.Manager
	RegistryMgr *registry.Manager
	VhostMgr    *vhost.Manager
	Dispatcher  *hook.Dispatcher

	// RateLimit guards the server-key unlock endpoint; nil disables it.
	RateLimit      *service.AuthRateLimitSettings
	TrustedProxies []*net.IPNet
}

// Handle registers every route.
func Handle(ctx context.Context, mux Muxes, deps Deps) error {
	signer, err := newRegistryTokenSigner(ctx, deps.Svc)
	if err != nil {
		return err
	}
	a := &api{
		svc:            deps.Svc,
		info:           deps.Info,
		mgr:            deps.Mgr,
		registryTokens: signer,
		rawHandler:     deps.RawHandler,
		serveMgr:       deps.ServeMgr,
		registryMgr:    deps.RegistryMgr,
		vhostMgr:       deps.VhostMgr,
		dispatcher:     deps.Dispatcher,
		appCtx:         context.Background(),
	}

	// Publish the configured registry listeners onto the shared vhost
	// layer at boot; every later registry mutation re-publishes via
	// reloadRegistry.
	a.reconcileRegistryListeners(a.appCtx)
	a.startRegistryRuntime(ctx)

	pub, m, data := mux.Public, mux.Protected, mux.Data
	pub.ErrorHandler(a.errorHandler)
	m.ErrorHandler(a.errorHandler)
	data.ErrorHandler(a.errorHandler)

	perm := a.withPerm
	settings := func(h func(*ada.Context) error) func(*ada.Context) error {
		return perm(service.CapSettingsManage, h)
	}
	regRead := func(h func(*ada.Context) error) func(*ada.Context) error {
		return a.withRepoPerm(service.CapRegistryRead, h)
	}

	// ── Public ──
	pub.GET("/healthz", pub.Wrap(a.healthzHandler))
	pub.GET("/api/v1/info", pub.Wrap(a.infoHandler))
	pub.GET("/api/v1/key/status", pub.Wrap(a.getKeyStatus))

	// ── Registry data plane (package-manager traffic) ──
	// Client tools (go, npm, docker, helm, ...) hit /registries/*. The
	// handlers authenticate with tokens, Basic credentials or the
	// session and answer with protocol-appropriate challenges.
	data.Handle("/registries/*", http.HandlerFunc(data.Wrap(a.serveRegistry)))
	data.Handle("/cdn/npm/*", http.HandlerFunc(data.Wrap(a.serveNPMCDN)))

	// Raw file serving accepts tokens / Basic too, so it lives on the
	// data mux and checks credentials itself.
	data.GET("/api/v1/raw/*", data.Wrap(a.getRaw))
	data.PUT("/api/v1/raw/*", data.Wrap(a.putRaw))
	data.DELETE("/api/v1/raw/*", data.Wrap(a.deleteRaw))

	// ── At-rest encryption key lifecycle ──
	m.POST("/api/v1/key/initialize", m.Wrap(settings(a.postKeyInitialize)))
	m.POST("/api/v1/key/unlock", m.Wrap(settings(a.postKeyUnlock)),
		authx.UnlockGuard("server-key", deps.RateLimit, deps.TrustedProxies))
	m.POST("/api/v1/key/lock", m.Wrap(settings(a.postKeyLock)))
	m.POST("/api/v1/key/rotate", m.Wrap(settings(a.postKeyRotate)))

	// ── Self-service ──
	m.POST("/api/v1/me/password", m.Wrap(a.changeMyPassword))
	m.POST("/api/v1/me/passkeys/begin", m.Wrap(a.withAccountSecurityAccess(a.beginPasskeyEnroll)))
	m.POST("/api/v1/me/passkeys/finish", m.Wrap(a.withAccountSecurityAccess(a.finishPasskeyEnroll)))
	m.GET("/api/v1/me/passkeys", m.Wrap(a.withAccountSecurityAccess(a.listMyPasskeys)))
	m.PATCH("/api/v1/me/passkeys/*", m.Wrap(a.withAccountSecurityAccess(a.renameMyPasskey)))
	m.DELETE("/api/v1/me/passkeys/*", m.Wrap(a.withAccountSecurityAccess(a.deleteMyPasskey)))
	m.GET("/api/v1/me/totp", m.Wrap(a.withAccountSecurityAccess(a.getMyTOTPStatus)))
	m.POST("/api/v1/me/totp/begin", m.Wrap(a.withAccountSecurityAccess(a.beginMyTOTPEnroll)))
	m.POST("/api/v1/me/totp/finish", m.Wrap(a.withAccountSecurityAccess(a.finishMyTOTPEnroll)))
	m.DELETE("/api/v1/me/totp", m.Wrap(a.withAccountSecurityAccess(a.disableMyTOTP)))
	m.POST("/api/v1/me/totp/recovery-codes", m.Wrap(a.withAccountSecurityAccess(a.regenerateMyTOTPRecoveryCodes)))

	// ── Users ──
	// Sibling paths (/users-kick, /users-totp, ...) instead of nested
	// ones because the /users/* wildcard would swallow them.
	users := func(h func(*ada.Context) error) func(*ada.Context) error {
		return perm(service.CapUsersManage, h)
	}
	m.GET("/api/v1/users", m.Wrap(users(a.listUsers)))
	m.POST("/api/v1/users", m.Wrap(users(a.createUser)))
	m.GET("/api/v1/users/*", m.Wrap(users(a.getUser)))
	m.PATCH("/api/v1/users/*", m.Wrap(users(a.updateUser)))
	m.DELETE("/api/v1/users/*", m.Wrap(users(a.deleteUser)))
	m.POST("/api/v1/users-kick/*", m.Wrap(users(a.kickUser)))
	m.DELETE("/api/v1/users-totp/*", m.Wrap(users(a.adminResetUserTOTP)))
	m.GET("/api/v1/users-effective/{user}", m.Wrap(users(a.getUserEffectivePermissions)))
	m.GET("/api/v1/users-identities/{user}", m.Wrap(users(a.getUserIdentities)))
	m.GET("/api/v1/users-sessions/{user}", m.Wrap(users(a.listUserSessions)))
	m.DELETE("/api/v1/users-sessions/{user}/{handle}", m.Wrap(users(a.revokeUserSession)))

	// ── Permissions ──
	perms := func(h func(*ada.Context) error) func(*ada.Context) error {
		return perm(service.CapPermissionsManage, h)
	}
	m.GET("/api/v1/permissions", m.Wrap(perms(a.listPermissions)))
	m.POST("/api/v1/permissions", m.Wrap(perms(a.createPermission)))
	m.PATCH("/api/v1/permissions/*", m.Wrap(perms(a.updatePermission)))
	m.DELETE("/api/v1/permissions/*", m.Wrap(perms(a.deletePermission)))
	m.GET("/api/v1/user-permissions/*", m.Wrap(perms(a.getUserPermissions)))
	m.PUT("/api/v1/user-permissions/*", m.Wrap(perms(a.setUserPermissions)))
	m.PUT("/api/v1/users-denied/{user}", m.Wrap(perms(a.setUserDeniedPermissions)))

	// ── API tokens ──
	tokens := func(h func(*ada.Context) error) func(*ada.Context) error {
		return perm(service.CapTokensManage, h)
	}
	m.GET("/api/v1/tokens", m.Wrap(tokens(a.listTokens)))
	m.POST("/api/v1/tokens", m.Wrap(tokens(a.createToken)))
	m.PATCH("/api/v1/tokens/*", m.Wrap(tokens(a.patchToken)))
	m.DELETE("/api/v1/tokens/*", m.Wrap(tokens(a.deleteToken)))

	// ── Authentication settings ──
	m.GET("/api/v1/settings/auth", m.Wrap(settings(a.getAuthSettings)))
	m.PUT("/api/v1/settings/auth", m.Wrap(settings(a.putAuthSettings)))

	// ── Raw mount management (Settings → Raw mounts) ──
	// The mount list is visible to file browsers (filtered by scope);
	// configs and mutations need settings.manage.
	m.GET("/api/v1/raw-mounts", m.Wrap(perm(service.CapRawRead, a.listRawMounts)))
	m.GET("/api/v1/raw-mounts/configs", m.Wrap(settings(a.listRawMountConfigs)))
	m.POST("/api/v1/raw-mounts", m.Wrap(settings(a.createRawMount)))
	m.PUT("/api/v1/raw-mounts/{prefix}", m.Wrap(settings(a.updateRawMount)))
	m.DELETE("/api/v1/raw-mounts/{prefix}", m.Wrap(settings(a.deleteRawMount)))

	// ── File serving (FTP / SFTP / TFTP / WebDAV / S3) ──
	m.GET("/api/v1/serve", m.Wrap(settings(a.getServeSettings)))
	m.PUT("/api/v1/serve", m.Wrap(settings(a.updateServeSettings)))
	m.GET("/api/v1/serve/status", m.Wrap(settings(a.getServeStatus)))

	// ── Raw file operations (session / token on the protected mux) ──
	m.POST("/api/v1/raw-mkdir/*", m.Wrap(a.withRawPerm(service.CapRawWrite, false, a.rawHandler.mkDir)))
	m.POST("/api/v1/raw-rename", m.Wrap(a.withRawFileOp(true, a.rawHandler.renameFile)))
	m.POST("/api/v1/raw-copy", m.Wrap(a.withRawFileOp(false, a.rawHandler.copyFile)))
	m.POST("/api/v1/raw-move", m.Wrap(a.withRawFileOp(true, a.rawHandler.moveFile)))

	// ── Registry admin ──
	admin := func(h func(*ada.Context) error) func(*ada.Context) error {
		return perm(service.CapRegistryAdmin, h)
	}
	m.GET("/api/v1/registries", m.Wrap(perm(service.CapRegistryRead, a.listRegistryNamespaces)))
	m.PUT("/api/v1/registries", m.Wrap(admin(a.setRegistryFeature)))
	m.GET("/api/v1/registries/repos", m.Wrap(perm(service.CapRegistryRead, a.listRegistryRepos)))

	m.POST("/api/v1/registries/namespaces", m.Wrap(admin(a.createRegistryNamespace)))
	m.PUT("/api/v1/registries/namespaces/{ns}", m.Wrap(admin(a.updateRegistryNamespace)))
	m.DELETE("/api/v1/registries/namespaces/{ns}", m.Wrap(admin(a.deleteRegistryNamespace)))

	m.POST("/api/v1/registries/namespaces/{ns}/repos", m.Wrap(admin(a.createRegistryRepository)))
	m.GET("/api/v1/registries/namespaces/{ns}/repos/{repo}", m.Wrap(admin(a.getRegistryRepository)))
	m.PUT("/api/v1/registries/namespaces/{ns}/repos/{repo}", m.Wrap(admin(a.updateRegistryRepository)))
	m.DELETE("/api/v1/registries/namespaces/{ns}/repos/{repo}", m.Wrap(admin(a.deleteRegistryRepository)))

	m.GET("/api/v1/registries/go/{ns}/{repo}/modules", m.Wrap(regRead(a.listRegistryGoModules)))
	m.GET("/api/v1/registries/go/{ns}/{repo}/modules/*", m.Wrap(regRead(a.getGoModuleGoMod)))
	m.GET("/api/v1/registries/npm/{ns}/{repo}/packages", m.Wrap(regRead(a.listRegistryNPMPackages)))
	m.GET("/api/v1/registries/npm/{ns}/{repo}/packages/{name}/readme", m.Wrap(regRead(a.getNPMPackageReadme)))
	m.GET("/api/v1/registries/docker/{ns}/{repo}/repos", m.Wrap(regRead(a.listRegistryDockerRepos)))
	m.GET("/api/v1/registries/helm/{ns}/{repo}/charts", m.Wrap(regRead(a.listRegistryHelmCharts)))
	m.GET("/api/v1/registries/maven/{ns}/{repo}/artifacts", m.Wrap(regRead(a.listRegistryMavenArtifacts)))
	m.GET("/api/v1/registries/pypi/{ns}/{repo}/packages", m.Wrap(regRead(a.listRegistryPyPIPackages)))
	m.GET("/api/v1/registries/cargo/{ns}/{repo}/crates", m.Wrap(regRead(a.listRegistryCargoCrates)))

	m.POST("/api/v1/registries/docker/{ns}/{repo}/gc", m.Wrap(admin(a.runDockerGC)))
	m.GET("/api/v1/registries/docker/{ns}/{repo}/gc/estimate", m.Wrap(admin(a.estimateDockerGC)))

	m.GET("/api/v1/registries/metrics", m.Wrap(admin(a.getRegistryMetrics)))
	m.GET("/api/v1/registries/search", m.Wrap(perm(service.CapRegistryRead, a.searchRegistryPackages)))
	m.GET("/api/v1/registries/listeners", m.Wrap(admin(a.getRegistryListeners)))
	m.PUT("/api/v1/registries/listeners", m.Wrap(admin(a.updateRegistryListeners)))
	m.GET("/api/v1/registries/listeners/status", m.Wrap(admin(a.getRegistryListenerStatus)))

	// Generic per-repo operations, registered under every protocol's
	// literal segment: ada's router prefers a literal segment over a
	// param and does NOT backtrack to a sibling param branch.
	for _, p := range service.KnownRegistryTypes {
		base := "/api/v1/registries/" + p + "/{ns}/{repo}"
		m.GET(base+"/entries", m.Wrap(regRead(a.listRegistryEntriesFor(p))))
		m.GET(base+"/usage", m.Wrap(regRead(a.getRegistryUsageFor(p))))
		m.GET(base+"/vulnerabilities/*", m.Wrap(regRead(a.getRegistryVulnerabilitiesFor(p))))
		m.POST(base+"/retention", m.Wrap(a.withRepoPerm(service.CapRegistryDelete, a.runRegistryRetentionFor(p))))
		m.POST(base+"/promote", m.Wrap(a.withRepoPerm(service.CapRegistryRead, a.promoteRegistryVersionFor(p))))
		m.GET(base+"/export", m.Wrap(regRead(a.exportRegistryRepoFor(p))))
		m.POST(base+"/import", m.Wrap(admin(a.importRegistryRepoFor(p))))
		m.POST(base+"/replicate", m.Wrap(admin(a.replicateRegistryRepoFor(p))))
		m.POST(base+"/prefetch", m.Wrap(admin(a.prefetchRegistryRepoFor(p))))
		m.GET(base+"/stats", m.Wrap(regRead(a.getRegistryStatsFor(""))))
		m.POST(base+"/test-upstream", m.Wrap(admin(a.runRegistryUpstreamProbeFor(""))))
		m.POST(base+"/purge", m.Wrap(a.withRepoPerm(service.CapRegistryDelete, a.runRegistryPurgeFor(""))))
		m.GET(base+"/packages/*", m.Wrap(regRead(a.getRegistryPackageDetailFor(""))))
		m.DELETE(base+"/packages/*", m.Wrap(a.withRepoPerm(service.CapRegistryDelete, a.deleteRegistryPackageArtifactFor(""))))
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

// listRawMounts returns the configured raw mounts for the UI.
func (a *api) listRawMounts(c *ada.Context) error {
	if a.rawHandler == nil {
		return c.SetStatus(http.StatusOK).SendJSON([]MountInfo{})
	}
	ctx := c.Request.Context()
	pats := service.CapabilityPatternsFromContext(ctx)
	out := []MountInfo{}
	for _, m := range a.rawHandler.MountsInfo() {
		if pats.AllowsAncestor(service.CapRawRead, m.Prefix) {
			out = append(out, m)
		}
	}
	return c.SetStatus(http.StatusOK).SendJSON(out)
}
