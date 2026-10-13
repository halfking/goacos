package registry

// Environment-driven registration for Go services.
//
// # Why this lives in the SDK and not in each service
//
// Every Go service that joins the registry has to make the same five decisions,
// and every one of them has a failure mode that looks like success:
//
//  1. registration must never be able to stop the service from serving;
//  2. the advertised port must be the business one, never inferred from
//     whatever listener happens to exist;
//  3. the advertised address must be one peers can actually dial — a loopback
//     result is accepted by the registry and unreachable from everywhere else;
//  4. registration happens *after* the service is listening, and the boot log
//     must reflect a registration that really happened;
//  5. deregistration on shutdown must use a **fresh** context, because the one
//     the service was running under is already cancelled by then.
//
// Written out per service, those five decisions get re-derived once per repo
// and drift. The drift is invisible: each service's own tests stay green, the
// instance is healthy in the registry, and the only symptom is that the data
// plane routes to an address nothing answers — or that a cleanly stopped
// service keeps receiving traffic until the registry evicts it.
//
// # Usage
//
//	cfg, err := registry.ConfigFromEnv(os.Getenv, registry.Service{
//	    Name:       "acc-go",
//	    ListenAddr: srv.Addr,
//	})
//	if err != nil {
//	    // Only operator typos land here. Decide deliberately whether a bad env
//	    // should be fatal for this service; it is never fatal by accident.
//	}
//
//	// … after the listener is up, never before …
//	stop := registry.Announce(ctx, cfg, srv.Addr, logger)
//	defer stop()
//
// # Registration is opt-in
//
// GOACOS_ENABLED gates everything and defaults to off. A service that has not
// been asked to register does not register, does not warn on every boot, and
// does not probe for a registry address that may not resolve where it runs.

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Environment variable names. One spelling for the whole fleet: these already
// exist in acc-go, the myvpn registrar sidecar and the goacos-registrar command
// under the same names. They drifted apart once, and that only happened because
// each side made the tradeoff privately.
const (
	EnvEnabled       = "GOACOS_ENABLED"
	EnvServerAddr    = "GOACOS_SERVER_ADDR"
	EnvNamespace     = "GOACOS_NAMESPACE"
	EnvServiceName   = "GOACOS_SERVICE_NAME"
	EnvIP            = "GOACOS_IP"
	EnvPort          = "GOACOS_PORT"
	EnvUsername      = "GOACOS_USERNAME"
	EnvPassword      = "GOACOS_PASSWORD"
	EnvAllowLoopback = "GOACOS_ALLOW_LOOPBACK"
)

// Service describes the calling service for ConfigFromEnv.
type Service struct {
	// Name is the registry name to use when GOACOS_SERVICE_NAME is unset.
	// It is the service's own name, not a host name — the registry keys
	// instances by service, and hosts come and go behind it.
	Name string

	// ListenAddr is the address the service serves on, e.g. ":8080". It is the
	// only source of the advertised port when GOACOS_PORT is unset, so a
	// service that does not pass it cannot register unless it sets the port.
	ListenAddr string

	// AllowLoopback opts this service in to advertising a **detected**
	// loopback or unspecified address. It is additive to GOACOS_ALLOW_LOOPBACK:
	// a codebase that genuinely is single-host can enable it without every
	// deployment having to set the variable.
	AllowLoopback bool

	// Metadata is optional descriptive key/value pairs. It is deliberately
	// minimal: anything the data plane needs in order to route belongs in the
	// gateway's route declaration, not in a string a service attaches to
	// itself.
	Metadata map[string]string
}

