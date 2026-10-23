// Package upnp talks to an internet gateway: its device description, and the
// SOAP actions that read and write its port mapping table.
//
// v1 got these by running `upnpc`, which meant the image carried miniupnpc,
// failures arrived as exit codes with the reason on stdout, and testing any of
// it needed a real router. Here it is an HTTP conversation like any other,
// through an *http.Client whose transport a test replaces with a fake router.
package upnp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielbodart/portical/internal/httpx"
	"github.com/danielbodart/portical/internal/model"
	"github.com/danielbodart/portical/internal/xml"
)

// NewMapping is a mapping we are asking the gateway to create.
type NewMapping struct {
	ExternalPort   int
	Protocol       model.Protocol
	InternalPort   int
	InternalClient string
	Description    string
	// Seconds; 0 asks for a mapping that never expires.
	LeaseDuration int
}

// Gateway is the gateway operations Portical needs.
//
// An interface because the same gateway is also reached from inside a macvlan
// container's network, which is a different Client over a different
// transport, and the daemon must not care which it was handed.
type Gateway interface {
	Mappings(ctx context.Context) ([]model.Mapping, error)
	Add(ctx context.Context, mapping NewMapping) error
	// Remove succeeds when the mapping was already gone.
	Remove(ctx context.Context, externalPort int, protocol model.Protocol) error
	// ExternalAddress is the gateway's WAN address, or "" when it has none
	// yet.
	ExternalAddress(ctx context.Context) (string, error)
}

// Error is a UPnP error, carrying what the gateway actually said.
//
// The shell version originally reported only an exit status, which left
// "something went wrong" as the entire diagnosis - the reason issue #6 was so
// hard for people to read. The gateway's own code and description are far more
// useful, so they are kept whole.
type Error struct {
	Code        int
	Description string
	Action      string
}

func (e *Error) Error() string {
	message := fmt.Sprintf("%s failed: %d %s", e.Action, e.Code, e.Description)
	if explanation, ok := explanations[e.Code]; ok {
		message += " - " + explanation
	}
	return message
}

// Codes worth naming, because the code itself decides what happens next.
const (
	// InvalidArgs, ActionFailed and NoSuchIndex all end the mapping table -
	// see Mappings.
	InvalidArgs  = 402
	ActionFailed = 501
	NoSuchIndex  = 713
	// NoSuchMapping is deleting something that was already gone.
	NoSuchMapping = 714
	// Conflict is another mapping claiming the external port - or, on a
	// gateway in secure mode, a mapping that points somewhere other than
	// whoever asked.
	Conflict = 718
)

// Plain-language notes for the codes people actually hit.
var explanations = map[int]string{
	402: "the gateway rejected the arguments",
	501: "the gateway refused the action",
	606: "UPnP is enabled but not authorised for this client",
	713: "no mapping at that index",
	714: "no such mapping",
	715: "the gateway requires a specific source address",
	716: "the gateway requires a specific external port",
	718: "another mapping already claims that external port",
	724: "the gateway requires the internal and external ports to match",
	725: "the gateway only supports permanent leases",
	726: "the gateway only supports a wildcard remote host",
	727: "the gateway only supports a wildcard external port",
	729: "conflicts with a mapping made by another mechanism",
}

// RequestTimeout bounds each request to the gateway, so a silent gateway
// cannot hang a pass. It is the http.Client's Timeout, set by whoever builds
// the client.
const RequestTimeout = 10 * time.Second

// MaxMappings stops a runaway listing.
//
// The table is read by asking for index 0, 1, 2... until the gateway says
// there is nothing there. A router that answers every index instead of
// reporting the end would otherwise spin forever.
const MaxMappings = 4096

// Services is the service types that can forward a port, best first.
var Services = []string{
	"urn:schemas-upnp-org:service:WANIPConnection:2",
	"urn:schemas-upnp-org:service:WANIPConnection:1",
	"urn:schemas-upnp-org:service:WANPPPConnection:1",
}

// Client is a gateway reached over HTTP.
type Client struct {
	http        *http.Client
	ControlURL  string
	ServiceType string
}

// New is a client for a gateway whose control URL and service are already
// known - the same gateway at found by At, reached some other way.
func New(client *http.Client, controlURL, serviceType string) *Client {
	return &Client{http: client, ControlURL: controlURL, ServiceType: serviceType}
}

