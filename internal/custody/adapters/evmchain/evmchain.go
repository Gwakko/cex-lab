// Package evmchain is the custody ChainClient for EVM nodes, built on
// go-ethereum's JSON-RPC client. It reports native ETH sent to watched
// addresses (from block transactions, successful receipts only) and ERC-20
// Transfer logs from configured token contracts. It is the only package that
// imports go-ethereum, and it only reads: no keys, no signing.
//
// Block hashes and parent hashes are taken from the node's JSON, never
// recomputed locally, so reorg detection compares exactly what the node says.
package evmchain

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/gwakko/cex-lab/internal/custody"
)

// NativeLogIndex is the LogIndex of native transfers, which have no log. It
// cannot collide with a real log index in the same transaction, so
// (txhash, logindex) stays a unique idempotency key.
const NativeLogIndex = math.MaxUint32

// TransferTopic is keccak256("Transfer(address,address,uint256)").
var TransferTopic = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))

var (
	// ErrAmountOverflow means a deposit does not fit the ledger's int64 units.
	ErrAmountOverflow = errors.New("evmchain: amount does not fit int64 ledger units")
	// ErrAmountPrecision means a deposit is not a whole number of ledger units.
	ErrAmountPrecision = errors.New("evmchain: amount is not a whole number of ledger units")
)

// Token is an ERC-20 contract to watch.
type Token struct {
	Address string // contract address
	Asset   string // ledger asset name, e.g. "USDC"
	UnitExp uint   // one ledger unit = 10^UnitExp token base units
}

// Config selects the node and the assets.
type Config struct {
	URL           string // JSON-RPC endpoint, e.g. http://127.0.0.1:8545
	NativeAsset   string // ledger asset for native deposits; "" disables them
	NativeUnitExp uint   // one ledger unit = 10^NativeUnitExp wei (9 = gwei)
	Tokens        []Token
}

// Client implements custody.ChainClient.
type Client struct {
	rpc       *rpc.Client
	eth       *ethclient.Client
	native    string
	nativeExp uint
	tokens    map[common.Address]Token
	addrs     []common.Address
}

var _ custody.ChainClient = (*Client)(nil)

// Dial connects to the node and validates the configuration.
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	tokens := map[common.Address]Token{}
	var addrs []common.Address
	for _, t := range cfg.Tokens {
		if !common.IsHexAddress(t.Address) || t.Asset == "" {
			return nil, fmt.Errorf("evmchain: invalid token %+v", t)
		}
		a := common.HexToAddress(t.Address)
		tokens[a] = t
		addrs = append(addrs, a)
	}
	rc, err := rpc.DialContext(ctx, cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("evmchain: dial %s: %w", cfg.URL, err)
	}
	return &Client{rpc: rc, eth: ethclient.NewClient(rc), native: cfg.NativeAsset, nativeExp: cfg.NativeUnitExp, tokens: tokens, addrs: addrs}, nil
}

// Close closes the connection.
func (c *Client) Close() { c.rpc.Close() }

func (c *Client) LatestBlock(ctx context.Context) (uint64, error) { return c.eth.BlockNumber(ctx) }

type rpcHeader struct {
	Number     hexutil.Uint64 `json:"number"`
	Hash       common.Hash    `json:"hash"`
	ParentHash common.Hash    `json:"parentHash"`
}

func (c *Client) HeaderByNumber(ctx context.Context, n uint64) (custody.Header, error) {
	var h *rpcHeader
	if err := c.rpc.CallContext(ctx, &h, "eth_getBlockByNumber", hexutil.EncodeUint64(n), false); err != nil {
		return custody.Header{}, err
	}
	if h == nil {
		return custody.Header{}, fmt.Errorf("%w: block %d", custody.ErrNotFound, n)
	}
	return custody.Header{Number: uint64(h.Number), Hash: h.Hash.Hex(), Parent: h.ParentHash.Hex()}, nil
}