// ConfigFromEnv builds a Config from the environment.
//
// getenv is injected rather than calling os.Getenv directly so tests can
// exercise a specific environment without mutating the process's own — the
// "test that cannot go red measures nothing" trap in its most literal form.
//
// The address policy is resolved here rather than in Announce, because this is
// the only place that knows whether an address was **chosen** (GOACOS_IP) or
// **detected**. A chosen address is used as given; a detected one is filtered,
// because nobody chose it and the loopback fallback the SDK resolves to would
// otherwise be broadcast to peers that read it as themselves.
//
// The returned error covers only what the operator typed — an unparseable port,
// a non-boolean flag. Everything else (no address, no port, an unreachable
// registry) is deliberately not an error here, because the correct response to
// all of them is the same: serve traffic anyway, and say why.
func ConfigFromEnv(getenv func(string) string, svc Service) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}

	enabled, err := envBool(getenv, EnvEnabled, false)
	if err != nil {
		return Config{}, err
	}
	allowLoopback, err := envBool(getenv, EnvAllowLoopback, svc.AllowLoopback)
	if err != nil {
		return Config{}, err
	}

	port := 0
	if raw := strings.TrimSpace(getenv(EnvPort)); raw != "" {
		port, err = strconv.Atoi(raw)
		if err != nil {
			return Config{}, fmt.Errorf("%s=%q is not a port number: %w", EnvPort, raw, err)
		}
	}

	name := strings.TrimSpace(getenv(EnvServiceName))
	if name == "" {
		name = strings.TrimSpace(svc.Name)
	}

	cfg := Config{
		Enabled:     enabled,
		ServerAddr:  strings.TrimSpace(getenv(EnvServerAddr)),
		NamespaceID: strings.TrimSpace(getenv(EnvNamespace)),
		ServiceName: name,
		IP:          strings.TrimSpace(getenv(EnvIP)),
		Port:        port,
		Username:    getenv(EnvUsername),
		Password:    getenv(EnvPassword),
		Metadata:    svc.Metadata,
	}

	// Auto-detect only when the operator named no address, and never when
	// registration is off: an address nobody will publish should not be probed
	// for on every boot of a service that never registers.
	if enabled && cfg.IP == "" {
		if detected := detectedAdvertiseIP(getenv); isRoutable(detected) {
			cfg.IP = detected
		} else if allowLoopback && isDialableLoopback(detected) {
			cfg.IP = detected
		}
		// Otherwise leave it empty. Announce reports that, loudly, with a hint.
	}

	return cfg, nil
}

// Announce registers cfg with the registry and returns the function that stops
// it. listenAddr is the address the service serves on and is used only when
// cfg.Port is unset.
//
// Announce never returns an error, by design. A service that cannot reach its
// registry must still serve traffic, and every reason for not registering is
// logged where it happens.
//
// The returned function is always non-nil and safe to call repeatedly and from
// a defer, so main() never has to check it — including in every case where
// registration was skipped.
//
// Call it **after** the listener is up. Registering earlier publishes an
// instance that the load balancer may dial before anything is serving there.
func Announce(ctx context.Context, cfg Config, listenAddr string, logger *slog.Logger) func() {
	noop := func() {}
	if logger == nil {
		logger = slog.Default()
	}

	if !cfg.Enabled {
		// Debug, not Warn. Off is the documented default, and warning about it
		// on every boot is how a warning stops being read.
		logger.Debug("goacos registration disabled", "env", EnvEnabled+" is not set")
		return noop
	}

	name := strings.TrimSpace(cfg.ServiceName)
	if name == "" {
		logger.Warn("goacos registration skipped: no service name", "hint", "set "+EnvServiceName)
		return noop
	}

	ip := strings.TrimSpace(cfg.IP)
	if ip == "" {
		logger.Warn("goacos registration skipped: no address peers can dial",
			"service", name,
			"hint", "set "+EnvIP+"; loopback is not an option because every peer reads it as itself")
		return noop
	}
	if net.ParseIP(ip) == nil {
		logger.Warn("goacos registration skipped: configured address is not an IP",
			"service", name, "ip", ip, "env", EnvIP)
		return noop
	}

	port := advertisedPortForTest(cfg.Port, listenAddr)
	if port == 0 {
		logger.Warn("goacos registration skipped: cannot determine the advertised port",
			"service", name, "listen_addr", listenAddr,
			"hint", "set "+EnvPort+" or pass the address the service listens on; "+
				"broadcasting a wrong port publishes an endpoint that 502s the gateway")
		return noop
	}

	cfg.IP = ip
	cfg.Port = port
	client := NewClient(cfg, logger)

	// Register once, synchronously, so the log line below reflects something
	// that actually happened. Start() would do it in the background with
	// backoff, which lets the boot log claim success for a registration that
	// had not happened yet — the same "logs say one thing, the registry says
	// another" split that is far harder to debug than a plain error.
	regCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err := client.Register(regCtx)
	cancel()

	var once sync.Once
	stop := func() {
		once.Do(func() {
			// A fresh context on purpose. ctx is already cancelled by the time a
			// shutdown handler runs, and deregistering under a cancelled context
			// fails — turning a clean shutdown into a ghost instance.
			//
			// Do not add a manual deregister before this call: Stop stops the
			// heartbeat thread first and only then deregisters. The reverse
			// order is a documented race in which a live heartbeat sees the
			// instance gone and registers it again.
			stopCtx, cancelStop := context.WithTimeout(context.Background(), 5*time.Second)
			stopErr := client.Stop(stopCtx)
			cancelStop()
			if stopErr != nil {
				// Not fatal — the registry evicts ephemeral instances that stop
				// beating anyway. But it has to be said out loud, because the
				// consequence (a routable-looking instance for a while longer)
				// is otherwise invisible.
				logger.Warn("goacos deregister failed; the instance will linger until "+
					"the registry evicts it", "service", name, "error", stopErr)
				return
			}
			logger.Info("deregistered from goacos", "service", name)
		})
	}

	if err != nil {
		logger.Warn("goacos registration failed; serving traffic anyway",
			"service", name, "ip", ip, "port", port, "error", err)
		return stop
	}

	logger.Info("registered with goacos", "service", name, "ip", ip, "port", port)

	// Only now start the heartbeat. Starting it after a failed register would
	// leave a client retrying forever against a registry that may never come
	// up, logging on every tick.
	//
	// WithoutCancel: a cancelled ctx should stop the *service*, not silently
	// freeze its registration while the process is still serving.
	client.Start(context.WithoutCancel(ctx))
	return stop
}

