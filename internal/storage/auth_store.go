package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/doug-martin/goqu/v9"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rakunlabs/query"
	"github.com/rakunlabs/query/adapter/adaptergoqu"

	"github.com/rakunlabs/kutu/internal/service"
)

const (
	userTable             = "kutu_user"
	userIdentityTable     = "kutu_user_identity"
	permissionTable       = "kutu_permission"
	userPermissionTable   = "kutu_user_permission"
	sessionTable          = "kutu_session"
	tokenTable            = "kutu_token"
	passkeyTable          = "kutu_passkey"
	passkeyChallengeTable = "kutu_passkey_challenge"
	userTOTPTable         = "kutu_user_totp"
)

func (s *Store) Users() service.UserStorage                         { return userStore{s} }
func (s *Store) UserIdentities() service.UserIdentityStorage        { return identityStore{s} }
func (s *Store) Sessions() service.SessionStorage                   { return sessionStore{s} }
func (s *Store) Permissions() service.PermissionStorage             { return permissionStore{s} }
func (s *Store) Tokens() service.TokenStorage                       { return tokenStore{s} }
func (s *Store) Passkeys() service.PasskeyStorage                   { return passkeyStore{s} }
func (s *Store) PasskeyChallenges() service.PasskeyChallengeStorage { return challengeStore{s} }
func (s *Store) UserTOTPs() service.UserTOTPStorage                 { return totpStore{s} }

// mapWriteErr turns a unique violation into service.ErrConflict.
func mapWriteErr(err error, what string) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return fmt.Errorf("%s already exists: %w", what, service.ErrConflict)
	}
	return fmt.Errorf("%s: %w", what, err)
}

func (s *Store) queryDS(ctx context.Context, ds *goqu.SelectDataset) (*sql.Rows, error) {
	q, args, err := ds.ToSQL()
	if err != nil {
		return nil, fmt.Errorf("build sql: %w", err)
	}
	return s.db.QueryContext(ctx, q, args...)
}

func (s *Store) queryRowDS(ctx context.Context, ds *goqu.SelectDataset) (*sql.Row, error) {
	q, args, err := ds.ToSQL()
	if err != nil {
		return nil, fmt.Errorf("build sql: %w", err)
	}
	return s.db.QueryRowContext(ctx, q, args...), nil
}

func nullTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return *t
}

func timePtr(nt sql.NullTime) *time.Time {
	if !nt.Valid {
		return nil
	}
	t := nt.Time
	return &t
}

func jsonOrEmptyArray(v any) (string, error) {
	raw, err := jsonbArg(v)
	if err != nil {
		return "", err
	}
	if raw == nil {
		return "[]", nil
	}
	return raw.(string), nil
}

// ── users ──

type userStore struct{ s *Store }

var userCols = []any{
	goqu.C("id"), goqu.C("username"), goqu.C("email"), goqu.C("display_name"),
	goqu.C("password_hash"), goqu.C("external"), goqu.C("disabled"), goqu.C("is_superadmin"),
	goqu.C("denied_capabilities"), goqu.C("created_at"), goqu.C("updated_at"),
}

func scanUser(sc scanner) (*service.User, error) {
	var u service.User
	var denied []byte
	if err := sc.Scan(&u.ID, &u.Username, &u.Email, &u.DisplayName, &u.PasswordHash,
		&u.External, &u.Disabled, &u.IsSuperadmin, &denied, &u.CreatedAt, &u.UpdatedAt); err != nil {
		return nil, err
	}
	if err := unmarshalJSONB(denied, &u.DeniedCapabilities); err != nil {
		return nil, err
	}
	return &u, nil
}

func (st userStore) getBy(ctx context.Context, col, val string) (*service.User, error) {
	if val == "" {
		return nil, fmt.Errorf("user: %w", service.ErrNotFound)
	}
	row, err := st.s.queryRowDS(ctx, dialect.From(userTable).Select(userCols...).
		Where(goqu.Ex{col: val}).Limit(1).Prepared(true))
	if err != nil {
		return nil, err
	}
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("user %q: %w", val, service.ErrNotFound)
	}
	return u, err
}

