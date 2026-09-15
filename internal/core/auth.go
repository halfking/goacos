package core

import (
	"context"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/halfking/goacos/internal/config"
	"github.com/halfking/goacos/internal/storage"
)

// ErrBadCredentials is returned on failed logins.
var ErrBadCredentials = errors.New("unknown user or wrong password")

// AuthService issues and validates Nacos-compatible access tokens (JWT HS256).
type AuthService struct {
	cfg *config.Config
	st  *storage.Store
}

// NewAuthService builds the service.
func NewAuthService(cfg *config.Config, st *storage.Store) *AuthService {
	return &AuthService{cfg: cfg, st: st}
}

// Login verifies credentials and returns (token, ttlSeconds, globalAdmin).
func (a *AuthService) Login(ctx context.Context, username, password string) (string, int, bool, error) {
	u, err := a.st.GetUser(ctx, username)
	if err != nil {
		return "", 0, false, err
	}
	if u == nil || !u.Enabled || bcrypt.CompareHashAndPassword([]byte(u.Password), []byte(password)) != nil {
		return "", 0, false, ErrBadCredentials
	}
	roles, err := a.st.ListRolesByUser(ctx, username)
	if err != nil {
		return "", 0, false, err
	}
	admin := false
	for _, r := range roles {
		if r == "ROLE_ADMIN" {
			admin = true
		}
	}
	now := time.Now()
	claims := jwt.MapClaims{
		"sub":   username,
		"roles": roles,
		"iss":   "goacos",
		"iat":   now.Unix(),
		"exp":   now.Add(time.Duration(a.cfg.TokenTTLSeconds) * time.Second).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString([]byte(a.cfg.TokenSecret))
	if err != nil {
		return "", 0, false, err
	}
	return signed, a.cfg.TokenTTLSeconds, admin, nil
}

// Claims is the parsed token payload.
type Claims struct {
	Username string
	Roles    []string
	Admin    bool
}

// Parse validates a token string and returns its claims.
func (a *AuthService) Parse(tokenString string) (*Claims, error) {
	tok, err := jwt.Parse(tokenString, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return []byte(a.cfg.TokenSecret), nil
	})
	if err != nil || !tok.Valid {
		return nil, errors.New("invalid token")
	}
	mc, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		return nil, errors.New("invalid claims")
	}
	sub, _ := mc["sub"].(string)
	if sub == "" {
		return nil, errors.New("invalid subject")
	}
	var roles []string
	if raw, ok := mc["roles"].([]any); ok {
		for _, r := range raw {
			if s, ok := r.(string); ok {
				roles = append(roles, s)
			}
		}
	}
	admin := false
	for _, r := range roles {
		if r == "ROLE_ADMIN" {
			admin = true
		}
	}
	return &Claims{Username: sub, Roles: roles, Admin: admin}, nil
}

// HashPassword produces a bcrypt hash for storage.
func HashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(b), err
}
