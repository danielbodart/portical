package docker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/danielbodart/portical/internal/fakes"
	"github.com/danielbodart/portical/internal/httpx"
	"github.com/danielbodart/portical/internal/model"
)

// fakeDocker is the Engine API as an http.Handler, so a whole reconcile can be
// driven without a daemon.
//
// Containers are started and stopped by calling methods, and the event stream
// they produce is served the way Docker serves it - one JSON line per event,
// on a response that stays open - so the client under test cannot tell the
// difference. It is reached through httpx.StreamingHandlerTransport, which
// /events needs and every other endpoint works through too.
type fakeDocker struct {
	mu      sync.Mutex
	running []Container
	// Non-empty makes listing fail with this, as a Docker restart would.
	failWith string
	// What inspecting a container says it was created from, by id.
	images map[string]string
	// Answers a helper container's command with what it prints and its exit
	// status. Set by the tests that exercise the relay.
	relay func(call relayCall) (output string, status int)
	// Every helper container Portical ran, in order.
	relayed []relayCall
	// Helpers created and not yet removed.
	helpers map[string]*helper
	created int

	// Events not yet taken by a stream. Kept while no stream is open, so a
	// stream reopened after a drop sees what happened in between.
	queue []event
	wake  chan struct{}
	// Open event streams, each closed to drop it.
	streams map[int]chan struct{}
	opened  int
	mux     *http.ServeMux
}

// relayCall is one helper container Portical asked for.
type relayCall struct {
	Container string
	Image     string
	Command   []string
	Tty       bool
}

type helper struct {
	call   relayCall
	output string
	status int
}

type event struct {
	action string
	id     string
	name   string
	labels map[string]string
}

func newFakeDocker() *fakeDocker {
	f := &fakeDocker{images: map[string]string{}, helpers: map[string]*helper{}, wake: make(chan struct{}, 1), streams: map[int]chan struct{}{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1.41/containers/json", f.list)
	mux.HandleFunc("GET /v1.41/networks", f.networks)
	mux.HandleFunc("GET /v1.41/events", f.events)
	mux.HandleFunc("POST /v1.41/containers/create", f.create)
	mux.HandleFunc("POST /v1.41/containers/{id}/start", f.start)
	mux.HandleFunc("GET /v1.41/containers/{id}/logs", f.logs)
	mux.HandleFunc("POST /v1.41/containers/{id}/wait", f.wait)
	mux.HandleFunc("DELETE /v1.41/containers/{id}", f.remove)
	mux.HandleFunc("GET /v1.41/containers/{id}/json", f.inspect)
	f.mux = mux
	return f
}

func (f *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) { f.mux.ServeHTTP(w, r) }

// client is a Client talking to this Docker.
func (f *fakeDocker) client() *Client {
	return NewClient(httpx.StreamingHandlerTransport(f))
}

// given replaces what is running, without saying so on the event stream.
func (f *fakeDocker) given(containers ...Container) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running = containers
}

func (f *fakeDocker) fail(message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failWith = message
}

// startContainer runs a container and announces it.
func (f *fakeDocker) startContainer(container Container) {
	f.mu.Lock()
	f.running = append(f.running, container)
	f.mu.Unlock()
	f.emit(event{action: "start", id: container.ID, name: container.Name, labels: container.Labels})
}

// stop takes a container away and announces that it died.
func (f *fakeDocker) stop(name string) {
	f.mu.Lock()
	index := slices.IndexFunc(f.running, func(c Container) bool { return c.Name == name })
	if index < 0 {
		f.mu.Unlock()
		panic("no container " + name)
	}
	gone := f.running[index]
	f.running = slices.Delete(f.running, index, index+1)
	f.mu.Unlock()
	f.emit(event{action: "die", id: gone.ID, name: gone.Name, labels: gone.Labels})
}

func (f *fakeDocker) emit(e event) {
	f.mu.Lock()
	f.queue = append(f.queue, e)
	f.mu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// dropStream ends every open event stream, as the socket's idle timeout or a
// Docker restart does.
func (f *fakeDocker) dropStream() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, drop := range f.streams {
		close(drop)
		delete(f.streams, id)
	}
}

// open is how many event streams are open now; opened how many ever were.
func (f *fakeDocker) open() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.streams)
}

func (f *fakeDocker) everOpened() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opened
}

func (f *fakeDocker) relayedCalls() []relayCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.relayed)
}

func (f *fakeDocker) clearRelayed() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.relayed = nil
}

