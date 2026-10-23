// Package daemon is one reconcile pass, and the loop that repeats it.
//
// v1 offered `listen` and `poll` as alternatives, which is the wrong shape:
// `listen` reacted to container starts but never renewed a lease or noticed a
// container stopping, so its mappings quietly expired; `poll` renewed but
// reacted no faster than its interval. They are not alternatives at all.
// Watching tells you when something changed; the interval catches lease
// expiry and any drift the events missed. This runs both into the same
// reconcile.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danielbodart/gokit/clock"
	"github.com/danielbodart/portical/internal/model"
	"github.com/danielbodart/portical/internal/reconcile"
	"github.com/danielbodart/portical/internal/source"
	"github.com/danielbodart/portical/internal/upnp"
)

// Log is where the daemon says what it is doing, one line at a time, without
// a trailing newline. The daemon never calls it from two goroutines at once.
type Log func(line string)

// Options shape the daemon.
type Options struct {
	// Between reconcile passes.
	Interval time.Duration
	// Rewrite a mapping whose remaining lease is below this.
	RenewWithin time.Duration
	// Ask the gateway for this lease, in whole seconds. 0 means never expire.
	LeaseDuration time.Duration
	Force         bool
	Steal         bool
	DryRun        bool
	// Remove our mappings on shutdown, rather than leaving them in place.
	// Acted on by whoever runs the daemon, by calling Cleanup after Run.
	CleanupOnExit bool
	// Manage every mapping carrying Portical's description, wherever it
	// points.
	//
	// Off by default, because it is only safe when there is exactly one
	// Portical on the network - see ManagedAddresses in reconcile.
	ManageAll bool
	// This host's address on the LAN, for everything forwarded to the host.
	HostAddress string
	// How long to wait before reopening a dropped event stream.
	ReconnectBackoff time.Duration
}

// Defaults is how the daemon runs unless told otherwise.
var Defaults = Options{
	Interval: 15 * time.Second,
	// Half a day, so a mapping the router downgraded to a 24 hour lease gets
	// renewed with plenty of room, without re-pushing every rule constantly.
	RenewWithin:      12 * time.Hour,
	ReconnectBackoff: time.Second,
}

// Deps is what a Daemon is built from.
type Deps struct {
	Source  source.Source
	Gateway upnp.Gateway
	Options Options
	Log     Log
	// Time, for the interval and the reconnect backoff. The system clock
	// when nil.
	Clock clock.Clock
	// The same gateway, reached from inside a container's network namespace.
	//
	// Only used for a forward whose target is the container's own address -
	// see gatewayFor. Nil asks for everything from the host.
	AsContainer func(owner string) upnp.Gateway
}

// Daemon brings the gateway into line with a source, once or continuously.
type Daemon struct {
	source      source.Source
	gateway     upnp.Gateway
	options     Options
	log         Log
	clock       clock.Clock
	asContainer func(owner string) upnp.Gateway

	// One pass at a time, whoever asks for it.
	mu sync.Mutex
	// Addresses this instance may remove mappings for, beyond the ones the
	// current pass names.
	//
	// Everything written this run, so a container that stops while we are up
	// is still cleaned up even though it is no longer around to tell us its
	// address.
	written     map[string]bool
	lastSummary string
}

// New is a daemon over deps.
func New(deps Deps) *Daemon {
	if deps.Clock == nil {
		deps.Clock = clock.System{}
	}
	if deps.Log == nil {
		deps.Log = func(string) {}
	}
	// Passes and the event stream both speak, on different goroutines, so
	// their lines are taken one at a time here rather than every Log having
	// to be safe for it.
	var speaking sync.Mutex
	log := func(line string) {
		speaking.Lock()
		defer speaking.Unlock()
		deps.Log(line)
	}
	return &Daemon{
		source:      deps.Source,
		gateway:     deps.Gateway,
		options:     deps.Options,
		log:         log,
		clock:       deps.Clock,
		asContainer: deps.AsContainer,
		written:     map[string]bool{},
	}
}

