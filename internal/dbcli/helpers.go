package dbcli

import (
	"io"
	"os"

	"github.com/halfking/goacos/internal/config"
)

func osWriter() io.Writer { return os.Stdout }

func osExit(code int) { os.Exit(code) }

// testConfig builds a minimal config.Config for db subcommands.
func testConfig(host string, port int, user, pass, db string) *config.Config {
	return &config.Config{
		MySQLHost: host, MySQLPort: port, MySQLUser: user, MySQLPassword: pass, MySQLDB: db,
		AdminUsername: "nacos", AdminPassword: "nacos",
		MaxOpenConns: 8, MaxIdleConns: 4,
	}
}
