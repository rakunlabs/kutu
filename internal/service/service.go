package service

import (
	"context"
	"fmt"

	"github.com/rakunlabs/query"

	"github.com/rakunlabs/kutu/internal/hook"
	"github.com/rakunlabs/kutu/internal/secret/keymgr"
)

// Meta keys for singleton config stored in kutu_meta.
const (
	metaRegistryDisabled = "registry_disabled"
	metaProxyDisabled    = "proxy_disabled"
	metaEventLogDisabled = "event_log_disabled"
	metaEncVerifier      = "encryption_verifier"
)

// Service is the transport-agnostic application core for kutu. It owns
// the relational config store and exposes per-entity CRUD plus the few
// aggregate views the registry/proxy managers need.
//
// Authentication is intentionally absent: kutu runs as a plain,
// unauthenticated server. ValidateToken always succeeds.
type Service struct {
	store Storage

	// keyManager owns the at-rest encryption key lifecycle. nil-safe:
	// the keyops methods return an error when it isn't wired.
	keyManager *keymgr.Manager
}

// New constructs a Service backed by the given storage.
func New(store Storage) *Service {
	return &Service{store: store}
}

// ── feature flags (meta) ──

func (s *Service) boolFlag(ctx context.Context, key string) bool {
	var v bool
	_, _ = s.store.GetMeta(ctx, key, &v)
	return v
}

// RegistryEnabled reports whether the artifact-registry feature is on.
func (s *Service) RegistryEnabled(ctx context.Context) bool {
	return !s.boolFlag(ctx, metaRegistryDisabled)
}

// SetRegistryDisabled toggles the registry feature flag.
func (s *Service) SetRegistryDisabled(ctx context.Context, disabled bool) error {
	return s.store.SetMeta(ctx, metaRegistryDisabled, disabled)
}

// ProxyEnabled reports whether the user-built proxy feature is on.
func (s *Service) ProxyEnabled(ctx context.Context) bool {
	return !s.boolFlag(ctx, metaProxyDisabled)
}

// SetProxyDisabled toggles the proxy feature flag.
func (s *Service) SetProxyDisabled(ctx context.Context, disabled bool) error {
	return s.store.SetMeta(ctx, metaProxyDisabled, disabled)
}

// EventLogEnabled reports whether the built-in event log is on (default).
func (s *Service) EventLogEnabled(ctx context.Context) bool {
	return !s.boolFlag(ctx, metaEventLogDisabled)
}

// ── registry ──

// GetRegistrySettings assembles the full registry tree (namespaces +
// repositories) plus the deployment-wide disabled flag.
func (s *Service) GetRegistrySettings(ctx context.Context) *RegistrySettings {
	rs, err := s.store.LoadRegistryTree(ctx)
	if err != nil || rs == nil {
		return nil
	}
	rs.Disabled = !s.RegistryEnabled(ctx)
	return rs
}

// ListNamespaces returns the namespaces (without repositories).
func (s *Service) ListNamespaces(ctx context.Context) ([]RegistryNamespace, error) {
	return s.store.ListNamespaces(ctx)
}

// CreateNamespace validates and inserts a namespace.
func (s *Service) CreateNamespace(ctx context.Context, ns *RegistryNamespace) error {
	if err := validateRegistryName(ns.Name); err != nil {
		return fmt.Errorf("namespace name: %w", err)
	}
	if existing, _ := s.store.GetNamespace(ctx, ns.Name); existing != nil {
		return fmt.Errorf("namespace %q already exists: %w", ns.Name, ErrConflict)
	}
	ns.Repositories = nil
	return s.store.CreateNamespace(ctx, ns)
}

// UpdateNamespace updates a namespace's description.
func (s *Service) UpdateNamespace(ctx context.Context, ns *RegistryNamespace) error {
	return s.store.UpdateNamespace(ctx, ns)
}

