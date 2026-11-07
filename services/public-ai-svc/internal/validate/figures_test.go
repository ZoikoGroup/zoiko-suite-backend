package validate

import (
	"errors"
	"testing"

	"zoiko.io/public-ai-svc/internal/model"
)

// Doc 03 §13: numbers are matched as whole tokens, not substrings.
// Doc 03 §17: a price or trial length needs a claim card that states it;
// evidence text alone never grounds one.
func TestFiguresAreWholeTokensAndP1NeedsAClaim(t *testing.T) {
	evPrice := []model.Evidence{{Ref: "e1", Title: "Pricing", Text: "Growth costs $49 per user. A 14-day trial is offered. Plans support 3 approval levels, 3 entities and 1,000 records."}}
	price := []model.ClaimCard{{Ref: "c1", Statement: "The Growth plan is listed at XTS 49.00 per user per month."}}
	trial := []model.ClaimCard{{Ref: "c2", Statement: "A 30-day self-serve trial is available."}}
	cases := []struct {
		name   string
		text   string
		claims []model.ClaimCard
		want   error
	}{
		{"substring of a grounded number", "Plans support 4 approval levels.", nil, ErrUnsupportedFact},
		{"whole grounded number", "Plans support 3 approval levels.", nil, nil},
		{"thousands separator", "Plans support 1000 records.", nil, nil},
		{"currency from evidence only", "Growth costs $49 per user.", nil, ErrUnsupportedFact},
		{"currency code from evidence only", "Growth costs USD 49.", nil, ErrUnsupportedFact},
		{"trailing currency code", "Growth costs 49 EUR.", nil, ErrUnsupportedFact},
		{"trial from evidence only", "A 14-day trial is offered.", nil, ErrUnsupportedFact},
		{"trial days phrasing", "There is a 14 days trial.", nil, ErrUnsupportedFact},
		{"price stated by claim", "The Growth plan is listed at XTS 49.00 per user per month.", price, nil},
		{"price with equivalent figure", "It is XTS 49 per user per month.", price, nil},
		{"price card does not ground another amount", "It is XTS 4 per user.", price, ErrUnsupportedFact},
		{"trial stated by claim", "A 30-day trial is available.", trial, nil},
		{"trial card does not ground another length", "A 14-day trial is available.", trial, ErrUnsupportedFact},
		// Doc 03 §17: user/entity capacity only from a signed entitlement claim,
		// never from article prose.
		{"capacity from prose", "Plans support 3 approval levels and 3 entities.", nil, ErrUnsupportedFact},
		{"capacity from a claim", "Growth includes 3 entities.", []model.ClaimCard{{Ref: "c-ent", Statement: "SYNTHETIC: Growth includes 3 entities."}}, nil},
	}
	for _, c := range cases {
		d := model.Draft{Text: c.text, EvidenceRefs: []string{"e1"}}
		for _, cl := range c.claims {
			d.ClaimRefs = append(d.ClaimRefs, cl.Ref)
		}
		if _, err := Check(d, c.claims, evPrice); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v want %v", c.name, err, c.want)
		}
	}
}
