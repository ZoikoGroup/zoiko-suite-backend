package clients

import (
	"bytes"
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

// JournalLine is the line shape a consolidation adjustment posts to
// general-ledger-svc (PostConsolidationAdjustmentJournal).
type JournalLine struct {
	AccountCode  string  `json:"account_code"`
	DebitAmount  float64 `json:"debit_amount"`
	CreditAmount float64 `json:"credit_amount"`
}

// FetchJournal returns one journal from general-ledger-svc, header and
// posted lines — the authoritative source ACC-12 elimination reads. The
// header is what lets a run check that a leg is a FINALIZED journal of its
// own entity in the run's period, i.e. one whose lines FetchTrialBalance
// summed (SPEC_DEVIATIONS A2/B9).
func (c *Clients) FetchJournal(ctx context.Context, tenantID, journalID string) (*domain.PostedJournal, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/v1/journals/%s", c.ledgerURL, url.PathEscape(journalID)), nil)
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
		return nil, fmt.Errorf("%w: general-ledger-svc returned status %d for journal %s", domain.ErrGLServiceUnavailable, resp.StatusCode, journalID)
	}

	var j domain.PostedJournal
	if err := json.NewDecoder(resp.Body).Decode(&j); err != nil {
		return nil, err
	}
	return &j, nil
}

// IntercompanyEntry is the part of intercompany-accounting-svc's pair this
// service reads (domain.IntercompanyPair). Amounts are exact decimal text
// (ZS-DATA-001 D06), never float64; see domain.Decimal for why they are not
// plain strings.
type IntercompanyEntry = domain.IntercompanyPair

// Intercompany pair statuses this service eliminates. CLOSED is
// intercompany-accounting-svc's terminal state for a pair that was MATCHED
// (its migration 000006, SPEC_DEVIATIONS A9/B8): reachable only from
// MATCHED, so its reciprocal balance is as settled as a MATCHED one's.
// Leaving it out would make a closed pair look never-matched, and a re-run
// for its period would leave it uneliminated.
const (
	intercompanyStatusMatched = "MATCHED"
	intercompanyStatusClosed  = "CLOSED"
)

// FetchEliminableIntercompanyEntries returns every intercompany entry for
// tenantID whose reciprocal balance is eliminable: MATCHED, or CLOSED
// (matched, then closed). Unmatched, mismatched, disputed and resolved
// entries are never returned. principalID is forwarded as X-Principal-Id — intercompany-
// accounting-svc's ListEntries requires a verified principal
// (internal/handler/handler.go's requirePrincipal), which this call
// previously never sent, so every prior invocation of this method failed
// with 401 before a single entry was ever read (see
// master-register-findings-2026-08-27.md §3.29). principalID is the same
// principal already authorized for CONSOLIDATION_RUN_INITIATE on the
// calling request — reusing it here, rather than inventing a separate
// system identity that doesn't exist anywhere on this platform.
func (c *Clients) FetchEliminableIntercompanyEntries(ctx context.Context, tenantID, principalID string) ([]IntercompanyEntry, error) {
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

	var eliminable []IntercompanyEntry
	for _, entry := range list {
		if entry.MatchStatus == intercompanyStatusMatched || entry.MatchStatus == intercompanyStatusClosed {
			eliminable = append(eliminable, entry)
		}
	}
	return eliminable, nil
}

// FetchReciprocalBalances reads intercompany-accounting-svc's
// GetReciprocalBalances for one entity pair and period. ACC-12's
// ExplainElimination returns this as the balance its MATCHED-or-CLOSED rule
// was checked against, rather than re-deriving the pair's positions from the
// entry list here (the aggregation is exact there: big.Rat sums, per-currency
// separation, never float64). principalID is forwarded as X-Principal-Id;
// that endpoint authorizes both entities.
func (c *Clients) FetchReciprocalBalances(ctx context.Context, tenantID, principalID, entityA, entityB, fiscalPeriod string) (*domain.ReciprocalBalances, error) {
	u, err := url.Parse(c.intercompanyURL + "/v1/intercompany/reciprocal-balances")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("entity_a", entityA)
	q.Set("entity_b", entityB)
	q.Set("period", fiscalPeriod)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("failed to query intercompany-accounting-svc reciprocal balances", zap.Error(err))
		return nil, domain.ErrIntercompanyUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		// The reciprocal-balances endpoint authorizes BOTH entities. A
		// denial is the caller's, not an outage: surface it as such.
		return nil, domain.ErrAuthorizationDenied
	}
	if resp.StatusCode != http.StatusOK {
		return nil, domain.ErrIntercompanyUnavailable
	}

	var balances domain.ReciprocalBalances
	if err := json.NewDecoder(resp.Body).Decode(&balances); err != nil {
		return nil, err
	}
	return &balances, nil
}

