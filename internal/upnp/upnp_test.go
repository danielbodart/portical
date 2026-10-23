package upnp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/danielbodart/gokit/httpx"
	"github.com/danielbodart/portical/internal/fakes"
	"github.com/danielbodart/portical/internal/model"
)

var ctx = context.Background()

func connect(t *testing.T, quirks fakes.Quirks) (*Client, *fakes.Gateway) {
	t.Helper()
	fake := fakes.NewGateway(quirks)
	gateway, err := At(ctx, fake.Client(), fakes.RootURL)
	if err != nil {
		t.Fatal(err)
	}
	return gateway, fake
}

func serving(handler http.HandlerFunc) *http.Client {
	return &http.Client{Transport: httpx.HandlerTransport(handler)}
}

func notAGateway(w http.ResponseWriter, _ *http.Request) {
	io.WriteString(w, `<root><device><serviceList><service>
		<serviceType>urn:schemas-upnp-org:service:Layer3Forwarding:1</serviceType>
		<controlURL>/ctl/L3F</controlURL></service></serviceList></device></root>`)
}

func TestAtFindsThePortForwardingServiceInADeviceDescription(t *testing.T) {
	gateway, _ := connect(t, fakes.Quirks{})
	if gateway.ControlURL != "http://192.168.1.1:5000/ctl/IPConn" || gateway.ServiceType != "urn:schemas-upnp-org:service:WANIPConnection:1" {
		t.Errorf("got %s %s", gateway.ControlURL, gateway.ServiceType)
	}
}

// Routers that omit URLBase expect control URLs resolved against wherever the
// description was fetched from, which is not always the root.
func TestAtResolvesARelativeControlURLAgainstTheDescriptionsOwnURL(t *testing.T) {
	client := serving(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `<root><service><serviceType>urn:schemas-upnp-org:service:WANIPConnection:1</serviceType>
			<controlURL>ctl/IPConn</controlURL></service></root>`)
	})
	gateway, err := At(ctx, client, "http://192.168.1.1:5000/upnp/rootDesc.xml")
	if err != nil {
		t.Fatal(err)
	}
	if gateway.ControlURL != "http://192.168.1.1:5000/upnp/ctl/IPConn" {
		t.Errorf("got %s", gateway.ControlURL)
	}
}

func TestAtResolvesAgainstURLBaseWhenThereIsOne(t *testing.T) {
	client := serving(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `<root><URLBase> http://10.0.0.1:49152/ </URLBase>
			<service><serviceType>urn:schemas-upnp-org:service:WANPPPConnection:1</serviceType>
			<controlURL>/ctl/PPP</controlURL></service></root>`)
	})
	gateway, err := At(ctx, client, "http://192.168.1.1:5000/rootDesc.xml")
	if err != nil {
		t.Fatal(err)
	}
	if gateway.ControlURL != "http://10.0.0.1:49152/ctl/PPP" || gateway.ServiceType != "urn:schemas-upnp-org:service:WANPPPConnection:1" {
		t.Errorf("got %s %s", gateway.ControlURL, gateway.ServiceType)
	}
}

func TestAtPrefersTheBestServiceWhateverOrderTheyAreListedIn(t *testing.T) {
	client := serving(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `<root>
			<service><serviceType>urn:schemas-upnp-org:service:WANPPPConnection:1</serviceType><controlURL>/ppp</controlURL></service>
			<service><serviceType>urn:schemas-upnp-org:service:WANIPConnection:2</serviceType><controlURL>/ip2</controlURL></service>
		</root>`)
	})
	gateway, err := At(ctx, client, "http://192.168.1.1/d.xml")
	if err != nil {
		t.Fatal(err)
	}
	if gateway.ControlURL != "http://192.168.1.1/ip2" {
		t.Errorf("got %s", gateway.ControlURL)
	}
}

func TestAtSaysSoPlainlyWhenTheDeviceForwardsNoPorts(t *testing.T) {
	_, err := At(ctx, serving(notAGateway), "http://192.168.1.1:5000/rootDesc.xml")
	if err == nil || !strings.Contains(err.Error(), "offers no port forwarding service") {
		t.Errorf("got %v", err)
	}
}

func TestAtReportsAnUnreachableGatewayRatherThanHanging(t *testing.T) {
	client := serving(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "nope", http.StatusNotFound) })
	_, err := At(ctx, client, "http://192.168.1.1:5000/rootDesc.xml")
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("got %v", err)
	}
}

