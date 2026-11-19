package outbox

import (
	"errors"
	"fmt"
	"testing"

	"github.com/gwakko/cex-lab/internal/events"
	"github.com/gwakko/cex-lab/ledger"
)

func seededLedger(t *testing.T, n int) *ledger.Ledger {
	t.Helper()
	l := ledger.New()
	for i := range n {
		if _, _, err := l.Apply(ledger.Deposit{Key: fmt.Sprintf("d%d", i), User: "alice", Asset: "BTC", Amount: 1}); err != nil {
			t.Fatal(err)
		}
	}
	return l
}

func TestEveryCommitEmitsOneEvent(t *testing.T) {
	l := seededLedger(t, 3)
	if _, replayed, _ := l.Apply(ledger.Deposit{Key: "d0", User: "alice", Asset: "BTC", Amount: 1}); !replayed {
		t.Fatal("expected replay")
	}
	evs := l.EventsSince(0)
	if len(evs) != 3 {
		t.Fatalf("idempotent retry must not emit a new event, got %d events", len(evs))
	}
	if evs[2].ID != "evt-3" || evs[2].Type != "deposit" || evs[2].Key != "d2" || evs[2].Source != "ledger" || evs[2].Version != events.SchemaV1 {
		t.Fatalf("unexpected event %+v", evs[2])
	}
}

func TestRelayRedeliversAfterFailureAndInboxDedups(t *testing.T) {
	l := seededLedger(t, 5)
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

func TestEventsSurviveJournalReplay(t *testing.T) {
	store := &ledger.MemStore{}
	l, _ := ledger.Open(store)
	l.Apply(ledger.Deposit{Key: "d1", User: "a", Asset: "BTC", Amount: 1})
	l2, err := ledger.Open(store)
	if err != nil {
		t.Fatal(err)
	}
	if evs := l2.EventsSince(0); len(evs) != 1 || evs[0].ID != "evt-1" {
		t.Fatalf("events after replay: %+v", evs)
	}
}
