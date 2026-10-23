package docker

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danielbodart/gokit/clock"
	"github.com/danielbodart/gokit/httpx"
	"github.com/danielbodart/portical/internal/daemon"
	"github.com/danielbodart/portical/internal/fakes"
	"github.com/danielbodart/portical/internal/upnp"
)

const containerIP = "192.168.1.40"

// framed is what the relay command prints around its result.
func framed(status int, body string) string {
	data, _ := json.Marshal(RelayedResponse{Status: status, Body: body})
	return relayStart + string(data) + relayEnd
}

func TestRelayEncodingSurvivesASOAPBodyWithQuotesAngleBracketsAndNewlines(t *testing.T) {
	body := `<?xml version="1.0"?>` + "\n" + `<s:Envelope><u:Add x="1">it's</u:Add></s:Envelope>`
	request, _ := http.NewRequest(http.MethodPost, fakes.ControlURL, strings.NewReader(body))
	request.Header.Set("SOAPAction", `"urn:x#AddPortMapping"`)

	relayed, err := ToRelayedRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	round, err := DecodeRelay(EncodeRelay(relayed))

	if err != nil || round.Body != body || round.Method != http.MethodPost || round.URL != fakes.ControlURL ||
		round.Headers["soapaction"] != `"urn:x#AddPortMapping"` {
		t.Errorf("got %+v, %v", round, err)
	}
}

func TestRelayOutputIsReadOutOfSurroundingNoise(t *testing.T) {
	output := "some runtime warning\n" + framed(200, "ok") + "\ntrailing"
	if got, err := ParseRelayOutput(output); err != nil || got != (RelayedResponse{Status: 200, Body: "ok"}) {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestRelayOutputComplainsUsefullyWhenTheContainerPrintedNothingUsable(t *testing.T) {
	if _, err := ParseRelayOutput("exec: not found\n"); err == nil || err.Error() != "relay produced no usable result: exec: not found" {
		t.Errorf("got %v", err)
	}
}

func TestRunRelayPerformsTheRequestAndFramesWhatCameBack(t *testing.T) {
	var asked *http.Request
	gateway := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		asked = r
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "<fault>"+string(body)+"</fault>")
	})
	request, _ := http.NewRequest(http.MethodPost, fakes.ControlURL, strings.NewReader("<x/>"))
	request.Header.Set("SOAPAction", `"urn:x#AddPortMapping"`)
	relayed, _ := ToRelayedRequest(request)

	var stdout bytes.Buffer
	code := RunRelay(EncodeRelay(relayed), &stdout, httpx.HandlerTransport(gateway))

	got, err := ParseRelayOutput(stdout.String())
	if code != 0 || err != nil || got != (RelayedResponse{Status: 500, Body: "<fault><x/></fault>"}) {
		t.Fatalf("got %d %+v, %v", code, got, err)
	}
	if asked.Header.Get("SOAPAction") != `"urn:x#AddPortMapping"` || asked.URL.Path != "/ctl/IPConn" {
		t.Errorf("asked %v %v", asked.URL, asked.Header)
	}
}

// Reported as a response, so the caller learns why the gateway could not be
// reached from inside the container, rather than that the container failed.
func TestRunRelayReportsAnUnreachableGatewayAsItsResult(t *testing.T) {
	unreachable := httpx.RoundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	})
	relayed := EncodeRelay(RelayedRequest{URL: fakes.ControlURL, Method: http.MethodPost, Body: "<x/>"})

	var stdout bytes.Buffer
	code := RunRelay(relayed, &stdout, unreachable)

	got, err := ParseRelayOutput(stdout.String())
	if code != 0 || err != nil || got.Status != 599 || !strings.Contains(got.Body, "relay failed:") {
		t.Errorf("got %d %+v, %v", code, got, err)
	}
}

func TestRunRelayReportsAPayloadItCannotRead(t *testing.T) {
	var stdout bytes.Buffer
	RunRelay("not base64!", &stdout, nil)
	if got, err := ParseRelayOutput(stdout.String()); err != nil || got.Status != 599 {
		t.Errorf("got %+v, %v", got, err)
	}
}

// The relay transport is an ordinary http.RoundTripper, so a gateway built on
// it behaves exactly like one talking over TCP. That is the whole point: the
// UPnP client, reconcile and the daemon never learn a container was involved.
func relaying(t *testing.T) *fakeDocker {
	t.Helper()
	docker := newFakeDocker()
	docker.relay = func(relayCall) (string, int) { return framed(200, "<ok/>"), 0 }
	return docker
}

func post(t *testing.T, transport http.RoundTripper) *http.Response {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPost, fakes.ControlURL, strings.NewReader("<x/>"))
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestViaContainerPerformsTheRequestFromInsideTheNamedContainer(t *testing.T) {
	docker := relaying(t)

	post(t, ViaContainer(docker.client(), "nginx", DefaultHelperImage)).Body.Close()

	calls := docker.relayedCalls()
	if len(calls) != 1 || calls[0].Container != "nginx" || calls[0].Image != DefaultHelperImage || calls[0].Command[0] != "relay" {
		t.Errorf("got %+v", calls)
	}
}

func TestViaContainerUsesTheImageItWasGiven(t *testing.T) {
	docker := relaying(t)
	post(t, ViaContainer(docker.client(), "nginx", "ghcr.io/someone/portical:dev")).Body.Close()
	if calls := docker.relayedCalls(); calls[0].Image != "ghcr.io/someone/portical:dev" {
		t.Errorf("got %+v", calls)
	}
}