func mappings(t *testing.T, gateway Gateway) []model.Mapping {
	t.Helper()
	found, err := gateway.Mappings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func TestMappingsReadsAnEmptyTable(t *testing.T) {
	gateway, _ := connect(t, fakes.Quirks{})
	if got := mappings(t, gateway); len(got) != 0 {
		t.Errorf("got %v", got)
	}
}

func TestMappingsReadsEveryRow(t *testing.T) {
	gateway, fake := connect(t, fakes.Quirks{})
	fake.Given(model.Mapping{ExternalPort: 9999, Protocol: model.TCP, Description: "portical: one"})
	fake.Given(model.Mapping{ExternalPort: 19132, Protocol: model.UDP, Description: "portical: two"})
	if got := mappings(t, gateway); len(got) != 2 {
		t.Errorf("got %v", got)
	}
}

func TestMappingsReadsTheFieldsOfARow(t *testing.T) {
	gateway, fake := connect(t, fakes.Quirks{})
	want := model.Mapping{
		ExternalPort:   9999,
		Protocol:       model.TCP,
		InternalPort:   8080,
		InternalClient: "192.168.1.10",
		Description:    "portical: (9999:8080/tcp) nginx",
		LeaseDuration:  86400,
	}
	fake.Given(want)
	if got := mappings(t, gateway); !reflect.DeepEqual(got, []model.Mapping{want}) {
		t.Errorf("got %+v", got)
	}
}

// The specification says 713 ends the table, but firmwares in the wild answer
// 402 or 501 instead. Treating those as failures would leave Portical unable
// to read its own rules on those routers.
func TestMappingsTreatsEachEndOfTableCodeAsTheEnd(t *testing.T) {
	for _, code := range []int{713, 402, 501} {
		gateway, fake := connect(t, fakes.Quirks{EndOfTableCode: code})
		fake.Given(model.Mapping{ExternalPort: 9999, Protocol: model.TCP})
		if got := mappings(t, gateway); len(got) != 1 {
			t.Errorf("%d: got %v", code, got)
		}
	}
}

func TestMappingsFailsOnAnyOtherCode(t *testing.T) {
	gateway, fake := connect(t, fakes.Quirks{EndOfTableCode: 606})
	fake.Given(model.Mapping{ExternalPort: 9999, Protocol: model.TCP})
	_, err := gateway.Mappings(ctx)
	var upnp *Error
	if !errors.As(err, &upnp) || upnp.Code != 606 {
		t.Errorf("got %v", err)
	}
}

func TestMappingsStopsAtARouterThatNeverEndsItsTable(t *testing.T) {
	gateway, fake := connect(t, fakes.Quirks{EndlessTable: true})
	fake.Given(model.Mapping{ExternalPort: 9999, Protocol: model.TCP})
	if got := mappings(t, gateway); len(got) != MaxMappings {
		t.Errorf("got %d rows", len(got))
	}
}

func TestMappingsDecodesEntitiesInADescription(t *testing.T) {
	gateway, fake := connect(t, fakes.Quirks{})
	fake.Given(model.Mapping{ExternalPort: 80, Protocol: model.TCP, Description: `Bob & Alice's "rule"`})
	if got := mappings(t, gateway)[0].Description; got != `Bob & Alice's "rule"` {
		t.Errorf("got %q", got)
	}
}

// A row we cannot read correctly is one we might otherwise delete.
func TestToMappingDropsARowItCannotRead(t *testing.T) {
	for _, entry := range []string{
		"<NewProtocol>GRE</NewProtocol><NewExternalPort>80</NewExternalPort><NewInternalPort>80</NewInternalPort>",
		"<NewProtocol>TCP</NewProtocol><NewInternalPort>80</NewInternalPort>",
		"<NewProtocol>TCP</NewProtocol><NewExternalPort>8O</NewExternalPort><NewInternalPort>80</NewInternalPort>",
	} {
		if mapping, ok := toMapping(entry); ok {
			t.Errorf("read %+v from %s", mapping, entry)
		}
	}
}

func newMapping(description string) NewMapping {
	return NewMapping{
		ExternalPort:   9999,
		Protocol:       model.TCP,
		InternalPort:   80,
		InternalClient: "192.168.1.40",
		Description:    description,
	}
}

func TestAddCreatesAMapping(t *testing.T) {
	gateway, fake := connect(t, fakes.Quirks{})
	if err := gateway.Add(ctx, newMapping("portical: (9999:80/tcp) nginx")); err != nil {
		t.Fatal(err)
	}
	want := []model.Mapping{{
		ExternalPort:   9999,
		Protocol:       model.TCP,
		InternalPort:   80,
		InternalClient: "192.168.1.40",
		Description:    "portical: (9999:80/tcp) nginx",
	}}
	if got := fake.Mappings(); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v", got)
	}
}

