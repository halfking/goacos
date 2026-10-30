# goacos registry SDK

`github.com/halfking/goacos/sdk/registry` is the client SDK for **goacos**, the
Nacos-API-compatible service registry in this repository. It lets any service
register itself, keep its registration alive, and find the registry when nobody
told it where the registry is.

It is a pure client library: standard library only, no third-party
dependencies, and it never touches a config file or any global state. The
caller owns configuration and precedence — `ConfigFromEnv` reads the
environment through a getter the caller passes in.

> Migrated from `acc-go/internal/platform/nacos`. The behaviour is unchanged;
> the package was renamed from `nacos` to `registry` so the import path reads
> naturally (`registry.NewClient`) and so the package can be reused outside ACC.

## Why it exists

Distributed services need two things, and this SDK provides both:

1. **An instance lifecycle** — register, heartbeat, deregister — so peers can
   find this process.
2. **A way to find the registry itself** — the address differs between a
   laptop, a docker-compose network, and a staged deployment, and a hard-coded
   default that is wrong in one of them is a silent outage.

## Capability list

| Capability | API |
| --- | --- |
| Build a `Config` from the environment | `ConfigFromEnv(getenv, Service{...})` |
| Register, beat and deregister as one call | `Announce(ctx, cfg, listenAddr, logger)` |
| Register an ephemeral instance | `Client.Register(ctx)` |
| Send one heartbeat | `Client.Beat(ctx)` |
| Deregister (best-effort) | `Client.Deregister(ctx)` |
| List healthy instances of a service | `Client.ListInstances(ctx, name)` |
| List registered service names (paged) | `Client.ListServices(ctx, pageNo, pageSize)` |
| Probe registry health | `Client.Ping(ctx)` |
| Run the register + heartbeat loop in the background | `Client.Start(ctx)` |
| Stop the loop and deregister | `Client.Stop(ctx)` |
| Log in and cache a bearer token (auto-refreshed) | automatic; set `Config.Username`/`Password` |
| Find the registry by probing candidates | `Discover(ctx, Candidates(cfg), timeout)` |
| Compute the address to advertise | `ResolveIP(cfg)` / `Client.AdvertiseIP()` |
| Inspect the current client | `Enabled()`, `ServerAddr()` |

Everything is **nil-safe**: a nil or disabled `*Client` returns `ErrNilClient` or
`ErrDisabled` instead of panicking, so an unconfigured registry can never take
the host process down at boot.

## Cross-SDK differences worth knowing

The three bindings ship one contract (`sdk/conformance/CONTRACT.md`) but they do
**not** share every default. Two differences have bitten real integrations:

| | Go | Python | Node |
|---|---|---|---|
| `group` / `cluster` empty | normalised to `DEFAULT_GROUP` / `DEFAULT` | same | same |
| `cluster` / `group` whitespace-only | treated as unset | same | same |
| **enabled by default** | **`false`** | `true` | `true` |

The Go default is the safe one — a client that is not configured stays inert
rather than registering itself somewhere unexpected.

The trap: porting code from the Python or Node examples into Go and **omitting
`Enabled: true`** leaves you with a client that registers nothing. It is not
silent, though — `Register` returns

```
nacos: client is disabled or not configured (Enabled=false)
```

and `errors.Is(err, registry.ErrDisabled)` is true. The message names the exact
field to set.

```go
client := registry.NewClient(registry.Config{
    Enabled:     true, // ← required in Go; implied in Python/Node
    ServiceName: "llm-router",
    Port:        8080,
    IP:          "100.106.177.3",
    ServerAddr:  "http://goacos:8848",
}, logger)
```

## Typical use

The minimal register → heartbeat → deregister program. This compiles as-is:

```go
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/halfking/goacos/sdk/registry"
)

func main() {
	// Pass nil instead and the client discards its own logs rather than panicking.
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	// 1. Resolve the address to advertise. Register refuses to guess, so this
	//    is an explicit, logged opt-in. Pass a Config with IP set to skip it.
	ip := registry.ResolveIP(registry.Config{})
	if ip == "" {
		logger.Error("no routable address to advertise; set it explicitly")
		os.Exit(1)
	}

	cfg := registry.Config{
		Enabled:     true,
		ServerAddr:  "http://goacos:8848",
		ServiceName: "my-service",
		IP:          ip,
		Port:        8080,
		Metadata:    map[string]string{"version": "1.0.0"},
	}
	client := registry.NewClient(cfg, logger)

	// 2. Register once explicitly, so the outcome is known and logged
	//    truthfully. Start() would also do it, but asynchronously with backoff.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := client.Register(ctx); err != nil {
		// Not fatal: Start retries with exponential backoff.
		logger.Warn("registration failed; retrying in background", "error", err)
	}

	// 3. Hand over to the built-in register/beat loop.
	client.Start(ctx)

	// 4. Deregister on shutdown. Best-effort by design: the server already
	//    evicts ephemeral instances that stop beating.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := client.Stop(shutdownCtx); err != nil {
		logger.Warn("deregister failed; the registry will expire this instance anyway", "error", err)
	}
}
```

