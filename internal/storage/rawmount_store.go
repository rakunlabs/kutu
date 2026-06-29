package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/doug-martin/goqu/v9"
	"github.com/rakunlabs/query"
	"github.com/rakunlabs/query/adapter/adaptergoqu"

	"github.com/rakunlabs/kutu/internal/service"
)

const rawMountTable = "kutu_raw_mount"

var rawMountCols = []any{goqu.C("prefix"), goqu.C("type"), goqu.C("config")}

// ListRawMounts returns raw mounts, filtered/sorted/paged by q.
func (s *Store) ListRawMounts(ctx context.Context, q *query.Query) ([]service.RawMountEntry, error) {
	ds := dialect.From(rawMountTable).Select(rawMountCols...).Order(goqu.C("prefix").Asc())
	if q != nil {
		q.Select = nil
		ds = adaptergoqu.Select(q, ds)
	} else {
		ds = ds.Prepared(true)
	}
	sqlStr, args, err := ds.ToSQL()
	if err != nil {
		return nil, fmt.Errorf("build raw mount list sql: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("list raw mounts: %w", err)
	}
	defer rows.Close()

	var out []service.RawMountEntry
	for rows.Next() {
		m, err := scanRawMount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// GetRawMount returns one raw mount, or ErrNotFound.
func (s *Store) GetRawMount(ctx context.Context, prefix string) (*service.RawMountEntry, error) {
	ds := dialect.From(rawMountTable).Select(rawMountCols...).
		Where(goqu.Ex{"prefix": prefix}).Prepared(true)
	sqlStr, args, err := ds.ToSQL()
	if err != nil {
		return nil, fmt.Errorf("build raw mount get sql: %w", err)
	}
	m, err := scanRawMount(s.db.QueryRowContext(ctx, sqlStr, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("raw mount %q: %w", prefix, service.ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	return m, nil
}

// CreateRawMount inserts a raw mount.
func (s *Store) CreateRawMount(ctx context.Context, m *service.RawMountEntry) error {
	rec, err := rawMountRecord(ctx, m)
	if err != nil {
		return err
	}
	ds := dialect.Insert(rawMountTable).Rows(rec).Prepared(true)
	if _, err := s.execDS(ctx, ds); err != nil {
		return fmt.Errorf("create raw mount: %w", err)
	}
	return nil
}

// UpdateRawMount updates a raw mount. ErrNotFound when absent.
func (s *Store) UpdateRawMount(ctx context.Context, m *service.RawMountEntry) error {
	rec, err := rawMountRecord(ctx, m)
	if err != nil {
		return err
	}
	delete(rec, "prefix")
	rec["updated_at"] = goqu.L("now()")
	ds := dialect.Update(rawMountTable).Set(rec).Where(goqu.Ex{"prefix": m.Prefix}).Prepared(true)
	res, err := s.execDS(ctx, ds)
	if err != nil {
		return fmt.Errorf("update raw mount: %w", err)
	}
	return notFoundIfZero(res, "raw mount", m.Prefix)
}

// DeleteRawMount removes a raw mount. ErrNotFound when absent.
func (s *Store) DeleteRawMount(ctx context.Context, prefix string) error {
	ds := dialect.Delete(rawMountTable).Where(goqu.Ex{"prefix": prefix}).Prepared(true)
	res, err := s.execDS(ctx, ds)
	if err != nil {
		return fmt.Errorf("delete raw mount: %w", err)
	}
	return notFoundIfZero(res, "raw mount", prefix)
}

func rawMountRecord(ctx context.Context, m *service.RawMountEntry) (goqu.Record, error) {
	typ := m.Type
	if typ == "" {
		typ = "local"
	}
	cfg, err := jsonbArg(m)
	if err != nil {
		return nil, err
	}
	return goqu.Record{"prefix": m.Prefix, "type": typ, "config": cfg, "updated_by": actor(ctx)}, nil
}

func scanRawMount(sc scanner) (*service.RawMountEntry, error) {
	var prefix, typ string
	var config []byte
	if err := sc.Scan(&prefix, &typ, &config); err != nil {
		return nil, err
	}
	var m service.RawMountEntry
	if err := unmarshalJSONB(config, &m); err != nil {
		return nil, err
	}
	m.Prefix = prefix
	m.Type = typ
	return &m, nil
}
