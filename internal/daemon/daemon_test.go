package daemon

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danielbodart/gokit/await"
	"github.com/danielbodart/gokit/httpx"
	sd "github.com/danielbodart/gokit/systemd"
	"github.com/danielbodart/portical/internal/fakes"
	"github.com/danielbodart/portical/internal/model"
	"github.com/danielbodart/portical/internal/source"
	"github.com/danielbodart/portical/internal/upnp"
)

func TestAContainerThatWantsAPortForwardedGetsOne(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	h.running(nginx())

	h.once(t)

	want := []model.Mapping{{
		ExternalPort:   9999,
		Protocol:       model.TCP,
		InternalPort:   80,
		InternalClient: host,
		Description:    "portical: (9999:80/tcp) nginx",
	}}
	if got := h.router.Mappings(); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v", got)
	}
}

func TestAContainerGetsBothProtocolsWhenItDoesNotNameOne(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	h.running(labelled("nginx", "25565"))
	h.once(t)
	var protocols []string
	for _, mapping := range h.router.Mappings() {
		protocols = append(protocols, string(mapping.Protocol))
	}
	slices.Sort(protocols)
	if !reflect.DeepEqual(protocols, []string{"tcp", "udp"}) {
		t.Errorf("got %v", protocols)
	}
}

// Issue #6, end to end.
//
// v1 asked "is my description a substring of the gateway's listing?" and any
// router that answered differently from how it was asked made every rule look
// missing. It then deleted and re-added on every pass - dropping live
// connections each interval, and failing with 714 when there was nothing to
// delete. These assert on the SOAP the router actually receives, because
// "nothing changed" is precisely the property that was broken.

func TestARuleThatIsAlreadyCorrectIsNotTouchedOnTheNextPass(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	h.running(nginx())
	h.once(t)

	h.router.ClearActions()
	h.once(t)

	if h.received("DeletePortMapping") || h.received("AddPortMapping") {
		t.Errorf("received %v", h.router.Actions())
	}
}

func TestARuleThatIsAlreadyCorrectIsNotTouchedEvenAfterTwentyPasses(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	h.running(nginx())
	h.once(t)

	h.router.ClearActions()
	for range 20 {
		h.once(t)
	}

	if h.received("AddPortMapping") || len(h.router.Mappings()) != 1 {
		t.Errorf("received %v", h.router.Actions())
	}
}

// The router in issue #6 stores a shortened description. v1 could never match
// it again, so it churned forever.
func TestARuleSurvivesARouterThatTruncatesTheDescription(t *testing.T) {
	h := newHarness(t, fakes.Quirks{DescriptionLimit: 22})
	h.running(labelled("qbittorrent", "6881/udp"))
	h.once(t)
	if got := h.router.Mappings()[0].Description; got != "portical: (6881:6881/u" {
		t.Fatalf("stored %q", got)
	}

	h.router.ClearActions()
	h.once(t)

	if h.received("DeletePortMapping") {
		t.Errorf("received %v", h.router.Actions())
	}
}

func TestARuleSurvivesARouterThatEndsItsTableWith501RatherThan713(t *testing.T) {
	h := newHarness(t, fakes.Quirks{EndOfTableCode: 501})
	h.running(nginx())
	h.once(t)

	h.router.ClearActions()
	h.once(t)

	if h.received("AddPortMapping") || len(h.router.Mappings()) != 1 {
		t.Errorf("received %v", h.router.Actions())
	}
}

func TestARuleIsNotRewrittenWhenItsContainerIsRenamed(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	h.running(nginx())
	h.once(t)

	h.running(labelled("nginx-renamed", "9999:80/tcp"))
	h.router.ClearActions()
	h.once(t)

	if h.received("AddPortMapping") {
		t.Errorf("received %v", h.router.Actions())
	}
}

// Issue #2, which falls out of reconciling rather than watching for stops.
func TestAContainerThatGoesAwayHasItsForwardRemoved(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	h.running(nginx())
	h.once(t)

	h.running()
	h.once(t)

	if got := h.router.Mappings(); len(got) != 0 {
		t.Errorf("got %+v", got)
	}
}

