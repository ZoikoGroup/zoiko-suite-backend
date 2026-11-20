// Package glclient is this service's HTTP client for general-ledger-svc's
// existing kernel migration-batch endpoints (Stage, Preview, Approve, Post,
// Reconcile, Evidence). It calls those endpoints exactly as they are --
// general-ledger-svc's own kernel logic is never modified here -- and it
// carries the same identity-envelope headers and Idempotency-Key convention
// every inter-service writer on this platform already uses (see
// internal/authz.Client.checkAllowedLive for the pattern this mirrors).
package glclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// ErrUnavailable is returned for any transport failure or non-2xx response
// general-ledger-svc gives back that this client cannot interpret as a
// specific domain error. Callers must treat it as "the promotion did not
// complete" and retry the step idempotently -- never as success.
var ErrUnavailable = errors.New("general-ledger-svc unavailable or returned an unexpected response")

type Client struct {
	baseURL    string
	httpClient *http.Client
}

func New(baseURL string) *Client {
	return &Client{baseURL: baseURL, httpClient: &http.Client{Timeout: 20 * time.Second}}
}

func NewWithHTTPClient(baseURL string, hc *http.Client) *Client {
	return &Client{baseURL: baseURL, httpClient: hc}
}

// Identity is the caller context forwarded on every call: the verified
// tenant and legal entity this service itself resolved, the acting
// principal, and a correlation id that threads this promotion's whole
// Stage->Preview->Approve->Post->Reconcile sequence through general-ledger-svc's
// own evidence and event trail.
type Identity struct {
	TenantID      string
	LegalEntityID string
	PrincipalID   string
	CorrelationID string
}

func (c *Client) do(ctx context.Context, method, path string, id Identity, idemSeed string, body, out any) (int, error) {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return 0, fmt.Errorf("encode request: %w", err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, &buf)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", id.TenantID)
	req.Header.Set("X-Principal-Id", id.PrincipalID)
	req.Header.Set("X-Actor-Subject-Id", id.PrincipalID)
	req.Header.Set("X-Legal-Entity-Id", id.LegalEntityID)
	req.Header.Set("X-Request-Id", "mig-glclient-"+uuid.New().String())
	correlationID := id.CorrelationID
	if correlationID == "" {
		correlationID = "mig-corr-" + uuid.New().String()
	}
	req.Header.Set("X-Correlation-ID", correlationID)
	req.Header.Set("X-Source-Channel", "integration")
	req.Header.Set("X-Source-System", "migration-integrity-svc")
	if method == http.MethodPost {
		// Deterministic per (step, seed): a retried promotion step replays
		// general-ledger-svc's own idempotent response (Existing=true)
		// instead of creating a second batch/journal for the same extract.
		req.Header.Set("Idempotency-Key", "mig-"+path+"-"+idemSeed)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()

	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("%w: %s %s -> %d: %s", ErrUnavailable, method, path, resp.StatusCode, string(b))
	}
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode response from %s: %w", path, err)
		}
	}
	return resp.StatusCode, nil
}

// ─── Request/response shapes, mirroring general-ledger-svc/internal/kernel ──

type Totals struct {
	Debit  string `json:"debit"`
	Credit string `json:"credit"`
}

type ControlTotal struct {
	Total string `json:"total"`
	Count int    `json:"count"`
	Ref   string `json:"reference"`
}

type LineInput struct {
	SourceRef           string            `json:"source_ref"`
	SourceAccount       string            `json:"source_account"`
	AccountCode         string            `json:"account_code"`
	Debit               string            `json:"debit"`
	Credit              string            `json:"credit"`
	CounterpartyID      string            `json:"counterparty_id,omitempty"`
	TransactionAmount   string            `json:"transaction_amount,omitempty"`
	TransactionCurrency string            `json:"transaction_currency,omitempty"`
	ExchangeRate        string            `json:"exchange_rate,omitempty"`
	Dimensions          map[string]string `json:"dimensions,omitempty"`
	Description         string            `json:"description,omitempty"`
}

type StageRequest struct {
	LegalEntityID     string                  `json:"legal_entity_id"`
	BookID            string                  `json:"book_id"`
	BatchCode         string                  `json:"batch_code"`
	Kind              string                  `json:"batch_kind,omitempty"`
	CutoverDate       string                  `json:"cutover_date"`
	SourceSystem      string                  `json:"source_system"`
	SourceExtractRef  string                  `json:"source_extract_ref"`
	SourceExtractHash string                  `json:"source_extract_hash"`
	MappingRef        string                  `json:"mapping_ref"`
	SourceTotals      Totals                  `json:"source_totals"`
	ControlTotals     map[string]ControlTotal `json:"control_totals,omitempty"`
	Lines             []LineInput             `json:"lines"`
}

