package dbcli

import (
	"io"
	"os"

	"github.com/halfking/goacos/internal/config"
	"github.com/halfking/goacos/internal/storage"
)

func osWriter() io.Writer { return os.Stdout }

func osExit(code int) { os.Exit(code) }

// testConfig builds a minimal config.Config for db subcommands.
func testConfig(d storage.Dialect, host string, port int, user, pass, db string) *config.Config {
	return &config.Config{
		DBType:    string(d),
		MySQLHost: host, MySQLPort: port, MySQLUser: user, MySQLPassword: pass, MySQLDB: db,
		AdminUsername: "nacos", AdminPassword: "nacos",
		MaxOpenConns: 8, MaxIdleConns: 4,
	}
}
