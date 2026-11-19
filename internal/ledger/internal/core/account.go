// Package core implements the ledger module: a double-entry, append-only ledger with integer amounts.
//
// Every command becomes one transaction whose entries sum to zero per asset.
// Balances are never stored on their own: they are derived from entries.
package core

// Asset identifies a currency. Amounts are always int64 minimal units
// (satoshi, cents, ...), never floats.
type Asset string

// Kind is the role of an account.
type Kind string

const (
	Available           Kind = "available"
	Locked              Kind = "locked"
	Fees                Kind = "fees"
	External            Kind = "external"
	WithdrawalsInFlight Kind = "withdrawals_in_flight"
)

// System owns the exchange-level accounts (fees, external, in-flight).
const System = "system"

// Account is identified by owner, asset and kind.
type Account struct {
	Owner string
	Asset Asset
	Kind  Kind
}

// UserAccount returns a user's account of the given kind.
func UserAccount(user string, asset Asset, kind Kind) Account {
	return Account{Owner: user, Asset: asset, Kind: kind}
}

// SystemAccount returns an exchange-level account.
func SystemAccount(asset Asset, kind Kind) Account {
	return Account{Owner: System, Asset: asset, Kind: kind}
}

func (a Account) String() string {
	return a.Owner + "/" + string(a.Asset) + "/" + string(a.Kind)
}

// mayBeNegative reports whether the account mirrors money outside the
// exchange. External goes negative by exactly the amount deposited and not
// yet withdrawn; every other account must stay >= 0.
func (a Account) mayBeNegative() bool {
	return a.Owner == System && a.Kind == External
}

// Entry is one leg of a transaction. Positive credits the account.
type Entry struct {
	Account Account
	Amount  int64
}
