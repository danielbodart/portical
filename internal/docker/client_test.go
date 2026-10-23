package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/danielbodart/gokit/httpx"
	"github.com/danielbodart/portical/internal/fakes"
	"github.com/danielbodart/portical/internal/source"
)

var ctx = context.Background()

var drivers = map[string]string{"bridge": "bridge", "lan": "macvlan", "proxy": "bridge", "internal": "bridge"}

// fromAPI is toContainer on a container as the Engine API writes it.
func fromAPI(t *testing.T, captured string) Container {
	t.Helper()
	var raw rawContainer
	if err := json.Unmarshal([]byte(captured), &raw); err != nil {
		t.Fatal(err)
	}
	return toContainer(raw, drivers)
}

func TestToContainerStripsDockersLeadingSlashFromTheName(t *testing.T) {
	if got := fromAPI(t, `{"Id":"abc","Names":["/nginx"]}`).Name; got != "nginx" {
		t.Errorf("got %q", got)
	}
}

func TestToContainerFallsBackToTheIdWhenAContainerHasNoName(t *testing.T) {
	if got := fromAPI(t, `{"Id":"abc"}`).Name; got != "abc" {
		t.Errorf("got %q", got)
	}
}

func TestToContainerJoinsEachNetworkToItsDriver(t *testing.T) {
	got := fromAPI(t, `{"Id":"abc","NetworkSettings":{"Networks":{
		"proxy":{"IPAddress":"172.18.0.2"},"lan":{"IPAddress":"192.168.1.40"}}}}`).Networks
	want := []Network{network("lan", "macvlan", "192.168.1.40"), network("proxy", "bridge", "172.18.0.2")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v", got)
	}
}

// A network attached by id, or removed between listing containers and
// listing networks, has no driver. Calling that "bridge" would send traffic
// somewhere nobody asked for, so it stays unknown and gets skipped.
func TestToContainerMarksANetworkWithNoKnownDriverAsUnknown(t *testing.T) {
	got := fromAPI(t, `{"Id":"abc","NetworkSettings":{"Networks":{"vanished":{}}}}`).Networks
	if want := []Network{network("vanished", "unknown", "")}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v", got)
	}
}

func TestToContainerReadsHostAndContainerPortWithProtocol(t *testing.T) {
	got := fromAPI(t, `{"Id":"abc","Ports":[{"PrivatePort":80,"PublicPort":8888,"Type":"tcp"}]}`).Published
	if want := []PublishedPort{port(8888, 80, "tcp")}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v", got)
	}
}

// Docker reports one `-p 8888:80` twice, once for 0.0.0.0 and once for ::.
// Left alone that is a duplicate rule for every published port.
func TestToContainerCollapsesTheIPv4AndIPv6EntriesForOneBinding(t *testing.T) {
	got := fromAPI(t, `{"Id":"abc","Ports":[
		{"IP":"0.0.0.0","PrivatePort":80,"PublicPort":8888,"Type":"tcp"},
		{"IP":"::","PrivatePort":80,"PublicPort":8888,"Type":"tcp"}]}`).Published
	if len(got) != 1 {
		t.Errorf("got %+v", got)
	}
}

func TestToContainerIgnoresAPortThatIsExposedButNotPublished(t *testing.T) {
	if got := fromAPI(t, `{"Id":"abc","Ports":[{"PrivatePort":80,"Type":"tcp"}]}`).Published; len(got) != 0 {
		t.Errorf("got %+v", got)
	}
}

func TestToContainerKeepsTCPAndUDPOnTheSameHostPortApart(t *testing.T) {
	got := fromAPI(t, `{"Id":"abc","Ports":[
		{"PrivatePort":53,"PublicPort":53,"Type":"tcp"},{"PrivatePort":53,"PublicPort":53,"Type":"udp"}]}`).Published
	if len(got) != 2 {
		t.Errorf("got %+v", got)
	}
}

func TestToContainerIgnoresAProtocolThatIsNeitherTCPNorUDP(t *testing.T) {
	if got := fromAPI(t, `{"Id":"abc","Ports":[{"PrivatePort":132,"PublicPort":132,"Type":"sctp"}]}`).Published; len(got) != 0 {
		t.Errorf("got %+v", got)
	}
}

func TestToContainerDefaultsLabelsAndNetworksWhenDockerOmitsThem(t *testing.T) {
	got := fromAPI(t, `{"Id":"abc"}`)
	if got.Labels == nil || len(got.Labels) != 0 || got.Networks == nil || len(got.Networks) != 0 || got.Published == nil || len(got.Published) != 0 {
		t.Errorf("got %+v", got)
	}
}

