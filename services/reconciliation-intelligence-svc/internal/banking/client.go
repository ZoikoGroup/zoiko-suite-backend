// Package banking reads banking-connector-svc's NORMALIZED canonical bank
// transactions (BNK-04 ListTransactions) to find the counterpart of an
// UNMATCHED bank statement line when no general-ledger-svc journal is one
// (SPEC_DEVIATIONS A2b): one bank account's transactions in one currency, in
// a date window around the statement date.
//
// Read-only by construction: there is no method here that calls a
// banking-connector-svc write route. A proposed transaction is only ever a
// suggestion; the match is made in bank-reconciliation-svc, by a human.
//
// The client fails CLOSED, like internal/ledger. Transactions that cannot be
// shown to be exactly what was asked for — another tenant, entity, account or
// currency, not NORMALIZED, outside the window, a page with more behind it,
// an amount that cannot be held exactly — are never returned, so no proposal
// is ever built on a partial or wrong set.
package banking

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/reconciliation-intelligence-svc/internal/domain"
	svcenvelope "zoiko.io/reconciliation-intelligence-svc/internal/envelope"
)

var (
	// ErrServiceUnavailable: transport failure or a 5xx. An outage.
	ErrServiceUnavailable = errors.New("banking-connector-svc unavailable")
	// ErrRequestRefused: a 4xx — a defect in this request, or this
	// service's principal not holding bank.transaction.read on the legal
	// entity (403). Not an outage.
	ErrRequestRefused = errors.New("banking-connector-svc refused the request")
	// ErrInconsistent: the answer does not describe what was asked for.
	ErrInconsistent = errors.New("banking-connector-svc returned inconsistent canonical transactions")
	// ErrWindowTooLarge: the page came back with a next_cursor, so it is not
	// the whole result. Cursor-following is not implemented; a partial read
	// could miss the one transaction that matches, and "no candidate" would
	// then be false.
	ErrWindowTooLarge = errors.New("bank account has too many canonical transactions in the search window to read in one page")
	// ErrScopeMissing: no verified tenant, principal or legal entity to
	// forward. ListTransactions is authorized per legal entity.
	ErrScopeMissing = errors.New("tenant, principal and legal entity are required to read banking-connector-svc")
)

const (
	// maxTransactions is banking-connector-svc's ListTransactions cap
	// (controlMaxLimit). A larger limit is silently replaced by it, so it is
	// asked for exactly.
	maxTransactions  = 5000
	maxResponseBytes = 16 << 20
	dateLayout       = "2006-01-02"
	statusNormalized = "NORMALIZED"
)

// Scope is who a read is made on behalf of: the verified tenant and
// principal of the inbound request, so banking-connector-svc applies its own
// row-level tenant isolation and bank.transaction.read authorization to the
// caller.
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
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), httpClient: &http.Client{Timeout: 10 * time.Second}, logger: logger}
}

// listItem is the part of banking-connector-svc's
// domain.CanonicalTransactionListItem this service needs. Amount is the
// NUMERIC(18,4) value as a JSON string and is decoded as one: a bare JSON
// number would mean that service's exact-decimal contract has regressed to
// float64, and is refused rather than accepted.
type listItem struct {
	TransactionID   string  `json:"transaction_id"`
	TenantID        string  `json:"tenant_id"`
	LegalEntityID   string  `json:"legal_entity_id"`
	BankAccountID   *string `json:"bank_account_id"`
	TransactionDate string  `json:"transaction_date"`
	Amount          string  `json:"amount"`
	Currency        string  `json:"currency"`
	Status          string  `json:"status"`
}

type listPage struct {
	Transactions *[]listItem `json:"transactions"`
	NextCursor   string      `json:"next_cursor"`
}

