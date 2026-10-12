// Package config loads goacos server configuration from environment variables.
// Both GOACOS_* and NACOS_* prefixes are accepted; GOACOS_* wins.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// LetsEncryptDirectory is the production ACME directory endpoint. It is the
// default rather than a hardcoded value at the call site so that pointing at
// the staging endpoint is one environment variable, which is the only safe way
// to exercise issuance end to end without spending production rate limits.
const LetsEncryptDirectory = "https://acme-v02.api.letsencrypt.org/directory"

type Config struct {
	Host string
	Port int

	// DBType selects the SQL engine: "mysql" (default) or "postgres".
	// DBDSN optionally carries a full connection string; a postgres:// DSN
	// implies PostgreSQL regardless of DBType. Deployment files (compose /
	// deploy.sh / k8s env) force the choice through these two variables.
	DBType string
	DBDSN  string

	// MySQL* fields carry the generic connection parts for the selected
	// engine (the GOACOS_MYSQL_* names predate PostgreSQL support and are
	// kept for backward compatibility; bare MYSQL_* also accepted).
	MySQLHost     string
	MySQLPort     int
	MySQLDB       string
	MySQLUser     string
	MySQLPassword string

	AuthEnabled     bool
	TokenSecret     string
	TokenTTLSeconds int
	AdminUsername   string
	AdminPassword   string

	// Ephemeral instance lifecycle (milliseconds), mirrors Nacos defaults.
	HeartbeatIntervalMs    int64
	HeartbeatTimeoutMs     int64 // mark unhealthy after this without beat
	EphemeralDeleteAfterMs int64 // delete ephemeral instance after this without beat
	SweepInterval          time.Duration

	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration

	LogLevel string

	// ---- gateway (data plane) ----
	//
	// The gateway is the same binary serving a second, separate surface: the
	// Nacos-compatible API above stays on Port for stock clients, while these
	// listeners carry real inbound traffic for the declared domains. It is off
	// by default so adding goacos to an existing environment does not silently
	// start competing for :80/:443.
	GatewayEnabled bool
	// GatewayNamespace is which namespace's routes this instance serves. An
	// empty value means "public", matching the rest of the system.
	GatewayNamespace string
	// GatewayHTTPAddr and GatewayHTTPSAddr default to the standard web ports
	// because a gateway that cannot answer on 80 cannot complete an ACME
	// HTTP-01 challenge, which is the default certificate path.
	GatewayHTTPAddr  string
	GatewayHTTPSAddr string
	// GatewayForceHTTPS redirects plain HTTP instead of proxying it. True by
	// default: a domain that has a certificate and still answers on 80 with
	// the same content is the shape of a downgrade attack.
	GatewayForceHTTPS bool
	// GatewayHSTSMaxAge is emitted per-domain; this is the default for domains
	// with no explicit policy row.
	GatewayHSTSMaxAge int
	// GatewayClientMaxBodyBytes caps request bodies. 0 means unlimited, which
	// is the pre-gateway default and is only safe behind an upstream that
	// enforces its own limit.
	GatewayClientMaxBodyBytes int64
	// Timeouts applied to every upstream unless a domain overrides them.
	GatewayConnectTimeoutMS int
	GatewayReadTimeoutMS    int
	GatewaySendTimeoutMS    int
	// GatewayWebsocket allows connection upgrades. On by default because
	// disabling it silently breaks chat/streaming services that otherwise look
	// healthy in every other check.
	GatewayWebsocket bool
	// GatewayTrustForwardedFor honours X-Forwarded-For for client-IP decisions.
	// Off by default: trusting it while the gateway is directly exposed lets a
	// client forge its own source address and defeat the IP rules.
	GatewayTrustForwardedFor bool
	// GatewayStaticRoot serves local files for components declared with
	// protocol=static. Empty disables local static serving entirely.
	GatewayStaticRoot string
	// GatewayStaticFallback serves index.html for unmatched paths under a
	// static root, which is what a single-page application needs to deep-link
	// its own routes.
	GatewayStaticFallback bool
	// GatewayCertRefreshSeconds is how often cached routes and certificates are
	// re-read from the database. This is the upper bound on "a new instance
	// registered now starts receiving traffic within".
	GatewayCertRefreshSeconds int
	// GatewayCertDir is a shared directory of certificates, mounted into the
	// container, whose contents are loaded into the same in-memory cache the
	// database feeds. It exists so that certificates produced by an external
	// tool (certbot, cert-manager, an internal PKI agent) can be served without
	// being copied into the database by hand.
	//
	// It is read on a timer, never on the handshake path.
	GatewayCertDir string
	// GatewayCertWriteBack mirrors certificates this gateway issues into that
	// directory. Defaults to true, which means a directory mount must be
	// writable by the container user — see the Dockerfile note. Set it to
	// false for a read-only mount when only the external side writes.
	GatewayCertWriteBack bool
	// GatewayCertScanSeconds is the directory rescan interval. It is separate
	// from the routing refresh because scanning costs disk I/O and the answer
	// does not need to be fresher than the certificate's own renewal schedule.
	GatewayCertScanSeconds int

	// ---- ACME (automatic certificate issuance) ----
	//
	// Enabled by default: a gateway that cannot obtain a certificate for a
	// public domain is the thing that forces operators back to hand-managed
	// nginx configs in the first place. Issuance only ever runs for declared
	// domains with no certificate, and never for names a public CA cannot
	// serve — see gateway/acme.go for the eligibility precheck, which exists to
	// avoid burning Let's Encrypt's failure rate limit on internal hostnames.
	ACMEEnabled bool
	ACMEEmail   string
	// ACMEDirectoryURL is the ACME directory endpoint. The default is
	// Let's Encrypt production; point it at a staging URL to test the whole
	// issuance path without consuming production rate limits.
	ACMEDirectoryURL string
	// ACMEDataDir holds the ACME account key. It is deliberately NOT in the
	// database: losing it loses the account and every rate limit attached to
	// it, so it is treated like any other secret file.
	ACMEDataDir string
	ACMETimeout int

	// SelfSignedDir caches generated self-signed certificates. It is a
	// directory rather than a database column because these are throwaway
	// material: a cache that can be deleted and regenerated must never be the
	// place a real certificate is trusted from.
	SelfSignedDir string
}