func TestContainersListsOnlyLabelledContainersWithTheirNetworksDrivers(t *testing.T) {
	docker := newFakeDocker()
	docker.given(
		publishing(container("nginx", map[string]string{ForwardLabel: "published"}, network("lan", "macvlan", "192.168.1.40")), port(8888, 80, "tcp")),
		container("other", map[string]string{}),
	)

	got, err := docker.client().Containers(ctx, ForwardLabel)
	if err != nil {
		t.Fatal(err)
	}
	want := []Container{{
		ID: "id-nginx", Name: "nginx", Labels: map[string]string{ForwardLabel: "published"},
		Networks:  []Network{network("lan", "macvlan", "192.168.1.40")},
		Published: []PublishedPort{port(8888, 80, "tcp")},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v", got)
	}
}

func TestContainersSaysWhichCallFailedAndWhy(t *testing.T) {
	docker := newFakeDocker()
	docker.fail("Cannot connect to the Docker daemon")

	_, err := docker.client().Containers(ctx, ForwardLabel)

	if err == nil ||
		!strings.Contains(err.Error(), "Docker API /v1.41/containers/json failed") ||
		!strings.Contains(err.Error(), "Cannot connect to the Docker daemon") {
		t.Errorf("got %v", err)
	}
}

// events opens the client's event stream in the background, collecting
// what it is told.
type events struct {
	got   chan Event
	ended chan error
}

func listen(t *testing.T, ctx context.Context, docker *fakeDocker) *events {
	t.Helper()
	e := &events{got: make(chan Event, 16), ended: make(chan error, 1)}
	go func() { e.ended <- docker.client().Events(ctx, ForwardLabel, func(event Event) { e.got <- event }) }()
	fakes.Await(t, "the event stream to open", func() bool { return docker.open() == 1 })
	return e
}

func (e *events) next(t *testing.T) Event {
	t.Helper()
	select {
	case event := <-e.got:
		return event
	case err := <-e.ended:
		t.Fatalf("stream ended with %v", err)
	case <-t.Context().Done():
	}
	return Event{}
}

func (e *events) end(t *testing.T) error {
	t.Helper()
	select {
	case err := <-e.ended:
		return err
	case event := <-e.got:
		t.Fatalf("got %+v instead of the stream ending", event)
	}
	return nil
}

func TestEventsReportsEachContainerByNameAsItHappens(t *testing.T) {
	docker := newFakeDocker()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := listen(t, ctx, docker)

	docker.startContainer(container("nginx", map[string]string{ForwardLabel: "80"}))
	if got := stream.next(t); got != (Event{Action: "start", Container: "nginx"}) {
		t.Errorf("got %+v", got)
	}
	docker.stop("nginx")
	if got := stream.next(t); got != (Event{Action: "die", Container: "nginx"}) {
		t.Errorf("got %+v", got)
	}
}

// Asked of Docker rather than filtered here, so a busy host's unrelated
// containers never reach Portical at all.
func TestEventsAsksOnlyAboutLabelledContainers(t *testing.T) {
	docker := newFakeDocker()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := listen(t, ctx, docker)

	docker.startContainer(container("unrelated", map[string]string{}))
	docker.startContainer(container("nginx", map[string]string{ForwardLabel: "80"}))

	if got := stream.next(t); got.Container != "nginx" {
		t.Errorf("got %+v", got)
	}
}

// The daemon cancels to shut down, and a stream that outlived it would hold
// the socket open and leak a goroutine for every reconnect.
func TestEventsReturnsNilAndClosesTheStreamWhenCancelled(t *testing.T) {
	docker := newFakeDocker()
	ctx, cancel := context.WithCancel(ctx)
	stream := listen(t, ctx, docker)

	cancel()

	if err := stream.end(t); err != nil {
		t.Errorf("got %v", err)
	}
	fakes.Await(t, "Docker to see the stream closed", func() bool { return docker.open() == 0 })
}

// Reopening is the daemon's business; the stream only has to stop.
func TestEventsReturnsWhenDockerDropsTheStream(t *testing.T) {
	docker := newFakeDocker()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := listen(t, ctx, docker)

	docker.dropStream()

	if err := stream.end(t); err != nil {
		t.Errorf("got %v", err)
	}
}

func TestEventsFailsWhenTheStreamCannotBeOpened(t *testing.T) {
	broken := NewClient(httpx.RoundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial unix /var/run/docker.sock: connect: no such file or directory")
	}))
	err := broken.Events(ctx, ForwardLabel, func(Event) { t.Error("notified") })
	if err == nil || !strings.Contains(err.Error(), "Docker API /v1.41/events failed") {
		t.Errorf("got %v", err)
	}
}

func TestRunInNetworkOfRunsTheImageJoinedToTheContainersNetwork(t *testing.T) {
	docker := newFakeDocker()
	docker.relay = func(call relayCall) (string, int) { return "hello from " + call.Container, 0 }

	output, err := docker.client().RunInNetworkOf(ctx, "nginx", "portical:dev", []string{"relay", "abc"})

	if err != nil || output != "hello from nginx" {
		t.Fatalf("got %q, %v", output, err)
	}
	want := []relayCall{{Container: "nginx", Image: "portical:dev", Command: []string{"relay", "abc"}, Tty: true}}
	if got := docker.relayedCalls(); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v", got)
	}
	if docker.leftBehind() != 0 {
		t.Error("left the helper container behind")
	}
}

