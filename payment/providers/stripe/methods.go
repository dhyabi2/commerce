package stripe

import (
	"context"
	"net/url"
	"sync"
	"time"

	"github.com/hanzoai/commerce/payment/processor"
)

// methodCapabilities maps a payment method, as the routing policy names it, to the account
// capability that lets Stripe take it (https://docs.stripe.com/api/accounts/object#account_object-capabilities).
// Netbanking is not a Stripe method; Indian bank payments are UPI.
var methodCapabilities = map[string]string{
	"us_bank_account": "us_bank_account_ach_payments",
	"acss_debit":      "acss_debit_payments",
	"bacs_debit":      "bacs_debit_payments",
	"au_becs_debit":   "au_becs_debit_payments",
	"sepa_debit":      "sepa_debit_payments",
	"ideal":           "ideal_payments",
	"bancontact":      "bancontact_payments",
	"blik":            "blik_payments",
	"p24":             "p24_payments",
	"eps":             "eps_payments",
	"pix":             "pix_payments",
	"upi":             "upi_payments",
	"fpx":             "fpx_payments",
	"multibanco":      "multibanco_payments",
	"oxxo":            "oxxo_payments",
	"boleto":          "boleto_payments",
}

var capCache struct {
	sync.Mutex
	at   map[string]time.Time
	have map[string]map[string]bool
}

// Methods says, for each payment method the policy names, whether this account's
// capability for it is active now. Read from the account, cached for five minutes; an
// account that cannot be read takes nothing, so a method is never shown on a guess.
func (p *Provider) Methods(ctx context.Context) map[string]bool {
	k := p.key()
	if k == "" {
		return map[string]bool{}
	}
	capCache.Lock()
	defer capCache.Unlock()
	if capCache.have == nil {
		capCache.have, capCache.at = map[string]map[string]bool{}, map[string]time.Time{}
	}
	if m, ok := capCache.have[k]; ok && time.Since(capCache.at[k]) < 5*time.Minute {
		return m
	}
	var acct struct {
		Capabilities map[string]string `json:"capabilities"`
	}
	out := map[string]bool{}
	if err := p.get(ctx, "/account", url.Values{}, &acct); err == nil {
		for method, c := range methodCapabilities {
			out[method] = acct.Capabilities[c] == "active"
		}
	}
	capCache.have[k], capCache.at[k] = out, time.Now()
	return out
}

var _ processor.Capable = (*Provider)(nil)
