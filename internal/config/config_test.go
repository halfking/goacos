package config

// Tests for the multi-line gateway settings.
//
// These exist because the gateway tests configure the struct fields directly.
// That skips the whole env-reading path — which is where an operator actually
// meets this feature, and where a typo in a docker-compose file would live. A
// setting that parses correctly when assigned in Go but is silently dropped
// when read from the environment would pass every other test in the tree.

import (
	"reflect"
	"testing"
)

func TestGetenvListReadsLinesAndDropsBlanksAndComments(t *testing.T) {
	t.Setenv("GOACOS_TEST_LIST", "\n"+
		"first line\n"+
		"   \n"+
		"# a comment\n"+
		"  second line  \n"+
		"third, with a comma\n")

	got := getenvList("TEST_LIST")
	want := []string{"first line", "second line", "third, with a comma"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("getenvList = %#v, want %#v", got, want)
	}
}

// TestGetenvListKeepsCommas pins the reason this splits on newlines and not
// commas: the values are Go regexps and header strings, and a regexp may
// legitimately contain a comma.
func TestGetenvListKeepsCommas(t *testing.T) {
	t.Setenv("GOACOS_TEST_LIST", `(union.*select|select.*from|concat.*char)`)
	got := getenvList("TEST_LIST")
	if len(got) != 1 || got[0] != `(union.*select|select.*from|concat.*char)` {
		t.Errorf("getenvList = %#v; a regexp containing commas must survive as one entry", got)
	}
}

// TestGetenvListUnsetIsNil matters to the caller: security.go treats nil and
// empty the same, but a non-nil empty slice would still be worth distinguishing.
func TestGetenvListUnsetIsNil(t *testing.T) {
	t.Setenv("GOACOS_TEST_LIST", "")
	if got := getenvList("TEST_LIST"); got != nil {
		t.Errorf("getenvList = %#v, want nil for an empty value", got)
	}
}

// TestFromEnvWiresGatewaySecuritySettings is the end-to-end guard: the gateway
// reads cfg.GatewaySecurityHeaders, and an operator sets GOACOS_GATEWAY_… or
// NACOS_GATEWAY_…. Both spellings have to reach the same field.
func TestFromEnvWiresGatewaySecuritySettings(t *testing.T) {
	t.Setenv("GOACOS_GATEWAY_SECURITY_HEADERS", "X-Content-Type-Options: nosniff\nX-Frame-Options: SAMEORIGIN")
	t.Setenv("GOACOS_GATEWAY_BLOCK_USER_AGENTS", `(?i)sqlmap|nikto`)
	t.Setenv("GOACOS_GATEWAY_BLOCK_PATHS", `(?i)\.(git|env)$`)

	cfg := FromEnv()
	if len(cfg.GatewaySecurityHeaders) != 2 ||
		cfg.GatewaySecurityHeaders[0] != "X-Content-Type-Options: nosniff" {
		t.Errorf("GatewaySecurityHeaders = %#v", cfg.GatewaySecurityHeaders)
	}
	if len(cfg.GatewayBlockUserAgents) != 1 ||
		cfg.GatewayBlockUserAgents[0] != `(?i)sqlmap|nikto` {
		t.Errorf("GatewayBlockUserAgents = %#v", cfg.GatewayBlockUserAgents)
	}
	if len(cfg.GatewayBlockPaths) != 1 {
		t.Errorf("GatewayBlockPaths = %#v", cfg.GatewayBlockPaths)
	}
}

// TestUnsetGatewaySecuritySettingsAreEmpty pins that the feature is off by
// default. The gateway tests cover the behavioural consequence; this one stops
// a future default flip from silently changing every deployment.
func TestUnsetGatewaySecuritySettingsAreEmpty(t *testing.T) {
	cfg := FromEnv()
	if len(cfg.GatewaySecurityHeaders) != 0 ||
		len(cfg.GatewayBlockUserAgents) != 0 ||
		len(cfg.GatewayBlockPaths) != 0 {
		t.Errorf("path-level controls must default to off; got headers=%#v ua=%#v paths=%#v",
			cfg.GatewaySecurityHeaders, cfg.GatewayBlockUserAgents, cfg.GatewayBlockPaths)
	}
}
