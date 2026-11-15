package billing

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hanzoai/commerce/datastore"
	"github.com/hanzoai/commerce/models/billingevent"
	"github.com/hanzoai/commerce/models/billinginvoice"
	"github.com/hanzoai/commerce/models/checkoutsession"
	"github.com/hanzoai/commerce/models/organization"
	"github.com/hanzoai/commerce/models/pendingintent"
	"github.com/hanzoai/commerce/models/subscription"
	"github.com/hanzoai/commerce/models/types/currency"
	"github.com/hanzoai/commerce/payment/processor"
)

// depositStripe is a Stripe that takes stablecoin deposits: each opened deposit is an intent
// that stays open until the test settles it.
type depositStripe struct {
	stripeDouble
	mu      sync.Mutex
	n       int
	amounts map[string]int64
	done    map[string]bool
}

func (d *depositStripe) StablecoinReady(context.Context) processor.StablecoinReady {
	return processor.StablecoinReady{On: true, Networks: []string{"base", "solana", "tempo"}, Currency: "usd"}
}

func (d *depositStripe) OpenStablecoinDeposit(_ context.Context, in processor.StablecoinDepositIn) (*processor.StablecoinDeposit, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.n++
	id := fmt.Sprintf("pi_dep_%d", d.n)
	if d.amounts == nil {
		d.amounts, d.done = map[string]int64{}, map[string]bool{}
	}
	d.amounts[id] = in.Amount
	return &processor.StablecoinDeposit{ID: id, Amount: in.Amount, Currency: "usd", Addresses: []processor.DepositAddress{
		{Network: "base", Address: "0xabc" + id, Tokens: []processor.DepositToken{{Currency: "usdc", Contract: "0xusdc"}}},
	}}, nil
}

func (d *depositStripe) GetTransaction(_ context.Context, id string) (*processor.Transaction, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := "requires_action"
	if d.done[id] {
		st = "succeeded"
	}
	return &processor.Transaction{ID: id, ProcessorRef: "ch_" + id, Amount: currency.Cents(d.amounts[id]), Currency: "usd", Status: st}, nil
}

func (d *depositStripe) Charge(_ context.Context, req processor.PaymentRequest) (*processor.PaymentResult, error) {
	return &processor.PaymentResult{Success: true, TransactionID: req.Token, ProcessorRef: "ch_" + req.Token}, nil
}

func (d *depositStripe) CancelIntent(context.Context, string) error { return nil }

// settle is Stripe reporting the deposit succeeded, delivered once.
func (d *depositStripe) settle(ctx context.Context, t *testing.T, org *organization.Organization, id string) error {
	t.Helper()
	d.mu.Lock()
	d.done[id] = true
	amount := d.amounts[id]
	d.mu.Unlock()
	db := datastore.New(org.Namespaced(ctx))
	return applySettlementEvent(ctx, db, org, succeeded(id, "ch_"+id, amount, "usd"))
}

func eventsNamed(t *testing.T, ctx context.Context, typ string) []*billingevent.BillingEvent {
	t.Helper()
	evs := make([]*billingevent.BillingEvent, 0)
	if _, err := billingevent.Query(webhookGuardDB(ctx)).Filter("Type=", typ).GetAll(&evs); err != nil {
		t.Fatal(err)
	}
	return evs
}

