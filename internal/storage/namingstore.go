package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Instance is one service instance (goacos naming_instance row).
type Instance struct {
	ID              int64
	NamespaceId     string
	GroupName       string
	ServiceName     string
	ClusterName     string
	IP              string
	Port            uint32
	Weight          float64
	Healthy         bool
	Enabled         bool
	Ephemeral       bool
	MetadataJSON    string
	LastHeartbeatMS int64
	GmtCreate       time.Time
	GmtModified     time.Time
}

// Metadata parses MetadataJSON into a map (never nil).
func (i *Instance) Metadata() map[string]string {
	m := map[string]string{}
	if i.MetadataJSON != "" {
		_ = json.Unmarshal([]byte(i.MetadataJSON), &m)
	}
	return m
}

// ServiceRow is one service (goacos naming_service row).
type ServiceRow struct {
	ID               int64
	NamespaceId      string
	GroupName        string
	Name             string
	ProtectThreshold float64
	MetadataJSON     string
	AppName          string
	IPCount          int64
	ClusterCount     int64
	GmtCreate        time.Time
	GmtModified      time.Time
}

// Metadata parses MetadataJSON into a map (never nil).
func (s *ServiceRow) Metadata() map[string]string {
	m := map[string]string{}
	if s.MetadataJSON != "" {
		_ = json.Unmarshal([]byte(s.MetadataJSON), &m)
	}
	return m
}

const instanceCols = "id, namespace_id, group_name, service_name, cluster_name, ip, port, weight, healthy, enabled, ephemeral, IFNULL(metadata,''), last_heartbeat_ms, gmt_create, gmt_modified"

func scanInstance(row interface{ Scan(...any) error }) (*Instance, error) {
	var i Instance
	var healthy, enabled, ephemeral int
	err := row.Scan(&i.ID, &i.NamespaceId, &i.GroupName, &i.ServiceName, &i.ClusterName, &i.IP, &i.Port,
		&i.Weight, &healthy, &enabled, &ephemeral, &i.MetadataJSON, &i.LastHeartbeatMS, &i.GmtCreate, &i.GmtModified)
	if err != nil {
		return nil, err
	}
	i.Healthy, i.Enabled, i.Ephemeral = healthy == 1, enabled == 1, ephemeral == 1
	return &i, nil
}

// EnsureService creates the service row when missing.
func (s *Store) EnsureService(ctx context.Context, ns, group, name string) error {
	_, err := s.DB.ExecContext(ctx,
		"INSERT IGNORE INTO naming_service (namespace_id, group_name, name) VALUES (?,?,?)", ns, group, name)
	return err
}

// GetService returns the service row or nil.
func (s *Store) GetService(ctx context.Context, ns, group, name string) (*ServiceRow, error) {
	row := s.DB.QueryRowContext(ctx,
		`SELECT s.id, s.namespace_id, s.group_name, s.name, s.protect_threshold, IFNULL(s.metadata,''), IFNULL(s.app_name,''),
		        s.gmt_create, s.gmt_modified,
		        (SELECT COUNT(*) FROM naming_instance i WHERE i.namespace_id=s.namespace_id AND i.group_name=s.group_name AND i.service_name=s.name),
		        (SELECT COUNT(DISTINCT i.cluster_name) FROM naming_instance i WHERE i.namespace_id=s.namespace_id AND i.group_name=s.group_name AND i.service_name=s.name)
		 FROM naming_service s WHERE s.namespace_id=? AND s.group_name=? AND s.name=?`, ns, group, name)
	var sr ServiceRow
	err := row.Scan(&sr.ID, &sr.NamespaceId, &sr.GroupName, &sr.Name, &sr.ProtectThreshold, &sr.MetadataJSON, &sr.AppName,
		&sr.GmtCreate, &sr.GmtModified, &sr.IPCount, &sr.ClusterCount)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sr, nil
}

// UpdateService mutates protect threshold / metadata / app name.
func (s *Store) UpdateService(ctx context.Context, ns, group, name string, protect *float64, metadata *string, appName *string) error {
	sets := []string{"gmt_modified=NOW()"}
	args := []any{}
	if protect != nil {
		sets = append(sets, "protect_threshold=?")
		args = append(args, *protect)
	}
	if metadata != nil {
		sets = append(sets, "metadata=?")
		args = append(args, *metadata)
	}
	if appName != nil {
		sets = append(sets, "app_name=?")
		args = append(args, *appName)
	}
	args = append(args, ns, group, name)
	_, err := s.DB.ExecContext(ctx,
		"UPDATE naming_service SET "+strings.Join(sets, ", ")+" WHERE namespace_id=? AND group_name=? AND name=?", args...)
	return err
}