func TestAContainerThatGoesAwayLeavesOtherContainersForwardsInPlace(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	h.running(nginx(), labelled("plex", "32400/tcp"))
	h.once(t)

	h.running(labelled("plex", "32400/tcp"))
	h.once(t)

	if got := h.ports(); !reflect.DeepEqual(got, []int{32400}) {
		t.Errorf("got %v", got)
	}
}

func TestAContainerThatGoesAwayLeavesAnotherToolsForwardsAlone(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	h.router.Given(model.Mapping{ExternalPort: 32400, Protocol: model.TCP, Description: "plex"})

	h.once(t)

	if len(h.router.Mappings()) != 1 {
		t.Errorf("got %+v", h.router.Mappings())
	}
}

func TestAContainerOnItsOwnLANAddressIsForwardedThere(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	h.running(container{name: "traefik", label: "80/tcp,443/tcp", macvlan: "192.168.1.40"})

	h.once(t)

	mappings := h.router.Mappings()
	if len(mappings) != 2 || mappings[0].InternalClient != "192.168.1.40" || mappings[1].InternalClient != "192.168.1.40" {
		t.Errorf("got %+v", mappings)
	}
}

func TestAMappingTheRouterDowngradedIsRenewedAsItNearsExpiry(t *testing.T) {
	h := newHarness(t, fakes.Quirks{LeaseLimit: 600}, func(o *Options) { o.RenewWithin = time.Hour })
	h.running(nginx())
	h.once(t)
	if got := h.router.Mappings()[0].LeaseDuration; got != 600 {
		t.Fatalf("lease %d", got)
	}

	h.router.ClearActions()
	h.once(t)

	if !h.received("AddPortMapping") {
		t.Errorf("received %v", h.router.Actions())
	}
}

func TestAMappingThatNeverExpiresIsLeftAlone(t *testing.T) {
	h := newHarness(t, fakes.Quirks{}, func(o *Options) { o.RenewWithin = 24 * time.Hour })
	h.running(nginx())
	h.once(t)

	h.router.ClearActions()
	h.once(t)

	if h.received("AddPortMapping") {
		t.Errorf("received %v", h.router.Actions())
	}
}

func TestTheLeaseAskedForIsInSeconds(t *testing.T) {
	h := newHarness(t, fakes.Quirks{}, func(o *Options) { o.LeaseDuration = 2 * time.Hour })
	h.running(nginx())
	h.once(t)
	if got := h.router.Mappings()[0].LeaseDuration; got != 7200 {
		t.Errorf("lease %d", got)
	}
}

func jellyfin() container { return labelled("jellyfin", "32400/tcp") }

func TestAnotherToolsMappingIsReportedAndLeftAlone(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	h.router.Given(model.Mapping{ExternalPort: 32400, Protocol: model.TCP, Description: "plex"})
	h.running(jellyfin())

	h.once(t)

	if got := h.router.Mappings()[0].Description; got != "plex" {
		t.Errorf("description %q", got)
	}
	if want := "Conflict: jellyfin wants 32400:32400/tcp but it is (plex). Use --steal to take it over."; !h.log.Contains(want) {
		t.Errorf("log:\n%s", h.log.Text())
	}
}

func TestAnotherToolsMappingIsTakenOverWhenAsked(t *testing.T) {
	h := newHarness(t, fakes.Quirks{}, func(o *Options) { o.Steal = true })
	h.router.Given(model.Mapping{ExternalPort: 32400, Protocol: model.TCP, Description: "plex"})
	h.running(jellyfin())

	h.once(t)

	if got := h.router.Mappings()[0].Description; got != "portical: (32400:32400/tcp) jellyfin" {
		t.Errorf("description %q", got)
	}
}

// v1 exited the process on any upnpc failure, so one rule the router would not
// accept stopped every other forward on the host.
func TestOneRuleTheRouterRefusesDoesNotStopTheOthers(t *testing.T) {
	h := newHarness(t, fakes.Quirks{SecureMode: true})
	h.running(container{name: "direct", label: "80/tcp", macvlan: "192.168.1.40"}, nginx())

	h.once(t)

	if got := h.ports(); !reflect.DeepEqual(got, []int{9999}) {
		t.Errorf("got %v", got)
	}
	if !h.log.ErrorContains("718") {
		t.Errorf("log:\n%s", h.log.Text())
	}
}

