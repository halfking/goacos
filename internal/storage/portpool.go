package storage

// Port homogeneity for a component's candidate set.
//
// # Why this file exists
//
// A naming service is keyed by (namespace, group, serviceName, cluster) and
// each instance carries an (ip, port) pair. Nothing in the Nacos model says
// the instances of one service share a port, so a registration mistake — two
// different services sharing one name — reaches the registry as a single
// service holding two port groups.
//
// The gateway then builds its candidate set from that name alone. Round robin
// picks an instance out of the mixed set and dials *its* port. When the mixed
// set holds a web port and an API port, part of the traffic is sent to the
// wrong service. The request is answered by something that is up, so no health
// check ever fires and the failure is indistinguishable from an application
// bug.
//
// The fix has two halves, and this file is the second one:
//
//   - the registration plane must stop sharing one name across two services.
//     That is a discipline problem, not a code problem.
//   - until it does, the gateway must be able to *say* that it happened, and
//     must be able to route around it on request.
//
// # The rule
//
//	every instance on one port       → use them (the normal case)
//	component declares servicePort   → use that port's instances only
//	otherwise                        → use them all, and report the spread
//
// The last line is where this differs from a "fail closed" design, and the
// difference is deliberate. The naming model stores a port per instance
// precisely so that an instance can be dialled at the port it actually
// listens on; a registry that refuses to serve a service whose replicas do
// not agree on a port would break that model and turn a reporting defect
// into an outage.
//
// So the gateway keeps the model's semantics by default and adds two things
// it did not have: a way for a component to say which port it means, and a
// way for an operator to see that the instances of one name disagree.
//
// # servicePort is a contract, not an instance pin
//
// The declared port is the port the *service* is contracted to listen on. It
// says nothing about which host answers: every instance in the selected pool
// still supplies its own address from live registry state. That distinction is
// what keeps this compatible with "never hard-code an instance address" — a
// declaration pins a contract, not a machine.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ServicePortMetaKey is the component metadata key that declares the port the
// component's service is contracted to listen on.
//
// It is metadata rather than a column because the application model
// deliberately holds no instance address of any kind: the IP part comes from
// live instance state and the policy part is reviewable data. A port is a
// property of the service contract, so it belongs with the other policy.
const ServicePortMetaKey = "servicePort"

// PortConflict describes a candidate set whose instances did not all agree on
// a port.
//
// It is reported rather than swallowed so the misregistration is visible
// before it becomes an outage: the gateway status lists it next to the
// component it affects.
type PortConflict struct {
	// ServiceName is the naming service whose instances conflicted.
	ServiceName string
	// Ports is every port that carried a healthy instance, ascending.
	Ports []uint32
	// Declared is the port the component asked for, 0 when it asked for none.
	Declared uint32
	// Chosen is the port whose instances were selected, 0 when the pool was
	// left unnarrowed because the component declared nothing.
	Chosen uint32
	// Excluded counts healthy instances left out of the chosen pool.
	Excluded int
}

// Reason renders the conflict as one operator-readable sentence.
//
// It is written to be read in a status listing, so it says what happened and
// what to do rather than only what the data was.
func (c *PortConflict) Reason() string {
	if c == nil {
		return ""
	}
	ports := make([]string, 0, len(c.Ports))
	for _, p := range c.Ports {
		ports = append(ports, strconv.Itoa(int(p)))
	}
	if c.Declared != 0 {
		return fmt.Sprintf(
			"service %q has healthy instances on ports %s, but the component declares %s; "+
				"only the instances on %s are eligible and the other %d are excluded. "+
				"Give each service its own name, or correct the component's %s metadata",
			c.ServiceName, strings.Join(ports, ", "), strconv.Itoa(int(c.Declared)),
			strconv.Itoa(int(c.Chosen)), c.Excluded, ServicePortMetaKey)
	}
	return fmt.Sprintf(
		"service %q has healthy instances on more than one port (%s), so the pool is not "+
			"homogeneous: every instance is eligible and each is dialled at its own port. "+
			"Give each service its own name, or declare the component's %s metadata to pin it",
		c.ServiceName, strings.Join(ports, ", "), ServicePortMetaKey)
}

// DeclaredServicePort reads the optional servicePort component metadata.
//
// It returns 0 when the component declared none. A value that is not a valid
// port is treated as "declared none" rather than as an error: the write path
// rejects such values, and a reader that could fail a request over a typo in
// a row somebody edited by hand would turn a cosmetic mistake into an outage.
func DeclaredServicePort(meta map[string]string) uint32 {
	if meta == nil {
		return 0
	}
	raw, ok := meta[ServicePortMetaKey]
	if !ok {
		return 0
	}
	port, err := ParseServicePort(raw)
	if err != nil {
		return 0
	}
	return port
}

// ParseServicePort validates a declared service port.
//
// The value must be a plain decimal port number. It is parsed strictly on
// purpose: a port that is silently coerced into a different number would send
// traffic somewhere the operator never approved.
func ParseServicePort(raw string) (uint32, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, fmt.Errorf("%s must not be empty", ServicePortMetaKey)
	}
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s %q is not a port number", ServicePortMetaKey, raw)
	}
	if n == 0 || n > 65535 {
		return 0, fmt.Errorf("%s %q is not a valid port (1-65535)", ServicePortMetaKey, raw)
	}
	return uint32(n), nil
}

// SelectPortPool narrows a candidate set to a single port.
//
// declared is the port the component declared (0 = none). The returned
// conflict is nil when the candidate set was already homogeneous, which is the
// case for every correctly registered service.
//
// When declared is set, only the instances on that port are returned and the
// rest are excluded and counted. When declared is zero, every instance is
// returned — each one is dialled at its own port, which is what the naming
// model means — and the conflict records that the pool was not homogeneous.
func SelectPortPool(instances []Instance, declared uint32) ([]Instance, *PortConflict) {
	if len(instances) == 0 {
		return instances, nil
	}

	byPort := map[uint32][]Instance{}
	ports := make([]uint32, 0, len(instances))
	for _, inst := range instances {
		if _, seen := byPort[inst.Port]; !seen {
			ports = append(ports, inst.Port)
		}
		byPort[inst.Port] = append(byPort[inst.Port], inst)
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })

	serviceName := instances[0].ServiceName

	if declared != 0 {
		pool := byPort[declared]
		if len(ports) == 1 && ports[0] == declared {
			return pool, nil
		}
		return pool, &PortConflict{
			ServiceName: serviceName,
			Ports:       ports,
			Declared:    declared,
			Chosen:      declared,
			Excluded:    len(instances) - len(pool),
		}
	}

	if len(ports) == 1 {
		return instances, nil
	}

	// No declaration and more than one port. Every instance stays eligible —
	// each is dialled at its own port — but the spread is reported, because
	// "the instances of one name do not agree on a port" is the signature of
	// two services sharing a name, and it is invisible otherwise.
	return instances, &PortConflict{
		ServiceName: serviceName,
		Ports:       ports,
	}
}
