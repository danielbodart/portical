package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/danielbodart/gokit/httpx"
	"github.com/danielbodart/portical/internal/docker"
	"github.com/danielbodart/portical/internal/fakes"
	"github.com/danielbodart/portical/internal/systemd"
)

// output is a stream a test reads back while Main may still be writing it.
type output struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.data.Write(p)
}

func (o *output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.data.String()
}

// fakeDocker is as much of the Engine API as the command line reaches: the
// containers and networks a pass reads, the event stream `run` holds open,
// inspecting our own container, and running the relay helper.
//
// The Docker source's own behaviour is tested against a far fuller fake in
// its package. This one is here to show the command line wires it up - the
// socket, the labels, the helper image - and so it is deliberately small.
type fakeDocker struct {
	router *fakes.Gateway

	mu         sync.Mutex
	containers []any
	networks   []any
	images     map[string]string
	helpers    map[string]helper
	created    []string // the image of each helper, in order
	streams    int
	requests   []string
}

type helper struct {
	address string
	command []string
}

func newFakeDocker(router *fakes.Gateway) *fakeDocker {
	return &fakeDocker{router: router, images: map[string]string{}, helpers: map[string]helper{}}
}

// container is one running container, as /containers/json lists it.
func (f *fakeDocker) container(name string, labels map[string]string, network, driver, address string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers = append(f.containers, map[string]any{
		"Id":              strings.Repeat("c", 63) + string(rune('0'+len(f.containers))),
		"Names":           []string{"/" + name},
		"Labels":          labels,
		"NetworkSettings": map[string]any{"Networks": map[string]any{network: map[string]any{"IPAddress": address}}},
	})
	f.networks = append(f.networks, map[string]any{"Name": network, "Driver": driver})
}

func (f *fakeDocker) transport() func(string) http.RoundTripper {
	return func(socket string) http.RoundTripper {
		f.mu.Lock()
		f.requests = append(f.requests, "socket "+socket)
		f.mu.Unlock()
		return httpx.StreamingHandlerTransport(f)
	}
}

func (f *fakeDocker) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func (f *fakeDocker) helperImages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.created...)
}

func (f *fakeDocker) open() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.streams
}

func (f *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/"+docker.API)
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+path)
	f.mu.Unlock()

	answer := func(value any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(value)
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case path == "/containers/json":
		f.mu.Lock()
		defer f.mu.Unlock()
		answer(f.containers)
	case path == "/networks":
		f.mu.Lock()
		defer f.mu.Unlock()
		answer(f.networks)
	case path == "/events":
		f.mu.Lock()
		f.streams++
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		f.mu.Lock()
		f.streams--
		f.mu.Unlock()
	case path == "/containers/create":
		var body struct {
			Image      string
			Cmd        []string
			HostConfig struct{ NetworkMode string }
		}
		json.NewDecoder(r.Body).Decode(&body)
		target := strings.TrimPrefix(body.HostConfig.NetworkMode, "container:")
		f.mu.Lock()
		id := "helper" + string(rune('0'+len(f.helpers)))
		f.helpers[id] = helper{address: f.addressOf(target), command: body.Cmd}
		f.created = append(f.created, body.Image)
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		answer(map[string]string{"Id": id})
	case len(parts) == 3 && parts[0] == "containers" && parts[2] == "start":
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 3 && parts[0] == "containers" && parts[2] == "logs":
		// The helper runs Portical's own relay command, from the target
		// container's address.
		f.mu.Lock()
		h := f.helpers[parts[1]]
		f.mu.Unlock()
		var printed bytes.Buffer
		docker.RunRelay(h.command[1], &printed, httpx.HandlerTransport(f.router.From(h.address)))
		w.Write(printed.Bytes())
	case len(parts) == 3 && parts[0] == "containers" && parts[2] == "wait":
		answer(map[string]int{"StatusCode": 0})
	case len(parts) == 2 && parts[0] == "containers" && r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 3 && parts[0] == "containers" && parts[2] == "json":
		f.mu.Lock()
		image, ok := f.images[parts[1]]
		f.mu.Unlock()
		if !ok {
			http.Error(w, `{"message":"No such container"}`, http.StatusNotFound)
			return
		}
		answer(map[string]any{"Config": map[string]string{"Image": image}})
	default:
		http.Error(w, `{"message":"page not found"}`, http.StatusNotFound)
	}
}

func (f *fakeDocker) addressOf(name string) string {
	for _, c := range f.containers {
		c := c.(map[string]any)
		if c["Names"].([]string)[0] == "/"+name {
			for _, network := range c["NetworkSettings"].(map[string]any)["Networks"].(map[string]any) {
				return network.(map[string]any)["IPAddress"].(string)
			}
		}
	}
	return ""
}

// fakeSystemd is the system bus with some units running, and their files.
type fakeSystemd struct {
	units []systemd.Unit
	files map[string]string

	mu      sync.Mutex
	watches int
}

func (s *fakeSystemd) Units(context.Context) ([]systemd.Unit, error) { return s.units, nil }

func (s *fakeSystemd) ControlGroup(_ context.Context, unit string) (string, error) {
	return "/system.slice/" + unit, nil
}

func (s *fakeSystemd) Watch(ctx context.Context, _ func(systemd.Change)) error {
	s.mu.Lock()
	s.watches++
	s.mu.Unlock()
	<-ctx.Done()
	s.mu.Lock()
	s.watches--
	s.mu.Unlock()
	return nil
}

func (s *fakeSystemd) watching() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.watches
}

// unit is a running service whose unit file says text.
func (s *fakeSystemd) unit(name, text string) {
	path := "/etc/systemd/system/" + name
	s.units = append(s.units, systemd.Unit{Name: name, FragmentPath: path})
	if s.files == nil {
		s.files = map[string]string{}
	}
	s.files[strings.TrimPrefix(path, "/")] = text
}