// Removals are inferred from absence, so reconciling against a list we failed
// to fetch would read as "everything stopped".
func TestASourceOutageNeverTearsDownLiveMappings(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	h.running(nginx())
	h.once(t)

	h.source.FailWith(errors.New("Cannot connect to the Docker daemon"))
	if _, err := h.daemon.Once(ctx); err == nil || !strings.Contains(err.Error(), "Docker daemon") {
		t.Errorf("got %v", err)
	}

	if len(h.router.Mappings()) != 1 {
		t.Errorf("got %+v", h.router.Mappings())
	}
}

func TestAGatewayThatCannotBeReadFailsThePassRatherThanReadingAsEmpty(t *testing.T) {
	h := newHarness(t, fakes.Quirks{EndOfTableCode: 606})
	h.router.Given(model.Mapping{ExternalPort: 7070, Protocol: model.TCP, Description: "portical: (7070:7070/tcp) gone"})
	h.running(nginx())

	if _, err := h.daemon.Once(ctx); err == nil {
		t.Fatal("the pass went ahead")
	}
	if h.received("AddPortMapping") || h.received("DeletePortMapping") {
		t.Errorf("received %v", h.router.Actions())
	}
}

func TestASourcesWarningsAreLogged(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	h.source.Want(source.Desired{Warnings: []string{"nginx: ignoring unrecognised rule '8O8O'"}})
	h.once(t)
	// Said as commentary, not as an error: the pass went ahead, and what is
	// wrong is a label for whoever wrote it to read, not a fault of Portical's.
	if !slices.Contains(h.log.Logs(), "Warning: nginx: ignoring unrecognised rule '8O8O'") {
		t.Errorf("log:\n%s", h.log.Text())
	}
}

func TestAForwardWithNoAddressAndNoHostAddressSaysWhatToSet(t *testing.T) {
	h := newHarness(t, fakes.Quirks{}, func(o *Options) { o.HostAddress = "" })
	h.source.Want(source.Desired{Forwards: []model.DesiredForward{{Rule: model.Rule{ExternalPort: 80, InternalPort: 80, Protocol: model.TCP}, Owner: "nginx"}}})
	h.once(t)
	if !h.log.ErrorContains("  FAILED: cannot tell which address to forward to - set --root") {
		t.Errorf("log:\n%s", h.log.Text())
	}
}

func TestADryRunChangesNothing(t *testing.T) {
	h := newHarness(t, fakes.Quirks{}, func(o *Options) { o.DryRun = true })
	h.running(nginx())
	h.once(t)
	if len(h.router.Mappings()) != 0 {
		t.Errorf("got %+v", h.router.Mappings())
	}
}

func TestADryRunStillSaysWhatItWouldDo(t *testing.T) {
	h := newHarness(t, fakes.Quirks{}, func(o *Options) { o.DryRun = true })
	h.running(nginx())
	h.once(t)
	if !h.log.Contains("Adding 9999:80/tcp for nginx") {
		t.Errorf("log:\n%s", h.log.Text())
	}
}

// Every Portical writes the same "portical:" prefix, so the description alone
// cannot say *which* Portical wrote a rule. Without this, a second instance -
// on another host, or run by hand from a laptop - sees the first's rules,
// finds nothing wanting them, and deletes them. The two then delete each
// other's rules forever.
//
// Found by pointing this at a live gateway: a dry run from a machine with no
// labelled containers proposed removing all eight of another host's mappings.
func elsewhere() model.Mapping {
	return model.Mapping{
		ExternalPort:   25565,
		Protocol:       model.TCP,
		InternalClient: "192.168.1.99",
		Description:    "portical: (25565:25565/tcp) minecraft",
	}
}

func TestAnotherPorticalsMappingsAreLeftAlone(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	h.router.Given(elsewhere())

	h.once(t)

	if len(h.router.Mappings()) != 1 {
		t.Errorf("got %+v", h.router.Mappings())
	}
}

