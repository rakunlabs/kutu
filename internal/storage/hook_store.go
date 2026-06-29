package storage

import (
	"context"
	"fmt"

	"github.com/doug-martin/goqu/v9"

	"github.com/rakunlabs/kutu/internal/hook"
)

const hookTable = "kutu_hook"

// ListHooks returns every configured hook.
func (s *Store) ListHooks(ctx context.Context) ([]hook.Hook, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM `+hookTable+` ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list hooks: %w", err)
	}
	defer rows.Close()

	var out []hook.Hook
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var h hook.Hook
		if err := unmarshalJSONB(data, &h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ReplaceHooks swaps the entire hook set in one transaction.
func (s *Store) ReplaceHooks(ctx context.Context, hooks []hook.Hook) error {
	who := actor(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin hooks tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM `+hookTable); err != nil {
		return fmt.Errorf("clear hooks: %w", err)
	}
	for i := range hooks {
		h := &hooks[i]
		data, err := jsonbArg(h)
		if err != nil {
			return err
		}
		ds := dialect.Insert(hookTable).Rows(goqu.Record{
			"name": h.Name, "enabled": h.Enabled, "data": data, "updated_by": who,
		}).Prepared(true)
		sqlStr, args, err := ds.ToSQL()
		if err != nil {
			return fmt.Errorf("build hook insert sql: %w", err)
		}
		if _, err := tx.ExecContext(ctx, sqlStr, args...); err != nil {
			return fmt.Errorf("insert hook %q: %w", h.Name, err)
		}
	}
	return tx.Commit()
}
