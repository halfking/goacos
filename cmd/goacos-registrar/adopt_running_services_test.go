package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The shell script is the other half of this command: it decides WHAT gets
// registered, the Go binary only performs one registration.
//
// The decision that matters here is the identity check. The reachability
// probe only proves that SOMETHING answers on a port — a 404 from a different
// service is still an answer, and a port published by another container is
// still a reachable port. Measured 2026-10-24 on 252: nbjl-app declares port
// 18081, netbird-management publishes 127.0.0.1:18081 and answers 404, so the
// probe passed and the entry would have been registered against the netbird
// management port — a healthy-looking instance pointing at somebody else's
// service.
//
// These tests drive the real script with a fake container runtime and a fake
// curl, so they say something about the script rather than about a mock of
// the script's idea of itself.

// fakeHost writes an executable into dir and returns dir with it on PATH.
//
// The fakes are what make this testable offline: the script's two inputs from
// the host are "does anything answer on this port" and "which container
// publishes this port", and both are answered by whatever is on PATH.
func fakeHost(t *testing.T, podmanPorts, curlCode string) string {
	t.Helper()
	dir := t.TempDir()

	podman := "#!/bin/sh\n" +
		"# Fake container runtime: one line per container, name|ports.\n" +
		"cat <<'EOF'\n" + podmanPorts + "EOF\n"
	if err := os.WriteFile(filepath.Join(dir, "podman"), []byte(podman), 0o755); err != nil {
		t.Fatalf("write fake podman: %v", err)
	}

	// curl is asked for an HTTP status code and prints it. "000" is what a
	// real curl prints for a connection that never happened, and the script
	// treats it as unreachable.
	curl := "#!/bin/sh\n" +
		"echo " + curlCode + "\n"
	if err := os.WriteFile(filepath.Join(dir, "curl"), []byte(curl), 0o755); err != nil {
		t.Fatalf("write fake curl: %v", err)
	}

	// A fake `docker` would be consulted only if podman is absent, so it must
	// NOT exist: the script asks podman first and stops at the first runtime
	// that answers.
	return dir
}

// runScript runs the real script with the fake host first on PATH.
func runScript(t *testing.T, fakeDir string, args ...string) (string, int) {
	t.Helper()
	script, err := filepath.Abs("adopt-running-services.sh")
	if err != nil {
		t.Fatalf("resolve script path: %v", err)
	}
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Env = append(os.Environ(),
		"PATH="+fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run script: %v\n%s", err, out)
	}
	return string(out), code
}

// A port published by a container that is not the declared one must be
// refused, not adopted.
func TestAdoptRefusesAPortPublishedByAnotherContainer(t *testing.T) {
	fake := fakeHost(t,
		"netbird-management|80/tcp -> 127.0.0.1:18081\n",
		"404")

	out, code := runScript(t, fake,
		"http://127.0.0.1:8848", "100.106.190.139", "--dry-run",
		"nbjl-app:18081@127.0.0.1#nbjl-app")

	if !strings.Contains(out, "netbird-management") {
		t.Errorf("the refusal must name the container that holds the port:\n%s", out)
	}
	if !strings.Contains(out, "跳过") {
		t.Errorf("the entry must be skipped, not adopted:\n%s", out)
	}
	if strings.Contains(out, "✅") {
		t.Errorf("the colliding entry was accepted:\n%s", out)
	}
	if code == 0 {
		t.Errorf("a refused entry must make the run fail, exit=0:\n%s", out)
	}
}

// The same entry with the right container must be accepted. Without this the
// check would be a way of refusing everything.
func TestAdoptAcceptsAPortPublishedByTheDeclaredContainer(t *testing.T) {
	fake := fakeHost(t,
		"nbjl-app|80/tcp -> 127.0.0.1:18081\n",
		"404")

	out, code := runScript(t, fake,
		"http://127.0.0.1:8848", "100.106.190.139", "--dry-run",
		"nbjl-app:18081@127.0.0.1#nbjl-app")

	if !strings.Contains(out, "✅") {
		t.Errorf("the matching entry must be accepted:\n%s", out)
	}
	if strings.Contains(out, "❌") {
		t.Errorf("the matching entry was refused:\n%s", out)
	}
	if code != 0 {
		t.Errorf("exit=%d, want 0:\n%s", code, out)
	}
}

