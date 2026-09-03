package clients

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"go.uber.org/zap"
	"zoiko.io/consolidation-svc/internal/domain"
)

type Clients struct {
	ledgerURL       string
	intercompanyURL string
	http            *http.Client
	log             *zap.Logger
}

func New(ledgerURL, intercompanyURL string, log *zap.Logger) *Clients {
	return &Clients{
		ledgerURL:       ledgerURL,
		intercompanyURL: intercompanyURL,
		http:            &http.Client{Timeout: 5 * time.Second},
		log:             log,
	}
}

type glJournal struct {
	JournalID string `json:"journal_id"`
	Status    string `json:"status"`
}

type glJournalLine struct {
	AccountCode  string  `json:"account_code"`
	DebitAmount  float64 `json:"debit_amount"`
	CreditAmount float64 `json:"credit_amount"`
}

type glJournalWithLines struct {
	Lines []glJournalLine `json:"lines"`
}

func (c *Clients) FetchTrialBalance(ctx context.Context, tenantID, legalEntityID, fiscalPeriod string) (map[string]float64, error) {
	u, err := url.Parse(c.ledgerURL + "/v1/journals")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("tenant_id", tenantID)
	q.Set("legal_entity_id", legalEntityID)
	q.Set("fiscal_period", fiscalPeriod)
	q.Set("status", "FINALIZED")
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("failed to query general-ledger-svc", zap.Error(err))
		return nil, domain.ErrGLServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, domain.ErrGLServiceUnavailable
	}

	var list []glJournal
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, err
	}

	balances := make(map[string]float64)
	for _, j := range list {
		lines, err := c.getJournalLines(ctx, tenantID, j.JournalID)
		if err != nil {
			return nil, err
		}
		for _, line := range lines {
			net := line.DebitAmount - line.CreditAmount
			balances[line.AccountCode] += net
		}
	}
	return balances, nil
}

func (c *Clients) getJournalLines(ctx context.Context, tenantID, journalID string) ([]glJournalLine, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/v1/journals/%s", c.ledgerURL, journalID), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, domain.ErrGLServiceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, domain.ErrGLServiceUnavailable
	}

	var detail glJournalWithLines
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		return nil, err
	}
	return detail.Lines, nil
}

// JournalLine is the exported shape of one posted journal line — used by
// the elimination path (see FetchJournalLines) as well as internally by
// FetchTrialBalance's own glJournalLine.
type JournalLine struct {
	AccountCode  string  `json:"account_code"`
	DebitAmount  float64 `json:"debit_amount"`
	CreditAmount float64 `json:"credit_amount"`
}

// FetchJournalLines returns the real posted lines of one journal from
// general-ledger-svc — the authoritative source ACC-12 elimination must
// read from, rather than any invented per-service mapping of intercompany
// amounts to elimination accounts (no such mapping exists anywhere on this
// platform; IntercompanyEntry itself carries no account_code).
func (c *Clients) FetchJournalLines(ctx context.Context, tenantID, journalID string) ([]JournalLine, error) {
	lines, err := c.getJournalLines(ctx, tenantID, journalID)
	if err != nil {
		return nil, err
	}
	out := make([]JournalLine, 0, len(lines))
	for _, l := range lines {
		out = append(out, JournalLine{AccountCode: l.AccountCode, DebitAmount: l.DebitAmount, CreditAmount: l.CreditAmount})
	}
	return out, nil
}

type IntercompanyEntry struct {
	IntercompanyEntryID string  `json:"intercompany_entry_id"`
	SourceLegalEntityID string  `json:"source_legal_entity_id"`
	TargetLegalEntityID string  `json:"target_legal_entity_id"`
	SourceJournalID     string  `json:"source_journal_id"`
	TargetJournalID     *string `json:"target_journal_id,omitempty"`
	Amount              float64 `json:"amount"`
	MatchStatus         string  `json:"match_status"`
}

// FetchMatchedIntercompanyEntries returns every MATCHED intercompany entry
// for tenantID. principalID is forwarded as X-Principal-Id — intercompany-
// accounting-svc's ListEntries requires a verified principal
// (internal/handler/handler.go's requirePrincipal), which this call
// previously never sent, so every prior invocation of this method failed
// with 401 before a single entry was ever read (see
// master-register-findings-2026-08-27.md §3.29). principalID is the same
// principal already authorized for CONSOLIDATION_RUN_INITIATE on the
// calling request — reusing it here, rather than inventing a separate
// system identity that doesn't exist anywhere on this platform.
func (c *Clients) FetchMatchedIntercompanyEntries(ctx context.Context, tenantID, principalID string) ([]IntercompanyEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.intercompanyURL+"/v1/intercompany/entries", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("failed to query intercompany-accounting-svc", zap.Error(err))
		return nil, domain.ErrIntercompanyUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, domain.ErrIntercompanyUnavailable
	}

	var list []IntercompanyEntry
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, err
	}

	var matched []IntercompanyEntry
	for _, entry := range list {
		if entry.MatchStatus == "MATCHED" {
			matched = append(matched, entry)
		}
	}
	return matched, nil
}
