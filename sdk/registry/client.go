// Package registry is the goacos / Nacos-API service-registry client. The
// package documentation lives in doc.go; this file holds the client itself.
//
// Everything here is deliberately boring: one typed Config in, a handful of
// nil-safe methods out, and no ambient state. The reasons are spelled out in
// doc.go and next to the individual decisions below.
package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Defaults that mirror the goacos server's own defaults. They are exported so
// a caller can render them in a config dump without re-typing the literals.
const (
	// DefaultGroupName is the group an instance lands in when none is set.
	DefaultGroupName = "DEFAULT_GROUP"
	// DefaultClusterName is the cluster an instance lands in when none is set.
	DefaultClusterName = "DEFAULT"
	// DefaultPort is the port a goacos server listens on.
	DefaultPort = 8848
	// DefaultNamespaceID is the PUBLIC namespace. Note this is the empty
	// string, not the literal "public" — see doc.go.
	DefaultNamespaceID = ""

	// DefaultHeartbeatInterval is only a fallback used until (or unless) the
	// server advertises clientBeatInterval. It is never the steady-state value
	// on a healthy connection.
	DefaultHeartbeatInterval = 5 * time.Second
	// DefaultRequestTimeout bounds a single registry HTTP call.
	DefaultRequestTimeout = 3 * time.Second

	// maxErrorBodyBytes caps how much of an error response we quote back.
	maxErrorBodyBytes = 512

	// defaultTokenTTL is the lifetime assumed when a registry authenticates a
	// login but advertises no tokenTtl. goacos itself defaults to 18000s; this
	// mirrors that so a silent TTL omission degrades to the registry's own
	// behaviour rather than to "cache forever".
	defaultTokenTTL = 5 * time.Hour
)

// Wire paths. Every path is relative to the "/nacos" context path and every
// one of them is a v2 endpoint — see the "why v2" note on the register method.
const (
	pathPrefix       = "/nacos"
	pathInstance     = "/v2/ns/instance"
	pathBeat         = "/v2/ns/instance/beat"
	pathInstanceList = "/v2/ns/instance/list"
	pathServiceList  = "/v2/ns/service/list"
	pathHealth       = "/actuator/health"
	pathLogin        = "/v1/auth/users/login"
)

// Lifecycle tuning constants.
//
// These are constants on purpose. They started as package variables so tests
// could shrink the retry ladder, but a test mutating shared state races with
// any lifecycle goroutine still winding down from an earlier test. They are now
// copied onto the Client at construction, where a test can adjust its own
// instance before Start without touching anyone else's.
const (
	// defaultRegisterBackoff / maxRegisterBackoffBound the exponential retry
	// of a failed registration. Thirty seconds is the ceiling because the
	// server evicts an ephemeral instance at 30s: retrying slower than that
	// would just mean the instance never comes back.
	defaultRegisterBackoff = time.Second
	maxRegisterBackoff     = 30 * time.Second
)

// Fixed lifecycle thresholds.
const (
	// beatFailureThreshold is how many consecutive beat failures trigger a
	// re-registration. One failed beat can be a dropped TCP segment; three in
	// a row means either the registry forgot us or the address moved.
	beatFailureThreshold = 3

	// minAdvertisedBeatInterval clamps absurdly small advertised intervals so
	// a misconfigured server cannot turn us into a busy loop.
	minAdvertisedBeatInterval = 50 * time.Millisecond

	// deregisterTimeout bounds the best-effort deregistration inside Stop.
	deregisterTimeout = 2 * time.Second
)

// Sentinel errors. Nil-receiver calls return ErrNilClient so that an operator
// can tell "you wired nothing up" apart from "the registry said no".
var (
	// ErrNilClient is returned by every method on a nil *Client.
	ErrNilClient = errors.New("nacos: client is nil")
	// ErrDisabled is returned when the client is disabled or under-configured.
	ErrDisabled = errors.New("nacos: client is disabled or not configured")
)

// Config describes one service instance and how to reach the registry.
//
// It is a plain struct on purpose: this package never reads the environment
// and never reaches for a global. The process' config layer fills it in, which
// keeps the registry address testable and keeps secrets (Password/Token) out
// of a package-level variable.
type Config struct {
	// Enabled turns the whole package off. A disabled client is inert: Start
	// does nothing and every network method returns ErrDisabled.
	Enabled bool

	// ServerAddr is the registry base URL, e.g. "http://goacos:8848". A
	// trailing slash is optional and a missing scheme is treated as http.
	ServerAddr string

	// ServiceName is the logical service name, e.g. "acc-go".
	ServiceName string

	// NamespaceID selects the namespace. "" is the public namespace.
	NamespaceID string

	// GroupName defaults to DefaultGroupName, ClusterName to
	// DefaultClusterName. Weight defaults to 1.0 when <= 0.
	GroupName   string
	ClusterName string

	// IP is the address advertised to other services. When empty it is NOT
	// guessed during registration — Register fails with a clear error and
	// AdvertiseIP is the opt-in fallback. See AdvertiseIP.
	IP string

	// Port is the port other services should dial. Required for Register.
	Port int

	// Weight is the load-balancing weight; <= 0 means the default 1.0.
	Weight float64

	// Metadata is free-form key/value data carried with the instance.
	Metadata map[string]string

	// Username/Password enable the login flow. Leave both empty for the
	// default deployment, which has auth disabled.
	Username string
	Password string

	// Token is a pre-obtained accessToken. When set, the login round trip is
	// skipped entirely.
	Token string

	// HeartbeatInterval is the fallback beat period, used only until the
	// server advertises clientBeatInterval of its own.
	HeartbeatInterval time.Duration

	// RequestTimeout bounds a single HTTP call to the registry.
	RequestTimeout time.Duration
}