func TestViaContainerPassesTheRelayedStatusBackThrough(t *testing.T) {
	docker := relaying(t)
	docker.relay = func(relayCall) (string, int) { return framed(500, "<fault/>"), 0 }

	response := post(t, ViaContainer(docker.client(), "nginx", DefaultHelperImage))
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)

	if response.StatusCode != 500 || string(body) != "<fault/>" {
		t.Errorf("got %d %q", response.StatusCode, body)
	}
}

func TestViaContainerFailsTheRequestWhenTheHelperCannotRun(t *testing.T) {
	docker := relaying(t)
	docker.relay = func(relayCall) (string, int) { return "Unable to find image\n", 125 }

	request, _ := http.NewRequest(http.MethodPost, fakes.ControlURL, strings.NewReader("<x/>"))
	_, err := (&http.Client{Transport: ViaContainer(docker.client(), "nginx", DefaultHelperImage)}).Do(request)

	if err == nil || !strings.Contains(err.Error(), "exited with status 125: Unable to find image") {
		t.Errorf("got %v", err)
	}
}

func TestAsContainerDefaultsToPorticalsOwnPublishedImage(t *testing.T) {
	docker := relaying(t)
	gateway := upnp.New(nil, fakes.ControlURL, fakes.ServiceType)

	AsContainer(docker.client(), gateway, "")("nginx").Remove(ctx, 8080, "tcp")

	if calls := docker.relayedCalls(); len(calls) != 1 || calls[0].Image != DefaultHelperImage || calls[0].Container != "nginx" {
		t.Errorf("got %+v", calls)
	}
}

// secureMode is the end of issue #1 and #6's story for macvlan.
//
// miniupnpd's secure_mode - the OpenWrt default, confirmed on a real gateway -
// only lets a client map to its own address. A macvlan container's rule names
// the container's address, so asking from the host is refused with 718. v1
// sidestepped this by running upnpc inside the container; this asks from
// inside the container whenever the rule points at the container's own
// address.
type secureMode struct {
	router *fakes.Gateway
	docker *fakeDocker
	daemon *daemon.Daemon
}

func newSecureMode(t *testing.T, secure bool) *secureMode {
	t.Helper()
	h := &secureMode{router: fakes.NewGateway(fakes.Quirks{SecureMode: secure}), docker: newFakeDocker()}
	gateway, err := upnp.At(ctx, h.router.Client(), fakes.RootURL)
	if err != nil {
		t.Fatal(err)
	}
	// The helper reaches the same router, but its request arrives from the
	// container's own address.
	h.docker.relayThrough(h.router)
	client := h.docker.client()
	options := daemon.Defaults
	options.HostAddress = fakes.HostAddress
	h.daemon = daemon.New(daemon.Deps{
		Source:      NewSource(client, Options{HostAddress: fakes.HostAddress}),
		Gateway:     gateway,
		Options:     options,
		Log:         (&fakes.Log{}).Line,
		Clock:       clock.NewStopped(time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)),
		AsContainer: AsContainer(client, gateway, DefaultHelperImage),
	})
	h.docker.given(container("direct", forward("8080/tcp"), network("lan", "macvlan", containerIP)))
	return h
}

func (h *secureMode) once(t *testing.T) {
	t.Helper()
	if _, err := h.daemon.Once(ctx); err != nil {
		t.Fatal(err)
	}
}

func (h *secureMode) relayedFrom() []string {
	var containers []string
	for _, call := range h.docker.relayedCalls() {
		containers = append(containers, call.Container)
	}
	return containers
}

func TestSecureModeIsSatisfiedByAskingFromInsideTheContainer(t *testing.T) {
	h := newSecureMode(t, true)

	h.once(t)

	if got := h.router.Mappings(); len(got) != 1 || got[0].InternalClient != containerIP {
		t.Errorf("got %+v", got)
	}
	if h.docker.leftBehind() != 0 {
		t.Error("left helper containers behind")
	}
}

func TestSecureModeRelaysThroughTheContainerTheRuleBelongsTo(t *testing.T) {
	h := newSecureMode(t, true)
	h.once(t)
	if got := h.relayedFrom(); !slices.Equal(got, []string{"direct"}) {
		t.Errorf("got %v", got)
	}
}

// Decided by the driver, not by trying and reacting to a refusal. 718 also
// means a genuine conflict with someone else's mapping, so using it to mean
// "wrong vantage point" would start a container to retry something no
// vantage point can fix, and would hide the real reason.
func TestSecureModeOrNotAMacvlanRuleIsAskedFromTheContainer(t *testing.T) {
	h := newSecureMode(t, false)

	h.once(t)

	if got := h.router.Mappings(); len(got) != 1 || got[0].InternalClient != containerIP {
		t.Errorf("got %+v", got)
	}
	if got := h.relayedFrom(); !slices.Equal(got, []string{"direct"}) {
		t.Errorf("got %v", got)
	}
}

func TestSecureModeNeverStartsAContainerForABridgeNetworkedRule(t *testing.T) {
	h := newSecureMode(t, true)
	h.docker.given(container("web", forward("8081/tcp"), network("br", "bridge", "172.17.0.9")))

	h.once(t)

	if got := h.router.Mappings(); len(got) != 1 || got[0].InternalClient != fakes.HostAddress {
		t.Errorf("got %+v", got)
	}
	if got := h.relayedFrom(); len(got) != 0 {
		t.Errorf("got %v", got)
	}
}

func TestSecureModeDoesNotChurnOnceTheRuleIsInPlace(t *testing.T) {
	h := newSecureMode(t, true)
	h.once(t)
	h.router.ClearActions()
	h.docker.clearRelayed()

	h.once(t)

	if h.router.Received("AddPortMapping") != 0 || len(h.relayedFrom()) != 0 {
		t.Errorf("got %v and %v", h.router.Actions(), h.relayedFrom())
	}
}
