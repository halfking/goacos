package config

import (
	"strings"
	"testing"
)

func TestEnabledAuthConfigurationBoundaries(t *testing.T) {
	base := Config{AuthEnabled: true, TokenSecret: strings.Repeat("k", 32), AdminUsername: "synthetic-admin", AdminPassword: strings.Repeat("p", 12), TokenTTLSeconds: 60}
	cases := []struct {
		name      string
		mutate    func(*Config)
		wantError bool
	}{
		{"minimum-key-password-ttl", func(c *Config) { c.TokenTTLSeconds = 1 }, false},
		{"maximum-password-ttl", func(c *Config) { c.AdminPassword = strings.Repeat("p", 72); c.TokenTTLSeconds = 86400 }, false},
		{"unicode-password", func(c *Config) { c.AdminPassword = strings.Repeat("字", 12) }, false},
		{"key-under-minimum", func(c *Config) { c.TokenSecret = strings.Repeat("k", 31) }, true},
		{"password-under-minimum", func(c *Config) { c.AdminPassword = strings.Repeat("p", 11) }, true},
		{"bcrypt-byte-limit", func(c *Config) { c.AdminPassword = strings.Repeat("p", 73) }, true},
		{"unicode-bcrypt-byte-limit", func(c *Config) { c.AdminPassword = strings.Repeat("字", 25) }, true},
		{"invalid-utf8-password", func(c *Config) { c.AdminPassword = strings.Repeat("\xff", 12) }, true},
		{"password-padding", func(c *Config) { c.AdminPassword = " " + c.AdminPassword }, true},
		{"username-padding", func(c *Config) { c.AdminUsername = " " + c.AdminUsername }, true},
		{"negative-ttl", func(c *Config) { c.TokenTTLSeconds = -1 }, true},
		{"ttl-over-bound", func(c *Config) { c.TokenTTLSeconds = 86401 }, true},
		{"auth-disabled-compatibility", func(c *Config) { c.AuthEnabled = false; c.TokenSecret = ""; c.AdminPassword = "nacos" }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mutate(&cfg)
			if got := cfg.ValidateAuth(); (got != nil) != tc.wantError {
				t.Errorf("configuration validation error=%t want %t", got != nil, tc.wantError)
			}
		})
	}
	var missing *Config
	if missing.ValidateAuth() == nil {
		t.Fatal("nil configuration accepted")
	}
}

func TestFromEnvHasNoPublicSigningKeyFallback(t *testing.T) {
	t.Setenv("GOACOS_AUTH_TOKEN_SECRET", "")
	t.Setenv("NACOS_AUTH_TOKEN_SECRET", "")
	if FromEnv().TokenSecret != "" {
		t.Fatal("missing explicit key used a public signing-key fallback")
	}
}
