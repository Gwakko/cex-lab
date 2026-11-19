package filelog_test

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gwakko/cex-lab/internal/trading/adapters/filelog"
	"github.com/gwakko/cex-lab/internal/trading/internal/engine"
)

var spec = engine.MarketSpec{Symbol: "BTC-USDT", Tick: 5, Lot: 10}

// commands is a deterministic mix of resting orders, crossing orders and cancels.
func commands(n int) []engine.Command {
	var cmds []engine.Command
	for i := range n {
		if i%5 == 4 {
			cmds = append(cmds, engine.Command{Type: engine.Cancel, Order: engine.Order{ID: fmt.Sprintf("o%d", i-3), User: "u"}})
			continue
		}
		side := engine.Side(1 + i%2)
		cmds = append(cmds, engine.Command{Type: engine.Place, TraceID: fmt.Sprintf("t%d", i), Order: engine.Order{
			ID: fmt.Sprintf("o%d", i), User: "u", Side: side, Type: engine.Limit,
			Price: spec.Tick * int64(18+i%5), Qty: spec.Lot * int64(1+i%7),
		}})
	}
	return cmds
}

func TestRestartFromFileLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cmd.log")
	open := func() (*engine.Engine, *filelog.Log) {
		fl, err := filelog.Open(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		e, err := engine.Start(spec, fl)
		if err != nil {
			t.Fatal(err)
		}
		return e, fl
	}
	ctx := context.Background()
	e, fl := open()
	trades := 0
	for _, c := range commands(200) {
		res, err := e.Submit(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		trades += len(res.Trades)
	}
	if trades == 0 {
		t.Fatal("scenario should produce trades")
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
	cmds, err := fl.Load()
	if err != nil || len(cmds) != 200 || cmds[0].TraceID != "t0" {
		t.Fatalf("log contents: %d commands, err %v", len(cmds), err)
	}
	if fl.Err() != nil {
		t.Fatalf("healthy log reports %v", fl.Err())
	}
}