// Once brings the gateway into line with the source, once.
//
// A source failure is returned rather than logged and skipped. Removals are
// derived from absence, so reconciling against a list we failed to fetch
// would read as "everything stopped" and tear down every live mapping. A rule
// the gateway refuses is not a failure of the pass - see attempt.
func (d *Daemon) Once(ctx context.Context) ([]reconcile.Action, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	desired, err := d.source.Desired(ctx)
	if err != nil {
		return nil, err
	}
	for _, warning := range desired.Warnings {
		d.log("Warning: " + warning)
	}

	mappings, err := d.gateway.Mappings(ctx)
	if err != nil {
		return nil, err
	}

	actions := reconcile.Reconcile(desired.Forwards, mappings, d.reconcileOptions(desired.Forwards))
	for _, action := range actions {
		d.apply(ctx, action)
	}

	// Reported only when it changes, so a daemon reconciling every 15 seconds
	// says something the first time and then stays quiet until it has news.
	if summary := summarise(actions); summary != d.lastSummary {
		d.log(summary)
		d.lastSummary = summary
	}
	return actions, nil
}

// Cleanup removes every mapping we own.
//
// Only run when asked. A restarting Portical should not take down the
// forwards it is about to put straight back, so leaving them in place is the
// default and this is opt-in.
func (d *Daemon) Cleanup(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.log("Removing Portical's mappings before exit...")
	mappings, err := d.gateway.Mappings(ctx)
	if err != nil {
		return err
	}
	options := d.reconcileOptions(nil)
	for _, action := range reconcile.Reconcile(nil, mappings, reconcile.Options{ManagedAddresses: options.ManagedAddresses}) {
		d.apply(ctx, action)
	}
	return nil
}

func (d *Daemon) reconcileOptions(forwards []model.DesiredForward) reconcile.Options {
	options := reconcile.Options{Force: d.options.Force, RenewWithin: d.options.RenewWithin, Steal: d.options.Steal}
	if d.options.ManageAll {
		return options
	}
	addresses := map[string]bool{}
	for address := range d.written {
		addresses[address] = true
	}
	if d.options.HostAddress != "" {
		addresses[d.options.HostAddress] = true
	}
	for _, forward := range forwards {
		if forward.InternalClient != "" {
			addresses[forward.InternalClient] = true
		}
	}
	options.ManagedAddresses = addresses
	return options
}

func (d *Daemon) apply(ctx context.Context, action reconcile.Action) {
	switch action.Kind {
	case reconcile.Keep:

	case reconcile.Conflict:
		d.log(fmt.Sprintf("Conflict: %s wants %s but it is %s. Use --steal to take it over.",
			action.Forward.Owner, action.Forward.Rule, strings.Replace(action.Reason, "already forwarded by something else ", "", 1)))

	case reconcile.Orphan:
		d.log(fmt.Sprintf("Leaving %s alone - %s. It will be reclaimed if that container returns, or "+
			"expire on its own if the gateway set a lease.", action.Mapping.Identity(), action.Reason))

	case reconcile.Remove:
		d.log(fmt.Sprintf("Removing %s - %s", action.Mapping.Identity(), action.Reason))
		if d.options.DryRun {
			return
		}
		d.attempt(func() error {
			return d.gateway.Remove(ctx, action.Mapping.ExternalPort, action.Mapping.Protocol)
		}, nil)

	case reconcile.Add, reconcile.Renew, reconcile.Replace:
		forward := action.Forward
		verb := map[reconcile.Kind]string{reconcile.Add: "Adding", reconcile.Renew: "Renewing", reconcile.Replace: "Replacing"}[action.Kind]
		d.log(fmt.Sprintf("%s %s for %s - %s", verb, forward.Rule, forward.Owner, action.Reason))
		if d.options.DryRun {
			return
		}
		var hint func(error) string
		if forward.Target.Kind == model.TargetContainer {
			hint = relayHint
		}
		d.attempt(func() error { return d.write(ctx, action) }, hint)
	}
}

