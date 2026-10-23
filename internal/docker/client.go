// Package docker is containers as a source of forwards: the Engine API client,
// the rules that turn a container's labels and networks into forwards, and the
// relay that reaches the gateway from inside a macvlan container's network.
package docker

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/danielbodart/portical/internal/httpx"
	"github.com/danielbodart/portical/internal/model"
)

// API is the Engine API version spoken. 1.41 ships with Docker 20.10, old
// enough to be safe.
const API = "v1.41"

// RequestTimeout bounds every Engine API call that is meant to come straight
// back.
//
// Set per request rather than as the http.Client's Timeout, which would also
// cut off /events: that is a long poll that sits idle for as long as nothing
// starts or stops, and must be allowed to.
const RequestTimeout = 30 * time.Second

// Network is a network a container is attached to, joined with that
// network's driver.
type Network struct {
	Name      string
	Driver    string
	IPAddress string
}

// PublishedPort is a port the container publishes on the Docker host.
type PublishedPort struct {
	HostPort      int
	ContainerPort int
	Protocol      model.Protocol
}

// Container is a running container, as much of it as deciding what to
// forward needs.
type Container struct {
	ID        string
	Name      string
	Labels    map[string]string
	Networks  []Network
	Published []PublishedPort
}

// Event is something that happened to a container.
type Event struct {
	Action    string
	Container string
}

// Client talks to the Docker Engine API.
//
// v1 shelled out to the `docker` CLI, which meant the image had to carry
// Docker itself, and a container list cost one process per container plus one
// more per network. This is two HTTP requests per pass and no CLI at all.
//
// It takes an http.RoundTripper rather than a socket path, so the tests hand
// it an in-memory Docker and it cannot tell the difference.
type Client struct {
	http *http.Client
}

// NewClient is a client for the Engine API reached through transport -
// httpx.UnixSocket for a real Docker.
func NewClient(transport http.RoundTripper) *Client {
	return &Client{http: &http.Client{Transport: transport}}
}

// Containers is every running container carrying label.
func (c *Client) Containers(ctx context.Context, label string) ([]Container, error) {
	// Networks are fetched whole and joined in memory rather than inspected
	// per container: a host has a handful of networks and this keeps a pass at
	// a constant two requests however many containers are labelled.
	var containers []rawContainer
	if err := c.getJSON(ctx, "/containers/json", url.Values{"filters": {filters(map[string][]string{"label": {label}})}}, &containers); err != nil {
		return nil, err
	}
	var networks []rawNetwork
	if err := c.getJSON(ctx, "/networks", nil, &networks); err != nil {
		return nil, err
	}

	drivers := map[string]string{}
	for _, network := range networks {
		drivers[network.Name] = network.Driver
	}
	found := make([]Container, 0, len(containers))
	for _, container := range containers {
		found = append(found, toContainer(container, drivers))
	}
	return found, nil
}

// Events calls notify for every container carrying label that starts,
// stops, dies, is killed, paused or unpaused, until ctx is done.
//
// Returns nil once ctx is done, and returns early - with the reason, or nil
// if Docker simply closed it - when the stream drops; reopening it is the
// caller's business.
func (c *Client) Events(ctx context.Context, label string, notify func(Event)) error {
	// `destroy` is not listened for: a container that is destroyed has already
	// died, and reacting twice only means reconciling twice.
	query := url.Values{"filters": {filters(map[string][]string{
		"type":  {"container"},
		"event": {"start", "die", "stop", "kill", "pause", "unpause"},
		"label": {label},
	})}}
	response, err := c.do(ctx, http.MethodGet, "/events", query, nil)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	defer response.Body.Close()

	// Docker writes one JSON object per line, as each event happens. The
	// decoder hands each one over as soon as it is complete, without waiting
	// for the next.
	decoder := json.NewDecoder(response.Body)
	for {
		var raw rawEvent
		if err := decoder.Decode(&raw); err != nil {
			if ctx.Err() != nil || errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("Docker API /%s/events failed: %w", API, err)
		}
		notify(Event{Action: raw.Action, Container: cmp.Or(raw.Actor.Attributes["name"], raw.ID, raw.Actor.ID)})
	}
}

