package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/doug-martin/goqu/v9"
	"github.com/rakunlabs/query"
	"github.com/rakunlabs/query/adapter/adaptergoqu"

	"github.com/rakunlabs/kutu/internal/secret/envelope"
	"github.com/rakunlabs/kutu/internal/service"
)

const registryNamespaceTable = "kutu_registry_namespace"
const registryRepositoryTable = "kutu_registry_repository"

// repoSelectCols is the fixed SELECT/scan column order for repositories.
// Only `namespace` and the auth/data blobs are read back; the type/kind/
// name columns exist purely so query filters/sorts hit real columns.
var repoSelectCols = []any{
	goqu.C("namespace"), goqu.C("auth"), goqu.C("auth_sealed"), goqu.C("data"),
}

type scanner interface{ Scan(dest ...any) error }

// ── namespaces ──

// ListNamespaces returns all namespaces (without their repositories).
func (s *Store) ListNamespaces(ctx context.Context) ([]service.RegistryNamespace, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, description FROM `+registryNamespaceTable+` ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	defer rows.Close()

	var out []service.RegistryNamespace
	for rows.Next() {
		var ns service.RegistryNamespace
		if err := rows.Scan(&ns.Name, &ns.Description); err != nil {
			return nil, err
		}
		out = append(out, ns)
	}
	return out, rows.Err()
}

// GetNamespace returns one namespace (without repositories), or ErrNotFound.
func (s *Store) GetNamespace(ctx context.Context, name string) (*service.RegistryNamespace, error) {
	var ns service.RegistryNamespace
	err := s.db.QueryRowContext(ctx,
		`SELECT name, description FROM `+registryNamespaceTable+` WHERE name = $1`, name,
	).Scan(&ns.Name, &ns.Description)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("namespace %q: %w", name, service.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get namespace: %w", err)
	}
	return &ns, nil
}

