package dockerrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"

	"golaunch/internal/domain/entities"
)

const (
	eventChanBuffer = 64
	eventRetryDelay = 2 * time.Second
)

// Events subscribes to container lifecycle events for containers this
// platform manages. Each call gets its own stream and its own goroutine; the
// channel closes when ctx is done.
//
// This is where RuntimeEventOOM becomes real — nothing in the host-exec
// runtime could observe memory pressure, so the event type existed but was
// never emitted.
func (r *DockerRuntime) Events(ctx context.Context) (<-chan entities.RuntimeEvent, error) {
	out := make(chan entities.RuntimeEvent, eventChanBuffer)
	go r.pumpEvents(ctx, out)
	return out, nil
}

// pumpEvents keeps a subscription alive across daemon restarts. Reconnecting
// from the last seen timestamp is what stops a death event from being lost in
// the gap — losing one would leave a project marked running forever.
func (r *DockerRuntime) pumpEvents(ctx context.Context, out chan<- entities.RuntimeEvent) {
	defer close(out)

	// empty means "from now on"; only a reconnect needs a real bookmark
	since := ""

	for {
		since = r.streamEvents(ctx, out, since)

		select {
		case <-ctx.Done():
			return
		case <-time.After(eventRetryDelay):
		}
	}
}

// streamEvents drains one subscription and returns the bookmark to resume from.
func (r *DockerRuntime) streamEvents(ctx context.Context, out chan<- entities.RuntimeEvent, since string) string {
	messages, errs := r.cli.Events(ctx, events.ListOptions{
		Since:   since,
		Filters: eventFilters(),
	})

	for {
		select {
		case <-ctx.Done():
			return since

		case err := <-errs:
			if err != nil && !errors.Is(err, io.EOF) && ctx.Err() == nil {
				log.Printf("[dockerrun] event stream ended: %v", err)
			}
			return since

		case msg := <-messages:
			if msg.TimeNano > 0 {
				since = eventBookmark(msg.TimeNano)
			}

			evt, ok := translateEvent(msg)
			if !ok {
				continue
			}
			select {
			case out <- evt:
			case <-ctx.Done():
				return since
			}
		}
	}
}

// eventBookmark renders a resume point the way the daemon parses "since":
// seconds, then fractional nanoseconds. A bare nanosecond count is read as
// seconds, which pushes the cutoff tens of billions of years out and makes
// the resumed stream deliver nothing at all.
//
// The +1ns stops the message this bookmark came from being replayed.
func eventBookmark(nano int64) string {
	nano++
	return fmt.Sprintf("%d.%09d", nano/int64(time.Second), nano%int64(time.Second))
}

func eventFilters() filters.Args {
	args := filters.NewArgs(
		filters.Arg("type", string(events.ContainerEventType)),
		filters.Arg("label", labelManaged+"="+managedValue),
	)
	args.Add("event", string(events.ActionStart))
	args.Add("event", string(events.ActionDie))
	args.Add("event", string(events.ActionOOM))
	return args
}

// translateEvent maps a daemon message onto the runtime-agnostic event.
// Actor.Attributes carries the container's labels, which is how the consumer
// resolves an event back to a deployment without a lookup.
func translateEvent(msg events.Message) (entities.RuntimeEvent, bool) {
	var kind entities.RuntimeEventType

	switch msg.Action {
	case events.ActionStart:
		kind = entities.RuntimeEventStarted
	case events.ActionDie:
		kind = entities.RuntimeEventDied
	case events.ActionOOM:
		kind = entities.RuntimeEventOOM
	default:
		return entities.RuntimeEvent{}, false
	}

	return entities.RuntimeEvent{
		Type:   kind,
		Handle: entities.RuntimeHandle(msg.Actor.ID),
		Labels: decodeLabels(msg.Actor.Attributes),
	}, true
}