// RunInNetworkOf runs image with command in container's network namespace,
// and returns what it printed.
//
// The one Docker operation that writes rather than reads, and the only reason
// Portical needs more than read access to the socket. Used to reach the
// gateway *as* a macvlan container - see ViaContainer.
func (c *Client) RunInNetworkOf(ctx context.Context, container, image string, command []string) (string, error) {
	// Tty gives back a single raw stream rather than Docker's multiplexed
	// stdout/stderr framing, which is one less thing to decode for output we
	// are going to search for a marker anyway.
	var created struct {
		ID string `json:"Id"`
	}
	err := c.postJSON(ctx, "/containers/create", map[string]any{
		"Image": image,
		"Cmd":   command,
		"Tty":   true,
		"HostConfig": map[string]any{
			"NetworkMode": "container:" + container,
			"AutoRemove":  false,
		},
	}, &created)
	if err != nil {
		return "", err
	}
	// Removed here rather than with AutoRemove, which would take the container
	// away before its output could be read. Even when ctx has been cancelled:
	// a helper left behind would sit on the host for good.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), RequestTimeout)
		defer cancel()
		if response, err := c.do(cleanup, http.MethodDelete, "/containers/"+created.ID, url.Values{"force": {"1"}, "v": {"1"}}, nil); err == nil {
			drain(response)
		}
	}()

	if err := c.postJSON(ctx, "/containers/"+created.ID+"/start", nil, nil); err != nil {
		return "", err
	}

	// follow=1 holds the response open until the container exits and its
	// output has been flushed, so the whole of it is here. Reading the logs
	// after /wait instead loses a race: /wait returns the moment the process
	// exits, which can be before its last write has been collected. It is
	// bounded by ctx alone, since how long that takes is the helper's
	// business.
	response, err := c.do(ctx, http.MethodGet, "/containers/"+created.ID+"/logs", url.Values{"follow": {"1"}, "stdout": {"1"}, "stderr": {"1"}}, nil)
	if err != nil {
		return "", err
	}
	logs, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		return "", fmt.Errorf("Docker API /%s/containers/%s/logs failed: %w", API, created.ID, err)
	}
	output := string(logs)

	var waited struct{ StatusCode int }
	if err := c.postJSON(ctx, "/containers/"+created.ID+"/wait", nil, &waited); err != nil {
		return "", err
	}
	if waited.StatusCode != 0 {
		return "", fmt.Errorf("%s exited with status %d: %s", image, waited.StatusCode, clip(output))
	}
	return output, nil
}

// ImageOf is the image a container was created from, or "" when Docker does
// not say.
func (c *Client) ImageOf(ctx context.Context, container string) (string, error) {
	var inspected struct{ Config struct{ Image string } }
	if err := c.getJSON(ctx, "/containers/"+container+"/json", nil, &inspected); err != nil {
		return "", err
	}
	return inspected.Config.Image, nil
}

// do makes one request and checks its status. ctx alone bounds it, so the
// caller decides whether it is the kind that comes straight back.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any) (*http.Response, error) {
	target := "/" + API + path
	fail := func(err error) error { return fmt.Errorf("Docker API %s failed: %w", target, err) }

	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fail(err)
		}
		reader = bytes.NewReader(data)
	}
	// The host is a placeholder: the transport decides where this goes.
	address := (&url.URL{Scheme: "http", Host: "docker", Path: target, RawQuery: query.Encode()}).String()
	request, err := http.NewRequestWithContext(ctx, method, address, reader)
	if err != nil {
		return nil, fail(err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, fail(err)
	}
	if err := httpx.Check(response); err != nil {
		return nil, fail(err)
	}
	return response, nil
}

// getJSON is a request that comes straight back, read into into.
func (c *Client) getJSON(ctx context.Context, path string, query url.Values, into any) error {
	return c.quick(ctx, http.MethodGet, path, query, nil, into)
}

