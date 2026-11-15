package billing

import (
	"testing"
	"time"

	"github.com/hanzoai/commerce/payment/processor"
)

// The catalog is in dollars, lists packs within the top-up bounds, and offers a payment type
// only where the account can take it.
func TestTheCatalogIsUSDAndOffersOnlyWhatCanBePaid(t *testing.T) {
	ctx := heldStore(t)
	org := routingOrg(ctx, t)
	useRails(t, false, true)
	cat, err := ReadCatalog(ctx, org, "")
	if err != nil {
		t.Fatal(err)
	}
	if cat.Currency != "usd" || cat.Version != agentVersion || len(cat.CreditPacks) == 0 {
		t.Fatalf("catalog = %+v", cat)
	}
	min, max := topupBounds()
	for _, p := range cat.CreditPacks {
		if p.UsdCents < min || p.UsdCents > max {
			t.Fatalf("pack %+v is outside the top-up bounds", p)
		}
	}
	for _, o := range cat.Payment {
		if o.Currency != "usd" {
			t.Fatalf("payment option %+v is not in dollars", o)
		}
		switch o.Type {
		case "stablecoin":
			if len(o.Networks) == 0 || o.Processor != "stripe" {
				t.Fatalf("stablecoin option = %+v", o)
			}
		case "shared_payment_token":
			t.Fatal("tokens offered by an account whose processor cannot read them")
		}
	}
	for _, link := range []string{"catalog", "checkouts", "orders", "discovery"} {
		if cat.Links[link] == "" {
			t.Fatalf("no %s link", link)
		}
	}
}

// A grant is judged by its limits: what is left on it, its currency, expiry and revocation,
// and an off-session charge needs a recurring interval.
func TestAGrantIsJudgedByWhatItAllows(t *testing.T) {
	g := func(f func(*processor.Grant)) *processor.Grant {
		x := &processor.Grant{Active: true, Currency: "usd", MaxAmount: 3000, ExpiresAt: 4_000_000_000}
		f(x)
		return x
	}
	now := time.Now()
	for _, tc := range []struct {
		name string
		g    *processor.Grant
		amt  int64
		cur  string
		off  bool
		want string
	}{
		{"usable", g(func(*processor.Grant) {}), 1500, "usd", false, ""},
		{"revoked", g(func(x *processor.Grant) { x.Active, x.Why = false, "revoked" }), 1500, "usd", false, "token_revoked"},
		{"expired", g(func(x *processor.Grant) { x.ExpiresAt = 1 }), 1500, "usd", false, "token_expired"},
		{"wrong currency", g(func(*processor.Grant) {}), 1500, "eur", false, "token_wrong_currency"},
		{"over its limit", g(func(*processor.Grant) {}), 3001, "usd", false, "token_over_limit"},
		{"nothing left", g(func(x *processor.Grant) { x.Captured = 3000 }), 1, "usd", false, "token_over_limit"},
		{"one-time, off session", g(func(*processor.Grant) {}), 1500, "usd", true, "token_not_recurring"},
		{"recurring, off session", g(func(x *processor.Grant) { x.RecurringInterval = "month" }), 1500, "usd", true, ""},
	} {
		if code, _ := processor.GrantRefusal(tc.g, tc.amt, tc.cur, tc.off, now); code != tc.want {
			t.Errorf("%s: code = %q; want %q", tc.name, code, tc.want)
		}
	}
}
