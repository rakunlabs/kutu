package authx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sync"
	"time"

	"github.com/rakunlabs/ada/middleware/auth/identity"

	"github.com/rakunlabs/kutu/internal/service"
)

// Principal is the resolved caller of a data-plane request (registry,
// raw files): who it is and what it may touch.
type Principal struct {
	// Kind is "user" or "token".
	Kind     string
	ID       string // user ID or token ID
	Username string // user name, or token name
	Caps     service.Capabilities
	Patterns service.CapabilityPatterns
}

// Allows reports whether the principal holds capKey on path.
func (p *Principal) Allows(capKey, path string) bool {
	if p == nil {
		return false
	}
	return p.Caps.Has(capKey) && p.Patterns.Allows(capKey, path)
}

// ResolveIdentity runs the capability resolver for an identity obtained
// outside the Require() chain (API token, Basic credentials).
func (m *Manager) ResolveIdentity(ctx context.Context, id *identity.Identity) *Principal {
	m.mu.Lock()
	resolver := m.capResolver
	m.mu.Unlock()
	if resolver == nil || id == nil {
		return nil
	}
	rep := resolver.resolveDetailed(ctx, id)
	p := &Principal{
		Kind:     "user",
		ID:       rep.UserID,
		Username: rep.Username,
		Caps:     service.Capabilities(rep.Capabilities),
		Patterns: service.CapabilityPatterns(rep.Patterns),
	}
	if id.Provider == service.TokenProvider {
		p.Kind = "token"
		p.ID, _ = id.Claims["token_id"].(string)
	}
	return p
}

// SessionPrincipal resolves the browser session cookie on r into a
// Principal. Returns nil when the request carries no valid session.
func (m *Manager) SessionPrincipal(r *http.Request) *Principal {
	id, caps, username, userID, patterns := m.ResolveRequest(r)
	if id == nil {
		return nil
	}
	return &Principal{
		Kind:     "user",
		ID:       userID,
		Username: username,
		Caps:     service.Capabilities(caps),
		Patterns: service.CapabilityPatterns(patterns),
	}
}

// PrincipalForUser rebuilds a user principal from the DB (no IdP roles).
// Used for short-lived registry tokens that only carry the user ID.
func (m *Manager) PrincipalForUser(ctx context.Context, userID string) *Principal {
	user, err := m.deps.Svc.GetUserByID(ctx, userID)
	if err != nil || user.Disabled {
		return nil
	}
	return m.ResolveIdentity(ctx, &identity.Identity{
		Subject:  user.Username,
		Provider: "local",
		Claims:   map[string]any{UserIDClaim: user.ID},
	})
}

// basicCacheTTL bounds how long a verified username/password pair is
// trusted without re-running bcrypt. Package managers send Basic auth on
// every request; a 40ms bcrypt per artifact would dominate latency.
const basicCacheTTL = 2 * time.Minute

type basicCacheEntry struct {
	userID  string
	expires time.Time
}

type basicCache struct {
	mu      sync.Mutex
	entries map[string]basicCacheEntry
}

func basicCacheKey(username, password string) string {
	sum := sha256.Sum256([]byte(username + "\x00" + password))
	return hex.EncodeToString(sum[:])
}

func (c *basicCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || time.Now().After(e.expires) {
		delete(c.entries, key)
		return "", false
	}
	return e.userID, true
}

func (c *basicCache) put(key, userID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]basicCacheEntry)
	}
	if len(c.entries) > 10_000 {
		now := time.Now()
		for k, e := range c.entries {
			if now.After(e.expires) {
				delete(c.entries, k)
			}
		}
	}
	c.entries[key] = basicCacheEntry{userID: userID, expires: time.Now().Add(basicCacheTTL)}
}

// PrincipalForPassword verifies local username/password credentials (as
// sent by package managers via Basic auth) and returns the user's
// principal. Users with TOTP enabled cannot authenticate this way — they
// must use an API token. Returns nil on any failure.
func (m *Manager) PrincipalForPassword(ctx context.Context, username, password string) *Principal {
	if username == "" || password == "" {
		return nil
	}
	key := basicCacheKey(username, password)
	userID, ok := m.basic.get(key)
	if !ok {
		info, err := m.deps.Svc.Authenticate(ctx, username, password)
		if err != nil {
			return nil
		}
		userID = info.ID
		m.basic.put(key, userID)
	}
	// Checked on every request, including cache hits, so enrolling TOTP
	// immediately stops password-only access.
	if m.totpEnabled(ctx, userID) {
		return nil
	}
	return m.PrincipalForUser(ctx, userID)
}

// totpEnabled reports whether the user has a live TOTP enrollment. Errors
// count as enabled (fail closed).
func (m *Manager) totpEnabled(ctx context.Context, userID string) bool {
	coord := m.deps.Svc.TOTPCoord()
	if coord == nil {
		return false
	}
	on, err := coord.IsEnabledForUser(ctx, userID)
	return err != nil || on
}

// PrincipalForToken validates a raw API token and returns its principal.
func (m *Manager) PrincipalForToken(ctx context.Context, raw string) (*Principal, error) {
	id, err := m.deps.Svc.ValidateTokenIdentity(ctx, raw)
	if err != nil {
		return nil, err
	}
	return m.ResolveIdentity(ctx, id), nil
}

// PrincipalForTokenID re-validates a token by ID (active, not expired).
func (m *Manager) PrincipalForTokenID(ctx context.Context, tokenID string) *Principal {
	id, err := m.deps.Svc.TokenIdentityByID(ctx, tokenID)
	if err != nil {
		return nil
	}
	return m.ResolveIdentity(ctx, id)
}
