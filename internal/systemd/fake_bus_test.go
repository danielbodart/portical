package systemd

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/godbus/dbus/v5"
)

// fakeBus is a message bus on a Unix socket in a test's temporary directory.
//
// Real enough that godbus cannot tell: the SASL handshake, framed messages
// both ways, replies matched by serial. So what is tested is the adapter's
// conversation with a bus - what it asks, in what order, how many at once,
// and what it makes of the answers and the signals - not godbus, which has
// tests of its own. Method calls are answered by answer, and every call is
// recorded.
type fakeBus struct {
	address string
	server  net.Listener

	mu      sync.Mutex
	calls   []*dbus.Message
	clients map[*busClient]bool
	serial  uint32
	answer  func(call *dbus.Message) answer
	// Set to answer calls concurrently and refuse any that would leave more
	// than this many waiting, as dbus-daemon's max_replies_per_connection
	// does.
	maxPending  int
	pending     int
	mostPending int
}

// answer is a reply's body, or an error in its place.
type answer struct {
	body []any
	err  *dbus.Error
}

type busClient struct {
	conn net.Conn
	mu   sync.Mutex // one message written at a time
}

func newFakeBus(t *testing.T) *fakeBus {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bus")
	server, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	bus := &fakeBus{
		address: "unix:path=" + path,
		server:  server,
		clients: map[*busClient]bool{},
		serial:  1000,
		answer:  func(*dbus.Message) answer { return answer{} },
	}
	go bus.accept()
	t.Cleanup(func() {
		server.Close()
		bus.Disconnect()
	})
	return bus
}

// Bus is the adapter, talking to this bus.
func (b *fakeBus) Bus(t *testing.T) *DBus {
	t.Helper()
	bus, err := SystemBus(map[string]string{"DBUS_SYSTEM_BUS_ADDRESS": b.address})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bus.(*DBus).Close() })
	return bus.(*DBus)
}

func (b *fakeBus) Answer(answer func(call *dbus.Message) answer) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.answer = answer
}

func (b *fakeBus) MaxPending(limit int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.maxPending = limit
}

func (b *fakeBus) MostPending() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.mostPending
}

// Calls is every method call received with this member, in order.
func (b *fakeBus) Calls(member string) []*dbus.Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	var found []*dbus.Message
	for _, call := range b.calls {
		if header[string](call, dbus.FieldMember) == member {
			found = append(found, call)
		}
	}
	return found
}

func (b *fakeBus) Connected() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.clients)
}

// Disconnect drops every client, as a bus restart would.
func (b *fakeBus) Disconnect() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for client := range b.clients {
		client.conn.Close()
	}
	b.clients = map[*busClient]bool{}
}

// Signal sends a signal to every client.
func (b *fakeBus) Signal(path dbus.ObjectPath, iface, member string, body ...any) {
	headers := map[dbus.HeaderField]dbus.Variant{
		dbus.FieldPath:      dbus.MakeVariant(path),
		dbus.FieldInterface: dbus.MakeVariant(iface),
		dbus.FieldMember:    dbus.MakeVariant(member),
		dbus.FieldSender:    dbus.MakeVariant(":1.1"),
	}
	if len(body) > 0 {
		headers[dbus.FieldSignature] = dbus.MakeVariant(dbus.SignatureOf(body...))
	}
	b.mu.Lock()
	clients := make([]*busClient, 0, len(b.clients))
	for client := range b.clients {
		clients = append(clients, client)
	}
	b.mu.Unlock()
	for _, client := range clients {
		b.send(client, &dbus.Message{Type: dbus.TypeSignal, Headers: headers, Body: body})
	}
}

func header[T any](message *dbus.Message, field dbus.HeaderField) T {
	value, _ := message.Headers[field].Value().(T)
	return value
}

func (b *fakeBus) accept() {
	for {
		conn, err := b.server.Accept()
		if err != nil {
			return
		}
		go b.serve(&busClient{conn: conn})
	}
}

