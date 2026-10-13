// Command goacos-registrar registers an endpoint that has no Go process to
// embed the SDK in.
//
// It exists for the services that [方案 39 §5](myvpn/docs/方案/39-全平台服务注册与双栈调度总体方案-2026-10-12.md)
// assigns to "注册器（静态站）": redclaw-console (nginx), the openpocket frontend
// (nginx), official-portal (nginx) and smm-web. None of them is a Go binary we
// can add an import to, but each one already listens on a port that other
// services dial. This command registers *that address* and then keeps it alive,
// so the frontend joins the same load-balanced pool as everything else instead
// of being a name pinned in someone's nginx file.
//
// # It does not listen
//
// This is the single most important thing about the command, and the reason
// several design choices below look conservative. The registrar opens no
// listener of its own: the port it publishes belongs to nginx or whatever is
// already serving. Consequently:
//
//   - -port is required and is never inferred from anything. There is no
//     "metrics port" or "the port I happen to bind" to fall back to, because
//     publishing a port nobody serves produces an instance that registers
//     healthy, appears in the registry, and 502s every request — the exact
//     failure mode the SDK's own Register guards against by refusing to guess.
//   - -verify-dial is on by default, so a frontend that is not up yet is
//     waited for rather than advertised as available.
//
// # Loopback
//
// The SDK's ResolveIP prefers a real interface address and returns "" when it
// cannot find one; it never falls back to 127.0.0.1 on its own (that fallback
// exists only in the explicit AdvertiseIP opt-in). Here that "" is a hard
// error: publishing 127.0.0.1 in a mesh means every peer resolves it to itself,
// so the instance looks healthy and every caller dials their own machine.
//
// A single-host deployment is a real case, so -allow-loopback exists, matching
// the same GOACOS_ALLOW_LOOPBACK switch the myvpn sidecar, ACC and RedClaw use.
// It only accepts addresses that are actually valid and only meaningful
// locally; being told "this deployment is single-host" must not turn a garbage
// string into a dialable endpoint.
//
// # Shutdown
//
// SIGTERM deregisters using a **fresh** context. signal.NotifyContext cancels
// the context that the beat loop is running under, and the SDK's Stop returns
// nil without deregistering when handed an already-cancelled context — which
// turns an orderly stop into a ghost instance that outlives the process.
//
// # Usage
//
//	goacos-registrar \
//	  -server http://goacos:8848 \
//	  -service redclaw-console \
//	  -port 27094 \
//	  -role frontend
//
// Environment fallbacks: GOACOS_SERVER_ADDR, GOACOS_NAMESPACE, GOACOS_GROUP,
// GOACOS_CLUSTER, GOACOS_USERNAME, GOACOS_PASSWORD, GOACOS_ALLOW_LOOPBACK.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/halfking/goacos/sdk/registry"
)

// allowLoopbackEnv is the same switch name and the same default (off) as the
// myvpn registrar sidecar, ACC and RedClaw use. One spelling across the fleet
// means the operator does not have to remember which subsystem they are in.
const allowLoopbackEnv = "GOACOS_ALLOW_LOOPBACK"

// stringList collects a repeatable flag (-meta k=v -meta k2=v2).
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

