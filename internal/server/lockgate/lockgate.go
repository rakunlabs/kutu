// Package lockgate provides an HTTP middleware that returns 503 for API
// requests when the server's at-rest encryption key has been
// initialized but not yet unlocked.
//
// Scope: only the /api/v1/ admin surface is gated. The registry data
// plane (/registries, /cdn) and raw serving keep working while locked —
// they only fail when a request genuinely needs a sealed secret (e.g. a
// remote registry's upstream credentials), which the registry layer
// reports on its own.
//
// Fresh-install behaviour: until a verifier exists on disk the gate is a
// no-op even on /api/v1/ — encryption is opt-in. Once a verifier is
// written, every restart enters the locked state and the gate engages
// until an operator unlocks (via config password auto-unlock or the UI
// unlock screen).
//
// Allowlist while locked: /api/v1/info, /api/v1/key/status and
// /api/v1/key/unlock pass through so the SPA can drive the unlock flow.
// Every gated 503 carries `X-Kutu-Locked: true` so the SPA's HTTP
// interceptor can switch to the unlock screen.
package lockgate

import (
	"net/http"
	"strings"

	"github.com/rakunlabs/kutu/internal/secret/keymgr"
)

// Middleware gates /api/v1/ requests on the manager being unlocked.
func Middleware(mgr *keymgr.Manager) func(http.Handler) http.Handler {
	const gatedPrefix = "/api/v1/"
	allowExact := map[string]struct{}{
		"/api/v1/info":       {},
		"/api/v1/key/status": {},
		"/api/v1/key/unlock": {},
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// No manager, fresh install, or already unlocked: pass.
			if mgr == nil || !mgr.Initialized() || mgr.IsUnlocked() {
				next.ServeHTTP(w, r)
				return
			}
			// Locked: only gate the admin API surface.
			if !strings.HasPrefix(r.URL.Path, gatedPrefix) {
				next.ServeHTTP(w, r)
				return
			}
			if _, ok := allowExact[r.URL.Path]; ok {
				next.ServeHTTP(w, r)
				return
			}
			writeLocked(w)
		})
	}
}

func writeLocked(w http.ResponseWriter) {
	w.Header().Set("X-Kutu-Locked", "true")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(`{"code":"server_locked","message":"Server is locked. An administrator must unlock it."}`))
}
