package storage

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// ConfigInfo is a config row (Nacos config_info compatible).
type ConfigInfo struct {
	ID          int64
	DataId      string
	GroupId     string
	Content     string
	Md5         string
	GmtCreate   time.Time
	GmtModified time.Time
	SrcUser     string
	SrcIp       string
	AppName     string
	TenantId    string
	CDesc       string
	CUse        string
	Effect      string
	Type        string
	CSchema     string
}

// HistoryInfo is a config history row.
type HistoryInfo struct {
	Nid         int64
	ID          int64
	DataId      string
	GroupId     string
	AppName     string
	Content     string
	Md5         string
	GmtCreate   time.Time
	GmtModified time.Time
	SrcUser     string
	SrcIp       string
	OpType      string
	TenantId    string
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

const configCols = "id, data_id, group_id, content, md5, gmt_create, gmt_modified, COALESCE(src_user,''), COALESCE(src_ip,''), COALESCE(app_name,''), tenant_id, COALESCE(c_desc,''), COALESCE(c_use,''), COALESCE(effect,''), COALESCE(type,''), COALESCE(c_schema,'')"

func scanConfig(row interface{ Scan(...any) error }) (*ConfigInfo, error) {
	var c ConfigInfo
	err := row.Scan(&c.ID, &c.DataId, &c.GroupId, &c.Content, &c.Md5, &c.GmtCreate, &c.GmtModified,
		&c.SrcUser, &c.SrcIp, &c.AppName, &c.TenantId, &c.CDesc, &c.CUse, &c.Effect, &c.Type, &c.CSchema)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// GetConfig returns the config or nil when not found.
func (s *Store) GetConfig(ctx context.Context, dataId, group, tenant string) (*ConfigInfo, error) {
	row := s.QueryRow(ctx,
		"SELECT "+configCols+" FROM config_info WHERE data_id=? AND group_id=? AND tenant_id=?", dataId, group, tenant)
	c, err := scanConfig(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return c, err
}

// ConfigUpsert carries fields for publish.
type ConfigUpsert struct {
	DataId    string
	GroupId   string
	TenantId  string
	Content   string
	SrcUser   string
	SrcIp     string
	AppName   string
	Type      string
	CDesc     string
	CUse      string
	Effect    string
	CSchema   string
	ConfigMD5 string // computed when empty
}

// UpsertConfig publishes a config; returns (config id, created).
func (s *Store) UpsertConfig(ctx context.Context, u ConfigUpsert) (int64, bool, error) {
	md5v := u.ConfigMD5
	if md5v == "" {
		md5v = md5hex(u.Content)
	}
	var existID int64
	err := s.QueryRow(ctx,
		"SELECT id FROM config_info WHERE data_id=? AND group_id=? AND tenant_id=?",
		u.DataId, u.GroupId, u.TenantId).Scan(&existID)
	created := err == sql.ErrNoRows
	if err != nil && err != sql.ErrNoRows {
		return 0, false, err
	}
	if created {
		insertSQL := `INSERT INTO config_info (data_id, group_id, content, md5, gmt_create, gmt_modified, src_user, src_ip, app_name, tenant_id, c_desc, c_use, effect, type, c_schema)
			 VALUES (?,?,?,?,NOW(),NOW(),?,?,?,?,?,?,?,?,?)`
		args := []any{u.DataId, u.GroupId, u.Content, md5v, nullIfEmpty(u.SrcUser), nullIfEmpty(u.SrcIp), nullIfEmpty(u.AppName), u.TenantId,
			nullIfEmpty(u.CDesc), nullIfEmpty(u.CUse), nullIfEmpty(u.Effect), nullIfEmpty(u.Type), nullIfEmpty(u.CSchema)}
		var id int64
		var ierr error
		if s.dialect == DialectPostgres {
			ierr = s.QueryRow(ctx, s.dialect.Rebind(insertSQL)+" RETURNING id", args...).Scan(&id)
		} else {
			var res sql.Result
			res, ierr = s.Exec(ctx, insertSQL, args...)
			if ierr == nil {
				id, _ = res.LastInsertId()
			}
		}
		if ierr != nil {
			// lost a concurrent create race — fall through to the UPDATE path
			created = false
			if err := s.QueryRow(ctx,
				"SELECT id FROM config_info WHERE data_id=? AND group_id=? AND tenant_id=?",
				u.DataId, u.GroupId, u.TenantId).Scan(&existID); err != nil {
				return 0, false, ierr
			}
		} else {
			if err := s.insertHistory(ctx, HistoryInfo{ID: id, DataId: u.DataId, GroupId: u.GroupId, TenantId: u.TenantId,
				Content: u.Content, Md5: md5v, SrcUser: u.SrcUser, SrcIp: u.SrcIp, AppName: u.AppName, OpType: "I",
				GmtCreate: time.Now(), GmtModified: time.Now()}); err != nil {
				return id, false, err
			}
			return id, true, nil
		}
	}
	if _, err := s.Exec(ctx,
		`UPDATE config_info SET content=?, md5=?, gmt_modified=NOW(), src_user=?, src_ip=?, app_name=?, c_desc=?, c_use=?, effect=?, type=?, c_schema=?
		 WHERE id=?`,
		u.Content, md5v, nullIfEmpty(u.SrcUser), nullIfEmpty(u.SrcIp), nullIfEmpty(u.AppName),
		nullIfEmpty(u.CDesc), nullIfEmpty(u.CUse), nullIfEmpty(u.Effect), nullIfEmpty(u.Type), nullIfEmpty(u.CSchema), existID); err != nil {
		return existID, false, err
	}
	if err := s.insertHistory(ctx, HistoryInfo{ID: existID, DataId: u.DataId, GroupId: u.GroupId, TenantId: u.TenantId,
		Content: u.Content, Md5: md5v, SrcUser: u.SrcUser, SrcIp: u.SrcIp, AppName: u.AppName, OpType: "U",
		GmtCreate: time.Now(), GmtModified: time.Now()}); err != nil {
		return existID, false, err
	}
	return existID, false, nil
}

// DeleteConfig removes a config; returns true when a row was deleted.
func (s *Store) DeleteConfig(ctx context.Context, dataId, group, tenant, srcUser, srcIp string) (bool, error) {
	c, err := s.GetConfig(ctx, dataId, group, tenant)
	if err != nil {
		return false, err
	}
	if c == nil {
		return false, nil
	}
	if _, err := s.Exec(ctx,
		"DELETE FROM config_info WHERE data_id=? AND group_id=? AND tenant_id=?", dataId, group, tenant); err != nil {
		return false, err
	}
	h := HistoryInfo{ID: c.ID, DataId: dataId, GroupId: group, TenantId: tenant, Content: c.Content,
		Md5: c.Md5, SrcUser: srcUser, SrcIp: srcIp, AppName: c.AppName, OpType: "D",
		GmtCreate: time.Now(), GmtModified: time.Now()}
	if err := s.insertHistory(ctx, h); err != nil {
		return true, err
	}
	_, _ = s.Exec(ctx,
		"DELETE FROM config_tags_relation WHERE data_id=? AND group_id=? AND tenant_id=?", dataId, group, tenant)
	return true, nil
}

// ListQuery filters for config listing.
type ListQuery struct {
	TenantId string
	DataId   string // exact when Accurate else blur
	GroupId  string
	AppName  string
	Tag      string
	Accurate bool
	PageNo   int
	PageSize int
	WithMeta bool
}

// ListConfigs returns one page plus total count.
func (s *Store) ListConfigs(ctx context.Context, q ListQuery) ([]ConfigInfo, int64, error) {
	where := []string{"tenant_id=?"}
	args := []any{q.TenantId}
	if q.DataId != "" {
		if q.Accurate {
			where = append(where, "data_id=?")
			args = append(args, q.DataId)
		} else {
			where = append(where, "data_id "+s.dialect.BlurMatch())
			args = append(args, q.DataId)
		}
	}
	if q.GroupId != "" {
		if q.Accurate {
			where = append(where, "group_id=?")
			args = append(args, q.GroupId)
		} else {
			where = append(where, "group_id "+s.dialect.BlurMatch())
			args = append(args, q.GroupId)
		}
	}
	if q.AppName != "" {
		where = append(where, "app_name=?")
		args = append(args, q.AppName)
	}
	join := ""
	if q.Tag != "" {
		join = " JOIN config_tags_relation t ON t.data_id=config_info.data_id AND t.group_id=config_info.group_id AND t.tenant_id=config_info.tenant_id AND t.tag_name=?"
		args = append(args, q.Tag)
	}
	w := strings.Join(where, " AND ")
	var total int64
	if err := s.QueryRow(ctx, "SELECT COUNT(*) FROM config_info"+join+" WHERE "+w, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if q.PageNo < 1 {
		q.PageNo = 1
	}
	if q.PageSize < 1 || q.PageSize > 500 {
		q.PageSize = 100
	}
	order := " ORDER BY id DESC LIMIT ? OFFSET ?"
	args = append(args, q.PageSize, (q.PageNo-1)*q.PageSize)
	rows, err := s.Query(ctx, "SELECT "+configCols+" FROM config_info"+join+" WHERE "+w+order, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []ConfigInfo
	for rows.Next() {
		c, err := scanConfig(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *c)
	}
	return out, total, rows.Err()
}

func (s *Store) insertHistory(ctx context.Context, h HistoryInfo) error {
	_, err := s.Exec(ctx,
		`INSERT INTO his_config_info (id, data_id, group_id, app_name, content, md5, gmt_create, gmt_modified, src_user, src_ip, op_type, tenant_id)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		h.ID, h.DataId, h.GroupId, nullIfEmpty(h.AppName), h.Content, h.Md5, h.GmtCreate, h.GmtModified,
		nullIfEmpty(h.SrcUser), nullIfEmpty(h.SrcIp), h.OpType, h.TenantId)
	return err
}

const historyCols = "nid, id, data_id, group_id, COALESCE(app_name,''), content, COALESCE(md5,''), gmt_create, gmt_modified, COALESCE(src_user,''), COALESCE(src_ip,''), op_type, tenant_id"

func scanHistory(row interface{ Scan(...any) error }) (*HistoryInfo, error) {
	var h HistoryInfo
	err := row.Scan(&h.Nid, &h.ID, &h.DataId, &h.GroupId, &h.AppName, &h.Content, &h.Md5, &h.GmtCreate,
		&h.GmtModified, &h.SrcUser, &h.SrcIp, &h.OpType, &h.TenantId)
	if err != nil {
		return nil, err
	}
	h.OpType = strings.TrimSpace(h.OpType) // char(N) columns pad on PostgreSQL
	return &h, nil
}

// HistoryListQuery filters history listing.
type HistoryListQuery struct {
	TenantId string
	DataId   string
	GroupId  string
	Accurate bool
	PageNo   int
	PageSize int
}

// ListHistory returns one page of history plus total.
func (s *Store) ListHistory(ctx context.Context, q HistoryListQuery) ([]HistoryInfo, int64, error) {
	where := []string{"tenant_id=?"}
	args := []any{q.TenantId}
	if q.DataId != "" {
		if q.Accurate {
			where = append(where, "data_id=?")
		} else {
			where = append(where, "data_id "+s.dialect.BlurMatch())
		}
		args = append(args, q.DataId)
	}
	if q.GroupId != "" {
		if q.Accurate {
			where = append(where, "group_id=?")
		} else {
			where = append(where, "group_id "+s.dialect.BlurMatch())
		}
		args = append(args, q.GroupId)
	}
	w := strings.Join(where, " AND ")
	var total int64
	if err := s.QueryRow(ctx, "SELECT COUNT(*) FROM his_config_info WHERE "+w, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if q.PageNo < 1 {
		q.PageNo = 1
	}
	if q.PageSize < 1 || q.PageSize > 500 {
		q.PageSize = 100
	}
	rows, err := s.Query(ctx,
		"SELECT "+historyCols+" FROM his_config_info WHERE "+w+" ORDER BY nid DESC LIMIT ? OFFSET ?",
		append(args, q.PageSize, (q.PageNo-1)*q.PageSize)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []HistoryInfo
	for rows.Next() {
		h, err := scanHistory(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *h)
	}
	return out, total, rows.Err()
}

// GetHistory returns one history entry by nid.
func (s *Store) GetHistory(ctx context.Context, nid int64) (*HistoryInfo, error) {
	h, err := scanHistory(s.QueryRow(ctx,
		"SELECT "+historyCols+" FROM his_config_info WHERE nid=?", nid))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return h, err
}

// GetPreviousHistory returns the newest history entry older than nid for a config.
func (s *Store) GetPreviousHistory(ctx context.Context, dataId, group, tenant string, nid int64) (*HistoryInfo, error) {
	h, err := scanHistory(s.QueryRow(ctx,
		"SELECT "+historyCols+" FROM his_config_info WHERE data_id=? AND group_id=? AND tenant_id=? AND nid<? ORDER BY nid DESC LIMIT 1",
		dataId, group, tenant, nid))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return h, err
}

// ReplaceConfigTags rewrites the tag relations of one config.
func (s *Store) ReplaceConfigTags(ctx context.Context, id int64, dataId, group, tenant string, tags []string) error {
	if _, err := s.Exec(ctx,
		"DELETE FROM config_tags_relation WHERE id=? AND tag_type=?", id, "text"); err != nil {
		return err
	}
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if _, err := s.Exec(ctx,
			"INSERT INTO config_tags_relation (id, tag_name, tag_type, data_id, group_id, tenant_id) VALUES (?,?,?,?,?,?)",
			id, tag, "text", dataId, group, tenant); err != nil {
			return fmt.Errorf("insert tag %q: %w", tag, err)
		}
	}
	return nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
