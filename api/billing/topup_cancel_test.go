package billing

import (
	"context"
	"fmt"
	"testing"

	"github.com/hanzoai/commerce/models/organization"
	"github.com/hanzoai/commerce/models/pendingintent"
	"github.com/hanzoai/commerce/payment/processor"
)

// cancellingStripe is a Stripe whose cancel moves the payment to canceled, as Stripe's does.
type cancellingStripe struct{ stripeDouble }

func (d *cancellingStripe) CancelIntent(_ context.Context, id string) error {
	d.cancelled = append(d.cancelled, id)
	d.tx.Status = "canceled"
	return nil
}

func useCancelling(t *testing.T, status string) *cancellingStripe {
	t.Helper()
	d := &cancellingStripe{stripeDouble{tx: &processor.Transaction{ProcessorRef: "ch_x", Amount: 2500, Currency: "usd", Status: status}}}
	old := processorsForOrg
	processorsForOrg = func(*organization.Organization) *processor.Registry {
		reg := processor.NewRegistry(processor.DefaultConfig())
		reg.Register(d)
		return reg
	}
	t.Cleanup(func() { processorsForOrg = old })
	return d
}

func TestACallerCancelsTheirOwnUnconfirmedTopupOnceAndTheSlotIsFreed(t *testing.T) {
	ctx := heldStore(t)
	org := routingOrg(ctx, t)
	d := useCancelling(t, "requires_payment_method")
	for i := 0; i < maxUnfinished; i++ {
		if err := pendingintent.Put(fmt.Sprintf("pi_cap_%d", i), org.Name, "buyer", pendingintent.Topup, 2500, "usd", false); err != nil {
			t.Fatal(err)
		}
	}
	if atCap("buyer") == nil {
		t.Fatalf("%d open payments are under the cap", maxUnfinished)
	}
	before := len(eventsNamed(t, ctx, "topup.intent.canceled"))

	out, f := CancelTopupIntent(ctx, org, "buyer", "pi_cap_0")
	if f != nil || out.State != "canceled" || len(d.cancelled) != 1 || d.cancelled[0] != "pi_cap_0" {
		t.Fatalf("cancel = %+v %v (cancelled at the processor: %v)", out, f, d.cancelled)
	}
	if p, _, _ := pendingintent.Get("pi_cap_0"); !p.Done {
		t.Fatal("the cancelled payment still holds a place under the cap")
	}
	if atCap("buyer") != nil {
		t.Fatal("the cap slot was not freed")
	}
	if n := len(eventsNamed(t, ctx, "topup.intent.canceled")); n != before+1 {
		t.Fatalf("cancellation events = %d; want %d", n, before+1)
	}

	// Again: the same answer, nothing more cancelled, no second event.
	again, f := CancelTopupIntent(ctx, org, "buyer", "pi_cap_0")
	if f != nil || again.State != "canceled" || len(d.cancelled) != 1 {
		t.Fatalf("second cancel = %+v %v (cancelled at the processor: %v)", again, f, d.cancelled)
	}
	if n := len(eventsNamed(t, ctx, "topup.intent.canceled")); n != before+1 {
		t.Fatalf("the repeat recorded another event: %d", n)
	}
}

func TestAPaidOrProcessingTopupCannotBeCancelled(t *testing.T) {
	for _, status := range []string{"succeeded", "processing"} {
		t.Run(status, func(t *testing.T) {
			ctx := heldStore(t)
			org := routingOrg(ctx, t)
			d := useCancelling(t, status)
			if err := pendingintent.Put("pi_paid", org.Name, "buyer", pendingintent.Topup, 2500, "usd", false); err != nil {
				t.Fatal(err)
			}
			if _, f := CancelTopupIntent(ctx, org, "buyer", "pi_paid"); f == nil || f.Status != 409 {
				t.Fatalf("cancelling a %s payment = %v; want 409", status, f)
			}
			if len(d.cancelled) != 0 {
				t.Fatal("a payment that cannot be cancelled was cancelled at the processor")
			}
			if p, _, _ := pendingintent.Get("pi_paid"); p.Done {
				t.Fatal("the payment's record was closed")
			}
		})
	}
}

func TestACreditedTopupCannotBeCancelled(t *testing.T) {
	ctx := heldStore(t)
	org := routingOrg(ctx, t)
	w := settledWallet(ctx, t, "pi_credited", 2500, 0, "")
	d := useCancelling(t, "requires_payment_method") // whatever the processor now says
	d.tx.ProcessorRef = w.charge
	if _, f := CancelTopupIntent(ctx, w.org, "payer", "pi_credited"); f == nil || f.Status != 409 {
		t.Fatalf("cancelling a credited payment = %v; want 409", f)
	}
	if len(d.cancelled) != 0 {
		t.Fatal("a credited payment was cancelled at the processor")
	}
	_ = org
}

func TestAnotherSubjectsTopupIsNotFound(t *testing.T) {
	ctx := heldStore(t)
	org := routingOrg(ctx, t)
	d := useCancelling(t, "requires_payment_method")
	if err := pendingintent.Put("pi_theirs", org.Name, "someone-else", pendingintent.Topup, 2500, "usd", false); err != nil {
		t.Fatal(err)
	}
	if err := pendingintent.Put("pi_sale", org.Name, "buyer", pendingintent.Sale, 2500, "usd", false); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"pi_theirs", "pi_sale", "pi_nobody"} {
		if _, f := CancelTopupIntent(ctx, org, "buyer", id); f == nil || f.Status != 404 {
			t.Fatalf("cancelling %s = %v; want 404", id, f)
		}
	}
	if len(d.cancelled) != 0 {
		t.Fatalf("a payment that is not the caller's top-up was cancelled: %v", d.cancelled)
	}
}
