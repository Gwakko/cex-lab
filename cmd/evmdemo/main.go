// Command evmdemo runs the custody watcher against a real local anvil node:
// it starts a fresh anvil (or attaches to CEXLAB_EVM_RPC_URL on loopback), deploys a test token, sends ETH and token deposits,
// forces a real reorg with anvil_reorg, mines confirmations and prints what
// the watcher credited into the ledger, with trace ids and metrics. It needs
// Foundry (https://www.getfoundry.sh/) and touches no real network.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"maps"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/gwakko/cex-lab/internal/app"
	"github.com/gwakko/cex-lab/internal/custody"
	"github.com/gwakko/cex-lab/internal/custody/adapters/evmchain"
	"github.com/gwakko/cex-lab/internal/custody/adapters/memcursor"
	"github.com/gwakko/cex-lab/internal/ledger"
	"github.com/gwakko/cex-lab/internal/platform/config"
	"github.com/gwakko/cex-lab/internal/platform/devnet"
)

const gwei = 9

// defaultWatched is used when CEXLAB_WATCHED_ADDRESSES is not set.
var defaultWatched = map[string]string{
	"0x000000000000000000000000000000000000A11C": "alice",
	"0x0000000000000000000000000000000000000B0B": "bob",
}

func main() {
	tokenBin := flag.String("token-bin", "internal/custody/adapters/evmchain/testdata/TestToken.bin", "test token bytecode")
	flag.Parse()
	ctx := context.Background()

	src, err := config.Load(".", os.Environ())
	must(err)
	settings, err := app.LoadSettings(src.Lookup)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	for _, k := range src.Keys(app.Prefix) {
		fmt.Printf("config: %s from %s\n", k, src.Origin(k)) // key and source only, never the value
	}
	owners := settings.WatchedAddresses
	if len(owners) == 0 {
		owners = defaultWatched
	}
	alice, bob := addressOf(owners, "alice"), addressOf(owners, "bob")
	confirmations := settings.Confirmations

	node := startNode(ctx, settings.EVMRPCURL)
	defer node.Stop()

	var logs bytes.Buffer
	a, err := app.New(app.Config{Logger: settings.Logger(&logs)})
	must(err)
	defer a.Close()

	code, err := os.ReadFile(*tokenBin)
	must(err)
	deploy, err := node.Deploy(ctx, string(code), "constructor(uint256)", "1000000000000000000000000") // 10^24
	must(err)
	fmt.Printf("deployed TestToken at %s (block %d)\n", deploy.ContractAddress, deploy.Block())

	w, closeClient, err := a.NewEVMDepositWatcher(ctx,
		evmchain.Config{URL: node.URL, NativeAsset: "ETH", NativeUnitExp: gwei,
			Tokens: []evmchain.Token{{Address: deploy.ContractAddress, Asset: "TKN", UnitExp: gwei}}},
		&memcursor.Store{}, owners,
		custody.Config{Confirmations: confirmations, Watched: slices.Sorted(maps.Keys(owners)), StartBlock: 1})
	must(err)
	defer closeClient()
	poll := func(step string) {
		must(w.Poll(ctx))
		head, _ := node.BlockNumber(ctx)
		fmt.Printf("%-44s head=%d pending=%d credited=%d\n", step, head, len(w.Pending()), len(w.Credited()))
	}

	fmt.Printf("\n== deposits (confirmations required: %d)\n", confirmations)
	r, err := node.SendETH(ctx, alice, "1.5ether")
	must(err)
	poll(fmt.Sprintf("alice +1.5 ETH in block %d", r.Block()))
	r, err = node.Transact(ctx, deploy.ContractAddress, "transfer(address,uint256)", alice, "250000000000000000000")
	must(err)
	poll(fmt.Sprintf("alice +250 TKN in block %d", r.Block()))
	r, err = node.SendETH(ctx, bob, "2ether")
	must(err)
	poll(fmt.Sprintf("bob +2 ETH in block %d", r.Block()))

	before, _ := node.BlockHash(ctx, r.Block())
	must(node.Reorg(ctx, 1))
	after, _ := node.BlockHash(ctx, r.Block())
	fmt.Printf("anvil_reorg depth 1: block %d hash %s… -> %s…\n", r.Block(), before[:10], after[:10])
	poll("after reorg (bob's deposit is gone)")

	must(node.Mine(ctx, int(confirmations)))
	poll(fmt.Sprintf("after mining %d blocks", confirmations))

	fmt.Println("\n== ledger balances (ETH and TKN in gwei-units)")
	for _, acc := range []ledger.Account{
		ledger.UserAccount("alice", "ETH", ledger.Available),
		ledger.UserAccount("alice", "TKN", ledger.Available),
		ledger.UserAccount("bob", "ETH", ledger.Available),
	} {
		fmt.Printf("  %-24s %d\n", acc, a.Ledger.Balance(acc))
	}
	must(a.Ledger.Verify())

	fmt.Println("\n== credit logs (watcher -> ledger, same trace_id)")
	if settings.LogFormat != "json" {
		fmt.Printf("  (shown only with %s=json)\n", app.KeyLogFormat)
	}
	printCredits(&logs)

	fmt.Println("\n== metrics")
	var page strings.Builder
	must(a.Registry.WriteText(&page))
	sc := bufio.NewScanner(strings.NewReader(page.String()))
	for sc.Scan() {
		line := sc.Text()
		for _, p := range []string{"chain_deposits_credited_total", "chain_deposits_pending", "chain_watcher_lag_blocks", "chain_head_block",
			"chain_confirmed_block", "chain_reorgs_total", "chain_reorg_depth_blocks_count", "chain_reorg_depth_blocks_sum",
			"chain_reorg_below_confirmation_threshold_total"} {
			if strings.HasPrefix(line, p+" ") || strings.HasPrefix(line, p+"{") {
				fmt.Println("  " + line)
			}
		}
	}
}

// startNode starts a fresh anvil, or attaches to a loopback dev node. The demo
// sends transactions with anvil's public dev key, so it refuses other hosts.
func startNode(ctx context.Context, rpcURL string) *devnet.Anvil {
	if rpcURL == "" {
		node, err := devnet.Start(ctx)
		must(err)
		fmt.Printf("%s at %s (fresh local devnet)\n", node.Version, node.URL)
		return node
	}
	u, err := url.Parse(rpcURL)
	must(err)
	if ip := net.ParseIP(u.Hostname()); u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		log.Fatalf("%s must point to a local dev node (loopback); refusing to send devnet transactions elsewhere", app.KeyEVMRPCURL)
	}
	node, err := devnet.Attach(ctx, rpcURL)
	must(err)
	fmt.Printf("%s via %s (attached local devnet)\n", node.Version, app.KeyEVMRPCURL)
	return node
}

func addressOf(owners map[string]string, user string) string {
	for addr, u := range owners {
		if u == user {
			return addr
		}
	}
	log.Fatalf("%s must assign an address to user %q", app.KeyWatchedAddresses, user)
	return ""
}

func printCredits(logs *bytes.Buffer) {
	var lines []map[string]any
	var raw []string
	sc := bufio.NewScanner(bytes.NewReader(logs.Bytes()))
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			lines, raw = append(lines, m), append(raw, sc.Text())
		}
	}
	for i, m := range lines {
		if m["msg"] != "deposit credited" {
			continue
		}
		fmt.Println("  " + raw[i])
		for j, n := range lines {
			if n["msg"] == "ledger tx committed" && n["trace_id"] == m["trace_id"] {
				fmt.Println("  " + raw[j])
			}
		}
	}
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
