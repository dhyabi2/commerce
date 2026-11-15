package processor

import (
	"context"
	"strings"
	"time"
)

// A Grant is a scoped, revocable permission from a payer (or the agent acting for them) to
// charge a payment method once, or on a schedule, within limits. A processor that issues
// them to sellers implements GrantReader; commerce reads the grant before every charge
// made on it.
type Grant struct {
	ID string `json:"id"`
	// Active says the grant can still be used: it has not been revoked or deactivated.
	// Why is the processor's reason when it is not ("revoked", "expired", ...).
	Active bool   `json:"active"`
	Why    string `json:"why"`
	// Limits the payer set. MaxAmount is in minor units of Currency; ExpiresAt is unix
	// seconds; RecurringInterval is "" for a one-time grant, else the period it may be
	// charged on ("month").
	Currency  string `json:"currency"`
	MaxAmount int64  `json:"maxAmount"`
	// Captured is what has already been charged on the token, against MaxAmount.
	Captured          int64  `json:"captured"`
	ExpiresAt         int64  `json:"expiresAt"`
	RecurringInterval string `json:"recurringInterval"`
	// What can be said of the payment method behind it without the credentials.
	Country string `json:"country"`
	Brand   string `json:"brand"`
	Last4   string `json:"last4"`
	// Test says the grant was made in the processor's test mode.
	Test bool `json:"test"`
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
	On bool `json:"on"`
	// Networks the processor's deposit mode accepts, in the processor's names, and the
	// currency stablecoin payments are presented in ("usd" only).
	Networks []string `json:"networks"`
	Currency string   `json:"currency"`
	// Why is the reason when On is false.
	Why string `json:"why"`
}

// StablecoinDepositIn opens a stablecoin payment of Amount (minor units of Currency, usd).
type StablecoinDepositIn struct {
	Amount   int64                  `json:"amount"`
	Currency string                 `json:"currency"`
	Network  string                 `json:"network"`
	Key      string                 // idempotency key `json:"key"`
	Metadata map[string]interface{} `json:"metadata"`
	Desc     string                 `json:"desc"`
}

// StablecoinDeposit is where to send the payment: the intent that tracks it and, per
// network, the address and the token contract to send.
type StablecoinDeposit struct {
	ID        string           `json:"id"`
	Amount    int64            `json:"amount"`
	Currency  string           `json:"currency"`
	Addresses []DepositAddress `json:"addresses"`
}

// DepositAddress is one network's address and the tokens it accepts.
type DepositAddress struct {
	Network string         `json:"network"`
	Address string         `json:"address"`
	Tokens  []DepositToken `json:"tokens"`
}

// DepositToken is one token a deposit address accepts: its symbol and contract (or mint).
type DepositToken struct {
	Currency string `json:"currency"`
	Contract string `json:"contract"`
}

// StablecoinDepositor is a processor that takes stablecoin payments by deposit.
type StablecoinDepositor interface {
	StablecoinReady(ctx context.Context) StablecoinReady
	OpenStablecoinDeposit(ctx context.Context, in StablecoinDepositIn) (*StablecoinDeposit, error)
}

// GrantRefusal says why a grant cannot be charged amount of cur at now ("" when it can),
// with a code: token_revoked, token_expired, token_wrong_currency, token_over_limit or
// token_not_recurring. offSession is a charge without the buyer there, which needs a token
// that carries a recurring interval: a one-time token pays once.
func GrantRefusal(g *Grant, amount int64, cur string, offSession bool, now time.Time) (code, why string) {
	switch {
	case !g.Active:
		return "token_revoked", "the token is " + g.Why
	case g.ExpiresAt > 0 && now.Unix() >= g.ExpiresAt:
		return "token_expired", "the token has expired"
	case g.Currency != "" && g.Currency != strings.ToLower(cur):
		return "token_wrong_currency", "the token is for " + strings.ToUpper(g.Currency) + ", not " + strings.ToUpper(cur)
	case g.MaxAmount > 0 && amount > g.MaxAmount-g.Captured:
		return "token_over_limit", "the amount is over what the token has left"
	case offSession && g.RecurringInterval == "":
		return "token_not_recurring", "the token carries no recurring authorization, so it pays once"
	}
	return "", ""
}
