// Package buildinfo carries version information injected at build time.
//
// Version scheme (2026-10-09, owner directive): every service build is
// identified by a unique version composed of semantic version + compile
// count: Version "+b" BuildNumber, e.g. "v1.1.1+b123". The compile count is
// the git commit count (monotonic, one value per commit), so every binary
// ever built is traceable to exactly one source state.
package buildinfo

import (
	"runtime"
	"time"
)

var (
	Version     = "dev"
	GitCommit   = "unknown"
	BuildDate   = "unknown"
	BuildNumber = "0" // compile count; injected via -ldflags, git rev-list --count HEAD
	GoVersion   = runtime.Version()
)

// UniqueVersion composes the canonical build identity: Version + "+b" + BuildNumber.
func UniqueVersion() string {
	return Version + "+b" + BuildNumber
}

// NacosAPICompat describes the upstream Nacos release whose HTTP Open API this
// server is compatible with.
const NacosAPICompat = "2.5.x / 3.2.x (HTTP Open API subset)"

// RuntimeInfo returns current runtime metrics.
func RuntimeInfo() map[string]any {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return map[string]any{
		"goVersion":      GoVersion,
		"numGoroutine":   runtime.NumGoroutine(),
		"numCPU":         runtime.NumCPU(),
		"allocMB":        m.Alloc / 1024 / 1024,
		"totalAllocMB":   m.TotalAlloc / 1024 / 1024,
		"sysMB":          m.Sys / 1024 / 1024,
		"numGC":          m.NumGC,
		"uptimeSeconds":  int64(time.Since(startTime).Seconds()),
	}
}

var startTime = time.Now()
