package xicconn_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
)

type fakeGate struct {
	calls      int
	budgetFree []bool
	ops        []string
	err        error
}

func (g *fakeGate) Admit(ctx context.Context, c *xicconn.Connection, op string, _ time.Time) error {
	g.calls++
	g.ops = append(g.ops, op)
	g.budgetFree = append(g.budgetFree, xicconn.BudgetFree(ctx))
	return g.err
}

func TestStageGate_AppliesToProductionOnlyAndLast(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	g := &fakeGate{}
	e.svc.Stage = g
	sb := e.sandbox(tenantA, v, "acc-sb", refOAuth, refHMAC)
	if _, err := e.svc.Operable(ctxFor(tenantA), tenantA, sb.ConnectionID, "payment-submit"); err != nil || g.calls != 0 {
		t.Fatalf("a sandbox connection is not staged: %v calls=%d", err, g.calls)
	}
	prod := e.production(tenantA, v, "acc-prod", refOAuth2, refHMAC2)
	if _, err := e.svc.Operable(ctxFor(tenantA), tenantA, prod.ConnectionID, "payment-submit"); err != nil || g.calls != 1 || g.ops[0] != "payment-submit" || g.budgetFree[0] {
		t.Fatalf("%v %+v", err, g)
	}
	// a re-check carries the marker that takes nothing from the pilot budget
	if _, err := e.svc.Operable(xicconn.WithoutBudget(ctxFor(tenantA)), tenantA, prod.ConnectionID, "payment-submit"); err != nil || !g.budgetFree[1] {
		t.Fatalf("%v %+v", err, g)
	}
	// the gate's refusal is the answer, with its own code
	g.err = &xicconn.Refusal{Code: xicconn.CodeRateLimited, Detail: "budget", ConnectionID: prod.ConnectionID}
	_, err := e.svc.Operable(ctxFor(tenantA), tenantA, prod.ConnectionID, "payment-submit")
	refusal(t, err, xicconn.CodeRateLimited)
	// a gate that cannot read its state fails closed
	g.err = errors.New("db down")
	if _, err := e.svc.Operable(ctxFor(tenantA), tenantA, prod.ConnectionID, "payment-submit"); err == nil {
		t.Fatal("an unreadable stage must not admit")
	}
	// a refused earlier gate never reaches the stage: another tenant's connection takes nothing
	before := g.calls
	g.err = nil
	if _, err := e.svc.Operable(ctxFor(tenantB), tenantB, prod.ConnectionID, "payment-submit"); err == nil || g.calls != before {
		t.Fatalf("%v calls %d -> %d", err, before, g.calls)
	}
	if _, err := e.svc.Operable(ctxFor(tenantA), tenantA, prod.ConnectionID, "no-such-op"); err == nil || g.calls != before {
		t.Fatalf("an operation the contract does not have takes nothing: %v", err)
	}
}

func TestCertificationStatus(t *testing.T) {
	e := newEnv(t)
	v := e.publish("bank-x", "1.0.0", nil)
	ctx := ctxFor(tenantA)
	sb := e.sandbox(tenantA, v, "acc-sb", refOAuth, refHMAC)
	st, err := e.svc.CertificationStatus(ctx, sb.ConnectionID)
	if err != nil || st.State != xicconn.CertNotRequired || st.RetestDue {
		t.Fatalf("%+v %v", st, err)
	}
	prod := e.production(tenantA, v, "acc-prod", refOAuth2, refHMAC2)
	st, err = e.svc.CertificationStatus(ctx, prod.ConnectionID)
	if err != nil || st.State != xicconn.CertValid || st.RetestDue || st.DaysRemaining == nil || *st.DaysRemaining < 100 || st.Certification == nil {
		t.Fatalf("%+v %v", st, err)
	}
	// inside the warning window a retest is due before production is blocked
	cur := e.get(tenantA, prod)
	exp := e.at().Add(400 * 24 * time.Hour)
	e.vault.set(refOAuth3, nil)
	if _, err := e.svc.Rotate(ctx, operator, cur.ConnectionID, cur.Version, xicconn.RotateInput{AuthProfile: "oauth", NewRef: refOAuth3, ExpiresAt: &exp}); err != nil {
		t.Fatal(err)
	}
	e.advance(170 * 24 * time.Hour)
	st, err = e.svc.CertificationStatus(ctx, prod.ConnectionID)
	if err != nil || st.State != xicconn.CertValid || !st.RetestDue || len(st.Reasons) != 1 || st.Reasons[0] != "EXPIRING" {
		t.Fatalf("%+v %v", st, err)
	}
	e.advance(20 * 24 * time.Hour)
	st, err = e.svc.CertificationStatus(ctx, prod.ConnectionID)
	if err != nil || st.State != xicconn.CertExpired || !st.RetestDue || st.Reasons[0] != "EXPIRED" {
		t.Fatalf("%+v %v", st, err)
	}
	if _, err := e.svc.CertificationStatus(ctxFor(tenantB), prod.ConnectionID); !errors.Is(err, xicconn.ErrNotFound) {
		t.Fatalf("another tenant reads the status: %v", err)
	}
}
