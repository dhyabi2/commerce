package processor

import "context"

// A Grant is a scoped, revocable permission from a payer (or the agent acting for them) to
// charge a payment method once, or on a schedule, within limits. A processor that issues
// them to sellers implements GrantReader; commerce reads the grant before every charge
// made on it.
type Grant struct {
	ID string
	// Active says the grant can still be used: it has not been revoked or deactivated.
	// Why is the processor's reason when it is not ("revoked", "expired", ...).
	Active bool
	Why    string
	// Limits the payer set. MaxAmount is in minor units of Currency; ExpiresAt is unix
	// seconds; RecurringInterval is "" for a one-time grant, else the period it may be
	// charged on ("month").
	Currency          string
	MaxAmount         int64
	ExpiresAt         int64
	RecurringInterval string
	// What can be said of the payment method behind it without the credentials.
	Country string
	Brand   string
	Last4   string
	// Test says the grant was made in the processor's test mode.
	Test bool
}

// GrantReader is a processor that can say what a grant allows. An error is the
// processor's refusal to know the grant (not found, not ours), and means no charge.
type GrantReader interface {
	Grant(ctx context.Context, id string) (*Grant, error)
}

// Stablecoin deposits: a payment made by sending a stablecoin to an address the processor
// shows, with no page in between.

// StablecoinReady says what the processor can take in stablecoins now.
type StablecoinReady struct {
	// On says the account can take stablecoin payments now.
	On bool
	// Networks the processor's deposit mode accepts, in the processor's names, and the
	// currency stablecoin payments are presented in ("usd" only).
	Networks []string
	Currency string
	// Why is the reason when On is false.
	Why string
}

// StablecoinDepositIn opens a stablecoin payment of Amount (minor units of Currency, usd).
type StablecoinDepositIn struct {
	Amount   int64
	Currency string
	Network  string
	Key      string // idempotency key
	Metadata map[string]interface{}
	Desc     string
}

// StablecoinDeposit is where to send the payment: the intent that tracks it and, per
// network, the address and the token contract to send.
type StablecoinDeposit struct {
	ID        string
	Amount    int64
	Currency  string
	Addresses []DepositAddress
}

// DepositAddress is one network's address and the tokens it accepts.
type DepositAddress struct {
	Network string
	Address string
	Tokens  []DepositToken
}

// DepositToken is one token a deposit address accepts: its symbol and contract (or mint).
type DepositToken struct {
	Currency string
	Contract string
}

// StablecoinDepositor is a processor that takes stablecoin payments by deposit.
type StablecoinDepositor interface {
	StablecoinReady(ctx context.Context) StablecoinReady
	OpenStablecoinDeposit(ctx context.Context, in StablecoinDepositIn) (*StablecoinDeposit, error)
}
