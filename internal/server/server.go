// Package server wires kutu's runtime: it loads settings, builds the
// raw-mount handler, the artifact-registry manager and the file-serving
// manager, boots authentication, then serves the HTTP admin + data plane
// through ada.
package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/rakunlabs/ada"
	"github.com/rakunlabs/ada/middleware/auth/session"
	mcors "github.com/rakunlabs/ada/middleware/cors"
	mlog "github.com/rakunlabs/ada/middleware/log"
	mrecover "github.com/rakunlabs/ada/middleware/recover"
	mrequestid "github.com/rakunlabs/ada/middleware/requestid"
	mserver "github.com/rakunlabs/ada/middleware/server"
	mtelemetry "github.com/rakunlabs/ada/middleware/telemetry"

	"github.com/rakunlabs/kutu/internal/config"
	"github.com/rakunlabs/kutu/internal/hook"
	"github.com/rakunlabs/kutu/internal/server/api"
	"github.com/rakunlabs/kutu/internal/server/authx"
	"github.com/rakunlabs/kutu/internal/server/lockgate"
	"github.com/rakunlabs/kutu/internal/server/serve"
	"github.com/rakunlabs/kutu/internal/server/vhost"
	"github.com/rakunlabs/kutu/internal/service"
)

// Start builds every subsystem and serves until ctx is cancelled.
func Start(ctx context.Context, cfg *config.Config, svc *service.Service, info api.Info) error {
	// Identify this service in hook user-agents / log lines.
	hook.ServiceName = config.ServiceName
	hook.Version = config.Version

	// ── Hook dispatcher (event bus) ──
	dispatcher := hook.NewDispatcher(256)
	dispatcher.SetEventLogEnabled(svc.EventLogEnabled(ctx))
	dispatcher.Start(ctx)
	if hooks, err := svc.Hooks(ctx); err == nil && len(hooks) > 0 {
		dispatcher.UpdateHooks(hooks)
	}

	// ── Raw mounts ──
	mounts, err := svc.RawMounts(ctx)
	if err != nil {
		return fmt.Errorf("load raw mounts: %w", err)
	}
	rawHandler := api.NewRawHandlerFromMounts(ctx, mounts, dispatcher)

	// ── Shared HTTP listeners (virtual hosts) ──
	// S3 / WebDAV serve instances and dedicated registry listeners
	// publish onto this layer; endpoints on the same port are routed
	// by the request's Host header, with optional per-hostname TLS
	// certificates selected via SNI.
	vhostMgr := vhost.NewManager(ctx)
	defer vhostMgr.Stop()

	// ── File serving (FTP / SFTP / TFTP / WebDAV / S3) ──
	// Shares resolve against the live raw-mount table above. A generated
	// SFTP host key is persisted back onto its server entry so it
	// survives restarts.
	serveMgr := serve.NewManager(ctx, rawHandler, vhostMgr, func(serverID, pem string) {
		cfg, err := svc.GetServeSettings(ctx)
		if err != nil {
			slog.Warn("persist generated SFTP host key: load serve settings", "error", err)
			return
		}
		for i := range cfg.Servers {
			if cfg.Servers[i].ID != serverID {
				continue
			}
			if cfg.Servers[i].SFTP == nil {
				cfg.Servers[i].SFTP = &service.SFTPServeSettings{}
			}
			cfg.Servers[i].SFTP.HostKeyPEM = pem
			if err := svc.SetServeSettings(ctx, cfg); err != nil {
				slog.Warn("persist generated SFTP host key", "server", serverID, "error", err)
			}
			return
		}
	})
	if serveCfg, serr := svc.GetServeSettings(ctx); serr != nil {
		slog.Warn("load serve settings", "error", serr)
	} else {
		serveMgr.Reconcile(serveCfg)
	}
	defer serveMgr.Stop()

	// ── Artifact registry ──
	registryMgr := api.BootRegistryManager(ctx, svc, rawHandler, dispatcher)
	if err := registerRegistryFactories(registryMgr); err != nil {
		return fmt.Errorf("register registry factories: %w", err)
	}
	registryMgr.Reload(ctx, svc.GetRegistrySettings(ctx))
	defer registryMgr.Close()

	// ── HTTP server ──
	server := ada.New()
	server.Use(
		mrecover.Middleware(),
		mserver.Middleware(config.Service),
		mcors.Middleware(),
		mrequestid.Middleware(),
		mlog.Middleware(),
		mtelemetry.Middleware(),
		// Lock gate: 503 the /api/v1 surface while the at-rest key is
		// initialized but not unlocked. Login stays reachable so an
		// administrator can sign in and unlock.
		lockgate.Middleware(svc.KeyManager()),
	)

	// ── Authentication ──
	authSettings := svc.GetAuthSettings(ctx)
	cookie := cookieName(authSettings)
	mgr := authx.New(authx.Deps{
		Svc:          svc,
		SessionStore: authx.NewSessionStore(svc, cookie),
		BasePath:     "/",
		CookieName:   cookie,
		Version:      info.Version,
	})
	if err := mgr.Boot(ctx, authSettings); err != nil {
		return fmt.Errorf("auth manager boot: %w", err)
	}

	// Brute-force protection on POST /login/pass/* and /login/register/*
	// per client IP and per username.
	var rl *service.AuthRateLimitSettings
	if authSettings != nil {
		rl = authSettings.RateLimit
	}
	rl = rl.WithDefaults()
	trustedProxies := authx.ParseCIDRs(rl.TrustedProxyCIDRs)

	mLogin := server.Group("", authx.LoginAudit(trustedProxies), authx.LoginGuard(rl, trustedProxies))
	mgr.Mount(mLogin)

	public := server.Group("")
	data := server.Group("")
	protected := server.Group("", noLoginRedirect, mgr.Require(), mgr.CapMiddleware(), actorMiddleware)

	if err := api.Handle(ctx, api.Muxes{Public: public, Protected: protected, Data: data}, api.Deps{
		Svc:            svc,
		Info:           info,
		Mgr:            mgr,
		RawHandler:     rawHandler,
		ServeMgr:       serveMgr,
		RegistryMgr:    registryMgr,
		VhostMgr:       vhostMgr,
		Dispatcher:     dispatcher,
		RateLimit:      rl,
		TrustedProxies: trustedProxies,
	}); err != nil {
		return err
	}

	// Embedded SPA as the catch-all (registered last so API + data-plane
	// routes take precedence). The SPA renders its own login screen.
	if err := folderHandler(public); err != nil {
		return fmt.Errorf("mount UI: %w", err)
	}

	return server.StartWithContext(ctx, cfg.Server.Addr())
}

// cookieName returns the session cookie name from the auth settings.
func cookieName(s *service.AuthSettings) string {
	if s != nil && s.Cookie.Name != "" {
		return s.Cookie.Name
	}
	return "kutu_session"
}

// noLoginRedirect makes unauthenticated API calls answer 401 JSON instead
// of a 303 to the login page; the SPA renders its own login screen.
func noLoginRedirect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(session.SetDisableRedirect(r.Context(), true)))
	})
}

// actorMiddleware stamps the authenticated user on the context so the
// storage layer's updated_by columns and hook events attribute changes.
func actorMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u := service.UserFromContext(r.Context()); u != "" {
			r = r.WithContext(service.WithActor(r.Context(), u))
		}
		next.ServeHTTP(w, r)
	})
}
