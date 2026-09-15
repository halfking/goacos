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

type Config struct {
	Host string
	Port int

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