func TestAnotherPorticalIsNotFoughtOverWhenWeWantTheSamePort(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	h.router.Given(elsewhere())
	h.running(labelled("mine", "25565:25565/tcp"))

	h.once(t)

	if got := h.router.Mappings()[0].InternalClient; got != "192.168.1.99" {
		t.Errorf("moved to %s", got)
	}
	if !h.log.Contains("already forwarded by another Portical to 192.168.1.99") {
		t.Errorf("log:\n%s", h.log.Text())
	}
}

func TestAnotherPorticalDoesNotStopUsCleaningUpOurOwn(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	h.router.Given(elsewhere())
	h.running(nginx())
	h.once(t)

	h.running()
	h.once(t)

	mappings := h.router.Mappings()
	if len(mappings) != 1 || mappings[0].InternalClient != "192.168.1.99" {
		t.Errorf("got %+v", mappings)
	}
}

// With no host address and nothing forwarded, there is no address to call
// ours. That must read as "none of these are ours", not "all of them are".
func TestAnotherPorticalsMappingsAreLeftAloneWhenWeCannotTellOurOwnAddress(t *testing.T) {
	h := newHarness(t, fakes.Quirks{}, func(o *Options) { o.HostAddress = "" })
	h.router.Given(elsewhere())

	h.once(t)
	if len(h.router.Mappings()) != 1 {
		t.Fatalf("once removed it: %+v", h.router.Mappings())
	}

	if err := h.daemon.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.router.Mappings()) != 1 {
		t.Errorf("cleanup removed it: %+v", h.router.Mappings())
	}
}

func TestAnotherPorticalsMappingsAreManagedAnywayWhenExplicitlyAsked(t *testing.T) {
	h := newHarness(t, fakes.Quirks{}, func(o *Options) { o.ManageAll = true })
	h.router.Given(elsewhere())

	h.once(t)

	if len(h.router.Mappings()) != 0 {
		t.Errorf("got %+v", h.router.Mappings())
	}
}

// A macvlan container that stops takes its address with it, so the address
// has to be remembered from when the mapping was written.
func TestAStoppedContainersOwnAddressIsStillCleanedUp(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	h.running(container{name: "direct", label: "8080/tcp", macvlan: "192.168.1.40"})
	h.once(t)
	if len(h.router.Mappings()) != 1 {
		t.Fatalf("got %+v", h.router.Mappings())
	}

	h.running()
	h.once(t)

	if len(h.router.Mappings()) != 0 {
		t.Errorf("got %+v", h.router.Mappings())
	}
}

func TestCleanupRemovesOurMappingsAndOnlyOurs(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	h.router.Given(model.Mapping{ExternalPort: 32400, Protocol: model.TCP, Description: "plex"})
	h.router.Given(elsewhere())
	h.running(nginx())
	h.once(t)

	if err := h.daemon.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}

	var descriptions []string
	for _, mapping := range h.router.Mappings() {
		descriptions = append(descriptions, mapping.Description)
	}
	if !reflect.DeepEqual(descriptions, []string{"plex", "portical: (25565:25565/tcp) minecraft"}) {
		t.Errorf("got %v", descriptions)
	}
}

func TestSummarySaysWhenThereIsNothingToDo(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	h.running(nginx())
	h.once(t)

	h.log.Clear()
	h.once(t)

	if got := h.log.Lines(); !reflect.DeepEqual(got, []string{"1 rule already correct, nothing to do"}) {
		t.Errorf("got %q", got)
	}
}

// A daemon reconciling every 15 seconds should say it has settled once, and
// then not repeat itself forever.
func TestSummaryStaysQuietWhileNothingChanges(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	h.running(nginx())
	h.once(t)
	h.once(t)

	h.log.Clear()
	for range 5 {
		h.once(t)
	}

	if got := h.log.Lines(); len(got) != 0 {
		t.Errorf("got %q", got)
	}
}

func TestSummarySpeaksUpAgainWhenSomethingChanges(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	h.running(nginx())
	h.once(t)
	h.log.Clear()

	h.running()
	h.once(t)

	if !slices.Contains(h.log.Lines(), "1 removed, 0 already correct") {
		t.Errorf("got %q", h.log.Lines())
	}
}

