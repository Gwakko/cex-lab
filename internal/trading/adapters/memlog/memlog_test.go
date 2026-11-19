package memlog_test

import (
	"testing"

	"github.com/gwakko/cex-lab/internal/trading"
	"github.com/gwakko/cex-lab/internal/trading/adapters/memlog"
)

func TestLoadReturnsCopyInOrder(t *testing.T) {
	l := &memlog.Log{}
	for i := range 3 {
		l.Append(trading.Command{Seq: uint64(i + 1)})
	}
	got, _ := l.Load()
	got[0].Seq = 99
	again, _ := l.Load()
	if len(again) != 3 || again[0].Seq != 1 || again[2].Seq != 3 {
		t.Fatalf("log %+v", again)
	}
}
