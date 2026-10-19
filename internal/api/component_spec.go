package api

// Declaration normalisation for application components.
//
// # Why this file exists
//
// Component fields used to be stored and echoed without ever being consulted.
// `kind` was the clearest case: storage declared the vocabulary
// (KindWeb/KindAPI/KindWorker/KindMCP/KindGateway) with a comment saying these
// are "the roles a complete frontend+backend application actually needs", and
// then nothing on the request path ever read it back. A frontend and a backend
// were forwarded identically, so "supporting a complete application" was a
// claim the gateway could not honour on its own.
//
// This file is where a declaration stops being prose and becomes behaviour.
// Two things happen here:
//
//   - **kind is validated.** Every one of the five declared kinds is accepted;
//     anything else is a 400. Previously a typo ("frontend", "API", "Web")
//     was stored happily and behaved exactly like the default, which is the
//     worst outcome available: it looks configured and does nothing.
//   - **a frontend that named no load-balancing algorithm gets a safe one.**
//     round_robin is correct for an API and wrong for a frontend, and the
//     difference is invisible until two instances run different builds.
//
// # Explicit intent still wins
//
// defaultLBAlgorithmForKind only decides what happens when lbAlgorithm is
// absent. A component that says round_robin keeps round_robin — this is a
// floor under silence, not an override of a decision. Existing declarations
// are therefore unaffected, and the operator still owns the explicit choice.

import (
	"fmt"
	"slices"
	"strings"

	"github.com/halfking/goacos/internal/storage"
)

// componentSpec is the write shape for one component declaration. It is named
// rather than anonymous so that normalisation can be tested without an HTTP
// server and a storage backend.
type componentSpec struct {
	NamespaceId string            `json:"namespaceId"`
	AppName     string            `json:"appName"`
	Name        string            `json:"name"`
	Kind        string            `json:"kind"`
	Protocol    string            `json:"protocol"`
	ServiceName string            `json:"serviceName"`
	GroupName   string            `json:"groupName"`
	ClusterName string            `json:"clusterName"`
	HealthPath  string            `json:"healthPath"`
	Weight      float64           `json:"weight"`
	LBAlgorithm string            `json:"lbAlgorithm"`
	Sticky      string            `json:"sticky"`
	Metadata    map[string]string `json:"metadata"`
}

// validKinds is the vocabulary storage declares. All five are accepted: they
// are the roles a complete application actually needs, and narrowing the set
// here would reject declarations the platform documents as legitimate.
var validKinds = map[string]bool{
	storage.KindAPI:     true,
	storage.KindWeb:     true,
	storage.KindWorker:  true,
	storage.KindMCP:     true,
	storage.KindGateway: true,
}

// defaultLBAlgorithmForKind returns the algorithm a component gets when its
// declaration named none.
//
// round_robin is a strict global counter: the pointer advances per request and
// its position has nothing to do with the path. That is right for an API, where
// every request stands alone and any healthy instance answers correctly.
//
// It is wrong for a frontend. One page load is a *group* of dependent requests
// — the HTML decides which hashes belong to which build — and an even request
// cadence advances the counter by exactly two, pinning the HTML to one instance
// and the script to another. Every load then mixes two builds. Both instances
// pass a health check on "/", so the failure never surfaces as an unhealthy
// instance; it surfaces as a blank page that gets blamed on the browser.
//
// ip_hash keeps one client on one instance for as long as the instance set is
// unchanged, with no cookie round-trip.
func defaultLBAlgorithmForKind(kind string) string {
	if kind == storage.KindWeb {
		return "ip_hash"
	}
	return "round_robin"
}

// normalizeComponentSpec fills in what the declaration left blank and rejects
// a kind the platform has no behaviour for. Protocol spelling is canonicalised
// before both service-name validation and storage, so the gateway sees the same
// protocol that validation accepted. Unknown protocols retain the existing
// runtime fallback, but never receive the static service-name exemption.
func normalizeComponentSpec(c *componentSpec) error {
	if c.Kind == "" {
		c.Kind = storage.KindAPI
	}
	if !validKinds[c.Kind] {
		names := make([]string, 0, len(validKinds))
		for k := range validKinds {
			names = append(names, k)
		}
		slices.Sort(names)
		return fmt.Errorf("kind %q is not one of %v", c.Kind, names)
	}
	c.Protocol = strings.ToLower(strings.TrimSpace(c.Protocol))
	if c.Protocol == "" {
		c.Protocol = storage.ProtocolHTTP
	}
	if c.GroupName == "" {
		c.GroupName = "DEFAULT_GROUP"
	}
	if c.Weight <= 0 {
		c.Weight = 1
	}
	if c.LBAlgorithm == "" {
		c.LBAlgorithm = defaultLBAlgorithmForKind(c.Kind)
	}
	// Whether a service name is owed depends on the protocol, and the protocol
	// has just been defaulted above — so this check belongs here, with the rest
	// of the declaration rules, rather than in the HTTP handler where it used
	// to sit. componentNeedsInstances is fail-closed over anything that is not
	// exactly "static", so an unknown protocol still lands on the strict side.
	if c.ServiceName == "" && componentNeedsInstances(c.Protocol) {
		return fmt.Errorf("serviceName is required for protocol %q "+
			"(a static component is served from disk and needs none)", c.Protocol)
	}
	return nil
}

// componentNeedsInstances reports whether a component has to be backed by
// registered service instances.
//
// The obligation is per-protocol, not per-component. http, https, grpc and ws
// are forwarded to instances the naming service tracks, so they must name one.
// static is served off disk: staticRoot keys the directory off
// app+component alone, and ServeStatic answers without consulting the naming
// service at all.
//
// Requiring a service name there was a contract bug rather than a safety rail.
// It made static frontends invent a placeholder service name despite having
// no instance-backed delivery path. A component declaration alone does not
// prove that a naming_service record was created.
//
// Failing closed is deliberate: anything that is not exactly "static" is
// treated as instance-backed, so a typo ("stattic") cannot quietly produce a
// component that nothing is able to serve.
//
// With default delivery, a missing directory falls through to Forward, which
// finds no healthy instance for the empty service name and answers 503. Explicit
// metadata delivery=static preserves the existing file-missing 404 path instead.
// Neither mode requires constructing an empty upstream.
func componentNeedsInstances(protocol string) bool {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case storage.ProtocolStatic:
		return false
	default:
		return true
	}
}
