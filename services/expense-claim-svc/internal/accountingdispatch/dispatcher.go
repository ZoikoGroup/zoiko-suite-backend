// Package accountingdispatch delivers accounting_posting_requests to the
// Accounting Kernel: general-ledger-svc ACC-04 POST /v1/postings/events.
// That command is idempotent on UNIQUE(tenant_id, source_event_id), so a
// retry (or an at-least-once re-delivery) can never post twice; a duplicate
// returns the prior execution and is recorded as such. This service never
// calls /v1/journals and never writes the ledger.
//
// Outcome mapping:
//   - 200/201 with execution status COMMITTED  -> POSTED
//   - 200/201 with status FAILED               -> FAILED (GL recorded a permanent failure)
//   - 200/201 with status QUARANTINED, or 422  -> QUARANTINED (ambiguous mapping: needs a human)
//   - 400                                      -> FAILED (the request itself is invalid)
//   - 412 (period locked), 5xx, transport      -> stays PENDING, retried with backoff
package accountingdispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"

	"zoiko.io/expense-claim-svc/internal/domain"
	"zoiko.io/expense-claim-svc/internal/store"
)

type Client interface {
	Post(ctx context.Context, tenantID, principalID string, payload []byte) (domain.PostingOutcome, error)
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
func (c *HTTPClient) Post(ctx context.Context, tenantID, principalID string, payload []byte) (domain.PostingOutcome, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/postings/events", bytes.NewReader(payload))
	if err != nil {
		return domain.PostingOutcome{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)
	resp, err := c.http.Do(req)
	if err != nil {
		return domain.PostingOutcome{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch {
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated:
		var ex execution
		if err := json.Unmarshal(raw, &ex); err != nil {
			return domain.PostingOutcome{}, fmt.Errorf("unreadable general-ledger-svc response: %w", err)
		}
		o := domain.PostingOutcome{ExecutionID: ex.ExecutionID}
		if ex.JournalID != nil {
			o.JournalID = *ex.JournalID
		}
		if ex.FailureReason != nil {
			o.Error = *ex.FailureReason
		}
		switch ex.Status {
		case "COMMITTED":
			o.Status = domain.PostingPosted
		case "FAILED":
			o.Status = domain.PostingFailed
		case "QUARANTINED":
			o.Status = domain.PostingQuarantined
		default:
			o.Status, o.Error = domain.PostingPending, "execution status "+ex.Status
		}
		return o, nil
	case resp.StatusCode == http.StatusUnprocessableEntity:
		return domain.PostingOutcome{Status: domain.PostingQuarantined, Error: string(raw)}, nil
	case resp.StatusCode == http.StatusBadRequest:
		return domain.PostingOutcome{Status: domain.PostingFailed, Error: string(raw)}, nil
	default:
		return domain.PostingOutcome{Status: domain.PostingPending, Error: fmt.Sprintf("general-ledger-svc status %d: %s", resp.StatusCode, string(raw))}, nil
	}
}

type Dispatcher struct {
	store     store.Store
	client    Client
	principal string
	log       *zap.Logger
	interval  time.Duration
	batch     int
	now       func() time.Time
}

// New builds a dispatcher; principalID is the service identity granted
// GL_POSTING_EXECUTE in general-ledger-svc.
func New(st store.Store, c Client, principalID string, log *zap.Logger) *Dispatcher {
	return &Dispatcher{store: st, client: c, principal: principalID, log: log, interval: 3 * time.Second, batch: 25, now: time.Now}
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

// RunOnce delivers every due PENDING request.
func (d *Dispatcher) RunOnce(ctx context.Context) int {
	n, err := d.store.DispatchPostings(ctx, d.batch, func(p domain.PostingRequest) domain.PostingOutcome {
		o, err := d.client.Post(ctx, p.TenantID, d.principal, p.Payload)
		if err != nil {
			o = domain.PostingOutcome{Status: domain.PostingPending, Error: err.Error()}
		}
		if o.Status == domain.PostingPending {
			o.NextAttempt = backoff(p.Attempts, d.now())
			d.log.Warn("accounting dispatch: will retry", zap.String("source_event_id", p.SourceEventID), zap.String("reason", o.Error))
		}
		return o
	})
	if err != nil {
		d.log.Error("accounting dispatch failed", zap.Error(err))
	}
	return n
}
