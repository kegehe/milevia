package cloud

import (
	"testing"
)

// A notification is the only thing that wakes an idle stream, so a dropped wake
// up delays every phone watching that computer. Verify the fan-out reaches every
// subscriber of the instance, not just the most recent one.
func TestEventBrokerFansOutToEverySubscriberOfTheInstance(t *testing.T) {
	broker := newEventBroker()
	first := broker.subscribe("cmd-1")
	second := broker.subscribe("cmd-1")
	defer broker.unsubscribe(first)
	defer broker.unsubscribe(second)

	broker.publish("cmd-1")

	for name, ch := range map[string]chan string{"first": first, "second": second} {
		select {
		case got := <-ch:
			if got != "cmd-1" {
				t.Fatalf("%s subscriber got %q, want cmd-1", name, got)
			}
		default:
			t.Fatalf("%s subscriber was not woken", name)
		}
	}
}

// Filtering by instance at the broker is what makes it safe for a stream to
// collapse its own backlog after a drain: only its own wake-ups are ever queued,
// so dropping the extras can never swallow a notification meant for another
// computer.
func TestEventBrokerDoesNotWakeOtherInstances(t *testing.T) {
	broker := newEventBroker()
	mine := broker.subscribe("cmd-1")
	defer broker.unsubscribe(mine)
	theirs := broker.subscribe("cmd-2")
	defer broker.unsubscribe(theirs)

	broker.publish("cmd-1")

	select {
	case <-mine:
	default:
		t.Fatal("subscriber for cmd-1 was not woken")
	}
	select {
	case got := <-theirs:
		t.Fatalf("subscriber for cmd-2 was woken with %q", got)
	default:
	}
}

// The publisher runs on the single LISTEN connection shared by every stream. A
// stream whose subscriber channel is full must never block the publisher and
// stall the other phones; it is expected to lose that wake-up and be rescued by
// its own fallback poll.
func TestEventBrokerDoesNotBlockOnSlowSubscriber(t *testing.T) {
	broker := newEventBroker()
	slow := broker.subscribe("cmd-1")
	defer broker.unsubscribe(slow)
	healthy := broker.subscribe("cmd-1")
	defer broker.unsubscribe(healthy)

	for i := 0; i < cap(slow)+50; i++ {
		broker.publish("cmd-1")
	}

	select {
	case <-healthy:
	default:
		t.Fatal("healthy subscriber was starved by a full one")
	}
}

func TestEventBrokerUnsubscribeIsIdempotent(t *testing.T) {
	broker := newEventBroker()
	ch := broker.subscribe("cmd-1")

	broker.unsubscribe(ch)
	// A stream may unregister twice — once from its defer and once on the
	// request context finishing. The second call must be a no-op rather than a
	// double close, which would panic the whole server.
	broker.unsubscribe(ch)

	broker.publish("cmd-1")

	if _, open := <-ch; open {
		t.Fatal("unsubscribed channel should be closed")
	}
}

func TestEventBrokerPublishAfterUnsubscribeKeepsServingRemainingSubscribers(t *testing.T) {
	broker := newEventBroker()
	leaving := broker.subscribe("cmd-2")
	staying := broker.subscribe("cmd-2")

	broker.unsubscribe(leaving)
	broker.publish("cmd-2")

	select {
	case got := <-staying:
		if got != "cmd-2" {
			t.Fatalf("got %q, want cmd-2", got)
		}
	default:
		t.Fatal("remaining subscriber was not woken after a peer unsubscribed")
	}
	broker.unsubscribe(staying)
}
