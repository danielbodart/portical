package docker

// Making an HTTP request from inside another container's network namespace.
//
// Needed because macvlan and ipvlan containers have their own address on the
// LAN, and gateways running miniupnpd with `secure_mode` on - the OpenWrt
// default - only let a client create or delete a mapping pointing at the
// address it is asking *from*. Portical asks from the host, so a rule naming
// the container's address is refused with 718.
//
// v1 solved this by running `upnpc` in a throwaway container sharing the
// target's network namespace. The same trick works here, except that what is
// relayed is an ordinary HTTP request rather than a command line - so the
// result is just another http.RoundTripper, and the UPnP client is entirely
// unaware that anything unusual is happening.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// RelayedRequest is a request to be made from inside a container.
//
// The JSON field names are the TypeScript version's, so either can relay for
// the other.
type RelayedRequest struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

// RelayedResponse is what came back.
type RelayedResponse struct {
	Status int    `json:"status"`
	Body   string `json:"body"`
}

// RelayTimeout bounds the one request a helper container makes.
const RelayTimeout = 10 * time.Second

// relayFailed is the status a helper reports when it could not reach the
// gateway at all: outside HTTP's own range, so it cannot be mistaken for
// anything the gateway said.
const relayFailed = 599

const (
	relayStart = "<<<PORTICAL-RELAY"
	relayEnd   = "PORTICAL-RELAY>>>"
)

// EncodeRelay is a request as one argument.
//
// Base64, so the whole request survives being a single argv entry: SOAP bodies
// contain quotes, angle brackets and newlines, and this value passes through
// the Docker API into a container's command line.
func EncodeRelay(request RelayedRequest) string {
	data, _ := json.Marshal(request)
	return base64.StdEncoding.EncodeToString(data)
}

// DecodeRelay is EncodeRelay undone.
func DecodeRelay(encoded string) (RelayedRequest, error) {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return RelayedRequest{}, fmt.Errorf("relay request is not base64: %w", err)
	}
	var request RelayedRequest
	if err := json.Unmarshal(data, &request); err != nil {
		return RelayedRequest{}, fmt.Errorf("relay request is not JSON: %w", err)
	}
	return request, nil
}

// ToRelayedRequest is request as something that can be relayed. It reads the
// request's body.
func ToRelayedRequest(request *http.Request) (RelayedRequest, error) {
	relayed := RelayedRequest{URL: request.URL.String(), Method: request.Method, Headers: map[string]string{}}
	for name, values := range request.Header {
		// Lower case, as the Fetch API gives them, so the two versions write
		// the same thing.
		relayed.Headers[strings.ToLower(name)] = strings.Join(values, ", ")
	}
	if request.Body != nil {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return RelayedRequest{}, err
		}
		relayed.Body = string(body)
	}
	return relayed, nil
}

// RunRelay performs a relayed request and reports the result on stdout. It
// is the `relay` command, and always succeeds: failing to reach the gateway
// is reported in the result.
//
// This runs inside the throwaway container, where there is no Docker socket
// and nothing else to do. The result is framed between markers because the
// runtime may print its own diagnostics to the same stream, and because with a
// TTY attached Docker gives back one undifferentiated pipe.
func RunRelay(payload string, stdout io.Writer, transport http.RoundTripper) int {
	result, err := relay(payload, transport)
	if err != nil {
		// Reported as a response rather than as a failure, so the caller sees
		// why the gateway could not be reached from inside the container -
		// which is a different and more interesting failure than the container
		// not starting.
		result = RelayedResponse{Status: relayFailed, Body: "relay failed: " + err.Error()}
	}
	data, _ := json.Marshal(result)
	fmt.Fprintf(stdout, "%s%s%s", relayStart, data, relayEnd)
	return 0
}

func relay(payload string, transport http.RoundTripper) (RelayedResponse, error) {
	relayed, err := DecodeRelay(payload)
	if err != nil {
		return RelayedResponse{}, err
	}
	var body io.Reader
	if relayed.Body != "" {
		body = strings.NewReader(relayed.Body)
	}
	request, err := http.NewRequest(relayed.Method, relayed.URL, body)
	if err != nil {
		return RelayedResponse{}, err
	}
	for name, value := range relayed.Headers {
		request.Header.Set(name, value)
	}
	response, err := (&http.Client{Transport: transport, Timeout: RelayTimeout}).Do(request)
	if err != nil {
		return RelayedResponse{}, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return RelayedResponse{}, err
	}
	return RelayedResponse{Status: response.StatusCode, Body: string(data)}, nil
}

// ParseRelayOutput recovers the result from whatever else the container
// printed.
func ParseRelayOutput(output string) (RelayedResponse, error) {
	start := strings.Index(output, relayStart)
	end := -1
	if start >= 0 {
		if offset := strings.Index(output[start:], relayEnd); offset >= 0 {
			end = start + offset
		}
	}
	if end < 0 {
		return RelayedResponse{}, fmt.Errorf("relay produced no usable result: %s", clip(output))
	}
	var response RelayedResponse
	if err := json.Unmarshal([]byte(output[start+len(relayStart):end]), &response); err != nil {
		return RelayedResponse{}, fmt.Errorf("relay produced no usable result: %w", err)
	}
	return response, nil
}
