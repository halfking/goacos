package main

import (
	"flag"
	"os"
	"strconv"
	"time"

	"github.com/halfking/goacos/internal/envbool"
)

// newFlagSet wires the flags and the environment fallbacks.
//
// Environment first, flag second, so a deployment can set the registry once in
// the compose file and override only what differs per service. Credentials are
// environment-only on purpose: they must never appear in a process listing or
// in a unit file.
func newFlagSet(o *options) *flag.FlagSet {
	fs := flag.NewFlagSet("goacos-registrar", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	fs.StringVar(&o.server, "server", os.Getenv("GOACOS_SERVER_ADDR"),
		"goacos base URL, e.g. http://goacos:8848 (env GOACOS_SERVER_ADDR)")
	fs.StringVar(&o.service, "service", os.Getenv("GOACOS_SERVICE_NAME"),
		"service name other services look up; required")
	fs.IntVar(&o.port, "port", envInt("GOACOS_PORT", 0),
		"port of the endpoint being advertised, i.e. the one nginx already listens on; required")
	fs.StringVar(&o.advertiseIP, "advertise-ip", os.Getenv("GOACOS_ADVERTISE_IP"),
		"address to publish; empty means auto-detect from the host's interfaces")
	fs.StringVar(&o.namespace, "namespace", os.Getenv("GOACOS_NAMESPACE"),
		"registry namespace; empty means the public one")
	fs.StringVar(&o.group, "group", os.Getenv("GOACOS_GROUP"), "service group")
	fs.StringVar(&o.cluster, "cluster", os.Getenv("GOACOS_CLUSTER"), "service cluster")
	fs.Float64Var(&o.weight, "weight", 1, "load-balancing weight")
	fs.StringVar(&o.role, "role", os.Getenv("GOACOS_ROLE"),
		"descriptive instance metadata, e.g. frontend — free-form; the registry does NOT "+
			"decide frontend-vs-API from it (that comes from the component's protocol; "+
			"see docs/GATEWAY.md §3.6)")
	fs.Var(&o.meta, "meta", "extra instance metadata as key=value; repeatable")

	fs.StringVar(&o.username, "username", os.Getenv("GOACOS_USERNAME"),
		"registry username (env only — never pass a password on the command line)")
	fs.StringVar(&o.password, "password", os.Getenv("GOACOS_PASSWORD"),
		"registry password (env only)")

	fs.BoolVar(&o.allowLoopback, "allow-loopback", envbool.MustParse(os.Getenv, allowLoopbackEnv, false),
		"permit advertising 127.0.0.0/8 or 0.0.0.0; single-host deployments only"+
			" (env GOACOS_ALLOW_LOOPBACK: 1/true/yes/on)")
	fs.BoolVar(&o.verifyDial, "verify-dial", true,
		"wait for the advertised address to accept a connection before registering")
	fs.DurationVar(&o.dialTimeout, "dial-timeout", 30*time.Second,
		"how long -verify-dial waits for the endpoint to come up")

	fs.Usage = func() {
		fs.SetOutput(os.Stderr)
		out := fs.Output()
		_, _ = out.Write([]byte(`goacos-registrar registers an endpoint this process does not serve.

It opens no listener: -port must name a port something else (nginx, a sidecar,
another container) is already serving. That is the whole point — it lets static
frontends join the same load-balanced pool as Go services without being able to
import the SDK themselves.

Example:
  goacos-registrar -server http://goacos:8848 -service redclaw-console \
                   -port 27094 -role frontend

Flags:
`))
		fs.PrintDefaults()
	}
	return fs
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
