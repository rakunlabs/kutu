package api

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/rakunlabs/ada"

	"github.com/rakunlabs/kutu/internal/server/serve"
	"github.com/rakunlabs/kutu/internal/service"
)

// File-serving management layer. A single JSONB singleton
// (ServeSettings) holds the list of configured server instances
// (FTP / SFTP / TFTP / WebDAV / S3, any number, any ports) plus the
// shared user + share pools. Every mutation reconciles the live servers
// via serve.Manager so adding an instance, toggling one on/off — or
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
// that silently refuses connections. It also normalizes: server entries
// without an ID get one assigned.
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
	hasPasswordUser := false
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
		if u.Password != "" {
			hasPasswordUser = true
		}
		// Referenced shares must exist.
		for _, s := range u.Shares {
			if !seenShare[s] {
				return errBadRequest(fmt.Sprintf("user %q references unknown share %q", u.Username, s))
			}
		}
	}

	if err := validateServeServers(cfg, seenShare, hasPasswordUser); err != nil {
		return err
	}
	return nil
}

// validProtocols is the closed set of serve protocols.
var validProtocols = map[string]bool{"ftp": true, "sftp": true, "tftp": true, "webdav": true, "s3": true}

// validateServeServers checks the server-instance list: known protocol,
// unique IDs, existing share references, port conflicts and
// per-protocol constraints (users for authenticated protocols, TLS
// pairing / password users).
//
// Port rules: FTP/SFTP/TFTP own their port exclusively. S3 and WebDAV
// serve HTTP through the shared vhost layer, so several instances may
// share one TCP port as long as their hostnames differ — the same rule
// registry listeners follow. Mixing TLS and plain instances on one
// port is rejected (the listener is either TLS or not).
func validateServeServers(cfg *service.ServeSettings, shareExists map[string]bool, hasPasswordUser bool) error {
	seenID := map[string]bool{}
	// transport+port → server display name, for exclusive protocols;
	// hosts are not compared because "" / 0.0.0.0 wildcards overlap
	// every address anyway.
	seenPort := map[string]string{}
	// TCP port → hostname → display name, for vhost-shared protocols.
	seenVhost := map[int]map[string]string{}
	tlsOnPort := map[int]bool{}
	plainOnPort := map[int]bool{}

	for i := range cfg.Servers {
		e := &cfg.Servers[i]
		e.Name = strings.TrimSpace(e.Name)
		e.Protocol = strings.ToLower(strings.TrimSpace(e.Protocol))
		if !validProtocols[e.Protocol] {
			return errBadRequest(fmt.Sprintf("server %q has unknown protocol %q", serverLabel(e), e.Protocol))
		}
		if e.ID == "" {
			e.ID = newServeServerID(e.Protocol)
		}
		if seenID[e.ID] {
			return errBadRequest(fmt.Sprintf("duplicate server id %q", e.ID))
		}
		seenID[e.ID] = true

		for _, s := range e.Shares {
			if !shareExists[s] {
				return errBadRequest(fmt.Sprintf("server %q references unknown share %q", serverLabel(e), s))
			}
		}

		hostname, certPEM, keyPEM := serverVhostFields(e)
		hostname = strings.ToLower(strings.TrimSpace(hostname))
		if err := service.ValidateListenerHostname(hostname); err != nil {
			return errBadRequest(fmt.Sprintf("server %q: %v", serverLabel(e), err))
		}
		if hostname != "" && !serverIsVhost(e.Protocol) {
			return errBadRequest(fmt.Sprintf("server %q: %s cannot route by hostname (the protocol carries no Host information); hostname is only supported for s3 and webdav", serverLabel(e), e.Protocol))
		}
		if (certPEM == "") != (keyPEM == "") {
			return errBadRequest(fmt.Sprintf("server %q: TLS requires both a certificate and a key", serverLabel(e)))
		}
		if certPEM != "" {
			if _, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM)); err != nil {
				return errBadRequest(fmt.Sprintf("server %q: invalid TLS keypair: %v", serverLabel(e), err))
			}
		}

		if !e.Enabled {
			continue
		}

		transport, port := serverBind(e)
		if serverIsVhost(e.Protocol) {
			if seenVhost[port] == nil {
				seenVhost[port] = map[string]string{}
			}
			if other, taken := seenVhost[port][hostname]; taken {
				if hostname == "" {
					return errBadRequest(fmt.Sprintf("servers %q and %q are both catch-all on port %d (set a hostname on one)", other, serverLabel(e), port))
				}
				return errBadRequest(fmt.Sprintf("servers %q and %q both claim hostname %q on port %d", other, serverLabel(e), hostname, port))
			}
			seenVhost[port][hostname] = serverLabel(e)
			if certPEM != "" {
				tlsOnPort[port] = true
			} else {
				plainOnPort[port] = true
			}
			if other, taken := seenPort[fmt.Sprintf("tcp:%d", port)]; taken {
				return errBadRequest(fmt.Sprintf("servers %q and %q both want tcp port %d", other, serverLabel(e), port))
			}
		} else {
			key := fmt.Sprintf("%s:%d", transport, port)
			if other, taken := seenPort[key]; taken {
				return errBadRequest(fmt.Sprintf("servers %q and %q both want %s port %d", other, serverLabel(e), transport, port))
			}
			seenPort[key] = serverLabel(e)
			if transport == "tcp" {
				if hosts, taken := seenVhost[port]; taken && len(hosts) > 0 {
					return errBadRequest(fmt.Sprintf("server %q wants tcp port %d, which is already used by an s3/webdav virtual host", serverLabel(e), port))
				}
			}
		}

		// Protocols with authentication need at least one user.
		if e.Protocol != "tftp" && len(cfg.Users) == 0 {
			return errBadRequest(fmt.Sprintf("server %q (%s) requires at least one user; add a user or disable it", serverLabel(e), e.Protocol))
		}

		// S3 SigV4 needs the plaintext secret (the password); a
		// key-only user cannot authenticate against the S3 endpoint.
		if e.Protocol == "s3" && !hasPasswordUser {
			return errBadRequest(fmt.Sprintf("server %q: S3 requires at least one user with a password (used as the secret key)", serverLabel(e)))
		}
	}

	for port := range tlsOnPort {
		if plainOnPort[port] {
			return errBadRequest(fmt.Sprintf("port %d mixes TLS and plain instances; every instance on a TLS port needs a certificate", port))
		}
	}
	return nil
}

