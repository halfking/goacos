package api

import (
	"testing"

	"github.com/halfking/goacos/internal/storage"
)

// A static frontend is served straight off disk. Nothing in its delivery path
// consults the naming service: staticRoot keys the directory off app+component
// alone, and ServeStatic answers the request from disk.
//
// Requiring a serviceName on it anyway was a contract bug, not a safety rail.
// It forced a placeholder service name into a component that does not need
// instance-backed delivery. This does not imply a naming_service record was
// created by that declaration.
//
// The obligation is per-protocol: http/https/grpc/ws are backed by registered
// instances and must name one; static is not.

func TestStaticComponentNeedsNoServiceName(t *testing.T) {
	if componentNeedsInstances(storage.ProtocolStatic) {
		t.Error("static is served from disk; it must not require a placeholder service name")
	}
}

func TestInstanceBackedProtocolsStillNeedAServiceName(t *testing.T) {
	for _, protocol := range []string{
		storage.ProtocolHTTP, storage.ProtocolHTTPS,
		storage.ProtocolGRPC, storage.ProtocolWS,
	} {
		if !componentNeedsInstances(protocol) {
			t.Errorf("%s is forwarded to registered instances; it must name a service", protocol)
		}
	}
}

// An absent protocol is the default, which is http. Treating it as
// instance-backed keeps the check fail-closed: a typo like "stattic" must not
// quietly create a component that nothing can ever serve.
func TestUnknownOrAbsentProtocolFailsClosed(t *testing.T) {
	for _, protocol := range []string{"", "   ", "stattic", "http2", "STATIC_TYPO"} {
		if !componentNeedsInstances(protocol) {
			t.Errorf("%q should still require a service name — "+
				"a typo must not produce a component nothing can serve", protocol)
		}
	}
}

// Operators type these by hand in a declaration file, so casing and stray
// whitespace cannot decide whether the field is required.
func TestProtocolMatchIgnoresCaseAndPadding(t *testing.T) {
	for _, protocol := range []string{"STATIC", " Static", "static "} {
		if componentNeedsInstances(protocol) {
			t.Errorf("%q should be recognised as static", protocol)
		}
	}
	for _, protocol := range []string{"HTTP", " HTTP", "Http"} {
		if !componentNeedsInstances(protocol) {
			t.Errorf("%q should be recognised as an instance-backed protocol", protocol)
		}
	}
}

// The whole point of the exemption is that a static component can be
// declared with no service at all. If normalisation still injected one, the
// exemption would buy nothing.
func TestStaticComponentNormalisesWithoutAServiceName(t *testing.T) {
	spec := componentSpec{
		AppName:  "shop",
		Name:     "web",
		Kind:     storage.KindWeb,
		Protocol: storage.ProtocolStatic,
	}
	if err := normalizeComponentSpec(&spec); err != nil {
		t.Fatalf("a static component with no serviceName must normalise: %v", err)
	}
	if spec.ServiceName != "" {
		t.Errorf("normalisation invented a service name %q; "+
			"the exemption would then buy nothing", spec.ServiceName)
	}
}

// ...and the exemption must not leak sideways: an http component that names
// no service is still rejected by normalisation's own rules.
func TestInstanceBackedComponentStillRejectedWithoutServiceName(t *testing.T) {
	for _, protocol := range []string{"", "   ", "http", "https", "grpc", "ws", "stattic", " HTTP "} {
		t.Run(protocol, func(t *testing.T) {
			spec := componentSpec{AppName: "shop", Name: "api", Protocol: protocol}
			if err := normalizeComponentSpec(&spec); err == nil {
				t.Error("an instance-backed component with no serviceName must be rejected")
			}
			spec.ServiceName = "api-service"
			if err := normalizeComponentSpec(&spec); err != nil {
				t.Fatalf("the same protocol with a serviceName must retain its existing acceptance: %v", err)
			}
		})
	}
}

func TestStaticProtocolCanonicalisedBeforeStorage(t *testing.T) {
	for _, protocol := range []string{"STATIC", " Static", "static "} {
		t.Run(protocol, func(t *testing.T) {
			spec := componentSpec{AppName: "shop", Name: "web", Protocol: protocol}
			if err := normalizeComponentSpec(&spec); err != nil {
				t.Fatalf("static spelling must be accepted: %v", err)
			}
			if spec.Protocol != storage.ProtocolStatic {
				t.Fatalf("gateway requires canonical static, got %q", spec.Protocol)
			}
			if spec.ServiceName != "" {
				t.Fatal("static must not invent a service name")
			}
		})
	}
}
