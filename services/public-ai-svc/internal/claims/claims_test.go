package claims

import (
	"testing"
	"time"
)

func boolPtr(v bool) *bool { return &v }

func completePriceClaim() Claim {
	return Claim{
		ClaimID: "c-price", ClaimType: Price, SubjectID: "PLAN", Plan: "PLAN",
		Market: "US", Currency: "XTS", BillingTerm: "monthly",
		AuthorityRef: "PRICE-BOOK", TaxesFeesCaveat: "Before tax.",
		PublicStatement: "The plan costs XTS 10 per month.", Status: StatusActive,
		OperationallyEnabled: true, PubliclyPublishable: true,
		ValidFrom:                time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ValidUntil:               time.Date(2099, 12, 31, 0, 0, 0, 0, time.UTC),
		EvidenceSourceVersionIDs: []string{"src-pricing-v1"},
		CitationURLKey:           "pricing",
	}
}

func completeTrialClaim() Claim {
	return Claim{
		ClaimID: "c-trial", ClaimType: Trial, SubjectID: "PLAN", Plan: "PLAN",
		Market: "US", TrialCohort: "self-serve", TrialDurationDays: 30,
		TrialCardRequired: boolPtr(false), TrialEndState: "convert_or_expire",
		TrialAuthorizationPolicy: "standard",
		PublicStatement:          "A 30-day self-serve trial is available.", Status: StatusActive,
		OperationallyEnabled: true, PubliclyPublishable: true,
		ValidFrom:                time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ValidUntil:               time.Date(2099, 12, 31, 0, 0, 0, 0, time.UTC),
		EvidenceSourceVersionIDs: []string{"src-pricing-v1"},
		CitationURLKey:           "pricing",
	}
}

func TestPriceClaimCompleteness(t *testing.T) {
	c := completePriceClaim()
	if !c.Complete() {
		t.Fatal("complete price claim reported incomplete")
	}
	for name, mutate := range map[string]func(*Claim){
		"plan":           func(c *Claim) { c.Plan = "" },
		"currency":       func(c *Claim) { c.Currency = "" },
		"billing term":   func(c *Claim) { c.BillingTerm = "" },
		"market":         func(c *Claim) { c.Market = "" },
		"effective date": func(c *Claim) { c.ValidFrom = time.Time{} },
		"authority":      func(c *Claim) { c.AuthorityRef = ""; c.PriceBookRelease = "" },
		"tax caveat":     func(c *Claim) { c.TaxesFeesCaveat = "" },
	} {
		bad := c
		mutate(&bad)
		if bad.Complete() {
			t.Errorf("price claim without %s reported complete", name)
		}
	}
}

func TestTrialClaimCompleteness(t *testing.T) {
	c := completeTrialClaim()
	if !c.Complete() {
		t.Fatal("complete trial claim reported incomplete")
	}
	for name, mutate := range map[string]func(*Claim){
		"plan":                 func(c *Claim) { c.Plan = "" },
		"market":               func(c *Claim) { c.Market = "" },
		"cohort":               func(c *Claim) { c.TrialCohort = "" },
		"duration":             func(c *Claim) { c.TrialDurationDays = 0 },
		"card requirement":     func(c *Claim) { c.TrialCardRequired = nil },
		"end state":            func(c *Claim) { c.TrialEndState = "" },
		"authorization policy": func(c *Claim) { c.TrialAuthorizationPolicy = "" },
	} {
		bad := c
		mutate(&bad)
		if bad.Complete() {
			t.Errorf("trial claim without %s reported complete", name)
		}
	}
}

func TestResolveRejectsIncompleteHighImpactClaims(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name   string
		claim  Claim
		verify bool
	}{
		{name: "complete price", claim: completePriceClaim(), verify: true},
		{name: "incomplete price", claim: func() Claim { c := completePriceClaim(); c.Currency = ""; return c }()},
		{name: "complete trial", claim: completeTrialClaim(), verify: true},
		{name: "incomplete trial", claim: func() Claim { c := completeTrialClaim(); c.TrialDurationDays = 0; return c }()},
		{name: "synthetic incomplete trial", claim: func() Claim {
			c := completeTrialClaim()
			c.TrialDurationDays = 0
			c.Synthetic = true
			return c
		}(), verify: true},
		{name: "synthetic price with a blank", claim: func() Claim {
			c := completePriceClaim()
			c.Synthetic = true
			c.PublicStatement = "SYNTHETIC: The plan is listed at [price] per month."
			return c
		}()},
		{name: "synthetic trial with a blank", claim: func() Claim {
			c := completeTrialClaim()
			c.Synthetic = true
			c.PublicStatement = "SYNTHETIC: A trial of [N] days is available."
			return c
		}()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := NewResolver(Manifest{Claims: []Claim{tt.claim}})
			d := r.Resolve(Request{Subject: tt.claim.ClaimType, SubjectID: tt.claim.SubjectID, Market: "US", AsOf: now}, nil)
			if tt.verify {
				if d.Decision != Verified || len(d.Verified) != 1 {
					t.Fatalf("got %s %v", d.Decision, d.Verified)
				}
				return
			}
			if d.Decision != InsufficientEvidence || d.ReasonCode != ReasonIncomplete {
				t.Fatalf("got %s %q", d.Decision, d.ReasonCode)
			}
		})
	}
}