// A plan paid by stablecoin: the first period opens when the deposit succeeds, a renewal
// opens a deposit and leaves the subscription past due until it is paid, and the period
// starts only then. A deposit reported twice changes nothing.
func TestACryptoSubscriptionIsPaidByDepositEachPeriod(t *testing.T) {
	ctx := heldStore(t)
	org := routingOrg(ctx, t)
	if _, _, err := SeedPlans(ctx); err != nil {
		t.Fatal(err)
	}
	led := newFakeLedger()
	injectLedger(t, led)
	d := &depositStripe{stripeDouble: stripeDouble{}}
	old := processorsForOrg
	processorsForOrg = func(*organization.Organization) *processor.Registry {
		reg := processor.NewRegistry(processor.DefaultConfig())
		reg.Register(d)
		return reg
	}
	t.Cleanup(func() { processorsForOrg = old })
	db := datastore.New(org.Namespaced(ctx))
	subject := "agent-buyer"
	price := lookupPlan("go").Price

	// The first period, paid by stablecoin: nothing opens until the deposit succeeds.
	v, f := CreateCheckout(ctx, org, subject, "", []ItemIn{{Kind: "plan", Plan: "go"}})
	if f != nil {
		t.Fatal(f.Message)
	}
	out := CompleteCheckout(ctx, org, subject, "", v.ID, "k1", PaymentIn{Type: "stablecoin", Network: "base"})
	if out.Status != 202 || out.View.Next == nil || out.View.Next.Type != "send_deposit" || len(out.View.Next.Addresses) == 0 {
		t.Fatalf("stablecoin plan checkout = %d %+v", out.Status, out.View)
	}
	intent := out.View.Next.IntentID
	if d.amounts[intent] != int64(price) {
		t.Fatalf("the deposit is for %d; want the plan's %d", d.amounts[intent], price)
	}
	if got, _ := ReadCheckout(ctx, org, subject, v.ID); got.Status != checkoutsession.Pending {
		t.Fatalf("the checkout before the deposit = %s", got.Status)
	}
	for i := 0; i < 3; i++ { // reported again and again
		if err := d.settle(ctx, t, org, intent); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := ReadCheckout(ctx, org, subject, v.ID)
	if got.Status != checkoutsession.Complete || got.Order == nil || got.Order.SubscriptionID == "" {
		t.Fatalf("the checkout after the deposit = %+v", got)
	}
	subs, _ := ListSubscriptions(ctx, org, subject, "")
	if len(subs) != 1 {
		t.Fatalf("the deposit opened %d subscriptions; want one", len(subs))
	}
	subID := got.Order.SubscriptionID
	if len(led.credits()) != 0 {
		t.Fatal("a plan's deposit was credited to the wallet")
	}

	// Renewal, unpaid: an invoice, a deposit for its exact amount, a past-due subscription and
	// the action that pays it.
	due(t, db, subID)
	before := len(eventsNamed(t, ctx, "renewal.action_required"))
	if r := renewDue(ctx, org, ""); len(r) != 1 || r[0].Success {
		t.Fatalf("an unpaid renewal = %+v", r)
	}
	if s := reload(t, db, subID); s.Status != subscription.PastDue {
		t.Fatalf("the subscription after an unpaid renewal is %s; want past_due", s.Status)
	}
	evs := eventsNamed(t, ctx, "renewal.action_required")
	if len(evs) != before+1 {
		t.Fatalf("renewal.action_required events = %d; want %d", len(evs), before+1)
	}
	inv := openInvoiceOf(t, db, subID)
	open := finishedInvoiceIntent(inv)
	if open == nil || d.amounts[open.Id()] != inv.AmountDue || inv.AmountDue != int64(price) {
		t.Fatalf("the renewal's deposit = %+v for an invoice of %d", open, inv.AmountDue)
	}

	// Paid: the deposit succeeds, the invoice is paid and the period starts — once, however
	// often it is reported.
	periodBefore := reload(t, db, subID).PeriodEnd
	for i := 0; i < 3; i++ {
		if err := d.settle(ctx, t, org, open.Id()); err != nil {
			t.Fatalf("report %d: %v", i, err)
		}
	}
	paid := billinginvoice.New(db)
	if err := paid.GetById(inv.Id()); err != nil || paid.Status != billinginvoice.Paid {
		t.Fatalf("the renewal invoice = %v (%v); want paid", paid.Status, err)
	}
	after := reload(t, db, subID)
	if after.Status != subscription.Active || !after.PeriodEnd.After(periodBefore) {
		t.Fatalf("after the deposit: status %s, period end %v (was %v)", after.Status, after.PeriodEnd, periodBefore)
	}
	if len(led.credits()) != 0 {
		t.Fatal("a renewal's deposit was credited to the wallet")
	}
	if n := len(pendingintentsFor(t, subject, pendingintent.Invoice)); n != 1 {
		t.Fatalf("the renewal opened %d deposits; want one", n)
	}
}

func pendingintentsFor(t *testing.T, subject, purpose string) []*pendingintent.Intent {
	t.Helper()
	all, err := pendingintent.Since(subject, purpose, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return all
}