type options struct {
	server      string
	service     string
	port        int
	advertiseIP string
	namespace   string
	group       string
	cluster     string
	weight      float64
	role        string
	meta        stringList

	username string
	password string

	allowLoopback bool
	verifyDial    bool
	dialTimeout   time.Duration
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "goacos-registrar: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var opt options
	fs := newFlagSet(&opt)
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q; this command takes flags only", fs.Arg(0))
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := opt.registryConfig()
	if err != nil {
		return err
	}
	logger.Info("registering",
		"service", cfg.ServiceName,
		"namespace", cfg.NamespaceID,
		"group", cfg.GroupName,
		"cluster", cfg.ClusterName,
		"address", net.JoinHostPort(cfg.IP, strconv.Itoa(cfg.Port)),
		"server", cfg.ServerAddr,
	)

	if opt.verifyDial {
		if err := waitForDial(cfg.IP, cfg.Port, opt.dialTimeout); err != nil {
			// Refusing to register beats registering something that answers
			// 502: an instance that is in the registry but not serving is
			// indistinguishable, from the caller's side, from a broken gateway.
			return fmt.Errorf("refusing to register %s: %w", cfg.ServiceName, err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client := registry.NewClient(cfg, logger)

	// Register synchronously and loudly. Start() backgrounds the beat loop and
	// would leave a failed first registration looking like a healthy process.
	if err := client.Register(ctx); err != nil {
		return fmt.Errorf("register %s: %w", cfg.ServiceName, err)
	}
	logger.Info("registered", "service", cfg.ServiceName)

	client.Start(ctx)

	<-ctx.Done()
	stop() // restore default handling: a second signal should kill outright

	// Fresh context on purpose — see the package comment's "Shutdown".
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Stop(shutdownCtx); err != nil {
		// Not fatal enough to hide the fact that the instance is still there.
		logger.Error("deregister failed; the instance will linger until the registry evicts it",
			"service", cfg.ServiceName, "error", err)
		return err
	}
	logger.Info("deregistered", "service", cfg.ServiceName)
	return nil
}

// registryConfig turns flags into SDK configuration, or explains precisely why
// it cannot.
//
// Every failure here is at boot. The alternative — a process that starts
// happily and is only discovered to be misconfigured when traffic 502s — is the
// failure mode this whole command exists to avoid.
func (o *options) registryConfig() (registry.Config, error) {
	if o.service == "" {
		return registry.Config{}, errors.New("-service is required: it is the name other services look up")
	}
	if o.port <= 0 || o.port > 65535 {
		return registry.Config{}, fmt.Errorf("-port must be between 1 and 65535, got %d", o.port)
	}
	if o.server == "" {
		return registry.Config{}, errors.New("-server (or GOACOS_SERVER_ADDR) is required: there is no default registry")
	}

	ip := o.advertiseIP
	if ip == "" {
		ip = registry.ResolveIP(registry.Config{})
	}
	if ip == "" {
		return registry.Config{}, errors.New(
			"could not determine an address to advertise. Set -advertise-ip explicitly. " +
				"Refusing to fall back to 127.0.0.1: in a mesh every peer resolves it to itself, " +
				"so the instance would look healthy while every caller dialed its own machine")
	}
	if err := checkAddress(ip, o.allowLoopback); err != nil {
		return registry.Config{}, err
	}

	meta := map[string]string{}
	for _, kv := range o.meta {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return registry.Config{}, fmt.Errorf("-meta %q is not key=value", kv)
		}
		meta[k] = v
	}
	if o.role != "" {
		meta["role"] = o.role
	}

	return registry.Config{
		Enabled:     true,
		ServerAddr:  o.server,
		ServiceName: o.service,
		NamespaceID: o.namespace,
		GroupName:   o.group,
		ClusterName: o.cluster,
		IP:          ip,
		Port:        o.port,
		Weight:      o.weight,
		Metadata:    meta,
		Username:    o.username,
		Password:    o.password,
	}, nil
}

// checkAddress applies the loopback policy.
//
// The opt-in accepts only addresses that are legitimately local — loopback or
// unspecified — and only when they are actually well-formed. It does not
// accept arbitrary strings just because the operator said "single host": a
// typo'd address must still fail at boot.
func checkAddress(ip string, allowLoopback bool) error {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return fmt.Errorf("-advertise-ip %q is not an IP address: %w", ip, err)
	}
	if addr.IsLoopback() || addr.IsUnspecified() {
		if !allowLoopback {
			return fmt.Errorf(
				"refusing to advertise %s. In a mesh every peer resolves it to itself, so this "+
					"instance would look healthy while callers dial their own machine. "+
					"Set %s=true only if this really is a single-host deployment", ip, allowLoopbackEnv)
		}
		return nil
	}
	return nil
}

// waitForDial blocks until the published address accepts a connection.
//
// A frontend container that registers before nginx is up turns into a phantom:
// present in the registry, counted by the load balancer, answering 502.
func waitForDial(ip string, port int, timeout time.Duration) error {
	target := net.JoinHostPort(ip, strconv.Itoa(port))
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		conn, err := net.DialTimeout("tcp", target, 2*time.Second)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return fmt.Errorf("nothing is listening on %s after %s (last error: %v). "+
				"If the endpoint is intentionally not listening yet, pass -verify-dial=false",
				target, timeout, lastErr)
		}
		time.Sleep(time.Second)
	}
}