func (d *Daemon) write(ctx context.Context, action reconcile.Action) error {
	forward := action.Forward
	gateway := d.gatewayFor(forward)

	// Removed first only when the mapping points somewhere else, which is the
	// one case that needs it: several firmwares answer 718 rather than
	// overwriting when the internal address changes.
	//
	// Never for a renewal. AddPortMapping on an unchanged target updates the
	// lease in place, whereas deleting first leaves the port briefly
	// unforwarded. Established flows would not notice - they are carried by
	// conntrack, not by the redirect - but anyone trying to connect in that
	// window is refused, which on a game server reads as the server having
	// gone down for no reason.
	if action.Kind == reconcile.Replace {
		if err := gateway.Remove(ctx, forward.Rule.ExternalPort, forward.Rule.Protocol); err != nil {
			return err
		}
	}

	internalClient := forward.InternalClient
	if internalClient == "" {
		internalClient = d.options.HostAddress
	}
	if internalClient == "" {
		return errors.New("cannot tell which address to forward to - set --root so the host's " +
			"address on the LAN can be worked out")
	}

	err := gateway.Add(ctx, upnp.NewMapping{
		ExternalPort:   forward.Rule.ExternalPort,
		Protocol:       forward.Rule.Protocol,
		InternalPort:   forward.Rule.InternalPort,
		InternalClient: internalClient,
		Description:    model.Describe(forward.Rule, forward.Owner),
		LeaseDuration:  int(d.options.LeaseDuration / time.Second),
	})
	if err != nil {
		return err
	}

	// Remembered so this mapping stays ours to clean up after whatever it
	// belongs to has gone and can no longer be asked.
	d.written[internalClient] = true
	return nil
}

// gatewayFor is which vantage point to ask the gateway from.
//
// Decided by the target, which is known up front, rather than by trying from
// the host and reacting to a refusal. A rule for a macvlan or ipvlan container
// names that container's own address, so the request has to come from there -
// gateways running miniupnpd with secure_mode on only allow a client to map to
// itself.
//
// Not inferred from the error code, because the code that comes back is 718
// ConflictInMappingEntry, which also means a genuine collision with someone
// else's mapping. Treating it as "wrong vantage point" would start a container
// to retry a conflict that no vantage point can fix, and would hide the real
// reason. The target says which case this is with certainty.
func (d *Daemon) gatewayFor(forward model.DesiredForward) upnp.Gateway {
	if forward.Target.Kind == model.TargetContainer && d.asContainer != nil {
		return d.asContainer(forward.Owner)
	}
	return d.gateway
}

// attempt reports a failed rule instead of ending the pass.
//
// One rule the router will not accept must not stop the other rules being
// applied, and must not take the daemon down. v1 exited the whole process on
// any upnpc failure, which is why a single stale mapping could stop every
// forward on the host.
func (d *Daemon) attempt(work func() error, hint func(error) string) {
	err := work()
	if err == nil {
		return
	}
	d.log("  FAILED: " + err.Error())
	if hint == nil {
		return
	}
	if explanation := hint(err); explanation != "" {
		d.log("  " + explanation)
	}
}

// relayHint explains a failure that happened while asking as the container.
//
// A rule for a macvlan or ipvlan container is created by starting a throwaway
// container joined to its network, so the things that can go wrong are
// different from an ordinary request, and none of them are obvious from the
// message Docker or the gateway returns.
//
// 718 keeps its plain meaning here. Because the request was already made from
// the address the rule names, a conflict really is a conflict rather than the
// gateway objecting to who asked.
func relayHint(err error) string {
	var refused *upnp.Error
	if errors.As(err, &refused) {
		if refused.Code == upnp.Conflict {
			return "The gateway says another mapping already claims that external port. " +
				"Portical asked as the container itself, so this is a real conflict " +
				"rather than the gateway objecting to who asked."
		}
		return ""
	}
	return "This rule points at the container's own address, so Portical asks the " +
		"gateway from inside that container's network by briefly running its own " +
		"image there. Check the Docker socket is writable and that Portical's " +
		"image can be pulled on this host, or name it with --helper-image."
}

