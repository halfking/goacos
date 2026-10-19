package api

import (
	"strings"
	"testing"

	"github.com/halfking/goacos/internal/storage"
)

// The regression this file guards is the one that made `kind` decorative: a
// frontend and a backend were forwarded identically, so nothing in the system
// distinguished them and a frontend that named no algorithm silently got
// round_robin.

func TestDefaultLBAlgorithmForKind(t *testing.T) {
	if got := defaultLBAlgorithmForKind(storage.KindWeb); got != "ip_hash" {
		t.Errorf("a frontend must default to a sticky algorithm, got %q", got)
	}
	for _, kind := range []string{storage.KindAPI, storage.KindWorker,
		storage.KindMCP, storage.KindGateway} {
		if got := defaultLBAlgorithmForKind(kind); got != "round_robin" {
			t.Errorf("kind %q should default to round_robin, got %q", kind, got)
		}
	}
}

func TestNormalizeComponentSpecDefaults(t *testing.T) {
	c := componentSpec{Kind: storage.KindWeb, ServiceName: "portal-web"}
	if err := normalizeComponentSpec(&c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.LBAlgorithm != "ip_hash" {
		t.Errorf("web component should default to ip_hash, got %q", c.LBAlgorithm)
	}
	if c.Protocol != storage.ProtocolHTTP {
		t.Errorf("protocol default wrong: %q", c.Protocol)
	}
	if c.GroupName != "DEFAULT_GROUP" {
		t.Errorf("group default wrong: %q", c.GroupName)
	}
	if c.Weight != 1 {
		t.Errorf("weight default wrong: %v", c.Weight)
	}
}

func TestNormalizeComponentSpecDefaultsKindToAPI(t *testing.T) {
	c := componentSpec{ServiceName: "acc-go"}
	if err := normalizeComponentSpec(&c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Kind != storage.KindAPI {
		t.Errorf("empty kind should default to api, got %q", c.Kind)
	}
	if c.LBAlgorithm != "round_robin" {
		t.Errorf("an api component should default to round_robin, got %q",
			c.LBAlgorithm)
	}
}

// Explicit intent must survive normalisation. This is the property that keeps
// the change from silently rewriting an operator's decision.
func TestNormalizeComponentSpecKeepsExplicitAlgorithm(t *testing.T) {
	for _, alg := range []string{"round_robin", "ip_hash", "least_conn",
		"cookie_sticky"} {
		c := componentSpec{Kind: storage.KindWeb, ServiceName: "w",
			LBAlgorithm: alg}
		if err := normalizeComponentSpec(&c); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if c.LBAlgorithm != alg {
			t.Errorf("explicit lbAlgorithm %q was rewritten to %q", alg, c.LBAlgorithm)
		}
	}
}

// Every kind storage declares must be accepted. Narrowing this set would reject
// declarations the platform documents as legitimate.
func TestNormalizeComponentSpecAcceptsEveryDeclaredKind(t *testing.T) {
	for kind := range validKinds {
		c := componentSpec{Kind: kind, ServiceName: "s"}
		if err := normalizeComponentSpec(&c); err != nil {
			t.Errorf("declared kind %q must be accepted, got %v", kind, err)
		}
	}
	if len(validKinds) != 5 {
		t.Errorf("expected the five declared kinds, got %d: %v",
			len(validKinds), validKinds)
	}
}

// A typo must be refused rather than stored. Silently accepting "frontend" and
// behaving like "api" is what made the field worthless in the first place.
func TestNormalizeComponentSpecRejectsUnknownKind(t *testing.T) {
	for _, kind := range []string{"frontend", "API", "Web", "web-api", "static"} {
		c := componentSpec{Kind: kind, ServiceName: "s"}
		err := normalizeComponentSpec(&c)
		if err == nil {
			t.Errorf("kind %q should have been rejected", kind)
			continue
		}
		if !strings.Contains(err.Error(), kind) {
			t.Errorf("error should quote the offending kind %q, got %v", kind, err)
		}
	}
}
