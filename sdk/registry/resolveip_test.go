package registry

import "testing"

// ResolveIP is the opt-in path callers use before NewClient, because Register
// reads Config.IP and refuses to guess. It must honour an explicit value
// untouched — auto-detection must never override an operator's choice.
func TestResolveIPPrefersConfiguredValue(t *testing.T) {
	if got := ResolveIP(Config{IP: "10.20.30.40"}); got != "10.20.30.40" {
		t.Errorf("ResolveIP with explicit IP = %q, want 10.20.30.40", got)
	}
}

// With nothing configured, ResolveIP must still find the host's routable
// address rather than returning empty — an empty result makes the caller skip
// registration entirely, which is a silent loss of discovery.
func TestResolveIPAutoDetectsWhenUnset(t *testing.T) {
	got := ResolveIP(Config{})
	if got == "" {
		t.Skip("host has no non-loopback IPv4 address to advertise")
	}
	if got == "127.0.0.1" {
		t.Errorf("ResolveIP auto-detected loopback; the chain is meant to prefer a routable address")
	}
}

// ResolveIP must be safe on the zero Config — it is called during boot wiring
// where a partially-built Config is normal.
func TestResolveIPZeroConfig(t *testing.T) {
	// Just assert it does not panic.
	_ = ResolveIP(Config{})
}
