// Package model is the vocabulary the rest of Portical is written in.
//
// The important distinction here is Rule vs Mapping. A Rule is something we
// *want* to exist, read off a container label or a unit file. A Mapping is
// something that *does* exist, read back off the gateway. Reconciliation is
// entirely a matter of comparing the two sets, so keeping them as separate
// types stops the two ideas from being conflated the way they were in the
// shell version.
package model

import (
	"fmt"
	"strings"
)

// Protocol is the transport a forward carries, as Portical writes it: lower
// case. The gateway speaks upper case, which the UPnP client translates.
type Protocol string

const (
	TCP Protocol = "tcp"
	UDP Protocol = "udp"
)

// Protocols is both of them, in the order a rule with no protocol expands to.
var Protocols = []Protocol{TCP, UDP}

// Rule is a port forward we want to exist.
type Rule struct {
	ExternalPort int
	InternalPort int
	Protocol     Protocol
}

// Mapping is a port forward that currently exists on the internet gateway.
type Mapping struct {
	ExternalPort   int
	Protocol       Protocol
	InternalClient string
	InternalPort   int
	Description    string
	RemoteHost     string
	// Seconds until the gateway drops this mapping; 0 means it never expires.
	LeaseDuration int
}

// Identity is the identity of a forward, as the gateway itself understands it.
//
// An IGD keys its mapping table on (RemoteHost, ExternalPort, Protocol), so
// two rules that agree on these collide no matter what else they say. v1
// decided "does this rule already exist?" by substring-matching the free-text
// description against `upnpc -l` output, which is why issue #6 happens: any
// router that truncates or rewrites the description makes every rule look
// missing, so every pass deletes and re-adds it. Comparing identity instead of
// prose is the fix.
//
// RemoteHost is left out because Portical only ever writes wildcard rules.
func Identity(protocol Protocol, externalPort int) string {
	return fmt.Sprintf("%s/%d", protocol, externalPort)
}

// Identity is the rule's slot on the gateway.
func (r Rule) Identity() string { return Identity(r.Protocol, r.ExternalPort) }

// Identity is the mapping's slot on the gateway.
func (m Mapping) Identity() string { return Identity(m.Protocol, m.ExternalPort) }

// String is the rule as a label would write it, `8080:80/tcp`.
func (r Rule) String() string {
	return fmt.Sprintf("%d:%d/%s", r.ExternalPort, r.InternalPort, r.Protocol)
}

// DescriptionPrefix is the marker that tells us a mapping on the gateway is
// one of ours.
const DescriptionPrefix = "portical:"

// Describe is the description a rule is written to the gateway with.
//
// Generated in exactly v1's format, deliberately. Anyone upgrading has live
// rules on their router carrying the old text, and ownership is decided by
// the prefix. Changing the format would orphan every pre-existing rule -
// Portical would neither recognise nor clean them up, and would sit alongside
// its own duplicates.
func Describe(rule Rule, owner string) string {
	return fmt.Sprintf("%s (%s) %s", DescriptionPrefix, rule, owner)
}

// IsOurs is whether a mapping on the gateway was put there by Portical.
func (m Mapping) IsOurs() bool { return strings.HasPrefix(m.Description, DescriptionPrefix) }

// TargetKind is where the traffic for a forward should land.
type TargetKind int

const (
	// TargetHost covers the bridge and host network drivers, and every systemd
	// unit, where the gateway forwards to this host and it takes it from
	// there.
	TargetHost TargetKind = iota
	// TargetContainer covers macvlan and ipvlan, where the container has its
	// own address on the LAN and the gateway forwards straight to it.
	TargetContainer
)

// Target is where a forward lands, and for a container, through which of its
// networks.
type Target struct {
	Kind    TargetKind
	Network string
}

// Host is the target of everything forwarded to this host.
var Host = Target{Kind: TargetHost}

// Container is the target of a forward straight to a container's own address
// on the named network.
func Container(network string) Target { return Target{Kind: TargetContainer, Network: network} }

// DesiredForward is a rule, resolved against whatever asked for it.
type DesiredForward struct {
	Rule Rule
	// Who asked: a container's name or a unit's. It is what ends up in the
	// rule's description on the router, and in every log line about it.
	Owner  string
	Target Target
	// The address the mapping should point at, or "" when it cannot be worked
	// out, in which case the address is not compared. Deliberately: an unknown
	// address must never be read as a mismatch, because a mismatch means
	// "replace", and replacing a working rule on every pass is exactly the
	// churn issue #6 reports.
	InternalClient string
}
