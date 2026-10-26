// Package ledger is the ONLY way payroll-run-svc reaches the general ledger: the
// posting engine (ACC-04) over HTTP. There is no ledger database access and no
// journal insert anywhere in this service (WFP-CTRL-011, WFP-NP-017).
package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/store"
)

type Client struct {
	baseURL      string
	kernelBookID string
	principal    string
	http         *http.Client
	log          *zap.Logger
}

// New builds a client. principal is the service identity sent as X-Principal-Id;
// it must hold the ledger's GL_POSTING_EXECUTE and GL_POSTING_REVERSE actions.
func New(baseURL, principal string, c *http.Client, log *zap.Logger) *Client {
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), principal: principal, http: c, log: log}
}

type execution struct {
	ExecutionID   string  `json:"execution_id"`
	Status        string  `json:"status"`
	JournalID     *string `json:"journal_id,omitempty"`
	FailureReason *string `json:"failure_reason,omitempty"`
}

func (c *Client) do(ctx context.Context, method, path string, tenantID, entityID, correlationID, idempotencyKey string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", c.principal)
	req.Header.Set("X-Legal-Entity-Id", entityID)
	req.Header.Set("X-Correlation-ID", correlationID)
	req.Header.Set("X-Request-Id", uuid.NewString())
	req.Header.Set("X-Source-Channel", "system")
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b, nil
}

// Post submits one accounting subject. A result is posted with
// POST /v1/postings/events; a reversal with POST /v1/postings/reversals. Both are
// idempotent on the subject's deterministic source_event_id, so a retry after a
// lost response, a second replica or a restart can never post twice.
//
// Outcomes: a committed execution is Posted; a refusal the ledger will not change
// (an unmapped key, a locked period, a refused permission: 4xx) is Final; a
// transport error, 429 or 5xx is transient and is retried.
func (c *Client) Post(ctx context.Context, b domain.AccountingBridge, originalJournalID string) store.PostOutcome {
	if b.PostingPurpose != domain.PurposeReversal && c.kernelBookID != "" {
		return c.postKernel(ctx, b)
	}
	var path string
	var body []byte
	var err error
	switch b.PostingPurpose {
	case domain.PurposeReversal:
		path = "/v1/postings/reversals"
		body, err = json.Marshal(map[string]string{"original_journal_id": originalJournalID, "reason": b.Description})
	default:
		path = "/v1/postings/events"
		body, err = b.LedgerEventPayload()
	}
	if err != nil {
		// The proposal cannot even be expressed; a retry cannot fix that.
		return store.PostOutcome{Final: true, Err: "cannot build the ledger request: " + err.Error()}
	}

	status, resp, err := c.do(ctx, http.MethodPost, path, b.TenantID, b.LegalEntityID, b.CorrelationID, b.SourceEventID, body)
	if err != nil {
		c.log.Warn("general-ledger-svc unreachable; will retry", zap.String("source_event_id", b.SourceEventID), zap.Error(err))
		return store.PostOutcome{Err: "ledger unreachable: " + err.Error()}
	}
	switch {
	case status == http.StatusOK || status == http.StatusCreated:
		var ex execution
		_ = json.Unmarshal(resp, &ex)
		// A 200 can also be the replay of an execution that failed earlier.
		if ex.Status == "FAILED" || ex.Status == "QUARANTINED" {
			reason := ex.Status
			if ex.FailureReason != nil {
				reason += ": " + *ex.FailureReason
			}
			return store.PostOutcome{Final: true, Err: "ledger recorded the posting as " + reason}
		}
		if ex.JournalID == nil || *ex.JournalID == "" {
			// Committed with no journal reference cannot be evidenced; retry (idempotent) rather than guess.
			return store.PostOutcome{Err: "ledger answered " + http.StatusText(status) + " without a journal id"}
		}
		return store.PostOutcome{Posted: true, ExecutionID: ex.ExecutionID, JournalID: *ex.JournalID}
	case status == http.StatusTooManyRequests || status == http.StatusRequestTimeout || status >= 500:
		return store.PostOutcome{Err: fmt.Sprintf("ledger answered %d", status)}
	default:
		msg := strings.TrimSpace(string(resp))
		if len(msg) > 500 {
			msg = msg[:500]
		}
		c.log.Error("ledger refused the posting request", zap.String("source_event_id", b.SourceEventID), zap.Int("status", status), zap.String("body", msg))
		return store.PostOutcome{Final: true, Err: fmt.Sprintf("ledger refused (%d): %s", status, msg)}
	}
}

// Journal is the part of a ledger journal that reconciliation compares.
type Journal struct {
	JournalID     string `json:"journal_id"`
	Status        string `json:"status"`
	LegalEntityID string `json:"legal_entity_id"`
	FiscalPeriod  string `json:"fiscal_period"`
	CurrencyCode  string `json:"currency_code"`
	Lines         []struct {
		DebitAmount  float64 `json:"debit_amount"`
		CreditAmount float64 `json:"credit_amount"`
	} `json:"lines"`
}