// CountNamespaces returns the number of registry namespaces.
func (s *Store) CountNamespaces(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM `+registryNamespaceTable).Scan(&n); err != nil {
		return 0, fmt.Errorf("count namespaces: %w", err)
	}
	return n, nil
}

// CreateNamespace inserts a namespace.
func (s *Store) CreateNamespace(ctx context.Context, ns *service.RegistryNamespace) error {
	ds := dialect.Insert(registryNamespaceTable).Rows(goqu.Record{
		"name":        ns.Name,
		"description": ns.Description,
		"updated_by":  actor(ctx),
	}).Prepared(true)
	if _, err := s.execDS(ctx, ds); err != nil {
		return fmt.Errorf("create namespace: %w", err)
	}
	return nil
}

// UpdateNamespace updates a namespace's description. ErrNotFound when absent.
func (s *Store) UpdateNamespace(ctx context.Context, ns *service.RegistryNamespace) error {
	ds := dialect.Update(registryNamespaceTable).
		Set(goqu.Record{"description": ns.Description, "updated_at": goqu.L("now()"), "updated_by": actor(ctx)}).
		Where(goqu.Ex{"name": ns.Name}).Prepared(true)
	res, err := s.execDS(ctx, ds)
	if err != nil {
		return fmt.Errorf("update namespace: %w", err)
	}
	return notFoundIfZero(res, "namespace", ns.Name)
}

// DeleteNamespace removes a namespace (cascading to its repositories).
func (s *Store) DeleteNamespace(ctx context.Context, name string) error {
	ds := dialect.Delete(registryNamespaceTable).Where(goqu.Ex{"name": name}).Prepared(true)
	res, err := s.execDS(ctx, ds)
	if err != nil {
		return fmt.Errorf("delete namespace: %w", err)
	}
	return notFoundIfZero(res, "namespace", name)
}

// ── repositories ──

// LoadRegistryTree assembles every namespace with its repositories.
func (s *Store) LoadRegistryTree(ctx context.Context) (*service.RegistrySettings, error) {
	namespaces, err := s.ListNamespaces(ctx)
	if err != nil {
		return nil, err
	}
	repos, err := s.ListRepositories(ctx, "", nil)
	if err != nil {
		return nil, err
	}
	byNS := map[string][]service.RegistryRepository{}
	for i := range repos {
		byNS[repos[i].Namespace] = append(byNS[repos[i].Namespace], repos[i].RegistryRepository)
	}
	rs := &service.RegistrySettings{}
	for i := range namespaces {
		ns := namespaces[i]
		ns.Repositories = byNS[ns.Name]
		rs.Namespaces = append(rs.Namespaces, ns)
	}
	return rs, nil
}

// ListRepositories returns repositories, optionally scoped to a single
// namespace, filtered/sorted/paged by q. Pass namespace="" for all.
func (s *Store) ListRepositories(ctx context.Context, namespace string, q *query.Query) ([]service.RegistryRepositoryRow, error) {
	ds := dialect.From(registryRepositoryTable).Select(repoSelectCols...)
	if namespace != "" {
		ds = ds.Where(goqu.Ex{"namespace": namespace})
	} else {
		ds = ds.Order(goqu.C("namespace").Asc(), goqu.C("name").Asc())
	}
	if q != nil {
		q.Select = nil // SELECT columns are fixed; ignore _fields
		ds = adaptergoqu.Select(q, ds)
	} else {
		ds = ds.Prepared(true)
	}

	sqlStr, args, err := ds.ToSQL()
	if err != nil {
		return nil, fmt.Errorf("build repo list sql: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("list repositories: %w", err)
	}
	defer rows.Close()

	var out []service.RegistryRepositoryRow
	for rows.Next() {
		repo, ns, err := s.scanRepo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, service.RegistryRepositoryRow{Namespace: ns, RegistryRepository: *repo})
	}
	return out, rows.Err()
}

// GetRepository returns one repository, or ErrNotFound.
func (s *Store) GetRepository(ctx context.Context, namespace, name string) (*service.RegistryRepository, error) {
	ds := dialect.From(registryRepositoryTable).Select(repoSelectCols...).
		Where(goqu.Ex{"namespace": namespace, "name": name}).Prepared(true)
	sqlStr, args, err := ds.ToSQL()
	if err != nil {
		return nil, fmt.Errorf("build repo get sql: %w", err)
	}
	repo, _, err := s.scanRepo(s.db.QueryRowContext(ctx, sqlStr, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("repository %s/%s: %w", namespace, name, service.ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	return repo, nil
}

// CreateRepository inserts a repository under namespace.
func (s *Store) CreateRepository(ctx context.Context, namespace string, repo *service.RegistryRepository) error {
	rec, err := s.repoRecord(ctx, namespace, repo)
	if err != nil {
		return err
	}
	ds := dialect.Insert(registryRepositoryTable).Rows(rec).Prepared(true)
	if _, err := s.execDS(ctx, ds); err != nil {
		return fmt.Errorf("create repository: %w", err)
	}
	return nil
}

// UpdateRepository updates a repository. ErrNotFound when absent.
func (s *Store) UpdateRepository(ctx context.Context, namespace string, repo *service.RegistryRepository) error {
	rec, err := s.repoRecord(ctx, namespace, repo)
	if err != nil {
		return err
	}
	delete(rec, "namespace")
	delete(rec, "name")
	rec["updated_at"] = goqu.L("now()")
	ds := dialect.Update(registryRepositoryTable).Set(rec).
		Where(goqu.Ex{"namespace": namespace, "name": repo.Name}).Prepared(true)
	res, err := s.execDS(ctx, ds)
	if err != nil {
		return fmt.Errorf("update repository: %w", err)
	}
	return notFoundIfZero(res, "repository", namespace+"/"+repo.Name)
}

// DeleteRepository removes a repository. ErrNotFound when absent.
func (s *Store) DeleteRepository(ctx context.Context, namespace, name string) error {
	ds := dialect.Delete(registryRepositoryTable).
		Where(goqu.Ex{"namespace": namespace, "name": name}).Prepared(true)
	res, err := s.execDS(ctx, ds)
	if err != nil {
		return fmt.Errorf("delete repository: %w", err)
	}
	return notFoundIfZero(res, "repository", namespace+"/"+name)
}

// repoRecord builds the goqu insert/update record. Queryable fields are
// real columns; the rest of the repository (minus the credential, which
// is sealed separately) is marshaled into the data JSONB column.
func (s *Store) repoRecord(ctx context.Context, namespace string, repo *service.RegistryRepository) (goqu.Record, error) {
	// Marshal the full repository minus its credentials into `data`.
	// The default auth and every per-upstream secret (auth + ssh key)
	// are stripped here and sealed separately so plaintext never lands
	// in the JSONB column.
	body := *repo
	body.Auth = nil
	if len(body.Upstreams) > 0 {
		stripped := make([]service.RegistryUpstream, len(body.Upstreams))
		for i := range body.Upstreams {
			u := body.Upstreams[i]
			u.SSHKey = ""
			if u.Auth != nil {
				a := *u.Auth
				a.Password, a.Token, a.Value = "", "", ""
				u.Auth = &a
			}
			stripped[i] = u
		}
		body.Upstreams = stripped
	}
	data, err := jsonbArg(body)
	if err != nil {
		return nil, err
	}
	authMeta, sealed, err := s.sealRepoSecrets(repo)
	if err != nil {
		return nil, err
	}
	return goqu.Record{
		"namespace":   namespace,
		"name":        repo.Name,
		"type":        repo.Type,
		"kind":        repo.Kind,
		"auth":        authMeta,
		"auth_sealed": sealed,
		"data":        data,
		"updated_by":  actor(ctx),
	}, nil
}

func (s *Store) scanRepo(sc scanner) (*service.RegistryRepository, string, error) {
	var namespace string
	var authRaw, sealed, data []byte
	if err := sc.Scan(&namespace, &authRaw, &sealed, &data); err != nil {
		return nil, "", err
	}
	var r service.RegistryRepository
	if err := unmarshalJSONB(data, &r); err != nil {
		return nil, "", err
	}
	s.openRepoSecrets(authRaw, sealed, &r)
	return &r, namespace, nil
}

// ── per-row credential sealing ──

type repoAuthMeta struct {
	Type     string `json:"type,omitempty"`
	Username string `json:"username,omitempty"`
	Header   string `json:"header,omitempty"`
}

// upstreamSecret holds the sealed fields of one prefix-routed upstream.
type upstreamSecret struct {
	Password string `json:"password,omitempty"`
	Token    string `json:"token,omitempty"`
	Value    string `json:"value,omitempty"`
	SSHKey   string `json:"ssh_key,omitempty"`
}

// repoSealedSecrets is the whole sealed blob for a repository row: the
// default upstream auth secrets at the top level (kept flat for
// backward compatibility with rows written before per-upstream support)
// plus an index-aligned list of per-upstream secrets.
type repoSealedSecrets struct {
	Password  string           `json:"password,omitempty"`
	Token     string           `json:"token,omitempty"`
	Value     string           `json:"value,omitempty"`
	Upstreams []upstreamSecret `json:"upstreams,omitempty"`
}

// sealRepoSecrets splits a repository's credentials into a non-secret
// JSONB meta blob (default-auth type/username/header) and a single
// sealed blob covering the default-auth secrets plus every
// prefix-routed upstream's secrets (auth + ssh key). Persisting any
// credential while the at-rest key is locked is refused.
func (s *Store) sealRepoSecrets(repo *service.RegistryRepository) (authMeta any, sealed []byte, err error) {
	var metaArg any
	if repo.Auth != nil {
		metaArg, err = jsonbArg(repoAuthMeta{Type: repo.Auth.Type, Username: repo.Auth.Username, Header: repo.Auth.Header})
		if err != nil {
			return nil, nil, err
		}
	}

	sec := repoSealedSecrets{}
	hasSecret := false
	if repo.Auth != nil {
		sec.Password, sec.Token, sec.Value = repo.Auth.Password, repo.Auth.Token, repo.Auth.Value
		if sec.Password != "" || sec.Token != "" || sec.Value != "" {
			hasSecret = true
		}
	}
	if len(repo.Upstreams) > 0 {
		sec.Upstreams = make([]upstreamSecret, len(repo.Upstreams))
		for i := range repo.Upstreams {
			u := repo.Upstreams[i]
			us := upstreamSecret{SSHKey: u.SSHKey}
			if u.Auth != nil {
				us.Password, us.Token, us.Value = u.Auth.Password, u.Auth.Token, u.Auth.Value
			}
			sec.Upstreams[i] = us
			if us.Password != "" || us.Token != "" || us.Value != "" || us.SSHKey != "" {
				hasSecret = true
			}
		}
	}

	if !hasSecret {
		return metaArg, nil, nil
	}
	if s.mgr == nil || !s.mgr.IsUnlocked() {
		return nil, nil, fmt.Errorf("server encryption is locked; unlock/initialize the at-rest key before storing registry credentials: %w", service.ErrBadRequest)
	}
	raw, err := json.Marshal(sec)
	if err != nil {
		return nil, nil, err
	}
	sealed, err = envelope.Seal(s.mgr, raw)
	if err != nil {
		return nil, nil, fmt.Errorf("seal registry credentials: %w", err)
	}
	return metaArg, sealed, nil
}

// openRepoSecrets reconstructs a repository's credentials from the
// JSONB meta + sealed blob, filling repo.Auth and each
// repo.Upstreams[i] secret fields in place. Secret fields stay blank
// while the at-rest key is locked.
func (s *Store) openRepoSecrets(authRaw, sealed []byte, repo *service.RegistryRepository) {
	var sec repoSealedSecrets
	if len(sealed) > 0 && s.mgr != nil && s.mgr.IsUnlocked() {
		if raw, err := envelope.Open(s.mgr, sealed); err == nil {
			_ = json.Unmarshal(raw, &sec)
		}
	}

	// Default upstream auth: present only when a meta blob was stored.
	if len(authRaw) > 0 {
		var m repoAuthMeta
		_ = unmarshalJSONB(authRaw, &m)
		repo.Auth = &service.RegistryUpstreamAuth{
			Type:     m.Type,
			Username: m.Username,
			Header:   m.Header,
			Password: sec.Password,
			Token:    sec.Token,
			Value:    sec.Value,
		}
	}

	// Per-upstream secrets, index-aligned with the (non-secret)
	// Upstreams already unmarshaled from the data JSONB.
	for i := range repo.Upstreams {
		if i >= len(sec.Upstreams) {
			break
		}
		us := sec.Upstreams[i]
		repo.Upstreams[i].SSHKey = us.SSHKey
		if repo.Upstreams[i].Auth != nil {
			repo.Upstreams[i].Auth.Password = us.Password
			repo.Upstreams[i].Auth.Token = us.Token
			repo.Upstreams[i].Auth.Value = us.Value
		} else if us.Password != "" || us.Token != "" || us.Value != "" {
			repo.Upstreams[i].Auth = &service.RegistryUpstreamAuth{
				Password: us.Password, Token: us.Token, Value: us.Value,
			}
		}
	}
}

// notFoundIfZero maps a zero-rows-affected result to ErrNotFound.
func notFoundIfZero(res sql.Result, kind, id string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%s %q: %w", kind, id, service.ErrNotFound)
	}
	return nil
}
