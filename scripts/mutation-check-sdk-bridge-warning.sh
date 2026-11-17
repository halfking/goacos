#!/usr/bin/env bash
# Mutation check for the registration surface of sdk/registry:
#   * the container-bridge warning (M1-M12), and
#   * the credential env surface — GOACOS_TOKEN (M13-M14).
#
# A criterion that cannot fail proves nothing. This script mutates the code the
# warning depends on, one mutation at a time, and requires that a NAMED
# criterion goes red for each one. A mutation nobody catches is reported as a
# survival and fails the run — an empty or mis-targeted mutation set is worse
# than no mutation set at all.
#
# Usage:
#   bash scripts/mutation-check-sdk-bridge-warning.sh
#   SELF_TEST=1 bash scripts/mutation-check-sdk-bridge-warning.sh   # prove the
#                                                                   # harness can fail
#
# Needs a Go toolchain and perl. The sdk/registry package is standard-library
# only, so this runs offline:
#   docker run --rm -v "$PWD:/src" -w /src golang:1.27.2 \
#     bash scripts/mutation-check-sdk-bridge-warning.sh
#
# Deliberately NOT wired into `make test`: this is a checker of the checker,
# not part of the package's own test run.

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO="${GO:-go}"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/goacos-bridge-mut.XXXXXX")"

cleanup() {
  if [ -d "$WORK" ]; then
    find "$WORK" -mindepth 1 -delete 2>/dev/null
    rmdir "$WORK" 2>/dev/null
  fi
}
trap cleanup EXIT

if ! command -v "$GO" >/dev/null 2>&1; then
  cat >&2 <<'EOF'
FATAL: no Go toolchain found (looked for '$GO').
Run this inside the golang image, for example:
  docker run --rm -v "$PWD:/src" -w /src golang:1.27.2 \
    bash scripts/mutation-check-sdk-bridge-warning.sh
EOF
  exit 2
fi

if ! command -v perl >/dev/null 2>&1; then
  echo "FATAL: perl is required for the multiline edits." >&2
  exit 2
fi

