package kit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"
)

// Spec invariant 14: "All accounting consequences enter through governed
// accounting events and ACC-04; AR services never write directly to the GL."
//
// A module that has an accounting consequence calls QueuePosting in the SAME
// transaction as its state change, so an issued invoice / issued note / applied
// cash / committed write-off always has a durable posting obligation. The
// PostingDispatcher is the only thing that talks to general-ledger-svc
// POST /v1/postings/events, which is idempotent on source_event_id.

// PostingLine is one side of an accounting event. Lines carry mapping keys, never
// account codes: general-ledger-svc (ACC-02) resolves them per tenant.
type PostingLine struct {
	MappingKey   string  `json:"mapping_key"`
	Debit        Amount  `json:"-"`
	Credit       Amount  `json:"-"`
	Description  string  `json:"description,omitempty"`
	DebitAmount  float64 `json:"debit_amount,omitempty"`
	CreditAmount float64 `json:"credit_amount,omitempty"`
}

// PostingEvent is the frozen body of PostAccountingEvent.
type PostingEvent struct {
	LegalEntityID       string        `json:"legal_entity_id"`
	FiscalPeriod        string        `json:"fiscal_period"`
	Description         string        `json:"description"`
	SourceEventID       string        `json:"source_event_id"`
	CorrelationID       string        `json:"correlation_id"`
	TransactionCurrency string        `json:"transaction_currency"`
	DocumentDate        string        `json:"document_date"`
	PostingDate         string        `json:"posting_date"`
	Lines               []PostingLine `json:"lines"`
}

// Posting source types — one per accounting consequence in spec §17.
const (
	SourceInvoice         = "INVOICE"
	SourceCreditNote      = "CREDIT_NOTE"
	SourceDebitNote       = "DEBIT_NOTE"
	SourceCashApplication = "CASH_APPLICATION"
	SourceCashUnapply     = "CASH_UNAPPLICATION"
	SourceRefund          = "REFUND"
	SourceWriteOff        = "WRITE_OFF"
)

// Posting statuses mirror accounts-payable-svc's queue.
const (
	PostingPending     = "PENDING"
	PostingInProgress  = "IN_PROGRESS"
	PostingPosted      = "POSTED"
	PostingFailed      = "FAILED"
	PostingQuarantined = "QUARANTINED"
)

// MaxPostingAttempts caps transient retries; after it a request becomes FAILED
// (visible), never retried forever.
const MaxPostingAttempts = 10

// ErrPostingUnbalanced is returned when debits and credits differ.
var ErrPostingUnbalanced = Invalid("accounting event is not balanced: debits must equal credits")

// Validate checks the event balances in exact arithmetic and has no negative
// or one-sided-and-two-sided lines.
func (e *PostingEvent) Validate() error {
	if len(e.Lines) < 2 {
		return Invalid("accounting event needs at least two lines")
	}
	var dr, cr Amount
	for i := range e.Lines {
		l := &e.Lines[i]
		if l.MappingKey == "" {
			return Invalid(fmt.Sprintf("line %d has no mapping_key", i+1))
		}
		if l.Debit.IsNegative() || l.Credit.IsNegative() {
			return Invalid(fmt.Sprintf("line %d has a negative amount", i+1))
		}
		if (l.Debit.IsZero()) == (l.Credit.IsZero()) {
			return Invalid(fmt.Sprintf("line %d must carry exactly one of debit or credit", i+1))
		}
		dr, cr = dr+l.Debit, cr+l.Credit
		l.DebitAmount, l.CreditAmount = l.Debit.Float(), l.Credit.Float()
	}
	if dr != cr {
		return ErrPostingUnbalanced
	}
	return nil
}

// QueuedPosting is one row of ar_accounting_posting_requests.
type QueuedPosting struct {
	RequestID     string
	TenantID      string
	LegalEntityID string
	SourceType    string
	SourceID      string
	SourceEventID string
	Payload       json.RawMessage
	PrincipalID   string
	CorrelationID string
	Attempts      int
}