func (st userStore) Get(ctx context.Context, id string) (*service.User, error) {
	return st.getBy(ctx, "id", id)
}

func (st userStore) GetByUsername(ctx context.Context, username string) (*service.User, error) {
	return st.getBy(ctx, "username", username)
}

func (st userStore) GetByEmail(ctx context.Context, email string) (*service.User, error) {
	return st.getBy(ctx, "email", email)
}

func (st userStore) Create(ctx context.Context, u *service.User) error {
	denied, err := jsonOrEmptyArray(u.DeniedCapabilities)
	if err != nil {
		return err
	}
	_, err = st.s.execDS(ctx, dialect.Insert(userTable).Rows(goqu.Record{
		"id": u.ID, "username": u.Username, "email": u.Email, "display_name": u.DisplayName,
		"password_hash": u.PasswordHash, "external": u.External, "disabled": u.Disabled,
		"is_superadmin": u.IsSuperadmin, "denied_capabilities": denied,
		"created_at": u.CreatedAt, "updated_at": u.UpdatedAt,
	}).Prepared(true))
	return mapWriteErr(err, fmt.Sprintf("user %q", u.Username))
}

func (st userStore) Update(ctx context.Context, u *service.User) error {
	res, err := st.s.execDS(ctx, dialect.Update(userTable).Set(goqu.Record{
		"username": u.Username, "email": u.Email, "display_name": u.DisplayName,
		"password_hash": u.PasswordHash, "external": u.External, "disabled": u.Disabled,
		"is_superadmin": u.IsSuperadmin, "updated_at": u.UpdatedAt,
	}).Where(goqu.Ex{"id": u.ID}).Prepared(true))
	if err != nil {
		return mapWriteErr(err, fmt.Sprintf("user %q", u.Username))
	}
	return notFoundIfZero(res, "user", u.ID)
}

// Delete removes the user; identities, grants, passkeys and TOTP cascade
// through foreign keys. Sessions carry no FK (unbound sessions exist), so
// they are removed explicitly.
func (st userStore) Delete(ctx context.Context, id string) error {
	if _, err := st.s.db.ExecContext(ctx, `DELETE FROM `+sessionTable+` WHERE user_id = $1`, id); err != nil {
		return fmt.Errorf("delete user sessions: %w", err)
	}
	res, err := st.s.execDS(ctx, dialect.Delete(userTable).Where(goqu.Ex{"id": id}).Prepared(true))
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	return notFoundIfZero(res, "user", id)
}

func (st userStore) Count(ctx context.Context) (int64, error) {
	var n int64
	err := st.s.db.QueryRowContext(ctx, `SELECT count(*) FROM `+userTable).Scan(&n)
	return n, err
}

// List applies q's filters (username/disabled/id/email…), sort and paging
// and returns the page plus the unpaged total.
func (st userStore) List(ctx context.Context, q *query.Query) ([]service.User, int64, error) {
	base := dialect.From(userTable)
	ds := base.Select(userCols...)
	countDS := base.Select(goqu.COUNT(goqu.Star()))
	if q != nil {
		q.Select = nil
		if where := adaptergoqu.Expression(q); len(where) > 0 {
			countDS = countDS.Where(where...)
		}
		ds = adaptergoqu.Select(q, ds)
		if len(q.Sort) == 0 {
			ds = ds.Order(goqu.C("username").Asc())
		}
	} else {
		ds = ds.Order(goqu.C("username").Asc()).Prepared(true)
	}
	countDS = countDS.Prepared(true)

	row, err := st.s.queryRowDS(ctx, countDS)
	if err != nil {
		return nil, 0, err
	}
	var total int64
	if err := row.Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count users: %w", err)
	}

	rows, err := st.s.queryDS(ctx, ds)
	if err != nil {
		return nil, 0, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	out := []service.User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *u)
	}
	return out, total, rows.Err()
}

// ── identities ──

