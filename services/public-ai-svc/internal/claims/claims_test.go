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

// Doc 03 §08: CONDITIONAL and TARGET claims are never verified facts, but
// they are returned as qualified with their governed label; an ACTIVE claim
// for the same request wins.
func TestConditionalAndTargetClaimsAreQualified(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	target := completePriceClaim()
	target.ClaimID, target.Status = "c-target", StatusTarget
	r := NewResolver(Manifest{Claims: []Claim{target}})
	q := Request{Subject: target.ClaimType, SubjectID: target.SubjectID, Market: "US", AsOf: now}
	d := r.Resolve(q, nil)
	if d.Decision != InsufficientEvidence || d.ReasonCode != ReasonStatusQualified || len(d.Qualified) != 1 || len(d.Verified) != 0 {
		t.Fatalf("target: %+v", d)
	}
	if StatusLabelFor(d.Qualified[0]) != "Planned" {
		t.Fatalf("label %q", StatusLabelFor(d.Qualified[0]))
	}
	cond := target
	cond.Status = StatusConditional
	if l := StatusLabelFor(cond); l != "Limited" {
		t.Fatalf("conditional label %q", l)
	}
	active := completePriceClaim()
	d = NewResolver(Manifest{Claims: []Claim{target, active}}).Resolve(q, nil)
	if d.Decision != Verified || len(d.Qualified) != 0 {
		t.Fatalf("an ACTIVE claim must win: %+v", d)
	}
	held := target
	held.Status = StatusHold
	if d := NewResolver(Manifest{Claims: []Claim{held}}).Resolve(q, nil); len(d.Qualified) != 0 {
		t.Fatal("a HOLD claim is never shown")
	}
}

// Doc 05 §17: currency narrows the request; a verified decision says when
// it expires; a miss names the missing key; the release signature is carried.
func TestDecisionFields(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	c := completePriceClaim()
	due := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	c.ReviewDueAt = &due
	r := NewResolver(Manifest{ReleaseID: "claims-1", Claims: []Claim{c}}).WithSignatureRef("ed25519:abcd").WithDisputes(nil)
	d := r.Resolve(Request{Subject: Price, SubjectID: "PLAN", Market: "US", Currency: "xts", AsOf: now}, nil)
	if d.Decision != Verified || d.ExpiresAt == nil || !d.ExpiresAt.Equal(due) || d.SignatureRef != "ed25519:abcd" || len(d.MissingClaimKeys) != 0 {
		t.Fatalf("verified: %+v", d)
	}
	d = r.Resolve(Request{Subject: Price, SubjectID: "PLAN", Market: "US", Currency: "EUR", AsOf: now}, nil)
	if d.Decision == Verified || len(d.MissingClaimKeys) != 1 || d.MissingClaimKeys[0] != "PRICE|PLAN|US||EUR" {
		t.Fatalf("other currency: %+v", d)
	}
}
