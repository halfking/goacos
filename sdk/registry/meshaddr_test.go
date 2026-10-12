package registry

import (
	"net"
	"testing"
)

// fakeIface builds one interface description in the shape selectAdvertiseIP
// consumes, so the ordering rules can be exercised without owning a NIC.
func fakeIface(name string, up bool, addrs ...string) netInterface {
	iface := netInterface{Name: name, Flags: net.FlagUp}
	if !up {
		iface.Flags = 0
	}
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip == nil {
			continue
		}
		iface.Addrs = append(iface.Addrs, &net.IPNet{IP: ip, Mask: net.CIDRMask(len(ip)*8, 32*8)})
	}
	return iface
}

func listOf(ifaces ...netInterface) func() ([]netInterface, error) {
	return func() ([]netInterface, error) { return ifaces, nil }
}

// A host with both a LAN and an overlay interface must advertise the OVERLAY
// address. This is the exact shape of the 245 failure: registration succeeded,
// the instance looked healthy in the registry, and the address it carried
// (172.16.2.241, the LAN one) was not dialable by any mesh peer — so the entry
// was worse than useless, it looked correct.
//
// The LAN interface is listed FIRST on purpose: that is the common ordering,
// and it is what made the old first-match-wins chain pick the wrong address.
func TestSelectAdvertiseIPPrefersOverlayOverLAN(t *testing.T) {
	got := selectAdvertiseIP("", listOf(
		fakeIface("eth0", true, "172.16.2.241"),
		fakeIface("wt0", true, "100.106.190.139"),
	))
	if got != "100.106.190.139" {
		t.Errorf("selectAdvertiseIP = %q, want the overlay address 100.106.190.139", got)
	}
}

// Reversing the interface order must not change the answer. If it does, the
// preference is not real — it just happened to match the table order.
func TestSelectAdvertiseIPOverlayWinsRegardlessOfOrder(t *testing.T) {
	forward := selectAdvertiseIP("", listOf(
		fakeIface("eth0", true, "172.16.2.241"),
		fakeIface("wt0", true, "100.106.190.139"),
	))
	reverse := selectAdvertiseIP("", listOf(
		fakeIface("wt0", true, "100.106.190.139"),
		fakeIface("eth0", true, "172.16.2.241"),
	))
	if forward != reverse {
		t.Errorf("result depends on interface order: %q vs %q", forward, reverse)
	}
	if forward != "100.106.190.139" {
		t.Errorf("both orders must yield the overlay address, got %q", forward)
	}
}

// A host with no overlay must still advertise its LAN address. That is the
// single-host case — a laptop, a lone docker-compose — where the LAN address
// genuinely is the address other local services should dial, and returning ""
// would make the caller skip registration entirely.
func TestSelectAdvertiseIPFallsBackToLANWithoutOverlay(t *testing.T) {
	got := selectAdvertiseIP("", listOf(
		fakeIface("lo0", true, "127.0.0.1"),
		fakeIface("eth0", true, "192.168.1.50"),
	))
	if got != "192.168.1.50" {
		t.Errorf("selectAdvertiseIP = %q, want the LAN address 192.168.1.50", got)
	}
}

// Loopback and link-local must never be advertised even when they are the only
// things present — both are unusable from another host.
func TestSelectAdvertiseIPSkipsLoopbackAndLinkLocal(t *testing.T) {
	got := selectAdvertiseIP("", listOf(
		fakeIface("lo0", true, "127.0.0.1"),
		fakeIface("en0", true, "169.254.7.7"),
	))
	if got != "" {
		t.Errorf("selectAdvertiseIP = %q, want empty when nothing routable exists", got)
	}
}

// An interface that is DOWN must be ignored, including when it is the one
// carrying the overlay address: a downed interface has nothing behind it.
func TestSelectAdvertiseIPIgnoresDownInterfaces(t *testing.T) {
	got := selectAdvertiseIP("", listOf(
		fakeIface("wt0", false, "100.106.190.139"),
		fakeIface("eth0", true, "172.16.2.241"),
	))
	if got != "172.16.2.241" {
		t.Errorf("selectAdvertiseIP = %q, want 172.16.2.241 from the up interface", got)
	}
}

// An explicit value always wins. Auto-detection must never override an operator
// who has stated the address deliberately — including when they stated a LAN
// address on a host that also has an overlay.
func TestSelectAdvertiseIPExplicitWinsOverOverlay(t *testing.T) {
	got := selectAdvertiseIP("10.20.30.40", listOf(
		fakeIface("eth0", true, "172.16.2.241"),
		fakeIface("wt0", true, "100.106.190.139"),
	))
	if got != "10.20.30.40" {
		t.Errorf("selectAdvertiseIP = %q, want the configured 10.20.30.40", got)
	}
}

// isMeshAddr must recognise the RFC 6598 block and reject everything else,
// including the addresses immediately outside it.
func TestIsMeshAddrBoundaries(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"100.64.0.0", true},      // first address in range
		{"100.127.255.255", true}, // last address in range
		{"100.106.190.139", true}, // a real NetBird address
		{"100.63.255.255", false}, // one below the range
		{"100.128.0.0", false},    // one above the range
		{"172.16.2.241", false},
		{"192.168.1.50", false}, // plain LAN address, outside the mesh block
		{"10.20.30.40", false},
		{"127.0.0.1", false},
	}
	for _, c := range cases {
		if got := isMeshAddr(net.ParseIP(c.ip)); got != c.want {
			t.Errorf("isMeshAddr(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
}

// ResolveIP is the caller-facing entry point and must expose the same
// preference, since that is the function cmd/acc actually calls.
func TestResolveIPPrefersOverlayOnThisHostWhenPresent(t *testing.T) {
	// Swap in a deterministic interface table for the duration of the call.
	prev := ifaces
	ifaces = listOf(
		fakeIface("eth0", true, "172.16.2.241"),
		fakeIface("wt0", true, "100.106.190.139"),
	)
	defer func() { ifaces = prev }()

	if got := ResolveIP(Config{}); got != "100.106.190.139" {
		t.Errorf("ResolveIP = %q, want 100.106.190.139", got)
	}
}
