package billing

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/hanzoai/commerce/datastore"
	"github.com/hanzoai/commerce/models/cryptopaymentintent"
	"github.com/hanzoai/commerce/models/organization"
	"github.com/hanzoai/commerce/models/plan"
	"github.com/hanzoai/commerce/models/subscription"
	"github.com/hanzoai/commerce/models/types/currency"
	"github.com/hanzoai/commerce/payment/processor"
	types "github.com/hanzoai/commerce/types"
	"github.com/hanzoai/commerce/util/test/ae"
)

type stripeVaultFake struct {
	*MockSquareProcessor
}

func (s *stripeVaultFake) Type() processor.ProcessorType { return processor.Stripe }

func TestCreateMethod_SquareAndStripeSaveAndArmUnbackedPlan(t *testing.T) {
	ctx := ae.NewContext()
	defer ctx.Close()

	org := organization.New(datastore.New(ctx))
	org.Name = "hanzo"
	org.MustCreate()
	db := datastore.New(org.Namespaced(ctx))

	pl := plan.New(plan.AuthorityDB(ctx))
	pl.Slug = "go"
	pl.Name = "Go"
	pl.Price = currency.Cents(800)
	pl.Currency = currency.USD
	pl.Interval = types.Monthly
	pl.IntervalCount = 1
	pl.Category = "personal"
	pl.Status = plan.StatusActive
	pl.Managed = true
	pl.MustCreate()

	// 1. Subject buys Go with crypto/wallet first -> DefaultPaymentMethod="credits".
	sub, err := createSubscription(db, pl, &createSubscriptionRequest{
		UserId:               "hanzo/alice",
		PlanId:               "go",
		DefaultPaymentMethod: "credits",
		Quantity:             1,
	})
	if err != nil {
		t.Fatalf("createSubscription: %v", err)
	}
	sub.ProviderType = "credit"
	_ = sub.Update()

	// 2. Subject saves a Square card -> Square card saved AND unbacked subscription armed onto Square card.
	sq := squareMock("sq_cust_1", "ccof_sq_1", "sq_pay_1")
	withFakeSquare(t, sq)
	m1, created1, err := CreateMethod(ctx, org, "alice@example.com", CreateMethodIn{
		CustomerId:   "hanzo/alice",
		Type:         "card",
		ProviderRef:  "cnon:card-nonce-ok",
		ProviderType: "square",
	})
	if err != nil || !created1 || m1.Id == "" {
		t.Fatalf("CreateMethod square = (%+v, %v, %v)", m1, created1, err)
	}
	reloaded := subscription.New(db)
	if err := reloaded.GetById(sub.Id()); err != nil {
		t.Fatalf("load sub: %v", err)
	}
	if reloaded.DefaultPaymentMethod != m1.Id || reloaded.ProviderType != "square" {
		t.Fatalf("after Square save: sub DefaultPaymentMethod=%q ProviderType=%q, want %q/square", reloaded.DefaultPaymentMethod, reloaded.ProviderType, m1.Id)
	}

	// 3. Another subject with a crypto-paid plan saves a Stripe card -> armed onto Stripe.
	sub2, err := createSubscription(db, pl, &createSubscriptionRequest{
		UserId:               "hanzo/bob",
		PlanId:               "go",
		DefaultPaymentMethod: "credits",
		Quantity:             1,
	})
	if err != nil {
		t.Fatalf("createSubscription bob: %v", err)
	}
	sub2.ProviderType = "credit"
	_ = sub2.Update()

	st := &stripeVaultFake{MockSquareProcessor: squareMock("cus_stripe_1", "pm_stripe_bob_1", "pi_stripe_1")}
	prev := processorsForOrg
	processorsForOrg = func(*organization.Organization) *processor.Registry {
		reg := processor.NewRegistry(processor.DefaultConfig())
		reg.Register(sq)
		reg.Register(st)
		return reg
	}
	t.Cleanup(func() { processorsForOrg = prev })

	m2, created2, err := CreateMethod(ctx, org, "bob@example.com", CreateMethodIn{
		CustomerId:   "hanzo/bob",
		Type:         "card",
		ProviderRef:  "pm_stripe_bob_1",
		ProviderType: "stripe",
	})
	if err != nil || !created2 || m2.Id == "" {
		t.Fatalf("CreateMethod stripe = (%+v, %v, %v)", m2, created2, err)
	}
	reloaded2 := subscription.New(db)
	if err := reloaded2.GetById(sub2.Id()); err != nil {
		t.Fatalf("load sub2: %v", err)
	}
	if reloaded2.DefaultPaymentMethod != m2.Id || reloaded2.ProviderType != "stripe" {
		t.Fatalf("after Stripe save: sub2 DefaultPaymentMethod=%q ProviderType=%q, want %q/stripe", reloaded2.DefaultPaymentMethod, reloaded2.ProviderType, m2.Id)
	}
}

