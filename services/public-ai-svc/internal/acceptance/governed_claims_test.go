package acceptance

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"zoiko.io/public-ai-svc/internal/claims"
	"zoiko.io/public-ai-svc/internal/contract"
	"zoiko.io/public-ai-svc/internal/fixtureenv"
	"zoiko.io/public-ai-svc/internal/orchestrator"
	"zoiko.io/public-ai-svc/internal/release"
	"zoiko.io/public-ai-svc/internal/signing"
)

func fixtureEnvWithClaim(t *testing.T, claimID string, mutate func(*claims.Claim)) *fixtureenv.Env {
	t.Helper()
	env, err := fixtureenv.New(ctx, 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := env.Releases.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	m := snap.Claims.Manifest()
	m.ReleaseID = "claims-incomplete-1"
	for i := range m.Claims {
		if m.Claims[i].ClaimID == claimID {
			mutate(&m.Claims[i])
		}
	}
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := signing.Sign(env.PrivKey, body)
	if err != nil {
		t.Fatal(err)
	}
	if err := release.Publish(ctx, env.Store, env.PubKey, m.ReleaseID, claims.KindClaims, body, sig); err != nil {
		t.Fatal(err)
	}
	if err := release.Promote(ctx, env.Store, claims.KindClaims, m.ReleaseID, nil); err != nil {
		t.Fatal(err)
	}
	if err := env.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	return env
}

func TestIncompletePriceClaimRoutesToPricing(t *testing.T) {
	env := fixtureEnvWithClaim(t, "c-price-us-growth-m", func(c *claims.Claim) { c.Currency = "" })
	r := env.Pipeline.Answer(ctx, orchestrator.Input{
		RequestID: "req_incomplete", Message: "How much is the Growth plan per month in the United States?",
		MarketCode: "US", Locale: "en-US",
	})
	status(t, r, contract.Abstain)
	if route(r) != "pricing" || len(r.Citations) != 0 {
		t.Fatalf("route/citations: %q %v", route(r), r.Citations)
	}
	lacks(t, r, "XTS 49.00")
}

func TestIncompleteTrialClaimRoutesToPricing(t *testing.T) {
	env := fixtureEnvWithClaim(t, "c-trial-us-starter", func(c *claims.Claim) { c.TrialDurationDays = 0 })
	r := env.Pipeline.Answer(ctx, orchestrator.Input{
		RequestID: "req_incomplete", Message: "Is there a free trial of the Starter plan in the US?",
		MarketCode: "US", Locale: "en-US",
	})
	status(t, r, contract.Abstain)
	if route(r) != "pricing" || len(r.Citations) != 0 {
		t.Fatalf("route/citations: %q %v", route(r), r.Citations)
	}
	if !strings.Contains(r.Text, "depends on plan and market") {
		t.Fatalf("text: %q", r.Text)
	}
}
