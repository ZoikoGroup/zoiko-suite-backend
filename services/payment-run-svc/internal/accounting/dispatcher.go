// Package accounting submits payment-settlement posting requests to ACC-04.
//
// AP-11 never writes the ledger. When BNK-07 settles an instruction the
// service records an accounting_posting_requests row in the same transaction
// as the status change; this dispatcher then submits each pending row to
// general-ledger-svc's posting engine (POST /v1/postings/events). The engine
// is idempotent on source_event_id, so a retry after a lost response, a
// second replica, or a replayed settlement can never post twice.
//
// Outcomes: 200/201 → POSTED; a 4xx the engine will not accept (an
// unresolvable ACC-02 mapping, a locked period, a refused permission) →
// QUARANTINED with the reason, waiting for an operator to fix the cause and
// requeue; anything else (transport error, 5xx, 429) is transient and is
// retried until MaxPostingAttempts, after which the request shows as FAILED.
package accounting

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"zoiko.io/payment-run-svc/internal/store"
)

// Dispatcher posts pending requests on an interval.
type Dispatcher struct {
	store     *store.PgStore
	ledgerURL string
	principal string
	http      *http.Client
	interval  time.Duration
	batch     int
	log       *zap.Logger
}

// New builds a dispatcher. principal is the service identity sent as
// X-Principal-Id; it must hold the ledger's GL_POSTING_EXECUTE action.
func New(st *store.PgStore, ledgerURL, principal string, interval time.Duration, batch int, log *zap.Logger) *Dispatcher {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if batch <= 0 {
		batch = 20
	}
	return &Dispatcher{
		store: st, ledgerURL: strings.TrimRight(ledgerURL, "/"), principal: principal,
		http: &http.Client{Timeout: 10 * time.Second}, interval: interval, batch: batch, log: log,
	}
}

// Start dispatches until ctx ends.
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

// RunOnce submits one batch and returns how many were POSTED.
func (d *Dispatcher) RunOnce(ctx context.Context) int {
	n, err := d.store.DispatchPending(ctx, d.batch, d.post)
	if err != nil {
		d.log.Error("accounting dispatch failed", zap.Error(err))
	}
	return n
}

type executionResponse struct {
	ExecutionID   string  `json:"execution_id"`
	Status        string  `json:"status"`
	FailureReason *string `json:"failure_reason,omitempty"`
}

func (d *Dispatcher) post(ctx context.Context, r store.PostingRequest) store.PostingResult {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.ledgerURL+"/v1/postings/events", bytes.NewReader(r.Payload))
	if err != nil {
		return store.PostingResult{Err: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", r.TenantID)
	req.Header.Set("X-Principal-Id", d.principal)

	resp, err := d.http.Do(req)
	if err != nil {
		d.log.Warn("general-ledger-svc unreachable; will retry", zap.String("source_event_id", r.SourceEventID), zap.Error(err))
		return store.PostingResult{Err: "ledger unreachable: " + err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	switch {
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated:
		var ex executionResponse
		_ = json.Unmarshal(body, &ex)
		// A 200 can also be the replay of an execution that failed earlier.
		if ex.Status == "FAILED" || ex.Status == "QUARANTINED" {
			reason := ex.Status
			if ex.FailureReason != nil {
				reason += ": " + *ex.FailureReason
			}
			return store.PostingResult{Final: true, Err: "ledger recorded the posting as " + reason}
		}
		return store.PostingResult{Final: true, Posted: true, ExecutionID: ex.ExecutionID}
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode >= 500:
		return store.PostingResult{Err: fmt.Sprintf("ledger answered %d", resp.StatusCode)}
	default:
		// 400/401/403/404/409/422: a retry will not change the answer.
		msg := strings.TrimSpace(string(body))
		if len(msg) > 500 {
			msg = msg[:500]
		}
		d.log.Error("ledger refused the posting request", zap.String("source_event_id", r.SourceEventID), zap.Int("status", resp.StatusCode), zap.String("body", msg))
		return store.PostingResult{Final: true, Err: fmt.Sprintf("ledger refused (%d): %s", resp.StatusCode, msg)}
	}
}
