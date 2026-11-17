package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// The gap this guards: the SDK is one of three clients of the same registry,
// and it was the only one that could not take a **pre-issued token** from the
// environment.
//
//	acc-go           envconfig:"GOACOS_TOKEN"    (agent-control-center)
//	registrar sidecar env_str("GOACOS_TOKEN")    (myvpn/deploy/goacos)
//	this package     — nothing, until now
//
// Its Config.Token field worked, but only for callers that built Config in
// code. For a fleet where every service registers itself, that difference is
// the difference between credentials that are per-service and revocable, and
// credentials that are a shared long-lived password held by every process.
// The registry treats them very differently, and so should the fleet.
//
// These tests pin the env surface (the spelling and the trimming) and the
// behaviour it buys (a token authenticates without a login round trip).

// TestConfigFromEnvReadsPreissuedToken pins the env plumbing: the token
// reaches Config, a dirty read is cleaned, and an unset token stays empty.
func TestConfigFromEnvReadsPreissuedToken(t *testing.T) {
	// The spelling is part of the contract: acc-go and the registrar sidecar
	// already use this name, and the fleet rule is one spelling for all three.
	if EnvToken != "GOACOS_TOKEN" {
		t.Fatalf("EnvToken = %q, want %q (the other two clients of this registry use this name)", EnvToken, "GOACOS_TOKEN")
	}

	env := map[string]string{
		EnvEnabled:     "true",
		EnvServerAddr:  "http://127.0.0.1:8848",
		EnvServiceName: "acc-go",
		EnvToken:       "  pre-issued-token\n",
	}
	cfg, err := ConfigFromEnv(func(k string) string { return env[k] }, Service{Name: "acc-go"})
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	// Trimmed: a secret read from a file or a heredoc routinely carries a
	// trailing newline, and a newline inside a Bearer header fails
	// authentication in a way that looks like a bad token, not a dirty read.
	if cfg.Token != "pre-issued-token" {
		t.Errorf("cfg.Token = %q, want %q (whitespace must be trimmed)", cfg.Token, "pre-issued-token")
	}

	// Absent stays empty — the SDK must not invent a credential, and an
	// empty token must keep the login flow as the fallback.
	env[EnvToken] = ""
	cfg2, err := ConfigFromEnv(func(k string) string { return env[k] }, Service{Name: "acc-go"})
	if err != nil {
		t.Fatalf("ConfigFromEnv (no token): %v", err)
	}
	if cfg2.Token != "" {
		t.Errorf("cfg.Token = %q, want empty when %s is unset", cfg2.Token, EnvToken)
	}
}