// registerRequest is the v2 instance-registration body. The json tags are the
// wire contract and must not be "cleaned up".
type registerRequest struct {
	ServiceName string            `json:"serviceName"`
	IP          string            `json:"ip"`
	Port        int               `json:"port"`
	NamespaceID string            `json:"namespaceId"`
	GroupName   string            `json:"groupName"`
	ClusterName string            `json:"clusterName"`
	Weight      float64           `json:"weight"`
	Enabled     bool              `json:"enabled"`
	Ephemeral   bool              `json:"ephemeral"`
	Metadata    map[string]string `json:"metadata"`
}

// Instance is one registered service instance as returned by the v2 list API.
type Instance struct {
	InstanceID  string            `json:"instanceId"`
	IP          string            `json:"ip"`
	Port        int               `json:"port"`
	Weight      float64           `json:"weight"`
	Healthy     bool              `json:"healthy"`
	Enabled     bool              `json:"enabled"`
	Ephemeral   bool              `json:"ephemeral"`
	ClusterName string            `json:"clusterName"`
	ServiceName string            `json:"serviceName"`
	Metadata    map[string]string `json:"metadata"`
}

// instanceListData is the data object of GET /v2/ns/instance/list.
type instanceListData struct {
	Name  string     `json:"name"`
	Hosts []Instance `json:"hosts"`
}

// serviceListData is the data object of GET /v2/ns/service/list.
type serviceListData struct {
	Count      int      `json:"count"`
	PageNumber int      `json:"pageNumber"`
	PageSize   int      `json:"pageSize"`
	Services   []string `json:"services"`
}

// beatData is the data object of POST /v2/ns/instance/beat. The server tells
// us how fast it wants to be beaten; clientBeatInterval is in milliseconds.
type beatData struct {
	ClientBeatInterval int64 `json:"clientBeatInterval"`
	Code               int   `json:"code"`
	LightBeatEnabled   bool  `json:"lightBeatEnabled"`
	Created            bool  `json:"created"`
}

// envelope is the goacos response wrapper. Code 0 means success; the data
// field's shape depends on the endpoint, so it stays raw until decoded.
type envelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// healthData is the subset of /nacos/actuator/health we care about.
type healthData struct {
	Status string `json:"status"`
}

// loginResponse models POST /v1/auth/users/login. goacos returns the token BOTH
// flat at the top level and enveloped under data; we read flat first and fall
// back to the envelope, so we work against either shape.
type loginResponse struct {
	AccessToken string `json:"accessToken"`
	TokenTTL    int    `json:"tokenTtl"`
	Username    string `json:"username"`
	GlobalAdmin bool   `json:"globalAdmin"`
	Code        int    `json:"code"`
	Message     string `json:"message"`
	Data        *struct {
		AccessToken string `json:"accessToken"`
		TokenTTL    int    `json:"tokenTtl"`
	} `json:"data"`
}

// netInterface is the subset of net.Interface that AdvertiseIP needs. It is
// its own type so the interface table can be injected in a test without
// owning a NIC.
type netInterface struct {
	Name  string
	Flags net.Flags
	Addrs []net.Addr
}

// ifaces is the interface lister used by AdvertiseIP. Production code never
// assigns to it; tests swap it to describe a fake interface table.
var ifaces = defaultIfaces

// defaultIfaces snapshots the host's interfaces. Interfaces whose address list
// cannot be read are dropped rather than failing the whole call: one
// unreadable interface should not defeat address detection.
func defaultIfaces() ([]netInterface, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]netInterface, 0, len(ifaces))
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		out = append(out, netInterface{Name: iface.Name, Flags: iface.Flags, Addrs: addrs})
	}
	return out, nil
}

// Client is a goacos registry client.
//
// All methods tolerate a nil receiver. The caller must be able to boot when the
// registry is down or unconfigured, and a nil client is simply how "no
// registry" is represented — the same shape an unconfigured Redis client takes
// in the caller's own codebase.
type Client struct {
	cfg    Config
	logger *slog.Logger
	httpc  *http.Client
	base   string

	// mu guards the two pieces of mutable wire state: the cached auth token
	// (with its expiry) and the server-advertised heartbeat interval.
	mu           sync.Mutex
	token        string
	tokenExpiry  time.Time
	beatInterval time.Duration

	// lifecycleMu guards the Start/Stop pair.
	lifecycleMu sync.Mutex
	cancel      context.CancelFunc
	done        chan struct{}
	stopped     bool

	// Register retry ladder, copied from the package defaults at construction
	// so a caller (or a test) can retune one client without global state.
	registerBackoffInitial time.Duration
	registerBackoffMax     time.Duration
}

