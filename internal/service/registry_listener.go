package service

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"
)

// registry_listener.go — dedicated listeners for registry repositories.
//
// A listener publishes one {namespace, repo} pair on its own host:port,
// optionally scoped to a hostname (virtual host) and optionally behind
// TLS. The registry is served from the *root* of that endpoint, which
// is what protocol clients with rigid URL layouts need — most notably
// Docker, whose CLI always talks to /v2/... at the host root and
// therefore cannot address the path-prefixed
// /registries/{ns}/{repo}/v2/... data plane on the main server.
//
// Several listeners may share a port as long as their hostnames
// differ (Host-header routing; SNI selects the per-hostname TLS
// certificate). TLS is optional: a plain listener still routes by
// hostname, which is the normal mode behind a TLS-terminating
// reverse proxy.
//
// The list is persisted as a kutu_meta singleton (like ServeSettings)
// and reconciled onto the shared vhost layer after every save.

// metaRegistryListeners is the kutu_meta key holding the singleton
// RegistryListenerSettings document.
const metaRegistryListeners = "registry_listeners"

// RegistryListener is one dedicated endpoint for a registry repo.
type RegistryListener struct {
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Enabled bool   `json:"enabled"`

	// Host is the bind address ("" = 0.0.0.0); Port is required.
	Host string `json:"host,omitempty"`
	Port int    `json:"port"`

	// Hostname scopes this listener to one virtual host on the port.
	// Empty = catch-all (answers every Host). "*.example.com"
	// wildcards match one extra label.
	Hostname string `json:"hostname,omitempty"`

	// Target repository, served from the endpoint root.
	Namespace string `json:"namespace"`
	Repo      string `json:"repo"`

	// Optional TLS keypair (PEM, both or neither). Optional on
	// purpose: deployments behind a TLS-terminating reverse proxy
	// run the listener plain.
	TLSCertPEM string `json:"tls_cert_pem,omitempty"`
	TLSKeyPEM  string `json:"tls_key_pem,omitempty"`
}

// RegistryListenerSettings is the persisted listener list.
type RegistryListenerSettings struct {
	Listeners []RegistryListener `json:"listeners,omitempty"`
}