// QueuePosting writes the posting obligation in the caller's transaction.
// sourceEventID must be stable per business fact (e.g. the invoice id, or
// "<application_id>:apply") — it is general-ledger-svc's idempotency key.
func QueuePosting(ctx context.Context, tx pgx.Tx, c Caller, sourceType, sourceID string, ev PostingEvent) error {
	if ev.SourceEventID == "" {
		return Invalid("accounting event needs a source_event_id")
	}
	ev.LegalEntityID = c.LegalEntityID
	if ev.CorrelationID == "" {
		ev.CorrelationID = c.CorrelationID
	}
	if err := ev.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO ar_accounting_posting_requests (
			request_id, tenant_id, legal_entity_id, source_type, source_id, source_event_id,
			request_payload, principal_id, correlation_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (tenant_id, source_event_id) DO NOTHING`,
		uuid.NewString(), c.TenantID, c.LegalEntityID, sourceType, sourceID, ev.SourceEventID,
		payload, c.PrincipalID, c.CorrelationID)
	return err
}

// PostingOutcome is what the Accounting Kernel answered for one request.
type PostingOutcome struct {
	Status      string // PENDING | POSTED | FAILED | QUARANTINED
	ExecutionID string
	JournalID   string
	Error       string
}

// PostingListener lets the module that owns a source type mirror the outcome
// onto its own aggregate (e.g. invoice accounting_state). It runs BEFORE the
// queue row is closed: if it fails the row stays claimed, its lease lapses and
// the next pass re-delivers (ACC-04 is idempotent, so that is safe).
type PostingListener interface {
	OnPostingOutcome(ctx context.Context, q QueuedPosting, o PostingOutcome) error
}

// LedgerPoster posts one event to the Accounting Kernel.
type LedgerPoster interface {
	Post(ctx context.Context, tenantID, principalID string, payload []byte) (PostingOutcome, error)
}

// HTTPLedgerPoster is the general-ledger-svc ACC-04 client.
type HTTPLedgerPoster struct {
	baseURL string
	http    *http.Client
}

func NewHTTPLedgerPoster(baseURL string) *HTTPLedgerPoster {
	return &HTTPLedgerPoster{baseURL: baseURL, http: &http.Client{Timeout: 10 * time.Second}}
}

type glExecution struct {
	ExecutionID   string  `json:"execution_id"`
	Status        string  `json:"status"`
	JournalID     *string `json:"journal_id"`
	FailureReason *string `json:"failure_reason"`
}

// Post maps any HTTP answer to an outcome; only a transport failure is an error
// (and is always retryable). Outcome map: COMMITTED→POSTED; FAILED or 400→FAILED;
// QUARANTINED or 422→QUARANTINED (ambiguous mapping: needs a human); 412 (period
// locked), 5xx, transport → stays PENDING with backoff.
func (c *HTTPLedgerPoster) Post(ctx context.Context, tenantID, principalID string, payload []byte) (PostingOutcome, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/postings/events", bytes.NewReader(payload))
	if err != nil {
		return PostingOutcome{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)
	resp, err := c.http.Do(req)
	if err != nil {
		return PostingOutcome{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch {
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated:
		var ex glExecution
		if err := json.Unmarshal(raw, &ex); err != nil {
			return PostingOutcome{}, fmt.Errorf("unreadable general-ledger-svc response: %w", err)
		}
		o := PostingOutcome{ExecutionID: ex.ExecutionID}
		if ex.JournalID != nil {
			o.JournalID = *ex.JournalID
		}
		if ex.FailureReason != nil {
			o.Error = *ex.FailureReason
		}
		switch ex.Status {
		case "COMMITTED":
			o.Status = PostingPosted
		case "FAILED":
			o.Status = PostingFailed
		case "QUARANTINED":
			o.Status = PostingQuarantined
		default:
			o.Status, o.Error = PostingPending, "execution status "+ex.Status
		}
		return o, nil
	case resp.StatusCode == http.StatusUnprocessableEntity:
		return PostingOutcome{Status: PostingQuarantined, Error: string(raw)}, nil
	case resp.StatusCode == http.StatusBadRequest:
		return PostingOutcome{Status: PostingFailed, Error: string(raw)}, nil
	default:
		return PostingOutcome{Status: PostingPending, Error: fmt.Sprintf("general-ledger-svc status %d: %s", resp.StatusCode, string(raw))}, nil
	}
}

// PostingDispatcher drains ar_accounting_posting_requests.
type PostingDispatcher struct {
	db        *DB
	ledger    LedgerPoster
	principal string
	log       *zap.Logger
	interval  time.Duration
	batch     int
	lease     time.Duration
	now       func() time.Time

	mu        sync.RWMutex
	listeners map[string]PostingListener
}

// NewPostingDispatcher builds a dispatcher. principalID, when set, is the service
// identity granted GL_POSTING_EXECUTE; when empty each posting is made as the
// principal recorded on the request.
func NewPostingDispatcher(db *DB, l LedgerPoster, principalID string, log *zap.Logger) *PostingDispatcher {
	return &PostingDispatcher{db: db, ledger: l, principal: principalID, log: log,
		interval: 3 * time.Second, batch: 25, lease: 60 * time.Second, now: time.Now,
		listeners: map[string]PostingListener{}}
}

// Register attaches the owning module's listener for a source type.
func (d *PostingDispatcher) Register(sourceType string, l PostingListener) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.listeners[sourceType] = l
}

func (d *PostingDispatcher) Start(ctx context.Context) {
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

func postingBackoff(attempts int, now time.Time) time.Time {
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
func (d *PostingDispatcher) RunOnce(ctx context.Context) int {
	reqs, err := d.claim(ctx)
	if err != nil {
		d.log.Error("ar posting dispatch: claim failed", zap.Error(err))
		return 0
	}
	for _, q := range reqs {
		d.deliver(ctx, q)
	}
	return len(reqs)
}

func (d *PostingDispatcher) claim(ctx context.Context) ([]QueuedPosting, error) {
	rows, err := d.db.Pool.Query(ctx, `
		UPDATE ar_accounting_posting_requests
		   SET status = 'IN_PROGRESS', attempts = attempts + 1, locked_until = now() + $2::interval, updated_at = now()
		 WHERE request_id IN (
			SELECT request_id FROM ar_accounting_posting_requests
			 WHERE status IN ('PENDING','IN_PROGRESS') AND next_attempt_at <= now()
			   AND (locked_until IS NULL OR locked_until < now())
			 ORDER BY next_attempt_at LIMIT $1 FOR UPDATE SKIP LOCKED)
		RETURNING request_id::text, tenant_id::text, legal_entity_id::text, source_type, source_id,
		          source_event_id, request_payload, principal_id, correlation_id, attempts`,
		d.batch, d.lease.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueuedPosting
	for rows.Next() {
		var q QueuedPosting
		if err := rows.Scan(&q.RequestID, &q.TenantID, &q.LegalEntityID, &q.SourceType, &q.SourceID,
			&q.SourceEventID, &q.Payload, &q.PrincipalID, &q.CorrelationID, &q.Attempts); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

func (d *PostingDispatcher) deliver(ctx context.Context, q QueuedPosting) {
	principal := d.principal
	if principal == "" {
		principal = q.PrincipalID
	}
	o, err := d.ledger.Post(ctx, q.TenantID, principal, q.Payload)
	if err != nil {
		o = PostingOutcome{Status: PostingPending, Error: err.Error()}
	}
	d.finish(ctx, q, o)
}

func (d *PostingDispatcher) finish(ctx context.Context, q QueuedPosting, o PostingOutcome) {
	retryAt := d.now()
	if o.Status == PostingPending {
		if q.Attempts >= MaxPostingAttempts {
			o.Status = PostingFailed
			o.Error = fmt.Sprintf("gave up after %d attempts: %s", q.Attempts, o.Error)
		} else {
			retryAt = postingBackoff(q.Attempts, d.now())
			d.log.Warn("ar posting dispatch: will retry", zap.String("source_event_id", q.SourceEventID), zap.String("reason", o.Error))
		}
	}
	if o.Status != PostingPending {
		d.mu.RLock()
		l := d.listeners[q.SourceType]
		d.mu.RUnlock()
		if l != nil {
			if err := l.OnPostingOutcome(ctx, q, o); err != nil {
				d.log.Error("ar posting dispatch: owner could not record the outcome; will re-deliver",
					zap.String("source_type", q.SourceType), zap.String("source_id", q.SourceID), zap.Error(err))
				return
			}
		}
	}
	_, err := d.db.Pool.Exec(ctx, `
		UPDATE ar_accounting_posting_requests
		   SET status = $2::varchar, posting_execution_id = COALESCE(NULLIF($3,''), posting_execution_id),
		       journal_id = COALESCE(NULLIF($4,''), journal_id), last_error = NULLIF($5,''),
		       next_attempt_at = $6, locked_until = NULL, updated_at = now(),
		       completed_at = CASE WHEN $2::text IN ('POSTED','FAILED','QUARANTINED') THEN now() ELSE NULL END
		 WHERE request_id::text = $1`,
		q.RequestID, o.Status, o.ExecutionID, o.JournalID, o.Error, retryAt)
	if err != nil {
		d.log.Error("ar posting dispatch: could not record the outcome", zap.String("request_id", q.RequestID), zap.Error(err))
	}
}

// PostingStatusFor returns the queue state for a source fact, for the
// Get...AccountingStatus queries (spec AR-04/AR-06). found=false means no posting
// was ever queued — itself a control exception for an issued document.
func (d *DB) PostingStatusFor(ctx context.Context, tenantID, sourceEventID string) (status, journalID, lastError string, found bool, err error) {
	err = d.WithTx(ctx, tenantID, func(tx pgx.Tx) error {
		var j, e *string
		qerr := tx.QueryRow(ctx, `
			SELECT status, journal_id, last_error FROM ar_accounting_posting_requests
			 WHERE tenant_id = $1 AND source_event_id = $2`, tenantID, sourceEventID).Scan(&status, &j, &e)
		if qerr == pgx.ErrNoRows {
			return nil
		}
		if qerr != nil {
			return qerr
		}
		found = true
		if j != nil {
			journalID = *j
		}
		if e != nil {
			lastError = *e
		}
		return nil
	})
	return
}