// DeleteNamespace removes a namespace and its repositories.
func (s *Service) DeleteNamespace(ctx context.Context, name string) error {
	return s.store.DeleteNamespace(ctx, name)
}

// ListRepositories returns repositories, optionally scoped to one
// namespace, filtered/sorted/paged by q.
func (s *Service) ListRepositories(ctx context.Context, namespace string, q *query.Query) ([]RegistryRepositoryRow, error) {
	return s.store.ListRepositories(ctx, namespace, q)
}

// GetRepository returns one repository.
func (s *Service) GetRepository(ctx context.Context, namespace, name string) (*RegistryRepository, error) {
	return s.store.GetRepository(ctx, namespace, name)
}

// CreateRepository validates (against the namespace's existing repos for
// virtual member resolution) and inserts a repository.
func (s *Service) CreateRepository(ctx context.Context, namespace string, repo *RegistryRepository) error {
	if _, err := s.store.GetNamespace(ctx, namespace); err != nil {
		return err
	}
	if existing, _ := s.store.GetRepository(ctx, namespace, repo.Name); existing != nil {
		return fmt.Errorf("repository %s/%s already exists: %w", namespace, repo.Name, ErrConflict)
	}
	if err := s.validateRepoInNamespace(ctx, namespace, repo, false); err != nil {
		return err
	}
	return s.store.CreateRepository(ctx, namespace, repo)
}

// UpdateRepository validates and updates a repository.
func (s *Service) UpdateRepository(ctx context.Context, namespace string, repo *RegistryRepository) error {
	if _, err := s.store.GetRepository(ctx, namespace, repo.Name); err != nil {
		return err
	}
	if err := s.validateRepoInNamespace(ctx, namespace, repo, true); err != nil {
		return err
	}
	return s.store.UpdateRepository(ctx, namespace, repo)
}

// DeleteRepository removes a repository.
func (s *Service) DeleteRepository(ctx context.Context, namespace, name string) error {
	return s.store.DeleteRepository(ctx, namespace, name)
}

// validateRepoInNamespace builds the namespace's effective repository
// set (existing repos with the target replaced/added) and runs the full
// tree validator so virtual-member references resolve correctly.
func (s *Service) validateRepoInNamespace(ctx context.Context, namespace string, repo *RegistryRepository, replace bool) error {
	rows, err := s.store.ListRepositories(ctx, namespace, nil)
	if err != nil {
		return err
	}
	repos := make([]RegistryRepository, 0, len(rows)+1)
	found := false
	for i := range rows {
		if rows[i].Name == repo.Name {
			repos = append(repos, *repo)
			found = true
			continue
		}
		repos = append(repos, rows[i].RegistryRepository)
	}
	if !found {
		repos = append(repos, *repo)
	}
	rs := &RegistrySettings{Namespaces: []RegistryNamespace{{Name: namespace, Repositories: repos}}}
	return rs.Validate()
}

