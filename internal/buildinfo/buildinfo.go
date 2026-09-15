// Package buildinfo carries version information injected at build time.
package buildinfo

var (
	Version   = "dev"
	GitCommit = "unknown"
	BuildDate = "unknown"
)

// NacosAPICompat describes the upstream Nacos release whose HTTP Open API this
// server is compatible with.
const NacosAPICompat = "2.5.x / 3.2.x (HTTP Open API subset)"
