// Package fakes is the far end of everything Portical talks to, in memory,
// for tests: a router, a source of forwards, and a log to read back.
//
// Only tests import it. The router is an http.Handler, reached through
// httpx.HandlerTransport, so a test is the real UPnP client and the real
// daemon with only the far end replaced.
package fakes

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/danielbodart/portical/internal/httpx"
	"github.com/danielbodart/portical/internal/model"
	"github.com/danielbodart/portical/internal/xml"
)

// HostAddress is the address requests reach the fake gateway from unless
// told otherwise: the Docker host, or the systemd host, on the LAN.
const HostAddress = "192.168.1.5"

// RootURL is where the fake gateway's device description is.
const RootURL = "http://192.168.1.1:5000/rootDesc.xml"

// ControlURL is where its WANIPConnection service answers.
const ControlURL = "http://192.168.1.1:5000/ctl/IPConn"

// ServiceType is the service the fake gateway offers.
const ServiceType = "urn:schemas-upnp-org:service:WANIPConnection:1"

// Quirks is the ways real routers misbehave, as switches.
//
// Every one of these is taken from a report on the tracker or from the IGD
// specification's list of permitted refusals. Being able to turn them on is
// the point of the fake: issue #6 is a router truncating descriptions, and
// before this there was no way to reproduce that without owning the router.
type Quirks struct {
	// Truncate descriptions to this many characters, as several firmwares do.
	DescriptionLimit int
	// Refuse an unlimited lease and substitute this many seconds.
	LeaseLimit int
	// Report the end of the mapping table with this code instead of 713.
	EndOfTableCode int
	// Never report the end of the mapping table: answer every index past the
	// last with the last row again.
	EndlessTable bool
	// Only allow a mapping that points at the address the request came from.
	//
	// This is miniupnpd's secure_mode, which is on by default on OpenWrt. It
	// is why a macvlan container's rule cannot be created from the host.
	SecureMode bool
	// Refuse AddPortMapping outright with this code, as an unauthorised
	// client would see.
	RefuseWith int
	// Refuse with 718 to overwrite a mapping with one pointing somewhere
	// else - another address or another internal port - rather than
	// replacing it, as several firmwares do. Rewriting a mapping with the
	// target it already has is still allowed.
	RefuseToMove bool
}

var faults = map[int]string{
	401: "Invalid Action",
	402: "Invalid Args",
	501: "Action Failed",
	606: "Action not authorized",
	713: "SpecifiedArrayIndexInvalid",
	714: "NoSuchEntryInArray",
	718: "ConflictInMappingEntry",
	725: "OnlyPermanentLeasesSupported",
}

// Gateway is an internet gateway that exists only as an http.Handler.
//
// Because Portical reaches the router through an http.RoundTripper, this
// stands in for one with no server, no port and no router - the whole daemon
// can be driven end to end against it in memory.
type Gateway struct {
	quirks     Quirks
	mu         sync.Mutex
	mappings   []model.Mapping
	actions    []string
	externalIP string
}

// NewGateway is a router with the given misbehaviours and an empty table.
func NewGateway(quirks Quirks) *Gateway {
	return &Gateway{quirks: quirks, externalIP: "81.2.69.142"}
}

// ServeHTTP is the gateway as seen from HostAddress.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.serve(w, r, HostAddress)
}

// From is the gateway as seen from some other address on the LAN.
//
// Used to stand in for a request relayed through a container's network
// namespace, which is the whole point of the exercise: the request is
// identical, but it arrives from the container's own address.
func (g *Gateway) From(requester string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { g.serve(w, r, requester) })
}

// Client is an HTTP client that reaches this gateway from HostAddress.
func (g *Gateway) Client() *http.Client {
	return &http.Client{Transport: httpx.HandlerTransport(g)}
}

