package engine

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

// randomCommands generates a mix of limit/market places and cancels.
func randomCommands(r *rand.Rand, n int) []Command {
	var cmds []Command
	var ids []string
	users := []string{"u1", "u2", "u3"}
	for i := range n {
		if len(ids) > 0 && r.IntN(4) == 0 {
			id := ids[r.IntN(len(ids))]
			cmds = append(cmds, Command{Type: Cancel, Order: Order{ID: id, User: users[r.IntN(len(users))]}})
			continue
		}
		o := Order{
			ID:   fmt.Sprintf("o%d", i),
			User: users[r.IntN(len(users))],
			Side: Side(1 + r.IntN(2)),
			Type: Limit,
			Qty:  spec.Lot * int64(1+r.IntN(10)),
		}
		if r.IntN(6) == 0 {
			o.Type = Market
			if o.Side == Buy {
				o.QuoteBudget = int64(1 + r.IntN(5000))
			}
		} else {
			o.Price = spec.Tick * int64(15+r.IntN(10))
		}
		ids = append(ids, o.ID)
		cmds = append(cmds, Command{Type: Place, Order: o})
	}
	return cmds
}

func TestReplayIsDeterministic(t *testing.T) {
	for seed := range uint64(20) {
		r := rand.New(rand.NewPCG(seed, 1))
		log := &MemLog{}
		e, err := Start(spec, log)
		if err != nil {
			t.Fatal(err)
		}
		var live []Result
		for _, c := range randomCommands(r, 500) {
			res, err := e.Submit(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			live = append(live, res)
			snap, _ := e.Snapshot(context.Background())
			if snap.Crossed() {
				t.Fatalf("seed %d: crossed book at seq %d", seed, res.Seq)
			}
		}
		liveSnap, _ := e.Snapshot(context.Background())
		e.Stop()

		cmds, _ := log.Load()
		book, replayed, err := Replay(spec, cmds)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(book.Snapshot(), liveSnap) {
			t.Fatalf("seed %d: replayed book differs from live book", seed)
		}
		for i := range live {
			if !reflect.DeepEqual(live[i].Trades, replayed[i].Trades) || live[i].Order != replayed[i].Order {
				t.Fatalf("seed %d: result %d differs on replay", seed, i)
			}
		}
	}
}

func TestSequenceIsMonotonicAndLoggedFirst(t *testing.T) {
	log := &MemLog{}
	e, _ := Start(spec, log)
	defer e.Stop()
	for i := range 5 {
		res, _ := e.Submit(context.Background(), Command{Type: Place, Order: limit(fmt.Sprint(i), "u", Buy, 100, 10)})
		if res.Seq != uint64(i+1) {
			t.Fatalf("seq %d, want %d", res.Seq, i+1)
		}
	}
	// A rejected command still consumes a sequence number: it is in the log.
	res, _ := e.Submit(context.Background(), Command{Type: Place, Order: limit("bad", "u", Buy, 101, 10)})
	if !errors.Is(res.Err, ErrInvalidOrder) || res.Seq != 6 {
		t.Fatalf("rejected command: %+v", res)
	}
	cmds, _ := log.Load()
	if len(cmds) != 6 || e.LastSeq() != 6 {
		t.Fatalf("log has %d commands, last seq %d", len(cmds), e.LastSeq())
	}
}

func TestRestartFromFileLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cmd.log")
	open := func() (*Engine, *FileLog) {
		fl, err := OpenFileLog(path)
		if err != nil {
			t.Fatal(err)
		}
		e, err := Start(spec, fl)
		if err != nil {
			t.Fatal(err)
		}
		return e, fl
	}
	ctx := context.Background()
	e, fl := open()
	for _, c := range randomCommands(rand.New(rand.NewPCG(7, 7)), 200) {
		if _, err := e.Submit(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := e.Snapshot(ctx)
	e.Stop()
	fl.Close()

	e, fl = open()
	defer fl.Close()
	defer e.Stop()
	after, _ := e.Snapshot(ctx)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("book after restart differs")
	}
	if e.LastSeq() != before.Seq {
		t.Fatalf("last seq %d, want %d", e.LastSeq(), before.Seq)
	}
}

type failingLog struct{ MemLog }

func (*failingLog) Append(Command) error { return errors.New("disk full") }

func TestLogFailureDoesNotApply(t *testing.T) {
	e, _ := Start(spec, &failingLog{})
	defer e.Stop()
	res, err := e.Submit(context.Background(), Command{Type: Place, Order: limit("a", "u", Buy, 100, 10)})
	if err != nil || res.Err == nil {
		t.Fatalf("expected log error in result, got %+v %v", res, err)
	}
	snap, _ := e.Snapshot(context.Background())
	if len(snap.Bids) != 0 || snap.Seq != 0 {
		t.Fatalf("command applied without being logged: %+v", snap)
	}
}

func TestConcurrentSubmitters(t *testing.T) {
	log := &MemLog{}
	e, _ := Start(spec, log)
	ctx := context.Background()
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(w), 99))
			for i, c := range randomCommands(r, 200) {
				if c.Type == Place {
					c.Order.ID = fmt.Sprintf("w%d-%d", w, i)
				}
				if _, err := e.Submit(ctx, c); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	live, _ := e.Snapshot(ctx)
	e.Stop()
	if _, err := e.Submit(ctx, Command{Type: Place}); !errors.Is(err, ErrStopped) {
		t.Fatalf("submit after stop: %v", err)
	}
	cmds, _ := log.Load()
	book, _, err := Replay(spec, cmds)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(book.Snapshot(), live) {
		t.Fatal("replay after concurrent submission differs")
	}
}

func TestReplayRejectsSequenceGapsAndDuplicates(t *testing.T) {
	o := limit("a", "u", Buy, 100, 10)
	for name, cmds := range map[string][]Command{
		"gap":       {{Seq: 1, Type: Place, Order: o}, {Seq: 3, Type: Cancel, Order: o}},
		"duplicate": {{Seq: 1, Type: Place, Order: o}, {Seq: 1, Type: Cancel, Order: o}},
	} {
		if _, _, err := Replay(spec, cmds); !errors.Is(err, ErrOutOfOrder) {
			t.Fatalf("%s: got %v", name, err)
		}
	}
}
