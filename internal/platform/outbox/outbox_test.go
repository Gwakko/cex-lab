package outbox

import (
	"errors"
	"fmt"
	"testing"

	"github.com/gwakko/cex-lab/internal/events"
)

// log is a fake Source holding n events.
type log []events.Event

func (l log) EventsSince(offset int) []events.Event {
	if offset >= len(l) {
		return nil
	}
	return append([]events.Event(nil), l[offset:]...)
}

func seededLog(n int) log {
	var l log
	for i := range n {
		l = append(l, events.Event{ID: fmt.Sprintf("evt-%d", i+1), Source: "test", Type: "deposit", Version: events.SchemaV1, Seq: uint64(i + 1)})
	}
	return l
}

func TestRelayRedeliversAfterFailureAndInboxDedups(t *testing.T) {
	l := seededLog(5)
	handled := map[string]int{}
	inbox := NewInbox(func(e events.Event) error { handled[e.ID]++; return nil })

	calls := 0
	flaky := func(e events.Event) error {
		calls++
		if calls == 3 {
			return errors.New("broker unavailable")
		}
		return inbox.Handle(e)
	}
	r := NewRelay(l, flaky, 0)
	if n, err := r.Drain(); err == nil || n != 2 {
		t.Fatalf("first drain: n=%d err=%v", n, err)
	}
	if n, err := r.Drain(); err != nil || n != 3 {
		t.Fatalf("second drain: n=%d err=%v", n, err)
	}

	// Simulate a crash that lost the checkpoint: everything is redelivered.
	r2 := NewRelay(l, inbox.Handle, 0)
	if n, err := r2.Drain(); err != nil || n != 5 {
		t.Fatalf("redelivery: n=%d err=%v", n, err)
	}
	if len(handled) != 5 {
		t.Fatalf("handled %d distinct events, want 5", len(handled))
	}
	for id, c := range handled {
		if c != 1 {
			t.Fatalf("event %s handled %d times", id, c)
		}
	}
}

func TestInboxRetriesFailedHandler(t *testing.T) {
	fail := true
	count := 0
	in := NewInbox(func(events.Event) error {
		if fail {
			return errors.New("transient")
		}
		count++
		return nil
	})
	e := events.Event{ID: "evt-1"}
	if err := in.Handle(e); err == nil {
		t.Fatal("expected error")
	}
	fail = false
	in.Handle(e)
	in.Handle(e)
	if count != 1 {
		t.Fatalf("handled %d times, want 1", count)
	}
}
