package httpx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// HandlerTransport turns an http.Handler into an http.RoundTripper: each
// request is served in memory, with no socket and no port, and the recorded
// response handed back as the server's.
//
// The whole response is recorded before the client sees any of it, which is
// right for every request/response exchange and wrong for a stream that never
// ends - for those, see StreamingHandlerTransport.
//
// A panicking handler surfaces as a transport error, as http.Server turns one
// into a dropped connection.
func HandlerTransport(handler http.Handler) http.RoundTripper {
	return RoundTripperFunc(func(request *http.Request) (response *http.Response, err error) {
		if request.Body != nil {
			defer request.Body.Close()
		}
		defer func() {
			if value := recover(); value != nil {
				response, err = nil, panicked(request, value)
			}
		}()
		recorder := &recorder{header: http.Header{}}
		handler.ServeHTTP(recorder, served(request))
		return recorder.response(request), nil
	})
}

// recorder is the ResponseWriter HandlerTransport serves into. It is
// httptest.ResponseRecorder's job, done here so the production binary does
// not link the testing packages that one brings in.
type recorder struct {
	header  http.Header
	written http.Header // the header as it was when the status was written
	status  int
	body    bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status, r.written = status, r.header.Clone()
	}
}

func (r *recorder) Write(p []byte) (int, error) {
	if r.status == 0 && r.header.Get("Content-Type") == "" {
		r.header.Set("Content-Type", http.DetectContentType(p))
	}
	r.WriteHeader(http.StatusOK)
	return r.body.Write(p)
}

// Flush does nothing: everything is handed over at the end anyway.
func (r *recorder) Flush() {}

func (r *recorder) response(request *http.Request) *http.Response {
	r.WriteHeader(http.StatusOK)
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", r.status, http.StatusText(r.status)),
		StatusCode:    r.status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        r.written,
		Body:          io.NopCloser(bytes.NewReader(r.body.Bytes())),
		ContentLength: int64(r.body.Len()),
		Request:       request,
	}
}

// StreamingHandlerTransport is HandlerTransport for a response that is read
// while it is still being written.
//
// Docker's /events never finishes: it holds the response open and writes a
// line whenever a container starts or stops. Recorded whole, the client would
// wait forever for the first event. Here the handler runs on its own
// goroutine and its body reaches the client through a pipe, so the response
// is returned as soon as the handler commits its status - by WriteHeader, its
// first Write, or a Flush - and each write is readable as soon as it is made.
//
// It ends the way a real connection does. Cancelling the request's context
// cancels the context the handler sees and fails the client's read; closing
// the body fails the handler's next write. A handler that panics before
// committing a status fails the round trip, and after it fails the body.
func StreamingHandlerTransport(handler http.Handler) http.RoundTripper {
	return RoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		reader, writer := io.Pipe()
		stream := &stream{header: http.Header{}, pipe: writer, committed: make(chan struct{})}

		go func() {
			if request.Body != nil {
				defer request.Body.Close()
			}
			defer func() {
				if value := recover(); value != nil {
					stream.fail(panicked(request, value))
					return
				}
				stream.commit(http.StatusOK)
				writer.Close()
			}()
			handler.ServeHTTP(stream, served(request))
		}()

		select {
		case <-stream.committed:
		case <-request.Context().Done():
			reader.CloseWithError(request.Context().Err())
			return nil, request.Context().Err()
		}
		if stream.err != nil {
			return nil, stream.err
		}

		// A real connection's body fails its read once the request is
		// cancelled, and a reader blocked on a quiet stream depends on that to
		// ever return.
		stop := context.AfterFunc(request.Context(), func() { reader.CloseWithError(request.Context().Err()) })
		return &http.Response{
			Status:        fmt.Sprintf("%d %s", stream.status, http.StatusText(stream.status)),
			StatusCode:    stream.status,
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
			Header:        stream.written,
			Body:          &body{reader: reader, stop: stop},
			ContentLength: -1,
			Request:       request,
		}, nil
	})
}

// stream is the ResponseWriter StreamingHandlerTransport serves into.
type stream struct {
	header    http.Header
	written   http.Header
	status    int
	err       error
	pipe      *io.PipeWriter
	once      sync.Once
	committed chan struct{}
}

func (s *stream) Header() http.Header { return s.header }

func (s *stream) commit(status int) {
	s.once.Do(func() {
		s.status, s.written = status, s.header.Clone()
		close(s.committed)
	})
}

// fail ends the exchange: before a status was committed the round trip
// returns err, and after it the client's next read does.
func (s *stream) fail(err error) {
	s.once.Do(func() {
		s.err = err
		close(s.committed)
	})
	s.pipe.CloseWithError(err)
}

func (s *stream) WriteHeader(status int) { s.commit(status) }

func (s *stream) Write(p []byte) (int, error) {
	if s.header.Get("Content-Type") == "" {
		s.header.Set("Content-Type", http.DetectContentType(p))
	}
	s.commit(http.StatusOK)
	return s.pipe.Write(p)
}

// Flush commits the status, so a handler can hand over its headers before it
// has anything to write. Each Write is already handed over as it is made.
func (s *stream) Flush() { s.commit(http.StatusOK) }

type body struct {
	reader *io.PipeReader
	stop   func() bool
}

func (b *body) Read(p []byte) (int, error) { return b.reader.Read(p) }

func (b *body) Close() error {
	b.stop()
	return b.reader.CloseWithError(errClosed)
}

// errClosed is what a streaming handler's write returns once the client has
// stopped reading.
var errClosed = errors.New("httpx: response body closed by the client")
