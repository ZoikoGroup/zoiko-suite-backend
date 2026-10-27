// Package accountingdispatch delivers accounting_posting_requests to the
// Accounting Kernel: general-ledger-svc ACC-04 POST /v1/postings/events.
//
// The request row is written in the SAME transaction as the invoice approval
// (store.TransitionInvoice / MutateInvoice), so an approved invoice always has a
// durable posting obligation. This worker is the only thing that posts it. ACC-04
// is idempotent on UNIQUE(tenant_id, source_event_id) (= the invoice id), so a
// retry or an at-least-once re-delivery can never post twice; a duplicate returns
// the prior execution. AP never calls /v1/journals and never writes the ledger.
//
// Outcome mapping:
//   - 200/201 with execution status COMMITTED -> POSTED (journal id recorded on the invoice)
//   - 200/201 with status FAILED              -> FAILED
//   - 200/201 with status QUARANTINED, or 422 -> QUARANTINED (ambiguous mapping: needs a human)
//   - 400                                     -> FAILED (the request itself is invalid)
//   - 412 (period locked), 5xx, transport     -> stays PENDING, retried with backoff
//
// After MaxAttempts transient failures a request becomes FAILED (visible), never
// retried forever. The invoice's accounting_state mirrors the queue:
// REQUESTED -> POSTED | FAILED.
package accountingdispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/accounts-payable-svc/internal/domain"
)

// MaxAttempts caps transient retries.
const MaxAttempts = 10

// Outcome is what the Accounting Kernel answered for one request.
type Outcome struct {
	Status      string // PENDING | POSTED | FAILED | QUARANTINED
	ExecutionID string
	JournalID   string
	Error       string
}

type Client interface {
	Post(ctx context.Context, tenantID, principalID string, payload []byte) (Outcome, error)
}

// Store is what the dispatcher needs from persistence.
type Store interface {
	ClaimPostingRequests(ctx context.Context, limit int, lease time.Duration) ([]domain.AccountingPostingRequest, error)
	FinishPostingRequest(ctx context.Context, requestID, status, executionID, lastError string, retryAt time.Time) error
	MutateInvoice(ctx context.Context, tenantID, invoiceID string, expectedVersion *int, fn domain.MutateFn) (*domain.VendorInvoice, error)
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
}

func NewHTTPClient(baseURL string) *HTTPClient {
	return &HTTPClient{baseURL: baseURL, http: &http.Client{Timeout: 10 * time.Second}}
}

type execution struct {
	ExecutionID   string  `json:"execution_id"`
	Status        string  `json:"status"`
	JournalID     *string `json:"journal_id"`
	FailureReason *string `json:"failure_reason"`
}