func TestAddReplacesRatherThanDuplicatesAnExistingExternalPort(t *testing.T) {
	gateway, fake := connect(t, fakes.Quirks{})
	mapping := newMapping("portical: one")
	gateway.Add(ctx, mapping)
	mapping.InternalPort = 8080
	gateway.Add(ctx, mapping)
	if got := fake.Mappings(); len(got) != 1 || got[0].InternalPort != 8080 {
		t.Errorf("got %+v", got)
	}
}

// Container names may contain characters that would close the element early.
func TestAddEscapesADescriptionThatContainsMarkup(t *testing.T) {
	gateway, fake := connect(t, fakes.Quirks{})
	gateway.Add(ctx, newMapping(`portical: <a & b> "c"`))
	if got := fake.Mappings()[0].Description; got != `portical: <a & b> "c"` {
		t.Errorf("got %q", got)
	}
}

func TestAddReportsTheGatewaysOwnReasonForARefusal(t *testing.T) {
	gateway, _ := connect(t, fakes.Quirks{RefuseWith: 606})
	err := gateway.Add(ctx, newMapping("portical: nginx"))
	var upnp *Error
	if !errors.As(err, &upnp) || upnp.Code != 606 {
		t.Fatalf("got %v", err)
	}
	if want := "AddPortMapping failed: 606 Action not authorized - UPnP is enabled but not authorised for this client"; err.Error() != want {
		t.Errorf("got %q", err)
	}
}

func TestAddSendsItsArgumentsInTheOrderTheSpecificationGives(t *testing.T) {
	var body string
	client := serving(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		body = string(data)
		if r.Header.Get("SOAPAction") != `"urn:schemas-upnp-org:service:WANIPConnection:1#AddPortMapping"` {
			t.Errorf("SOAPAction %q", r.Header.Get("SOAPAction"))
		}
	})
	New(client, "http://192.168.1.1/ctl", "urn:schemas-upnp-org:service:WANIPConnection:1").Add(ctx, newMapping("d"))
	order := []string{"NewRemoteHost", "NewExternalPort", "NewProtocol>TCP", "NewInternalPort", "NewInternalClient", "NewEnabled", "NewPortMappingDescription", "NewLeaseDuration>0"}
	last := -1
	for _, name := range order {
		at := strings.Index(body, "<"+name)
		if at <= last {
			t.Fatalf("%s out of order in %s", name, body)
		}
		last = at
	}
}

func TestAHTTPFailureWithoutAUPnPErrorIsReportedWithItsBody(t *testing.T) {
	client := serving(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gateway rebooting", http.StatusServiceUnavailable)
	})
	err := New(client, "http://192.168.1.1/ctl", Services[0]).Remove(ctx, 80, model.TCP)
	if err == nil || err.Error() != "DeletePortMapping failed: HTTP 503 gateway rebooting" {
		t.Errorf("got %v", err)
	}
}

func TestRemoveDeletesAMapping(t *testing.T) {
	gateway, fake := connect(t, fakes.Quirks{})
	fake.Given(model.Mapping{ExternalPort: 9999, Protocol: model.TCP})
	if err := gateway.Remove(ctx, 9999, model.TCP); err != nil {
		t.Fatal(err)
	}
	if got := fake.Mappings(); len(got) != 0 {
		t.Errorf("got %+v", got)
	}
}

func TestRemoveLeavesTheOtherProtocolOnTheSamePortAlone(t *testing.T) {
	gateway, fake := connect(t, fakes.Quirks{})
	fake.Given(model.Mapping{ExternalPort: 9999, Protocol: model.TCP})
	fake.Given(model.Mapping{ExternalPort: 9999, Protocol: model.UDP})
	gateway.Remove(ctx, 9999, model.TCP)
	if got := fake.Mappings(); len(got) != 1 || got[0].Protocol != model.UDP {
		t.Errorf("got %+v", got)
	}
}

// Issue #6's error. v1 treated 714 as fatal and exited, taking the daemon down
// over a mapping that was already in the state we wanted.
func TestRemoveTreatsDeletingAMappingThatIsNotThereAsSuccess(t *testing.T) {
	gateway, _ := connect(t, fakes.Quirks{})
	if err := gateway.Remove(ctx, 9999, model.TCP); err != nil {
		t.Errorf("got %v", err)
	}
}

func TestExternalAddressReadsTheWANAddress(t *testing.T) {
	gateway, _ := connect(t, fakes.Quirks{})
	if got, err := gateway.ExternalAddress(ctx); err != nil || got != "81.2.69.142" {
		t.Errorf("got %q, %v", got, err)
	}
}

