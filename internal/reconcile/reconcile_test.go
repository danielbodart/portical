package reconcile

import (
	"reflect"
	"testing"
	"time"

	"github.com/danielbodart/portical/internal/model"
)

func rule(external, internal int, protocol model.Protocol) model.Rule {
	return model.Rule{ExternalPort: external, InternalPort: internal, Protocol: protocol}
}

func tcp(port int) model.Rule { return rule(port, port, model.TCP) }

func want(r model.Rule, owner, internalClient string) model.DesiredForward {
	return model.DesiredForward{Rule: r, Owner: owner, Target: model.Host, InternalClient: internalClient}
}

func nginx(r model.Rule) model.DesiredForward { return want(r, "nginx", "") }

// ours is a mapping on the gateway that Portical put there, changed by edit.
func ours(r model.Rule, owner string, edit ...func(*model.Mapping)) model.Mapping {
	mapping := model.Mapping{
		ExternalPort:   r.ExternalPort,
		Protocol:       r.Protocol,
		InternalClient: "192.168.1.5",
		InternalPort:   r.InternalPort,
		Description:    model.Describe(r, owner),
	}
	for _, e := range edit {
		e(&mapping)
	}
	return mapping
}

func plex() model.Mapping {
	return ours(tcp(32400), "nginx", func(m *model.Mapping) { m.Description = "plex" })
}

func kinds(actions []Action) []string {
	found := []string{}
	for _, action := range actions {
		found = append(found, action.Kind.String())
	}
	return found
}

