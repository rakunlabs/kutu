package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rakunlabs/kutu/internal/server/authx"
	"github.com/rakunlabs/kutu/internal/service"
)

// Docker clients authenticate with the bearer-token flow: /v2/ answers 401
// with a WWW-Authenticate challenge pointing at /v2/token, the client
// exchanges Basic credentials there for a short-lived bearer token, and
// sends that on every following request.
//
// kutu mints these tokens itself as compact HS256 JWTs. They carry only
// the principal reference (user ID or API token ID); permissions are
// re-resolved on every request, so revoking the user or token, or
// changing their permissions, takes effect immediately.

const registryTokenLifetime = 5 * time.Minute

type registryTokenClaims struct {
	Subject string `json:"sub"`
	Kind    string `json:"knd"` // "user" | "token"
	ID      string `json:"pid"`
	Issued  int64  `json:"iat"`
	Expires int64  `json:"exp"`
}

type registryTokenSigner struct {
	key []byte
}

// newRegistryTokenSigner loads the signing key from kutu_meta, creating
// it on first boot so every replica shares one key.
func newRegistryTokenSigner(ctx context.Context, svc *service.Service) (*registryTokenSigner, error) {
	key, err := svc.RegistryTokenKey(ctx)
	if err != nil {
		return nil, err
	}
	return &registryTokenSigner{key: key}, nil
}

func (s *registryTokenSigner) Issue(p *authx.Principal) (string, error) {
	if s == nil || len(s.key) == 0 {
		return "", errors.New("registry token signer not configured")
	}
	now := time.Now().Unix()
	claims := registryTokenClaims{
		Subject: p.Username,
		Kind:    p.Kind,
		ID:      p.ID,
		Issued:  now,
		Expires: now + int64(registryTokenLifetime.Seconds()),
	}
	header, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	input := b64url(header) + "." + b64url(payload)
	return input + "." + b64url(s.sign(input)), nil
}

func (s *registryTokenSigner) Verify(token string) (*registryTokenClaims, error) {
	if s == nil || len(s.key) == 0 {
		return nil, errors.New("registry token signer not configured")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("not a registry token")
	}
	got, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(got, s.sign(parts[0]+"."+parts[1])) {
		return nil, errors.New("invalid registry token signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var claims registryTokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, err
	}
	if claims.Expires < time.Now().Unix() {
		return nil, errors.New("registry token expired")
	}
	return &claims, nil
}

func (s *registryTokenSigner) sign(input string) []byte {
	h := hmac.New(sha256.New, s.key)
	_, _ = h.Write([]byte(input))
	return h.Sum(nil)
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// principalFromRegistryToken re-resolves the principal behind a registry
// bearer token. nil when the user/token has since been disabled or removed.
func (a *api) principalFromRegistryToken(r *http.Request, c *registryTokenClaims) *authx.Principal {
	switch c.Kind {
	case "user":
		return a.mgr.PrincipalForUser(r.Context(), c.ID)
	case "token":
		return a.mgr.PrincipalForTokenID(r.Context(), c.ID)
	}
	return nil
}

// dockerRealm reconstructs the public URL of the /v2/token endpoint that
// pairs with the registry prefix of this request.
func dockerRealm(r *http.Request, prefix string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}
	host := r.Host
	if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
		host = fh
	}
	return scheme + "://" + host + prefix + "/v2/token"
}

// isDockerRequest reports whether a registry sub-path is Docker's /v2 API.
func isDockerRequest(regType, rest string) bool {
	return regType == service.RegistryTypeDocker && (rest == "/v2" || strings.HasPrefix(rest, "/v2/"))
}

// writeRegistryAuthError answers a failed registry authorization in the
// shape the client expects: Docker gets its bearer challenge; every other
// protocol gets a Basic challenge so npm/pip/maven/cargo/helm prompt for
// or send credentials.
func writeRegistryAuthError(w http.ResponseWriter, r *http.Request, docker bool, prefix string, err error) {
	status := http.StatusUnauthorized
	if errors.Is(err, service.ErrForbidden) {
		status = http.StatusForbidden
	}
	w.Header().Set("Content-Type", "application/json")
	if docker {
		if status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate",
				fmt.Sprintf(`Bearer realm=%q,service="kutu"`, dockerRealm(r, prefix)))
		}
		code := "UNAUTHORIZED"
		if status == http.StatusForbidden {
			code = "DENIED"
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"errors": []map[string]string{{"code": code, "message": err.Error()}},
		})
		return
	}
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="kutu"`)
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": err.Error()})
}

// serveDockerToken handles GET {prefix}/v2/token. The client presents
// Basic credentials (username+password, or anything + a kutu API token);
// an authenticated caller gets a bearer token. Scope checks happen on
// each subsequent request, not here.
func (a *api) serveDockerToken(w http.ResponseWriter, r *http.Request, prefix string) {
	p, err := a.dataPrincipal(r)
	if err == nil && p == nil {
		err = fmt.Errorf("%w: %w", errNoCredentials, service.ErrUnauthorized)
	}
	if err != nil {
		writeRegistryAuthError(w, r, true, prefix, err)
		return
	}
	token, err := a.registryTokens.Issue(p)
	if err != nil {
		writeRegistryAuthError(w, r, true, prefix, fmt.Errorf("%v: %w", err, service.ErrUnauthorized))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"token":        token,
		"access_token": token,
		"expires_in":   int(registryTokenLifetime.Seconds()),
		"issued_at":    time.Now().UTC().Format(time.RFC3339),
	})
}