func TestRunInNetworkOfReportsAFailedHelperWithWhatItSaidAndStillRemovesIt(t *testing.T) {
	docker := newFakeDocker()
	docker.relay = func(relayCall) (string, int) { return "exec: relay: not found\n", 127 }

	_, err := docker.client().RunInNetworkOf(ctx, "nginx", "portical:dev", []string{"relay", "abc"})

	if err == nil || err.Error() != "portical:dev exited with status 127: exec: relay: not found" {
		t.Errorf("got %v", err)
	}
	if docker.leftBehind() != 0 {
		t.Error("left the helper container behind")
	}
}

const mountinfo = `1234 1200 0:31 /var/lib/docker/containers/` +
	`0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef/hostname /etc/hostname rw,relatime - ext4 /dev/sda1 rw
1235 1200 0:31 / / rw,relatime - overlay overlay rw`

func TestOwnImageAsksDockerAboutTheContainerItsMountsCameFrom(t *testing.T) {
	docker := newFakeDocker()
	docker.images["0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"] = "ghcr.io/someone/portical:dev"

	if got := docker.client().OwnImage(ctx, []byte(mountinfo)); got != "ghcr.io/someone/portical:dev" {
		t.Errorf("got %q", got)
	}
}

func TestOwnImageIsUnknownOutsideAContainer(t *testing.T) {
	if got := newFakeDocker().client().OwnImage(ctx, []byte("1235 1200 0:31 / / rw - ext4 /dev/sda1 rw")); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestOwnImageIsUnknownWhenDockerDeclinesToSay(t *testing.T) {
	if got := newFakeDocker().client().OwnImage(ctx, []byte(mountinfo)); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestSourceWantsWhatTheLabelledContainersAsk(t *testing.T) {
	docker := newFakeDocker()
	docker.given(container("nginx", map[string]string{ForwardLabel: "9999:80/tcp"}))

	desired, err := NewSource(docker.client(), Options{HostAddress: fakes.HostAddress}).Desired(ctx)

	if err != nil || len(desired.Forwards) != 1 || desired.Forwards[0].InternalClient != fakes.HostAddress {
		t.Errorf("got %+v, %v", desired, err)
	}
}

func TestSourceReadsAnotherLabelWhenTold(t *testing.T) {
	docker := newFakeDocker()
	docker.given(
		container("nginx", map[string]string{ForwardLabel: "9999:80/tcp"}),
		container("custom", map[string]string{"my.forward": "8080/tcp"}),
	)

	desired, err := NewSource(docker.client(), Options{Label: "my.forward"}).Desired(ctx)

	if err != nil || len(desired.Forwards) != 1 || desired.Forwards[0].Owner != "custom" {
		t.Errorf("got %+v, %v", desired, err)
	}
}

func TestSourceDescribesEachEventByContainer(t *testing.T) {
	docker := newFakeDocker()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	got := make(chan source.Event, 1)
	go NewSource(docker.client(), Options{}).Events(ctx, func(event source.Event) { got <- event })
	fakes.Await(t, "the event stream to open", func() bool { return docker.open() == 1 })

	docker.startContainer(container("nginx", map[string]string{ForwardLabel: "80"}))

	if event := <-got; event.Description != "Container nginx start" {
		t.Errorf("got %+v", event)
	}
}

// The same client over a real unix socket and a real HTTP server, as it
// reaches Docker: events must arrive one at a time on a response that stays
// open, and cancelling must close it.
func TestEventsStreamOverAUnixSocket(t *testing.T) {
	docker := newFakeDocker()
	path := filepath.Join(t.TempDir(), "docker.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: docker}
	go server.Serve(listener)
	defer server.Close()
	docker.given(container("nginx", map[string]string{ForwardLabel: "80"}))
	client := NewClient(httpx.UnixSocket(path))

	if found, err := client.Containers(ctx, ForwardLabel); err != nil || len(found) != 1 {
		t.Fatalf("got %+v, %v", found, err)
	}

	ctx, cancel := context.WithCancel(ctx)
	got, ended := make(chan Event, 1), make(chan error, 1)
	go func() { ended <- client.Events(ctx, ForwardLabel, func(event Event) { got <- event }) }()
	fakes.Await(t, "the event stream to open", func() bool { return docker.open() == 1 })

	docker.stop("nginx")
	if event := <-got; event != (Event{Action: "die", Container: "nginx"}) {
		t.Errorf("got %+v", event)
	}
	cancel()
	if err := <-ended; err != nil {
		t.Errorf("got %v", err)
	}
	fakes.Await(t, "Docker to see the stream closed", func() bool { return docker.open() == 0 })
}
