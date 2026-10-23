package httpx

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func get(t *testing.T, transport http.RoundTripper, ctx context.Context, url string) (*http.Response, error) {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return (&http.Client{Transport: transport}).Do(request)
}

func read(t *testing.T, response *http.Response) string {
	t.Helper()
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestHandlerTransportServesARequestInMemory(t *testing.T) {
	transport := HandlerTransport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.RequestURI != "/path?q=1" || r.Host != "docker" {
			t.Errorf("served %q to %q", r.RequestURI, r.Host)
		}
		w.Header().Set("X-Seen", r.Method)
		w.WriteHeader(http.StatusTeapot)
		io.WriteString(w, "short and stout")
	}))
	response, err := get(t, transport, context.Background(), "http://docker/path?q=1")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusTeapot || response.Header.Get("X-Seen") != "GET" {
		t.Errorf("got %d %v", response.StatusCode, response.Header)
	}
	if got := read(t, response); got != "short and stout" {
		t.Errorf("got %q", got)
	}
}

func TestHandlerTransportReportsAPanicAsATransportError(t *testing.T) {
	transport := HandlerTransport(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	if _, err := get(t, transport, context.Background(), "http://x/"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("got %v", err)
	}
}

// What /events needs: the first line must be readable while the handler is
// still holding the response open, or a daemon would never see an event.
func TestStreamingHandlerTransportHandsOverEachWriteAsItIsMade(t *testing.T) {
	lines := make(chan string)
	transport := StreamingHandlerTransport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		for {
			select {
			case line := <-lines:
				io.WriteString(w, line+"\n")
			case <-r.Context().Done():
				return
			}
		}
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	response, err := get(t, transport, ctx, "http://docker/events")
	if err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(response.Body)
	for _, want := range []string{"start", "die"} {
		lines <- want
		if !scanner.Scan() || scanner.Text() != want {
			t.Fatalf("got %q, %v", scanner.Text(), scanner.Err())
		}
	}

	cancel()
	if scanner.Scan() {
		t.Errorf("read %q after cancelling", scanner.Text())
	}
	response.Body.Close()
}

func TestStreamingHandlerTransportReturnsOnceTheStatusIsFlushed(t *testing.T) {
	release := make(chan struct{})
	transport := StreamingHandlerTransport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		<-release
	}))
	response, err := get(t, transport, context.Background(), "http://x/")
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	if got := read(t, response); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestStreamingHandlerTransportStopsTheHandlerWhenTheBodyIsClosed(t *testing.T) {
	stopped := make(chan error, 1)
	transport := StreamingHandlerTransport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for {
			if _, err := io.WriteString(w, "tick\n"); err != nil {
				stopped <- err
				return
			}
		}
	}))
	response, err := get(t, transport, context.Background(), "http://x/")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	select {
	case err := <-stopped:
		if !errors.Is(err, errClosed) {
			t.Errorf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler kept writing after the body was closed")
	}
}

func TestStreamingHandlerTransportServesAnOrdinaryResponseToo(t *testing.T) {
	transport := StreamingHandlerTransport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such container", http.StatusNotFound)
	}))
	response, err := get(t, transport, context.Background(), "http://x/")
	if err != nil {
		t.Fatal(err)
	}
	if err := Check(response); err == nil || err.Error() != "404 Not Found: no such container" {
		t.Errorf("got %v", err)
	}
}

func TestStreamingHandlerTransportReportsAPanicBeforeTheStatus(t *testing.T) {
	transport := StreamingHandlerTransport(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	if _, err := get(t, transport, context.Background(), "http://x/"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("got %v", err)
	}
}

func TestStreamingHandlerTransportFailsTheBodyOnAPanicAfterTheStatus(t *testing.T) {
	transport := StreamingHandlerTransport(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "partial")
		panic("boom")
	}))
	response, err := get(t, transport, context.Background(), "http://x/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.ReadAll(response.Body); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("got %v", err)
	}
}

func TestStreamingHandlerTransportGivesUpWaitingForAStatusWhenCancelled(t *testing.T) {
	transport := StreamingHandlerTransport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := get(t, transport, ctx, "http://x/"); !errors.Is(err, context.Canceled) {
		t.Errorf("got %v", err)
	}
}

func TestCheckPassesASuccessThrough(t *testing.T) {
	if err := Check(&http.Response{StatusCode: 204, Body: http.NoBody}); err != nil {
		t.Error(err)
	}
}

func TestCheckCarriesTheServersOwnWords(t *testing.T) {
	err := Check(&http.Response{StatusCode: 500, Status: "500 Internal Server Error", Body: io.NopCloser(strings.NewReader(" busy \n"))})
	var status *StatusError
	if !errors.As(err, &status) || status.StatusCode != 500 || err.Error() != "500 Internal Server Error: busy" {
		t.Errorf("got %v", err)
	}
}

// A unix socket, not a TCP port: the transport Docker is reached through.
func TestUnixSocketReachesAServerWhateverTheURLNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "docker.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.URL.Path)
	})}
	go server.Serve(listener)
	defer server.Close()

	response, err := get(t, UnixSocket(path), context.Background(), "http://docker/v1.41/info")
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, response); got != "/v1.41/info" {
		t.Errorf("got %q", got)
	}
}
