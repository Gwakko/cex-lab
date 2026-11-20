//go:build evm

// Integration tests against a real local anvil node. Run with `make evm-test`
// (go test -tags evm). They skip when Foundry is not installed.
package evmchain_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gwakko/cex-lab/internal/app"
	"github.com/gwakko/cex-lab/internal/custody"
	"github.com/gwakko/cex-lab/internal/custody/adapters/evmchain"
	"github.com/gwakko/cex-lab/internal/custody/adapters/filecursor"
	"github.com/gwakko/cex-lab/internal/custody/adapters/memcursor"
	"github.com/gwakko/cex-lab/internal/ledger"
	"github.com/gwakko/cex-lab/internal/ledger/adapters/memstore"
	"github.com/gwakko/cex-lab/internal/platform/devnet"
)

const (
	alice    = "0x000000000000000000000000000000000000A11C"
	bob      = "0x0000000000000000000000000000000000000B0B"
	stranger = "0x000000000000000000000000000000000000dEaD"
	gwei     = 9
)

type env struct {
	t      *testing.T
	ctx    context.Context
	node   *devnet.Anvil
	ledger *ledger.Ledger
}

func start(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	node, err := devnet.Start(ctx)
	if errors.Is(err, devnet.ErrNoFoundry) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(node.Stop)
	t.Logf("%s at %s", node.Version, node.URL)
	return &env{t: t, ctx: ctx, node: node, ledger: memstore.NewLedger()}
}