func (f *fakeDocker) leftBehind() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.helpers)
}

// relayThrough makes helper containers run the real `relay` command against
// router, arriving from the address of the container whose network they
// joined - which is exactly what a gateway in secure mode cares about.
func (f *fakeDocker) relayThrough(router *fakes.Gateway) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.relay = func(call relayCall) (string, int) {
		var output bytes.Buffer
		output.WriteString("some runtime warning\n")
		status := RunRelay(call.Command[1], &output, httpx.HandlerTransport(router.From(f.addressOf(call.Container))))
		return output.String(), status
	}
}

func (f *fakeDocker) addressOf(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, container := range f.running {
		if container.Name == name {
			for _, network := range container.Networks {
				if network.IPAddress != "" {
					return network.IPAddress
				}
			}
		}
	}
	return ""
}

// filtersOf reads Docker's filters parameter, and checks it is one.
func filtersOf(r *http.Request) (map[string][]string, error) {
	spec := map[string][]string{}
	if raw := r.URL.Query().Get("filters"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &spec); err != nil {
			return nil, fmt.Errorf("invalid filters: %w", err)
		}
	}
	return spec, nil
}

// labelled is whether labels pass Docker's label filter: every entry is a key
// that must be present, or key=value.
func labelled(labels map[string]string, wanted []string) bool {
	for _, entry := range wanted {
		key, value, exact := strings.Cut(entry, "=")
		have, ok := labels[key]
		if !ok || exact && have != value {
			return false
		}
	}
	return true
}

