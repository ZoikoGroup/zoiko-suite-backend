package ar08

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"

	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/svcclient"
)

// BankTransaction is the Banking-owned canonical transaction (banking-connector-svc GET /v1/banking/canonical-transactions/{id}).
// A positive Amount is money received. Banking owns finality: only NORMALIZED is confirmed.
type BankTransaction struct {
	TransactionID   string
	TenantID        string
	StatementLineID string
	Currency        string
	Status          string // NORMALIZED | QUARANTINED | SUPERSEDED
	Counterparty    string
	SupersededBy    string
	Amount          kit.Amount
	TransactionDate time.Time
}

// Source errors. Anything but a clean answer fails closed.
var (
	ErrBankTxnNotFound   = errors.New("bank transaction not found")
	ErrSourceUnavailable = errors.New("settlement source unavailable")
)

// ReceiptSource reads settlement finality from Banking.
type ReceiptSource interface {
	Get(ctx context.Context, c kit.Caller, transactionID string) (*BankTransaction, error)
}

// OpenItemFinder lists a customer's open items with a residual (AR-07's ListOpenForCustomer satisfies it).
type OpenItemFinder interface {
	ListOpenForCustomer(ctx context.Context, tenantID, legalEntityID, customerPartyID, currency string) ([]kit.OpenItemView, error)
}

var currencyRE = regexp.MustCompile(`^[A-Z]{3}$`)

// VerifyConfirmed decides whether the Banking answer is a CONFIRMED receipt. Anything else is SETTLEMENT_NOT_CONFIRMED.
func VerifyConfirmed(t *BankTransaction, wantID, tenantID string) error {
	switch {
	case t == nil:
		return errNotConfirmed("the bank transaction was not found")
	case !strings.EqualFold(t.TransactionID, wantID):
		return errNotConfirmed("Banking answered for a different transaction: the settlement is ambiguous")
	case t.TenantID != "" && !strings.EqualFold(t.TenantID, tenantID):
		return errNotConfirmed("the bank transaction does not belong to this tenant")
	case t.Status != BankNormalized:
		return errNotConfirmed("the bank transaction is " + t.Status + ", not NORMALIZED: settlement is not confirmed")
	case !t.Amount.IsPositive():
		return errNotConfirmed("the bank transaction is not a receipt (amount is not positive)")
	case !currencyRE.MatchString(t.Currency):
		return errNotConfirmed("the bank transaction has no valid currency")
	case !t.Amount.FitsCurrency(t.Currency):
		return errNotConfirmed("the bank transaction amount does not fit its currency")
	case t.TransactionDate.IsZero():
		return errNotConfirmed("the bank transaction has no value date")
	}
	return nil
}

// HTTPReceiptSource is the banking-connector-svc client.
type HTTPReceiptSource struct{ c *svcclient.Client }

func NewHTTPReceiptSource(baseURL string) *HTTPReceiptSource {
	return &HTTPReceiptSource{c: svcclient.New(baseURL)}
}

type bankWire struct {
	TransactionID   string      `json:"transaction_id"`
	TenantID        string      `json:"tenant_id"`
	StatementLineID string      `json:"statement_line_id"`
	TransactionDate time.Time   `json:"transaction_date"`
	Amount          json.Number `json:"amount"`
	Currency        string      `json:"currency"`
	Counterparty    string      `json:"counterparty"`
	Status          string      `json:"status"`
	SupersededBy    string      `json:"superseded_by"`
}

// Get reads one canonical transaction. 404 is ErrBankTxnNotFound; every other non-200, a transport failure or an unreadable
// body is ErrSourceUnavailable (callers fail closed).
func (s *HTTPReceiptSource) Get(ctx context.Context, c kit.Caller, id string) (*BankTransaction, error) {
	status, body, err := s.c.Do(ctx, c.SvcCaller(), "GET", "/v1/banking/canonical-transactions/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, ErrSourceUnavailable
	}
	switch status {
	case 200:
	case 404:
		return nil, ErrBankTxnNotFound
	default:
		return nil, ErrSourceUnavailable
	}
	var w bankWire
	if json.Unmarshal(body, &w) != nil {
		return nil, ErrSourceUnavailable
	}
	amt, perr := kit.ParseAmount(w.Amount.String())
	if perr != nil {
		return nil, ErrSourceUnavailable
	}
	return &BankTransaction{TransactionID: w.TransactionID, TenantID: w.TenantID, StatementLineID: w.StatementLineID, Currency: strings.ToUpper(w.Currency),
		Status: strings.ToUpper(w.Status), Counterparty: w.Counterparty, SupersededBy: w.SupersededBy, Amount: amt, TransactionDate: w.TransactionDate}, nil
}
