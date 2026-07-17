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
// needed to compute a journal's net amount.
type JournalLine struct {
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

// NetAmount returns the journal's balanced amount — the sum of one side of
// a balanced double-entry journal (debits and credits are equal by the time
// a journal is FINALIZED; general-ledger-svc enforces that at VALIDATE).
func (j Journal) NetAmount() float64 {
	var debit, credit float64
	for _, l := range j.Lines {
		debit += l.DebitAmount
		credit += l.CreditAmount
	}
	if debit != 0 {
		return debit
	}
	return credit
}

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
		http:    &http.Client{Timeout: 3 * time.Second},
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