// EnsureDefaultRegistryNamespace creates the "default" namespace on a
// fresh install. Non-destructive: does nothing when any namespace exists.
func (s *Service) EnsureDefaultRegistryNamespace(ctx context.Context) error {
	n, err := s.store.CountNamespaces(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	err = s.store.CreateNamespace(ctx, &RegistryNamespace{Name: DefaultRegistryNamespace})
	// Tolerate a concurrent bootstrap that already created it.
	if err != nil {
		if existing, _ := s.store.GetNamespace(ctx, DefaultRegistryNamespace); existing != nil {
			return nil
		}
	}
	return err
}

// ── raw mounts ──

// RawMounts returns every configured raw mount (used to build the file
// handler at boot / reload).
func (s *Service) RawMounts(ctx context.Context) ([]RawMountEntry, error) {
	return s.store.ListRawMounts(ctx, nil)
}

func (s *Service) ListRawMounts(ctx context.Context, q *query.Query) ([]RawMountEntry, error) {
	return s.store.ListRawMounts(ctx, q)
}

func (s *Service) GetRawMount(ctx context.Context, prefix string) (*RawMountEntry, error) {
	return s.store.GetRawMount(ctx, prefix)
}

func (s *Service) CreateRawMount(ctx context.Context, m *RawMountEntry) error {
	if m.Prefix == "" {
		return fmt.Errorf("raw mount prefix is required: %w", ErrBadRequest)
	}
	if existing, _ := s.store.GetRawMount(ctx, m.Prefix); existing != nil {
		return fmt.Errorf("raw mount %q already exists: %w", m.Prefix, ErrConflict)
	}
	return s.store.CreateRawMount(ctx, m)
}

func (s *Service) UpdateRawMount(ctx context.Context, m *RawMountEntry) error {
	return s.store.UpdateRawMount(ctx, m)
}

func (s *Service) DeleteRawMount(ctx context.Context, prefix string) error {
	return s.store.DeleteRawMount(ctx, prefix)
}

// ── proxy ──

func (s *Service) ListProxyListeners(ctx context.Context, q *query.Query) ([]ProxyListener, error) {
	return s.store.ListProxyListeners(ctx, q)
}

func (s *Service) GetProxyListener(ctx context.Context, id string) (*ProxyListener, error) {
	return s.store.GetProxyListener(ctx, id)
}

func (s *Service) CreateProxyListener(ctx context.Context, l *ProxyListener) error {
	return s.store.CreateProxyListener(ctx, l)
}

func (s *Service) UpdateProxyListener(ctx context.Context, l *ProxyListener) error {
	return s.store.UpdateProxyListener(ctx, l)
}

func (s *Service) DeleteProxyListener(ctx context.Context, id string) error {
	return s.store.DeleteProxyListener(ctx, id)
}

func (s *Service) ListProxyServers(ctx context.Context, q *query.Query) ([]ProxyServer, error) {
	return s.store.ListProxyServers(ctx, q)
}

func (s *Service) GetProxyServer(ctx context.Context, id string) (*ProxyServer, error) {
	return s.store.GetProxyServer(ctx, id)
}

func (s *Service) CreateProxyServer(ctx context.Context, srv *ProxyServer) error {
	return s.store.CreateProxyServer(ctx, srv)
}

func (s *Service) UpdateProxyServer(ctx context.Context, srv *ProxyServer) error {
	return s.store.UpdateProxyServer(ctx, srv)
}

func (s *Service) DeleteProxyServer(ctx context.Context, id string) error {
	return s.store.DeleteProxyServer(ctx, id)
}

// ── hooks ──

// Hooks returns the configured hooks.
func (s *Service) Hooks(ctx context.Context) ([]hook.Hook, error) {
	return s.store.ListHooks(ctx)
}

// ReplaceHooks swaps the entire hook set.
func (s *Service) ReplaceHooks(ctx context.Context, hooks []hook.Hook) error {
	return s.store.ReplaceHooks(ctx, hooks)
}

// ── auth (no-op) ──

// ValidateToken always returns nil — kutu has no token auth.
func (s *Service) ValidateToken(ctx context.Context, raw, scope, op string) error {
	return nil
}

// ── config data / files (not supported) ──

// DataResult is the resolved configuration value returned by GetData.
type DataResult struct {
	Data    []byte `json:"data,omitempty"`
	Format  string `json:"format,omitempty"`
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
}

// GetData reports that the config-data feature is unavailable.
func (s *Service) GetData(ctx context.Context, key, versionStr, variant string) (*DataResult, error) {
	return nil, fmt.Errorf("config data is not available: %w", ErrNotFound)
}

// File is a stored configuration file (used by config:// references).
type File struct {
	Data []byte `json:"data,omitempty"`
	Meta any    `json:"meta,omitempty"`
}

// File reports that the config-file store is unavailable.
func (s *Service) File(ctx context.Context, key string, version int64) (*File, error) {
	return nil, fmt.Errorf("config files are not available: %w", ErrNotFound)
}