// A container port is NOT a published host port. A container that exposes
// 18081 internally publishes nothing, and must not be mistaken for the
// holder of host port 18081.
func TestAdoptDoesNotConfuseAContainerPortWithAPublishedPort(t *testing.T) {
	fake := fakeHost(t,
		"other-service|18081/tcp, 443/tcp\n",
		"404")

	out, code := runScript(t, fake,
		"http://127.0.0.1:8848", "100.106.190.139", "--dry-run",
		"nbjl-app:18081@127.0.0.1#nbjl-app")

	if strings.Contains(out, "❌") {
		t.Errorf("a container port is not a published host port:\n%s", out)
	}
	if code != 0 {
		t.Errorf("exit=%d, want 0:\n%s", code, out)
	}
}

// Without a container name the identity cannot be checked, and the script has
// to say so rather than pretend it checked.
func TestAdoptSaysWhenTheIdentityWasNotChecked(t *testing.T) {
	fake := fakeHost(t,
		"netbird-management|80/tcp -> 127.0.0.1:18081\n",
		"404")

	out, _ := runScript(t, fake,
		"http://127.0.0.1:8848", "100.106.190.139", "--dry-run",
		"nbjl-app:18081@127.0.0.1")

	if !strings.Contains(out, "身份未核对") {
		t.Errorf("an entry with no container name must say the identity was not checked:\n%s", out)
	}
}

// Only ONE runtime may be consulted per host.
//
// A host that has both podman and docker installed is not unusual on a
// workstation, and asking both produces nonsense: a port that podman does
// not publish gets attributed to a container of the other runtime, and the
// entry is refused for a reason that has nothing to do with it.
func TestAdoptConsultsOnlyTheFirstRuntime(t *testing.T) {
	fake := fakeHost(t,
		"nbjl-app|80/tcp -> 127.0.0.1:18081\n",
		"404")
	// A docker that claims every port. It must never be consulted.
	docker := "#!/bin/sh\n" +
		"cat <<'INNER'\n" +
		"other-stack-container|80/tcp -> 0.0.0.0:18081\n" +
		"INNER\n"
	if err := os.WriteFile(filepath.Join(fake, "docker"), []byte(docker), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}

	out, _ := runScript(t, fake,
		"http://127.0.0.1:8848", "100.106.190.139", "--dry-run",
		"nbjl-app:18081@127.0.0.1#nbjl-app")

	if strings.Contains(out, "other-stack-container") {
		t.Errorf("the second runtime must not be consulted:\n%s", out)
	}
	if !strings.Contains(out, "✅") {
		t.Errorf("the matching entry must still be accepted:\n%s", out)
	}
}

// An unreachable port is still skipped — the identity check must not have
// replaced it.
func TestAdoptStillSkipsAnUnreachablePort(t *testing.T) {
	fake := fakeHost(t,
		"nbjl-app|80/tcp -> 127.0.0.1:18081\n",
		"000")

	out, code := runScript(t, fake,
		"http://127.0.0.1:8848", "100.106.190.139", "--dry-run",
		"nbjl-app:18081@127.0.0.1#nbjl-app")

	if !strings.Contains(out, "不响应") {
		t.Errorf("an unreachable port must still be skipped:\n%s", out)
	}
	if code == 0 {
		t.Errorf("a skipped entry must make the run fail, exit=0:\n%s", out)
	}
}

// The container name in the declaration is the name of the container ON THE
// MACHINE THE DECLARATION WAS WRITTEN FOR. Deployments rename containers per
// machine, and this check compares the two names character for character.
//
// Measured 2026-10-24 on 252: the declaration says
// "redclaw-local-stack-redclaw-api-1" while the container on that host is
// "redclaw-252-redclaw-api-1". The entry is therefore skipped.
//
// This is measured, not predicted: the reconciliation tool's C5 counts 12 of
// the 15 declarations that hold on 252 as skipped for exactly this reason.
// This test is the other half of that number — without it the number would
// rest on one tool's model of the other's behaviour.
func TestAdoptRefusesAContainerTheMachineRenamed(t *testing.T) {
	fake := fakeHost(t,
		"redclaw-252-redclaw-api-1|8080/tcp -> 0.0.0.0:27000\n",
		"200")

	out, code := runScript(t, fake,
		"http://127.0.0.1:8848", "100.106.190.139", "--dry-run",
		"redclaw-api:27000#redclaw-local-stack-redclaw-api-1")

	if !strings.Contains(out, "跳过") {
		t.Errorf("an entry whose container name does not match must be skipped:\n%s", out)
	}
	if strings.Contains(out, "✅") {
		t.Errorf("the renamed-container entry was accepted:\n%s", out)
	}
	if code == 0 {
		t.Errorf("a refused entry must make the run fail, exit=0:\n%s", out)
	}
}

