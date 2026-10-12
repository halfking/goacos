// SPDX-License-Identifier: Apache-2.0
// Package registry registers a service instance in a goacos registry and
// discovers the registry when nobody told us where it is.
//
// # Why this package exists
//
// A distributed control plane — agents, workers and gateways that look each
// other up at request time rather than through a static list in a config file —
// requires two things, and this package provides both:
//
//  1. An instance lifecycle — register, heartbeat, deregister — so peers can
//     find this process.
//  2. A way to find the registry itself, because the address differs between a
//     laptop, the docker-compose network and a staged deployment, and a hard
//     default that is wrong in one of them is a silent outage.
//
// The server is goacos (github.com/halfking/goacos), a Nacos-API-compatible
// registry. "Nacos-API-compatible" is doing real work in that sentence: the
// wire format is Nacos v2, so an operator who already knows Nacos knows this
// package, and swapping in a stock Nacos server changes the address and
// nothing else.
//
// # Why v2 endpoints exclusively
//
// Every instance operation here uses the v2 routes under /nacos/v2/ns. That
// is not a style preference. The v1 beat endpoint accepts only GET and PUT —
// not POST — and its optional per-beat override is parsed from the REQUEST
// BODY rather than from the query string. A conventional client that sends
// ?beat=... is therefore silently ignored, which is the worst possible failure
// mode for a heartbeat: it looks configured and is not. v2 is POST everywhere
// with query parameters that mean what they say, so one code path covers
// register, beat and deregister. v1 is used for exactly one thing — the login
// endpoint — because that is the only login route the server exposes, and it
// has none of the beat quirks.
//
// # Why the advertised heartbeat interval is authoritative
//
// The server evicts an ephemeral instance 15 seconds after a missed beat and
// removes it at 30 seconds, and its config allows an operator to tighten those
// windows. A client that hardcodes its own 5-second timer is therefore making
// a guess about someone else's policy, and a guess that fails silently when
// wrong. So Beat reads clientBeatInterval from the response and adopts it;
// Config.HeartbeatInterval is only the fallback used before the first
// successful beat. The server's clock and eviction rules outrank our
// configuration file.
//
// # Why deregistration is best-effort
//
// Deregistering an instance that does not exist returns "ok", so it is safe
// on every shutdown path. But it is not what makes the system correct. The
// server already removes an ephemeral instance that stops beating, which means
// a process killed with SIGKILL disappears on its own. Deregistration only
// shortens the 15-second window in which peers may still route to a dead
// process. Code that treats a failed deregistration as a shutdown failure is
// reading the wrong invariant.
//
// # Why everything is nil-safe
//
// The caller must be able to boot when the registry is unavailable. An
// unconfigured registry is represented by a nil or disabled *Client. Every
// method tolerates a nil receiver and returns ErrNilClient or ErrDisabled
// rather than panicking, so a wiring mistake in one optional subsystem cannot
// take the process down at boot. Registration failures are likewise not fatal:
// they are retried with exponential backoff and logged, because "the process is
// up but invisible" is a failure mode worth knowing about from the logs rather
// than from an outage.
//
// # Why the advertised address is never guessed silently
//
// Register returns an error when Config.IP is unset rather than picking
// whatever net.Interfaces() happens to report. An auto-detected address that
// turns out to be wrong is close to undebuggable — it produces connection
// refusals or hangs on a remote host, with nothing pointing back at this
// decision. AdvertiseIP() exposes the fallback chain (explicit config, then
// the first non-loopback global-unicast IPv4 on an interface that is up, then
// 127.0.0.1) so a caller can opt in and log the result.
//
// Loopback as the last resort is legitimate rather than defensive: in a
// single-host docker-compose, or on a laptop, the only address that exists is
// 127.0.0.1 and it is also the correct one. It is a misconfiguration the
// moment there is a second host, because every peer then resolves 127.0.0.1 to
// itself. That asymmetry is why the guess is opt-in.
//
// # Why discovery fails fast
//
// The well-known candidates include Docker-network DNS names that frequently do
// not exist. A client that waits out the full timeout on each of them turns a
// three-line config into a multi-second boot, so every layer has its own
// deadline: net.Dialer.Timeout covers name resolution as well as the TCP
// handshake, which is what makes an unresolvable name cost milliseconds
// instead of the whole budget. When nothing answers, Discover returns an error
// listing every candidate, its source, and why it failed — that message is
// what an operator reads, so it is written for them rather than for the
// profiler.
//
// # Scope and trade-offs
//
// This package speaks the registry wire protocol and nothing else. It does not
// read the environment, does not cache discovered peers, does not load balance
// across instances, and does not depend on anything outside the standard
// library. Discovery returns one address; the caller decides whether to reuse
// it, re-run it, or fall back to a static one. Those decisions belong in the
// config layer, which owns the environment and the precedence rules.
//
// Auth is off on a default goacos deployment, so the common path costs
// nothing. When a username and password are configured, the client logs in
// once and sends the token as "Authorization: Bearer" — the server checks the
// query parameter, then a cookie, then the header, and the header is last
// because it is the one that does not leak the token into access logs and
// proxy configuration.
package registry