func (b *fakeBus) serve(client *busClient) {
	defer client.conn.Close()
	in := bufio.NewReader(client.conn)
	if !b.authenticate(client, in) {
		return
	}
	b.mu.Lock()
	b.clients[client] = true
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.clients, client)
	}()

	for {
		call, err := dbus.DecodeMessage(in)
		if err != nil {
			return
		}
		b.mu.Lock()
		concurrent := b.maxPending > 0
		b.mu.Unlock()
		if concurrent {
			go b.respond(client, call)
		} else {
			b.respond(client, call)
		}
	}
}

// authenticate is the server's half of SASL: refuse the bare AUTH with the
// one mechanism on offer, accept EXTERNAL, decline file descriptors.
func (b *fakeBus) authenticate(client *busClient, in *bufio.Reader) bool {
	if nul, err := in.ReadByte(); err != nil || nul != 0 {
		return false
	}
	for {
		line, err := in.ReadString('\n')
		if err != nil {
			return false
		}
		line = strings.TrimRight(line, "\r\n")
		var reply string
		switch {
		case line == "AUTH":
			reply = "REJECTED EXTERNAL"
		case strings.HasPrefix(line, "AUTH EXTERNAL"):
			reply = "OK 0123456789abcdef0123456789abcdef"
		case line == "NEGOTIATE_UNIX_FD":
			reply = "ERROR"
		case line == "BEGIN":
			return true
		default:
			reply = "ERROR"
		}
		if _, err := io.WriteString(client.conn, reply+"\r\n"); err != nil {
			return false
		}
	}
}

func (b *fakeBus) respond(client *busClient, call *dbus.Message) {
	b.mu.Lock()
	b.calls = append(b.calls, call)
	answerer := b.answer
	refuse := b.maxPending > 0 && b.pending >= b.maxPending
	if !refuse {
		b.pending++
		b.mostPending = max(b.mostPending, b.pending)
	}
	b.mu.Unlock()

	var reply answer
	switch {
	case refuse:
		reply = answer{err: dbus.NewError("org.freedesktop.DBus.Error.LimitsExceeded",
			[]any{"The maximum number of pending replies per connection has been reached"})}
	case header[string](call, dbus.FieldMember) == "Hello":
		reply = answer{body: []any{":1.42"}}
	default:
		reply = answerer(call)
	}
	if !refuse {
		b.mu.Lock()
		b.pending--
		b.mu.Unlock()
	}

	headers := map[dbus.HeaderField]dbus.Variant{
		dbus.FieldReplySerial: dbus.MakeVariant(call.Serial()),
		dbus.FieldDestination: dbus.MakeVariant(":1.42"),
	}
	message := &dbus.Message{Type: dbus.TypeMethodReply, Headers: headers, Body: reply.body}
	if reply.err != nil {
		headers[dbus.FieldErrorName] = dbus.MakeVariant(reply.err.Name)
		message = &dbus.Message{Type: dbus.TypeError, Headers: headers, Body: reply.err.Body}
	}
	if len(message.Body) > 0 {
		headers[dbus.FieldSignature] = dbus.MakeVariant(dbus.SignatureOf(message.Body...))
	}
	b.send(client, message)
}

// send writes a message with the bus's next serial. godbus will not set a
// serial on a message it did not send itself, so it is written into the
// encoded header, where it sits at offset 8.
func (b *fakeBus) send(client *busClient, message *dbus.Message) {
	var buffer bytes.Buffer
	if err := message.EncodeTo(&buffer, binary.LittleEndian); err != nil {
		panic(err)
	}
	b.mu.Lock()
	b.serial++
	binary.LittleEndian.PutUint32(buffer.Bytes()[8:12], b.serial)
	b.mu.Unlock()
	client.mu.Lock()
	defer client.mu.Unlock()
	client.conn.Write(buffer.Bytes())
}
