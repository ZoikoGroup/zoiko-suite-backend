package arctl

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/svcclient"
)

// HTTPGLBalance reads the AR control-account balance from general-ledger-svc:
//
//  1. GET /v1/account-mappings/{mapping_key}   -> account_code the key resolves to (AR_RECEIVABLE_STANDARD)
//  2. GET /v1/control-populations/account-postings?legal_entity_id&period_id&account_codes&normal_balance=DEBIT&limit=1
//     -> declared_totals.totals: the whole-set cumulative DEBIT-normal balance per currency at the period end, plus the
//     population watermark. (/v1/ledger/balance is a single-period movement, not a cumulative balance, so it cannot tie out.)
//
// Any failure (transport, non-200, unreadable body, missing watermark, unknown mapping) is returned as an UNAVAILABLE read:
// the run is then UNVERIFIED, never a pass.
type HTTPGLBalance struct{ c *svcclient.Client }

// NewHTTPGLBalance builds the client for cfg.LedgerServiceURL.
func NewHTTPGLBalance(baseURL string) *HTTPGLBalance {
	return &HTTPGLBalance{c: svcclient.New(baseURL)}
}

type glMapping struct {
	MappingKey  string `json:"mapping_key"`
	AccountCode string `json:"account_code"`
}

type glPopulation struct {
	NextCursor     string `json:"next_cursor"`
	Watermark      string `json:"watermark"`
	DeclaredTotals *struct {
		RowCount int64             `json:"row_count"`
		Totals   map[string]string `json:"totals"`
	} `json:"declared_totals"`
}

// ControlAccountBalance implements GLBalanceSource. It never returns a Go error: an unreadable GL is data (Available=false).
func (g *HTTPGLBalance) ControlAccountBalance(ctx context.Context, c svcclient.Caller, legalEntityID, periodID string, mappingKeys []string) (*GLRead, error) {
	out := &GLRead{MappingKeys: mappingKeys, PeriodID: periodID, Totals: map[string]kit.Amount{}}
	fail := func(format string, a ...any) (*GLRead, error) {
		out.Available, out.Error = false, fmt.Sprintf(format, a...)
		return out, nil
	}
	codes := make([]string, 0, len(mappingKeys))
	for _, key := range mappingKeys {
		status, body, err := g.c.Do(ctx, c, "GET", "/v1/account-mappings/"+url.PathEscape(key), nil)
		if err != nil {
			return fail("general ledger unreachable resolving %s", key)
		}
		if status != 200 {
			return fail("general ledger answered HTTP %d resolving mapping %s", status, key)
		}
		var m glMapping
		if json.Unmarshal(body, &m) != nil || strings.TrimSpace(m.AccountCode) == "" {
			return fail("mapping %s did not resolve to an account code", key)
		}
		codes = append(codes, m.AccountCode)
	}
	out.AccountCodes = codes
	q := url.Values{}
	q.Set("legal_entity_id", legalEntityID)
	q.Set("period_id", periodID)
	q.Set("account_codes", strings.Join(codes, ","))
	q.Set("normal_balance", "DEBIT")
	q.Set("limit", "1")
	status, body, err := g.c.Do(ctx, c, "GET", "/v1/control-populations/account-postings?"+q.Encode(), nil)
	if err != nil {
		return fail("general ledger unreachable reading the control account")
	}
	if status != 200 {
		return fail("general ledger answered HTTP %d reading the control account", status)
	}
	var p glPopulation
	if json.Unmarshal(body, &p) != nil || p.DeclaredTotals == nil || strings.TrimSpace(p.Watermark) == "" {
		return fail("general ledger answered an unreadable or unwatermarked control-account population")
	}
	for cur, s := range p.DeclaredTotals.Totals {
		a, perr := kit.ParseAmount(s)
		if perr != nil {
			return fail("general ledger total for %s is not an exact decimal", cur)
		}
		out.Totals[cur] = a
	}
	out.Watermark, out.Available = p.Watermark, true
	return out, nil
}
