// Package devnet runs a throwaway local anvil node for tests and demos. It
// uses only the standard library: anvil and cast run as subprocesses, and
// anvil-specific RPCs (mine, reorg, snapshot) go over plain JSON-RPC.
//
// It signs nothing itself: transactions are sent with cast using anvil's
// public, well-known dev account #0. Never point it at a real network.
package devnet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Anvil's default dev account #0. Anvil prints this key on startup; it is
// public and only valid on local devnets.
const (
	DevKey0  = "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	DevAddr0 = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
)

// ErrNoFoundry means anvil or cast was not found.
var ErrNoFoundry = errors.New("devnet: Foundry (anvil, cast) not found; install from https://www.getfoundry.sh/ or set FOUNDRY_BIN")

// FoundryBin locates a Foundry binary: $FOUNDRY_BIN/<name>, then PATH, then
// ~/.foundry/bin/<name>.
func FoundryBin(name string) (string, error) {
	if dir := os.Getenv("FOUNDRY_BIN"); dir != "" {
		if p := filepath.Join(dir, name); isExec(p) {
			return p, nil
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		if p := filepath.Join(home, ".foundry", "bin", name); isExec(p) {
			return p, nil
		}
	}
	return "", ErrNoFoundry
}

func isExec(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && st.Mode()&0o111 != 0
}

// Anvil is a running node with fresh state.
type Anvil struct {
	URL     string
	Version string
	cast    string
	cmd     *exec.Cmd
	exited  chan struct{}
}

// Start launches anvil on a free local port and waits until it answers.
func Start(ctx context.Context) (*Anvil, error) {
	anvilBin, err := FoundryBin("anvil")
	if err != nil {
		return nil, err
	}
	castBin, err := FoundryBin("cast")
	if err != nil {
		return nil, err
	}
	version, err := exec.CommandContext(ctx, anvilBin, "--version").Output()
	if err != nil {
		return nil, fmt.Errorf("devnet: anvil --version: %w", err)
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(anvilBin, "--host", "127.0.0.1", "--port", strconv.Itoa(port), "--silent")
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("devnet: start anvil: %w", err)
	}
	a := &Anvil{
		URL:     fmt.Sprintf("http://127.0.0.1:%d", port),
		Version: strings.SplitN(strings.TrimSpace(string(version)), "\n", 2)[0],
		cast:    castBin,
		cmd:     cmd,
		exited:  make(chan struct{}),
	}
	go func() { cmd.Wait(); close(a.exited) }()
	if err := a.waitReady(ctx); err != nil {
		a.Stop()
		return nil, err
	}
	return a, nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func (a *Anvil) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := a.BlockNumber(ctx); err == nil {
			return nil
		}
		select {
		case <-a.exited:
			return errors.New("devnet: anvil exited during startup")
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return errors.New("devnet: anvil did not become ready")
		}
	}
}

// Stop kills the node and waits for it to exit.
func (a *Anvil) Stop() {
	a.cmd.Process.Kill()
	<-a.exited
}

// Call performs a JSON-RPC call and decodes the result into out (may be nil).
func (a *Anvil) Call(ctx context.Context, out any, method string, params ...any) error {
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("devnet: %s: %w", method, err)
	}
	if r.Error != nil {
		return fmt.Errorf("devnet: %s: %s", method, r.Error.Message)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(r.Result, out)
}

// BlockNumber returns the latest block number.
func (a *Anvil) BlockNumber(ctx context.Context) (uint64, error) {
	var hex string
	if err := a.Call(ctx, &hex, "eth_blockNumber"); err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimPrefix(hex, "0x"), 16, 64)
}

// BlockHash returns the canonical hash of block n.
func (a *Anvil) BlockHash(ctx context.Context, n uint64) (string, error) {
	var b *struct {
		Hash string `json:"hash"`
	}
	if err := a.Call(ctx, &b, "eth_getBlockByNumber", "0x"+strconv.FormatUint(n, 16), false); err != nil {
		return "", err
	}
	if b == nil {
		return "", fmt.Errorf("devnet: no block %d", n)
	}
	return b.Hash, nil
}

// Mine mines n empty blocks (anvil_mine).
func (a *Anvil) Mine(ctx context.Context, n int) error {
	return a.Call(ctx, nil, "anvil_mine", "0x"+strconv.FormatInt(int64(n), 16))
}

// Reorg replaces the top depth blocks with new empty blocks (anvil_reorg),
// keeping the chain height. Transactions in the replaced blocks are dropped.
func (a *Anvil) Reorg(ctx context.Context, depth int) error {
	return a.Call(ctx, nil, "anvil_reorg", depth, []any{})
}

// Receipt is the part of a cast receipt the tests use.
type Receipt struct {
	TxHash          string `json:"transactionHash"`
	BlockNumber     string `json:"blockNumber"`
	BlockHash       string `json:"blockHash"`
	Status          string `json:"status"`
	ContractAddress string `json:"contractAddress"`
}

// Block returns the receipt's block number.
func (r Receipt) Block() uint64 {
	n, _ := strconv.ParseUint(strings.TrimPrefix(r.BlockNumber, "0x"), 16, 64)
	return n
}

// SendETH sends value (cast units, e.g. "1ether" or a wei integer) from dev
// account #0 to to.
func (a *Anvil) SendETH(ctx context.Context, to, value string) (Receipt, error) {
	return a.send(ctx, to, "--value", value)
}

// Deploy creates a contract from hex bytecode with optional constructor
// signature and arguments.
func (a *Anvil) Deploy(ctx context.Context, bytecode string, ctorAndArgs ...string) (Receipt, error) {
	return a.send(ctx, "--create", append([]string{strings.TrimSpace(bytecode)}, ctorAndArgs...)...)
}

// Transact calls a contract function from dev account #0.
func (a *Anvil) Transact(ctx context.Context, to, sig string, args ...string) (Receipt, error) {
	return a.send(ctx, to, append([]string{sig}, args...)...)
}

func (a *Anvil) send(ctx context.Context, first string, rest ...string) (Receipt, error) {
	args := append([]string{"send", "--json", "--rpc-url", a.URL, "--private-key", DevKey0, first}, rest...)
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, a.cast, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return Receipt{}, fmt.Errorf("devnet: cast send: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var r Receipt
	if err := json.Unmarshal(out, &r); err != nil {
		return Receipt{}, fmt.Errorf("devnet: cast send output: %w", err)
	}
	if r.Status != "0x1" {
		return r, fmt.Errorf("devnet: transaction %s failed (status %s)", r.TxHash, r.Status)
	}
	return r, nil
}