// At reads a gateway's device description and finds the service that
// forwards ports.
//
// This is what `-r`/`PORTICAL_UPNP_ROOT_URL` points at, and what SSDP
// discovery returns.
func At(ctx context.Context, client *http.Client, rootDescriptionURL string) (*Client, error) {
	description, err := fetch(ctx, client, rootDescriptionURL)
	if err != nil {
		return nil, err
	}

	// URLBase is optional, and routers that omit it expect control URLs to be
	// resolved against wherever the description was fetched from.
	base := rootDescriptionURL
	if found, ok := xml.Text(description, "URLBase"); ok && strings.TrimSpace(found) != "" {
		base = strings.TrimSpace(found)
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("%s: unreadable URLBase %q: %w", rootDescriptionURL, base, err)
	}

	services := xml.Elements(description, "service")
	for _, wanted := range Services {
		for _, service := range services {
			if found, _ := xml.Text(service, "serviceType"); strings.TrimSpace(found) != wanted {
				continue
			}
			control, _ := xml.Text(service, "controlURL")
			control = strings.TrimSpace(control)
			if control == "" {
				continue
			}
			controlURL, err := url.Parse(control)
			if err != nil {
				continue
			}
			return New(client, baseURL.ResolveReference(controlURL).String(), wanted), nil
		}
	}

	return nil, fmt.Errorf("%s is a UPnP device but offers no port forwarding service (looked for %s)",
		rootDescriptionURL, strings.Join(Services, ", "))
}

