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

func TestAuthServiceRejectsTokensOutsideIssuerContract(t *testing.T) {
	cfg := &config.Config{TokenSecret: "unit-test-issuer-contract", TokenTTLSeconds: 60}
	a := NewAuthService(cfg, nil)
	cases := []struct {
		name   string
		method jwt.SigningMethod
		mutate func(jwt.MapClaims)
	}{
		{"missing-issuer", jwt.SigningMethodHS256, func(c jwt.MapClaims) { delete(c, "iss") }},
		{"wrong-issuer", jwt.SigningMethodHS256, func(c jwt.MapClaims) { c["iss"] = "another-service" }},
		{"missing-expiry", jwt.SigningMethodHS256, func(c jwt.MapClaims) { delete(c, "exp") }},
		{"expired", jwt.SigningMethodHS256, func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Minute).Unix() }},
		{"future-issued-at", jwt.SigningMethodHS256, func(c jwt.MapClaims) { c["iat"] = time.Now().Add(time.Hour).Unix() }},
		{"missing-subject", jwt.SigningMethodHS256, func(c jwt.MapClaims) { delete(c, "sub") }},
		{"blank-subject", jwt.SigningMethodHS256, func(c jwt.MapClaims) { c["sub"] = "  " }},
		{"padded-subject", jwt.SigningMethodHS256, func(c jwt.MapClaims) { c["sub"] = " reader " }},
		{"HS384", jwt.SigningMethodHS384, func(jwt.MapClaims) {}},
		{"HS512", jwt.SigningMethodHS512, func(jwt.MapClaims) {}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := jwt.MapClaims{"sub": "reader", "roles": []string{"ROLE_USER"}, "iss": "goacos",
				"iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix()}
			tc.mutate(c)
			token, err := jwt.NewWithClaims(tc.method, c).SignedString([]byte(cfg.TokenSecret))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.Parse(token); err == nil {
				t.Error("token outside goacos issuer contract was accepted")
			}
		})
	}
}