// NewClient builds a Client. It never returns nil and never fails: a bad or
// missing address surfaces as an error from the first call, not as a panic or
// a nil pointer at boot.
func NewClient(cfg Config, logger *slog.Logger) *Client {
	if logger == nil {
		// A package must not blow up on a nil logger; discard is the honest
		// default and keeps NewClient total.
		logger = slog.New(discardHandler{})
	}

	cfg.GroupName = orString(cfg.GroupName, DefaultGroupName)
	cfg.ClusterName = orString(cfg.ClusterName, DefaultClusterName)
	if cfg.Weight <= 0 {
		cfg.Weight = 1.0
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = DefaultRequestTimeout
	}

	c := &Client{
		cfg:    cfg,
		logger: logger,
		base:   normalizeBaseURL(cfg.ServerAddr),

		registerBackoffInitial: defaultRegisterBackoff,
		registerBackoffMax:     maxRegisterBackoff,
	}
	c.httpc = &http.Client{Timeout: cfg.RequestTimeout}

	if !cfg.Enabled {
		c.logger.Info("nacos client disabled")
		return c
	}
	c.logger.Info("nacos client configured",
		"server", c.base,
		"service", cfg.ServiceName,
		"namespace", cfg.NamespaceID,
		"group", cfg.GroupName,
		"cluster", cfg.ClusterName)
	return c
}

// Enabled reports whether the client was configured to talk to a registry.
func (c *Client) Enabled() bool {
	return c != nil && c.cfg.Enabled
}

// ServerAddr returns the normalized registry base URL, or "" when unset.
func (c *Client) ServerAddr() string {
	if c == nil {
		return ""
	}
	return c.base
}

// AdvertiseIP returns the address this client would advertise.
//
// The fallback chain is deliberate and ordered:
//
//  1. Config.IP, if set — always wins.
//  2. The first non-loopback global-unicast IPv4 on an interface that is up.
//     Interfaces that are down or have no IPv4 address are skipped, and
//     link-local (169.254/16) addresses are skipped because they are not
//     routable off-host.
//  3. 127.0.0.1.
//
// Step 3 is not a bug, it is a real deployment: a single-host docker-compose
// or a laptop where the only address is loopback genuinely is the address
// other containers should dial. It is a misconfiguration the moment the
// topology has more than one host, because every peer resolves 127.0.0.1 to
// itself and will happily dial its own port. That is why Register does NOT
// call this: it returns an error when Config.IP is empty so the operator
// finds out at boot instead of debugging phantom 502s. Callers who want the
// guess opt in explicitly.
func (c *Client) AdvertiseIP() string {
	if c == nil {
		return ""
	}
	if ip := selectAdvertiseIP(c.cfg.IP, ifaces); ip != "" {
		return ip
	}
	return "127.0.0.1"
}

// ResolveIP applies the same chain as AdvertiseIP but without a Client, so a
// caller can decide to advertise an auto-detected address BEFORE constructing
// the client.
//
// This exists because Register reads Config.IP and deliberately refuses to
// guess: by the time you hold a Client, the decision has already been made, and
// the only way to change your mind would be a setter on an object that is
// meant to be immutable configuration. Callers that want auto-detection should
// do this:
//
//	ncfg.IP = registry.ResolveIP(ncfg) // when the advertised IP is unset
//	client := registry.NewClient(ncfg, logger)
//
// It returns "" when nothing suitable can be found, which the caller should
// treat as a configuration error rather than something to paper over.
func ResolveIP(cfg Config) string {
	return selectAdvertiseIP(cfg.IP, ifaces)
}

// meshPrefix is the CGNAT block RFC 6598 reserves for carrier-grade NAT, which
// is exactly the range NetBird (and Tailscale) allocate overlay addresses from.
var meshPrefix = netip.MustParsePrefix("100.64.0.0/10")

// isMeshAddr reports whether v4 falls in the overlay range, i.e. whether it is
// the address peers on the mesh can actually dial.
func isMeshAddr(v4 net.IP) bool {
	a, ok := netip.AddrFromSlice(v4.To4())
	if !ok {
		return false
	}
	return meshPrefix.Contains(a.Unmap())
}

// selectAdvertiseIP implements the AdvertiseIP chain against an injected
// interface lister.
//
// The chain prefers an OVERLAY address over a LAN one, and that ordering is the
// whole point of this function rather than a detail. A service registry stores
// the address other services will dial; on a host that has both a LAN interface
// and a NetBird interface, taking whichever comes first in the interface table
// usually lands on the LAN address — which the other peers cannot reach at all.
//
// The failure it produces is unusually well camouflaged: registration SUCCEEDS,
// the instance shows up healthy in the registry with a plausible address, and
// every log line says everything worked. The breakage only surfaces later, as
// connection timeouts from whoever tried to use the address the registry gave
// them. Observed exactly this on 245, which advertised 172.16.2.241 while its
// NetBird peers could only have reached it over 100.x.
//
// So: scan for a mesh address and return it as soon as one is found; remember
// the first routable non-mesh address only as a fallback, for the
// single-host case (a laptop, a lone docker-compose) where no overlay exists
// and the LAN address genuinely is the right one.
func selectAdvertiseIP(configuredIP string, list func() ([]netInterface, error)) string {
	if configuredIP != "" {
		return configuredIP
	}
	if list == nil {
		return ""
	}
	addrs, err := list()
	if err != nil {
		return ""
	}

	var fallback string
	for _, iface := range addrs {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		for _, addr := range iface.Addrs {
			ip := addrIP(addr)
			if ip == nil {
				continue
			}
			v4 := ip.To4()
			if v4 == nil {
				continue // prefer IPv4; the registry is reached over it here
			}
			if v4.IsLoopback() {
				continue
			}
			if v4.IsLinkLocalUnicast() {
				continue
			}
			if !v4.IsGlobalUnicast() {
				continue
			}
			if isMeshAddr(v4) {
				return v4.String()
			}
			if fallback == "" {
				fallback = v4.String()
			}
		}
	}
	return fallback
}

