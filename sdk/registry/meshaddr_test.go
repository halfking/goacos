package registry

import (
	"bytes"
	"log/slog"
	"net"
	"net/netip"
	"strings"
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

// A container is the case where the LAN fallback stops being safe.
//
// Inside a container the interface list is `lo` plus `eth0 <bridge address>` and
// nothing else. NetBird runs on the host, not in the container, so the mesh
// address selectAdvertiseIP prefers is not visible from in here — verified on
// 211, where every container had exactly lo and eth0 172.18.0.x. The fallback
// then lands on the container's own bridge address.
//
// Pin this explicitly rather than leaving it implied by the laptop case: a
// laptop on 192.168.1.50 and a container on 172.18.0.5 are indistinguishable
// from the IP alone, and only one of them is safe to advertise to a consumer
// living outside that Docker network.
func TestSelectAdvertiseIPInsideAContainerLandsOnTheBridge(t *testing.T) {
	got := selectAdvertiseIP("", listOf(
		fakeIface("lo", true, "127.0.0.1"),
		fakeIface("eth0", true, "172.18.0.5"),
	))
	if got != "172.18.0.5" {
		t.Fatalf("selectAdvertiseIP = %q, want the container's bridge address "+
			"172.18.0.5 — this test exists to pin the trap, not to change it", got)
	}
}

// 172.16.x is a real LAN range, not Docker's pool. 252's eth0 is 172.16.2.210
// and must not be treated as a container address.
func TestDockerPoolExcludes17216(t *testing.T) {
	for _, s := range []string{"172.16.2.210", "172.16.0.1", "172.32.0.1"} {
		a, err := netip.ParseAddr(s)
		if err != nil {
			t.Fatalf("parse %s: %v", s, err)
		}
		if inDockerDefaultPool(a) {
			t.Errorf("inDockerDefaultPool(%s) = true, want false: "+
				"Docker's pool starts at 172.17.0.0", s)
		}
	}
	for _, s := range []string{"172.17.0.1", "172.18.0.5", "172.23.0.9", "172.31.255.254"} {
		a, err := netip.ParseAddr(s)
		if err != nil {
			t.Fatalf("parse %s: %v", s, err)
		}
		if !inDockerDefaultPool(a) {
			t.Errorf("inDockerDefaultPool(%s) = false, want true", s)
		}
	}
}

// The warning must fire for an auto-detected bridge address and stay silent
// otherwise. Silent in three cases that matter: an explicit Config.IP (the
// operator already decided), a normal LAN address, and a mesh address.
//
// The container context is PINNED per case, and that is a fix rather than a
// detail. The first version of this test left the question to the machine it
// ran on, so the verdict changed with the environment: "LAN does not warn" is
// only true off a container, and the same test binary inside a CI container
// answers a different question. A test whose result depends on where it runs
// passes for the wrong reason.
func TestContainerBridgeWarningFiresOnlyForAutoDetectedBridges(t *testing.T) {
	cases := []struct {
		name        string
		inContainer bool
		cfgIP       string
		detected    string
		wantWarns   int
		wantInText  string
	}{
		{"auto-detected container bridge warns", false, "", "172.18.0.5", 1, "cannot reach it"},
		{"auto-detected LAN does not warn", false, "", "192.168.1.50", 0, ""},
		{"auto-detected mesh does not warn", false, "", "100.106.67.198", 0, ""},
		{"explicit choice is never second-guessed", false, "10.9.9.9", "172.18.0.5", 0, ""},
		{"auto-detected 172.16 LAN does not warn", false, "", "172.16.2.210", 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, nil))
			c := &Client{cfg: Config{IP: tc.cfgIP}, logger: logger}

			// Go through AdvertiseIP rather than calling the helper directly.
			// Calling the helper proved it works; it did NOT prove anything
			// calls it — the first version of this test did exactly that, and
			// deleting the call site from AdvertiseIP left it green.
			restore := swapIfaces(listOf(
				fakeIface("lo", true, "127.0.0.1"),
				fakeIface("eth0", true, tc.detected),
			))
			defer restore()
			restoreNetns := swapContainerNetns(tc.inContainer)
			defer restoreNetns()
			c.AdvertiseIP()
			c.AdvertiseIP() // once, not once per call

			got := strings.Count(buf.String(), "cannot reach it")
			if got != tc.wantWarns {
				t.Errorf("warning count = %d, want %d; log=%q", got, tc.wantWarns, buf.String())
			}
			if tc.wantInText != "" && !strings.Contains(buf.String(), tc.wantInText) {
				t.Errorf("log does not mention %q: %q", tc.wantInText, buf.String())
			}
		})
	}
}

