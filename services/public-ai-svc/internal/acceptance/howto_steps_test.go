package acceptance

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"zoiko.io/public-ai-svc/internal/contract"
)

var stepLineRe = regexp.MustCompile(`(?m)^(\d+)\. \S`)

// How-to questions about the console are answered as numbered steps that
// start from signing in, name the menu and page, and end with the button to
// press, one step per line, never as one long paragraph.
func TestHowToAnswersAreSteps(t *testing.T) {
	x := publicKBServer(t)
	for q, page := range map[string]string{
		"use of Register a jurisdiction":          "**Jurisdictions & Rules**",
		"how do I register a jurisdiction":        "**Jurisdictions & Rules**",
		"Set the limit":                           "**Policies**",
		"use of the create rule":                  "**Policies**",
		"how do I raise an obligation":            "**Obligations**",
		"how do I start a workflow":               "**Workflows & Approvals**",
		"how do I grant a role":                   "**Authorization**",
		"how do I add a requirement":              "**Evidence**",
		"how do I record a journal":               "**Finance & Payments**",
		"how do I issue a purchase order":         "**Commercial Ops**",
		"how do I raise a purchase request":       "**Purchase Requests**",
		"how do I delegate my approval authority": "**Delegated Authority**",
		"how do I provision a tenant":             "**Tenants & Entities**",
		"how do I draft a contract":               "**Legal & Contracts**",
		"how do I send a notification":            "**Notifications**",
	} {
		r := x.env.Pipeline.Answer(ctx, inputFor(q))
		if r.Status != contract.Answered {
			t.Errorf("%q: %s %s", q, r.Status, r.Reason)
			continue
		}
		steps := stepLineRe.FindAllStringSubmatch(r.Text, -1)
		if len(steps) < 3 {
			t.Errorf("%q: want numbered steps, got %q", q, r.Text)
			continue
		}
		for i, s := range steps {
			if s[1] != strconv.Itoa(i+1) {
				t.Errorf("%q: steps out of order: %q", q, r.Text)
				break
			}
		}
		if !strings.Contains(r.Text, "\n1. Sign in to your ZoikoSuite account.") {
			t.Errorf("%q: first step is not signing in: %q", q, r.Text)
		}
		if !strings.Contains(r.Text, page) {
			t.Errorf("%q: does not name the page %s: %q", q, page, r.Text)
		}
		if strings.Contains(r.Text, "§") {
			t.Errorf("%q: heading marker in answer: %q", q, r.Text)
		}
	}
}