// glCall is one request to general-ledger-svc's group-book API. The caller is identified the way every internal call here is: tenant,
// principal and the legal entity the principal acts on.
func (c *Clients) glCall(ctx context.Context, method, path, tenantID, principalID, legalEntityID string, body any, out any) (int, error) {
	var rdr *bytes.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rdr = bytes.NewReader(payload)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.ledgerURL+path, rdr)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)
	req.Header.Set("X-Legal-Entity-Id", legalEntityID)
	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("general-ledger-svc group-book call failed", zap.String("path", path), zap.Error(err))
		return 0, domain.ErrGLServiceUnavailable
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode < 300 {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, err
		}
	}
	if resp.StatusCode >= 300 {
		var problem struct {
			Code string `json:"code"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&problem)
		if problem.Code == "ADJUSTMENT_APPLIED" {
			return resp.StatusCode, domain.ErrAdjustmentInGroupRun
		}
	}
	return resp.StatusCode, nil
}

type glGroup struct {
	GroupID        string `json:"group_id"`
	ParentEntityID string `json:"parent_entity_id"`
}

type glAdjustment struct {
	AdjustmentID string `json:"adjustment_id"`
	Status       string `json:"status"`
}

// PostConsolidationAdjustmentJournal submits an approved consolidation adjustment to general-ledger-svc's group book. It never reaches an
// entity ledger: general-ledger-svc keeps the adjustment as a group adjustment of the consolidation group whose reporting entity is
// legalEntityID, and a consolidation run there applies it, in the group book only. The adjustment id is the source reference, so a
// retried post is the same adjustment. The adjustment was approved here by approverPrincipalID, who is not its author, and the same
// separation is enforced there by approving it as that principal. The id it returns is general-ledger-svc's adjustment id.
func (c *Clients) PostConsolidationAdjustmentJournal(ctx context.Context, tenantID, principalID, approverPrincipalID, legalEntityID, fiscalPeriod, description, sourceEventID, correlationID string, lines []JournalLine) (adjustmentID string, err error) {
	var groups struct {
		Groups []glGroup `json:"groups"`
	}
	status, err := c.glCall(ctx, http.MethodGet, "/v1/consolidation-groups?parent_entity_id="+url.QueryEscape(legalEntityID), tenantID, principalID, legalEntityID, nil, &groups)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("%w: general-ledger-svc returned status %d listing the consolidation groups", domain.ErrGLServiceUnavailable, status)
	}
	if len(groups.Groups) != 1 {
		return "", fmt.Errorf("%w: %d consolidation groups of the kernel have %s as their reporting entity", domain.ErrNoGroupBook, len(groups.Groups), legalEntityID)
	}
	type line struct {
		AccountCode string `json:"account_code"`
		Amount      string `json:"amount"`
	}
	body := map[string]any{"fiscal_period": fiscalPeriod, "description": description, "policy_ref": "consolidation-svc adjustment", "evidence_ref": sourceEventID, "source_ref": sourceEventID}
	var ls []line
	for _, l := range lines {
		ls = append(ls, line{AccountCode: l.AccountCode, Amount: fmt.Sprintf("%.2f", l.DebitAmount-l.CreditAmount)})
	}
	body["lines"] = ls
	var adj glAdjustment
	status, err = c.glCall(ctx, http.MethodPost, "/v1/consolidation-groups/"+groups.Groups[0].GroupID+"/adjustments", tenantID, principalID, legalEntityID, body, &adj)
	if err != nil {
		return "", err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return "", fmt.Errorf("%w: general-ledger-svc returned status %d for the group adjustment", domain.ErrGLServiceUnavailable, status)
	}
	if adj.Status == "PROPOSED" {
		status, err = c.glCall(ctx, http.MethodPost, "/v1/consolidation-adjustments/"+adj.AdjustmentID+"/approve", tenantID, approverPrincipalID, legalEntityID, nil, &adj)
		if err != nil {
			return "", err
		}
		if status != http.StatusOK {
			return "", fmt.Errorf("%w: general-ledger-svc returned status %d approving the group adjustment", domain.ErrGLServiceUnavailable, status)
		}
	}
	return adj.AdjustmentID, nil
}

// ReverseConsolidationAdjustmentJournal withdraws the group adjustment general-ledger-svc holds for a posted adjustment. An adjustment a
// consolidation run there has already applied is not withdrawn behind that run's back: the run is superseded first.
func (c *Clients) ReverseConsolidationAdjustmentJournal(ctx context.Context, tenantID, principalID, legalEntityID, adjustmentID, reason string) error {
	status, err := c.glCall(ctx, http.MethodPost, "/v1/consolidation-adjustments/"+adjustmentID+"/withdraw", tenantID, principalID, legalEntityID, map[string]string{"reason": reason}, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("%w: general-ledger-svc returned status %d withdrawing the group adjustment", domain.ErrGLServiceUnavailable, status)
	}
	return nil
}