func fetch(ctx context.Context, client *http.Client, location string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	if err := httpx.Check(response); err != nil {
		return "", fmt.Errorf("%s: %w", location, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	return string(data), err
}

// FirstAmong is the first of the discovered devices that can actually forward
// a port.
//
// Every answer is tried, not just the first. SSDP is answered by anything that
// speaks it, and a network can easily contain a device that replies to a
// search for an InternetGatewayDevice while offering no port forwarding
// service at all - a real one was found doing exactly that on the network this
// was developed against. Stopping at the first reply made discovery fail with
// a working router sitting on the same LAN.
func FirstAmong(ctx context.Context, client *http.Client, candidates []string) (*Client, error) {
	var failures []string
	for _, candidate := range candidates {
		gateway, err := At(ctx, client, candidate)
		if err == nil {
			return gateway, nil
		}
		failures = append(failures, fmt.Sprintf("  %s: %v", candidate, err))
	}
	plural := "s"
	if len(candidates) == 1 {
		plural = ""
	}
	return nil, fmt.Errorf("found %d UPnP device%s, but none of them forwards ports:\n%s\n"+
		"Set --root or PORTICAL_UPNP_ROOT_URL to name your gateway directly.",
		len(candidates), plural, strings.Join(failures, "\n"))
}

// endsTheTable is the codes that mean the listing has run past the last row.
// The specification says 713, but plenty of routers answer 402 or 501 once
// the index runs past the end, and treating those as failures would make
// Portical unable to read its own rules on those routers.
var endsTheTable = []int{NoSuchIndex, InvalidArgs, ActionFailed}

func (c *Client) Mappings(ctx context.Context) ([]model.Mapping, error) {
	found := []model.Mapping{}
	for index := 0; index < MaxMappings; index++ {
		entry, err := c.call(ctx, "GetGenericPortMappingEntry", argument{"NewPortMappingIndex", strconv.Itoa(index)})
		if err != nil {
			var upnp *Error
			if errors.As(err, &upnp) && slices.Contains(endsTheTable, upnp.Code) {
				break
			}
			return nil, err
		}
		if mapping, ok := toMapping(entry); ok {
			found = append(found, mapping)
		}
	}
	return found, nil
}

func (c *Client) Add(ctx context.Context, mapping NewMapping) error {
	// Argument order follows the IGD specification exactly. It should not
	// matter, but a number of firmwares parse positionally and reject anything
	// else with a bare 402.
	_, err := c.call(ctx, "AddPortMapping",
		argument{"NewRemoteHost", ""},
		argument{"NewExternalPort", strconv.Itoa(mapping.ExternalPort)},
		argument{"NewProtocol", strings.ToUpper(string(mapping.Protocol))},
		argument{"NewInternalPort", strconv.Itoa(mapping.InternalPort)},
		argument{"NewInternalClient", mapping.InternalClient},
		argument{"NewEnabled", "1"},
		argument{"NewPortMappingDescription", mapping.Description},
		argument{"NewLeaseDuration", strconv.Itoa(mapping.LeaseDuration)},
	)
	return err
}

func (c *Client) Remove(ctx context.Context, externalPort int, protocol model.Protocol) error {
	_, err := c.call(ctx, "DeletePortMapping",
		argument{"NewRemoteHost", ""},
		argument{"NewExternalPort", strconv.Itoa(externalPort)},
		argument{"NewProtocol", strings.ToUpper(string(protocol))},
	)
	// Already gone is the outcome we wanted. This is the error at the heart of
	// issue #6: v1 treated it as fatal and took the whole daemon down with it,
	// when it means nothing more than "there was nothing to do".
	var upnp *Error
	if errors.As(err, &upnp) && upnp.Code == NoSuchMapping {
		return nil
	}
	return err
}

func (c *Client) ExternalAddress(ctx context.Context) (string, error) {
	response, err := c.call(ctx, "GetExternalIPAddress")
	if err != nil {
		return "", err
	}
	address, _ := xml.Text(response, "NewExternalIPAddress")
	address = strings.TrimSpace(address)
	// Routers that have not got a WAN address yet report 0.0.0.0 rather than
	// failing, which is worse than saying nothing.
	if address == "0.0.0.0" {
		return "", nil
	}
	return address, nil
}

type argument struct{ name, value string }

func (c *Client) call(ctx context.Context, action string, arguments ...argument) (string, error) {
	var body strings.Builder
	body.WriteString(`<?xml version="1.0"?>` + "\n")
	body.WriteString(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" `)
	body.WriteString(`s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">`)
	fmt.Fprintf(&body, `<s:Body><u:%s xmlns:u="%s">`, action, c.ServiceType)
	for _, argument := range arguments {
		fmt.Fprintf(&body, "<%s>%s</%s>", argument.name, xml.Encode(argument.value), argument.name)
	}
	fmt.Fprintf(&body, "</u:%s></s:Body></s:Envelope>", action)

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ControlURL, strings.NewReader(body.String()))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	request.Header.Set("SOAPAction", fmt.Sprintf(`"%s#%s"`, c.ServiceType, action))

	response, err := c.http.Do(request)
	if err != nil {
		return "", fmt.Errorf("%s failed: %w", action, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return "", fmt.Errorf("%s failed: %w", action, err)
	}
	payload := string(data)

	// A SOAP fault arrives as HTTP 500 with the real reason in the body, so
	// the body is read before the status is judged.
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		codeText, _ := xml.Text(payload, "errorCode")
		if code, err := strconv.Atoi(strings.TrimSpace(codeText)); err == nil && code > 0 {
			description, _ := xml.Text(payload, "errorDescription")
			return "", &Error{Code: code, Description: strings.TrimSpace(description), Action: action}
		}
		return "", fmt.Errorf("%s failed: HTTP %d %s", action, response.StatusCode, strings.TrimSpace(payload))
	}
	return payload, nil
}

// toMapping reads one row of the gateway's mapping table.
//
// Rows that name a protocol we do not manage, or whose ports are not
// numbers, are dropped rather than half-understood - a row we
// cannot read correctly is one we might otherwise delete.
func toMapping(entry string) (model.Mapping, bool) {
	field := func(name string) string {
		value, _ := xml.Text(entry, name)
		return strings.TrimSpace(value)
	}

	protocol := model.Protocol(strings.ToLower(field("NewProtocol")))
	if protocol != model.TCP && protocol != model.UDP {
		return model.Mapping{}, false
	}
	externalPort, err := strconv.Atoi(field("NewExternalPort"))
	if err != nil {
		return model.Mapping{}, false
	}
	internalPort, err := strconv.Atoi(field("NewInternalPort"))
	if err != nil {
		return model.Mapping{}, false
	}
	// Unreadable reads as unlimited, which is what an absent lease means.
	lease, _ := strconv.Atoi(field("NewLeaseDuration"))
	description, _ := xml.Text(entry, "NewPortMappingDescription")

	return model.Mapping{
		ExternalPort:   externalPort,
		Protocol:       protocol,
		InternalPort:   internalPort,
		InternalClient: field("NewInternalClient"),
		Description:    description,
		RemoteHost:     field("NewRemoteHost"),
		LeaseDuration:  lease,
	}, true
}