type identityStore struct{ s *Store }

const identitySelect = `SELECT id, user_id, provider, subject, email, display_name, created_at, last_login_at FROM ` + userIdentityTable

func scanIdentity(sc scanner) (*service.UserIdentity, error) {
	var i service.UserIdentity
	var last sql.NullTime
	if err := sc.Scan(&i.ID, &i.UserID, &i.Provider, &i.Subject, &i.Email, &i.DisplayName, &i.CreatedAt, &last); err != nil {
		return nil, err
	}
	i.LastLoginAt = timePtr(last)
	return &i, nil
}

func (st identityStore) Upsert(ctx context.Context, in *service.UserIdentity) (*service.UserIdentity, error) {
	id := in.ID
	if id == "" {
		id = newID()
	}
	row := st.s.db.QueryRowContext(ctx, `INSERT INTO `+userIdentityTable+`
		(id, user_id, provider, subject, email, display_name, created_at, last_login_at)
		VALUES ($1, $2, $3, $4, $5, $6, now(), now())
		ON CONFLICT (provider, subject) DO UPDATE SET
			email = EXCLUDED.email, display_name = EXCLUDED.display_name, last_login_at = now()
		RETURNING id, user_id, provider, subject, email, display_name, created_at, last_login_at`,
		id, in.UserID, in.Provider, in.Subject, in.Email, in.DisplayName)
	out, err := scanIdentity(row)
	if err != nil {
		return nil, mapWriteErr(err, "user identity")
	}
	return out, nil
}

func (st identityStore) FindByProviderSubject(ctx context.Context, provider, subject string) (*service.UserIdentity, error) {
	i, err := scanIdentity(st.s.db.QueryRowContext(ctx, identitySelect+` WHERE provider = $1 AND subject = $2`, provider, subject))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("identity %s/%s: %w", provider, subject, service.ErrNotFound)
	}
	return i, err
}

