package envbool

import (
	"testing"
)

// The spellings below are not arbitrary. `true` is the one the registrar's own
// error message tells the operator to write, so a regression on that single
// value breaks the documented recovery path — which is how the original bug
// survived: every test in the repo used "1".
func TestParseAcceptsDocumentedSpellings(t *testing.T) {
	truthy := []string{"1", "t", "true", "TRUE", "True", "yes", "YES", "y", "on", "ON", "  true  "}
	for _, spelling := range truthy {
		got, err := Parse(func(string) string { return spelling }, "FLAG", false)
		if err != nil {
			t.Errorf("%q: unexpected error %v", spelling, err)
			continue
		}
		if !got {
			t.Errorf("%q should be truthy, got false", spelling)
		}
	}

	falsy := []string{"0", "f", "false", "FALSE", "False", "no", "n", "off", "OFF"}
	for _, spelling := range falsy {
		got, err := Parse(func(string) string { return spelling }, "FLAG", true)
		if err != nil {
			t.Errorf("%q: unexpected error %v", spelling, err)
			continue
		}
		if got {
			t.Errorf("%q should be falsy, got true", spelling)
		}
	}
}

// The exact value the registrar's refusal message names. If this one case
// regresses, the tool tells the operator to do something that does not work.
func TestParseAcceptsTheValueTheRegistrarErrorMessageNames(t *testing.T) {
	const named = "GOACOS_ALLOW_LOOPBACK"
	got, err := Parse(func(key string) string {
		if key == named {
			return "true"
		}
		return ""
	}, named, false)
	if err != nil {
		t.Fatalf("the documented recovery value must parse: %v", err)
	}
	if !got {
		t.Fatal(`"true" must enable the flag; the registrar's own error message says to set it`)
	}
}

func TestUnsetReturnsFallback(t *testing.T) {
	for _, fallback := range []bool{true, false} {
		got, err := Parse(func(string) string { return "" }, "FLAG", fallback)
		if err != nil {
			t.Fatalf("unset must not error: %v", err)
		}
		if got != fallback {
			t.Errorf("unset → %v, want fallback %v", got, fallback)
		}
	}
}

func TestBlankIsTreatedAsUnset(t *testing.T) {
	got, err := Parse(func(string) string { return "   " }, "FLAG", true)
	if err != nil {
		t.Fatalf("blank must not error: %v", err)
	}
	if !got {
		t.Error("blank should fall back to true")
	}
}

// A typo must be loud. Returning the fallback here is how "GOACOS_ENABLE=true"
// silently turns a feature off.
func TestMalformedIsAnError(t *testing.T) {
	for _, bad := range []string{"maybe", "tru", "2", "--true", "enabled"} {
		if _, err := Parse(func(string) string { return bad }, "FLAG", false); err == nil {
			t.Errorf("%q should be an error, got nil", bad)
		}
	}
}

func TestErrorNamesKeyAndValue(t *testing.T) {
	_, err := Parse(func(string) string { return "maybe" }, "GOACOS_ALLOW_LOOPBACK", false)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"GOACOS_ALLOW_LOOPBACK", "maybe"} {
		if !contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestMustParseFallsBackOnMalformed(t *testing.T) {
	if !MustParse(func(string) string { return "maybe" }, "FLAG", true) {
		t.Error("MustParse should return the fallback on a malformed value")
	}
	if !MustParse(func(string) string { return "true" }, "FLAG", false) {
		t.Error("MustParse should still honour a valid value")
	}
	if MustParse(func(string) string { return "false" }, "FLAG", true) {
		t.Error("MustParse should honour an explicit false over the fallback")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) &&
		(haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
