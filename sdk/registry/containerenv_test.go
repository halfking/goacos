package registry

import (
	"net/netip"
	"os"
	"testing"
)

// Both runtimes' marker files must be probed.
//
// Docker writes /.dockerenv. Podman writes /run/.containerenv — and on 252
// the podman containers have the second and NOT the first, verified
// 2026-10-24 inside redclaw-252-redclaw-api-1. Probing only the Docker
// marker would leave every podman container invisible to the warning.
//
// Probing only one file is a silent gap: the warning would simply never
// fire on one runtime, with no error anywhere.
func TestContainerMarkerPathsCoverBothRuntimes(t *testing.T) {
	want := map[string]bool{
		"/.dockerenv":        false,
		"/run/.containerenv": false,
	}
	for _, p := range containerMarkerPaths() {
		if _, ok := want[p]; !ok {
			t.Errorf("unexpected marker path %q; each path needs a reason it is probed", p)
			continue
		}
		want[p] = true
	}
	for p, seen := range want {
		if !seen {
			t.Errorf("marker path %q is never probed", p)
		}
	}
}

// The cgroup fallback must recognise a container and must NOT recognise a
// host that merely runs a container runtime.
//
// The negative case is the one that matters: on a Docker host the daemon is
// PID 1 and its cgroup path contains "docker". Matching the bare word would
// make every Docker host look like a container, and the warning would fire
// on ordinary LAN addresses there.
//
// The positive cases are the real shapes: 252's podman containers report
// /machine.slice/libpod-<id>.scope, and a Kubernetes pod reports a
// /kubepods/... path.
func TestLooksLikeContainerCgroup(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{
			"podman on 252",
			"13:pids:/machine.slice/libpod-d25f9aab9e9c20a38bb2db899ff95bb84108fefd95491b844d7a9fb1d710c069.scope",
			true,
		},
		{"kubernetes pod", "11:memory:/kubepods/besteffort/pod8f3c1de-1111-2222-3333-444455556666", true},
		{"containerd scope", "0::/system.slice/containerd.service", true},
		{"lxc container", "0::/lxc/mycontainer", true},
		{
			"host dockerd as PID 1",
			"0::/system.slice/docker.service",
			false,
		},
		{"ordinary user session", "0::/user.slice/user-1000.slice/session.slice", false},
		{"cgroup v2, no signal", "0::/", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := looksLikeContainerCgroup(tc.content); got != tc.want {
				t.Errorf("looksLikeContainerCgroup(%q) = %v, want %v", tc.content, got, tc.want)
			}
		})
	}
}

// The probe as a whole: a marker file wins, the cgroup path is the fallback,
// and nothing means "not a container".
//
// The filesystem and cgroup seams are pinned so the verdict is the code's,
// not the machine's.
func TestDetectContainerNetns(t *testing.T) {
	cases := []struct {
		name      string
		present   []string
		cgroup    string
		cgroupErr bool
		want      bool
	}{
		{"docker marker file", []string{"/.dockerenv"}, "", false, true},
		{"podman marker file", []string{"/run/.containerenv"}, "", false, true},
		{"both marker files", []string{"/.dockerenv", "/run/.containerenv"}, "", false, true},
		{"cgroup fallback", nil, "/kubepods/besteffort/pod123", false, true},
		{"host with dockerd as PID 1", nil, "/system.slice/docker.service", false, false},
		{"host, nothing to go on", nil, "", false, false},
		{"unreadable cgroup", nil, "", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			present := map[string]bool{}
			for _, p := range tc.present {
				present[p] = true
			}
			restoreFS := swapContainerFSExists(func(p string) bool { return present[p] })
			defer restoreFS()
			restoreCgroup := swapContainerCgroupPath(func() (string, error) {
				if tc.cgroupErr {
					return "", os.ErrNotExist
				}
				return tc.cgroup, nil
			})
			defer restoreCgroup()

			if got := detectContainerNetns(); got != tc.want {
				t.Errorf("detectContainerNetns() = %v, want %v", got, tc.want)
			}
		})
	}
}

// isContainerLocalAddr decides what counts as a container's own address.
//
// RFC1918 private space is the criterion. The overlay range is excluded on
// purpose: 100.64.0.0/10 is where NetBird hands out addresses, and those are
// dialable from outside the container's network — warning about them would
// be exactly backwards.
func TestIsContainerLocalAddrBoundaries(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"10.89.11.126", true},     // podman network on 252, measured
		{"10.88.1.226", true},      // podman default CNI pool, measured
		{"172.18.0.5", true},       // docker default bridge
		{"172.16.2.210", true},     // RFC1918, even though Docker never uses it
		{"192.168.1.50", true},     // RFC1918
		{"100.106.190.139", false}, // mesh overlay — dialable from outside
		{"100.64.0.1", false},      // mesh overlay boundary
		{"8.8.8.8", false},         // public
		{"127.0.0.1", false},       // loopback
	}
	for _, c := range cases {
		a, err := netip.ParseAddr(c.ip)
		if err != nil {
			t.Fatalf("parse %s: %v", c.ip, err)
		}
		if got := isContainerLocalAddr(a); got != c.want {
			t.Errorf("isContainerLocalAddr(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
}

// swapContainerFSExists pins the filesystem half of the container probe.
func swapContainerFSExists(fn func(string) bool) func() {
	prev := containerFSExists
	containerFSExists = fn
	return func() { containerFSExists = prev }
}

// swapContainerCgroupPath pins the cgroup half of the container probe.
func swapContainerCgroupPath(fn func() (string, error)) func() {
	prev := containerCgroupPath
	containerCgroupPath = fn
	return func() { containerCgroupPath = prev }
}