func TestSummaryCountsAMixedPass(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	h.router.Given(model.Mapping{ExternalPort: 7070, Protocol: model.TCP, Description: "portical: (7070:7070/tcp) gone"})
	h.router.Given(elsewhere())
	h.running(nginx(), labelled("plex", "32400/tcp"))

	h.once(t)

	if !slices.Contains(h.log.Lines(), "2 added, 1 removed, 1 left alone, 0 already correct") {
		t.Errorf("got %q", h.log.Lines())
	}
}

func TestSummarySaysWhenThereIsNothingToManage(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	h.once(t)
	if got := h.log.Lines(); !reflect.DeepEqual(got, []string{"No rules to manage"}) {
		t.Errorf("got %q", got)
	}
}

// miniupnpd's secure_mode - the OpenWrt default - only lets a client map to
// its own address. A macvlan container's mapping names the container's
// address, not Portical's, so the gateway refuses with 718, which otherwise
// reads as an ordinary port collision and sends people hunting for a conflict
// that does not exist.

// With no way in to ask as the container, a macvlan rule is asked for from the
// host and refused. The message has to say what to check.
func TestSecureModeExplainsWhatToCheckWhenThereIsNoWayToAskAsTheContainer(t *testing.T) {
	h := newHarness(t, fakes.Quirks{SecureMode: true})
	h.running(container{name: "direct", label: "8080/tcp", macvlan: "192.168.1.40"})

	h.once(t)

	if !h.log.ErrorContains("718") || !h.log.ErrorContains("real conflict") {
		t.Errorf("log:\n%s", h.log.Text())
	}
}

// A bridge container forwards to the host, which is the requester, so it works
// - and must not be given a misleading explanation.
func TestSecureModeSaysNothingExtraAboutABridgeContainerThatSucceeded(t *testing.T) {
	h := newHarness(t, fakes.Quirks{SecureMode: true})
	h.running(nginx())

	h.once(t)

	if h.log.Contains("real conflict") {
		t.Errorf("log:\n%s", h.log.Text())
	}
}

// What the Docker source's relay is for: the same gateway, asked from the
// container's own address, is allowed to map to it.
func TestSecureModeIsSatisfiedByAskingAsTheContainer(t *testing.T) {
	h := newHarness(t, fakes.Quirks{SecureMode: true})
	asked := []string{}
	h.daemon.asContainer = func(owner string) upnp.Gateway {
		asked = append(asked, owner)
		return upnp.New(&http.Client{Transport: httpx.HandlerTransport(h.router.From("192.168.1.40"))}, fakes.ControlURL, fakes.ServiceType)
	}
	h.running(container{name: "direct", label: "8080/tcp", macvlan: "192.168.1.40"}, nginx())

	h.once(t)

	if got := h.ports(); !reflect.DeepEqual(got, []int{8080, 9999}) {
		t.Errorf("got %v\n%s", got, h.log.Text())
	}
	if !reflect.DeepEqual(asked, []string{"direct"}) {
		t.Errorf("asked as %v", asked)
	}
}

func TestAFailureAskingAsTheContainerSaysWhatToCheck(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	h.daemon.asContainer = func(string) upnp.Gateway {
		return upnp.New(&http.Client{Transport: httpx.RoundTripperFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("no such image")
		})}, fakes.ControlURL, fakes.ServiceType)
	}
	h.running(container{name: "direct", label: "8080/tcp", macvlan: "192.168.1.40"})

	h.once(t)

	if !h.log.ErrorContains("no such image") || !h.log.ErrorContains("--helper-image") {
		t.Errorf("log:\n%s", h.log.Text())
	}
}

// A gateway's redirect rule governs *new* flows only; established ones are
// carried by conntrack. So deleting a rule to re-add it is invisible to
// everyone already connected, and a closed door to anyone arriving in that
// window - which on a game server looks like the server going down for no
// reason, twice a day, for no visible cause.
func expiring(t *testing.T) *harness {
	h := newHarness(t, fakes.Quirks{LeaseLimit: 600}, func(o *Options) { o.RenewWithin = time.Hour })
	h.running(nginx())
	h.once(t)
	h.router.ClearActions()
	return h
}

func TestRenewingALeaseNeverTakesTheRuleDownFirst(t *testing.T) {
	h := expiring(t)
	h.once(t)
	if !h.received("AddPortMapping") || h.received("DeletePortMapping") {
		t.Errorf("received %v", h.router.Actions())
	}
}