// The bridge warning must also fire for the container networks that actually
// run in production.
//
// 252 runs podman, and the app containers there sit in 10.89.x/24 — measured
// 2026-10-24 on container redclaw-252-redclaw-api-1: eth0 10.89.11.126,
// eth1 10.89.13.133, and /run/.containerenv present with no /.dockerenv.
// Those addresses are outside Docker's default pool
// (172.17.0.0/16-172.31.0.0/16), so the original range check said nothing
// and the trap was silent on the one machine that runs the fleet.
//
// The addresses below are the measured ones, not invented ones.
func TestContainerBridgeWarningFiresForPodmanCNIPoolAddresses(t *testing.T) {
	cases := []struct {
		name     string
		detected string
	}{
		{"podman redclaw-local-stack network", "10.89.11.126"},
		{"podman redclaw-252-ext network", "10.89.13.133"},
		{"podman default CNI pool", "10.88.1.226"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			c := &Client{cfg: Config{}, logger: slog.New(slog.NewTextHandler(&buf, nil))}
			restore := swapIfaces(listOf(
				fakeIface("lo", true, "127.0.0.1"),
				fakeIface("eth0", true, tc.detected),
			))
			defer restore()
			restoreNetns := swapContainerNetns(true)
			defer restoreNetns()

			c.AdvertiseIP()

			if !strings.Contains(buf.String(), "cannot reach it") {
				t.Errorf("no warning for container address %s; log=%q", tc.detected, buf.String())
			}
		})
	}
}

// Outside a container the same private address is a perfectly good LAN
// address: a laptop on 192.168.1.50 reaches its LAN peers with it, and so
// does a host whose eth0 is 172.16.2.210. The warning is about a CONTAINER's
// own bridge, so with no container context there is nothing to warn about —
// warning on every private LAN address would be noise on every host.
func TestContainerBridgeWarningSilentOutsideContainer(t *testing.T) {
	for _, detected := range []string{"192.168.1.50", "10.89.11.126", "172.16.2.210"} {
		t.Run(detected, func(t *testing.T) {
			var buf bytes.Buffer
			c := &Client{cfg: Config{}, logger: slog.New(slog.NewTextHandler(&buf, nil))}
			restore := swapIfaces(listOf(
				fakeIface("lo", true, "127.0.0.1"),
				fakeIface("eth0", true, detected),
			))
			defer restore()
			restoreNetns := swapContainerNetns(false)
			defer restoreNetns()

			c.AdvertiseIP()

			if strings.Contains(buf.String(), "cannot reach it") {
				t.Errorf("warned for %s outside a container; log=%q", detected, buf.String())
			}
		})
	}
}

// A mesh address is dialable from outside the container's network, so it must
// never trigger the warning — including from inside a container, where the
// rest of the logic would otherwise treat any private address as a bridge.
func TestContainerBridgeWarningNeverFiresForMeshAddress(t *testing.T) {
	var buf bytes.Buffer
	c := &Client{cfg: Config{}, logger: slog.New(slog.NewTextHandler(&buf, nil))}
	restore := swapIfaces(listOf(
		fakeIface("lo", true, "127.0.0.1"),
		fakeIface("eth0", true, "100.106.190.139"),
	))
	defer restore()
	restoreNetns := swapContainerNetns(true)
	defer restoreNetns()

	c.AdvertiseIP()

	if strings.Contains(buf.String(), "cannot reach it") {
		t.Errorf("warned for a mesh address; log=%q", buf.String())
	}
}

// An explicit address is the operator's decision. The SDK does not
// second-guess it, not even inside a container where the same address would
// otherwise be flagged.
func TestContainerBridgeWarningNeverFiresForExplicitIP(t *testing.T) {
	var buf bytes.Buffer
	c := &Client{cfg: Config{IP: "172.16.2.210"}, logger: slog.New(slog.NewTextHandler(&buf, nil))}
	restore := swapIfaces(listOf(
		fakeIface("lo", true, "127.0.0.1"),
		fakeIface("eth0", true, "10.89.11.126"),
	))
	defer restore()
	restoreNetns := swapContainerNetns(true)
	defer restoreNetns()

	c.AdvertiseIP()

	if strings.Contains(buf.String(), "cannot reach it") {
		t.Errorf("warned despite an explicit address; log=%q", buf.String())
	}
}

// Docker's default pool must keep warning even when the container probe
// says "not a container".
//
// The probe is a best-effort signal: a runtime that writes neither marker
// file would switch the warning off entirely if the range check were made to
// depend on it. The range check is the one that cannot silently stop
// working, so it stays independent.
func TestDockerPoolWarningSurvivesAFailedContainerProbe(t *testing.T) {
	var buf bytes.Buffer
	c := &Client{cfg: Config{}, logger: slog.New(slog.NewTextHandler(&buf, nil))}
	restore := swapIfaces(listOf(
		fakeIface("lo", true, "127.0.0.1"),
		fakeIface("eth0", true, "172.18.0.5"),
	))
	defer restore()
	restoreNetns := swapContainerNetns(false)
	defer restoreNetns()

	c.AdvertiseIP()

	if !strings.Contains(buf.String(), "cannot reach it") {
		t.Errorf("Docker pool address stopped warning when the container probe "+
			"answered false; log=%q", buf.String())
	}
}

// swapContainerNetns pins the container context for the duration of a test,
// so the warning's verdict does not depend on the machine running it.
func swapContainerNetns(inContainer bool) func() {
	prev := containerNetns
	containerNetns = func() bool { return inContainer }
	return func() { containerNetns = prev }
}

// swapIfaces points the package-level interface lister at a fake for the
// duration of a test. There was no hook for this, which is why the first
// version of the warning test had to reach past AdvertiseIP and call the
// helper by hand — and consequently could not tell whether anything called it.
func swapIfaces(list func() ([]netInterface, error)) func() {
	prev := ifaces
	ifaces = list
	return func() { ifaces = prev }
}
