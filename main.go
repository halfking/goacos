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

// version and buildNumber are injected at build time via -ldflags
// "-X main.version=... -X main.buildNumber=...". buildNumber is the compile
// count (git rev-list --count HEAD); version+buildNumber composes the unique
// build identity (see internal/buildinfo).
var version, buildNumber string

func main() {
	if version != "" {
		buildinfo.Version = version
	}
	if buildNumber != "" {
		buildinfo.BuildNumber = buildNumber
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
		fmt.Printf("goacos %s (unique %s, commit %s, built %s)\nnacos api compatibility: %s\n",
			buildinfo.Version, buildinfo.UniqueVersion(), buildinfo.GitCommit, buildinfo.BuildDate, buildinfo.NacosAPICompat)
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
		fmt.Fprintf(os.Stderr, `goacos — lightweight Nacos-compatible server (Go + MySQL/PostgreSQL)

Usage:
  goacos serve                 run the server (default)
  goacos db discover|verify|init   database discovery & initialization
  goacos healthcheck [addr]    container health probe
  goacos version               print version

Server configuration is environment-driven (GOACOS_* or NACOS_* prefix):
  GOACOS_DB_TYPE=mysql|postgres           database engine (default mysql)
  GOACOS_DB_DSN=postgres://...            full DSN override (optional)
  GOACOS_PORT=8848                        HTTP listen port (Nacos API)
  GOACOS_MYSQL_HOST=127.0.0.1             database host (database auto-created)
  GOACOS_MYSQL_PORT=3306                  database port (mysql 3306 / pg 5432)
  GOACOS_MYSQL_DB=goacos                  database name
  GOACOS_MYSQL_USER=root                  database user
  GOACOS_MYSQL_PASSWORD=                  database password
  GOACOS_AUTH_ENABLED=false               require access tokens
  GOACOS_AUTH_TOKEN_SECRET=...            JWT signing secret (set in HA setups)
  GOACOS_ADMIN_USERNAME=nacos             seeded admin username
  GOACOS_ADMIN_PASSWORD=nacos             seeded admin password

Gateway data plane (off by default; declared domain+path routes are served
by registered healthy instances, with TLS terminated here):
  GOACOS_GATEWAY_ENABLED=false            turn on the gateway listeners
  GOACOS_GATEWAY_NAMESPACE=public         namespace whose routes to serve
  GOACOS_GATEWAY_HTTP_ADDR=:80            ACME challenges + HTTP->HTTPS redirect
  GOACOS_GATEWAY_HTTPS_ADDR=:443          real traffic
  GOACOS_GATEWAY_FORCE_HTTPS=true         redirect plain HTTP instead of proxying
  GOACOS_GATEWAY_HSTS_MAX_AGE=0           per-domain default (0 = off)
  GOACOS_GATEWAY_MAX_BODY_BYTES=0         request body cap (0 = unlimited)
  GOACOS_GATEWAY_CONNECT_TIMEOUT_MS=3000  upstream dial timeout
  GOACOS_GATEWAY_READ_TIMEOUT_MS=60000    upstream response timeout
  GOACOS_GATEWAY_WEBSOCKET=true           allow connection upgrades
  GOACOS_GATEWAY_TRUST_FORWARDED_FOR=false  honour X-Forwarded-For for IP rules
  GOACOS_GATEWAY_STATIC_ROOT=             document root for protocol=static
  GOACOS_GATEWAY_REFRESH_SECONDS=10       routing/certificate refresh interval

Certificates (a declared domain always answers on 443; see docs/GATEWAY.md):
  GOACOS_GATEWAY_ACME_ENABLED=true        HTTP-01 auto-issue from the ACME CA
  GOACOS_GATEWAY_ACME_EMAIL=              contact address for the ACME account
  GOACOS_GATEWAY_ACME_DIRECTORY_URL=...   point at staging to test issuance
  GOACOS_GATEWAY_ACME_DATA_DIR=./data/acme     ACME account key (back this up)
  GOACOS_GATEWAY_SELFSIGNED_DIR=./data/selftest self-signed fallback cache
  GOACOS_GATEWAY_CERT_DIR=               shared certificate directory to load
  GOACOS_GATEWAY_CERT_WRITE_BACK=true     mirror issued certificates into it
  GOACOS_GATEWAY_CERT_SCAN_SECONDS=30     directory rescan interval

Declare applications, components and domain routes under /goacos/v1/, and
inspect the running gateway at GET /goacos/v1/gateway/status.
`)
		os.Exit(2)
	}
}