// addrIP extracts the IP from either *net.IPNet or *net.IPAddr.
func addrIP(addr net.Addr) net.IP {
	switch a := addr.(type) {
	case *net.IPNet:
		return a.IP
	case *net.IPAddr:
		return a.IP
	default:
		return nil
	}
}

// Register announces this instance to the registry.
//
// Why v2 and not v1: the v1 beat endpoint accepts only GET and PUT, and its
// optional "beat" override is parsed from the request BODY rather than the
// query string, so a client that sends the conventional query-param form is
// silently ignored — heartbeat overrides in v1 do not work for a standard
// client. v2 is POST everywhere with honest query parameters, so one shape
// covers register, beat and deregister.
//
// This method never guesses the advertised address: an unset IP or Port is a
// configuration error and is returned as one.
func (c *Client) Register(ctx context.Context) error {
	if err := c.ready(); err != nil {
		return err
	}

	body := registerRequest{
		ServiceName: c.cfg.ServiceName,
		IP:          c.cfg.IP,
		Port:        c.cfg.Port,
		NamespaceID: c.cfg.NamespaceID,
		GroupName:   c.cfg.GroupName,
		ClusterName: c.cfg.ClusterName,
		Weight:      c.cfg.Weight,
		Enabled:     true,
		Ephemeral:   true,
		Metadata:    c.cfg.Metadata,
	}
	return c.do(ctx, http.MethodPost, pathInstance, nil, body, nil)
}

// Beat sends one heartbeat and adopts the interval the server advertises.
//
// Honouring clientBeatInterval is the whole point of this method. The server
// owns the eviction policy (15s to unhealthy, 30s to removal), so a client
// that hardcodes its own 5s timer will either beat far too often or — worse,
// too rarely for a server configured with a tighter window. The configured
// Config.HeartbeatInterval is only used until the first beat comes back.
func (c *Client) Beat(ctx context.Context) error {
	if err := c.ready(); err != nil {
		return err
	}

	q := url.Values{}
	q.Set("serviceName", c.cfg.ServiceName)
	q.Set("ip", c.cfg.IP)
	q.Set("port", strconv.Itoa(c.cfg.Port))
	q.Set("clusterName", c.cfg.ClusterName)

	var data beatData
	if err := c.do(ctx, http.MethodPost, pathBeat, q, nil, &data); err != nil {
		return err
	}
	if data.ClientBeatInterval > 0 {
		interval := time.Duration(data.ClientBeatInterval) * time.Millisecond
		if interval < minAdvertisedBeatInterval {
			// A sub-50ms advertisement is either a unit mix-up or a hostile
			// value. Clamp rather than turn into a busy loop.
			interval = minAdvertisedBeatInterval
		}
		// No upper clamp on purpose: the server owns its own eviction policy
		// and beating faster than it asks would just be noise on the wire.
		if current := c.heartbeatInterval(); current != interval {
			c.logger.Info("nacos heartbeat interval adopted from server",
				"interval", interval, "previous", current)
		}
		c.setHeartbeatInterval(interval)
	}
	return nil
}

// Deregister removes this instance from the registry.
//
// Best-effort by construction: the server answers "ok" for an instance that
// does not exist, so Deregister is safe to call on every shutdown even if
// registration never succeeded. Correctness does not depend on it — an
// ephemeral instance that simply stops beating is marked unhealthy at 15s and
// removed at 30s — but deregistering turns a 15s window of phantom routing
// into an instant one.
func (c *Client) Deregister(ctx context.Context) error {
	if err := c.ready(); err != nil {
		return err
	}

	q := url.Values{}
	q.Set("serviceName", c.cfg.ServiceName)
	q.Set("ip", c.cfg.IP)
	q.Set("port", strconv.Itoa(c.cfg.Port))
	q.Set("clusterName", c.cfg.ClusterName)
	q.Set("namespaceId", c.cfg.NamespaceID)
	q.Set("groupName", c.cfg.GroupName)

	return c.do(ctx, http.MethodDelete, pathInstance, q, nil, nil)
}