// postJSON is the same for a POST. A nil into reads and discards the reply.
func (c *Client) postJSON(ctx context.Context, path string, body, into any) error {
	return c.quick(ctx, http.MethodPost, path, nil, body, into)
}

// quick is a call bounded by RequestTimeout whose body is always read to
// the end and closed, so a reply nobody wanted cannot leave the connection to
// the socket out of step for the next request - which is what made the
// TypeScript relay appear to produce no output.
func (c *Client) quick(ctx context.Context, method, path string, query url.Values, body, into any) error {
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	response, err := c.do(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	defer drain(response)
	if into == nil {
		return nil
	}
	if err := json.NewDecoder(response.Body).Decode(into); err != nil {
		return fmt.Errorf("Docker API /%s%s failed: %w", API, path, err)
	}
	return nil
}

func drain(response *http.Response) {
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
}

// filters is how Docker wants its filters: a JSON map of name to values.
func filters(spec map[string][]string) string {
	data, _ := json.Marshal(spec)
	return string(data)
}

// clip is the start of what a helper printed, enough to say why it failed
// without filling the log.
func clip(output string) string {
	output = strings.TrimSpace(output)
	if len(output) > 500 {
		return output[:500]
	}
	return output
}

type rawContainer struct {
	ID     string `json:"Id"`
	Names  []string
	Labels map[string]string
	Ports  []struct {
		PrivatePort int
		PublicPort  int
		Type        string
	}
	NetworkSettings struct {
		Networks map[string]struct{ IPAddress string }
	}
}

type rawNetwork struct {
	Name   string
	Driver string
}

type rawEvent struct {
	Action string
	ID     string `json:"id"`
	Actor  struct {
		ID         string
		Attributes map[string]string
	}
}

// toContainer turns one Engine API container into our own shape.
//
// The mapping is where the interesting mistakes live, not in the HTTP, so the
// tests drive it from captured API responses.
func toContainer(raw rawContainer, drivers map[string]string) Container {
	// Docker reports names with a leading slash, and a container may have
	// several. The first is the one people recognise, and it is what ends up
	// in the rule description on the router.
	name := raw.ID
	if len(raw.Names) > 0 {
		name = raw.Names[0]
	}
	container := Container{
		ID:        raw.ID,
		Name:      strings.TrimPrefix(name, "/"),
		Labels:    raw.Labels,
		Networks:  []Network{},
		Published: publishedPorts(raw),
	}
	if container.Labels == nil {
		container.Labels = map[string]string{}
	}
	for name, network := range raw.NetworkSettings.Networks {
		// A network attached by id, or removed between the two calls, has no
		// entry. Reported as unknown so it is skipped as an unsupported driver
		// rather than silently treated as bridge.
		driver, ok := drivers[name]
		if !ok {
			driver = "unknown"
		}
		container.Networks = append(container.Networks, Network{Name: name, Driver: driver, IPAddress: network.IPAddress})
	}
	// In name order, which is the order Docker writes them in, so that what
	// is said about them does not change from one pass to the next.
	slices.SortFunc(container.Networks, func(a, b Network) int { return strings.Compare(a.Name, b.Name) })
	return container
}

// publishedPorts is the ports this container actually publishes on the host.
//
// Two things are filtered out. Ports with no PublicPort are merely exposed,
// not published, so nothing on the host listens for them. And Docker reports a
// single `-p 8888:80` twice, once for 0.0.0.0 and once for ::, which would
// otherwise produce a duplicate rule for every published port.
func publishedPorts(raw rawContainer) []PublishedPort {
	found := []PublishedPort{}
	seen := map[string]bool{}
	for _, port := range raw.Ports {
		protocol := model.Protocol(port.Type)
		if port.PublicPort == 0 || !slices.Contains(model.Protocols, protocol) {
			continue
		}
		key := model.Identity(protocol, port.PublicPort)
		if seen[key] {
			continue
		}
		seen[key] = true
		found = append(found, PublishedPort{HostPort: port.PublicPort, ContainerPort: port.PrivatePort, Protocol: protocol})
	}
	return found
}
