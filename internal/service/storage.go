package service

import (
	"context"
	"time"

	"github.com/rakunlabs/query"

	"github.com/rakunlabs/kutu/internal/hook"
)

// RegistryRepositoryRow is a repository plus the namespace it belongs to,
// used by flat list responses (GET /api/v1/registries/repos).
type RegistryRepositoryRow struct {
	Namespace string `json:"namespace"`
	RegistryRepository
}

// RegistryStore persists the registry namespace/repository tables.
type RegistryStore interface {
	// LoadRegistryTree assembles every namespace with its repositories.
	LoadRegistryTree(ctx context.Context) (*RegistrySettings, error)
	ListNamespaces(ctx context.Context) ([]RegistryNamespace, error)
	GetNamespace(ctx context.Context, name string) (*RegistryNamespace, error)
	CountNamespaces(ctx context.Context) (int, error)
	CreateNamespace(ctx context.Context, ns *RegistryNamespace) error
	UpdateNamespace(ctx context.Context, ns *RegistryNamespace) error
	DeleteNamespace(ctx context.Context, name string) error
	ListRepositories(ctx context.Context, namespace string, q *query.Query) ([]RegistryRepositoryRow, error)
	GetRepository(ctx context.Context, namespace, name string) (*RegistryRepository, error)
	CreateRepository(ctx context.Context, namespace string, repo *RegistryRepository) error
	UpdateRepository(ctx context.Context, namespace string, repo *RegistryRepository) error
	DeleteRepository(ctx context.Context, namespace, name string) error
}

// RawMountStore persists the raw-mount table.
type RawMountStore interface {
	ListRawMounts(ctx context.Context, q *query.Query) ([]RawMountEntry, error)
	GetRawMount(ctx context.Context, prefix string) (*RawMountEntry, error)
	CreateRawMount(ctx context.Context, m *RawMountEntry) error
	UpdateRawMount(ctx context.Context, m *RawMountEntry) error
	DeleteRawMount(ctx context.Context, prefix string) error
}

// HookStore persists the hook table.
type HookStore interface {
	ListHooks(ctx context.Context) ([]hook.Hook, error)
	ReplaceHooks(ctx context.Context, hooks []hook.Hook) error
}

// MetaStore persists singleton key/value config (encryption verifier,
// feature flags, serve settings).
type MetaStore interface {
	GetMeta(ctx context.Context, key string, dest any) (bool, error)
	SetMeta(ctx context.Context, key string, value any) error
}

// Storage is the full relational persistence surface the service needs.
type Storage interface {
	RegistryStore
	RawMountStore
	HookStore
	MetaStore
	AuthStore
}

// UserStorage manages user records.
type UserStorage interface {
	Create(ctx context.Context, user *User) error
	Get(ctx context.Context, id string) (*User, error)
	GetByUsername(ctx context.Context, username string) (*User, error)
	// GetByEmail returns ErrNotFound when no user has that email or the
	// email is empty.
	GetByEmail(ctx context.Context, email string) (*User, error)
	List(ctx context.Context, q *query.Query) ([]User, int64, error)
	// Update writes every column except the grants and deny overlay,
	// which only change through PermissionStorage.
	Update(ctx context.Context, user *User) error
	// Delete removes the user and cascades to identities, grants,
	// sessions, passkeys and TOTP.
	Delete(ctx context.Context, id string) error
	Count(ctx context.Context) (int64, error)
}