// A host-network container shows NO published ports in `ps`, so the executor
// cannot see it as a port holder — and the identity check never runs.
//
// Measured 2026-10-24 on 252: `acc-go-252|` is empty in `podman ps` although
// the container listens on 4101.
//
// Two things are asserted here. The entry is not refused (there is no holder
// to refuse it for). And the run SAYS that the identity was not checked —
// silence would be a claim that a check happened when none did.
func TestAdoptSaysWhenTheIdentityCheckNeverRan(t *testing.T) {
	fake := fakeHost(t,
		"acc-go-252|\n",
		"200")

	out, code := runScript(t, fake,
		"http://127.0.0.1:8848", "100.106.190.139", "--dry-run",
		"acc-go:4101#acc-go-211")

	if strings.Contains(out, "❌") {
		t.Errorf("no port holder is visible, so nothing justifies a refusal:\n%s", out)
	}
	if !strings.Contains(out, "身份未核对") {
		t.Errorf("the identity check did not run and the script must say so:\n%s", out)
	}
	if code != 0 {
		t.Errorf("exit=%d, want 0:\n%s", code, out)
	}
}

// ---------------------------------------------------------------------------
// The port comes from the container's CURRENT published port.
//
// The declaration's hostPort is a snapshot of one moment, and services move:
// llm-gateway's active blue-green slot rotates between 8781 and 8782 (the
// repo says so itself), so "register the declared port" is right today and
// wrong tomorrow, with nothing in between to notice.
//
// These cases pin the three rules: the declared port is still published →
// keep it; exactly one other port is published → the port moved
// unambiguously, use the live one and say so; several ports are published →
// do not guess.
// ---------------------------------------------------------------------------

