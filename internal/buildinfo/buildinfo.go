// Package buildinfo carries version information injected at build time.
package buildinfo

import (
	"runtime"
	"time"
)

var (
	Version   = "dev"
	GitCommit = "unknown"
	BuildDate = "unknown"
	GoVersion = runtime.Version()
)

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