// TestConfigFromEnvTokenAuthenticatesWithoutLogin pins the behaviour the
// knob exists for: with a token in hand, the client authenticates with it
// and never performs a login round trip — so the password path stays unused
// and a revoked token stays revocable per service.
func TestConfigFromEnvTokenAuthenticatesWithoutLogin(t *testing.T) {
	var (
		logins   atomic.Int32
		requests atomic.Int32
	)
	const token = "pre-issued-token"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathPrefix + pathLogin:
			logins.Add(1)
			_, _ = w.Write([]byte(`{"accessToken":"from-login","tokenTtl":18000,"code":200,"message":"success"}`))
		case pathPrefix + pathInstance:
			requests.Add(1)
			if got := r.Header.Get("Authorization"); got != "Bearer "+token {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"code":401,"message":"unknown user"}`))
				return
			}
			writeEnvelope(t, w, "ok")
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	// Build the config the way an operator would: through the environment.
	env := map[string]string{
		EnvEnabled:     "true",
		EnvServerAddr:  srv.URL,
		EnvServiceName: "acc-go",
		EnvToken:       token,
		// Credentials are present too. The token must win: that is the
		// difference between "this service holds a revocable credential" and
		// "this service holds a password".
		EnvUsername: "acc-go",
		EnvPassword: "should-not-be-used",
	}
	cfg, err := ConfigFromEnv(func(k string) string { return env[k] }, Service{Name: "acc-go"})
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	cfg.GroupName = DefaultGroupName
	cfg.ClusterName = DefaultClusterName
	cfg.IP = "10.0.0.5"
	cfg.Port = 4101
	cfg.Weight = 1.0
	cfg.RequestTimeout = 2 * time.Second

	c := NewClient(cfg, testLogger())
	if err := c.Register(context.Background()); err != nil {
		t.Fatalf("Register with a pre-issued token: %v", err)
	}
	if got := logins.Load(); got != 0 {
		t.Errorf("login endpoint called %d times, want 0 — a caller-supplied token must not trigger a login round trip", got)
	}
	if got := requests.Load(); got == 0 {
		t.Error("no registration request reached the registry")
	}
}

// TestConfigFromEnvDoesNotReadRealEnvironment guards the other direction:
// ConfigFromEnv must not fall back to the process environment when it is
// given a reader. A stray GOACOS_TOKEN in the operator's shell would
// otherwise silently become a credential for a service that was configured
// without one.
func TestConfigFromEnvDoesNotReadRealEnvironment(t *testing.T) {
	t.Setenv(EnvToken, "leaked-from-the-shell")
	t.Setenv(EnvUsername, "leaked-user")

	cfg, err := ConfigFromEnv(func(k string) string { return "" }, Service{Name: "acc-go"})
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.Token != "" {
		t.Errorf("cfg.Token = %q, want empty — a supplied reader must be the only source", cfg.Token)
	}
	if cfg.Username != "" {
		t.Errorf("cfg.Username = %q, want empty — a supplied reader must be the only source", cfg.Username)
	}
}

// TestConfigFromEnvReadsClusterName pins the second gap of the same family.
//
// The cluster is part of the instance identity key on the server: namespace,
// group, service, cluster, ip, port. A service that registers through this
// package used to land in "DEFAULT" whatever the operator asked for, because
// this package could not read the cluster name from the environment — while
// the goacos-registrar command, acc-go and the myvpn registrar sidecar all
// could. A fleet spread over several hosts then showed one cluster, and the
// balancing had no cluster dimension to work with.
func TestConfigFromEnvReadsClusterName(t *testing.T) {
	if EnvCluster != "GOACOS_CLUSTER" {
		t.Fatalf("EnvCluster = %q, want %q (the other three clients of this registry use this name)", EnvCluster, "GOACOS_CLUSTER")
	}

	env := map[string]string{
		EnvEnabled:     "true",
		EnvServerAddr:  "http://127.0.0.1:8848",
		EnvServiceName: "acc-go",
		EnvCluster:     "  kxpms-prod\n",
	}
	cfg, err := ConfigFromEnv(func(k string) string { return env[k] }, Service{Name: "acc-go"})
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.ClusterName != "kxpms-prod" {
		t.Errorf("cfg.ClusterName = %q, want %q (whitespace must be trimmed)", cfg.ClusterName, "kxpms-prod")
	}

	// Unset stays empty on purpose: the server and the client both default
	// it to "DEFAULT". What must not happen is a named cluster being dropped.
	env[EnvCluster] = ""
	cfg2, err := ConfigFromEnv(func(k string) string { return env[k] }, Service{Name: "acc-go"})
	if err != nil {
		t.Fatalf("ConfigFromEnv (no cluster): %v", err)
	}
	if cfg2.ClusterName != "" {
		t.Errorf("cfg.ClusterName = %q, want empty when %s is unset", cfg2.ClusterName, EnvCluster)
	}
}

// TestConfigFromEnvReadsGroupName pins the third gap of the same family.
//
// The group is the other half of the instance identity key: namespace, group,
// service, cluster, ip, port. acc-go, the goacos-registrar command and the
// myvpn registrar sidecar all read GOACOS_GROUP; this package could not, so
// every service that registered through it landed in DEFAULT_GROUP — and a
// registry that cannot separate two groups of services cannot route between
// them either.
func TestConfigFromEnvReadsGroupName(t *testing.T) {
	if EnvGroup != "GOACOS_GROUP" {
		t.Fatalf("EnvGroup = %q, want %q (the other three clients of this registry use this name)", EnvGroup, "GOACOS_GROUP")
	}

	env := map[string]string{
		EnvEnabled:     "true",
		EnvServerAddr:  "http://127.0.0.1:8848",
		EnvServiceName: "acc-go",
		EnvGroup:       "  kxpms  ",
	}
	cfg, err := ConfigFromEnv(func(k string) string { return env[k] }, Service{Name: "acc-go"})
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.GroupName != "kxpms" {
		t.Errorf("cfg.GroupName = %q, want %q", cfg.GroupName, "kxpms")
	}

	env[EnvGroup] = ""
	cfg2, err := ConfigFromEnv(func(k string) string { return env[k] }, Service{Name: "acc-go"})
	if err != nil {
		t.Fatalf("ConfigFromEnv (no group): %v", err)
	}
	if cfg2.GroupName != "" {
		t.Errorf("cfg.GroupName = %q, want empty when %s is unset", cfg2.GroupName, EnvGroup)
	}
}

// TestConfigFromEnvParsesFleetMetadataFormat pins the format the other
// clients already use — comma-separated `k=v` — and the two rules that make
// it safe: an unparseable segment is skipped rather than fatal, and the
// operator's value wins over the service's own on a key collision.
func TestConfigFromEnvParsesFleetMetadataFormat(t *testing.T) {
	if EnvMetadata != "GOACOS_METADATA" {
		t.Fatalf("EnvMetadata = %q, want %q", EnvMetadata, "GOACOS_METADATA")
	}

	base := map[string]string{"role": "primary", "app": "from-the-code"}
	env := map[string]string{
		EnvEnabled:     "true",
		EnvServerAddr:  "http://127.0.0.1:8848",
		EnvServiceName: "acc-go",
		EnvMetadata:    "app=redclaw, component=auth ,broken,=nokey,k2=v2",
	}
	cfg, err := ConfigFromEnv(func(k string) string { return env[k] }, Service{Name: "acc-go", Metadata: base})
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	want := map[string]string{
		"role":      "primary", // from the service's own metadata
		"app":       "redclaw", // the operator wins on a collision
		"component": "auth",    // spaces around the pair are trimmed
		"k2":        "v2",      // a later pair is kept
	}
	if len(cfg.Metadata) != len(want) {
		t.Errorf("cfg.Metadata = %v, want %v (an unparseable segment must be skipped, not fatal)", cfg.Metadata, want)
	}
	for k, v := range want {
		if cfg.Metadata[k] != v {
			t.Errorf("cfg.Metadata[%q] = %q, want %q (full map: %v)", k, cfg.Metadata[k], v, cfg.Metadata)
		}
	}
	// The service's own map must not be mutated: a caller that reuses its
	// Service value must not see the operator's metadata leak into it.
	if base["app"] != "from-the-code" {
		t.Errorf("the service's metadata map was mutated: app = %q", base["app"])
	}
}
