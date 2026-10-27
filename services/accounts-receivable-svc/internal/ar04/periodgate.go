package ar04

import (
	"context"
	"encoding/json"
	"net/url"
	"time"

	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/svcclient"
)

// ModuleScope is the module scope AR asks accounting-period-svc to evaluate the gate for.
const ModuleScope = "AR"

// PeriodGate answers whether the accounting period covering a date accepts an AR posting, and its
// period key (carried onto the accounting event). A closed, locked or missing period is PERIOD_CLOSED;
// "cannot answer" is a 503 and the invoice stays unissued (fail closed). Implemented by
// HTTPPeriodGate over accounting-period-svc.
type PeriodGate interface {
	PostingPeriod(ctx context.Context, c kit.Caller, legalEntityID string, date time.Time) (periodKey string, err error)
}

// HTTPPeriodGate is the accounting-period-svc ResolvePeriodByDate client
// (GET /v1/accounting-periods:resolve, purpose=post).
type HTTPPeriodGate struct{ c *svcclient.Client }

func NewHTTPPeriodGate(baseURL string) *HTTPPeriodGate {
	return &HTTPPeriodGate{c: svcclient.New(baseURL)}
}

type periodResolution struct {
	PeriodKey      string `json:"period_key"`
	State          string `json:"state"`
	PostingAllowed bool   `json:"posting_allowed"`
	Reason         string `json:"reason"`
}

func (g *HTTPPeriodGate) PostingPeriod(ctx context.Context, c kit.Caller, legalEntityID string, date time.Time) (string, error) {
	q := url.Values{}
	q.Set("legal_entity_id", legalEntityID)
	q.Set("date", day(date))
	q.Set("module_scope", ModuleScope)
	q.Set("purpose", "post")
	status, body, err := g.c.Do(ctx, c.SvcCaller(), "GET", "/v1/accounting-periods:resolve?"+q.Encode(), nil)
	if err != nil {
		return "", kit.Unavailable("accounting period service unavailable: the issue date's period cannot be verified")
	}
	switch status {
	case 200:
		var r periodResolution
		if json.Unmarshal(body, &r) != nil {
			return "", kit.Unavailable("accounting period service returned an unreadable answer")
		}
		if !r.PostingAllowed {
			msg := "the accounting period for the issue date does not accept postings"
			if r.State != "" {
				msg += " (state " + r.State + ")"
			}
			return "", kit.Conflict(kit.CodePeriodClosed, msg)
		}
		return r.PeriodKey, nil
	case 404, 409:
		// No period covers the date, or several disagree: posting is not allowed (the gate never defaults to OPEN).
		return "", kit.Conflict(kit.CodePeriodClosed, "no accounting period accepts a posting on the issue date")
	default:
		return "", kit.Unavailable("accounting period service unavailable: the issue date's period cannot be verified")
	}
}