// ListBankAccountTransactions returns scope.LegalEntityID's NORMALIZED
// canonical transactions on bankAccountID in currency with a transaction
// date (UTC) in [from, to] (YYYY-MM-DD, inclusive). One call per bank
// account, currency and window per run, never per line.
func (c *Client) ListBankAccountTransactions(ctx context.Context, scope Scope, bankAccountID, currency, from, to string) ([]domain.CanonicalTransaction, error) {
	if scope.TenantID == "" || scope.PrincipalID == "" || scope.LegalEntityID == "" {
		return nil, ErrScopeMissing
	}
	if bankAccountID == "" || currency == "" {
		return nil, fmt.Errorf("%w: bank account and currency are required", ErrInconsistent)
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
		"bank_account_id":       {bankAccountID},
		"currency":              {currency},
		"transaction_date_from": {from},
		"transaction_date_to":   {to},
		"limit":                 {strconv.Itoa(maxTransactions)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/banking/canonical-transactions?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrServiceUnavailable, err)
	}
	setEnvelopeHeaders(ctx, req, scope)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.logger.Error("banking-connector-svc unreachable — failing closed", zap.Error(err))
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
		c.logger.Error("banking-connector-svc refused the canonical transaction read",
			zap.Int("status", resp.StatusCode), zap.ByteString("body", body))
		return nil, fmt.Errorf("%w: HTTP %d", ErrRequestRefused, resp.StatusCode)
	}

	var page listPage
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&page); err != nil {
		return nil, fmt.Errorf("%w: unreadable body: %v", ErrInconsistent, err)
	}
	if page.Transactions == nil {
		return nil, fmt.Errorf("%w: response has no transactions array", ErrInconsistent)
	}
	raw := *page.Transactions
	// next_cursor is present exactly when more rows exist. This page is then
	// not the whole result, and nothing may be concluded from it.
	if page.NextCursor != "" {
		return nil, fmt.Errorf("%w: bank account %s, %s, %s to %s: more than one page", ErrWindowTooLarge, bankAccountID, currency, from, to)
	}
	if len(raw) > maxTransactions {
		return nil, fmt.Errorf("%w: %d transactions on a page capped at %d", ErrInconsistent, len(raw), maxTransactions)
	}

	seen := make(map[string]bool, len(raw))
	out := make([]domain.CanonicalTransaction, 0, len(raw))
	for _, t := range raw {
		// Every row is checked against the scope asked for: a filter
		// silently ignored upstream would otherwise turn into proposals
		// about another entity's or account's transactions.
		id, err := uuid.Parse(t.TransactionID)
		switch {
		case err != nil:
			return nil, fmt.Errorf("%w: transaction_id %q is not a UUID", ErrInconsistent, t.TransactionID)
		case seen[id.String()]:
			return nil, fmt.Errorf("%w: transaction %s returned twice", ErrInconsistent, id)
		case t.TenantID != scope.TenantID || t.LegalEntityID != scope.LegalEntityID:
			return nil, fmt.Errorf("%w: transaction %s is outside the requested tenant and legal entity", ErrInconsistent, id)
		case t.BankAccountID == nil || *t.BankAccountID != bankAccountID:
			return nil, fmt.Errorf("%w: transaction %s is not on bank account %s", ErrInconsistent, id, bankAccountID)
		case t.Currency != currency:
			return nil, fmt.Errorf("%w: transaction %s is in %s, asked for %s", ErrInconsistent, id, t.Currency, currency)
		case t.Status != statusNormalized:
			return nil, fmt.Errorf("%w: transaction %s has status %q, only NORMALIZED is current", ErrInconsistent, id, t.Status)
		}
		seen[id.String()] = true

		// transaction_date is a timestamp; the route's window is UTC calendar
		// days, so the date is taken in UTC.
		ts, err := time.Parse(time.RFC3339Nano, t.TransactionDate)
		if err != nil {
			return nil, fmt.Errorf("%w: transaction %s transaction_date %q: %v", ErrInconsistent, id, t.TransactionDate, err)
		}
		date := ts.UTC().Format(dateLayout)
		day, _ := time.Parse(dateLayout, date)
		if day.Before(fromDate) || day.After(toDate) {
			return nil, fmt.Errorf("%w: transaction %s transaction_date %s is outside %s to %s", ErrInconsistent, id, date, from, to)
		}
		// Signed, exactly as banking-connector-svc stores it (positive is
		// money in), never through float64.
		amount, err := domain.ParseStorableAmount(t.Amount)
		if err != nil {
			return nil, fmt.Errorf("%w: transaction %s amount: %v", ErrInconsistent, id, err)
		}
		out = append(out, domain.CanonicalTransaction{
			TransactionID: id.String(), TenantID: t.TenantID, LegalEntityID: t.LegalEntityID,
			BankAccountID: *t.BankAccountID, Currency: t.Currency, Status: t.Status,
			TransactionDate: date, Amount: amount,
		})
	}
	return out, nil
}

// setEnvelopeHeaders forwards the canonical service input envelope
// (ZS-ARCH-SVC-001 §4), the same way internal/ledger and internal/bankrec
// do: the caller's verified tenant and principal, and the caller's request,
// correlation and causation ids so banking-connector-svc's read is traceable
// to the analysis that caused it.
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