// DeleteService removes the service row when it has no instances.
func (s *Store) DeleteService(ctx context.Context, ns, group, name string) (bool, error) {
	res, err := s.DB.ExecContext(ctx,
		"DELETE FROM naming_service WHERE namespace_id=? AND group_name=? AND name=? AND NOT EXISTS (SELECT 1 FROM naming_instance i WHERE i.namespace_id=naming_service.namespace_id AND i.group_name=naming_service.group_name AND i.service_name=naming_service.name)",
		ns, group, name)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ServiceListQuery filters service listing.
type ServiceListQuery struct {
	NamespaceId string
	GroupName   string // exact; empty = all groups
	NameBlur    string
	PageNo      int
	PageSize    int
}

// ListServices returns one page of services with counts.
func (s *Store) ListServices(ctx context.Context, q ServiceListQuery) ([]ServiceRow, int64, error) {
	where := []string{"s.namespace_id=?"}
	args := []any{q.NamespaceId}
	if q.GroupName != "" {
		where = append(where, "s.group_name=?")
		args = append(args, q.GroupName)
	}
	if q.NameBlur != "" {
		where = append(where, "s.name LIKE CONCAT('%',?,'%')")
		args = append(args, q.NameBlur)
	}
	w := strings.Join(where, " AND ")
	var total int64
	if err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM naming_service s WHERE "+w, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if q.PageNo < 1 {
		q.PageNo = 1
	}
	if q.PageSize < 1 || q.PageSize > 500 {
		q.PageSize = 10
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT s.id, s.namespace_id, s.group_name, s.name, s.protect_threshold, IFNULL(s.metadata,''), IFNULL(s.app_name,''),
		        s.gmt_create, s.gmt_modified,
		        (SELECT COUNT(*) FROM naming_instance i WHERE i.namespace_id=s.namespace_id AND i.group_name=s.group_name AND i.service_name=s.name),
		        (SELECT COUNT(DISTINCT i.cluster_name) FROM naming_instance i WHERE i.namespace_id=s.namespace_id AND i.group_name=s.group_name AND i.service_name=s.name)
		 FROM naming_service s WHERE `+w+" ORDER BY s.id DESC LIMIT ? OFFSET ?",
		append(args, q.PageSize, (q.PageNo-1)*q.PageSize)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []ServiceRow
	for rows.Next() {
		var sr ServiceRow
		if err := rows.Scan(&sr.ID, &sr.NamespaceId, &sr.GroupName, &sr.Name, &sr.ProtectThreshold, &sr.MetadataJSON, &sr.AppName,
			&sr.GmtCreate, &sr.GmtModified, &sr.IPCount, &sr.ClusterCount); err != nil {
			return nil, 0, err
		}
		out = append(out, sr)
	}
	return out, total, rows.Err()
}

// InstanceUpsert carries register params.
type InstanceUpsert struct {
	NamespaceId string
	GroupName   string
	ServiceName string
	ClusterName string
	IP          string
	Port        uint32
	Weight      float64
	Enabled     bool
	Ephemeral   bool
	Metadata    map[string]string
}

// RegisterInstance creates or refreshes an instance. Returns true when created.
func (s *Store) RegisterInstance(ctx context.Context, u InstanceUpsert) (bool, error) {
	meta := "{}"
	if u.Metadata != nil {
		b, err := json.Marshal(u.Metadata)
		if err != nil {
			return false, err
		}
		meta = string(b)
	}
	if u.ClusterName == "" {
		u.ClusterName = "DEFAULT"
	}
	if u.Weight <= 0 {
		u.Weight = 1
	}
	now := NowMS()
	var id int64
	err := s.DB.QueryRowContext(ctx,
		`SELECT id FROM naming_instance WHERE namespace_id=? AND group_name=? AND service_name=? AND cluster_name=? AND ip=? AND port=?`,
		u.NamespaceId, u.GroupName, u.ServiceName, u.ClusterName, u.IP, u.Port).Scan(&id)
	created := err == sql.ErrNoRows
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	if created {
		if err := s.EnsureService(ctx, u.NamespaceId, u.GroupName, u.ServiceName); err != nil {
			return false, err
		}
		_, err = s.DB.ExecContext(ctx,
			`INSERT INTO naming_instance (namespace_id, group_name, service_name, cluster_name, ip, port, weight, healthy, enabled, ephemeral, metadata, last_heartbeat_ms)
			 VALUES (?,?,?,?,?,?,?,1,?,?,?,?)`,
			u.NamespaceId, u.GroupName, u.ServiceName, u.ClusterName, u.IP, u.Port, u.Weight,
			boolInt(u.Enabled), boolInt(u.Ephemeral), meta, now)
		return true, err
	}
	_, err = s.DB.ExecContext(ctx,
		`UPDATE naming_instance SET weight=?, enabled=?, ephemeral=?, metadata=?, last_heartbeat_ms=?, healthy=1, gmt_modified=NOW() WHERE id=?`,
		u.Weight, boolInt(u.Enabled), boolInt(u.Ephemeral), meta, now, id)
	return false, err
}

// BeatInstance refreshes a heartbeat; creates the instance when missing
// (auto-attach convenience, mirrors client re-register behavior).
func (s *Store) BeatInstance(ctx context.Context, u InstanceUpsert) (created bool, err error) {
	res, err := s.DB.ExecContext(ctx,
		`UPDATE naming_instance SET last_heartbeat_ms=?, healthy=1, gmt_modified=NOW()
		 WHERE namespace_id=? AND group_name=? AND service_name=? AND cluster_name=? AND ip=? AND port=?`,
		NowMS(), u.NamespaceId, u.GroupName, u.ServiceName, u.ClusterName, u.IP, u.Port)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return false, nil
	}
	return s.RegisterInstance(ctx, u)
}

// DeleteInstance removes one instance; true when deleted.
func (s *Store) DeleteInstance(ctx context.Context, ns, group, service, cluster, ip string, port uint32) (bool, error) {
	if cluster == "" {
		cluster = "DEFAULT"
	}
	res, err := s.DB.ExecContext(ctx,
		"DELETE FROM naming_instance WHERE namespace_id=? AND group_name=? AND service_name=? AND cluster_name=? AND ip=? AND port=?",
		ns, group, service, cluster, ip, port)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// GetInstance returns one instance or nil.
func (s *Store) GetInstance(ctx context.Context, ns, group, service, cluster, ip string, port uint32) (*Instance, error) {
	if cluster == "" {
		cluster = "DEFAULT"
	}
	i, err := scanInstance(s.DB.QueryRowContext(ctx,
		"SELECT "+instanceCols+" FROM naming_instance WHERE namespace_id=? AND group_name=? AND service_name=? AND cluster_name=? AND ip=? AND port=?",
		ns, group, service, cluster, ip, port))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return i, err
}

// InstanceListQuery filters instance listing.
type InstanceListQuery struct {
	NamespaceId string
	GroupName   string
	ServiceName string
	Clusters    string // comma separated, empty = all
	HealthyOnly bool
}

// ListInstances returns matching instances ordered by cluster.
func (s *Store) ListInstances(ctx context.Context, q InstanceListQuery) ([]Instance, error) {
	where := []string{"namespace_id=?", "group_name=?", "service_name=?"}
	args := []any{q.NamespaceId, q.GroupName, q.ServiceName}
	if q.Clusters != "" {
		clusters := []string{}
		for _, c := range strings.Split(q.Clusters, ",") {
			c = strings.TrimSpace(c)
			if c != "" {
				clusters = append(clusters, c)
			}
		}
		if len(clusters) > 0 {
			where = append(where, "cluster_name IN ("+strings.TrimSuffix(strings.Repeat("?,", len(clusters)), ",")+")")
			for _, c := range clusters {
				args = append(args, c)
			}
		}
	}
	if q.HealthyOnly {
		where = append(where, "healthy=1")
	}
	rows, err := s.DB.QueryContext(ctx,
		"SELECT "+instanceCols+" FROM naming_instance WHERE "+strings.Join(where, " AND ")+" ORDER BY cluster_name, ip, port", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Instance
	for rows.Next() {
		i, err := scanInstance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *i)
	}
	return out, rows.Err()
}

// SweepEphemeral marks stale ephemeral instances unhealthy and removes the
// long-stale ones. Idempotent, safe with multiple goacos nodes.
func (s *Store) SweepEphemeral(ctx context.Context, timeoutMS, deleteAfterMS int64) (markedUnhealthy, deleted int64, err error) {
	now := NowMS()
	res, err := s.DB.ExecContext(ctx,
		"UPDATE naming_instance SET healthy=0 WHERE ephemeral=1 AND healthy=1 AND last_heartbeat_ms < ?", now-timeoutMS)
	if err != nil {
		return 0, 0, fmt.Errorf("sweep unhealthy: %w", err)
	}
	markedUnhealthy, _ = res.RowsAffected()
	res, err = s.DB.ExecContext(ctx,
		"DELETE FROM naming_instance WHERE ephemeral=1 AND last_heartbeat_ms < ?", now-deleteAfterMS)
	if err != nil {
		return markedUnhealthy, 0, fmt.Errorf("sweep delete: %w", err)
	}
	deleted, _ = res.RowsAffected()
	return markedUnhealthy, deleted, nil
}

// CountNaming returns totals for the metrics endpoint.
func (s *Store) CountNaming(ctx context.Context) (services, instances int64, err error) {
	if err = s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM naming_service").Scan(&services); err != nil {
		return
	}
	err = s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM naming_instance").Scan(&instances)
	return
}

// CountConfigs returns config totals for metrics.
func (s *Store) CountConfigs(ctx context.Context) (n int64, err error) {
	err = s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM config_info").Scan(&n)
	return
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