func (e *env) dial(cfg evmchain.Config) *evmchain.Client {
	e.t.Helper()
	cfg.URL = e.node.URL
	c, err := evmchain.Dial(e.ctx, cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(c.Close)
	return c
}

func (e *env) watcher(c custody.ChainClient, confirmations uint64, cursor custody.CursorStore) *custody.Watcher {
	e.t.Helper()
	w, err := custody.NewWatcher(c, cursor, app.NewDepositCreditor(e.ledger, map[string]string{alice: "alice", bob: "bob"}),
		custody.Config{Confirmations: confirmations, Watched: []string{alice, bob}, StartBlock: 1})
	if err != nil {
		e.t.Fatal(err)
	}
	return w
}

func (e *env) poll(w *custody.Watcher) {
	e.t.Helper()
	if err := w.Poll(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) mine(n int) {
	e.t.Helper()
	if err := e.node.Mine(e.ctx, n); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) balance(user, asset string) int64 {
	return e.ledger.Balance(ledger.UserAccount(user, ledger.Asset(asset), ledger.Available))
}

func (e *env) want(w *custody.Watcher, pending, credited int) {
	e.t.Helper()
	if p, c := len(w.Pending()), len(w.Credited()); p != pending || c != credited {
		e.t.Fatalf("pending=%d credited=%d, want %d/%d", p, c, pending, credited)
	}
}

func (e *env) deployToken(supply string) string {
	e.t.Helper()
	code, err := os.ReadFile(filepath.Join("testdata", "TestToken.bin"))
	if err != nil {
		e.t.Fatal(err)
	}
	r, err := e.node.Deploy(e.ctx, string(code), "constructor(uint256)", supply)
	if err != nil {
		e.t.Fatal(err)
	}
	return r.ContractAddress
}

func (e *env) transferToken(token, to, amount string) devnet.Receipt {
	e.t.Helper()
	r, err := e.node.Transact(e.ctx, token, "transfer(address,uint256)", to, amount)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *env) sendETH(to, value string) devnet.Receipt {
	e.t.Helper()
	r, err := e.node.SendETH(e.ctx, to, value)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func TestNativeDepositCreditedAfterConfirmations(t *testing.T) {
	e := start(t)
	w := e.watcher(e.dial(evmchain.Config{NativeAsset: "ETH", NativeUnitExp: gwei}), 3, &memcursor.Store{})
	e.sendETH(stranger, "1ether") // not watched; mined before the deposit
	r := e.sendETH(alice, "1ether")
	if n, _ := e.node.BlockNumber(e.ctx); n != r.Block() {
		t.Fatalf("deposit in block %d, head %d", r.Block(), n)
	}

	e.poll(w)
	e.want(w, 1, 0) // 1 confirmation
	e.mine(1)
	e.poll(w)
	e.want(w, 1, 0) // 2 confirmations
	if e.balance("alice", "ETH") != 0 {
		t.Fatal("credited before confirmations")
	}
	e.mine(1)
	e.poll(w)
	e.want(w, 0, 1)

	d := w.Credited()[0]
	nodeHash, _ := e.node.BlockHash(e.ctx, r.Block())
	if d.TxHash != r.TxHash || d.LogIndex != evmchain.NativeLogIndex || d.Block != r.Block() || d.BlockHash != nodeHash || d.To != alice {
		t.Fatalf("deposit %+v, receipt %+v, node hash %s", d, r, nodeHash)
	}
	if got := e.balance("alice", "ETH"); got != 1_000_000_000 { // 1 ether in gwei
		t.Fatalf("alice ETH = %d gwei", got)
	}
}

func TestERC20DepositCreditedAfterConfirmations(t *testing.T) {
	e := start(t)
	token := e.deployToken("1000000000000000000000000000000") // 10^30 base units to dev account #0
	w := e.watcher(e.dial(evmchain.Config{Tokens: []evmchain.Token{{Address: token, Asset: "TKN", UnitExp: gwei}}}), 2, &memcursor.Store{})
	e.transferToken(token, stranger, "7")                     // not watched
	r := e.transferToken(token, alice, "5000000000000000000") // 5 * 10^18, the head block

	e.poll(w)
	e.want(w, 1, 0)
	e.mine(1)
	e.poll(w)
	e.want(w, 0, 1)
	d := w.Credited()[0]
	if d.TxHash != r.TxHash || d.LogIndex != 0 || d.Asset != "TKN" || d.Amount != 5_000_000_000 {
		t.Fatalf("deposit %+v", d)
	}
	if got := e.balance("alice", "TKN"); got != 5_000_000_000 {
		t.Fatalf("alice TKN = %d", got)
	}
}

func TestRealReorgDropsPendingDeposit(t *testing.T) {
	e := start(t)
	w := e.watcher(e.dial(evmchain.Config{NativeAsset: "ETH", NativeUnitExp: gwei}), 3, &memcursor.Store{})
	r := e.sendETH(bob, "1ether")
	e.poll(w)
	e.want(w, 1, 0)
	before, _ := e.node.BlockHash(e.ctx, r.Block())

	if err := e.node.Reorg(e.ctx, 1); err != nil { // anvil_reorg: replace the deposit's block
		t.Fatal(err)
	}
	after, _ := e.node.BlockHash(e.ctx, r.Block())
	if before == after {
		t.Fatal("anvil_reorg did not change the block hash")
	}
	e.poll(w)
	e.want(w, 0, 0)
	e.mine(3)
	e.poll(w)
	e.want(w, 0, 0)
	if e.balance("bob", "ETH") != 0 {
		t.Fatal("reorged deposit was credited")
	}
	if err := e.ledger.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotRevertDropsPendingDeposit(t *testing.T) {
	e := start(t)
	w := e.watcher(e.dial(evmchain.Config{NativeAsset: "ETH", NativeUnitExp: gwei}), 3, &memcursor.Store{})
	e.mine(1)
	e.poll(w)
	var snap string
	if err := e.node.Call(e.ctx, &snap, "evm_snapshot"); err != nil {
		t.Fatal(err)
	}
	e.sendETH(bob, "2ether")
	e.poll(w)
	e.want(w, 1, 0)
	var ok bool
	if err := e.node.Call(e.ctx, &ok, "evm_revert", snap); err != nil || !ok {
		t.Fatalf("evm_revert: %v %v", ok, err)
	}
	e.mine(2) // a longer chain without the deposit
	e.poll(w)
	e.want(w, 0, 0)
	if e.balance("bob", "ETH") != 0 {
		t.Fatal("reverted deposit was credited")
	}
}

func TestReorgDeeperThanConfirmationsRaisesAlert(t *testing.T) {
	e := start(t)
	w := e.watcher(e.dial(evmchain.Config{NativeAsset: "ETH", NativeUnitExp: gwei}), 2, &memcursor.Store{})
	e.sendETH(alice, "1ether") // block 1
	e.mine(1)                  // block 2: two confirmations
	e.poll(w)
	e.want(w, 0, 1)

	if err := e.node.Reorg(e.ctx, 2); err != nil { // replaces blocks 1 and 2
		t.Fatal(err)
	}
	err := w.Poll(e.ctx)
	var alert *custody.ReorgAlert
	if !errors.Is(err, custody.ErrConfirmedDepositReorged) || !errors.As(err, &alert) || alert.Ancestor != 0 || len(alert.Deposits) != 1 {
		t.Fatalf("expected reorg alert, got %v", err)
	}
	if err := w.Poll(e.ctx); !errors.Is(err, custody.ErrConfirmedDepositReorged) {
		t.Fatalf("watcher resumed after alert: %v", err)
	}
	if got := e.balance("alice", "ETH"); got != 1_000_000_000 {
		t.Fatalf("balance silently changed to %d", got)
	}
	if err := e.ledger.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestDuplicateDeliveryAndResumeFromCursor(t *testing.T) {
	e := start(t)
	client := e.dial(evmchain.Config{NativeAsset: "ETH", NativeUnitExp: gwei})
	cursor := filecursor.Store{Path: filepath.Join(t.TempDir(), "cursor.json")}
	w := e.watcher(client, 3, cursor)
	e.sendETH(alice, "2ether")
	e.poll(w)
	e.want(w, 1, 0)

	w = e.watcher(client, 3, cursor) // restart from the persisted cursor
	e.want(w, 1, 0)
	e.mine(2)
	e.poll(w)
	e.poll(w) // polling again delivers nothing new
	e.want(w, 0, 1)

	// Crash after crediting but before saving: restart from a stale cursor.
	stale := &memcursor.Store{}
	ws := e.watcher(client, 3, stale)
	e.poll(ws) // credits again through the ledger, which deduplicates by chain:txhash:logindex
	e.want(ws, 0, 1)
	if got := e.balance("alice", "ETH"); got != 2_000_000_000 {
		t.Fatalf("alice ETH = %d gwei after duplicate delivery", got)
	}
	if n := len(e.ledger.EventsSince(0)); n != 1 {
		t.Fatalf("ledger has %d transactions, want 1", n)
	}
}

func TestAmountsThatDoNotFitStopTheWatcherLoudly(t *testing.T) {
	t.Run("native overflow in wei", func(t *testing.T) {
		e := start(t)
		e.sendETH(alice, "10ether") // 10^19 wei > max int64
		e.mine(1)
		inWei := e.watcher(e.dial(evmchain.Config{NativeAsset: "ETH", NativeUnitExp: 0}), 1, &memcursor.Store{})
		for range 2 { // the cursor does not move past the bad block
			if err := inWei.Poll(e.ctx); !errors.Is(err, evmchain.ErrAmountOverflow) {
				t.Fatalf("got %v, want overflow", err)
			}
		}
		if e.balance("alice", "ETH") != 0 || len(inWei.Credited()) != 0 {
			t.Fatal("overflowing deposit was credited")
		}
		// The same deposit fits when the ledger unit is gwei.
		inGwei := e.watcher(e.dial(evmchain.Config{NativeAsset: "ETH", NativeUnitExp: gwei}), 1, &memcursor.Store{})
		e.poll(inGwei)
		if got := e.balance("alice", "ETH"); got != 10_000_000_000 {
			t.Fatalf("alice ETH = %d gwei", got)
		}
	})
	t.Run("token overflow", func(t *testing.T) {
		e := start(t)
		token := e.deployToken("100000000000000000000000000000000")       // 10^32
		e.transferToken(token, alice, "10000000000000000000000000000000") // 10^31 base units = 10^22 gwei-units
		w := e.watcher(e.dial(evmchain.Config{Tokens: []evmchain.Token{{Address: token, Asset: "TKN", UnitExp: gwei}}}), 1, &memcursor.Store{})
		if err := w.Poll(e.ctx); !errors.Is(err, evmchain.ErrAmountOverflow) {
			t.Fatalf("got %v, want overflow", err)
		}
		if e.balance("alice", "TKN") != 0 {
			t.Fatal("overflowing token deposit was credited")
		}
	})
	t.Run("token precision", func(t *testing.T) {
		e := start(t)
		token := e.deployToken("1000")
		e.transferToken(token, alice, "1") // 1 base unit is not a whole gwei-unit
		w := e.watcher(e.dial(evmchain.Config{Tokens: []evmchain.Token{{Address: token, Asset: "TKN", UnitExp: gwei}}}), 1, &memcursor.Store{})
		if err := w.Poll(e.ctx); !errors.Is(err, evmchain.ErrAmountPrecision) {
			t.Fatalf("got %v, want precision error", err)
		}
	})
}