func TestActivateCryptoPlan_OpensSubscriptionAndLeavesRemainder(t *testing.T) {
	ctx := ae.NewContext()
	defer ctx.Close()

	org := organization.New(datastore.New(ctx))
	org.Name = "hanzo"
	org.MustCreate()
	db := datastore.New(org.Namespaced(ctx))

	pl := plan.New(plan.AuthorityDB(ctx))
	pl.Slug = "go"
	pl.Name = "Go"
	pl.Price = currency.Cents(800)
	pl.Currency = currency.USD
	pl.Interval = types.Monthly
	pl.IntervalCount = 1
	pl.Category = "personal"
	pl.Status = plan.StatusActive
	pl.Managed = true
	pl.MustCreate()

	intent := cryptopaymentintent.New(db)
	intent.Amount = 800
	intent.Currency = "usd"
	intent.Chain = cryptopaymentintent.Base
	intent.Token = "usdc"
	intent.DepositAddress = "0x1111111111111111111111111111111111111111"
	intent.CustomerRef = "hanzo/crypto-buyer"
	intent.Status = cryptopaymentintent.Pending
	intent.Metadata = types.Map{
		"planId":      "go",
		"quantity":    1,
		"interval":    "month",
		"chargeCents": int64(800),
	}
	intent.Defaults()
	if err := intent.Create(); err != nil {
		t.Fatalf("create intent: %v", err)
	}

	subID, consumed, err := activateCryptoPlan(ctx, "hanzo", "hanzo/crypto-buyer", intent, 1000, "0xtx123")
	if err != nil {
		t.Fatalf("activateCryptoPlan: %v", err)
	}
	if subID == "" || consumed != 800 {
		t.Fatalf("activateCryptoPlan = (%q, %d), want non-empty subID and 800 consumed", subID, consumed)
	}
	sub := subscription.New(db)
	if err := sub.GetById(subID); err != nil {
		t.Fatalf("load activated sub: %v", err)
	}
	if sub.Status != subscription.Active || sub.DefaultPaymentMethod != "credits" {
		t.Fatalf("activated sub = %+v; want active with credits until card/bank added", sub)
	}
}

func TestLiveStripe_UniformCashAppBankAndNativeCryptoPlan(t *testing.T) {
	l := liveSetup(t)
	pl := plan.New(plan.AuthorityDB(*l.ctx))
	pl.Slug = "go"
	pl.Name = "Go"
	pl.Price = currency.Cents(800)
	pl.Currency = currency.USD
	pl.Interval = types.Monthly
	pl.IntervalCount = 1
	pl.Category = "personal"
	pl.Status = plan.StatusActive
	pl.Managed = true
	pl.MustCreate()

	// 1. Uniform Cash App Pay pm_ -> SetupIntent ActionRequired (402 step) via CreateMethod.
	pmObj := l.call("POST", "/payment_methods", url.Values{"type": {"cashapp"}})
	pmID := str(pmObj["id"])
	_, _, err := CreateMethod(*l.ctx, l.org, "cashapp-live@hanzo.ai", CreateMethodIn{
		CustomerId:   l.subject,
		Type:         "card",
		ProviderRef:  pmID,
		ProviderType: "stripe",
	})
	var ar *authRequired
	if !errors.As(err, &ar) || !strings.HasPrefix(ar.ID, "seti_") || ar.ClientSecret == "" {
		t.Fatalf("CashApp CreateMethod = %v; want *authRequired with seti_ clientSecret", err)
	}

	// 2. Uniform US bank account SetupIntent via CreateMethod.
	_, _, err = CreateMethod(*l.ctx, l.org, "bank-live@hanzo.ai", CreateMethodIn{
		CustomerId:   l.subject,
		Type:         "card",
		ProviderRef:  "us_bank_account",
		ProviderType: "stripe",
	})
	var arBank *authRequired
	if !errors.As(err, &arBank) || !strings.HasPrefix(arBank.ID, "seti_") || arBank.ClientSecret == "" {
		t.Fatalf("US bank CreateMethod = %v; want *authRequired with seti_ clientSecret", err)
	}

	// 3. Native server-side Crypto plan deposit via CreateCryptoDepositForPlan (zero Stripe UI embed)
	//    and uniform readback via GetCryptoDeposit.
	dep, err := CreateCryptoDepositForPlan(*l.ctx, l.org, l.subject, "base", "usdc", 0, CryptoPlanIn{
		PlanID:   "go",
		Quantity: 1,
		Interval: types.Monthly,
	})
	if err != nil {
		t.Fatalf("CreateCryptoDepositForPlan (base/usdc): %v", err)
	}
	if !strings.HasPrefix(dep.ID, "pi_") || !strings.HasPrefix(dep.DepositAddress, "0x") || dep.AmountCents != 800 || dep.PlanID != "go" {
		t.Fatalf("unexpected native crypto plan deposit: %+v", dep)
	}
	readBack, err := GetCryptoDeposit(*l.ctx, l.org, l.subject, dep.ID)
	if err != nil || readBack.ID != dep.ID || readBack.Status != "pending" || readBack.PlanID != "go" {
		t.Fatalf("GetCryptoDeposit(%s) = %+v, %v", dep.ID, readBack, err)
	}
	l.call("POST", "/payment_intents/"+dep.ID+"/cancel", nil)
}
