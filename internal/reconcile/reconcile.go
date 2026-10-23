// Package reconcile works out what has to change on the gateway.
//
// Pure, and deliberately so - this is where every interesting decision in
// Portical is made, so it should be testable without a router, a daemon or a
// clock.
package reconcile

import (
	"fmt"
	"strings"
	"time"

	"github.com/danielbodart/portical/internal/model"
)

// Kind is what an Action does.
type Kind int

const (
	Add Kind = iota
	Replace
	// Renew extends a mapping's lease without disturbing it.
	//
	// Kept apart from Replace because how it is carried out matters. A rule
	// that is merely being renewed is rewritten in place with AddPortMapping,
	// never deleted first: the gateway's redirect only governs *new* flows, so
	// removing it briefly is invisible to a connection already established -
	// until the moment someone tries to join, which is when a game server
	// appears to have gone down for no reason.
	Renew
	Remove
	Orphan
	Keep
	Conflict
)

var names = [...]string{"add", "replace", "renew", "remove", "orphan", "keep", "conflict"}

func (k Kind) String() string { return names[k] }

// Action is one thing to do, or deliberately not do, on the gateway.
//
// Forward is set for everything that concerns a forward we want: Add,
// Replace, Renew, Keep and Conflict. Mapping is set for everything that
// concerns a mapping already on the gateway: all but Add.
type Action struct {
	Kind    Kind
	Forward model.DesiredForward
	Mapping model.Mapping
	Reason  string
}

// Options shape a reconcile.
type Options struct {
	// Rewrite every rule even if it already looks correct.
	Force bool
	// Rewrite a mapping whose remaining lease is below this.
	//
	// Portical asks for an unlimited lease, but plenty of routers silently
	// downgrade that to a finite one, and a `listen`-only daemon in v1 had
	// nothing that would ever notice the mapping quietly expiring.
	RenewWithin time.Duration
	// Take over a matching mapping that another tool appears to own.
	//
	// Off by default. Two tools that both believe they own an external port
	// will otherwise overwrite each other forever, and a fight between
	// daemons is far harder to diagnose than a logged conflict.
	Steal bool
	// The addresses this Portical is entitled to forward to.
	//
	// Ownership cannot be decided by the description alone. Every Portical on
	// a network writes the same "portical:" prefix, so a second one - on
	// another host, or run by hand from a laptop to look at something - sees
	// the first's rules as its own, finds nothing asking for them, and
	// deletes them. The two then delete each other's rules forever.
	//
	// Restricting removals to mappings that point somewhere this instance
	// could legitimately have sent traffic makes that impossible. Nil means
	// manage everything, which is the old behaviour and what --manage-all
	// asks for. Empty is not the same: it is an instance that cannot tell
	// where it sends traffic - no host address and nothing forwarded yet -
	// and so manages nothing, rather than reading "none" as "all" and
	// deleting every other Portical's rules.
	ManagedAddresses map[string]bool
}