func answer(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func refuse(w http.ResponseWriter, status int, message string) {
	answer(w, status, map[string]string{"message": message})
}

func (f *fakeDocker) list(w http.ResponseWriter, r *http.Request) {
	spec, err := filtersOf(r)
	if err != nil {
		refuse(w, http.StatusBadRequest, err.Error())
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != "" {
		refuse(w, http.StatusInternalServerError, f.failWith)
		return
	}
	listed := []map[string]any{}
	for _, container := range f.running {
		if !labelled(container.Labels, spec["label"]) {
			continue
		}
		networks := map[string]any{}
		for _, network := range container.Networks {
			networks[network.Name] = map[string]string{"IPAddress": network.IPAddress}
		}
		ports := []map[string]any{}
		for _, port := range container.Published {
			// Twice, as Docker reports a binding: once for 0.0.0.0 and once
			// for ::.
			for _, ip := range []string{"0.0.0.0", "::"} {
				ports = append(ports, map[string]any{"IP": ip, "PrivatePort": port.ContainerPort, "PublicPort": port.HostPort, "Type": string(port.Protocol)})
			}
		}
		listed = append(listed, map[string]any{
			"Id":              container.ID,
			"Names":           []string{"/" + container.Name},
			"Labels":          container.Labels,
			"Ports":           ports,
			"NetworkSettings": map[string]any{"Networks": networks},
		})
	}
	answer(w, http.StatusOK, listed)
}

func (f *fakeDocker) networks(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != "" {
		refuse(w, http.StatusInternalServerError, f.failWith)
		return
	}
	drivers := map[string]string{"bridge": "bridge", "host": "host", "none": "null"}
	for _, container := range f.running {
		for _, network := range container.Networks {
			// A driver Docker has no record of is how a network removed
			// between the two calls looks.
			if network.Driver != "unknown" {
				drivers[network.Name] = network.Driver
			}
		}
	}
	listed := []map[string]string{}
	for name, driver := range drivers {
		listed = append(listed, map[string]string{"Name": name, "Driver": driver})
	}
	answer(w, http.StatusOK, listed)
}

func (f *fakeDocker) events(w http.ResponseWriter, r *http.Request) {
	spec, err := filtersOf(r)
	if err != nil {
		refuse(w, http.StatusBadRequest, err.Error())
		return
	}
	f.mu.Lock()
	id := f.opened
	f.opened++
	drop := make(chan struct{})
	f.streams[id] = drop
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.streams, id)
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()

	wanted := func(e event) bool {
		return (len(spec["type"]) == 0 || slices.Contains(spec["type"], "container")) &&
			(len(spec["event"]) == 0 || slices.Contains(spec["event"], e.action)) &&
			labelled(e.labels, spec["label"])
	}
	for {
		f.mu.Lock()
		var next event
		queued := len(f.queue) > 0
		if queued {
			next, f.queue = f.queue[0], f.queue[1:]
		}
		f.mu.Unlock()

		if !queued {
			select {
			case <-f.wake:
			case <-drop:
				return
			case <-r.Context().Done():
				return
			}
			continue
		}
		if !wanted(next) {
			continue
		}
		attributes := map[string]string{"name": next.name, "image": "nginx:latest"}
		for key, value := range next.labels {
			attributes[key] = value
		}
		line, _ := json.Marshal(map[string]any{
			"status": next.action, "id": next.id, "from": "nginx:latest",
			"Type": "container", "Action": next.action,
			"Actor": map[string]any{"ID": next.id, "Attributes": attributes},
			"scope": "local",
		})
		if _, err := w.Write(append(line, '\n')); err != nil {
			return
		}
		w.(http.Flusher).Flush()
	}
}

func (f *fakeDocker) create(w http.ResponseWriter, r *http.Request) {
	var spec struct {
		Image      string
		Cmd        []string
		Tty        bool
		HostConfig struct{ NetworkMode string }
	}
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil || r.Header.Get("Content-Type") != "application/json" {
		refuse(w, http.StatusBadRequest, fmt.Sprintf("invalid create request: %v", err))
		return
	}
	container, ok := strings.CutPrefix(spec.HostConfig.NetworkMode, "container:")
	if !ok {
		refuse(w, http.StatusBadRequest, "expected to join another container's network, got "+spec.HostConfig.NetworkMode)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created++
	id := fmt.Sprintf("helper%d", f.created)
	call := relayCall{Container: container, Image: spec.Image, Command: spec.Cmd, Tty: spec.Tty}
	f.helpers[id] = &helper{call: call}
	f.relayed = append(f.relayed, call)
	answer(w, http.StatusCreated, map[string]any{"Id": id, "Warnings": []string{}})
}

func (f *fakeDocker) helper(w http.ResponseWriter, r *http.Request) *helper {
	f.mu.Lock()
	defer f.mu.Unlock()
	found, ok := f.helpers[r.PathValue("id")]
	if !ok {
		refuse(w, http.StatusNotFound, "No such container: "+r.PathValue("id"))
		return nil
	}
	return found
}

// start runs the helper to completion at once: what it prints and how it
// exits are ready for logs and wait.
func (f *fakeDocker) start(w http.ResponseWriter, r *http.Request) {
	found := f.helper(w, r)
	if found == nil {
		return
	}
	f.mu.Lock()
	relay := f.relay
	f.mu.Unlock()
	output, status := "exec: no relay configured for "+found.call.Container+"\n", 1
	if relay != nil {
		output, status = relay(found.call)
	}
	f.mu.Lock()
	found.output, found.status = output, status
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeDocker) logs(w http.ResponseWriter, r *http.Request) {
	found := f.helper(w, r)
	if found == nil {
		return
	}
	if r.URL.Query().Get("follow") != "1" {
		refuse(w, http.StatusBadRequest, "the relay must follow the logs to have the whole of them")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
	w.Write([]byte(found.output))
}

func (f *fakeDocker) wait(w http.ResponseWriter, r *http.Request) {
	found := f.helper(w, r)
	if found == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	answer(w, http.StatusOK, map[string]any{"StatusCode": found.status})
}

func (f *fakeDocker) remove(w http.ResponseWriter, r *http.Request) {
	if f.helper(w, r) == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.helpers, r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeDocker) inspect(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	image, ok := f.images[r.PathValue("id")]
	if !ok {
		refuse(w, http.StatusNotFound, "No such container: "+r.PathValue("id"))
		return
	}
	answer(w, http.StatusOK, map[string]any{"Id": r.PathValue("id"), "Config": map[string]any{"Image": image}})
}

// container is a running container in our own shape, on the default bridge
// unless networks are given.
func container(name string, labels map[string]string, networks ...Network) Container {
	if len(networks) == 0 {
		networks = []Network{network("bridge", "bridge", "172.17.0.2")}
	}
	return Container{ID: "id-" + name, Name: name, Labels: labels, Networks: networks, Published: []PublishedPort{}}
}

func network(name, driver, ipAddress string) Network {
	return Network{Name: name, Driver: driver, IPAddress: ipAddress}
}

func publishing(c Container, ports ...PublishedPort) Container {
	c.Published = ports
	return c
}

func port(host, container int, protocol model.Protocol) PublishedPort {
	return PublishedPort{HostPort: host, ContainerPort: container, Protocol: protocol}
}