func (st identityStore) ListByUserID(ctx context.Context, userID string) ([]service.UserIdentity, error) {
	rows, err := st.s.db.QueryContext(ctx, identitySelect+` WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []service.UserIdentity{}
	for rows.Next() {
		i, err := scanIdentity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *i)
	}
	return out, rows.Err()
}

func (st identityStore) Delete(ctx context.Context, id string) error {
	res, err := st.s.db.ExecContext(ctx, `DELETE FROM `+userIdentityTable+` WHERE id = $1`, id)
	if err != nil {
		return err
	}
	return notFoundIfZero(res, "identity", id)
}

func (st identityStore) DeleteByUserID(ctx context.Context, userID string) error {
	_, err := st.s.db.ExecContext(ctx, `DELETE FROM `+userIdentityTable+` WHERE user_id = $1`, userID)
	return err
}

// ── sessions ──

type sessionStore struct{ s *Store }

const sessionSelect = `SELECT id, user_id, username, payload, refresh_id, created_at, expires_at FROM ` + sessionTable

func scanSession(sc scanner) (*service.Session, error) {
	var x service.Session
	if err := sc.Scan(&x.ID, &x.UserID, &x.Username, &x.Payload, &x.RefreshID, &x.CreatedAt, &x.ExpiresAt); err != nil {
		return nil, err
	}
	return &x, nil
}

func (st sessionStore) Put(ctx context.Context, x *service.Session) error {
	created := x.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	_, err := st.s.db.ExecContext(ctx, `INSERT INTO `+sessionTable+`
		(id, user_id, username, payload, refresh_id, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (id) DO UPDATE SET
			user_id = EXCLUDED.user_id, username = EXCLUDED.username, payload = EXCLUDED.payload,
			refresh_id = EXCLUDED.refresh_id, expires_at = EXCLUDED.expires_at`,
		x.ID, x.UserID, x.Username, x.Payload, x.RefreshID, created, x.ExpiresAt)
	if err != nil {
		return fmt.Errorf("put session: %w", err)
	}
	return nil
}

func (st sessionStore) Get(ctx context.Context, id string) (*service.Session, error) {
	x, err := scanSession(st.s.db.QueryRowContext(ctx, sessionSelect+` WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("session: %w", service.ErrNotFound)
	}
	return x, err
}

func (st sessionStore) Delete(ctx context.Context, id string) error {
	_, err := st.s.db.ExecContext(ctx, `DELETE FROM `+sessionTable+` WHERE id = $1`, id)
	return err
}

func (st sessionStore) DeleteByUserID(ctx context.Context, userID string) error {
	if userID == "" {
		return nil
	}
	_, err := st.s.db.ExecContext(ctx, `DELETE FROM `+sessionTable+` WHERE user_id = $1`, userID)
	return err
}

func (st sessionStore) DeleteExpired(ctx context.Context) error {
	_, err := st.s.db.ExecContext(ctx, `DELETE FROM `+sessionTable+` WHERE expires_at < now()`)
	return err
}

func (st sessionStore) CountByUserID(ctx context.Context, userID string) (int64, error) {
	var n int64
	err := st.s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM `+sessionTable+` WHERE user_id = $1 AND expires_at > now()`, userID).Scan(&n)
	return n, err
}

func (st sessionStore) ListByUserID(ctx context.Context, userID string) ([]*service.Session, error) {
	rows, err := st.s.db.QueryContext(ctx,
		sessionSelect+` WHERE user_id = $1 AND expires_at > now() ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*service.Session
	for rows.Next() {
		x, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// ── permissions ──

type permissionStore struct{ s *Store }

const permissionSelect = `SELECT p.id, p.key, p.name, p.description, p.keys, p.key_patterns, p.created_at FROM ` + permissionTable + ` p`

func scanPermission(sc scanner) (*service.Permission, error) {
	var p service.Permission
	var keys, patterns []byte
	if err := sc.Scan(&p.ID, &p.Key, &p.Name, &p.Description, &keys, &patterns, &p.CreatedAt); err != nil {
		return nil, err
	}
	if err := unmarshalJSONB(keys, &p.Keys); err != nil {
		return nil, err
	}
	if err := unmarshalJSONB(patterns, &p.KeyPatterns); err != nil {
		return nil, err
	}
	if p.Keys == nil {
		p.Keys = []string{}
	}
	return &p, nil
}

func (st permissionStore) list(ctx context.Context, q string, args ...any) ([]service.Permission, error) {
	rows, err := st.s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []service.Permission{}
	for rows.Next() {
		p, err := scanPermission(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func permissionRecord(p *service.Permission) (goqu.Record, error) {
	keys, err := jsonOrEmptyArray(p.Keys)
	if err != nil {
		return nil, err
	}
	var patterns any
	if len(p.KeyPatterns) > 0 {
		if patterns, err = jsonbArg(p.KeyPatterns); err != nil {
			return nil, err
		}
	}
	return goqu.Record{"key": p.Key, "name": p.Name, "description": p.Description,
		"keys": keys, "key_patterns": patterns}, nil
}

func (st permissionStore) Create(ctx context.Context, p *service.Permission) error {
	rec, err := permissionRecord(p)
	if err != nil {
		return err
	}
	rec["id"] = p.ID
	rec["created_at"] = p.CreatedAt
	_, err = st.s.execDS(ctx, dialect.Insert(permissionTable).Rows(rec).Prepared(true))
	return mapWriteErr(err, fmt.Sprintf("permission %q", p.Key))
}

func (st permissionStore) Get(ctx context.Context, id string) (*service.Permission, error) {
	p, err := scanPermission(st.s.db.QueryRowContext(ctx, permissionSelect+` WHERE p.id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("permission %q: %w", id, service.ErrNotFound)
	}
	return p, err
}

func (st permissionStore) List(ctx context.Context) ([]service.Permission, error) {
	return st.list(ctx, permissionSelect+` ORDER BY p.name`)
}

func (st permissionStore) Update(ctx context.Context, p *service.Permission) error {
	rec, err := permissionRecord(p)
	if err != nil {
		return err
	}
	res, err := st.s.execDS(ctx, dialect.Update(permissionTable).Set(rec).Where(goqu.Ex{"id": p.ID}).Prepared(true))
	if err != nil {
		return mapWriteErr(err, fmt.Sprintf("permission %q", p.Key))
	}
	return notFoundIfZero(res, "permission", p.ID)
}

func (st permissionStore) Delete(ctx context.Context, id string) error {
	res, err := st.s.db.ExecContext(ctx, `DELETE FROM `+permissionTable+` WHERE id = $1`, id)
	if err != nil {
		return err
	}
	return notFoundIfZero(res, "permission", id)
}

func (st permissionStore) SetUserPermissions(ctx context.Context, userID string, permissionIDs []string) error {
	tx, err := st.s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+userPermissionTable+` WHERE user_id = $1`, userID); err != nil {
		return err
	}
	for _, pid := range permissionIDs {
		if pid == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+userPermissionTable+` (user_id, permission_id)
			VALUES ($1, $2) ON CONFLICT DO NOTHING`, userID, pid); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23503" {
				return fmt.Errorf("permission %q: %w", pid, service.ErrNotFound)
			}
			return err
		}
	}
	return tx.Commit()
}

func (st permissionStore) GetUserPermissions(ctx context.Context, userID string) ([]service.Permission, error) {
	return st.list(ctx, permissionSelect+` JOIN `+userPermissionTable+` up ON up.permission_id = p.id
		WHERE up.user_id = $1 ORDER BY p.name`, userID)
}

func (st permissionStore) ListUserIDsByPermission(ctx context.Context, permissionID string) ([]string, error) {
	rows, err := st.s.db.QueryContext(ctx,
		`SELECT user_id FROM `+userPermissionTable+` WHERE permission_id = $1`, permissionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (st permissionStore) GetUserCapabilityKeys(ctx context.Context, userID string) ([]string, error) {
	perms, err := st.GetUserPermissions(ctx, userID)
	if err != nil {
		return nil, err
	}
	keys, _ := service.CapabilitiesFromBundles(perms)
	return keys, nil
}

func (st permissionStore) HasCapabilityKey(ctx context.Context, key string) (bool, error) {
	var ok bool
	err := st.s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM `+permissionTable+` WHERE keys ? $1)`, key).Scan(&ok)
	return ok, err
}

func (st permissionStore) GetUserDeniedCapabilities(ctx context.Context, userID string) ([]string, error) {
	var raw []byte
	err := st.s.db.QueryRowContext(ctx,
		`SELECT denied_capabilities FROM `+userTable+` WHERE id = $1`, userID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("user %q: %w", userID, service.ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	out := []string{}
	return out, unmarshalJSONB(raw, &out)
}

func (st permissionStore) SetUserDeniedCapabilities(ctx context.Context, userID string, keys []string) error {
	raw, err := jsonOrEmptyArray(keys)
	if err != nil {
		return err
	}
	res, err := st.s.db.ExecContext(ctx,
		`UPDATE `+userTable+` SET denied_capabilities = $2, updated_at = now() WHERE id = $1`, userID, raw)
	if err != nil {
		return err
	}
	return notFoundIfZero(res, "user", userID)
}

// ── tokens ──

type tokenStore struct{ s *Store }

var tokenCols = []any{
	goqu.C("id"), goqu.C("name"), goqu.C("hashed_key"), goqu.C("scopes"), goqu.C("active"),
	goqu.C("created_by"), goqu.C("created_at"), goqu.C("expires_at"), goqu.C("last_used_at"),
}

func scanToken(sc scanner) (*service.Token, error) {
	var t service.Token
	var scopes []byte
	var expires, lastUsed sql.NullTime
	if err := sc.Scan(&t.ID, &t.Name, &t.HashedKey, &scopes, &t.Active, &t.CreatedBy,
		&t.CreatedAt, &expires, &lastUsed); err != nil {
		return nil, err
	}
	if err := unmarshalJSONB(scopes, &t.Scopes); err != nil {
		return nil, err
	}
	t.ExpiresAt = timePtr(expires)
	t.LastUsedAt = timePtr(lastUsed)
	return &t, nil
}

func (st tokenStore) getBy(ctx context.Context, col, val string) (*service.Token, error) {
	row, err := st.s.queryRowDS(ctx, dialect.From(tokenTable).Select(tokenCols...).
		Where(goqu.Ex{col: val}).Prepared(true))
	if err != nil {
		return nil, err
	}
	t, err := scanToken(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("token: %w", service.ErrNotFound)
	}
	return t, err
}

func (st tokenStore) Get(ctx context.Context, id string) (*service.Token, error) {
	return st.getBy(ctx, "id", id)
}

func (st tokenStore) FindByHash(ctx context.Context, hashed string) (*service.Token, error) {
	return st.getBy(ctx, "hashed_key", hashed)
}

func (st tokenStore) Create(ctx context.Context, t *service.Token) error {
	scopes, err := jsonOrEmptyArray(t.Scopes)
	if err != nil {
		return err
	}
	_, err = st.s.execDS(ctx, dialect.Insert(tokenTable).Rows(goqu.Record{
		"id": t.ID, "name": t.Name, "hashed_key": t.HashedKey, "scopes": scopes, "active": t.Active,
		"created_by": t.CreatedBy, "created_at": t.CreatedAt, "expires_at": nullTime(t.ExpiresAt),
	}).Prepared(true))
	return mapWriteErr(err, fmt.Sprintf("token %q", t.Name))
}

func (st tokenStore) Update(ctx context.Context, t *service.Token) error {
	scopes, err := jsonOrEmptyArray(t.Scopes)
	if err != nil {
		return err
	}
	res, err := st.s.execDS(ctx, dialect.Update(tokenTable).Set(goqu.Record{
		"name": t.Name, "scopes": scopes, "active": t.Active, "expires_at": nullTime(t.ExpiresAt),
	}).Where(goqu.Ex{"id": t.ID}).Prepared(true))
	if err != nil {
		return mapWriteErr(err, fmt.Sprintf("token %q", t.Name))
	}
	return notFoundIfZero(res, "token", t.ID)
}

func (st tokenStore) TouchLastUsed(ctx context.Context, id string, at time.Time) error {
	_, err := st.s.db.ExecContext(ctx, `UPDATE `+tokenTable+` SET last_used_at = $2
		WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < $2)`, id, at)
	return err
}

func (st tokenStore) Delete(ctx context.Context, id string) error {
	res, err := st.s.db.ExecContext(ctx, `DELETE FROM `+tokenTable+` WHERE id = $1`, id)
	if err != nil {
		return err
	}
	return notFoundIfZero(res, "token", id)
}

func (st tokenStore) List(ctx context.Context, q *query.Query) ([]service.Token, int64, error) {
	base := dialect.From(tokenTable)
	ds := base.Select(tokenCols...)
	countDS := base.Select(goqu.COUNT(goqu.Star()))
	if q != nil {
		q.Select = nil
		if where := adaptergoqu.Expression(q); len(where) > 0 {
			countDS = countDS.Where(where...)
		}
		ds = adaptergoqu.Select(q, ds)
		if len(q.Sort) == 0 {
			ds = ds.Order(goqu.C("created_at").Desc())
		}
	} else {
		ds = ds.Order(goqu.C("created_at").Desc()).Prepared(true)
	}

	row, err := st.s.queryRowDS(ctx, countDS.Prepared(true))
	if err != nil {
		return nil, 0, err
	}
	var total int64
	if err := row.Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count tokens: %w", err)
	}
	rows, err := st.s.queryDS(ctx, ds)
	if err != nil {
		return nil, 0, fmt.Errorf("list tokens: %w", err)
	}
	defer rows.Close()
	out := []service.Token{}
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *t)
	}
	return out, total, rows.Err()
}

// ── passkeys ──

type passkeyStore struct{ s *Store }

const passkeySelect = `SELECT id, user_id, credential_id, public_key, aaguid, sign_count, transports,
	user_verified, backup_eligible, backup_state, attestation_type, name, created_at, last_used_at FROM ` + passkeyTable

func scanPasskey(sc scanner) (*service.PasskeyCredential, error) {
	var c service.PasskeyCredential
	var transports []byte
	var signCount int64
	var lastUsed sql.NullTime
	if err := sc.Scan(&c.ID, &c.UserID, &c.CredentialID, &c.PublicKey, &c.AAGUID, &signCount, &transports,
		&c.UserVerified, &c.BackupEligible, &c.BackupState, &c.AttestationType, &c.Name, &c.CreatedAt, &lastUsed); err != nil {
		return nil, err
	}
	c.SignCount = uint32(signCount)
	if lastUsed.Valid {
		c.LastUsedAt = lastUsed.Time
	}
	return &c, unmarshalJSONB(transports, &c.Transports)
}

func (st passkeyStore) one(ctx context.Context, where string, arg any) (*service.PasskeyCredential, error) {
	c, err := scanPasskey(st.s.db.QueryRowContext(ctx, passkeySelect+` WHERE `+where+` = $1`, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("passkey: %w", service.ErrNotFound)
	}
	return c, err
}

func (st passkeyStore) Get(ctx context.Context, id string) (*service.PasskeyCredential, error) {
	return st.one(ctx, "id", id)
}

func (st passkeyStore) FindByCredentialID(ctx context.Context, credentialID []byte) (*service.PasskeyCredential, error) {
	return st.one(ctx, "credential_id", credentialID)
}

func (st passkeyStore) Create(ctx context.Context, c *service.PasskeyCredential) error {
	transports, err := jsonOrEmptyArray(c.Transports)
	if err != nil {
		return err
	}
	_, err = st.s.db.ExecContext(ctx, `INSERT INTO `+passkeyTable+`
		(id, user_id, credential_id, public_key, aaguid, sign_count, transports, user_verified,
		 backup_eligible, backup_state, attestation_type, name, created_at, last_used_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		c.ID, c.UserID, c.CredentialID, c.PublicKey, c.AAGUID, int64(c.SignCount), transports, c.UserVerified,
		c.BackupEligible, c.BackupState, c.AttestationType, c.Name, c.CreatedAt, nullTime(&c.LastUsedAt))
	return mapWriteErr(err, "passkey")
}

func (st passkeyStore) Update(ctx context.Context, c *service.PasskeyCredential) error {
	res, err := st.s.db.ExecContext(ctx, `UPDATE `+passkeyTable+` SET sign_count = $2, name = $3,
		backup_state = $4, last_used_at = $5 WHERE id = $1`,
		c.ID, int64(c.SignCount), c.Name, c.BackupState, nullTime(&c.LastUsedAt))
	if err != nil {
		return err
	}
	return notFoundIfZero(res, "passkey", c.ID)
}

func (st passkeyStore) ListByUserID(ctx context.Context, userID string) ([]service.PasskeyCredential, error) {
	rows, err := st.s.db.QueryContext(ctx, passkeySelect+` WHERE user_id = $1 ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []service.PasskeyCredential{}
	for rows.Next() {
		c, err := scanPasskey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func (st passkeyStore) Delete(ctx context.Context, id string) error {
	res, err := st.s.db.ExecContext(ctx, `DELETE FROM `+passkeyTable+` WHERE id = $1`, id)
	if err != nil {
		return err
	}
	return notFoundIfZero(res, "passkey", id)
}

func (st passkeyStore) DeleteByUserID(ctx context.Context, userID string) error {
	_, err := st.s.db.ExecContext(ctx, `DELETE FROM `+passkeyTable+` WHERE user_id = $1`, userID)
	return err
}

// ── passkey challenges ──

type challengeStore struct{ s *Store }

const challengeCols = `id, kind, user_id, data, created_at, expires_at`

func scanChallenge(sc scanner) (*service.PasskeyChallenge, error) {
	var c service.PasskeyChallenge
	if err := sc.Scan(&c.ID, &c.Kind, &c.UserID, &c.Data, &c.CreatedAt, &c.ExpiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("passkey challenge: %w", service.ErrNotFound)
		}
		return nil, err
	}
	return &c, nil
}

func (st challengeStore) Save(ctx context.Context, c *service.PasskeyChallenge) error {
	_, err := st.s.db.ExecContext(ctx, `INSERT INTO `+passkeyChallengeTable+` (`+challengeCols+`)
		VALUES ($1, $2, $3, $4, now(), $5)
		ON CONFLICT (id) DO UPDATE SET kind = EXCLUDED.kind, user_id = EXCLUDED.user_id,
			data = EXCLUDED.data, expires_at = EXCLUDED.expires_at`,
		c.ID, c.Kind, c.UserID, c.Data, c.ExpiresAt)
	return err
}

func (st challengeStore) Get(ctx context.Context, id string) (*service.PasskeyChallenge, error) {
	return scanChallenge(st.s.db.QueryRowContext(ctx,
		`SELECT `+challengeCols+` FROM `+passkeyChallengeTable+` WHERE id = $1`, id))
}

func (st challengeStore) Consume(ctx context.Context, id string) (*service.PasskeyChallenge, error) {
	return scanChallenge(st.s.db.QueryRowContext(ctx,
		`DELETE FROM `+passkeyChallengeTable+` WHERE id = $1 RETURNING `+challengeCols, id))
}

func (st challengeStore) Delete(ctx context.Context, id string) error {
	_, err := st.s.db.ExecContext(ctx, `DELETE FROM `+passkeyChallengeTable+` WHERE id = $1`, id)
	return err
}

func (st challengeStore) DeleteExpired(ctx context.Context) (int, error) {
	res, err := st.s.db.ExecContext(ctx, `DELETE FROM `+passkeyChallengeTable+` WHERE expires_at < now()`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ── TOTP ──

type totpStore struct{ s *Store }

func (st totpStore) Get(ctx context.Context, userID string) (*service.UserTOTP, error) {
	var t service.UserTOTP
	var codes []byte
	var lastUsed sql.NullTime
	err := st.s.db.QueryRowContext(ctx, `SELECT user_id, secret, enabled, recovery_codes, created_at, last_used_at
		FROM `+userTOTPTable+` WHERE user_id = $1`, userID).
		Scan(&t.UserID, &t.Secret, &t.Enabled, &codes, &t.CreatedAt, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("totp: %w", service.ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	if lastUsed.Valid {
		t.LastUsedAt = lastUsed.Time
	}
	return &t, unmarshalJSONB(codes, &t.RecoveryCodes)
}

func (st totpStore) Set(ctx context.Context, t *service.UserTOTP) error {
	codes, err := jsonOrEmptyArray(t.RecoveryCodes)
	if err != nil {
		return err
	}
	created := t.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	_, err = st.s.db.ExecContext(ctx, `INSERT INTO `+userTOTPTable+`
		(user_id, secret, enabled, recovery_codes, created_at, last_used_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (user_id) DO UPDATE SET secret = EXCLUDED.secret, enabled = EXCLUDED.enabled,
			recovery_codes = EXCLUDED.recovery_codes, created_at = EXCLUDED.created_at,
			last_used_at = EXCLUDED.last_used_at`,
		t.UserID, t.Secret, t.Enabled, codes, created, nullTime(&t.LastUsedAt))
	return mapWriteErr(err, "totp")
}

func (st totpStore) Delete(ctx context.Context, userID string) error {
	res, err := st.s.db.ExecContext(ctx, `DELETE FROM `+userTOTPTable+` WHERE user_id = $1`, userID)
	if err != nil {
		return err
	}
	return notFoundIfZero(res, "totp", userID)
}

// newID returns a random 16-byte hex identifier.
func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Errorf("storage: rng failure: %w", err))
	}
	return hex.EncodeToString(b)
}
