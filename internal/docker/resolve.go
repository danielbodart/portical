package docker

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/danielbodart/portical/internal/model"
	"github.com/danielbodart/portical/internal/rules"
	"github.com/danielbodart/portical/internal/source"
)

// ForwardLabel is the label a container's forwards are read from.
const ForwardLabel = "portical.upnp.forward"

// NetworkLabel names which network to forward to, for containers attached to
// several.
//
// Portical can usually pick on its own, but "usually" is not good enough when
// the wrong choice sends traffic to an address the gateway cannot reach, so
// there has to be a way to say so explicitly.
const NetworkLabel = "portical.upnp.network"

// Options shape how containers become forwards.
type Options struct {
	// The label holding the forwards; ForwardLabel when "".
	Label string
	// The label naming the network; NetworkLabel when "".
	NetworkLabel string
	// The Docker host's address on the LAN, as found by talking to the
	// gateway.
	//
	// The target for bridge and host networking. "" when it is not known,
	// because an unknown address is better than a guessed one - see
	// model.DesiredForward.InternalClient.
	HostAddress string
}

func (o Options) withDefaults() Options {
	o.Label = cmp.Or(o.Label, ForwardLabel)
	o.NetworkLabel = cmp.Or(o.NetworkLabel, NetworkLabel)
	return o
}

// preference is which network driver to prefer when a container is on more
// than one.
//
// macvlan and ipvlan come first because they give the container its own
// address on the LAN, which the gateway can forward to directly. Bridge is
// last: it works, but only via a second hop through the host's published
// ports, so it is the fallback rather than the choice.
var preference = []string{"macvlan", "ipvlan", "host", "bridge"}

// Resolve turns running containers into the forwards that ought to exist.
func Resolve(containers []Container, options Options) source.Desired {
	options = options.withDefaults()
	var desired source.Desired

	for _, container := range containers {
		value, ok := container.Labels[options.Label]
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}

		terms := rules.Parse(value)
		for _, err := range terms.Errors {
			desired.Warnings = append(desired.Warnings, container.Name+": "+err)
		}

		network, warnings, err := selectNetwork(container, container.Labels[options.NetworkLabel])
		if err != "" {
			desired.Warnings = append(desired.Warnings, container.Name+": "+err)
			continue
		}
		for _, warning := range warnings {
			desired.Warnings = append(desired.Warnings, container.Name+": "+warning)
		}

		target, internalClient := describeTarget(network, options.HostAddress)

		// Explicit rules come first so that they win any collision with a rule
		// derived from `published`, which is the more specific intent.
		found := terms.Rules
		if terms.Published {
			found = append(slices.Clone(found), fromPublished(container)...)
		}
		for _, rule := range rules.Collapse(found) {
			desired.Forwards = append(desired.Forwards, model.DesiredForward{
				Rule: rule, Owner: container.Name, Target: target, InternalClient: internalClient,
			})
		}
	}
	return desired
}

// selectNetwork chooses which of a container's networks the forward should
// point at, or says why none will do.
//
// Issue #1 is here. v1 read the network with the Go template
// `{{range $key, $_ := .NetworkSettings.Networks}}{{ $key }}{{end}}`, which
// concatenates every network name with no separator - a container on four
// networks yielded one nonsense name like "networkanetworkbnetworkc". The
// lookup then failed, the driver came back empty, and every rule was skipped
// as "Unsupported network driver: ". Reported by two people, and it makes
// Portical unusable with exactly the reverse-proxy setups it is most useful
// for.
func selectNetwork(container Container, requested string) (chosen Network, warnings []string, refusal string) {
	if len(container.Networks) == 0 {
		return Network{}, nil, "is not attached to any network, skipping"
	}

	if requested = strings.TrimSpace(requested); requested != "" {
		index := slices.IndexFunc(container.Networks, func(network Network) bool { return network.Name == requested })
		if index < 0 {
			return Network{}, nil, fmt.Sprintf("is not attached to network '%s' (has: %s), skipping",
				requested, strings.Join(names(container.Networks), ", "))
		}
		named := container.Networks[index]
		if !slices.Contains(preference, named.Driver) {
			return Network{}, nil, fmt.Sprintf("network '%s' has unsupported driver '%s', skipping", named.Name, named.Driver)
		}
		return named, nil, ""
	}

	var supported []Network
	for _, network := range container.Networks {
		if slices.Contains(preference, network.Driver) {
			supported = append(supported, network)
		}
	}
	if len(supported) == 0 {
		var drivers []string
		for _, network := range container.Networks {
			if !slices.Contains(drivers, network.Driver) {
				drivers = append(drivers, network.Driver)
			}
		}
		return Network{}, nil, fmt.Sprintf("has no supported network driver (found: %s), skipping", strings.Join(drivers, ", "))
	}
	// Sorted by name within a driver so that a container on two macvlans
	// resolves the same way on every pass. An unstable choice would flip the
	// target address back and forth and rewrite the rule forever.
	slices.SortStableFunc(supported, func(a, b Network) int {
		return cmp.Or(
			cmp.Compare(slices.Index(preference, a.Driver), slices.Index(preference, b.Driver)),
			strings.Compare(a.Name, b.Name))
	})

	best := supported[0]
	var rivals []Network
	for _, network := range supported {
		if network.Driver == best.Driver {
			rivals = append(rivals, network)
		}
	}
	if len(rivals) > 1 {
		warnings = append(warnings, fmt.Sprintf("is on %d %s networks (%s), forwarding to '%s' - set %s to choose",
			len(rivals), best.Driver, strings.Join(names(rivals), ", "), best.Name, NetworkLabel))
	}
	return best, warnings, ""
}

func names(networks []Network) []string {
	var names []string
	for _, network := range networks {
		names = append(names, network.Name)
	}
	return names
}

func describeTarget(network Network, hostAddress string) (model.Target, string) {
	// macvlan and ipvlan put the container on the LAN in its own right, so the
	// gateway forwards straight to it and we know the address from Docker.
	if network.Driver == "macvlan" || network.Driver == "ipvlan" {
		return model.Container(network.Name), network.IPAddress
	}
	// bridge and host both land on the Docker host, and Docker forwards on from
	// there. The container's own bridge address is not routable from the
	// gateway, so it deliberately plays no part here.
	return model.Host, hostAddress
}

// fromPublished is the rules implied by `published`.
//
// External and internal port are both the *host* port, matching v1. The
// gateway forwards to the host, and Docker's own published-port mapping takes
// it the rest of the way to the container - so the container port is Docker's
// business, not the router's.
func fromPublished(container Container) []model.Rule {
	var found []model.Rule
	for _, port := range container.Published {
		found = append(found, model.Rule{ExternalPort: port.HostPort, InternalPort: port.HostPort, Protocol: port.Protocol})
	}
	return found
}