// Given puts a mapping in place without going through the gateway's own API.
// An internal port or client left unset defaults to the external port and
// HostAddress.
func (g *Gateway) Given(mapping model.Mapping) *Gateway {
	if mapping.InternalClient == "" {
		mapping.InternalClient = HostAddress
	}
	if mapping.InternalPort == 0 {
		mapping.InternalPort = mapping.ExternalPort
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mappings = append(g.mappings, mapping)
	return g
}

// Mappings is a copy of the table as it stands.
func (g *Gateway) Mappings() []model.Mapping {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]model.Mapping{}, g.mappings...)
}

// Actions is every SOAP action received since the last ClearActions, in
// order.
func (g *Gateway) Actions() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string{}, g.actions...)
}

// Received is how many times an action has been received since the last
// ClearActions.
func (g *Gateway) Received(action string) int {
	count := 0
	for _, received := range g.Actions() {
		if received == action {
			count++
		}
	}
	return count
}

// ClearActions forgets the actions received so far.
func (g *Gateway) ClearActions() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.actions = nil
}

// SetExternalIP changes the WAN address the gateway reports.
func (g *Gateway) SetExternalIP(address string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.externalIP = address
}

func (g *Gateway) serve(w http.ResponseWriter, r *http.Request, requester string) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/rootDesc.xml":
		reply(w, http.StatusOK, description)
	case r.Method == http.MethodPost && r.URL.Path == "/ctl/IPConn":
		body, _ := io.ReadAll(r.Body)
		status, response := g.soap(string(body), requester)
		reply(w, status, response)
	default:
		http.Error(w, "Not Found", http.StatusNotFound)
	}
}

var actionName = regexp.MustCompile(`<(?:\w+:)?(\w+) xmlns:u=`)

func (g *Gateway) soap(body, requester string) (int, string) {
	action := ""
	if match := actionName.FindStringSubmatch(body); match != nil {
		action = match[1]
	}
	field := func(name string) string { value, _ := xml.Text(body, name); return value }

	g.mu.Lock()
	defer g.mu.Unlock()
	g.actions = append(g.actions, action)

	switch action {
	case "GetGenericPortMappingEntry":
		index, err := strconv.Atoi(field("NewPortMappingIndex"))
		if err != nil {
			return fault(402)
		}
		return g.entry(index)
	case "AddPortMapping":
		return g.add(field, requester)
	case "DeletePortMapping":
		return g.remove(field)
	case "GetExternalIPAddress":
		return http.StatusOK, envelope("GetExternalIPAddressResponse", "NewExternalIPAddress", g.externalIP)
	default:
		return fault(401)
	}
}

func (g *Gateway) entry(index int) (int, string) {
	if index >= len(g.mappings) && g.quirks.EndlessTable && len(g.mappings) > 0 {
		index = len(g.mappings) - 1
	}
	if index < 0 || index >= len(g.mappings) {
		code := g.quirks.EndOfTableCode
		if code == 0 {
			code = 713
		}
		return fault(code)
	}
	m := g.mappings[index]
	return http.StatusOK, envelope("GetGenericPortMappingEntryResponse",
		"NewRemoteHost", m.RemoteHost,
		"NewExternalPort", strconv.Itoa(m.ExternalPort),
		"NewProtocol", strings.ToUpper(string(m.Protocol)),
		"NewInternalPort", strconv.Itoa(m.InternalPort),
		"NewInternalClient", m.InternalClient,
		"NewEnabled", "1",
		"NewPortMappingDescription", m.Description,
		"NewLeaseDuration", strconv.Itoa(m.LeaseDuration),
	)
}

