package billing

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"net/http"
	"strings"

	"github.com/zap-proto/zip"

	"github.com/hanzoai/commerce/billing/engine"
	"github.com/hanzoai/commerce/log"
	"github.com/hanzoai/commerce/middleware"
	"github.com/hanzoai/commerce/models/idempotencykey"
	"github.com/hanzoai/commerce/models/organization"
	"github.com/hanzoai/commerce/models/pendingintent"
	"github.com/hanzoai/commerce/models/types/currency"
	jsonhttp "github.com/hanzoai/commerce/util/json/http"
)

// wireMarker is the Ref a wire's record carries, so a payer's open wire is found by it.
const wireMarker = "wire"

// openWireReference is the reference the payer quotes on their wire: unique to the transfer,
// unguessable, and kept until the bank's receipt of it is reconciled (a payer who asks again
// before paying gets the same one; after it is credited, a new one). It is the key the credit
// is posted under.
func openWireReference(org *organization.Organization, payer string) (string, error) {
	subject := strings.ToLower(strings.TrimSpace(payer))
	if open, err := pendingintent.OpenFor(subject, wireMarker); err != nil {
		return "", err
	} else if open != nil && open.Purpose == pendingintent.Wire {
		return open.Id(), nil
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	ref := "WIRE-" + strings.TrimRight(base32.StdEncoding.EncodeToString(b[:]), "=")
	if err := pendingintent.PutRef(ref, org.Name, subject, pendingintent.Wire, wireMarker, 0, "usd", org.TestMode()); err != nil {
		return "", err
	}
	return ref, nil
}

// WireCredit is the outcome of reconciling a wire.
type WireCredit struct {
	Reference   string `json:"reference"`
	Subject     string `json:"subject"`
	AmountCents int64  `json:"amountCents"`
	// Credited is true for the call that posted the credit; a repeat answers false with the
	// same facts and posts nothing.
	Credited bool `json:"credited"`
}

// ReconcileWire credits the wallet that holds reference with cents the bank received, once.
// by names who attests the receipt (an admin, or a bank feed's identity); bankRef is the
// bank's own reference for the transfer. Nothing is credited for a reference nobody was
// given, and a second reconcile of the same reference posts nothing and refuses a different
// amount. Every reconcile is recorded as a billing event.
func ReconcileWire(ctx context.Context, reference string, cents int64, bankRef, by string) (*WireCredit, *PaymentFault) {
	reference = strings.ToUpper(strings.TrimSpace(reference))
	by = strings.TrimSpace(by)
	switch {
	case reference == "":
		return nil, fault(400, "reference is required", nil)
	case cents <= 0:
		return nil, fault(400, "amountCents must be positive", nil)
	case by == "":
		return nil, fault(401, "the reconciling identity is required", nil)
	}
	if max := depositMaxCents(); cents > max {
		return nil, fault(400, fmt.Sprintf("amountCents %d exceeds maximum of %d", cents, max), nil)
	}
	p, found, err := pendingintent.Get(reference)
	if err != nil {
		return nil, fault(503, "billing is temporarily unavailable; please retry", err)
	}
	if !found || p.Purpose != pendingintent.Wire {
		return nil, fault(404, "no wire was given that reference", nil)
	}
	org, err := intentOrg(ctx, p.Org, p.Test)
	if err != nil {
		return nil, fault(503, "the wire's org could not be read", err)
	}
	claim, replay, err := idemBegin(webhookGuardDB(ctx), "billing-wire", reference)
	if err != nil {
		return nil, fault(503, "billing is temporarily unavailable; please retry", err)
	}
	if replay {
		if claim != nil && claim.Status == idempotencykey.StatusCompleted {
			if claim.Response != fmt.Sprint(cents) {
				return nil, fault(409, "this wire was credited for another amount", nil)
			}
			return &WireCredit{Reference: reference, Subject: p.Subject, AmountCents: cents}, nil
		}
		return nil, fault(409, "this wire is being reconciled", nil)
	}
	if err := bankAt(ctx, org, "", p.Subject, currency.Type("usd"), receipt{TransactionId: reference, ProcessorRef: "wire:" + reference, AmountCents: cents}, "Top-up via wire"); err != nil {
		_ = claim.Delete() // nothing was credited: a retry posts it
		log.Error("RECONCILE: wire %s was received and the balance was NOT credited: %v", reference, err)
		return nil, fault(503, "the wire was received and the balance is still being updated; repeat the request to finish", err)
	}
	_ = idempotencykey.Complete(claim, fmt.Sprint(cents))
	_, _ = pendingintent.Settle(reference)
	if _, eerr := engine.EmitBillingEvent(webhookGuardDB(ctx), "wire.reconciled", "wire", reference, p.Subject,
		map[string]interface{}{"amountCents": cents, "bankReference": bankRef, "by": by, "org": p.Org}, nil); eerr != nil {
		log.Warn("wire %s: the reconcile was not recorded: %v", reference, eerr)
	}
	return &WireCredit{Reference: reference, Subject: p.Subject, AmountCents: cents, Credited: true}, nil
}

type wireReconcileRequest struct {
	Reference     string `json:"reference"`
	AmountCents   int64  `json:"amountCents"`
	BankReference string `json:"bankReference"`
}

// ReconcileWireHandler credits a wire the bank received (platform admin only).
//
//	POST /v1/billing/wire/reconcile   { reference, amountCents, bankReference }
func ReconcileWireHandler(c *zip.Ctx) error {
	var req wireReconcileRequest
	if err := c.Bind(&req); err != nil {
		return jsonhttp.Fail(c, http.StatusBadRequest, "invalid request body", err)
	}
	out, f := ReconcileWire(c.Context(), req.Reference, req.AmountCents, strings.TrimSpace(req.BankReference), middleware.Actor(c))
	if f != nil {
		return jsonhttp.Fail(c, f.Status, f.Message, f.Err)
	}
	return c.JSON(200, out)
}