// TransfersTo returns native and token deposits to addrs in the block. The
// returned To is the caller's spelling of the address, so it matches the
// owners map used for crediting.
func (c *Client) TransfersTo(ctx context.Context, blockHash string, addrs []string) ([]custody.Transfer, error) {
	watched := map[common.Address]string{}
	for _, a := range addrs {
		if !common.IsHexAddress(a) {
			return nil, fmt.Errorf("evmchain: invalid watched address %q", a)
		}
		watched[common.HexToAddress(a)] = a
	}
	if len(watched) == 0 {
		return nil, nil
	}
	hash := common.HexToHash(blockHash)
	var out []custody.Transfer
	if c.native != "" {
		ts, err := c.nativeTransfers(ctx, hash, watched)
		if err != nil {
			return nil, err
		}
		out = append(out, ts...)
	}
	if len(c.addrs) > 0 {
		ts, err := c.tokenTransfers(ctx, hash, watched)
		if err != nil {
			return nil, err
		}
		out = append(out, ts...)
	}
	return out, nil
}

type rpcBlock struct {
	Transactions []struct {
		Hash  common.Hash     `json:"hash"`
		To    *common.Address `json:"to"`
		Value *hexutil.Big    `json:"value"`
	} `json:"transactions"`
}

func (c *Client) nativeTransfers(ctx context.Context, hash common.Hash, watched map[common.Address]string) ([]custody.Transfer, error) {
	var b *rpcBlock
	if err := c.rpc.CallContext(ctx, &b, "eth_getBlockByHash", hash, true); err != nil {
		return nil, err
	}
	if b == nil {
		return nil, fmt.Errorf("%w: block %s", custody.ErrNotFound, hash.Hex())
	}
	var out []custody.Transfer
	for _, tx := range b.Transactions {
		if tx.To == nil || tx.Value == nil || tx.Value.ToInt().Sign() == 0 {
			continue
		}
		to, ok := watched[*tx.To]
		if !ok {
			continue
		}
		r, err := c.eth.TransactionReceipt(ctx, tx.Hash)
		if err != nil {
			return nil, fmt.Errorf("evmchain: receipt %s: %w", tx.Hash.Hex(), err)
		}
		if r.Status != types.ReceiptStatusSuccessful {
			continue // reverted: no value moved
		}
		amount, err := toUnits(tx.Value.ToInt(), c.nativeExp)
		if err != nil {
			return nil, fmt.Errorf("%w (native tx %s, %s wei)", err, tx.Hash.Hex(), tx.Value.ToInt())
		}
		out = append(out, custody.Transfer{TxHash: tx.Hash.Hex(), LogIndex: NativeLogIndex, To: to, Asset: c.native, Amount: amount})
	}
	return out, nil
}

func (c *Client) tokenTransfers(ctx context.Context, hash common.Hash, watched map[common.Address]string) ([]custody.Transfer, error) {
	var toTopics []common.Hash
	for a := range watched {
		toTopics = append(toTopics, common.BytesToHash(a.Bytes()))
	}
	logs, err := c.eth.FilterLogs(ctx, ethereum.FilterQuery{
		BlockHash: &hash,
		Addresses: c.addrs,
		Topics:    [][]common.Hash{{TransferTopic}, nil, toTopics},
	})
	if err != nil {
		return nil, fmt.Errorf("evmchain: logs for %s: %w", hash.Hex(), err)
	}
	var out []custody.Transfer
	for _, l := range logs {
		if l.Removed || len(l.Topics) != 3 || len(l.Data) != 32 {
			continue // not a standard Transfer, or no longer canonical
		}
		to, ok := watched[common.BytesToAddress(l.Topics[2].Bytes())]
		token, known := c.tokens[l.Address]
		if !ok || !known {
			continue
		}
		raw := new(big.Int).SetBytes(l.Data)
		amount, err := toUnits(raw, token.UnitExp)
		if err != nil {
			return nil, fmt.Errorf("%w (%s transfer tx %s log %d, %s base units)", err, token.Asset, l.TxHash.Hex(), l.Index, raw)
		}
		if amount == 0 {
			continue
		}
		out = append(out, custody.Transfer{TxHash: l.TxHash.Hex(), LogIndex: uint32(l.Index), To: to, Asset: token.Asset, Amount: amount})
	}
	return out, nil
}

// toUnits converts base units (wei, token base units) to int64 ledger units of
// 10^exp base units. It never truncates or wraps: a remainder or an overflow
// is an error, so the watcher stops loudly instead of crediting a wrong amount.
func toUnits(v *big.Int, exp uint) (int64, error) {
	unit := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(exp)), nil)
	q, r := new(big.Int).QuoRem(v, unit, new(big.Int))
	if r.Sign() != 0 {
		return 0, ErrAmountPrecision
	}
	if !q.IsInt64() {
		return 0, ErrAmountOverflow
	}
	return q.Int64(), nil
}
