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

const proxyListenerTable = "kutu_proxy_listener"
const proxyServerTable = "kutu_proxy_server"

// Only the `data` blob is read back; the other columns exist so query
// filters/sorts hit real columns.
var proxyDataCol = []any{goqu.C("data")}

// ── listeners ──

func (s *Store) ListProxyListeners(ctx context.Context, q *query.Query) ([]service.ProxyListener, error) {
	ds := dialect.From(proxyListenerTable).Select(proxyDataCol...).Order(goqu.C("name").Asc())
	if q != nil {
		q.Select = nil
		ds = adaptergoqu.Select(q, ds)
	} else {
		ds = ds.Prepared(true)
	}
	sqlStr, args, err := ds.ToSQL()
	if err != nil {
		return nil, fmt.Errorf("build proxy listener list sql: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("list proxy listeners: %w", err)
	}
	defer rows.Close()

	var out []service.ProxyListener
	for rows.Next() {
		l, err := scanProxyListener(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *l)
	}
	return out, rows.Err()
}

func (s *Store) GetProxyListener(ctx context.Context, id string) (*service.ProxyListener, error) {
	ds := dialect.From(proxyListenerTable).Select(proxyDataCol...).
		Where(goqu.Ex{"id": id}).Prepared(true)
	sqlStr, args, err := ds.ToSQL()
	if err != nil {
		return nil, fmt.Errorf("build proxy listener get sql: %w", err)
	}
	l, err := scanProxyListener(s.db.QueryRowContext(ctx, sqlStr, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("proxy listener %q: %w", id, service.ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	return l, nil
}

func (s *Store) CreateProxyListener(ctx context.Context, l *service.ProxyListener) error {
	rec, err := proxyListenerRecord(ctx, l)
	if err != nil {
		return err
	}
	ds := dialect.Insert(proxyListenerTable).Rows(rec).Prepared(true)
	if _, err := s.execDS(ctx, ds); err != nil {
		return fmt.Errorf("create proxy listener: %w", err)
	}
	return nil
}

func (s *Store) UpdateProxyListener(ctx context.Context, l *service.ProxyListener) error {
	rec, err := proxyListenerRecord(ctx, l)
	if err != nil {
		return err
	}
	delete(rec, "id")
	rec["updated_at"] = goqu.L("now()")
	ds := dialect.Update(proxyListenerTable).Set(rec).Where(goqu.Ex{"id": l.ID}).Prepared(true)
	res, err := s.execDS(ctx, ds)
	if err != nil {
		return fmt.Errorf("update proxy listener: %w", err)
	}
	return notFoundIfZero(res, "proxy listener", l.ID)
}

func (s *Store) DeleteProxyListener(ctx context.Context, id string) error {
	ds := dialect.Delete(proxyListenerTable).Where(goqu.Ex{"id": id}).Prepared(true)
	res, err := s.execDS(ctx, ds)
	if err != nil {
		return fmt.Errorf("delete proxy listener: %w", err)
	}
	return notFoundIfZero(res, "proxy listener", id)
}

func proxyListenerRecord(ctx context.Context, l *service.ProxyListener) (goqu.Record, error) {
	data, err := jsonbArg(l)
	if err != nil {
		return nil, err
	}
	return goqu.Record{
		"id": l.ID, "name": l.Name, "protocol": l.Protocol, "port": l.Port,
		"enabled": l.Enabled, "data": data, "updated_by": actor(ctx),
	}, nil
}

func scanProxyListener(sc scanner) (*service.ProxyListener, error) {
	var data []byte
	if err := sc.Scan(&data); err != nil {
		return nil, err
	}
	var l service.ProxyListener
	if err := unmarshalJSONB(data, &l); err != nil {
		return nil, err
	}
	return &l, nil
}

// ── servers ──

func (s *Store) ListProxyServers(ctx context.Context, q *query.Query) ([]service.ProxyServer, error) {
	ds := dialect.From(proxyServerTable).Select(proxyDataCol...).Order(goqu.C("name").Asc())
	if q != nil {
		q.Select = nil
		ds = adaptergoqu.Select(q, ds)
	} else {
		ds = ds.Prepared(true)
	}
	sqlStr, args, err := ds.ToSQL()
	if err != nil {
		return nil, fmt.Errorf("build proxy server list sql: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("list proxy servers: %w", err)
	}
	defer rows.Close()

	var out []service.ProxyServer
	for rows.Next() {
		srv, err := scanProxyServer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *srv)
	}
	return out, rows.Err()
}

func (s *Store) GetProxyServer(ctx context.Context, id string) (*service.ProxyServer, error) {
	ds := dialect.From(proxyServerTable).Select(proxyDataCol...).
		Where(goqu.Ex{"id": id}).Prepared(true)
	sqlStr, args, err := ds.ToSQL()
	if err != nil {
		return nil, fmt.Errorf("build proxy server get sql: %w", err)
	}
	srv, err := scanProxyServer(s.db.QueryRowContext(ctx, sqlStr, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("proxy server %q: %w", id, service.ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	return srv, nil
}

func (s *Store) CreateProxyServer(ctx context.Context, srv *service.ProxyServer) error {
	rec, err := proxyServerRecord(ctx, srv)
	if err != nil {
		return err
	}
	ds := dialect.Insert(proxyServerTable).Rows(rec).Prepared(true)
	if _, err := s.execDS(ctx, ds); err != nil {
		return fmt.Errorf("create proxy server: %w", err)
	}
	return nil
}

func (s *Store) UpdateProxyServer(ctx context.Context, srv *service.ProxyServer) error {
	rec, err := proxyServerRecord(ctx, srv)
	if err != nil {
		return err
	}
	delete(rec, "id")
	rec["updated_at"] = goqu.L("now()")
	ds := dialect.Update(proxyServerTable).Set(rec).Where(goqu.Ex{"id": srv.ID}).Prepared(true)
	res, err := s.execDS(ctx, ds)
	if err != nil {
		return fmt.Errorf("update proxy server: %w", err)
	}
	return notFoundIfZero(res, "proxy server", srv.ID)
}

func (s *Store) DeleteProxyServer(ctx context.Context, id string) error {
	ds := dialect.Delete(proxyServerTable).Where(goqu.Ex{"id": id}).Prepared(true)
	res, err := s.execDS(ctx, ds)
	if err != nil {
		return fmt.Errorf("delete proxy server: %w", err)
	}
	return notFoundIfZero(res, "proxy server", id)
}

func proxyServerRecord(ctx context.Context, srv *service.ProxyServer) (goqu.Record, error) {
	data, err := jsonbArg(srv)
	if err != nil {
		return nil, err
	}
	return goqu.Record{
		"id": srv.ID, "name": srv.Name, "enabled": srv.Enabled,
		"listener_id": srv.ListenerID, "protocol": srv.Protocol,
		"data": data, "updated_by": actor(ctx),
	}, nil
}

func scanProxyServer(sc scanner) (*service.ProxyServer, error) {
	var data []byte
	if err := sc.Scan(&data); err != nil {
		return nil, err
	}
	var srv service.ProxyServer
	if err := unmarshalJSONB(data, &srv); err != nil {
		return nil, err
	}
	return &srv, nil
}