// advertisedPort returns the port peers should dial, or 0 when none can be
// determined with confidence.
//
//	Precedence: an explicit port > the port in listenAddr > 0 (give up)
//
// There is deliberately no fallback to a metrics listener. A service that also
// exposes Prometheus on :9090 has two plausible ports and no way to tell them
// apart from here; publishing 9090 puts a working but wrong endpoint in the
// registry, and the service then looks registered and reachable while requests
// for the real API 502. Refusing to publish is the only honest answer.
func advertisedPort(explicit int, listenAddr string) int {
	if explicit > 0 && explicit <= 65535 {
		return explicit
	}
	addr := strings.TrimSpace(listenAddr)
	if addr == "" {
		return 0
	}
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		// A bare "8080" (no colon) fails SplitHostPort; treat it as the port.
		portStr = addr
	}
	port, err := strconv.Atoi(strings.TrimSpace(portStr))
	if err != nil || port <= 0 || port > 65535 {
		return 0
	}
	return port
}

// isDialableLoopback reports whether an address is one the single-host opt-in
// is willing to publish: 127.0.0.0/8 or ::1, and nothing else.
//
// Two things are being kept out, and they are not the same thing:
//
//   - **Garbage.** "This address is fine once you say you are alone" is a real
//     claim about 127.0.0.1. "This string is not an IP address at all" is not —
//     no claim of being the only host makes garbage dialable.
//   - **The unspecified address (0.0.0.0, ::).** This one is easy to get
//     wrong by lumping it in with loopback, because both mean "everything" and
//     both look local-only. But 0.0.0.0 is a *bind wildcard*, not a
//     destination: a peer that dials it reaches nothing, on a single host just
//     as much as on ten. On one host the correct answer is 127.0.0.1, and the
//     opt-in is what lets that through.
//
// An address the operator wrote into GOACOS_IP bypasses this entirely — they
// asked for it by name, and this filter is about what we broadcast on their
// behalf.
func isDialableLoopback(ip string) bool {
	parsed := net.ParseIP(strings.TrimSpace(ip))
	return parsed != nil && parsed.IsLoopback()
}

// isRoutable filters a *detected* address down to one worth broadcasting.
//
// Rejecting rather than passing through: a loopback result means every peer
// reads 127.0.0.1 as itself, which is how an instance ends up healthy in the
// registry and unroutable everywhere else. The comparison is a real IP parse
// rather than a "127." prefix test, so the whole 127.0.0.0/8 block and the
// IPv6 ::1 are covered too — and so a garbage detection result is treated as
// "no answer" instead of being broadcast verbatim.
func isRoutable(ip string) bool {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return false
	}
	parsed := net.ParseIP(ip)
	return parsed != nil && !parsed.IsLoopback() && !parsed.IsUnspecified()
}

// Indirection seams.
//
// Each exists so that a specific rule has a test demonstrably able to fail. A
// test that cannot go red measures nothing, and production code always routes
// through all three.
var (
	detectedAdvertiseIP = func(getenv func(string) string) string {
		return ResolveIP(Config{})
	}
	advertisedPortForTest = advertisedPort
)

// envBool parses the spellings that appear in compose files, .env files and
// shell exports.
func envBool(getenv func(string) string, key string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(getenv(key))
	if raw == "" {
		return fallback, nil
	}
	switch strings.ToLower(raw) {
	case "1", "t", "true", "yes", "y", "on":
		return true, nil
	case "0", "f", "false", "no", "n", "off":
		return false, nil
	}
	return false, fmt.Errorf("%s=%q is not a boolean", key, raw)
}