Without a `slog.Logger`, pass `nil` — the client then discards its own logs
rather than panicking.

## Configuring registry self-discovery

If `Config.ServerAddr` is empty, the client does not hard-code a default: call
`registry.Candidates` + `registry.Discover` and feed the result back into
`Config.ServerAddr`.

`Candidates` returns the ordered, de-duplicated probe list:

1. `Config.ServerAddr`, when set — an operator who told us where the registry
   is gets no second-guessing.
2. Well-known Docker-network names, in order: `goacos:8848`, `nacos:8848`,
   `host.docker.internal:8848`. (`nacos` is what a stock Nacos compose file
   calls the service.)
3. `127.0.0.1:8848` as a last resort.

Each entry carries a `Source` label (`config`, `env`, `wellknown:<name>`,
`loopback`) so a failure message says where each attempt came from. Discovery
returns the **first** address whose `/nacos/actuator/health` answers `200`, and
on total failure returns an error listing every candidate, its source, and why
it failed.

Discovery is deliberately fail-fast: each candidate gets its own short timeout
and `net.Dialer.Timeout` covers DNS resolution, so an unresolvable Docker name
costs milliseconds rather than the whole budget.

Because a registry is a separate process with an independent lifecycle, treat
"registry not up yet at boot" as transient: retry `Discover` in the background
until it succeeds, rather than reporting a permanent failure at startup.

## Design decisions

The full rationale for each decision below lives in [`doc.go`](./doc.go), which
is the authoritative source. This is only an index.

| Question | Answer | Where |
| --- | --- | --- |
| **Why does the client adopt the server's heartbeat interval?** | The server owns the eviction policy (15s to unhealthy, 30s to removal) and an operator may tighten those windows. A client that hardcodes its own 5s timer is guessing about someone else's policy, and the guess fails silently when wrong. `Beat` reads `clientBeatInterval` from the response and adopts it; `Config.HeartbeatInterval` is only the fallback before the first successful beat. | `doc.go` — "Why the advertised heartbeat interval is authoritative"; also on `Client.Beat` |
| Why v2 endpoints exclusively? | The v1 beat endpoint takes its override from the request **body**, not the query string, so a conventional client is silently ignored — the worst failure mode for a heartbeat. v1 is used only for login, the one route with no such quirk. | `doc.go` — "Why v2 endpoints exclusively" |
| Why is deregistration best-effort? | The server already evicts ephemeral instances that stop beating, so a `SIGKILL`ed process disappears anyway. Deregistration only shortens a 15s window; a failed deregister is not a shutdown failure. | `doc.go` — "Why deregistration is best-effort" |
| Why is everything nil-safe? | The host must boot when the registry is unavailable. Every method tolerates a nil receiver rather than panicking, and registration failures are retried with backoff and logged. | `doc.go` — "Why everything is nil-safe" |
| Why is the advertised address never guessed silently? | A wrong auto-detected address is near-undebuggable (connection refusals on a remote host). `Register` errors instead; `ResolveIP`/`AdvertiseIP` let the caller opt in and log the result. Loopback is a legitimate single-host answer, but a misconfiguration the moment there is a second host. | `doc.go` — "Why the advertised address is never guessed silently" |
| Why does discovery fail fast? | Three dead Docker DNS names must not turn a three-line config into a multi-second boot. Every layer carries its own deadline, and the aggregate failure message is written for an operator, not a profiler. | `doc.go` — "Why discovery fails fast" |
| Why is a token refreshed on the advertised TTL? | Caching indefinitely fails in the worst possible way: hours in, every register and beat returns 401, the instance is evicted, and because the stale token still looks valid it never comes back. | `Client.ensureToken` |
| Why prefer a mesh (CGNAT 100.64/10) address? | A registry stores the address peers will actually dial. On a host with both LAN and overlay interfaces, taking whichever comes first usually lands on the LAN address — which overlay peers cannot reach — while registration still *succeeds* and every log line looks healthy. | `selectAdvertiseIP` |

## Non-goals

This package speaks the registry wire protocol and nothing else. Apart from
`ConfigFromEnv` — which reads the `GOACOS_*` variables through a getter the
caller passes in — it does not read the environment, cache discovered peers,
load-balance across instances, or depend on anything outside the standard
library. Discovery returns one address; the caller decides whether to reuse
it, re-run it, or fall back to a static one.