// GetJournal reads a journal. ok is false when the ledger has no such journal; err
// is non-nil when it could not answer (which reconciliation reports as unknown,
// never as a match).
func (c *Client) GetJournal(ctx context.Context, tenantID, entityID, correlationID, journalID string) (j *Journal, ok bool, err error) {
	status, resp, err := c.do(ctx, http.MethodGet, "/v1/journals/"+url.PathEscape(journalID), tenantID, entityID, correlationID, "", nil)
	if err != nil {
		return nil, false, domain.ErrLedgerUnavailable
	}
	switch {
	case status == http.StatusNotFound:
		return nil, false, nil
	case status != http.StatusOK:
		return nil, false, domain.ErrLedgerUnavailable
	}
	var out Journal
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, false, domain.ErrLedgerUnavailable
	}
	return &out, true, nil
}

// WithKernelBook makes the client post payroll results through general-ledger-svc's accounting kernel into the given accounting book
// instead of the legacy posting command. An empty book id keeps the legacy path.
func (c *Client) WithKernelBook(bookID string) *Client {
	c.kernelBookID = strings.TrimSpace(bookID)
	return c
}

type kernelEvent struct {
	AccountingEventID string `json:"accounting_event_id"`
}

type kernelPosting struct {
	JournalID   string `json:"journal_id"`
	ExecutionID string `json:"posting_execution_id"`
}

// postKernel registers the payroll result as a PAYROLL.RESULT_ACCEPTED accounting event and posts it into the kernel book. Both steps are
// idempotent on the subject's source_event_id (the event by its source reference, the posting by event and book), so a retry after a
// lost response or a restart never posts twice. A refusal the ledger will not change (an unbalanced result, an unmapped key, a locked
// period, a refused permission: 4xx) is Final; a transport error, 429 or 5xx is retried.
func (c *Client) postKernel(ctx context.Context, b domain.AccountingBridge) store.PostOutcome {
	facts, err := b.KernelFacts()
	if err != nil {
		return store.PostOutcome{Final: true, Err: "cannot express the result as a kernel event: " + err.Error()}
	}
	day := func(d string) string { return d + "T00:00:00Z" }
	eventBody, _ := json.Marshal(map[string]any{
		"event_type": "PAYROLL.RESULT_ACCEPTED", "event_version": 1, "legal_entity_id": b.LegalEntityID, "source_system": "payroll-run-svc",
		"source_object_type": "payroll_result", "source_object_id": b.ResultID, "source_event_id": b.SourceEventID, "occurred_at": b.CreatedAt.UTC().Format(time.RFC3339),
		"document_date": day(b.DocumentDate), "transaction_currency": b.TransactionCurrency, "facts": facts,
		"idempotency_key": b.SourceEventID, "correlation_id": b.CorrelationID, "causation_id": b.BridgeID,
	})
	classify := func(step string, status int, resp []byte) (store.PostOutcome, bool) {
		switch {
		case status == http.StatusOK || status == http.StatusCreated:
			return store.PostOutcome{}, false
		case status == http.StatusTooManyRequests || status == http.StatusRequestTimeout || status >= 500:
			return store.PostOutcome{Err: fmt.Sprintf("ledger kernel answered %d (%s)", status, step)}, true
		default:
			msg := strings.TrimSpace(string(resp))
			if len(msg) > 500 {
				msg = msg[:500]
			}
			c.log.Error("ledger kernel refused the payroll result", zap.String("source_event_id", b.SourceEventID), zap.String("step", step), zap.Int("status", status), zap.String("body", msg))
			return store.PostOutcome{Final: true, Err: fmt.Sprintf("ledger kernel refused (%d, %s): %s", status, step, msg)}, true
		}
	}
	status, resp, err := c.do(ctx, http.MethodPost, "/v1/accounting-events", b.TenantID, b.LegalEntityID, b.CorrelationID, b.SourceEventID, eventBody)
	if err != nil {
		c.log.Warn("general-ledger-svc unreachable; will retry", zap.String("source_event_id", b.SourceEventID), zap.Error(err))
		return store.PostOutcome{Err: "ledger unreachable: " + err.Error()}
	}
	if out, done := classify("register event", status, resp); done {
		return out
	}
	var ev kernelEvent
	if err := json.Unmarshal(resp, &ev); err != nil || ev.AccountingEventID == "" {
		return store.PostOutcome{Err: "ledger kernel answered without an accounting event id"}
	}
	postBody, _ := json.Marshal(map[string]any{"accounting_event_id": ev.AccountingEventID, "book_id": c.kernelBookID, "posting_date": day(b.PostingDate)})
	status, resp, err = c.do(ctx, http.MethodPost, "/v1/accounting-postings", b.TenantID, b.LegalEntityID, b.CorrelationID, b.SourceEventID, postBody)
	if err != nil {
		c.log.Warn("general-ledger-svc unreachable; will retry", zap.String("source_event_id", b.SourceEventID), zap.Error(err))
		return store.PostOutcome{Err: "ledger unreachable: " + err.Error()}
	}
	if out, done := classify("post", status, resp); done {
		return out
	}
	var p kernelPosting
	if err := json.Unmarshal(resp, &p); err != nil || p.JournalID == "" {
		return store.PostOutcome{Err: "ledger kernel answered without a journal id"}
	}
	return store.PostOutcome{Posted: true, ExecutionID: p.ExecutionID, JournalID: p.JournalID}
}
