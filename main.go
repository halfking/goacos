// goacos — a lightweight, Nacos-compatible configuration center and service
// discovery server in Go, backed by MySQL.
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/halfking/goacos/internal/app"
	"github.com/halfking/goacos/internal/buildinfo"
	"github.com/halfking/goacos/internal/config"
	"github.com/halfking/goacos/internal/dbcli"
)

// version is injected at build time via -ldflags "-X main.version=...".
var version string

func main() {
	if version != "" {
		buildinfo.Version = version
	}
	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{"serve"}
	}
	switch args[0] {
	case "serve":
		if err := app.RunServer(config.FromEnv()); err != nil {
			fmt.Fprintln(os.Stderr, "goacos:", err)
			os.Exit(1)
		}
	case "version":
		fmt.Printf("goacos %s (commit %s, built %s)\nnacos api compatibility: %s\n",
			buildinfo.Version, buildinfo.GitCommit, buildinfo.BuildDate, buildinfo.NacosAPICompat)
	case "db":
		dbcli.Run(args[1:])
	case "healthcheck":
		addr := "127.0.0.1:8848"
		if len(args) > 1 {
			addr = args[1]
		}
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get("http://" + addr + "/nacos/actuator/health")
		if err != nil {
			fmt.Fprintln(os.Stderr, "unhealthy:", err)
			os.Exit(1)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			fmt.Fprintf(os.Stderr, "unhealthy: %s %s\n", resp.Status, b)
			os.Exit(1)
		}
		fmt.Println("healthy")
	default:
		fmt.Fprintf(os.Stderr, `goacos — lightweight Nacos-compatible server (Go + MySQL)

Usage:
  goacos serve                 run the server (default)
  goacos db discover|verify|init   MySQL discovery & initialization
  goacos healthcheck [addr]    container health probe
  goacos version               print version

Server configuration is environment-driven (GOACOS_* or NACOS_* prefix):
  GOACOS_DB_TYPE=mysql|postgres           database engine (default mysql)
  GOACOS_DB_DSN=postgres://...            full DSN override (optional)
  GOACOS_PORT=8848                        HTTP listen port
  GOACOS_MYSQL_HOST=127.0.0.1             database host (database auto-created)
  GOACOS_MYSQL_PORT=3306                  database port (mysql 3306 / pg 5432)
  GOACOS_MYSQL_DB=goacos                  database name
  GOACOS_MYSQL_USER=root                  database user
  GOACOS_MYSQL_PASSWORD=                  database password
  GOACOS_AUTH_ENABLED=false               require access tokens
  GOACOS_AUTH_TOKEN_SECRET=...            JWT signing secret (set in HA setups)
  GOACOS_ADMIN_USERNAME=nacos             seeded admin username
  GOACOS_ADMIN_PASSWORD=nacos             seeded admin password
`)
		os.Exit(2)
	}
}
