package chain

import (
	"context"
	"errors"
	"testing"
)

func TestFakeChainLinksAndReorgs(t *testing.T) {
	ctx := context.Background()
	c := NewFakeChain()
	tr := Transfer{TxHash: "0xaa", To: "addr1", Asset: "ETH", Amount: 5}
	b1 := c.Mine(tr)
	b2 := c.Mine()
	if b2.Parent != b1.Hash {
		t.Fatal("parent link broken")
	}
	if got, _ := c.TransfersTo(ctx, b1.Hash, []string{"addr1"}); len(got) != 1 {
		t.Fatalf("transfers %v", got)
	}
	if got, _ := c.TransfersTo(ctx, b1.Hash, []string{"other"}); len(got) != 0 {
		t.Fatal("address filter ignored")
	}

	if err := c.Reorg(2, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	latest, _ := c.LatestBlock(ctx)
	h1, _ := c.HeaderByNumber(ctx, 1)
	if latest != 3 || h1.Hash == b1.Hash {
		t.Fatalf("reorg did not replace blocks: latest=%d h1=%v", latest, h1)
	}
	if _, err := c.HeaderByNumber(ctx, 4); !errors.Is(err, ErrNotFound) {
		t.Fatalf("beyond tip: %v", err)
	}
	if err := c.Reorg(10); err == nil {
		t.Fatal("reorg past genesis must fail")
	}
}