func TestRenewingALeaseLeavesTheMappingInPlaceThroughout(t *testing.T) {
	h := expiring(t)
	h.once(t)
	if mappings := h.router.Mappings(); len(mappings) != 1 || mappings[0].InternalPort != 80 {
		t.Errorf("got %+v", mappings)
	}
}

func TestRenewingALeaseSaysItIsRenewingRatherThanReplacing(t *testing.T) {
	h := expiring(t)
	h.once(t)
	if !h.log.Contains("Renewing 9999:80/tcp for nginx - lease expires in 600s") {
		t.Errorf("log:\n%s", h.log.Text())
	}
}

// Force means "write it again", not "tear it down and build it back".
func TestForceDoesNotTakeTheRuleDownEither(t *testing.T) {
	h := newHarness(t, fakes.Quirks{}, func(o *Options) { o.Force = true })
	h.running(nginx())
	h.once(t)
	h.router.ClearActions()

	h.once(t)

	if !h.received("AddPortMapping") || h.received("DeletePortMapping") {
		t.Errorf("received %v", h.router.Actions())
	}
}

// Moving a rule to a different internal target is the one case that needs the
// delete, because several firmwares refuse to overwrite it otherwise - which
// the fake does here, so a missing delete would fail the move.
func TestARuleThatMustMoveIsStillTakenDownFirst(t *testing.T) {
	h := newHarness(t, fakes.Quirks{RefuseToMove: true})
	h.running(labelled("nginx", "9999:80/tcp"))
	h.once(t)

	h.running(labelled("nginx", "9999:8080/tcp"))
	h.router.ClearActions()
	h.once(t)

	if !h.received("DeletePortMapping") {
		t.Errorf("received %v", h.router.Actions())
	}
	if got := h.router.Mappings()[0].InternalPort; got != 8080 {
		t.Errorf("internal port %d\n%s", got, h.log.Text())
	}
}

func (h *harness) start(c container, others ...container) {
	h.running(append(others, c)...)
	h.source.Emit(source.Event{Description: "Container " + c.name + " start"})
}

func TestRunReconcilesOnAnEventWithoutWaitingForTheInterval(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	stop := h.run(t)

	h.start(nginx())
	await.Until(t, "the forward", func() bool { return len(h.router.Mappings()) == 1 })

	h.running()
	h.source.Emit(source.Event{Description: "Container nginx die"})
	await.Until(t, "the forward to go", func() bool { return len(h.router.Mappings()) == 0 })

	stop()
	if !h.log.Contains("Container nginx start") || !h.log.Contains("Container nginx die") {
		t.Errorf("log:\n%s", h.log.Text())
	}
}

func TestRunReconcilesEveryInterval(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	h.run(t)
	await.Until(t, "the first pass", func() bool { return h.source.Calls() == 1 })

	// Wanted without saying so, so only the interval can notice.
	h.running(nginx())
	if err := h.clock.BlockUntil(ctx, 1); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(Defaults.Interval)

	await.Until(t, "the interval pass", func() bool { return len(h.router.Mappings()) == 1 })
}

func TestRunSaysItIsWatchingAndHowOften(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	stop := h.run(t)
	await.Until(t, "the stream", func() bool { return h.source.Open() == 1 })
	stop()
	if !slices.Contains(h.log.Lines(), "Watching for changes, reconciling every 15s...") {
		t.Errorf("log:\n%s", h.log.Text())
	}
}

