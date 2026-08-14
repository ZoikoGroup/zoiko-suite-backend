// Package ledger provides a read-only client against general-ledger-svc,
// used to verify that a journal a caller claims reconciles a bank statement
// line is real, FINALIZED, belongs to the right legal entity, and matches
// the statement line's amount.
//
// The tenant scope is passed via the X-Tenant-Id header (the same
// gateway-auth-svc convention every service in this platform trusts) and
// the journal ID is passed as a path segment, path-escaped defensively —
// never interpolated into a query string. That sidesteps the query-string
// injection smell entirely rather than relying on the caller's IDs always
// happening to be valid UUIDs.
package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"time"
)

var (
	// ErrJournalNotFound means general-ledger-svc was reached and answered
	// that no journal exists with the given ID (under the given tenant).
	ErrJournalNotFound = errors.New("journal not found")
	// ErrUnavailable means general-ledger-svc could not be reached or
	// returned an unexpected status — callers must fail closed, never treat
	// this as "no matching journal."
	ErrUnavailable = errors.New("general-ledger-svc unavailable")
)

// JournalLine mirrors the subset of general-ledger-svc's JournalLine fields
// needed to compute a journal's movement on a given account.
//
// AccountCode is what makes direction knowable and was previously not read at
// all — without it every line looks alike and only magnitudes can be
// compared.
type JournalLine struct {
	AccountCode  string  `json:"account_code"`
	DebitAmount  float64 `json:"debit_amount"`
	CreditAmount float64 `json:"credit_amount"`
}

// Journal mirrors the subset of general-ledger-svc's JournalWithLines fields
// needed for reconciliation verification.
type Journal struct {
	JournalID     string        `json:"journal_id"`
	TenantID      string        `json:"tenant_id"`
	LegalEntityID string        `json:"legal_entity_id"`
	Status        string        `json:"status"`
	Lines         []JournalLine `json:"lines"`
}

// CashMovementCents returns the journal's SIGNED net movement on accountCode,
// in cents: positive for money into the account (a net debit to it), negative
// for money out (a net credit). The second return is false when the journal
// does not touch that account at all, which is not the same as a movement of
// zero — one means "this journal has nothing to do with this bank account",
// the other means "it nets to nothing", and a caller must be able to tell
// them apart.
//
// In cents, not float64, and compared exactly rather than within an epsilon.
// The amounts are NUMERIC(18,2) at both ends; they are only ever float64 in
// transit because that is what JSON gives us. 0.1+0.2 != 0.3 in binary
// floating point, and this comparison decides whether money is declared
// reconciled.
//
// This replaces NetAmount, which summed one side of the journal and so was
// always positive — carrying no direction whatsoever. Comparing its magnitude
// to a statement line's magnitude meant a 500.00 payment OUT reconciled
// cleanly against a journal that moved 500.00 IN.
func (j Journal) CashMovementCents(accountCode string) (int64, bool) {
	var cents int64
	var touched bool
	for _, l := range j.Lines {
		if l.AccountCode != accountCode {
			continue
		}
		touched = true
		cents += toCents(l.DebitAmount) - toCents(l.CreditAmount)
	}
	return cents, touched
}

// ToCents converts a JSON-decoded money amount to exact cents. Rounding is
// required because a decimal like 12.34 has no exact float64 representation,
// so 12.34*100 can land on 1233.9999999999998 and truncate to 1233.
func ToCents(v float64) int64 { return toCents(v) }

func toCents(v float64) int64 { return int64(math.Round(v * 100)) }

// Client is the narrow interface the handler depends on.
type Client interface {
	GetJournal(ctx context.Context, tenantID, journalID string) (*Journal, error)
}

// HTTPClient implements Client against a real general-ledger-svc instance.
type HTTPClient struct {
	baseURL string
	http    *http.Client
}

// NewHTTPClient constructs an HTTPClient bound to baseURL, e.g.
// "http://general-ledger-svc:8098" (no trailing slash).
func NewHTTPClient(baseURL string) *HTTPClient {
	return &HTTPClient{
		baseURL: baseURL,
		http:    &http.Client{Timeout: 3 * time.Second, Transport: newRetryTransport()},
	}
}

// GetJournal fetches a single journal by ID, scoped to tenantID via the
// X-Tenant-Id header. Returns ErrJournalNotFound for a 404, ErrUnavailable
// for anything else that isn't a clean 200.
func (c *HTTPClient) GetJournal(ctx context.Context, tenantID, journalID string) (*Journal, error) {
	reqURL := fmt.Sprintf("%s/v1/journals/%s", c.baseURL, url.PathEscape(journalID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", ErrUnavailable, err)
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrJournalNotFound
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("%w: status %d: %s", ErrUnavailable, resp.StatusCode, body)
	}

	var j Journal
	if err := json.NewDecoder(resp.Body).Decode(&j); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrUnavailable, err)
	}
	return &j, nil
}
