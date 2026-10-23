package docker

import (
	"context"

	"github.com/danielbodart/portical/internal/source"
)

// Source is containers as a source.Source: the forwards their labels ask for.
//
// The client stays the narrower thing. It is also what the relay uses to run
// a container in another's network, which has nothing to do with deciding
// what to forward, so the Source wraps it rather than the client growing into
// one.
type Source struct {
	client  *Client
	options Options
}

// NewSource is the forwards that client's labelled containers ask for.
func NewSource(client *Client, options Options) *Source {
	return &Source{client: client, options: options.withDefaults()}
}

// Desired is every labelled container's forwards, or why Docker could not be
// asked.
func (s *Source) Desired(ctx context.Context) (source.Desired, error) {
	containers, err := s.client.Containers(ctx, s.options.Label)
	if err != nil {
		return source.Desired{}, err
	}
	return Resolve(containers, s.options), nil
}

// Events is one event per labelled container that changes, until ctx is
// done or Docker drops the stream.
func (s *Source) Events(ctx context.Context, notify func(source.Event)) error {
	return s.client.Events(ctx, s.options.Label, func(event Event) {
		notify(source.Event{Description: "Container " + event.Container + " " + event.Action})
	})
}
