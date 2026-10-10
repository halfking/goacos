package app

import (
	"strings"
	"testing"

	"github.com/halfking/goacos/internal/config"
)

func TestServerRejectsUnsafeAuthBeforeOpeningDatabase(t *testing.T) {
	cfg := &config.Config{AuthEnabled: true, TokenSecret: "short", TokenTTLSeconds: 60,
		AdminUsername: "synthetic-admin", AdminPassword: "synthetic-password",
		DBType: "postgres", DBDSN: "postgres://127.0.0.1:1/isolated_invalid_auth?connect_timeout=1&sslmode=disable"}
	err := RunServer(cfg)
	if err == nil || !strings.Contains(err.Error(), "auth configuration") {
		t.Fatal("unsafe auth reached database opening instead of failing configuration validation")
	}
}
