// Package envbool parses boolean-valued environment variables.
//
// # Why this package exists
//
// The repository had three separate implementations of the same question, and
// they did not agree:
//
//	sdk/registry.envBool          1 t true yes y on   / 0 f false no n off
//	internal/config.getenvBool    1 t TRUE true True … / 0 f FALSE false …
//	cmd/goacos-registrar.envBool  1
//
// The registrar's was the damaging one, because the error it prints when it
// refuses a loopback address says:
//
//	Set GOACOS_ALLOW_LOOPBACK=true only if this really is a single-host deployment
//
// …and `true` was precisely the spelling that implementation rejected. An
// operator following the tool's own advice got the identical error again,
// with no hint that the advice was the problem.
//
// The consequence of the split was not cosmetic: `-allow-loopback` is how a
// legitimate single-host deployment is allowed to advertise 127.0.0.1, so
// "set the documented env var" simply did not work.
//
// `sdk/registry` deliberately keeps its own copy — the SDK is meant to be
// self-contained, and that copy was already the most complete of the three.
// Everything inside this module shares this one.
package envbool

import (
	"fmt"
	"strings"
)

// Parse reads key through getenv and interprets it as a boolean.
//
// Truthy, case-insensitive: 1, t, true, yes, y, on.
// Falsy,  case-insensitive: 0, f, false, no, n, off.
// An unset or blank key returns fallback with no error.
//
// Anything else is an error naming the key and the value. Returning fallback
// silently there would turn a typo into "the feature is off", which is the
// failure mode this package was written to remove.
func Parse(getenv func(string) string, key string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(getenv(key))
	if raw == "" {
		return fallback, nil
	}
	switch strings.ToLower(raw) {
	case "1", "t", "true", "yes", "y", "on":
		return true, nil
	case "0", "f", "false", "no", "n", "off":
		return false, nil
	}
	return false, fmt.Errorf("%s=%q is not a boolean", key, raw)
}

// MustParse is Parse for callers that cannot act on an error: a malformed
// value falls back rather than failing the process. Use it only where the
// surrounding code already degrades safely — the registrar's flag defaults,
// for instance, where refusing to start would be worse than using the
// documented default.
func MustParse(getenv func(string) string, key string, fallback bool) bool {
	value, err := Parse(getenv, key, fallback)
	if err != nil {
		return fallback
	}
	return value
}