// Reconcile is the actions that bring actual into line with desired.
//
// The caller must pass a *complete* set of desired forwards. Removals are
// derived from absence, so reconciling against a partial list would read as
// "these containers are gone" and tear down live rules. Anything that fails
// to enumerate what it forwards must abort rather than reconcile with what it
// managed to collect.
func Reconcile(desired []model.DesiredForward, actual []model.Mapping, options Options) []Action {
	// Ours, and pointing somewhere we could legitimately have sent traffic.
	managed := func(mapping model.Mapping) bool {
		return mapping.IsOurs() && (options.ManagedAddresses == nil || options.ManagedAddresses[mapping.InternalClient])
	}

	existing := map[string]model.Mapping{}
	for _, mapping := range actual {
		existing[mapping.Identity()] = mapping
	}
	wanted := map[string]bool{}
	for _, forward := range desired {
		wanted[forward.Rule.Identity()] = true
	}
	actions := []Action{}

	for _, forward := range desired {
		mapping, ok := existing[forward.Rule.Identity()]
		if !ok {
			actions = append(actions, Action{Kind: Add, Forward: forward, Reason: "no mapping on the gateway"})
			continue
		}
		act := func(kind Kind, reason string) {
			actions = append(actions, Action{Kind: kind, Forward: forward, Mapping: mapping, Reason: reason})
		}

		// Another Portical's rule, on another host. Reported rather than taken:
		// silently reassigning it would break whatever is running over there,
		// and the two instances would then fight over the port indefinitely.
		if mapping.IsOurs() && !managed(mapping) {
			act(Conflict, "already forwarded by another Portical to "+mapping.InternalClient)
			continue
		}

		if !mapping.IsOurs() {
			if !options.Steal {
				act(Conflict, fmt.Sprintf("already forwarded by something else (%s)", owner(mapping)))
				continue
			}
			// Rewritten even when it already points the right way, so that our
			// description replaces the other tool's. Ownership is decided by
			// that description, so a mapping left carrying someone else's text
			// is one we would refuse to touch on the next pass and never clean
			// up.
			act(Replace, "taking over from "+owner(mapping))
			continue
		}

		// Drift is the only thing that justifies taking a rule down. Where the
		// mapping already points at the right place, it is rewritten in place
		// - there is nothing to move it away from.
		if drift := mismatch(forward, mapping); drift != "" {
			act(Replace, drift)
			continue
		}

		if options.Force {
			act(Renew, "forced")
			continue
		}

		if expiring(mapping, options.RenewWithin) {
			act(Renew, fmt.Sprintf("lease expires in %ds", mapping.LeaseDuration))
			continue
		}

		act(Keep, "")
	}

	// Anything of ours nothing asks for any more. This is the whole of issue
	// #2 - a stopped container simply stops contributing desired forwards, so
	// its rules fall out here without any need to watch for stop events.
	for _, mapping := range actual {
		if wanted[mapping.Identity()] || !mapping.IsOurs() {
			continue
		}

		if managed(mapping) {
			actions = append(actions, Action{Kind: Remove, Mapping: mapping, Reason: "nothing asks for it any more"})
			continue
		}

		// Ours by description, but pointing somewhere we could not have sent
		// traffic. Either another Portical wrote it, or it belonged to a
		// macvlan container of ours that has since gone and taken its address
		// with it.
		//
		// Reported rather than removed, and rather than passed over in
		// silence. We cannot tell those two cases apart, and deleting on a
		// guess would break another host's forwarding. Left alone, such a rule
		// is reclaimed if the container returns to the same address, and
		// expires on its own if the gateway granted a finite lease - which
		// most do.
		actions = append(actions, Action{Kind: Orphan, Mapping: mapping,
			Reason: fmt.Sprintf("points at %s, which is not ours to manage", mapping.InternalClient)})
	}

	return actions
}

// mismatch is why an existing mapping does not match what we want, or "" if
// it does.
//
// Note what is *not* compared: the description. v1 decided this by substring
// matching the description against the gateway's listing, so any router that
// truncated or rewrote it made every rule look absent and every pass deleted
// and re-added it - dropping live connections every interval, and failing with
// code 714 when there was nothing there to delete. That is issue #6.
func mismatch(forward model.DesiredForward, mapping model.Mapping) string {
	if mapping.InternalPort != forward.Rule.InternalPort {
		return fmt.Sprintf("points at port %d, expected %d", mapping.InternalPort, forward.Rule.InternalPort)
	}
	if forward.InternalClient != "" && mapping.InternalClient != forward.InternalClient {
		return fmt.Sprintf("points at %s, expected %s", mapping.InternalClient, forward.InternalClient)
	}
	return ""
}

// expiring is whether a lease runs out within renewWithin. A lease of 0 means
// the mapping never expires, so it is never expiring.
func expiring(mapping model.Mapping, renewWithin time.Duration) bool {
	lease := time.Duration(mapping.LeaseDuration) * time.Second
	return lease > 0 && lease <= renewWithin
}

func owner(mapping model.Mapping) string {
	if strings.TrimSpace(mapping.Description) == "" {
		return fmt.Sprintf("%s:%d, no description", mapping.InternalClient, mapping.InternalPort)
	}
	return mapping.Description
}
