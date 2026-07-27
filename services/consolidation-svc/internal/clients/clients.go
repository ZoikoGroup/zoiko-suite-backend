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

type IntercompanyEntry struct {
	IntercompanyEntryID string  `json:"intercompany_entry_id"`
	SourceLegalEntityID string  `json:"source_legal_entity_id"`
	TargetLegalEntityID string  `json:"target_legal_entity_id"`
	Amount              float64 `json:"amount"`
	MatchStatus         string  `json:"match_status"`
}

func (c *Clients) FetchMatchedIntercompanyEntries(ctx context.Context, tenantID string) ([]IntercompanyEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.intercompanyURL+"/v1/intercompany/entries", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Tenant-Id", tenantID)

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