// UserIdentity is an external (OAuth2 / header) credential linked to a
// user. (provider, subject) is globally unique.
type UserIdentity struct {
	ID          string     `json:"id"`
	UserID      string     `json:"user_id"`
	Provider    string     `json:"provider"`
	Subject     string     `json:"subject"`
	Email       string     `json:"email,omitempty"`
	DisplayName string     `json:"display_name,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
}

// UserIdentityStorage manages external identity links.
type UserIdentityStorage interface {
	// Upsert creates or refreshes the (provider, subject) row.
	Upsert(ctx context.Context, identity *UserIdentity) (*UserIdentity, error)
	FindByProviderSubject(ctx context.Context, provider, subject string) (*UserIdentity, error)
	ListByUserID(ctx context.Context, userID string) ([]UserIdentity, error)
	Delete(ctx context.Context, id string) error
	DeleteByUserID(ctx context.Context, userID string) error
}

// Session is an active login session. ID is the raw cookie value.
type Session struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Username  string    `json:"username"`
	Payload   []byte    `json:"payload,omitempty"`
	RefreshID string    `json:"refresh_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// SessionStorage manages login sessions.
type SessionStorage interface {
	// Put creates or replaces the session row.
	Put(ctx context.Context, session *Session) error
	Get(ctx context.Context, id string) (*Session, error)
	Delete(ctx context.Context, id string) error
	DeleteByUserID(ctx context.Context, userID string) error
	DeleteExpired(ctx context.Context) error
	CountByUserID(ctx context.Context, userID string) (int64, error)
	// ListByUserID returns non-expired sessions, newest first.
	ListByUserID(ctx context.Context, userID string) ([]*Session, error)
}

// Permission is a named, assignable bundle of capability keys.
// KeyPatterns optionally narrows a granted key to doublestar globs; a key
// with no patterns is unrestricted.
type Permission struct {
	ID          string              `json:"id"`
	Key         string              `json:"key"`
	Name        string              `json:"name"`
	Description string              `json:"description"`
	Keys        []string            `json:"keys"`
	KeyPatterns map[string][]string `json:"key_patterns,omitempty"`
	CreatedAt   time.Time           `json:"created_at"`
}

// PermissionStorage manages permission bundles and their assignment.
type PermissionStorage interface {
	Create(ctx context.Context, perm *Permission) error
	Get(ctx context.Context, id string) (*Permission, error)
	List(ctx context.Context) ([]Permission, error)
	Update(ctx context.Context, perm *Permission) error
	Delete(ctx context.Context, id string) error
	// SetUserPermissions replaces every assignment for a user.
	SetUserPermissions(ctx context.Context, userID string, permissionIDs []string) error
	GetUserPermissions(ctx context.Context, userID string) ([]Permission, error)
	ListUserIDsByPermission(ctx context.Context, permissionID string) ([]string, error)
	GetUserCapabilityKeys(ctx context.Context, userID string) ([]string, error)
	HasCapabilityKey(ctx context.Context, key string) (bool, error)
	GetUserDeniedCapabilities(ctx context.Context, userID string) ([]string, error)
	SetUserDeniedCapabilities(ctx context.Context, userID string, keys []string) error
}

// TokenStorage manages API access tokens.
type TokenStorage interface {
	Create(ctx context.Context, token *Token) error
	Get(ctx context.Context, id string) (*Token, error)
	FindByHash(ctx context.Context, hashedKey string) (*Token, error)
	List(ctx context.Context, q *query.Query) ([]Token, int64, error)
	Update(ctx context.Context, token *Token) error
	// TouchLastUsed moves last_used_at forward; a missing token is not
	// an error.
	TouchLastUsed(ctx context.Context, id string, at time.Time) error
	Delete(ctx context.Context, id string) error
}

// PasskeyCredential is one WebAuthn credential bound to a user.
type PasskeyCredential struct {
	ID              string    `json:"id"`
	UserID          string    `json:"user_id"`
	CredentialID    []byte    `json:"credential_id"`
	PublicKey       []byte    `json:"-"`
	AAGUID          []byte    `json:"aaguid,omitempty"`
	SignCount       uint32    `json:"sign_count"`
	Transports      []string  `json:"transports,omitempty"`
	UserVerified    bool      `json:"user_verified"`
	BackupEligible  bool      `json:"backup_eligible"`
	BackupState     bool      `json:"backup_state"`
	AttestationType string    `json:"attestation_type,omitempty"`
	Name            string    `json:"name"`
	CreatedAt       time.Time `json:"created_at"`
	LastUsedAt      time.Time `json:"last_used_at,omitempty"`
}

// PasskeyStorage manages WebAuthn credentials.
type PasskeyStorage interface {
	Create(ctx context.Context, c *PasskeyCredential) error
	Get(ctx context.Context, id string) (*PasskeyCredential, error)
	FindByCredentialID(ctx context.Context, credentialID []byte) (*PasskeyCredential, error)
	ListByUserID(ctx context.Context, userID string) ([]PasskeyCredential, error)
	Update(ctx context.Context, c *PasskeyCredential) error
	Delete(ctx context.Context, id string) error
	DeleteByUserID(ctx context.Context, userID string) error
}

// PasskeyChallenge is one in-flight WebAuthn ceremony. Data is the
// JSON-encoded ada passkey.SessionData.
type PasskeyChallenge struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	UserID    string    `json:"user_id,omitempty"`
	Data      []byte    `json:"-"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// PasskeyChallengeStorage persists short-lived WebAuthn ceremony state so
// begin and finish may land on different replicas.
type PasskeyChallengeStorage interface {
	Save(ctx context.Context, c *PasskeyChallenge) error
	Get(ctx context.Context, id string) (*PasskeyChallenge, error)
	// Consume atomically reads and deletes a row; ErrNotFound if absent.
	Consume(ctx context.Context, id string) (*PasskeyChallenge, error)
	Delete(ctx context.Context, id string) error
	DeleteExpired(ctx context.Context) (int, error)
}

// UserTOTP is a user's TOTP second factor. Secret and RecoveryCodes
// (bcrypt hashes) never leave the service layer.
type UserTOTP struct {
	UserID        string    `json:"user_id"`
	Secret        string    `json:"-"`
	Enabled       bool      `json:"enabled"`
	RecoveryCodes []string  `json:"-"`
	CreatedAt     time.Time `json:"created_at"`
	LastUsedAt    time.Time `json:"last_used_at,omitempty"`
}

// UserTOTPStorage manages per-user TOTP state.
type UserTOTPStorage interface {
	Get(ctx context.Context, userID string) (*UserTOTP, error)
	Set(ctx context.Context, t *UserTOTP) error
	Delete(ctx context.Context, userID string) error
}

// AuthStore groups the identity, access-control and session tables.
type AuthStore interface {
	Users() UserStorage
	UserIdentities() UserIdentityStorage
	Sessions() SessionStorage
	Permissions() PermissionStorage
	Tokens() TokenStorage
	Passkeys() PasskeyStorage
	PasskeyChallenges() PasskeyChallengeStorage
	UserTOTPs() UserTOTPStorage
}
