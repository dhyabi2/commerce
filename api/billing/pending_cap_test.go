package billing

import (
	"fmt"
	"testing"
	"time"

	"github.com/hanzoai/commerce/models/pendingintent"
)

// aged moves an open payment's record back by d, as if it had been opened d ago.
func aged(t *testing.T, id string, d time.Duration) {
	t.Helper()
	p, found, err := pendingintent.Get(id)
	if err != nil || !found {
		t.Fatalf("payment %s: found %v, %v", id, found, err)
	}
	p.CreatedAt = p.CreatedAt.Add(-d)
	if ok, err := p.Swap(); err != nil || !ok {
		t.Fatalf("ageing %s: %v %v", id, ok, err)
	}
}

// openPayments records n payments for subject that are still waiting on their bank.
func openPayments(t *testing.T, prefix, subject string, n int) []string {
	t.Helper()
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("pi_%s_%d", prefix, i)
		if err := pendingintent.Put(ids[i], "hanzo", subject, pendingintent.Topup, 2500, "usd", true); err != nil {
			t.Fatal(err)
		}
	}
	return ids
}

// The cap counts the payments commerce is still waiting on, and only those. A payment
// past the moment commerce stops waiting on its buyer (the sweep's own deadline) holds
// no place under it, whether or not the sweep has closed it yet. The sweep runs only
// inside the renewal sweep, and a deployment that never runs that (testnet has no
// schedules) used to keep five abandoned payments open forever, refusing every
// payment their buyer tried after them.
func TestAPaymentPastItsDeadlineHoldsNoPlaceUnderTheCap(t *testing.T) {
	heldStore(t)

	// A card's 48 hours.
	cards := openPayments(t, "card", "stale-buyer", maxUnfinished)
	if atCap("stale-buyer") == nil {
		t.Fatalf("%d payments inside their 48 hours are under the cap", maxUnfinished)
	}
	for _, id := range cards {
		aged(t, id, abandonAfter-time.Hour)
	}
	if atCap("stale-buyer") == nil {
		t.Fatal("payments an hour short of their 48 hours no longer hold their place")
	}
	for _, id := range cards {
		aged(t, id, 2*time.Hour)
	}
	if err := atCap("stale-buyer"); err != nil {
		t.Fatalf("%d payments past their 48 hours still fill the cap: %v", maxUnfinished, err)
	}

	// A delayed method waits its eight days, not a card's 48 hours.
	slow := openPayments(t, "slow", "slow-buyer", maxUnfinished)
	for _, id := range slow {
		if err := pendingintent.Bind(id, pendingintent.Terms{Delayed: true}); err != nil {
			t.Fatal(err)
		}
		aged(t, id, 3*24*time.Hour)
	}
	if atCap("slow-buyer") == nil {
		t.Fatal("bank debits three days old, inside their eight days, no longer hold their place")
	}
	for _, id := range slow {
		aged(t, id, delayedAbandonAfter)
	}
	if err := atCap("slow-buyer"); err != nil {
		t.Fatalf("bank debits past their eight days still fill the cap: %v", err)
	}

	// A card on a locked quote is abandoned when its lock lapses, however young.
	for i := 0; i < maxUnfinished; i++ {
		id := fmt.Sprintf("pi_quoted_%d", i)
		if err := pendingintent.PutQuoted(id, "hanzo", "quoted-buyer", pendingintent.Topup, 2500, "usd", 13000, "brl", 1, true); err != nil {
			t.Fatal(err)
		}
		if err := pendingintent.Bind(id, pendingintent.Terms{LockExpiresAt: time.Now().Add(-time.Minute).Unix()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := atCap("quoted-buyer"); err != nil {
		t.Fatalf("cards whose quote lapsed still fill the cap: %v", err)
	}

	// New payments beside the abandoned ones: only the new ones count.
	openPayments(t, "fresh", "stale-buyer", maxUnfinished-1)
	if err := atCap("stale-buyer"); err != nil {
		t.Fatalf("%d waiting payments beside abandoned ones are at the cap: %v", maxUnfinished-1, err)
	}
	openPayments(t, "last", "stale-buyer", 1)
	if atCap("stale-buyer") == nil {
		t.Fatalf("%d payments still waiting are under the cap", maxUnfinished)
	}
}
