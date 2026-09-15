package core

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/halfking/goacos/internal/config"
)

func TestAuthServiceParseRoundTrip(t *testing.T) {
	cfg := &config.Config{TokenSecret: "unit-test-secret", TokenTTLSeconds: 60}
	a := NewAuthService(cfg, nil)

	claims := jwt.MapClaims{"sub": "nacos", "roles": []string{"ROLE_ADMIN"}, "iss": "goacos",
		"exp": time.Now().Add(time.Minute).Unix()}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(cfg.TokenSecret))
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.Parse(tok)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Username != "nacos" || !got.Admin {
		t.Errorf("claims mismatch: %+v", got)
	}

	// wrong secret
	cfg2 := &config.Config{TokenSecret: "other"}
	if _, err := NewAuthService(cfg2, nil).Parse(tok); err == nil {
		t.Error("token signed with different secret must fail")
	}

	// expired
	claims["exp"] = time.Now().Add(-time.Minute).Unix()
	tokExp, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(cfg.TokenSecret))
	if _, err := a.Parse(tokExp); err == nil {
		t.Error("expired token must fail")
	}
}
