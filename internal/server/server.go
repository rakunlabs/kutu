// Package server wires kutu's runtime: it loads settings, builds the
// raw-mount handler, the artifact-registry manager and the user-built
// proxy manager, then serves the HTTP admin + data plane through ada.
// There is no authentication — a capability-planting middleware marks
// every request as fully capable so the checks inherited from pika are
// satisfied.
package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/rakunlabs/ada"
	mcors "github.com/rakunlabs/ada/middleware/cors"
	mlog "github.com/rakunlabs/ada/middleware/log"
	mrecover "github.com/rakunlabs/ada/middleware/recover"
	mrequestid "github.com/rakunlabs/ada/middleware/requestid"
	mserver "github.com/rakunlabs/ada/middleware/server"
	mtelemetry "github.com/rakunlabs/ada/middleware/telemetry"

	"github.com/rakunlabs/kutu/internal/config"
	"github.com/rakunlabs/kutu/internal/hook"
	"github.com/rakunlabs/kutu/internal/server/api"
	"github.com/rakunlabs/kutu/internal/server/lockgate"
	"github.com/rakunlabs/kutu/internal/server/proxy"
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

	// ── Artifact registry ──
	registryMgr := api.BootRegistryManager(ctx, svc, rawHandler, dispatcher)
	if err := registerRegistryFactories(registryMgr); err != nil {
		return fmt.Errorf("register registry factories: %w", err)
	}
	registryMgr.Reload(ctx, svc.GetRegistrySettings(ctx))
	defer registryMgr.Close()

	// ── User-built proxy ──
	adapter := &proxy.ServiceAdapter{
		S:          svc,
		RawMounts:  rawHandler,
		Registries: registryMgr,
	}
	proxyMgr := proxy.NewManager(ctx, adapter, cfg.Server.Host, []string{cfg.Server.Port})
	listeners, _ := svc.ListProxyListeners(ctx, nil)
	servers, _ := svc.ListProxyServers(ctx, nil)
	if rerr := proxyMgr.ReconcileAll(listeners, servers); rerr != nil {
		slog.Warn("initial proxy reconcile reported issues", "error", rerr)
	}
	defer proxyMgr.Stop()

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
		// initialized but not unlocked. No-op on fresh installs.
		lockgate.Middleware(svc.KeyManager()),
		capabilityMiddleware, // kutu has no auth: every request is fully capable
		actorMiddleware,      // capture the optional X-User header for audit
	)

	if err := api.Handle(server.Mux, svc, info, rawHandler, proxyMgr, registryMgr, dispatcher); err != nil {
		return err
	}

	// Embedded SPA as the catch-all (registered last so API + data-plane
	// routes take precedence).
	if err := folderHandler(server.Mux); err != nil {
		return fmt.Errorf("mount UI: %w", err)
	}

	slog.Info("kutu listening", "addr", cfg.Server.Addr())
	return server.StartWithContext(ctx, cfg.Server.Addr())
}

// capabilityMiddleware plants the full capability set on every request.
// kutu does not authenticate, so the capability checks copied from pika
// always pass.
func capabilityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(service.WithAllCapabilities(r.Context())))
	})
}

// actorMiddleware threads the optional X-User request header into the
// context so the storage layer can stamp updated_by and the registry
// handlers can attribute hook events. kutu has no auth, so this is a
// best-effort, caller-supplied attribution only.
func actorMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u := r.Header.Get("X-User"); u != "" {
			r = r.WithContext(service.WithActor(r.Context(), u))
		}
		next.ServeHTTP(w, r)
	})
}