func TestRunCollapsesABurstOfEventsIntoOnePassBehindTheOneRunning(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	gate := make(chan struct{})
	h.source.Blocking(func(ctx context.Context) error {
		select {
		case <-gate:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	stop := h.run(t)

	gate <- struct{}{} // the first pass
	await.Until(t, "the stream", func() bool { return h.source.Open() == 1 })

	h.source.Emit(source.Event{})
	await.Until(t, "the event's pass", func() bool { return h.source.Calls() == 2 })
	for range 30 {
		h.source.Emit(source.Event{Description: "burst"})
	}
	// Every one of them seen while the event's pass is still running.
	await.Until(t, "the burst", func() bool { return strings.Count(h.log.Text(), "burst") == 30 })
	gate <- struct{}{} // the event's pass

	await.Until(t, "the pass queued behind it", func() bool { return h.source.Calls() == 3 })
	close(gate) // that one, and any that should not have been queued
	stop()

	// The first pass, the first event's, and one more for everything after it.
	if got := h.source.Calls(); got != 3 {
		t.Errorf("%d passes", got)
	}
}

func TestRunKeepsGoingAfterAPassFails(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	h.run(t)
	await.Until(t, "the stream", func() bool { return h.source.Open() == 1 })

	h.source.FailWith(errors.New("Cannot connect to the Docker daemon"))
	h.start(nginx())
	await.Until(t, "the failure", func() bool { return h.log.ErrorContains("Error: Cannot connect to the Docker daemon") })

	h.source.FailWith(nil)
	h.start(labelled("plex", "32400/tcp"), nginx())

	// Both containers, including the one whose start event was lost to the
	// outage - every pass reconciles from scratch rather than replaying
	// events, so nothing has to be remembered across the failure.
	await.Until(t, "both forwards", func() bool { return reflect.DeepEqual(h.ports(), []int{9999, 32400}) })
}

func TestRunReopensAStreamThatDropsInsteadOfExiting(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	h.run(t)
	await.Until(t, "the stream", func() bool { return h.source.Open() == 1 })

	// The stream drops the way the Docker socket's idle timeout drops it on a
	// quiet host - failing, without the daemon being stopped.
	h.source.Drop(errors.New("The operation timed out"))
	// Kept while the daemon is between streams; a reopened stream must
	// deliver it. Had Run exited on the drop, it would never be seen and no
	// mapping would appear, the interval being stopped.
	h.start(nginx())

	// The interval's wait and the backoff's.
	if err := h.clock.BlockUntil(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if h.source.Opened() != 1 {
		t.Error("reopened before the backoff")
	}
	h.clock.Advance(Defaults.ReconnectBackoff)

	await.Until(t, "the forward", func() bool { return len(h.router.Mappings()) == 1 })
	if h.source.Opened() != 2 {
		t.Errorf("opened %d streams", h.source.Opened())
	}
}

func TestRunReopensAStreamThatEndsCleanly(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	h.run(t)
	await.Until(t, "the stream", func() bool { return h.source.Open() == 1 })

	h.source.Drop(nil)
	if err := h.clock.BlockUntil(ctx, 2); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(Defaults.ReconnectBackoff)

	await.Until(t, "the stream to reopen", func() bool { return h.source.Opened() == 2 && h.source.Open() == 1 })
}

// Issue #10, reported as a memory leak: each reconnect in the TypeScript
// version left a listener on a signal that lived as long as the daemon. With
// contexts there is no listener to leave, and this holds that in place - the
// daemon's goroutines are the same after twenty reconnects as before them.
func TestRunLeavesNothingBehindOnEveryReconnect(t *testing.T) {
	// An hour, so twenty backoffs never add up to an interval pass.
	h := newHarness(t, fakes.Quirks{}, func(o *Options) { o.Interval = time.Hour })
	h.run(t)
	await.Until(t, "the stream", func() bool { return h.source.Open() == 1 })
	settled := runtime.NumGoroutine()

	for i := range 20 {
		h.source.Drop(errors.New("The operation timed out"))
		if err := h.clock.BlockUntil(ctx, 2); err != nil {
			t.Fatal(err)
		}
		h.clock.Advance(Defaults.ReconnectBackoff)
		await.Until(t, "the stream to reopen", func() bool { return h.source.Opened() == i+2 && h.source.Open() == 1 })
	}

	await.Until(t, "the goroutines to settle", func() bool { return runtime.NumGoroutine() <= settled })
	if h.clock.Waiting() != 1 {
		t.Errorf("%d waits outstanding, want only the interval's", h.clock.Waiting())
	}
}

func TestRunStopsPromptlyDuringTheBackoff(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	stop := h.run(t)
	await.Until(t, "the stream", func() bool { return h.source.Open() == 1 })
	h.source.Drop(errors.New("The operation timed out"))
	if err := h.clock.BlockUntil(ctx, 2); err != nil {
		t.Fatal(err)
	}
	stop()
	if h.source.Opened() != 1 {
		t.Error("reopened while stopping")
	}
}

// Shutting down mid-pass must not leave a rule taken down and not yet put
// back, so the pass in flight finishes before Run returns.
func TestRunFinishesThePassInFlightBeforeReturning(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	cancel, done := h.launch()
	await.Until(t, "the stream", func() bool { return h.source.Open() == 1 })

	entered, release := make(chan struct{}), make(chan struct{})
	h.source.Blocking(func(context.Context) error {
		close(entered)
		<-release
		return nil
	})
	h.start(nginx())
	<-entered
	cancel()

	select {
	case <-done:
		t.Fatal("returned with a pass in flight")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	<-done
	if len(h.router.Mappings()) != 1 {
		t.Errorf("the pass was abandoned: %+v", h.router.Mappings())
	}
}

// Under Type=notify, systemd holds a start - and `systemctl restart` - until
// Ready, so it is sent once the first pass has put the forwards in place.
func TestRunSaysItIsReadyOnceTheFirstPassIsDone(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	h.running(nginx())
	stop := h.run(t)
	await.Until(t, "ready", func() bool { return slices.Contains(h.notified(), sd.Ready) })
	stop()

	if got, want := h.notified(), []string{sd.Status("1 forward"), sd.Ready}; !slices.Equal(got, want) {
		t.Errorf("notified %q, want %q", got, want)
	}
	if len(h.router.Mappings()) != 1 {
		t.Errorf("ready before the forward was in place")
	}
}

func TestRunsStatusIsUpdatedOnlyWhenItChanges(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	h.run(t)
	await.Until(t, "the stream", func() bool { return h.source.Open() == 1 })

	h.start(nginx())
	await.Until(t, "the forward", func() bool { return len(h.router.Mappings()) == 1 })
	h.source.Emit(source.Event{})
	await.Until(t, "the next pass", func() bool { return h.source.Calls() == 3 })
	h.source.FailWith(errors.New("Cannot connect to the Docker daemon"))
	h.source.Emit(source.Event{})
	want := []string{sd.Status("0 forwards"), sd.Ready, sd.Status("1 forward"), sd.Status("Error: Cannot connect to the Docker daemon")}
	await.Until(t, "the failure's status", func() bool { return len(h.notified()) == len(want) })

	if got := h.notified(); !slices.Equal(got, want) {
		t.Errorf("notified %q, want %q", got, want)
	}
}

func TestRunsStatusCountsWhatIsInConflictAndWhatFailed(t *testing.T) {
	h := newHarness(t, fakes.Quirks{SecureMode: true})
	h.router.Given(model.Mapping{ExternalPort: 32400, Protocol: model.TCP, InternalClient: "192.168.1.9", InternalPort: 32400, Description: "plex"})
	h.running(nginx(), labelled("jellyfin", "32400/tcp"), container{name: "direct", label: "80/tcp", macvlan: "192.168.1.40"})
	stop := h.run(t)
	await.Until(t, "ready", func() bool { return slices.Contains(h.notified(), sd.Ready) })
	stop()

	if want := sd.Status("1 forward, 1 in conflict, 1 failed"); h.notified()[0] != want {
		t.Errorf("notified %q, want %q first", h.notified(), want)
	}
}

func TestOnceTellsNoOne(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	h.running(nginx())
	h.once(t)
	if got := h.notified(); len(got) != 0 {
		t.Errorf("notified %q", got)
	}
}

func TestAFailureToTellSystemdIsAnErrorButNotAStop(t *testing.T) {
	h := newHarness(t, fakes.Quirks{})
	h.daemon.notify = func(...string) error { return errors.New("sd_notify: connection refused") }
	h.running(nginx())
	stop := h.run(t)
	await.Until(t, "the stream", func() bool { return h.source.Open() == 1 })
	stop()

	if !h.log.ErrorContains("Error: sd_notify: connection refused") {
		t.Errorf("log:\n%s", h.log.Text())
	}
	if len(h.router.Mappings()) != 1 {
		t.Errorf("got %+v", h.router.Mappings())
	}
}
