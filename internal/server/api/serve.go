package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/rakunlabs/ada"

	"github.com/rakunlabs/kutu/internal/service"
)

// File-serving management layer. A single JSONB singleton
// (ServeSettings) configures the built-in FTP / SFTP / TFTP / WebDAV
// servers plus the shared user + share lists. Every mutation reconciles
// the live servers via serve.Manager so toggling a protocol on/off — or
// editing a share — takes effect without a restart, the same
// reconcile-after-write contract the proxy + raw-mount CRUD use.

// reconcileServe rebuilds the live serve servers from persisted
// settings. Best-effort: a load error leaves the running servers in
// place. Also called after raw-mount mutations because shares resolve
// against the live mount table.
func (a *api) reconcileServe(ctx context.Context) {
	if a.serveMgr == nil {
		return
	}
	cfg, err := a.svc.GetServeSettings(ctx)
	if err != nil {
		return
	}
	a.serveMgr.Reconcile(cfg)
}

// getServeSettings returns the persisted file-serving configuration.
func (a *api) getServeSettings(c *ada.Context) error {
	cfg, err := a.svc.GetServeSettings(c.Request.Context())
	if err != nil {
		return err
	}
	return c.SetStatus(http.StatusOK).SendJSON(cfg)
}

// updateServeSettings validates and persists the configuration, then
// hot-reloads the live servers. Returns the stored document so the SPA
// can pick up any server-side normalization (e.g. a generated SFTP
// host key is surfaced on the following GET).
func (a *api) updateServeSettings(c *ada.Context) error {
	var cfg service.ServeSettings
	if err := c.Bind(&cfg); err != nil {
		return errors.Join(err, service.ErrBadRequest)
	}
	if err := validateServeSettings(&cfg); err != nil {
		return err
	}
	if err := a.svc.SetServeSettings(c.Request.Context(), &cfg); err != nil {
		return err
	}
	a.reconcileServe(c.Request.Context())
	return c.SetStatus(http.StatusOK).SendJSON(cfg)
}

// getServeStatus exposes the live runtime state of each protocol.
func (a *api) getServeStatus(c *ada.Context) error {
	if a.serveMgr == nil {
		return c.SetStatus(http.StatusOK).SendJSON([]any{})
	}
	return c.SetStatus(http.StatusOK).SendJSON(a.serveMgr.Status())
}

// validateServeSettings rejects a malformed document before it is
// persisted so the operator gets a precise 400 rather than a server
// that silently refuses connections.
func validateServeSettings(cfg *service.ServeSettings) error {
	seenShare := map[string]bool{}
	rootCount := 0
	for i := range cfg.Shares {
		sh := &cfg.Shares[i]
		sh.Name = strings.TrimSpace(sh.Name)
		if sh.Name == "" {
			return errBadRequest("every share needs a name")
		}
		if strings.ContainsAny(sh.Name, "/\\") {
			return errBadRequest(fmt.Sprintf("share name %q must not contain slashes", sh.Name))
		}
		if seenShare[sh.Name] {
			return errBadRequest(fmt.Sprintf("duplicate share name %q", sh.Name))
		}
		seenShare[sh.Name] = true
		if sh.Root {
			rootCount++
		}
		if len(sh.Paths) == 0 {
			return errBadRequest(fmt.Sprintf("share %q needs at least one path", sh.Name))
		}
	}
	if rootCount > 1 {
		return errBadRequest("only one share may be marked as root")
	}

	seenUser := map[string]bool{}
	for i := range cfg.Users {
		u := &cfg.Users[i]
		u.Username = strings.TrimSpace(u.Username)
		if u.Username == "" {
			return errBadRequest("every user needs a username")
		}
		if seenUser[u.Username] {
			return errBadRequest(fmt.Sprintf("duplicate username %q", u.Username))
		}
		seenUser[u.Username] = true
		if u.Password == "" && u.AuthorizedKeys == "" {
			return errBadRequest(fmt.Sprintf("user %q needs a password or an authorized key", u.Username))
		}
		// Referenced shares must exist.
		for _, s := range u.Shares {
			if !seenShare[s] {
				return errBadRequest(fmt.Sprintf("user %q references unknown share %q", u.Username, s))
			}
		}
	}

	// A server with no users accepts no connections (except TFTP, which
	// is anonymous). Warn early only when a protocol that needs auth is
	// enabled without any users.
	if len(cfg.Users) == 0 && (cfg.FTP.Enabled || cfg.SFTP.Enabled || cfg.WebDAV.Enabled) {
		return errBadRequest("FTP / SFTP / WebDAV require at least one user; add a user or disable those protocols")
	}
	return nil
}