// Batch mirrors kernel.MigrationBatch's fields this client needs to read.
// It is intentionally a subset: this service reads general-ledger-svc's
// batch, it never re-derives or re-states its financial content.
type Batch struct {
	BatchID         string          `json:"batch_id"`
	Status          string          `json:"status"`
	JournalID       string          `json:"journal_id,omitempty"`
	JournalNumber   string          `json:"journal_number,omitempty"`
	EvidenceHash    string          `json:"evidence_hash,omitempty"`
	Reconciliation  *Reconciliation `json:"reconciliation,omitempty"`
	Existing        bool            `json:"existing,omitempty"`
}

type Reconciliation struct {
	Status        string   `json:"status"`
	JournalID     string   `json:"journal_id"`
	JournalNumber string   `json:"journal_number"`
	JournalHash   string   `json:"journal_hash"`
	Differences   []Diff   `json:"differences"`
}

type Diff struct {
	What     string `json:"what"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
}

type Issue struct {
	Line      int    `json:"line,omitempty"`
	SourceRef string `json:"source_ref,omitempty"`
	Code      string `json:"code"`
	Detail    string `json:"detail"`
}

type Preview struct {
	Batch *Batch `json:"batch"`
	Ready bool   `json:"ready_to_approve"`
	Analysis struct {
		Issues []Issue `json:"issues"`
	} `json:"analysis"`
}

type Evidence struct {
	Batch         *Batch `json:"batch"`
	LinesVerified bool   `json:"lines_verified"`
	Verified      bool   `json:"evidence_verified"`
}

// ─── Calls ───────────────────────────────────────────────────────────────

// Stage calls POST /v1/migration-batches. idemSeed should be stable per
// (job, batch_code) so a retried promotion stages the same batch rather
// than a new one.
func (c *Client) Stage(ctx context.Context, id Identity, idemSeed string, req StageRequest) (*Batch, error) {
	var out Batch
	if _, err := c.do(ctx, http.MethodPost, "/v1/migration-batches", id, idemSeed, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Preview(ctx context.Context, id Identity, batchID string) (*Preview, error) {
	var out Preview
	if _, err := c.do(ctx, http.MethodGet, "/v1/migration-batches/"+batchID+"/preview", id, "", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Validate(ctx context.Context, id Identity, idemSeed, batchID string) (*Batch, []Issue, error) {
	var out struct {
		Batch    Batch `json:"batch"`
		Analysis struct {
			Issues []Issue `json:"issues"`
		} `json:"analysis"`
		Valid bool `json:"valid"`
	}
	if _, err := c.do(ctx, http.MethodPost, "/v1/migration-batches/"+batchID+"/validate", id, idemSeed, nil, &out); err != nil {
		return nil, nil, err
	}
	return &out.Batch, out.Analysis.Issues, nil
}

type approveBody struct {
	AuthorityRef string    `json:"authority_ref"`
	WindowStart  time.Time `json:"window_start"`
	WindowEnd    time.Time `json:"window_end"`
}

func (c *Client) Approve(ctx context.Context, id Identity, idemSeed, batchID, authorityRef string, windowStart, windowEnd time.Time) (*Batch, error) {
	var out Batch
	body := approveBody{AuthorityRef: authorityRef, WindowStart: windowStart, WindowEnd: windowEnd}
	if _, err := c.do(ctx, http.MethodPost, "/v1/migration-batches/"+batchID+"/approve", id, idemSeed, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Post(ctx context.Context, id Identity, idemSeed, batchID string) (*Batch, error) {
	var out Batch
	if _, err := c.do(ctx, http.MethodPost, "/v1/migration-batches/"+batchID+"/post", id, idemSeed, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Reconcile(ctx context.Context, id Identity, idemSeed, batchID string) (*Batch, error) {
	var out Batch
	if _, err := c.do(ctx, http.MethodPost, "/v1/migration-batches/"+batchID+"/reconcile", id, idemSeed, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Evidence calls GET /v1/migration-batches/{id}/evidence -- the read this
// client uses to back a certificate with general-ledger-svc's OWN
// reconciliation result, fetched fresh at issuance time rather than trusted
// from an earlier step's response.
func (c *Client) Evidence(ctx context.Context, id Identity, batchID string) (*Evidence, error) {
	var out Evidence
	if _, err := c.do(ctx, http.MethodGet, "/v1/migration-batches/"+batchID+"/evidence", id, "", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