func (g *Gateway) add(field func(string) string, requester string) (int, string) {
	if g.quirks.RefuseWith != 0 {
		return fault(g.quirks.RefuseWith)
	}

	internalClient := field("NewInternalClient")
	if g.quirks.SecureMode && internalClient != requester {
		return fault(718)
	}

	externalPort, err := strconv.Atoi(field("NewExternalPort"))
	if err != nil {
		return fault(402)
	}
	internalPort, err := strconv.Atoi(field("NewInternalPort"))
	if err != nil {
		return fault(402)
	}
	requested, err := strconv.Atoi(field("NewLeaseDuration"))
	if err != nil {
		return fault(402)
	}
	description := field("NewPortMappingDescription")
	if limit := g.quirks.DescriptionLimit; limit > 0 && len(description) > limit {
		description = description[:limit]
	}
	lease := requested
	if requested == 0 && g.quirks.LeaseLimit > 0 {
		lease = g.quirks.LeaseLimit
	}

	mapping := model.Mapping{
		ExternalPort:   externalPort,
		Protocol:       model.Protocol(strings.ToLower(field("NewProtocol"))),
		InternalPort:   internalPort,
		InternalClient: internalClient,
		Description:    description,
		RemoteHost:     field("NewRemoteHost"),
		LeaseDuration:  lease,
	}

	// The IGD table is keyed on (remote host, external port, protocol), so
	// adding over an existing entry replaces it rather than duplicating.
	existing := slices.IndexFunc(g.mappings, func(m model.Mapping) bool { return m.Identity() == mapping.Identity() })
	if existing == -1 {
		g.mappings = append(g.mappings, mapping)
		return http.StatusOK, envelope("AddPortMappingResponse")
	}
	if g.quirks.RefuseToMove {
		old := g.mappings[existing]
		if old.InternalClient != mapping.InternalClient || old.InternalPort != mapping.InternalPort {
			return fault(718)
		}
	}
	g.mappings[existing] = mapping
	return http.StatusOK, envelope("AddPortMappingResponse")
}

func (g *Gateway) remove(field func(string) string) (int, string) {
	externalPort, err := strconv.Atoi(field("NewExternalPort"))
	if err != nil {
		return fault(402)
	}
	identity := model.Identity(model.Protocol(strings.ToLower(field("NewProtocol"))), externalPort)
	index := slices.IndexFunc(g.mappings, func(m model.Mapping) bool { return m.Identity() == identity })
	if index == -1 {
		return fault(714)
	}
	g.mappings = slices.Delete(g.mappings, index, index+1)
	return http.StatusOK, envelope("DeletePortMappingResponse")
}

// envelope is a SOAP response carrying the given name, value pairs.
func envelope(action string, pairs ...string) string {
	var body strings.Builder
	body.WriteString(`<?xml version="1.0"?>` + "\n")
	body.WriteString(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">` + "\n")
	fmt.Fprintf(&body, `<s:Body><u:%s xmlns:u="%s">`, action, ServiceType)
	for i := 0; i+1 < len(pairs); i += 2 {
		fmt.Fprintf(&body, "<%s>%s</%s>", pairs[i], xml.Encode(pairs[i+1]), pairs[i])
	}
	fmt.Fprintf(&body, "</u:%s></s:Body></s:Envelope>", action)
	return body.String()
}

func fault(code int) (int, string) {
	description, ok := faults[code]
	if !ok {
		description = "Action Failed"
	}
	return http.StatusInternalServerError, fmt.Sprintf(`<?xml version="1.0"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
<s:Body><s:Fault><faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring>
<detail><UPnPError xmlns="urn:schemas-upnp-org:control-1-0">
<errorCode>%d</errorCode><errorDescription>%s</errorDescription>
</UPnPError></detail></s:Fault></s:Body></s:Envelope>`, code, description)
}

func reply(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.WriteHeader(status)
	io.WriteString(w, body)
}

const description = `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <specVersion><major>1</major><minor>0</minor></specVersion>
  <device>
    <deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:1</deviceType>
    <friendlyName>Fake Router</friendlyName>
    <deviceList><device>
      <deviceType>urn:schemas-upnp-org:device:WANDevice:1</deviceType>
      <deviceList><device>
        <deviceType>urn:schemas-upnp-org:device:WANConnectionDevice:1</deviceType>
        <serviceList>
          <service>
            <serviceType>urn:schemas-upnp-org:service:WANIPConnection:1</serviceType>
            <serviceId>urn:upnp-org:serviceId:WANIPConn1</serviceId>
            <controlURL>/ctl/IPConn</controlURL>
            <eventSubURL>/evt/IPConn</eventSubURL>
            <SCPDURL>/WANIPCn.xml</SCPDURL>
          </service>
        </serviceList>
      </device></deviceList>
    </device></deviceList>
  </device>
</root>`