// serverIsVhost reports whether the protocol serves HTTP through the
// shared vhost layer (may share a port, split by hostname).
func serverIsVhost(protocol string) bool {
	return protocol == "s3" || protocol == "webdav"
}

// serverVhostFields extracts the hostname + TLS PEM pair of an S3 /
// WebDAV entry (zero values for the other protocols — their TLS is
// configured differently and hostname is unsupported).
func serverVhostFields(e *service.ServeServerEntry) (hostname, certPEM, keyPEM string) {
	switch e.Protocol {
	case "s3":
		if e.S3 != nil {
			return e.S3.Hostname, e.S3.TLSCertPEM, e.S3.TLSKeyPEM
		}
	case "webdav":
		if e.WebDAV != nil {
			return e.WebDAV.Hostname, e.WebDAV.TLSCertPEM, e.WebDAV.TLSKeyPEM
		}
	}
	return "", "", ""
}

// serverLabel is the human-readable identifier used in validation
// errors: the display name when set, otherwise the ID.
func serverLabel(e *service.ServeServerEntry) string {
	if e.Name != "" {
		return e.Name
	}
	if e.ID != "" {
		return e.ID
	}
	return e.Protocol
}

// serverBind returns the transport ("tcp"/"udp") and effective port of
// a server entry, applying protocol defaults.
func serverBind(e *service.ServeServerEntry) (string, int) {
	transport := "tcp"
	port := 0
	switch e.Protocol {
	case "ftp":
		if e.FTP != nil {
			port = e.FTP.Port
		}
	case "sftp":
		if e.SFTP != nil {
			port = e.SFTP.Port
		}
	case "tftp":
		transport = "udp"
		if e.TFTP != nil {
			port = e.TFTP.Port
		}
	case "webdav":
		if e.WebDAV != nil {
			port = e.WebDAV.Port
		}
	case "s3":
		if e.S3 != nil {
			port = e.S3.Port
		}
	}
	if port == 0 {
		port = serve.DefaultPort(e.Protocol)
	}
	return transport, port
}

// newServeServerID generates a short random id for a server entry, e.g.
// "ftp-9f2c1a".
func newServeServerID(protocol string) string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return protocol + "-" + hex.EncodeToString(b[:])
}
