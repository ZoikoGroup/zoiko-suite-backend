// Package ledger reads general-ledger-svc's posted ledger (ACC-05
// QueryLedger) to find the book side of an UNMATCHED bank statement line
// (SPEC_DEVIATIONS A2a): one cash account's FINALIZED entries in a date
// window around the statement date.
//
// Read-only by construction: there is no method here that calls a
// general-ledger-svc write route. A proposed journal is only ever a
// suggestion; the match is made in bank-reconciliation-svc, by a human.
//
// The client fails CLOSED. Entries that cannot be shown to be exactly what
// was asked for — another tenant, entity or account, outside the window, a
// truncated page, an amount that cannot be held exactly — are never
// returned, so no proposal is ever built on a partial or wrong set.
package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/reconciliation-intelligence-svc/internal/domain"
	svcenvelope "zoiko.io/reconciliation-intelligence-svc/internal/envelope"
)

var (
	// ErrServiceUnavailable: transport failure or a 5xx. An outage.
	ErrServiceUnavailable = errors.New("general-ledger-svc unavailable")
	// ErrRequestRefused: a 4xx — a defect in this request (a missing
	// envelope header, a filter it does not accept), not an outage.
	ErrRequestRefused = errors.New("general-ledger-svc refused the request")
	// ErrInconsistent: the answer does not describe what was asked for.
	ErrInconsistent = errors.New("general-ledger-svc returned inconsistent ledger entries")
	// ErrWindowTooLarge: QueryLedger has no cursor and caps a page at 1000
	// entries, so a full page cannot be told apart from a truncated one. A
	// partial read could miss the one journal that matches, and "no
	// candidate" would then be false.
	ErrWindowTooLarge = errors.New("cash account has too many ledger entries in the search window to read completely")
	// ErrScopeMissing: no verified tenant or principal to forward.
	ErrScopeMissing = errors.New("tenant and principal are required to read general-ledger-svc")
)

const (
	// maxEntries is general-ledger-svc's store.MaxListLimit. It must be asked
	// for exactly: a larger limit is silently replaced by its default (200).
	maxEntries       = 1000
	maxResponseBytes = 16 << 20
	dateLayout       = "2006-01-02"
)

// Scope is who a read is made on behalf of: the verified tenant and
// principal of the inbound request, so general-ledger-svc applies its own
// tenant isolation and GL_LEDGER_QUERY authorization to the caller.
type Scope struct {
	TenantID      string
	PrincipalID   string
	LegalEntityID string
}

type Client struct {
	baseURL    string
	httpClient *http.Client
	logger     *zap.Logger
}

func NewClient(baseURL string, logger *zap.Logger) *Client {
	return &Client{baseURL: baseURL, httpClient: &http.Client{Timeout: 10 * time.Second}, logger: logger}
}

// ledgerEntry is the part of general-ledger-svc's domain.LedgerEntry this
// service needs. Amounts are decoded as json.Number and parsed exactly.
type ledgerEntry struct {
	JournalID       string      `json:"journal_id"`
	TenantID        string      `json:"tenant_id"`
	LegalEntityID   string      `json:"legal_entity_id"`
	AccountCode     string      `json:"account_code"`
	DebitAmount     json.Number `json:"debit_amount"`
	CreditAmount    json.Number `json:"credit_amount"`
	CurrencyCode    string      `json:"currency_code"`
	TransactionDate string      `json:"transaction_date"`
}