# ---------------------------------------------------------------- fixture ---
mkdir -p "$WORK/sdk/registry"
cp "$ROOT/go.mod" "$WORK/go.mod"
[ -f "$ROOT/go.sum" ] && cp "$ROOT/go.sum" "$WORK/go.sum"
cp "$ROOT"/sdk/registry/*.go "$WORK/sdk/registry/"
cp "$WORK/sdk/registry/client.go" "$WORK/client.go.pristine"
cp "$WORK/sdk/registry/service.go" "$WORK/service.go.pristine"

TOTAL=0
FAILED=0
CAUGHT=0

# run_mutation <id> <file> <description> <expected-test> <perl-expression>
#
# Verifies three things, in this order, and reports which one failed:
#   1. the mutation actually changed the source (otherwise the "catch" would
#      be an accident of an unrelated failure);
#   2. the named test went red;
#   3. the named test — not some other test — is what went red.
#
# The delimiter of the perl expression is NOT '{...}': perl counts nested
# braces there, so a pattern that contains a Go slice literal swallows its own
# closing delimiter and silently stops matching. '!' is used instead.
run_mutation() {
  local id="$1" file="$2" desc="$3" expect="$4" expr="$5"
  local out rc
  local src="$WORK/sdk/registry/$file"
  local pristine="$WORK/$file.pristine"
  TOTAL=$((TOTAL + 1))

  # Reset BOTH mutated files, not just the one this mutation targets.
  #
  # A mutation that is not undone leaks into the next run: the second variant
  # of this harness restored only the file it was about to touch, so M9's
  # change to client.go was still in place while M10 ran against
  # service.go — M10 then "passed" for M9's reason, and the equivalent-
  # mutation check below reported a false alarm. A mutation check that cannot
  # tell whose change it is measuring measures nothing.
  cp "$WORK/client.go.pristine" "$WORK/sdk/registry/client.go"
  cp "$WORK/service.go.pristine" "$WORK/sdk/registry/service.go"

  cp "$pristine" "$src"
  if ! perl -0pi -e "$expr" "$src"; then
    printf '  [%-4s] ERROR    perl failed\n' "$id"
    FAILED=$((FAILED + 1))
    return 1
  fi
  if cmp -s "$src" "$pristine"; then
    printf '  [%-4s] ERROR    mutation did not change the source — the run would count a no-op\n' "$id"
    FAILED=$((FAILED + 1))
    return 1
  fi

  out="$(cd "$WORK" && "$GO" test ./sdk/registry/... -count=1 -run "^${expect}\$" 2>&1)"
  rc=$?
  if [ "$rc" -eq 0 ]; then
    printf '  [%-4s] SURVIVED %s\n            (expected %s to go red)\n' "$id" "$desc" "$expect"
    FAILED=$((FAILED + 1))
    return 1
  fi
  if ! printf '%s\n' "$out" | grep -q -- "--- FAIL: ${expect}"; then
    printf '  [%-4s] WRONG    %s\n            (expected %s to go red; something else did)\n' "$id" "$desc" "$expect"
    printf '%s\n' "$out" | sed 's/^/            /' | head -20
    FAILED=$((FAILED + 1))
    return 1
  fi

  printf '  [%-4s] caught   %s\n            (by %s)\n' "$id" "$desc" "$expect"
  CAUGHT=$((CAUGHT + 1))
  return 0
}

echo "== mutation check: sdk/registry registration surface (bridge warning + credential env) =="
echo "   workdir: $WORK"
echo

# ------------------------------------------------------------- mutations ---
# --- what qualifies as a container-local address -------------------------
# M1: the container-local predicate stops recognising private addresses.
run_mutation M1 client.go \
  "isContainerLocalAddr never matches (podman/CNI addresses stop warning)" \
  TestContainerBridgeWarningFiresForPodmanCNIPoolAddresses \
  's!return a\.IsPrivate\(\)!return false!'

# M2: the guard that keeps the overlay range out of the container-local set
# stops excluding it, so a mesh address starts warning.
run_mutation M2 client.go \
  "the overlay range is no longer excluded from the container-local set" \
  TestContainerBridgeWarningNeverFiresForMeshAddress \
  's!\tif meshPrefix\.Contains\(a\) \{\n\t\treturn false\n\t\}!\tif meshPrefix.Contains(a) {\n\t\treturn true\n\t}!'

# M3: the cgroup marker list loses every runtime but Kubernetes's.
run_mutation M3 client.go \
  "the cgroup marker list keeps only kubepods" \
  TestLooksLikeContainerCgroup \
  's!"kubepods", "containerd", "libpod", "lxc", "docker/", "docker-"!"kubepods"!'

# M4: the podman marker file is no longer probed.
run_mutation M4 client.go \
  "the podman marker file /run/.containerenv is not probed" \
  TestContainerMarkerPathsCoverBothRuntimes \
  's!"/\.dockerenv", "/run/\.containerenv"!"/.dockerenv"!'

# M5: the warning no longer asks whether it is inside a container, so every
# private address warns.
run_mutation M5 client.go \
  "the warning drops the container-context check" \
  TestContainerBridgeWarningSilentOutsideContainer \
  's!case containerNetns\(\) && isContainerLocalAddr\(addr\):!case isContainerLocalAddr(addr):!'

# M6: Docker's pool grows downwards and swallows real LAN space.
run_mutation M6 client.go \
  "Docker's pool starts at 172.16.0.0 instead of 172.17.0.0" \
  TestDockerPoolExcludes17216 \
  's!\[4\]byte\{172, 17, 0, 0\}![4]byte{172, 16, 0, 0}!'

# M7: the explicit-address guard is inverted, so a configured address stops
# being respected.
run_mutation M7 client.go \
  "the explicit-address guard is inverted" \
  TestContainerBridgeWarningNeverFiresForExplicitIP \
  's{c\.cfg\.IP != ""}{c.cfg.IP == ""}'

# M8: the cgroup fallback of the container probe is gone.
run_mutation M8 client.go \
  "detectContainerNetns drops the cgroup fallback" \
  TestDetectContainerNetns \
  's!\n\tif c, err := containerCgroupPath\(\); err == nil && looksLikeContainerCgroup\(c\) \{\n\t\treturn true\n\t\}\n!\n!'

# --- whether the warning is reachable at all ---------------------------
# M9: Register stops calling the warning. This is the mutation that describes
# the defect the warning had: it lived in AdvertiseIP, which Register never
# calls, so no registration could ever raise it.
run_mutation M9 client.go \
  "Register no longer checks the address it publishes" \
  TestRegisterWarnsWhenTheAdvertisedAddressWasAutoDetected \
  's!\tc\.warnContainerBridgeOnce\(c\.cfg\.IP\)\n!!'

# M10: the provenance of the address is lost, so nobody can tell a chosen
# address from a detected one.
run_mutation M10 service.go \
  "ConfigFromEnv stops recording that the address was detected" \
  TestAnnounceWarnsWhenTheAddressWasAutoDetected \
  's!\n\t\t\tcfg\.IPAutoDetected = true!!g'

# M11: the "did anybody choose it" check is inverted, so an operator-chosen
# address is second-guessed.
run_mutation M11 client.go \
  "the chosen-versus-detected check is inverted" \
  TestRegisterDoesNotWarnForAnOperatorChosenAddress \
  's{c\.cfg\.IP != "" && !c\.cfg\.IPAutoDetected}{c.cfg.IP == ""}'

# M12: the exported helper stops saying anything.
run_mutation M12 client.go \
  "ContainerLocalWarning never says anything" \
  TestContainerLocalWarningBoundaries \
  's!func ContainerLocalWarning\(ip string\) string \{\n\taddr, ok := netip\.AddrFromSlice\(net\.ParseIP\(ip\)\.To4\(\)\)!func ContainerLocalWarning(ip string) string {\n\taddr, ok := netip.AddrFromSlice(net.ParseIP(ip).To4())\n\tif ok {\n\t\treturn ""\n\t}!'

# M13: the pre-issued token stops being read from the environment. The SDK is
# then the only one of the three clients of this registry that cannot take a
# token from the environment — the fleet ends up on shared passwords.
run_mutation M13 service.go \
  "ConfigFromEnv stops reading the pre-issued token" \
  TestConfigFromEnvReadsPreissuedToken \
  's!\t\tToken:\s+strings\.TrimSpace\(getenv\(EnvToken\)\),\n!!'

# M14: the token is read but not trimmed. A secret read from a file or a
# heredoc routinely carries a trailing newline, and a newline inside a Bearer
# header fails authentication in a way that looks like a bad token.
run_mutation M14 service.go \
  "the pre-issued token is no longer trimmed" \
  TestConfigFromEnvReadsPreissuedToken \
  's!strings\.TrimSpace\(getenv\(EnvToken\)\)!getenv(EnvToken)!'

# M15: the cluster name stops being read from the environment. The SDK is then
# the only one of the four clients of this registry that cannot say which
# cluster an instance belongs to, so every service lands in "DEFAULT" and the
# registry loses its cluster dimension.
run_mutation M15 service.go \
  "ConfigFromEnv stops reading the cluster name" \
  TestConfigFromEnvReadsClusterName \
  's!\t\tClusterName:\s+strings\.TrimSpace\(getenv\(EnvCluster\)\),\n!!'

# M16: the cluster name is read but not trimmed.
run_mutation M16 service.go \
  "the cluster name is no longer trimmed" \
  TestConfigFromEnvReadsClusterName \
  's!strings\.TrimSpace\(getenv\(EnvCluster\)\)!getenv(EnvCluster)!'

# M17: the group name stops being read from the environment.
run_mutation M17 service.go \
  "ConfigFromEnv stops reading the group name" \
  TestConfigFromEnvReadsGroupName \
  's!\t\tGroupName:\s+strings\.TrimSpace\(getenv\(EnvGroup\)\),\n!!'

# M18: the operator's metadata is ignored — the service's own map is used as
# it is. The metadata is what tells a registry which instance is which tier,
# so dropping it is silent: the instance registers and looks fine.
run_mutation M18 service.go \
  "the operator's GOACOS_METADATA is ignored" \
  TestConfigFromEnvParsesFleetMetadataFormat \
  's!mergeMetadata\(svc\.Metadata, getenv\(EnvMetadata\)\)!mergeMetadata(svc.Metadata, "")!'

# M19: the precedence is inverted, so the service's own metadata wins over the
# operator's on a key collision.
run_mutation M19 service.go \
  "the operator's metadata no longer overrides the service's own" \
  TestConfigFromEnvParsesFleetMetadataFormat \
  's@out\[k\] = strings\.TrimSpace\(v\)@if _, dup := out[k]; !dup { out[k] = strings.TrimSpace(v) }@'

echo
echo "== equivalent mutation (recorded, not counted as a survival) =="
# Dropping the explicit overlay guard inside isContainerLocalAddr changes
# nothing: RFC1918 already excludes 100.64.0.0/10, so the guard is
# belt-and-braces on top of a predicate that says the same thing. It is
# recorded here rather than hidden, because an equivalent mutation is a fact
# about the code, not a defect.
cp "$WORK/client.go.pristine" "$WORK/sdk/registry/client.go"
cp "$WORK/service.go.pristine" "$WORK/sdk/registry/service.go"
if perl -0pi -e 's!\n\tif meshPrefix\.Contains\(a\) \{\n\t\treturn false\n\t\}\n!\n!' "$WORK/sdk/registry/client.go" \
   && ! cmp -s "$WORK/sdk/registry/client.go" "$WORK/client.go.pristine"; then
  eq_out="$(cd "$WORK" && "$GO" test ./sdk/registry/... -count=1 2>&1)"
  eq_rc=$?
  if [ "$eq_rc" -eq 0 ]; then
    echo "  [EQ-1] equivalent: removing the redundant overlay guard changes no behaviour"
  else
    echo "  [EQ-1] NOT equivalent — the overlay guard IS load-bearing:"
    printf '%s\n' "$eq_out" | sed 's/^/            /' | head -20
  fi
else
  echo "  [EQ-1] ERROR: could not apply the equivalent mutation"
fi

echo
if [ "${SELF_TEST:-0}" = "1" ]; then
  # Reverse guard for the harness itself: aim a mutation at a test it cannot
  # affect. A harness that cannot report a survival would report every
  # mutation as "caught" and prove nothing.
  echo "== self-test: the harness must be able to report a survival =="
  saved_total=$TOTAL
  saved_failed=$FAILED
  saved_caught=$CAUGHT
  if run_mutation SELF client.go \
    "self-test: a mutation aimed at a test it cannot affect" \
    TestSelectAdvertiseIPPrefersOverlayOverLAN \
    's!return a\.IsPrivate\(\)!return false!'; then
    echo "  [self] BROKEN: the harness reported a catch for a mutation nobody caught"
    TOTAL=$saved_total
    FAILED=$saved_failed
    CAUGHT=$saved_caught
    exit 1
  else
    echo "  [self] OK: the harness reported a survival for a mutation nobody caught"
    TOTAL=$saved_total
    FAILED=$saved_failed
    CAUGHT=$saved_caught
  fi
  echo
fi

echo "== summary =="
echo "  mutations: $TOTAL   caught: $CAUGHT   failed: $FAILED"
if [ "$FAILED" -ne 0 ]; then
  echo "  RESULT: FAIL — a mutation was not caught by its named criterion"
  exit 1
fi
echo "  RESULT: PASS — every mutation was caught by its named criterion"
