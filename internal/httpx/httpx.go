// Package httpx is the seam everything that talks HTTP goes through.
//
// Every client in Portical - the Docker Engine API over its unix socket, the
// gateway's device description and SOAP - is an *http.Client over an
// http.RoundTripper, and every fake is an http.Handler. The adapters here join
// the two, so a test hands a client an in-memory Docker or router with no
// server, no port and no socket, and the client cannot tell the difference.
// That is what makes the daemon testable end to end.
//
// It is also why nothing above this line knows whether it is talking over a
// unix socket or TCP.
package httpx

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// RoundTripperFunc is to http.RoundTripper what http.HandlerFunc is to
// http.Handler.
type RoundTripperFunc func(*http.Request) (*http.Response, error)

func (f RoundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

// UnixSocket talks to a real server over a unix socket, whatever host the
// request's URL names.
//
// Reaching the Docker Engine API needs no client library and no `docker`
// binary - only this. It sets no timeout of its own: Docker's /events is a
// long poll that sits idle whenever nothing starts or stops, and must be
// allowed to, so bounding the other calls is the client's business.
func UnixSocket(path string) http.RoundTripper {
	var dialer net.Dialer
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", path)
		},
	}
}

// StatusError is a response that was not a success, with the server's own
// words rather than a bare status code.
type StatusError struct {
	StatusCode int
	// The status line, such as "404 Not Found".
	Status string
	// The body, trimmed, which is where servers put the reason.
	Body string
}

func (e *StatusError) Error() string {
	if e.Body == "" {
		return e.Status
	}
	return e.Status + ": " + e.Body
}

// Check passes a 2xx response through, and turns any other into a
// *StatusError carrying its body, which it reads and closes.
func Check(response *http.Response) error {
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	status := response.Status
	if status == "" {
		status = fmt.Sprintf("%d %s", response.StatusCode, http.StatusText(response.StatusCode))
	}
	return &StatusError{StatusCode: response.StatusCode, Status: status, Body: strings.TrimSpace(string(body))}
}

// served is the request as a server would have received it: a request URI, a
// Host and a non-nil body.
func served(request *http.Request) *http.Request {
	served := request.Clone(request.Context())
	served.RequestURI = request.URL.RequestURI()
	if served.Host == "" {
		served.Host = request.URL.Host
	}
	if served.Body == nil {
		served.Body = http.NoBody
	}
	return served
}

func panicked(request *http.Request, value any) error {
	return fmt.Errorf("%s %s: handler panicked: %v", request.Method, request.URL, value)
}
