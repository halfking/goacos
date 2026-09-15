package storage

import (
	"context"
)

// Namespace is a tenant_info row (kp='1').
type Namespace struct {
	NamespaceId string
	Name        string
	Desc        string
	ConfigCount int64
}

// ListNamespaces returns all namespaces with config counts.
func (s *Store) ListNamespaces(ctx context.Context) ([]Namespace, error) {
	rows, err := s.Query(ctx,
		`SELECT t.tenant_id, COALESCE(t.namespace_name,''), COALESCE(t.namespace_desc,''),
		        (SELECT COUNT(*) FROM config_info c WHERE c.tenant_id=t.tenant_id)
		 FROM tenant_info t WHERE t.kp='1' ORDER BY t.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Namespace
	for rows.Next() {
		var n Namespace
		if err := rows.Scan(&n.NamespaceId, &n.Name, &n.Desc, &n.ConfigCount); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// CreateNamespace inserts a namespace; returns false when it already exists.
func (s *Store) CreateNamespace(ctx context.Context, id, name, desc string) (bool, error) {
	var n int
	if err := s.QueryRow(ctx,
		"SELECT COUNT(*) FROM tenant_info WHERE kp='1' AND tenant_id=?", id).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	_, err := s.Exec(ctx,
		"INSERT INTO tenant_info (kp, tenant_id, namespace_name, namespace_desc) VALUES ('1',?,?,?)", id, name, desc)
	return true, err
}

// UpdateNamespace edits name/desc.
func (s *Store) UpdateNamespace(ctx context.Context, id, name, desc string) error {
	_, err := s.Exec(ctx,
		"UPDATE tenant_info SET namespace_name=?, namespace_desc=? WHERE kp='1' AND tenant_id=?", name, desc, id)
	return err
}

// DeleteNamespace removes the namespace row; refuses when configs exist.
func (s *Store) DeleteNamespace(ctx context.Context, id string) (bool, string, error) {
	var n int64
	if err := s.QueryRow(ctx, "SELECT COUNT(*) FROM config_info WHERE tenant_id=?", id).Scan(&n); err != nil {
		return false, "", err
	}
	if n > 0 {
		return false, "namespace has configs, delete them first", nil
	}
	res, err := s.Exec(ctx, "DELETE FROM tenant_info WHERE kp='1' AND tenant_id=?", id)
	if err != nil {
		return false, "", err
	}
	aff, _ := res.RowsAffected()
	return aff > 0, "", nil
}
