package model

import "testing"

// The format is load-bearing: routers out there already carry rules written
// by v1 in exactly this shape, and ownership is decided by its prefix.
func TestDescribeWritesV1sFormat(t *testing.T) {
	got := Describe(Rule{ExternalPort: 9999, InternalPort: 80, Protocol: TCP}, "nginx")
	if want := "portical: (9999:80/tcp) nginx"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestIsOursByPrefix(t *testing.T) {
	if !(Mapping{Description: "portical: (6881:6881/ud"}).IsOurs() {
		t.Error("a truncated description of ours is still ours")
	}
	if (Mapping{Description: "plex"}).IsOurs() {
		t.Error("another tool's mapping is not ours")
	}
}

func TestIdentityIgnoresEverythingButProtocolAndPort(t *testing.T) {
	rule := Rule{ExternalPort: 8080, InternalPort: 80, Protocol: TCP}
	mapping := Mapping{ExternalPort: 8080, Protocol: TCP, InternalPort: 9090, Description: "plex"}
	if rule.Identity() != mapping.Identity() || rule.Identity() != "tcp/8080" {
		t.Errorf("got %q and %q", rule.Identity(), mapping.Identity())
	}
}