// ListInstances returns the instances currently registered for serviceName.
// The name may be bare ("acc-go") or fully qualified
// ("DEFAULT_GROUP@@acc-go"), exactly as the v2 API accepts it.
func (c *Client) ListInstances(ctx context.Context, serviceName string) ([]Instance, error) {
	if c == nil {
		return nil, ErrNilClient
	}
	if err := c.usable(); err != nil {
		return nil, err
	}
	if serviceName == "" {
		return nil, errors.New("nacos: ListInstances requires a service name")
	}

	q := url.Values{}
	q.Set("serviceName", serviceName)
	q.Set("namespaceId", c.cfg.NamespaceID)
	q.Set("groupName", c.cfg.GroupName)
	q.Set("clusters", c.cfg.ClusterName)
	q.Set("healthyOnly", "true")

	var data instanceListData
	if err := c.do(ctx, http.MethodGet, pathInstanceList, q, nil, &data); err != nil {
		return nil, err
	}
	return data.Hosts, nil
}

// ListServices returns the registered service names for one page. The server
// qualifies each entry as "GROUP@@name".
func (c *Client) ListServices(ctx context.Context, pageNo, pageSize int) ([]string, error) {
	if c == nil {
		return nil, ErrNilClient
	}
	if err := c.usable(); err != nil {
		return nil, err
	}
	if pageNo <= 0 {
		pageNo = 1
	}
	if pageSize <= 0 {
		pageSize = 10
	}

	q := url.Values{}
	q.Set("pageNo", strconv.Itoa(pageNo))
	q.Set("pageSize", strconv.Itoa(pageSize))

	var data serviceListData
	if err := c.do(ctx, http.MethodGet, pathServiceList, q, nil, &data); err != nil {
		return nil, err
	}
	return data.Services, nil
}

// Ping probes /nacos/actuator/health.
//
// The health endpoint is deliberately NOT one of the enveloped endpoints: it
// answers with a bare {"status":"UP"} body. Running it through the envelope
// decoder would silently discard the status field and report every registry as
// healthy, so this path parses the raw body instead.
func (c *Client) Ping(ctx context.Context) error {
	if c == nil {
		return ErrNilClient
	}
	if err := c.usable(); err != nil {
		return err
	}

	raw, err := c.send(ctx, http.MethodGet, pathHealth, nil, nil)
	if err != nil {
		return err
	}

	var health healthData
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &health); err != nil {
			return fmt.Errorf("nacos: decode health response: %w (body: %s)",
				err, summarizeBody(raw))
		}
	}
	if health.Status != "" && !strings.EqualFold(health.Status, "UP") {
		return fmt.Errorf("nacos: registry at %s reports status %q", c.base, health.Status)
	}
	return nil
}

// ready reports whether the client is configured enough to talk to a registry.
// Network calls use it; Start uses it to decide whether it has any work at all.
func (c *Client) ready() error {
	if c == nil {
		return ErrNilClient
	}
	if err := c.usable(); err != nil {
		return err
	}
	if c.cfg.IP == "" {
		return fmt.Errorf("nacos: cannot register %q: IP is not set; set it explicitly "+
			"(call AdvertiseIP for the auto-detected fallback) instead of guessing",
			c.cfg.ServiceName)
	}
	if c.cfg.Port <= 0 || c.cfg.Port > 65535 {
		return fmt.Errorf("nacos: cannot register %q: port %d is not valid (1-65535)",
			c.cfg.ServiceName, c.cfg.Port)
	}
	return nil
}

// usable reports the weaker condition: enabled, with a server address and a
// service name. It deliberately does not require IP/Port, because discovery
// and health work without an instance identity.
func (c *Client) usable() error {
	if c == nil {
		return ErrNilClient
	}
	if !c.cfg.Enabled {
		return fmt.Errorf("%w (Enabled=false)", ErrDisabled)
	}
	if c.base == "" {
		return fmt.Errorf("%w (ServerAddr is empty; run Discover first)", ErrDisabled)
	}
	if c.cfg.ServiceName == "" {
		return fmt.Errorf("%w (ServiceName is empty)", ErrDisabled)
	}
	return nil
}

// do performs one registry call and unwraps the standard envelope.
//
// It retries exactly once when the registry rejects our credentials. That
// covers the cases the TTL arithmetic cannot predict — a registry restart, a
// token revoked out of band, a server whose advertised TTL outlasts the token
// it actually honours — and turns a permanent, silent loss of registration
// into one extra login. The retry is bounded to a single attempt because an
// unbounded one turns a genuinely wrong password into a login storm.
//
// out may be nil when the caller does not care about the data object. body may
// be nil for GET/DELETE.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	raw, err := c.send(ctx, method, path, query, body)
	if err != nil {
		if !isAuthRejection(err) {
			return err
		}
		c.logger.Warn("nacos rejected credentials; refreshing token and retrying once",
			"path", path, "method", method)
		c.invalidateToken()
		raw, err = c.send(ctx, method, path, query, body)
		if err != nil {
			return err
		}
	}

	var env envelope
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &env); err != nil {
			return fmt.Errorf("nacos: %s %s: decode envelope: %w (body: %s)",
				method, path, err, summarizeBody(raw))
		}
		if env.Code != 0 {
			return fmt.Errorf("nacos: %s %s: registry error code %d: %s",
				method, path, env.Code, orString(env.Message, summarizeBody(raw)))
		}
	}

	if out == nil || len(env.Data) == 0 {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("nacos: %s %s: decode data: %w", method, path, err)
	}
	return nil
}