// The declared port is stale and the container publishes exactly one port:
// that port is unambiguously the service port, so it is the one registered.
func TestAdoptUsesThePortsTheContainerActuallyPublishes(t *testing.T) {
	fake := fakeHost(t,
		"llm-gateway|8080/tcp -> 127.0.0.1:8782\n",
		"200")

	out, code := runScript(t, fake,
		"http://127.0.0.1:8848", "100.106.190.139", "--dry-run",
		"llm-gateway:8781@127.0.0.1#llm-gateway")

	if code != 0 {
		t.Fatalf("exit=%d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "llm-gateway:8782") {
		t.Errorf("the live published port must be the one registered:\n%s", out)
	}
	if strings.Contains(out, "llm-gateway:8781 →") {
		t.Errorf("the stale declared port must not be registered:\n%s", out)
	}
	if !strings.Contains(out, "漂移") {
		t.Errorf("a port that moved must be said out loud, not silently absorbed:\n%s", out)
	}
}

// Several published ports and a stale declaration: which one is the service
// port is not knowable from here, and registering the wrong one produces a
// healthy instance pointing at somebody else's service. Skip it.
func TestAdoptDoesNotGuessWhenSeveralPortsArePublished(t *testing.T) {
	fake := fakeHost(t,
		"svc|80/tcp -> 127.0.0.1:9001, 443/tcp -> 127.0.0.1:9002\n",
		"200")

	out, code := runScript(t, fake,
		"http://127.0.0.1:8848", "100.106.190.139", "--dry-run",
		"svc:9000@127.0.0.1#svc")

	if !strings.Contains(out, "❌") {
		t.Errorf("an ambiguous port must be refused:\n%s", out)
	}
	if !strings.Contains(out, "跳过") {
		t.Errorf("the entry must be skipped, not adopted:\n%s", out)
	}
	if strings.Contains(out, "✅") {
		t.Errorf("an ambiguous port was adopted anyway:\n%s", out)
	}
	if code == 0 {
		t.Errorf("a refused entry must make the run fail, exit=0:\n%s", out)
	}
}

// The declared port is still published: nothing moved, and the run must look
// exactly as it did before this rule existed.
func TestAdoptKeepsTheDeclaredPortWhenItIsStillPublished(t *testing.T) {
	fake := fakeHost(t,
		"svc|80/tcp -> 127.0.0.1:9001\n",
		"200")

	out, code := runScript(t, fake,
		"http://127.0.0.1:8848", "100.106.190.139", "--dry-run",
		"svc:9001@127.0.0.1#svc")

	if code != 0 {
		t.Fatalf("exit=%d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "svc:9001 → 127.0.0.1:9001") {
		t.Errorf("the declared port must be registered unchanged:\n%s", out)
	}
	if strings.Contains(out, "漂移") {
		t.Errorf("nothing moved, so nothing should be reported as drift:\n%s", out)
	}
}

// A host-network container publishes no port at all (measured 2026-10-24 on
// 252: `acc-go-252|` — empty Ports column, although it listens on 4101).
// There is nothing to derive from, so the declaration stays authoritative and
// the entry must not be refused for a reason that does not apply.
func TestAdoptKeepsTheDeclaredPortWhenTheContainerPublishesNothing(t *testing.T) {
	fake := fakeHost(t,
		"acc-go-252|\n",
		"200")

	out, code := runScript(t, fake,
		"http://127.0.0.1:8848", "100.106.190.139", "--dry-run",
		"acc-go:4101@127.0.0.1#acc-go-252")

	if code != 0 {
		t.Fatalf("exit=%d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "acc-go:4101 → 127.0.0.1:4101") {
		t.Errorf("the declared port must stay authoritative when nothing can be derived:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// A container identity may be a FAMILY, not just one container's name.
//
// A literal name is the container's name on the machine the declaration was
// written for, and deployments rename containers per machine — so a literal
// declaration describes one host, not a service. The requirement is that
// services run on many instances across many hosts, and a declaration that
// pins the instance to one machine's name cannot express that.
//
// The family form keeps the safety property (the port's publisher must
// belong to the declared identity) while letting ONE declaration serve
// every host.
// ---------------------------------------------------------------------------

// The machine renamed the container and the declaration says so as a family:
// the entry must be accepted. This is the whole point of the family form.
func TestAdoptAcceptsAContainerTheMachineRenamedWhenTheIdentityIsAFamily(t *testing.T) {
	fake := fakeHost(t,
		"redclaw-252-redclaw-api-1|8080/tcp -> 0.0.0.0:27000\n",
		"200")

	out, code := runScript(t, fake,
		"http://127.0.0.1:8848", "100.106.190.139", "--dry-run",
		"redclaw-api:27000#redclaw-*-redclaw-api-1")

	if code != 0 {
		t.Fatalf("exit=%d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "✅") {
		t.Errorf("a family identity that matches the live container must be accepted:\n%s", out)
	}
	if strings.Contains(out, "❌") {
		t.Errorf("the renamed container was refused:\n%s", out)
	}
}

// The port belongs to somebody outside the family: still refused. The family
// form widens WHICH names count as this service, never WHOSE port may be
// taken.
func TestAdoptRefusesAPublisherOutsideTheFamily(t *testing.T) {
	fake := fakeHost(t,
		"netbird-management|80/tcp -> 127.0.0.1:18081\n",
		"404")

	out, code := runScript(t, fake,
		"http://127.0.0.1:8848", "100.106.190.139", "--dry-run",
		"nbjl-app:18081@127.0.0.1#nbjl-*")

	if !strings.Contains(out, "netbird-management") {
		t.Errorf("the refusal must name the container that holds the port:\n%s", out)
	}
	if !strings.Contains(out, "跳过") {
		t.Errorf("the entry must be skipped, not adopted:\n%s", out)
	}
	if strings.Contains(out, "✅") {
		t.Errorf("a publisher outside the family was adopted:\n%s", out)
	}
	if code == 0 {
		t.Errorf("a refused entry must make the run fail, exit=0:\n%s", out)
	}
}

// A sibling service of the same family is NOT the declared service. The
// redclaw stack holds ten containers whose names all start the same way; a
// family that swallowed the siblings would register the admin container
// under the api's name.
func TestAdoptRefusesASiblingServiceInsideTheSameFamily(t *testing.T) {
	fake := fakeHost(t,
		"redclaw-252-redclaw-admin-1|8080/tcp -> 0.0.0.0:27000\n",
		"200")

	out, code := runScript(t, fake,
		"http://127.0.0.1:8848", "100.106.190.139", "--dry-run",
		"redclaw-api:27000#redclaw-*-redclaw-api-1")

	if !strings.Contains(out, "跳过") {
		t.Errorf("a sibling service must be refused even inside the family:\n%s", out)
	}
	if strings.Contains(out, "✅") {
		t.Errorf("the sibling was adopted as the api:\n%s", out)
	}
	if code == 0 {
		t.Errorf("a refused entry must make the run fail, exit=0:\n%s", out)
	}
}

// A family with no literal character matches every container, which is not
// an identity check — it is the absence of one wearing a pattern's clothes.
// The script must refuse it rather than let it pass silently.
func TestAdoptRefusesAnIdentityWithNoLiteralCharacters(t *testing.T) {
	fake := fakeHost(t,
		"anything|80/tcp -> 127.0.0.1:18081\n",
		"404")

	out, code := runScript(t, fake,
		"http://127.0.0.1:8848", "100.106.190.139", "--dry-run",
		"nbjl-app:18081@127.0.0.1#*")

	if !strings.Contains(out, "字面量") {
		t.Errorf("a degenerate identity must be named as such:\n%s", out)
	}
	if strings.Contains(out, "✅") {
		t.Errorf("a degenerate identity was accepted:\n%s", out)
	}
	if code == 0 {
		t.Errorf("a refused entry must make the run fail, exit=0:\n%s", out)
	}
}

// `case` reads [ as the start of a bracket expression, so an identity
// carrying one would mean something the author never wrote. Refuse it.
func TestAdoptRefusesAnIdentityWithIllegalCharacters(t *testing.T) {
	fake := fakeHost(t,
		"nbjl-app|80/tcp -> 127.0.0.1:18081\n",
		"404")

	out, code := runScript(t, fake,
		"http://127.0.0.1:8848", "100.106.190.139", "--dry-run",
		"nbjl-app:18081@127.0.0.1#nbjl-[go")

	if !strings.Contains(out, "非法字符") {
		t.Errorf("a malformed identity must be named as such:\n%s", out)
	}
	if strings.Contains(out, "✅") {
		t.Errorf("a malformed identity was accepted:\n%s", out)
	}
	if code == 0 {
		t.Errorf("a refused entry must make the run fail, exit=0:\n%s", out)
	}
}

// Several containers belong to the family. Their ports are collected, and the
// declared port — still published by one of them — stays authoritative.
func TestAdoptCollectsThePortsOfEveryMemberOfTheFamily(t *testing.T) {
	fake := fakeHost(t,
		"acc-go-252|8080/tcp -> 0.0.0.0:4101\n"+
			"acc-blue-252|8080/tcp -> 0.0.0.0:4102\n",
		"200")

	out, code := runScript(t, fake,
		"http://127.0.0.1:8848", "100.106.190.139", "--dry-run",
		"acc-go:4101#acc-*-252")

	if code != 0 {
		t.Fatalf("exit=%d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "acc-go:4101 →") {
		t.Errorf("the declared port must stay authoritative when it is still published:\n%s", out)
	}
	if strings.Contains(out, "漂移") {
		t.Errorf("nothing moved, so nothing should be reported as drift:\n%s", out)
	}
}

// The family publishes several ports and none of them is the declared one:
// which one is the service port is not knowable from here, so the entry is
// skipped rather than guessed at.
func TestAdoptDoesNotGuessWhenTheFamilyPublishesSeveralOtherPorts(t *testing.T) {
	fake := fakeHost(t,
		"acc-a-252|8080/tcp -> 0.0.0.0:5001\n"+
			"acc-b-252|8080/tcp -> 0.0.0.0:5002\n",
		"200")

	out, code := runScript(t, fake,
		"http://127.0.0.1:8848", "100.106.190.139", "--dry-run",
		"svc:5000#acc-*-252")

	if !strings.Contains(out, "❌") {
		t.Errorf("an ambiguous port must be refused:\n%s", out)
	}
	if !strings.Contains(out, "跳过") {
		t.Errorf("the entry must be skipped, not adopted:\n%s", out)
	}
	if strings.Contains(out, "✅") {
		t.Errorf("an ambiguous port was adopted anyway:\n%s", out)
	}
	if code == 0 {
		t.Errorf("a refused entry must make the run fail, exit=0:\n%s", out)
	}
}

// The family moved the service to another port and only one port is
// published: the port moved unambiguously, so the live one is used and said
// out loud.
func TestAdoptFollowsTheFamilyWhenTheServiceMovedToOneOtherPort(t *testing.T) {
	fake := fakeHost(t,
		"acc-go-252|8080/tcp -> 0.0.0.0:4102\n",
		"200")

	out, code := runScript(t, fake,
		"http://127.0.0.1:8848", "100.106.190.139", "--dry-run",
		"acc-go:4101#acc-*-252")

	if code != 0 {
		t.Fatalf("exit=%d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "acc-go:4102 →") {
		t.Errorf("the live published port must be the one registered:\n%s", out)
	}
	if !strings.Contains(out, "漂移") {
		t.Errorf("a port that moved must be said out loud:\n%s", out)
	}
}
