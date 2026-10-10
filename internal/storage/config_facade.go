package storage

import (
	"context"
	"database/sql"
)

func (s *Store) GetConfig(ctx context.Context, d, g, t string) (out *ConfigInfo, e error) {
	e = s.WithConfigTransaction(ctx, func(ctx context.Context) error { out, e = s.getConfig(ctx, d, g, t); return e })
	return
}
func (s *Store) UpsertConfig(ctx context.Context, in ConfigUpsert) (id int64, created bool, e error) {
	e = s.WithConfigTransaction(ctx, func(ctx context.Context) error { id, created, e = s.upsertConfig(ctx, in); return e })
	return
}
func (s *Store) DeleteConfig(ctx context.Context, d, g, t, u, ip string) (deleted bool, e error) {
	e = s.WithConfigTransaction(ctx, func(ctx context.Context) error { deleted, e = s.deleteConfig(ctx, d, g, t, u, ip); return e })
	return
}
func (s *Store) ListConfigs(ctx context.Context, in ListQuery) (out []ConfigInfo, total int64, e error) {
	e = s.WithConfigTransaction(ctx, func(ctx context.Context) error { out, total, e = s.listConfigs(ctx, in); return e })
	return
}
func (s *Store) ListHistory(ctx context.Context, in HistoryListQuery) (out []HistoryInfo, total int64, e error) {
	e = s.WithConfigTransaction(ctx, func(ctx context.Context) error { out, total, e = s.listHistory(ctx, in); return e })
	return
}
func (s *Store) GetHistory(ctx context.Context, id int64) (out *HistoryInfo, e error) {
	e = s.WithConfigTransaction(ctx, func(ctx context.Context) error { out, e = s.getHistory(ctx, id); return e })
	return
}
func (s *Store) GetPreviousHistory(ctx context.Context, d, g, t string, id int64) (out *HistoryInfo, e error) {
	e = s.WithConfigTransaction(ctx, func(ctx context.Context) error { out, e = s.getPreviousHistory(ctx, d, g, t, id); return e })
	return
}
func (s *Store) ReplaceConfigTags(ctx context.Context, id int64, d, g, t string, tags []string) error {
	return s.WithConfigTransaction(ctx, func(ctx context.Context) error { return s.replaceConfigTags(ctx, id, d, g, t, tags) })
}
func (s *Store) CountConfigs(ctx context.Context) (n int64, e error) {
	e = s.WithConfigTransaction(ctx, func(ctx context.Context) error { n, e = s.countConfigs(ctx); return e })
	return
}

type ConfigMD5 struct{ DataID, Group, Tenant, MD5 string }

func (s *Store) ConfigMD5Index(ctx context.Context) (out []ConfigMD5, e error) {
	e = s.WithConfigTransaction(ctx, func(ctx context.Context) error {
		rows, e := s.Query(ctx, `SELECT data_id,group_id,tenant_id,coalesce(md5,'') FROM config_info`)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var v ConfigMD5
			if e = rows.Scan(&v.DataID, &v.Group, &v.Tenant, &v.MD5); e != nil {
				return e
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	return
}
func (s *Store) ConfigMD5(ctx context.Context, d, g, t string) (out string, e error) {
	e = s.WithConfigTransaction(ctx, func(ctx context.Context) error {
		e = s.QueryRow(ctx, `SELECT coalesce(md5,'') FROM config_info WHERE data_id=? AND group_id=? AND tenant_id=?`, d, g, t).Scan(&out)
		if e == sql.ErrNoRows {
			e = nil
		}
		return e
	})
	return
}
