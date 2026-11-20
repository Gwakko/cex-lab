package evmchain

import (
	"context"
	"errors"
	"math"
	"math/big"
	"testing"
)

func TestToUnitsNeverWrapsOrTruncates(t *testing.T) {
	ether := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	tenEther := new(big.Int).Mul(ether, big.NewInt(10))
	cases := []struct {
		name string
		v    *big.Int
		exp  uint
		want int64
		err  error
	}{
		{"1 ether in wei", ether, 0, 1_000_000_000_000_000_000, nil},
		{"1 ether in gwei", ether, 9, 1_000_000_000, nil},
		{"max int64 wei", big.NewInt(math.MaxInt64), 0, math.MaxInt64, nil},
		{"10 ether in wei overflows", tenEther, 0, 0, ErrAmountOverflow},
		{"10 ether in gwei fits", tenEther, 9, 10_000_000_000, nil},
		{"2^63 overflows", new(big.Int).Lsh(big.NewInt(1), 63), 0, 0, ErrAmountOverflow},
		{"1 wei in gwei is not whole", big.NewInt(1), 9, 0, ErrAmountPrecision},
		{"zero", big.NewInt(0), 9, 0, nil},
	}
	for _, tc := range cases {
		got, err := toUnits(tc.v, tc.exp)
		if !errors.Is(err, tc.err) || got != tc.want {
			t.Errorf("%s: got %d, %v; want %d, %v", tc.name, got, err, tc.want, tc.err)
		}
	}
}

func TestTransferTopicIsTheERC20EventSignature(t *testing.T) {
	// Same value as `cast keccak 'Transfer(address,address,uint256)'`.
	const want = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"
	if TransferTopic.Hex() != want {
		t.Fatalf("topic %s", TransferTopic.Hex())
	}
}

func TestDialRejectsInvalidTokens(t *testing.T) {
	for _, tok := range []Token{{Address: "nope", Asset: "X"}, {Address: "0x0000000000000000000000000000000000000001"}} {
		if _, err := Dial(context.Background(), Config{URL: "http://127.0.0.1:1", Tokens: []Token{tok}}); err == nil {
			t.Errorf("token %+v accepted", tok)
		}
	}
}
