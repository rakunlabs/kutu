package service

import (
	"context"

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
}