// Reported by routers that have not got a WAN address yet, which is worse
// than saying nothing at all.
func TestExternalAddressTreats0000AsNotKnowing(t *testing.T) {
	gateway, fake := connect(t, fakes.Quirks{})
	fake.SetExternalIP("0.0.0.0")
	if got, err := gateway.ExternalAddress(ctx); err != nil || got != "" {
		t.Errorf("got %q, %v", got, err)
	}
}

// Several firmwares refuse to overwrite a mapping that points elsewhere, which
// is why the daemon deletes first when a rule has to move.
func TestTheFakeRefusesToMoveAMappingWhenToldTo(t *testing.T) {
	gateway, fake := connect(t, fakes.Quirks{RefuseToMove: true})
	mapping := newMapping("portical: one")
	if err := gateway.Add(ctx, mapping); err != nil {
		t.Fatal(err)
	}
	if err := gateway.Add(ctx, mapping); err != nil {
		t.Errorf("rewriting in place was refused: %v", err)
	}
	mapping.InternalClient = "192.168.1.41"
	var upnp *Error
	if err := gateway.Add(ctx, mapping); !errors.As(err, &upnp) || upnp.Code != Conflict {
		t.Errorf("got %v", err)
	}
	if fake.Mappings()[0].InternalClient != "192.168.1.40" {
		t.Error("the refused mapping moved anyway")
	}
}

func TestTheFakeTruncatesDescriptionsAndDowngradesLeasesWhenToldTo(t *testing.T) {
	gateway, fake := connect(t, fakes.Quirks{DescriptionLimit: 22, LeaseLimit: 600})
	gateway.Add(ctx, newMapping("portical: (6881:6881/udp) qbittorrent"))
	got := fake.Mappings()[0]
	if got.Description != "portical: (6881:6881/u" || got.LeaseDuration != 600 {
		t.Errorf("got %+v", got)
	}
}

func TestTheFakeInSecureModeOnlyMapsToWhoeverAsked(t *testing.T) {
	fake := fakes.NewGateway(fakes.Quirks{SecureMode: true})
	asHost := New(fake.Client(), fakes.ControlURL, fakes.ServiceType)
	asContainer := New(&http.Client{Transport: httpx.HandlerTransport(fake.From("192.168.1.40"))}, fakes.ControlURL, fakes.ServiceType)

	var upnp *Error
	if err := asHost.Add(ctx, newMapping("d")); !errors.As(err, &upnp) || upnp.Code != Conflict {
		t.Errorf("from the host: %v", err)
	}
	if err := asContainer.Add(ctx, newMapping("d")); err != nil {
		t.Errorf("from the container: %v", err)
	}
}

func TestFirstAmongSkipsADeviceThatForwardsNoPortsAndUsesTheNext(t *testing.T) {
	real := fakes.NewGateway(fakes.Quirks{})
	client := serving(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Hostname() == "10.0.0.203" {
			notAGateway(w, r)
			return
		}
		real.ServeHTTP(w, r)
	})
	gateway, err := FirstAmong(ctx, client, []string{"http://10.0.0.203:80/description.xml", fakes.RootURL})
	if err != nil {
		t.Fatal(err)
	}
	if gateway.ControlURL != fakes.ControlURL {
		t.Errorf("got %s", gateway.ControlURL)
	}
}

func TestFirstAmongSkipsOneThatCannotBeReachedAtAll(t *testing.T) {
	real := fakes.NewGateway(fakes.Quirks{})
	client := serving(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Hostname() == "10.0.0.203" {
			http.Error(w, "nope", http.StatusInternalServerError)
			return
		}
		real.ServeHTTP(w, r)
	})
	gateway, err := FirstAmong(ctx, client, []string{"http://10.0.0.203/d.xml", fakes.RootURL})
	if err != nil || !strings.Contains(gateway.ControlURL, "/ctl/IPConn") {
		t.Errorf("got %v, %v", gateway, err)
	}
}

func TestFirstAmongReportsEveryDeviceItTriedWhenNoneWillDo(t *testing.T) {
	_, err := FirstAmong(ctx, serving(notAGateway), []string{"http://10.0.0.203/d.xml", "http://10.0.0.128/d.xml"})
	if err == nil {
		t.Fatal("found a gateway")
	}
	for _, want := range []string{"found 2 UPnP devices, but none of them forwards ports", "10.0.0.203", "10.0.0.128", "Set --root"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q does not say %q", err, want)
		}
	}
}