// send performs one HTTP call and returns the raw body.
//
// Any non-2xx status is an error even when the body is plain text — goacos
// answers a bad register with a 400 and the literal string
// "serviceName/ip/port required", which is far more useful to the operator
// than a bare status code, so the body is quoted back.
func (c *Client) send(ctx context.Context, method, path string, query url.Values, body any) ([]byte, error) {
	if c == nil {
		return nil, ErrNilClient
	}
	if ctx == nil {
		return nil, errors.New("nacos: nil context")
	}

	var payload io.Reader
	contentType := ""
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("nacos: marshal %s %s request: %w", method, path, err)
		}
		payload = bytes.NewReader(raw)
		contentType = "application/json"
	}

	u := c.base + pathPrefix + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, u, payload)
	if err != nil {
		return nil, fmt.Errorf("nacos: build %s %s: %w", method, u, err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")

	token, err := c.ensureToken(ctx)
	if err != nil {
		return nil, err
	}
	if token != "" {
		// goacos checks accessToken (query) → accessToken cookie → Bearer
		// header, in that order. The header is used because it does not leak
		// the token into access logs and proxy configuration.
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("nacos: %s %s: %w", method, u, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBodyBytes))
		_ = resp.Body.Close()
	}()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("nacos: read %s %s response: %w", method, u, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &statusError{
			method: method, path: u,
			status: resp.StatusCode,
			body:   summarizeBody(raw),
		}
	}
	return raw, nil
}

// ensureToken returns the bearer token to use, logging in on first need.
//
// Auth is off on a default goacos deployment, so an empty username/password
// pair is the normal case and costs nothing. The token is cached because it
// is valid for the server-advertised tokenTtl (18000s by default) and
// re-logging in on every beat would triple the request count.
//
// The TTL is honoured rather than cached forever. Caching it indefinitely
// fails in the worst possible way: five hours in, every register and beat
// starts returning 401, goacos marks the ephemeral instance unhealthy after
// 15s and drops it after 30s — and because the stale token is still cached
// and still "valid" as far as this function is concerned, the instance never
// comes back. Service discovery for a long-running process would fail silently
// and permanently, with the only cure being a process restart. Refreshing
// before expiry costs one login every five hours and removes that failure
// mode entirely.
func (c *Client) ensureToken(ctx context.Context) (string, error) {
	if c.cfg.Token != "" {
		return c.cfg.Token, nil
	}
	if c.cfg.Username == "" && c.cfg.Password == "" {
		return "", nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && !c.tokenExpiredLocked(time.Now()) {
		return c.token, nil
	}

	token, ttl, err := c.login(ctx)
	if err != nil {
		return "", err
	}
	c.token = token
	c.tokenExpiry = time.Now().Add(refreshLifetime(ttl))
	return token, nil
}

// tokenRefreshMargin bounds how early we refresh, and tokenRefreshFloor is
// the shortest window we will ever believe in.
//
// The margin absorbs the two things that make a naive "expiry = receipt + ttl"
// slightly wrong: the token was issued before the response reached us, and
// this process's clock is not the registry's. Both push real expiry EARLIER
// than our estimate, never later — so refreshing on the early side is the only
// safe direction. Refreshing a little early costs one extra login; refreshing
// late costs a deregistration.
//
// The margin scales with the TTL rather than being a flat 60s: a flat margin
// exceeds short-lived test tokens entirely, which would degenerate into
// re-logging in on every single call.
const (
	tokenRefreshMargin = 60 * time.Second
	tokenRefreshFloor  = 100 * time.Millisecond
)

// refreshLifetime returns how long to trust a token advertised with ttl.
func refreshLifetime(ttl time.Duration) time.Duration {
	margin := ttl / 10
	if margin > tokenRefreshMargin {
		margin = tokenRefreshMargin
	}
	d := ttl - margin
	if d < tokenRefreshFloor {
		// Pathologically short TTL: halving it is still a usable schedule,
		// whereas trusting a value below the floor means refreshing in a loop.
		d = ttl / 2
	}
	return d
}

// tokenExpiredLocked reports whether the cached token must be replaced.
//
// Callers must hold c.mu. A token with no known expiry is treated as expired:
// the client only reaches that state if the server omitted tokenTtl, and
// assuming a token of unknown lifetime is valid forever is precisely the bug
// this whole mechanism exists to prevent.
func (c *Client) tokenExpiredLocked(now time.Time) bool {
	if c.tokenExpiry.IsZero() {
		return true
	}
	return !now.Before(c.tokenExpiry)
}

// invalidateToken drops the cached token so the next ensureToken re-logs in.
//
// Called when the registry rejects a request we believed was authenticated.
// This is the recovery path for the case the TTL maths cannot cover: a
// registry restart, a token revoked out of band, or a server whose advertised
// TTL is longer than the token it actually honours.
func (c *Client) invalidateToken() {
	if c.cfg.Token != "" {
		// Caller-supplied token: we cannot mint a replacement, and clearing it
		// would turn a recoverable 401 into permanent failure.
		return
	}
	c.mu.Lock()
	c.token = ""
	c.tokenExpiry = time.Time{}
	c.mu.Unlock()
}

// login performs POST /nacos/v1/auth/users/login. v1 is used here on purpose:
// it is the only login endpoint goacos exposes, and it is a plain form POST
// with no v1 beat quirks attached.
//
// It returns the advertised lifetime alongside the token so the caller can
// schedule a refresh; see ensureToken for why ignoring that TTL is a
// production outage waiting to happen.
func (c *Client) login(ctx context.Context) (string, time.Duration, error) {
	if ctx == nil {
		return "", 0, errors.New("nacos: nil context")
	}

	u := c.base + pathPrefix + pathLogin
	form := url.Values{}
	form.Set("username", c.cfg.Username)
	form.Set("password", c.cfg.Password)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("nacos: build login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpc.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("nacos: login at %s: %w", u, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBodyBytes))
		_ = resp.Body.Close()
	}()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", 0, fmt.Errorf("nacos: read login response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// A wrong password is a 401 with {"code":401,"message":"unknown user"}.
		return "", 0, fmt.Errorf("nacos: login at %s failed: status %d: %s",
			u, resp.StatusCode, summarizeBody(raw))
	}

	var out loginResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", 0, fmt.Errorf("nacos: decode login response: %w (body: %s)",
			err, summarizeBody(raw))
	}

	token := out.AccessToken
	if token == "" && out.Data != nil {
		token = out.Data.AccessToken
	}
	if token == "" {
		return "", 0, fmt.Errorf("nacos: login at %s returned no accessToken: %s",
			u, summarizeBody(raw))
	}
	ttl := out.TokenTTL
	if ttl <= 0 && out.Data != nil {
		ttl = out.Data.TokenTTL
	}
	// A registry that authenticates but advertises no TTL leaves us with no
	// basis for a refresh schedule. Rather than cache the token forever — the
	// failure mode this whole change exists to remove — assume a conservative
	// lifetime and refresh on it.
	if ttl <= 0 {
		ttl = int(defaultTokenTTL.Seconds())
		c.logger.Warn("nacos login advertised no token TTL; assuming the default",
			"user", c.cfg.Username, "assumed_ttl", defaultTokenTTL)
	}
	c.logger.Info("nacos login ok", "user", c.cfg.Username,
		"token_ttl", time.Duration(ttl)*time.Second)
	return token, time.Duration(ttl) * time.Second, nil
}