// ListCashAccountEntries returns scope.LegalEntityID's FINALIZED ledger
// entries on accountCode with a transaction_date in [from, to] (YYYY-MM-DD,
// inclusive). One call per cash account per run, never per line.
func (c *Client) ListCashAccountEntries(ctx context.Context, scope Scope, accountCode, from, to string) ([]domain.LedgerEntry, error) {
	if scope.TenantID == "" || scope.PrincipalID == "" {
		return nil, ErrScopeMissing
	}
	fromDate, err := time.Parse(dateLayout, from)
	if err != nil {
		return nil, fmt.Errorf("window start %q: %w", from, err)
	}
	toDate, err := time.Parse(dateLayout, to)
	if err != nil {
		return nil, fmt.Errorf("window end %q: %w", to, err)
	}

	q := url.Values{
		"legal_entity_id":       {scope.LegalEntityID},
		"account_code":          {accountCode},
		"transaction_date_from": {from},
		"transaction_date_to":   {to},
		"journal_status":        {"FINALIZED"},
		"limit":                 {strconv.Itoa(maxEntries)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/ledger/entries?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrServiceUnavailable, err)
	}
	setEnvelopeHeaders(ctx, req, scope)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.logger.Error("general-ledger-svc unreachable — failing closed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", ErrServiceUnavailable, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode >= 500:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("%w: HTTP %d", ErrServiceUnavailable, resp.StatusCode)
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		c.logger.Error("general-ledger-svc refused the ledger read",
			zap.Int("status", resp.StatusCode), zap.ByteString("body", body))
		return nil, fmt.Errorf("%w: HTTP %d", ErrRequestRefused, resp.StatusCode)
	}

	dec := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes))
	dec.UseNumber()
	var raw []ledgerEntry
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("%w: unreadable body: %v", ErrInconsistent, err)
	}
	if len(raw) >= maxEntries {
		return nil, fmt.Errorf("%w: account %s, %s to %s: %d or more entries", ErrWindowTooLarge, accountCode, from, to, maxEntries)
	}

	out := make([]domain.LedgerEntry, 0, len(raw))
	for _, e := range raw {
		// Every row is checked against the scope asked for: a filter
		// silently ignored upstream would otherwise turn into proposals
		// about another entity's or account's journals.
		switch {
		case e.JournalID == "":
			return nil, fmt.Errorf("%w: entry without journal_id", ErrInconsistent)
		case e.TenantID != scope.TenantID || e.LegalEntityID != scope.LegalEntityID:
			return nil, fmt.Errorf("%w: journal %s is outside the requested tenant and legal entity", ErrInconsistent, e.JournalID)
		case e.AccountCode != accountCode:
			return nil, fmt.Errorf("%w: journal %s entry is on account %s, asked for %s", ErrInconsistent, e.JournalID, e.AccountCode, accountCode)
		}
		date, err := time.Parse(dateLayout, e.TransactionDate)
		if err != nil || date.Before(fromDate) || date.After(toDate) {
			return nil, fmt.Errorf("%w: journal %s transaction_date %q is outside %s to %s", ErrInconsistent, e.JournalID, e.TransactionDate, from, to)
		}
		debit, err := exactAmount(e.DebitAmount)
		if err != nil {
			return nil, fmt.Errorf("%w: journal %s debit_amount: %v", ErrInconsistent, e.JournalID, err)
		}
		credit, err := exactAmount(e.CreditAmount)
		if err != nil {
			return nil, fmt.Errorf("%w: journal %s credit_amount: %v", ErrInconsistent, e.JournalID, err)
		}
		out = append(out, domain.LedgerEntry{
			JournalID: e.JournalID, TenantID: e.TenantID, LegalEntityID: e.LegalEntityID,
			AccountCode: e.AccountCode, CurrencyCode: e.CurrencyCode, TransactionDate: e.TransactionDate,
			Debit: debit, Credit: credit,
		})
	}
	return out, nil
}

// exactAmount parses a ledger amount without going through float64. An
// absent amount is zero (a line carries one side only).
func exactAmount(n json.Number) (*big.Rat, error) {
	if n == "" {
		return new(big.Rat), nil
	}
	v, err := domain.ParseAmount(n.String())
	if err != nil {
		return nil, err
	}
	if v.Sign() < 0 {
		return nil, errors.New("negative")
	}
	return v, nil
}

// setEnvelopeHeaders forwards the canonical service input envelope
// (ZS-ARCH-SVC-001 §4), the same way internal/bankrec does: the caller's
// verified tenant and principal, and the caller's request, correlation and
// causation ids so general-ledger-svc's read is traceable to the analysis
// that caused it.
func setEnvelopeHeaders(ctx context.Context, req *http.Request, scope Scope) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set(svcenvelope.HeaderTenantID, scope.TenantID)
	req.Header.Set(svcenvelope.HeaderActorSubjectID, scope.PrincipalID)
	if scope.LegalEntityID != "" {
		req.Header.Set(svcenvelope.HeaderLegalEntityID, scope.LegalEntityID)
	}
	requestID := middleware.GetReqID(ctx)
	sourceChannel := string(svcenvelope.ChannelSystem)
	correlationID := ""
	if env, ok := svcenvelope.FromContext(ctx); ok {
		if env.RequestID != "" {
			requestID = env.RequestID
		}
		if env.SourceChannel != "" {
			sourceChannel = string(env.SourceChannel)
		}
		if env.SourceSystem != "" {
			req.Header.Set(svcenvelope.HeaderSourceSystem, env.SourceSystem)
		}
		if env.CorrelationID != "" {
			correlationID = env.CorrelationID
		}
		if env.CausationID != "" {
			req.Header.Set(svcenvelope.HeaderCausationID, env.CausationID)
		}
	}
	if requestID == "" {
		requestID = uuid.NewString()
	}
	if correlationID == "" {
		correlationID = uuid.NewString()
	}
	req.Header.Set(svcenvelope.HeaderRequestID, requestID)
	req.Header.Set(svcenvelope.HeaderCorrelationID, correlationID)
	req.Header.Set(svcenvelope.HeaderSourceChannel, sourceChannel)
}