// Post returns an outcome for any HTTP answer; an error only for transport
// failures (always retryable).
func (c *HTTPClient) Post(ctx context.Context, tenantID, principalID string, payload []byte) (Outcome, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/postings/events", bytes.NewReader(payload))
	if err != nil {
		return Outcome{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)
	resp, err := c.http.Do(req)
	if err != nil {
		return Outcome{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch {
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated:
		var ex execution
		if err := json.Unmarshal(raw, &ex); err != nil {
			return Outcome{}, fmt.Errorf("unreadable general-ledger-svc response: %w", err)
		}
		o := Outcome{ExecutionID: ex.ExecutionID}
		if ex.JournalID != nil {
			o.JournalID = *ex.JournalID
		}
		if ex.FailureReason != nil {
			o.Error = *ex.FailureReason
		}
		switch ex.Status {
		case "COMMITTED":
			o.Status = domain.PostingStatusPosted
		case "FAILED":
			o.Status = domain.PostingStatusFailed
		case "QUARANTINED":
			o.Status = domain.PostingStatusQuarantined
		default:
			o.Status, o.Error = domain.PostingStatusPending, "execution status "+ex.Status
		}
		return o, nil
	case resp.StatusCode == http.StatusUnprocessableEntity:
		return Outcome{Status: domain.PostingStatusQuarantined, Error: string(raw)}, nil
	case resp.StatusCode == http.StatusBadRequest:
		return Outcome{Status: domain.PostingStatusFailed, Error: string(raw)}, nil
	default:
		return Outcome{Status: domain.PostingStatusPending, Error: fmt.Sprintf("general-ledger-svc status %d: %s", resp.StatusCode, string(raw))}, nil
	}
}

type Dispatcher struct {
	store     Store
	client    Client
	principal string // optional service identity; empty = post as the approving principal
	log       *zap.Logger
	interval  time.Duration
	batch     int
	lease     time.Duration
	now       func() time.Time
}

// New builds a dispatcher. principalID, when set, is the service identity granted
// GL_POSTING_EXECUTE in general-ledger-svc; when empty each posting is made as the
// principal who approved the invoice (the identity recorded on the request).
func New(st Store, c Client, principalID string, log *zap.Logger) *Dispatcher {
	return &Dispatcher{store: st, client: c, principal: principalID, log: log, interval: 3 * time.Second, batch: 25, lease: 60 * time.Second, now: time.Now}
}

func (d *Dispatcher) Start(ctx context.Context) {
	t := time.NewTicker(d.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.RunOnce(ctx)
		}
	}
}

func backoff(attempts int, now time.Time) time.Time {
	dur := 5 * time.Second
	for i := 0; i < attempts && dur < 10*time.Minute; i++ {
		dur *= 2
	}
	if dur > 10*time.Minute {
		dur = 10 * time.Minute
	}
	return now.Add(dur)
}

// RunOnce delivers every due request and returns how many it handled.
func (d *Dispatcher) RunOnce(ctx context.Context) int {
	reqs, err := d.store.ClaimPostingRequests(ctx, d.batch, d.lease)
	if err != nil {
		d.log.Error("accounting dispatch: claim failed", zap.Error(err))
		return 0
	}
	for _, p := range reqs {
		d.deliver(ctx, p)
	}
	return len(reqs)
}

func (d *Dispatcher) deliver(ctx context.Context, p domain.AccountingPostingRequest) {
	payload, err := json.Marshal(p.Payload)
	if err != nil {
		d.finish(ctx, p, Outcome{Status: domain.PostingStatusFailed, Error: "unmarshalable payload: " + err.Error()})
		return
	}
	principal := d.principal
	if principal == "" {
		principal = p.PrincipalID
	}
	o, err := d.client.Post(ctx, p.TenantID, principal, payload)
	if err != nil {
		o = Outcome{Status: domain.PostingStatusPending, Error: err.Error()}
	}
	d.finish(ctx, p, o)
}

// finish records the outcome. The invoice's accounting dimension is updated BEFORE
// the queue row is closed: if that update fails the row stays claimed, its lease
// lapses and the next pass re-delivers (ACC-04 is idempotent, so that is safe).
func (d *Dispatcher) finish(ctx context.Context, p domain.AccountingPostingRequest, o Outcome) {
	retryAt := d.now()
	if o.Status == domain.PostingStatusPending {
		if p.Attempts >= MaxAttempts {
			o.Status = domain.PostingStatusFailed
			o.Error = fmt.Sprintf("gave up after %d attempts: %s", p.Attempts, o.Error)
		} else {
			retryAt = backoff(p.Attempts, d.now())
			d.log.Warn("accounting dispatch: will retry", zap.String("source_event_id", p.SourceEventID), zap.String("reason", o.Error))
		}
	}
	if o.Status != domain.PostingStatusPending {
		if err := d.recordOnInvoice(ctx, p, o); err != nil {
			d.log.Error("accounting dispatch: could not update the invoice; will re-deliver", zap.String("invoice_id", p.InvoiceID), zap.Error(err))
			return
		}
	}
	if err := d.store.FinishPostingRequest(ctx, p.RequestID, o.Status, o.ExecutionID, o.Error, retryAt); err != nil {
		d.log.Error("accounting dispatch: could not record the outcome", zap.String("request_id", p.RequestID), zap.Error(err))
	}
}

func (d *Dispatcher) recordOnInvoice(ctx context.Context, p domain.AccountingPostingRequest, o Outcome) error {
	target := domain.AccountingPosted
	if o.Status != domain.PostingStatusPosted {
		target = domain.AccountingFailed
	}
	_, err := d.store.MutateInvoice(ctx, p.TenantID, p.InvoiceID, nil, func(inv *domain.VendorInvoice) (*domain.Mutation, error) {
		if inv.AccountingState == target {
			return nil, nil // already recorded (a re-delivery)
		}
		if err := inv.SetAccounting(target); err != nil {
			return nil, err
		}
		mut := &domain.Mutation{Actor: "accounting-dispatcher", CorrelationID: p.CorrelationID, KeepVersion: true,
			Detail: map[string]any{"request_id": p.RequestID, "status": o.Status, "execution_id": o.ExecutionID, "journal_id": o.JournalID}}
		if target == domain.AccountingPosted {
			// accounting_event_id is a UUID column: record the execution only if it is one.
			if _, err := uuid.Parse(o.ExecutionID); err == nil {
				ex := o.ExecutionID
				inv.AccountingEventID = &ex
			}
			if o.JournalID != "" {
				j := o.JournalID
				inv.ApprovalJournalID = &j
			}
			mut.Command = "AccountingPosted"
			mut.Events = []domain.OutboxEvent{{EventType: "vendor.invoice.accounting_posted", Payload: map[string]any{
				"invoice_id": inv.InvoiceID, "journal_id": o.JournalID, "execution_id": o.ExecutionID}}}
		} else {
			mut.Command = "AccountingPostingFailed"
			mut.Reason = o.Error
			mut.Events = []domain.OutboxEvent{{EventType: "vendor.invoice.accounting_failed", Payload: map[string]any{
				"invoice_id": inv.InvoiceID, "status": o.Status, "reason": o.Error}}}
		}
		return mut, nil
	})
	return err
}