// heartbeatInterval returns the effective beat period: the server-advertised
// value once a beat has succeeded, the configured fallback before that.
func (c *Client) heartbeatInterval() time.Duration {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.beatInterval > 0 {
		return c.beatInterval
	}
	if c.cfg.HeartbeatInterval > 0 {
		return c.cfg.HeartbeatInterval
	}
	return DefaultHeartbeatInterval
}

func (c *Client) setHeartbeatInterval(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.beatInterval = d
}

// normalizeBaseURL trims trailing slashes and assumes http when no scheme was
// given. Discovery candidates are written as "goacos:8848" because that is how
// they read in a compose file; a scheme-less URL is a net/http error waiting to
// happen, so it is fixed here once instead of at every call site.
func normalizeBaseURL(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	addr = strings.TrimRight(addr, "/")
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	return addr
}

// summarizeBody trims a response body down to something loggable.
// isAuthRejection reports whether err is the registry refusing our token.
//
// Matched on the status goacos returns for an expired or wrong credential.
// Matching the status rather than the message keeps this working across
// goacos versions that word the error differently, and it deliberately does
// NOT treat a transport failure as an auth problem: re-logging in cannot fix
// a dial timeout, and doing so on every blip would hammer the login endpoint
// exactly when the registry is already unwell.
func isAuthRejection(err error) bool {
	if err == nil {
		return false
	}
	var se *statusError
	if errors.As(err, &se) {
		return se.status == http.StatusUnauthorized || se.status == http.StatusForbidden
	}
	return false
}

// statusError carries an HTTP status alongside the message so callers can
// react to the code rather than pattern-matching prose.
type statusError struct {
	method string
	path   string
	status int
	body   string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("nacos: %s %s: unexpected status %d: %s",
		e.method, e.path, e.status, e.body)
}

func summarizeBody(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return "(empty body)"
	}
	if len(s) > maxErrorBodyBytes {
		return s[:maxErrorBodyBytes] + "..."
	}
	return s
}