// Run reconciles continuously until ctx is done.
//
// The event stream and the interval both do nothing but ask for another pass,
// so a burst of activity collapses into one reconcile rather than one per
// event. Returns once the pass in flight, if any, has finished: a pass is
// never abandoned halfway through its writes, so shutting down cannot leave a
// rule removed and not yet put back.
func (d *Daemon) Run(ctx context.Context) {
	// Passes outlive ctx, for the reason above. Each request they make is
	// still bounded by the client's own timeout.
	passes := context.WithoutCancel(ctx)
	pass := func() {
		if _, err := d.Once(passes); err != nil {
			// A pass that fails is not fatal: Docker may be restarting, or the
			// router may be briefly unreachable. The next one tries again.
			d.log("Error: " + err.Error())
		}
	}

	pass()

	// Room for one: a request made while a pass is running asks for the one
	// pass queued behind it, rather than each waiting its turn for a pass of
	// its own. Otherwise a boot or a rebuild restarting dozens of units turns
	// into dozens of passes in a row, each walking the router's table.
	requested := make(chan struct{}, 1)
	request := func() {
		select {
		case requested <- struct{}{}:
		default:
		}
	}

	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-requested:
				pass()
			}
		}
	})
	wg.Go(func() {
		for clock.Sleep(ctx, d.clock, d.options.Interval) == nil {
			request()
		}
	})

	d.log(fmt.Sprintf("Watching for changes, reconciling every %ss...",
		strconv.FormatFloat(d.options.Interval.Seconds(), 'f', -1, 64)))

	// Reopened rather than given up on when the stream ends. Losing it is not
	// fatal: the interval keeps reconciling, so the worst case is degrading to
	// poll-only until a fresh stream opens. Earlier versions exited, so any
	// dropped stream took the whole process down and `restart:
	// unless-stopped` bounced the container in a loop.
	//
	// Reopening is silent, and what the stream failed with is dropped: the
	// interval pass still reports any real trouble when its own Desired
	// fails, so a bare reopen is not worth a line.
	//
	// The backoff is a wait on ctx, so nothing is left registered on it
	// between reconnects - the TypeScript version leaked an abort listener
	// per reconnect on a signal that lived as long as the daemon (#10).
	for ctx.Err() == nil {
		d.source.Events(ctx, func(event source.Event) {
			if event.Description != "" {
				d.log(event.Description)
			}
			request()
		})
		if ctx.Err() != nil {
			break
		}
		// So a stream that fails to open at once - Docker still coming up -
		// does not spin.
		clock.Sleep(ctx, d.clock, d.options.ReconnectBackoff)
	}
}

// summarise is one line saying what the pass did.
//
// "Nothing to do" is worth saying out loud. It is the normal state, and in v1
// it was unreachable - every pass rewrote every rule - so seeing it is how you
// know the churn is gone.
func summarise(actions []reconcile.Action) string {
	count := func(kind reconcile.Kind) int {
		n := 0
		for _, action := range actions {
			if action.Kind == kind {
				n++
			}
		}
		return n
	}

	var changes []string
	for _, part := range []struct {
		name string
		kind reconcile.Kind
	}{
		{"added", reconcile.Add},
		{"renewed", reconcile.Renew},
		{"replaced", reconcile.Replace},
		{"removed", reconcile.Remove},
		{"in conflict", reconcile.Conflict},
	} {
		if n := count(part.kind); n > 0 {
			changes = append(changes, fmt.Sprintf("%d %s", n, part.name))
		}
	}
	if orphans := count(reconcile.Orphan); orphans > 0 {
		changes = append(changes, fmt.Sprintf("%d left alone", orphans))
	}
	correct := count(reconcile.Keep)

	if len(changes) == 0 {
		switch correct {
		case 0:
			return "No rules to manage"
		case 1:
			return "1 rule already correct, nothing to do"
		default:
			return fmt.Sprintf("%d rules already correct, nothing to do", correct)
		}
	}
	return strings.Join(append(changes, fmt.Sprintf("%d already correct", correct)), ", ")
}
