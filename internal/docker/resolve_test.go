package docker

import (
	"reflect"
	"slices"
	"testing"

	"github.com/danielbodart/portical/internal/model"
)

func forward(label string) map[string]string { return map[string]string{ForwardLabel: label} }

func TestResolveIgnoresAContainerWithoutTheLabel(t *testing.T) {
	if got := Resolve([]Container{container("other", map[string]string{})}, Options{}).Forwards; len(got) != 0 {
		t.Errorf("got %+v", got)
	}
}

func TestResolveIgnoresAContainerWhoseLabelIsBlank(t *testing.T) {
	if got := Resolve([]Container{container("other", forward("  "))}, Options{}).Forwards; len(got) != 0 {
		t.Errorf("got %+v", got)
	}
}

func TestResolveTurnsALabelIntoAForwardAgainstTheHost(t *testing.T) {
	got := Resolve([]Container{container("nginx", forward("9999:80/tcp"))}, Options{HostAddress: "192.168.1.5"}).Forwards
	want := []model.DesiredForward{{
		Rule:   model.Rule{ExternalPort: 9999, InternalPort: 80, Protocol: model.TCP},
		Owner:  "nginx",
		Target: model.Host, InternalClient: "192.168.1.5",
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v", got)
	}
}

func TestResolveReportsARuleItCouldNotParseWithoutDroppingTheContainer(t *testing.T) {
	got := Resolve([]Container{container("nginx", forward("nonsense,8080/tcp"))}, Options{})
	if len(got.Forwards) != 1 || !slices.Equal(got.Warnings, []string{"nginx: ignoring unrecognised rule 'nonsense'"}) {
		t.Errorf("got %+v", got)
	}
}

func TestResolveReadsAnotherLabelWhenTold(t *testing.T) {
	got := Resolve([]Container{container("nginx", map[string]string{"my.forward": "80/tcp"})}, Options{Label: "my.forward"})
	if len(got.Forwards) != 1 {
		t.Errorf("got %+v", got)
	}
}

func TestPublishedForwardsTheHostPortToItself(t *testing.T) {
	got := Resolve([]Container{publishing(container("minecraft", forward("published")), port(25565, 25565, model.TCP))}, Options{}).Forwards
	if len(got) != 1 || got[0].Rule != (model.Rule{ExternalPort: 25565, InternalPort: 25565, Protocol: model.TCP}) {
		t.Errorf("got %+v", got)
	}
}

// The gateway forwards to the host; Docker's own published-port mapping
// takes it on to the container. The container port is Docker's business.
func TestPublishedIgnoresTheContainerPortWhenItDiffersFromTheHostPort(t *testing.T) {
	got := Resolve([]Container{publishing(container("nginx", forward("published")), port(8888, 80, model.TCP))}, Options{}).Forwards
	if len(got) != 1 || got[0].Rule != (model.Rule{ExternalPort: 8888, InternalPort: 8888, Protocol: model.TCP}) {
		t.Errorf("got %+v", got)
	}
}

func TestPublishedYieldsNothingWhenTheContainerPublishesNothing(t *testing.T) {
	if got := Resolve([]Container{container("nginx", forward("published"))}, Options{}).Forwards; len(got) != 0 {
		t.Errorf("got %+v", got)
	}
}

func TestPublishedLosesACollisionToAnExplicitRule(t *testing.T) {
	got := Resolve([]Container{publishing(container("nginx", forward("published,8888:80/tcp")), port(8888, 80, model.TCP))}, Options{}).Forwards
	if len(got) != 1 || got[0].Rule.InternalPort != 80 {
		t.Errorf("got %+v", got)
	}
}

func TestResolveForwardsStraightToAMacvlanContainersOwnAddress(t *testing.T) {
	got := Resolve([]Container{container("nginx", forward("80/tcp"), network("lan", "macvlan", "192.168.1.40"))}, Options{HostAddress: "192.168.1.5"}).Forwards
	if got[0].Target != model.Container("lan") || got[0].InternalClient != "192.168.1.40" {
		t.Errorf("got %+v", got[0])
	}
}

func TestResolveForwardsAnIpvlanContainerTheSameWay(t *testing.T) {
	got := Resolve([]Container{container("nginx", forward("80/tcp"), network("lan", "ipvlan", "192.168.1.41"))}, Options{}).Forwards
	if got[0].Target != model.Container("lan") {
		t.Errorf("got %+v", got[0])
	}
}

func TestResolveForwardsAHostNetworkedContainerToTheHost(t *testing.T) {
	got := Resolve([]Container{container("nginx", forward("80/tcp"), network("host", "host", ""))}, Options{HostAddress: "192.168.1.5"}).Forwards
	if got[0].Target != model.Host || got[0].InternalClient != "192.168.1.5" {
		t.Errorf("got %+v", got[0])
	}
}

// The bridge address is not routable from the gateway, so it must never
// become the target - the host is.
func TestResolveNeverTargetsABridgeContainersOwnAddress(t *testing.T) {
	got := Resolve([]Container{container("nginx", forward("80/tcp"), network("bridge", "bridge", "172.17.0.2"))}, Options{}).Forwards
	if got[0].InternalClient != "" {
		t.Errorf("got %+v", got[0])
	}
}

func TestResolveSkipsAContainerOnAnUnsupportedDriverSayingWhich(t *testing.T) {
	got := Resolve([]Container{container("nginx", forward("80/tcp"), network("mesh", "overlay", ""))}, Options{})
	if len(got.Forwards) != 0 || !slices.Equal(got.Warnings, []string{"nginx: has no supported network driver (found: overlay), skipping"}) {
		t.Errorf("got %+v", got)
	}
}

func TestResolveSkipsAContainerAttachedToNoNetworkAtAll(t *testing.T) {
	bare := container("nginx", forward("80/tcp"))
	bare.Networks = nil
	got := Resolve([]Container{bare}, Options{})
	if len(got.Forwards) != 0 || !slices.Equal(got.Warnings, []string{"nginx: is not attached to any network, skipping"}) {
		t.Errorf("got %+v", got)
	}
}

func TestResolveSkipsANamedNetworkWithAnUnsupportedDriver(t *testing.T) {
	labels := forward("80/tcp")
	labels[NetworkLabel] = "mesh"
	got := Resolve([]Container{container("nginx", labels, network("mesh", "overlay", ""), network("bridge", "bridge", ""))}, Options{})
	if len(got.Forwards) != 0 || !slices.Equal(got.Warnings, []string{"nginx: network 'mesh' has unsupported driver 'overlay', skipping"}) {
		t.Errorf("got %+v", got)
	}
}

// Issue #1. v1's Go template concatenated every network name into one
// nonsense string, the driver lookup failed, and every rule was skipped with
// "Unsupported network driver: ". This is the reverse-proxy setup from that
// issue: a container on many networks, one of which reaches the LAN.
func traefik(labels ...string) Container {
	all := forward("80/tcp,443/tcp")
	for i := 0; i+1 < len(labels); i += 2 {
		all[labels[i]] = labels[i+1]
	}
	return container("traefik", all,
		network("networka", "bridge", "172.18.0.2"),
		network("networkb", "bridge", "172.19.0.2"),
		network("lan", "macvlan", "192.168.1.40"),
		network("networkc", "bridge", "172.20.0.2"),
	)
}

func TestSeveralNetworksResolveInsteadOfFailing(t *testing.T) {
	got := Resolve([]Container{traefik()}, Options{})
	if len(got.Forwards) != 2 || len(got.Warnings) != 0 {
		t.Errorf("got %+v", got)
	}
}

func TestSeveralNetworksPreferTheOneThatReachesTheLAN(t *testing.T) {
	got := Resolve([]Container{traefik()}, Options{}).Forwards[0]
	if got.Target != model.Container("lan") || got.InternalClient != "192.168.1.40" {
		t.Errorf("got %+v", got)
	}
}

func TestSeveralNetworksFallBackToTheHostWhenEveryNetworkIsABridge(t *testing.T) {
	got := Resolve([]Container{container("nginx", forward("80/tcp"),
		network("networka", "bridge", "172.18.0.2"), network("networkb", "bridge", "172.19.0.2"))},
		Options{HostAddress: "192.168.1.5"}).Forwards[0]
	if got.Target != model.Host || got.InternalClient != "192.168.1.5" {
		t.Errorf("got %+v", got)
	}
}

func TestSeveralNetworksHonourAnExplicitlyNamedNetwork(t *testing.T) {
	if got := Resolve([]Container{traefik(NetworkLabel, "networkb")}, Options{}).Forwards[0]; got.Target != model.Host {
		t.Errorf("got %+v", got)
	}
}

func TestSeveralNetworksReadAnotherNetworkLabelWhenTold(t *testing.T) {
	if got := Resolve([]Container{traefik("my.network", "networkb")}, Options{NetworkLabel: "my.network"}).Forwards[0]; got.Target != model.Host {
		t.Errorf("got %+v", got)
	}
}

func TestSeveralNetworksSkipTheContainerWhenTheNamedNetworkIsNotAttached(t *testing.T) {
	got := Resolve([]Container{traefik(NetworkLabel, "absent")}, Options{})
	want := "traefik: is not attached to network 'absent' (has: networka, networkb, lan, networkc), skipping"
	if len(got.Forwards) != 0 || len(got.Warnings) == 0 || got.Warnings[0] != want {
		t.Errorf("got %+v", got)
	}
}

func TestSeveralNetworksWarnAndPickDeterministicallyWhenTwoTie(t *testing.T) {
	got := Resolve([]Container{container("nginx", forward("80/tcp"),
		network("zeta", "macvlan", "192.168.1.50"), network("alpha", "macvlan", "192.168.1.51"))}, Options{})
	want := "nginx: is on 2 macvlan networks (alpha, zeta), forwarding to 'alpha' - set " + NetworkLabel + " to choose"
	if got.Forwards[0].InternalClient != "192.168.1.51" || len(got.Warnings) == 0 || got.Warnings[0] != want {
		t.Errorf("got %+v", got)
	}
}

// An unstable choice would flip the target between passes and rewrite the
// rule forever, which is the churn issue #6 is about arriving another way.
func TestSeveralNetworksMakeTheSameChoiceHoweverDockerOrdersThem(t *testing.T) {
	a, z := network("alpha", "macvlan", "192.168.1.51"), network("zeta", "macvlan", "192.168.1.50")
	first := Resolve([]Container{container("nginx", forward("80/tcp"), a, z)}, Options{}).Forwards
	second := Resolve([]Container{container("nginx", forward("80/tcp"), z, a)}, Options{}).Forwards
	if !reflect.DeepEqual(first, second) {
		t.Errorf("got %+v and %+v", first, second)
	}
}

func TestResolveResolvesSeveralContainersInOnePass(t *testing.T) {
	got := Resolve([]Container{container("nginx", forward("80/tcp")), container("minecraft", forward("25565"))}, Options{}).Forwards
	var owners []string
	for _, forward := range got {
		owners = append(owners, forward.Owner)
	}
	if !slices.Equal(owners, []string{"nginx", "minecraft", "minecraft"}) {
		t.Errorf("got %v", owners)
	}
}