func orString(v, fallback string) string {
	// TrimSpace is deliberate, not hygiene: a group/cluster of "   " is not
	// an empty name to be filled in later, it is a **different, unreachable
	// name** — the instance lands in a bucket nobody queries, and every layer
	// above reports success. The Python binding already strips its env values;
	// without this the three SDKs disagree on what an unset variable means.
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// discardHandler is the logger used when NewClient is handed a nil *slog.Logger.
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (h discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h discardHandler) WithGroup(string) slog.Handler           { return h }

// Start begins the register-and-heartbeat loop in a background goroutine.
//
// Registration is retried forever with exponential backoff. That is a
// deliberate trade: an unavailable registry must not stop the process from
// booting, but it also must not be silently ignored, because "the service is
// up but not registered" is exactly the failure where callers keep retrying a
// service that has no peers. Every attempt is logged.
//
// Calling Start on a disabled or under-configured client is a no-op, and
// calling it twice is also a no-op — the second call logs and returns.
//
// ctx governs the loop's lifetime. Nothing inside the loop substitutes
// context.Background(); a cancelled ctx is the single shutdown signal.
func (c *Client) Start(ctx context.Context) {
	if c == nil {
		return
	}
	if ctx == nil {
		c.logger.Error("nacos Start called with a nil context; ignoring")
		return
	}
	if err := c.ready(); err != nil {
		c.logger.Warn("nacos Start is a no-op", "reason", err)
		return
	}

	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	if c.cancel != nil {
		c.logger.Debug("nacos client already started")
		return
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	c.cancel = cancel
	c.done = done
	c.stopped = false

	go c.run(runCtx, done)
	c.logger.Info("nacos lifecycle started", "service", c.cfg.ServiceName)
}

// run is the register/beat loop. It owns done and closes it on exit.
func (c *Client) run(ctx context.Context, done chan struct{}) {
	defer close(done)

	backoff := c.registerBackoffInitial
	registered := false
	consecutiveBeatFailures := 0

	for {
		if ctx.Err() != nil {
			return
		}

		if !registered {
			// Register with a short per-attempt budget so a hung registry
			// cannot stall the loop; the retry cadence is what handles a
			// registry that is down, not a single blocked call.
			attemptCtx, cancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
			err := c.Register(attemptCtx)
			cancel()

			if err == nil {
				registered = true
				consecutiveBeatFailures = 0
				backoff = c.registerBackoffInitial
				c.logger.Info("nacos instance registered",
					"service", c.cfg.ServiceName,
					"ip", c.cfg.IP,
					"port", c.cfg.Port,
					"group", c.cfg.GroupName)
			} else if ctx.Err() != nil {
				return
			} else {
				c.logger.Warn("nacos registration failed; will retry",
					"service", c.cfg.ServiceName,
					"attempt_in", backoff,
					"error", err)
				if !sleepCtx(ctx, backoff) {
					return
				}
				backoff = c.nextBackoff(backoff)
				continue
			}
		}

		// Sleep first: a beat immediately after registration is redundant.
		interval := c.heartbeatInterval()
		if !sleepCtx(ctx, interval) {
			return
		}

		beatCtx, cancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
		err := c.Beat(beatCtx)
		cancel()
		if err == nil {
			consecutiveBeatFailures = 0
			continue
		}
		if ctx.Err() != nil {
			return
		}

		consecutiveBeatFailures++
		c.logger.Warn("nacos heartbeat failed",
			"consecutive_failures", consecutiveBeatFailures,
			"error", err)

		if consecutiveBeatFailures >= beatFailureThreshold {
			// The server may have evicted the instance (a missed beat past
			// the 15s/30s thresholds removes it outright), in which case
			// beating is pointless forever until we register again. Re-register
			// rather than assume our own state is still authoritative.
			c.logger.Warn("nacos beat failing repeatedly; re-registering",
				"service", c.cfg.ServiceName)
			registered = false
			consecutiveBeatFailures = 0
		}
	}
}

// Stop deregisters the instance and stops the background loop.
//
// Stop is safe without a preceding Start and safe to call twice. Deregistration
// gets its own short timeout, derived from ctx, so a wedged registry delays
// shutdown by seconds rather than minutes.
//
// A cancelled ctx means no request can be sent at all. Deregistration is
// best-effort (the server evicts ephemeral instances on its own), so Stop
// reports success in that case instead of handing the caller a context error
// it cannot act on.
func (c *Client) Stop(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	c.lifecycleMu.Lock()
	if c.stopped {
		c.lifecycleMu.Unlock()
		return nil
	}
	c.stopped = true
	cancel, done := c.cancel, c.done
	c.cancel, c.done = nil, nil
	c.lifecycleMu.Unlock()

	if cancel != nil {
		cancel()
		// Wait for the loop to exit, but never past ctx: shutdown is not
		// allowed to hang on a misbehaving client.
		select {
		case <-done:
		case <-ctx.Done():
			c.logger.Warn("nacos Stop timed out waiting for the lifecycle goroutine")
		}
	}

	if ctx.Err() != nil {
		c.logger.Warn("nacos Stop: context already done; skipping deregistration " +
			"(the server evicts ephemeral instances on its own)")
		return nil
	}
	if err := c.usable(); err != nil {
		return nil
	}
	if err := c.ready(); err != nil {
		// Never registered (no IP/port), so there is nothing on the server.
		return nil
	}

	deregCtx, cancelDereg := context.WithTimeout(ctx, deregisterTimeout)
	defer cancelDereg()
	if err := c.Deregister(deregCtx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("nacos: deregister on shutdown: %w", err)
	}
	c.logger.Info("nacos instance deregistered", "service", c.cfg.ServiceName)
	return nil
}

// nextBackoff doubles the retry delay up to this client's cap.
func (c *Client) nextBackoff(d time.Duration) time.Duration {
	next := d * 2
	if next > c.registerBackoffMax {
		return c.registerBackoffMax
	}
	return next
}

// sleepCtx waits for d, returning false if ctx was cancelled first.
// Returns true immediately for a non-positive duration, but still reports
// cancellation if ctx is already done.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