func getenv(name, def string) string {
	if v := os.Getenv("GOACOS_" + name); v != "" {
		return v
	}
	if v := os.Getenv("NACOS_" + name); v != "" {
		return v
	}
	// bare MYSQL_* names are the container-ecosystem convention and what
	// deploy/deploy.sh and docker-compose pass to the image
	if strings.HasPrefix(name, "MYSQL_") {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	return def
}

func getenvInt(name string, def int) int {
	if v := getenv(name, ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func getenvInt64(name string, def int64) int64 {
	if v := getenv(name, ""); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func getenvBool(name string, def bool) bool {
	defStr := "false"
	if def {
		defStr = "true"
	}
	b, err := strconv.ParseBool(getenv(name, defStr))
	if err != nil {
		return def
	}
	return b
}

// FromEnv builds a Config from the process environment with sane defaults.
func FromEnv() *Config {
	return &Config{
		Host:                   getenv("HOST", "0.0.0.0"),
		Port:                   getenvInt("PORT", 8848),
		DBType:                 getenv("DB_TYPE", "mysql"),
		DBDSN:                  getenv("DB_DSN", ""),
		MySQLHost:              getenv("MYSQL_HOST", "127.0.0.1"),
		MySQLPort:              getenvInt("MYSQL_PORT", 3306),
		MySQLDB:                getenv("MYSQL_DB", "goacos"),
		MySQLUser:              getenv("MYSQL_USER", "root"),
		MySQLPassword:          getenv("MYSQL_PASSWORD", ""),
		AuthEnabled:            getenvBool("AUTH_ENABLED", false),
		TokenSecret:            getenv("AUTH_TOKEN_SECRET", "goacos-default-secret-SecretKey012345678901234567890123456789012345678901234567890123456789"),
		TokenTTLSeconds:        getenvInt("AUTH_TOKEN_TTL_SECONDS", 18000),
		AdminUsername:          getenv("ADMIN_USERNAME", "nacos"),
		AdminPassword:          getenv("ADMIN_PASSWORD", "nacos"),
		HeartbeatIntervalMs:    getenvInt64("HEARTBEAT_INTERVAL_MS", 5000),
		HeartbeatTimeoutMs:     getenvInt64("HEARTBEAT_TIMEOUT_MS", 15000),
		EphemeralDeleteAfterMs: getenvInt64("EPHEMERAL_DELETE_AFTER_MS", 30000),
		SweepInterval:          time.Duration(getenvInt64("SWEEP_INTERVAL_MS", 5000)) * time.Millisecond,
		MaxOpenConns:           getenvInt("MYSQL_MAX_OPEN_CONNS", 100),
		MaxIdleConns:           getenvInt("MYSQL_MAX_IDLE_CONNS", 20),
		ConnMaxLifetime:        time.Duration(getenvInt64("MYSQL_CONN_MAX_LIFETIME_SECONDS", 1800)) * time.Second,
		LogLevel:               getenv("LOG_LEVEL", "info"),

		// ---- gateway data plane ----
		GatewayEnabled:            getenvBool("GATEWAY_ENABLED", false),
		GatewayNamespace:          getenv("GATEWAY_NAMESPACE", "public"),
		GatewayHTTPAddr:           getenv("GATEWAY_HTTP_ADDR", ":80"),
		GatewayHTTPSAddr:          getenv("GATEWAY_HTTPS_ADDR", ":443"),
		GatewayForceHTTPS:         getenvBool("GATEWAY_FORCE_HTTPS", true),
		GatewayHSTSMaxAge:         getenvInt("GATEWAY_HSTS_MAX_AGE", 0),
		GatewayClientMaxBodyBytes: int64(getenvInt64("GATEWAY_MAX_BODY_BYTES", 0)),
		GatewayConnectTimeoutMS:   getenvInt("GATEWAY_CONNECT_TIMEOUT_MS", 3000),
		GatewayReadTimeoutMS:      getenvInt("GATEWAY_READ_TIMEOUT_MS", 60000),
		GatewaySendTimeoutMS:      getenvInt("GATEWAY_SEND_TIMEOUT_MS", 60000),
		GatewayWebsocket:          getenvBool("GATEWAY_WEBSOCKET", true),
		GatewayTrustForwardedFor:  getenvBool("GATEWAY_TRUST_FORWARDED_FOR", false),
		GatewayStaticRoot:         getenv("GATEWAY_STATIC_ROOT", ""),
		GatewayStaticFallback:     getenvBool("GATEWAY_STATIC_FALLBACK", true),
		GatewayCertRefreshSeconds: getenvInt("GATEWAY_REFRESH_SECONDS", 10),
		GatewayCertDir:            getenv("GATEWAY_CERT_DIR", ""),
		GatewayCertWriteBack:      getenvBool("GATEWAY_CERT_WRITE_BACK", true),
		GatewayCertScanSeconds:    getenvInt("GATEWAY_CERT_SCAN_SECONDS", 30),

		// ---- ACME (automatic certificate issuance) ----
		ACMEEnabled:      getenvBool("GATEWAY_ACME_ENABLED", true),
		ACMEEmail:        getenv("GATEWAY_ACME_EMAIL", ""),
		ACMEDirectoryURL: getenv("GATEWAY_ACME_DIRECTORY_URL", LetsEncryptDirectory),
		ACMEDataDir:      getenv("GATEWAY_ACME_DATA_DIR", "./data/acme"),
		ACMETimeout:      getenvInt("GATEWAY_ACME_TIMEOUT_SECONDS", 120),

		SelfSignedDir: getenv("GATEWAY_SELFSIGNED_DIR", "./data/selftest"),
	}
}

// Addr returns the HTTP listen address.
func (c *Config) Addr() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

// MySQLAddr returns "host:port" for the configured MySQL server.
func (c *Config) MySQLAddr() string {
	return fmt.Sprintf("%s:%d", c.MySQLHost, c.MySQLPort)
}
