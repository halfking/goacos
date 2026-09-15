package storage

import (
	"context"
	"database/sql"
)

// User is an auth user row.
type User struct {
	Username string
	Password string // bcrypt hash
	Enabled  bool
}

// GetUser returns the user or nil.
func (s *Store) GetUser(ctx context.Context, username string) (*User, error) {
	var u User
	var enabled int
	err := s.QueryRow(ctx, "SELECT username, password, enabled FROM users WHERE username=?", username).
		Scan(&u.Username, &u.Password, &enabled)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.Enabled = enabled == 1
	return &u, nil
}

// CreateUser inserts a user (bcrypt hash expected).
func (s *Store) CreateUser(ctx context.Context, username, passwordHash string) error {
	_, err := s.Exec(ctx, "INSERT INTO users (username, password, enabled) VALUES (?,?,1)", username, passwordHash)
	return err
}

// UpdatePassword replaces a user's password hash.
func (s *Store) UpdatePassword(ctx context.Context, username, passwordHash string) error {
	_, err := s.Exec(ctx, "UPDATE users SET password=? WHERE username=?", passwordHash, username)
	return err
}

// DeleteUser removes a user with its roles.
func (s *Store) DeleteUser(ctx context.Context, username string) error {
	if _, err := s.Exec(ctx, "DELETE FROM roles WHERE username=?", username); err != nil {
		return err
	}
	_, err := s.Exec(ctx, "DELETE FROM users WHERE username=?", username)
	return err
}

// ListUsers returns one page of users with roles.
func (s *Store) ListUsers(ctx context.Context, pageNo, pageSize int) ([]User, map[string][]string, int64, error) {
	if pageNo < 1 {
		pageNo = 1
	}
	if pageSize < 1 || pageSize > 500 {
		pageSize = 100
	}
	var total int64
	if err := s.QueryRow(ctx, "SELECT COUNT(*) FROM users").Scan(&total); err != nil {
		return nil, nil, 0, err
	}
	rows, err := s.Query(ctx, "SELECT username, password, enabled FROM users ORDER BY username LIMIT ? OFFSET ?",
		pageSize, (pageNo-1)*pageSize)
	if err != nil {
		return nil, nil, 0, err
	}
	defer rows.Close()
	var out []User
	roles := map[string][]string{}
	for rows.Next() {
		var u User
		var enabled int
		if err := rows.Scan(&u.Username, &u.Password, &enabled); err != nil {
			return nil, nil, 0, err
		}
		u.Enabled = enabled == 1
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, 0, err
	}
	rrows, err := s.Query(ctx, "SELECT username, role FROM roles")
	if err != nil {
		return out, roles, total, err
	}
	defer rrows.Close()
	for rrows.Next() {
		var un, role string
		if err := rrows.Scan(&un, &role); err != nil {
			continue
		}
		roles[un] = append(roles[un], role)
	}
	return out, roles, total, rrows.Err()
}

// ListRolesByUser returns the roles of one user.
func (s *Store) ListRolesByUser(ctx context.Context, username string) ([]string, error) {
	rows, err := s.Query(ctx, "SELECT role FROM roles WHERE username=?", username)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AddRole grants a role to a user.
func (s *Store) AddRole(ctx context.Context, username, role string) error {
	_, err := s.Exec(ctx, s.dialect.Rebind(s.dialect.InsertIgnore("roles", "username", "role")), username, role)
	return err
}

// DeleteRole revokes a role from a user.
func (s *Store) DeleteRole(ctx context.Context, username, role string) error {
	_, err := s.Exec(ctx, "DELETE FROM roles WHERE username=? AND role=?", username, role)
	return err
}