// GetRegistryListeners returns the persisted listener list. A fresh
// install yields an empty document.
func (s *Service) GetRegistryListeners(ctx context.Context) (*RegistryListenerSettings, error) {
	var cfg RegistryListenerSettings
	if _, err := s.store.GetMeta(ctx, metaRegistryListeners, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// SetRegistryListeners persists the listener list. The caller (api
// layer) validates first and reconciles the live listeners afterwards.
func (s *Service) SetRegistryListeners(ctx context.Context, cfg *RegistryListenerSettings) error {
	if cfg == nil {
		cfg = &RegistryListenerSettings{}
	}
	return s.store.SetMeta(ctx, metaRegistryListeners, cfg)
}

// Validate checks the listener list for shape errors. rs supplies the
// current registry tree so target references resolve; it may be nil
// (reference checks are then skipped — used by tests).
//
// Cross-subsystem port sharing (e.g. an S3 vhost and a registry
// listener on the same port) is intentionally NOT rejected here: the
// vhost layer merges them at runtime and reports per-binding
// conflicts. Only conflicts *within* this list are hard errors.
func (rls *RegistryListenerSettings) Validate(rs *RegistrySettings) error {
	if rls == nil {
		return nil
	}
	seenID := map[string]bool{}
	// port → hostname → listener label, for duplicate detection.
	seenHost := map[int]map[string]string{}
	// port → whether any / all listeners carry TLS.
	tlsOnPort := map[int]bool{}
	plainOnPort := map[int]bool{}

	for i := range rls.Listeners {
		l := &rls.Listeners[i]
		label := registryListenerLabel(l)

		if l.ID != "" {
			if seenID[l.ID] {
				return fmt.Errorf("duplicate listener id %q: %w", l.ID, ErrBadRequest)
			}
			seenID[l.ID] = true
		}
		if l.Port < 1 || l.Port > 65535 {
			return fmt.Errorf("listener %q: port must be 1-65535: %w", label, ErrBadRequest)
		}
		l.Hostname = strings.ToLower(strings.TrimSpace(l.Hostname))
		if err := ValidateListenerHostname(l.Hostname); err != nil {
			return fmt.Errorf("listener %q: %w", label, err)
		}
		if l.Namespace == "" || l.Repo == "" {
			return fmt.Errorf("listener %q: namespace and repo are required: %w", label, ErrBadRequest)
		}
		if rs != nil {
			ns := rs.FindNamespace(l.Namespace)
			if ns == nil {
				return fmt.Errorf("listener %q: namespace %q not found: %w", label, l.Namespace, ErrBadRequest)
			}
			if ns.FindRepository(l.Repo) == nil {
				return fmt.Errorf("listener %q: repository %s/%s not found: %w", label, l.Namespace, l.Repo, ErrBadRequest)
			}
		}
		if (l.TLSCertPEM == "") != (l.TLSKeyPEM == "") {
			return fmt.Errorf("listener %q: TLS requires both a certificate and a key: %w", label, ErrBadRequest)
		}
		if l.TLSCertPEM != "" {
			if _, err := tls.X509KeyPair([]byte(l.TLSCertPEM), []byte(l.TLSKeyPEM)); err != nil {
				return fmt.Errorf("listener %q: invalid TLS keypair: %v: %w", label, err, ErrBadRequest)
			}
		}

		if !l.Enabled {
			continue
		}
		if seenHost[l.Port] == nil {
			seenHost[l.Port] = map[string]string{}
		}
		if other, dup := seenHost[l.Port][l.Hostname]; dup {
			if l.Hostname == "" {
				return fmt.Errorf("listeners %q and %q are both catch-all on port %d (set a hostname on one): %w",
					other, label, l.Port, ErrBadRequest)
			}
			return fmt.Errorf("listeners %q and %q both claim hostname %q on port %d: %w",
				other, label, l.Hostname, l.Port, ErrBadRequest)
		}
		seenHost[l.Port][l.Hostname] = label
		if l.TLSCertPEM != "" {
			tlsOnPort[l.Port] = true
		} else {
			plainOnPort[l.Port] = true
		}
	}

	for port := range tlsOnPort {
		if plainOnPort[port] {
			return fmt.Errorf("port %d mixes TLS and plain listeners; every listener on a TLS port needs a certificate: %w",
				port, ErrBadRequest)
		}
	}
	return nil
}

// ValidateListenerHostname checks a virtual-host name: lowercase DNS
// labels, optionally with a single leading "*." wildcard. Empty is
// allowed (catch-all). Exported because serve-server entries reuse
// the same rule for their hostname field.
func ValidateListenerHostname(hostname string) error {
	if hostname == "" {
		return nil
	}
	h := hostname
	h = strings.TrimPrefix(h, "*.")
	if h == "" || strings.Contains(h, "*") {
		return fmt.Errorf("hostname %q: wildcard must be a single leading \"*.\" label: %w", hostname, ErrBadRequest)
	}
	if strings.Contains(h, ":") {
		return fmt.Errorf("hostname %q must not contain a port: %w", hostname, ErrBadRequest)
	}
	if len(h) > 253 {
		return fmt.Errorf("hostname %q too long: %w", hostname, ErrBadRequest)
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" {
			return fmt.Errorf("hostname %q has an empty label: %w", hostname, ErrBadRequest)
		}
		for _, r := range label {
			ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
			if !ok {
				return fmt.Errorf("hostname %q: invalid character %q (allowed: a-z 0-9 - .): %w", hostname, r, ErrBadRequest)
			}
		}
		if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return fmt.Errorf("hostname %q: label %q must not start or end with '-': %w", hostname, label, ErrBadRequest)
		}
	}
	return nil
}

func registryListenerLabel(l *RegistryListener) string {
	if l.Name != "" {
		return l.Name
	}
	if l.ID != "" {
		return l.ID
	}
	return fmt.Sprintf("%s/%s:%d", l.Namespace, l.Repo, l.Port)
}