func assertKinds(t *testing.T, actions []Action, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	if got := kinds(actions); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func assertReason(t *testing.T, action Action, want string) {
	t.Helper()
	if action.Reason != want {
		t.Errorf("got reason %q, want %q", action.Reason, want)
	}
}

func desired(forwards ...model.DesiredForward) []model.DesiredForward { return forwards }

func actual(mappings ...model.Mapping) []model.Mapping { return mappings }

func TestAddsAForwardThatIsNotOnTheGateway(t *testing.T) {
	assertKinds(t, Reconcile(desired(nginx(tcp(8080))), nil, Options{}), "add")
}

func TestRemovesAMappingOfOursThatNothingAsksFor(t *testing.T) {
	assertKinds(t, Reconcile(nil, actual(ours(tcp(8080), "nginx")), Options{}), "remove")
}

func TestLeavesAMappingAloneThatIsNotOurs(t *testing.T) {
	assertKinds(t, Reconcile(nil, actual(plex()), Options{}))
}

// Issue #6. v1 compared the free-text description, so a router that
// truncated or rewrote it made a perfectly good rule look missing and deleted
// it - every interval, dropping live connections each time.
func TestDoesNotChurnARuleThatIsAlreadyCorrect(t *testing.T) {
	udp := rule(6881, 6881, model.UDP)
	for _, c := range []struct {
		name    string
		forward model.DesiredForward
		mapping model.Mapping
	}{
		{"when the description matches exactly", nginx(tcp(8080)), ours(tcp(8080), "nginx")},
		{"when the router truncated the description", want(udp, "qbittorrent", ""),
			ours(udp, "qbittorrent", func(m *model.Mapping) { m.Description = "portical: (6881:6881/ud" })},
		{"when the router rewrote the description in its own words", nginx(tcp(8080)),
			ours(tcp(8080), "nginx", func(m *model.Mapping) { m.Description = "portical: rule 1" })},
		{"when the container it came from was renamed", want(tcp(8080), "nginx-new", ""), ours(tcp(8080), "nginx-old")},
	} {
		t.Run(c.name, func(t *testing.T) {
			assertKinds(t, Reconcile(desired(c.forward), actual(c.mapping), Options{}), "keep")
		})
	}
}

func TestReplacesAMappingThatPointsAtTheWrongInternalPort(t *testing.T) {
	drifted := ours(tcp(8080), "nginx", func(m *model.Mapping) { m.InternalPort = 9090 })
	actions := Reconcile(desired(nginx(rule(8080, 80, model.TCP))), actual(drifted), Options{})
	assertKinds(t, actions, "replace")
	assertReason(t, actions[0], "points at port 9090, expected 80")
}

func TestReplacesAMappingThatPointsAtTheWrongAddress(t *testing.T) {
	drifted := ours(tcp(8080), "nginx", func(m *model.Mapping) { m.InternalClient = "192.168.1.99" })
	actions := Reconcile(desired(want(tcp(8080), "nginx", "192.168.1.5")), actual(drifted), Options{})
	assertKinds(t, actions, "replace")
	assertReason(t, actions[0], "points at 192.168.1.99, expected 192.168.1.5")
}

// An address we could not determine must not read as a mismatch, or every
// pass replaces a working rule.
func TestDoesNotReplaceWhenTheWantedAddressIsUnknown(t *testing.T) {
	mapping := ours(tcp(8080), "nginx", func(m *model.Mapping) { m.InternalClient = "192.168.1.99" })
	assertKinds(t, Reconcile(desired(nginx(tcp(8080))), actual(mapping), Options{}), "keep")
}

func lease(seconds int) func(*model.Mapping) {
	return func(m *model.Mapping) { m.LeaseDuration = seconds }
}

// Renewed, not replaced. The difference is that a renewal is carried out with
// AddPortMapping alone, leaving the rule in place throughout - see Renew.
func TestRenewsALeaseThatIsAboutToExpire(t *testing.T) {
	actions := Reconcile(desired(nginx(tcp(8080))), actual(ours(tcp(8080), "nginx", lease(300))), Options{RenewWithin: time.Hour})
	assertKinds(t, actions, "renew")
	assertReason(t, actions[0], "lease expires in 300s")
}

func TestLeavesALeaseWithPlentyOfTimeLeft(t *testing.T) {
	actions := Reconcile(desired(nginx(tcp(8080))), actual(ours(tcp(8080), "nginx", lease(86400))), Options{RenewWithin: time.Hour})
	assertKinds(t, actions, "keep")
}

// 0 means "never expires", not "expired an infinitely long time ago".
func TestNeverTreatsAnUnlimitedLeaseAsExpiring(t *testing.T) {
	actions := Reconcile(desired(nginx(tcp(8080))), actual(ours(tcp(8080), "nginx", lease(0))), Options{RenewWithin: 24 * time.Hour})
	assertKinds(t, actions, "keep")
}

func TestReportsAnotherToolsMappingRatherThanStealingIt(t *testing.T) {
	actions := Reconcile(desired(want(tcp(32400), "jellyfin", "")), actual(plex()), Options{})
	assertKinds(t, actions, "conflict")
	assertReason(t, actions[0], "already forwarded by something else (plex)")
}

// Rewritten even though it already points the right way: ownership lives in
// the description, so a mapping still carrying "plex" is one we would refuse
// to touch next pass and never clean up.
func TestTakesOverAnotherToolsMappingWhenAskedClaimingTheDescription(t *testing.T) {
	actions := Reconcile(desired(nginx(tcp(32400))), actual(plex()), Options{Steal: true})
	assertKinds(t, actions, "replace")
	assertReason(t, actions[0], "taking over from plex")
}

func TestDescribesAnUnlabelledOwnerUsefully(t *testing.T) {
	anonymous := plex()
	anonymous.Description = "  "
	actions := Reconcile(desired(nginx(tcp(32400))), actual(anonymous), Options{})
	assertReason(t, actions[0], "already forwarded by something else (192.168.1.5:32400, no description)")
}

// Rewritten in place rather than taken down and recreated: it already points
// where we want it, so there is nothing to move it away from.
func TestForceRewritesARuleThatIsAlreadyCorrect(t *testing.T) {
	actions := Reconcile(desired(nginx(tcp(8080))), actual(ours(tcp(8080), "nginx")), Options{Force: true})
	assertKinds(t, actions, "renew")
	assertReason(t, actions[0], "forced")
}

// Drift is the one case that justifies deleting first, because several
// firmwares refuse to overwrite a mapping whose internal address changed.
func TestStillReplacesARuleThatPointsSomewhereElseEvenUnderForce(t *testing.T) {
	drifted := ours(tcp(8080), "nginx", func(m *model.Mapping) { m.InternalPort = 9090 })
	assertKinds(t, Reconcile(desired(nginx(rule(8080, 80, model.TCP))), actual(drifted), Options{Force: true}), "replace")
}

func TestTreatsTheTwoProtocolsOnOnePortAsSeparateMappings(t *testing.T) {
	actions := Reconcile(
		desired(nginx(rule(25565, 25565, model.TCP)), nginx(rule(25565, 25565, model.UDP))),
		actual(ours(rule(25565, 25565, model.TCP), "nginx")),
		Options{},
	)
	assertKinds(t, actions, "keep", "add")
}

func TestHandlesAMixedPassWithoutDisturbingTheSettledRules(t *testing.T) {
	actions := Reconcile(
		desired(nginx(tcp(8080)), nginx(tcp(9090))),
		actual(ours(tcp(8080), "nginx"), ours(tcp(7070), "nginx"), plex()),
		Options{},
	)
	assertKinds(t, actions, "keep", "add", "remove")
	if actions[2].Mapping.ExternalPort != 7070 {
		t.Errorf("removed %v", actions[2].Mapping)
	}
}

func elsewhere() model.Mapping {
	return ours(tcp(25565), "minecraft", func(m *model.Mapping) { m.InternalClient = "192.168.1.99" })
}

var mine = Options{ManagedAddresses: map[string]bool{"192.168.1.5": true}}

func TestLeavesAnotherPorticalsMappingAloneAsAnOrphan(t *testing.T) {
	actions := Reconcile(nil, actual(elsewhere()), mine)
	assertKinds(t, actions, "orphan")
	assertReason(t, actions[0], "points at 192.168.1.99, which is not ours to manage")
}

func TestReportsAnotherPorticalsMappingAsAConflictRatherThanTakingIt(t *testing.T) {
	actions := Reconcile(desired(want(tcp(25565), "mine", "192.168.1.5")), actual(elsewhere()), mine)
	assertKinds(t, actions, "conflict")
	assertReason(t, actions[0], "already forwarded by another Portical to 192.168.1.99")
}

// Steal is for other tools. Another Portical is somebody's live forward.
func TestDoesNotStealFromAnotherPortical(t *testing.T) {
	options := mine
	options.Steal = true
	assertKinds(t, Reconcile(desired(want(tcp(25565), "mine", "192.168.1.5")), actual(elsewhere()), options), "conflict")
}

func TestManagesEveryMappingOfOursWhenNoAddressesAreGiven(t *testing.T) {
	assertKinds(t, Reconcile(nil, actual(elsewhere()), Options{}), "remove")
}

// No addresses at all is an instance that cannot yet tell where it sends
// traffic, not one asked to manage everything.
func TestManagesNothingWhenTheAddressesAreKnownToBeNone(t *testing.T) {
	none := Options{ManagedAddresses: map[string]bool{}}
	assertKinds(t, Reconcile(nil, actual(elsewhere()), none), "orphan")
	assertKinds(t, Reconcile(desired(want(tcp(25565), "mine", "")), actual(elsewhere()), none), "conflict")
}
