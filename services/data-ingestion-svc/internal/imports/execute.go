package imports

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"zoiko.io/eventing/integration"
)

// TargetCall is one command to an owning domain.
type TargetCall struct {
	Jurisdiction   string
	TargetKey      string
	Method         string
	Path           string
	Body           []byte
	TenantID       string
	LegalEntityID  string
	Principal      string
	Correlation    string
	IdempotencyKey string
	RequestID      string
	SourceSystem   string
}

// TargetResult is the owning domain's answer. Err is set only when no answer was obtained
// (network failure, timeout); a non-2xx status is an answer.
type TargetResult struct {
	Status int
	Body   []byte
	Err    error
}

// TargetClient sends commands to the owning domains. Has says whether this deployment is
// configured for a target key: a profile can only name a target the operator has set up.
type TargetClient interface {
	Has(key string) bool
	Do(ctx context.Context, c TargetCall) TargetResult
}

// HTTPTargets is the HTTP TargetClient. The targets are the operator's configured base URLs, so
// a profile can never point the importer at an address of its own choosing.
type HTTPTargets struct {
	bases  map[string]string
	client *http.Client
}

// NewHTTPTargets builds the client from key -> base URL. A target answers within the timeout or
// not at all, and a redirect is never followed.
func NewHTTPTargets(bases map[string]string, timeout time.Duration) (*HTTPTargets, error) {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	clean := make(map[string]string, len(bases))
	for k, v := range bases {
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("target %q: base URL must be an absolute http(s) URL with no credentials, query or fragment", k)
		}
		clean[k] = strings.TrimRight(v, "/")
	}
	return &HTTPTargets{bases: clean, client: &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// Has implements TargetClient.
func (h *HTTPTargets) Has(key string) bool { _, ok := h.bases[key]; return ok }

// maxTargetResponse bounds what is read back from a domain.
const maxTargetResponse = 64 << 10

// Do implements TargetClient. It stamps the canonical input contract the owning domains enforce:
// who is acting (the person who executed the import), the tenant and entity, the trace ids, the
// import channel and source system, and the deterministic idempotency key that makes a retry safe.
func (h *HTTPTargets) Do(ctx context.Context, c TargetCall) TargetResult {
	base, ok := h.bases[c.TargetKey]
	if !ok {
		return TargetResult{Err: ErrTargetNotConfigured}
	}
	req, err := http.NewRequestWithContext(ctx, c.Method, base+c.Path, bytes.NewReader(c.Body))
	if err != nil {
		return TargetResult{Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", c.TenantID)
	req.Header.Set("X-Legal-Entity-Id", c.LegalEntityID)
	req.Header.Set("X-Principal-Id", c.Principal)
	req.Header.Set("X-Request-Id", c.RequestID)
	req.Header.Set("X-Correlation-Id", c.Correlation)
	req.Header.Set("X-Source-Channel", "import")
	req.Header.Set("X-Source-System", c.SourceSystem)
	if c.Jurisdiction != "" {
		req.Header.Set("X-Jurisdiction-Context", c.Jurisdiction)
	}
	req.Header.Set("X-Purpose-Context", "bulk-import")
	req.Header.Set("X-Timezone", "UTC")
	req.Header.Set("Idempotency-Key", c.IdempotencyKey)
	resp, err := h.client.Do(req)
	if err != nil {
		return TargetResult{Err: err}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxTargetResponse))
	return TargetResult{Status: resp.StatusCode, Body: body}
}

// Executor delivers the items of IMPORTING batches to their owning domains.
type Executor struct {
	svc *Service
	log *zap.Logger
	obs ExecObserver
}

// ExecObserver receives execution telemetry.
type ExecObserver interface {
	Item(outcome string)
}

type nopExecObserver struct{}

func (nopExecObserver) Item(string) {}

// NewExecutor builds an Executor over a Service.
func NewExecutor(svc *Service, log *zap.Logger, obs ExecObserver) *Executor {
	if log == nil {
		log = zap.NewNop()
	}
	if obs == nil {
		obs = nopExecObserver{}
	}
	return &Executor{svc: svc, log: log, obs: obs}
}

// Run delivers until ctx is cancelled.
func (e *Executor) Run(ctx context.Context, idle time.Duration) {
	if idle <= 0 {
		idle = 2 * time.Second
	}
	for {
		n, err := e.RunOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			e.log.Warn("import executor: round failed", zap.Error(err))
		}
		if n == 0 || err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(idle):
			}
		}
	}
}

// RunOnce makes one round over the batches that have work and returns how many items it handled.
func (e *Executor) RunOnce(ctx context.Context) (int, error) {
	refs, err := e.svc.repo.DueExecutions(ctx, e.svc.now(), 20)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, r := range refs {
		n, err := e.ProcessBatch(ctx, r)
		total += n
		if err != nil {
			e.log.Warn("import executor: batch failed", zap.String("batch_id", r.BatchID), zap.Error(err))
		}
	}
	return total, nil
}

// ProcessBatch claims a round of due items for the batch, delivers them with a bounded worker
// pool, and finishes the batch when nothing is left to do.
func (e *Executor) ProcessBatch(ctx context.Context, ref Ref) (int, error) {
	s := e.svc
	b, err := s.repo.GetBatch(ctx, ref.TenantID, ref.BatchID)
	if err != nil {
		return 0, err
	}
	if b.Status != StatusImporting {
		return 0, nil
	}
	p, err := s.profileOf(ctx, b)
	if err != nil {
		return 0, err
	}
	spec := p.Spec.Normalize()
	items, err := s.repo.ClaimItems(ctx, ref.TenantID, ref.BatchID, s.now(), s.cfg.ClaimBatch, s.cfg.ItemLease)
	if err != nil {
		return 0, err
	}
	sem := make(chan struct{}, s.cfg.ExecWorkers)
	var wg sync.WaitGroup
	for i := range items {
		it := items[i]
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					e.log.Error("import executor: item panicked", zap.Any("panic", r))
				}
			}()
			e.deliver(ctx, b, spec, it)
		}()
	}
	wg.Wait()
	if err := e.finishIfDone(ctx, b); err != nil {
		return len(items), err
	}
	return len(items), nil
}

func (e *Executor) deliver(ctx context.Context, b *Batch, spec Spec, it Item) {
	s := e.svc
	body, err := spec.BuildBody(it.Values, b.TenantID, b.LegalEntityID)
	if err != nil {
		e.finish(ctx, b, it, ItemResult{Status: ItemQuarantined, ErrorClass: "build_body", QuarantineReason: "MAPPING_ERROR", QuarantineOwner: b.CreatedBy})
		return
	}
	res := s.targets.Do(ctx, TargetCall{
		TargetKey: spec.Target.Key, Method: spec.Target.Method, Path: spec.Target.Path, Body: body,
		TenantID: b.TenantID, LegalEntityID: b.LegalEntityID, Principal: orDefault(b.ExecutedBy, b.CreatedBy), Correlation: b.CorrelationID,
		IdempotencyKey: it.IdempotencyKey, RequestID: fmt.Sprintf("%s:%d", it.IdempotencyKey, it.Attempts), SourceSystem: b.SourceSystem, Jurisdiction: b.JurisdictionContext,
	})
	out := classifyTarget(spec, res)
	attempts := it.Attempts // ClaimItems counts the attempt it hands out, so this one is included
	switch {
	case out.Status == ItemPending:
		if attempts >= s.cfg.ItemMaxAttempts {
			out.Status, out.ErrorClass = ItemFailed, out.ErrorClass+"_exhausted"
			out.NextAttemptAt = s.now()
		} else {
			out.NextAttemptAt = s.now().Add(backoff(s.cfg, attempts))
		}
	case out.Status == ItemQuarantined:
		out.QuarantineOwner = b.CreatedBy
	}
	if out.Status == ItemImported && spec.Target.ResultRefField != "" {
		out.ResultRef = ExtractRef(spec.Target.ResultRefField, res.Body)
	}
	e.finish(ctx, b, it, out)
}

func orDefault(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (e *Executor) finish(ctx context.Context, b *Batch, it Item, out ItemResult) {
	var events []Event
	if out.Status == ItemQuarantined {
		events = append(events, Event{Type: EventItemQuarantined, Payload: map[string]any{
			"batch_id": b.BatchID, "row_no": it.RowNo, "reason": out.QuarantineReason, "owner": out.QuarantineOwner, "domain_status": out.DomainStatus,
		}})
	}
	e.obs.Item(string(out.Status))
	if err := e.svc.repo.FinishItem(ctx, b.TenantID, b.BatchID, it.RowNo, out, events); err != nil {
		e.log.Error("import executor: cannot record an item outcome", zap.String("batch_id", b.BatchID), zap.Int("row", it.RowNo), zap.Error(err))
	}
}

func backoff(c Config, attempts int) time.Duration {
	d := c.ItemRetryBase
	for i := 1; i < attempts && d < c.ItemRetryCap; i++ {
		d *= 2
	}
	if d > c.ItemRetryCap {
		d = c.ItemRetryCap
	}
	return d
}

// classifyTarget decides what an owning domain's answer means for an item.
//
//	2xx in the profile's success codes   imported (a replay of the same idempotency key answers
//	                                     with the original result, so a retry is also "imported")
//	408, 425, 429, 5xx, no answer        transient: retried with backoff, then FAILED
//	any other 4xx, or a redirect         the domain refused it: quarantined with the reason and an
//	                                     owner, never discarded, never retried blindly
func classifyTarget(spec Spec, r TargetResult) ItemResult {
	if r.Err != nil {
		class := "network_error"
		var ne interface{ Timeout() bool }
		if errors.Is(r.Err, context.DeadlineExceeded) || os.IsTimeout(r.Err) || (errors.As(r.Err, &ne) && ne.Timeout()) {
			class = "timeout"
		}
		return ItemResult{Status: ItemPending, ErrorClass: class}
	}
	code := r.Status
	st := code
	for _, ok := range spec.Target.SuccessCodes {
		if code == ok {
			return ItemResult{Status: ItemImported, DomainStatus: &st}
		}
	}
	switch {
	case code == 408 || code == 425 || code == 429 || code >= 500:
		return ItemResult{Status: ItemPending, DomainStatus: &st, ErrorClass: fmt.Sprintf("http_%d", code)}
	case code >= 300 && code < 400:
		return ItemResult{Status: ItemQuarantined, DomainStatus: &st, ErrorClass: "target_redirect", QuarantineReason: "TARGET_REDIRECT"}
	}
	return ItemResult{Status: ItemQuarantined, DomainStatus: &st, ErrorClass: fmt.Sprintf("http_%d", code), QuarantineReason: fmt.Sprintf("DOMAIN_REJECTED_%d", code)}
}

// finishIfDone completes the batch once every valid item has been delivered or has ended in an
// explicit state. A batch with items that are not imported is COMPLETED_WITH_EXCEPTIONS, never
// COMPLETED: partial failure is reported, not hidden.
func (e *Executor) finishIfDone(ctx context.Context, b *Batch) error {
	s := e.svc
	pending, err := s.repo.Pending(ctx, b.TenantID, b.BatchID)
	if err != nil || pending > 0 {
		return err
	}
	counts, err := s.repo.ItemCounts(ctx, b.TenantID, b.BatchID)
	if err != nil {
		return err
	}
	if counts[ItemValid] > 0 || counts[ItemPending] > 0 {
		return nil // not all items were activated, or one is in flight
	}
	status := StatusCompleted
	if counts[ItemQuarantined]+counts[ItemFailed]+counts[ItemSkipped] > 0 {
		status = StatusCompletedWithExceptions
	}
	now := s.now()
	imp, qua, fai, ski := counts[ItemImported], counts[ItemQuarantined], counts[ItemFailed], counts[ItemSkipped]
	a := Actor{TenantID: b.TenantID, PrincipalID: "system:import-executor", CorrelationID: b.CorrelationID}
	_, err = s.transition(ctx, a, b, []Status{StatusImporting}, status, "EXECUTION_FINISHED", "",
		Patch{Imported: &imp, Quarantined: &qua, Failed: &fai, Skipped: &ski, CompletedAt: &now},
		batchEvent(EventCompleted, b, map[string]any{"imported": imp, "quarantined": qua, "failed": fai, "skipped": ski, "outcome": string(status)}))
	if errors.Is(err, ErrInvalidState) {
		return nil // another worker finished it first
	}
	return err
}

// ValidationRunner runs the scan/parse/validate pipeline for batches that asked for it.
type ValidationRunner struct {
	svc *Service
	log *zap.Logger
}

// NewValidationRunner builds a ValidationRunner over a Service.
func NewValidationRunner(svc *Service, log *zap.Logger) *ValidationRunner {
	if log == nil {
		log = zap.NewNop()
	}
	return &ValidationRunner{svc: svc, log: log}
}

// Run processes validation requests until ctx is cancelled.
func (v *ValidationRunner) Run(ctx context.Context, idle time.Duration) {
	if idle <= 0 {
		idle = 2 * time.Second
	}
	for {
		n, failed := v.RunOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		// A stage that could not complete (the scanner is down) waits for the idle interval
		// instead of being hammered in a tight loop.
		if n == 0 || failed {
			select {
			case <-ctx.Done():
				return
			case <-time.After(idle):
			}
		}
	}
}

// RunOnce processes one round and returns how many batches it touched and whether any stopped on an error.
func (v *ValidationRunner) RunOnce(ctx context.Context) (int, bool) {
	refs, err := v.svc.repo.DueValidations(ctx, 10)
	if err != nil {
		v.log.Warn("import validation: discovery failed", zap.Error(err))
		return 0, true
	}
	failed := false
	for _, r := range refs {
		if _, err := v.svc.RunPipeline(ctx, r.TenantID, r.BatchID); err != nil {
			v.log.Warn("import validation: pipeline stopped", zap.String("batch_id", r.BatchID), zap.Error(err))
			failed = true
		}
	}
	return len(refs), failed
}

// Code maps an error to its stable INT-* code.
func Code(err error) integration.Code {
	switch {
	case errors.Is(err, ErrHashMismatch), errors.Is(err, ErrChunkConflict), errors.Is(err, ErrIncompleteUpload),
		errors.Is(err, ErrTooLarge), errors.Is(err, ErrPreviewMismatch), errors.Is(err, ErrSchemaMismatch):
		return integration.CodeFileIntegrity
	case errors.Is(err, ErrDuplicateBatch), errors.Is(err, ErrVersionConflict):
		return integration.CodeIdempotencyConflict
	case errors.Is(err, ErrReconciliationFailed):
		return integration.CodeReconciliationBreak
	case errors.Is(err, ErrSelfApproval), errors.Is(err, ErrProfileSelfCertify), errors.Is(err, ErrNotPlatformScope):
		return integration.CodeAuthDenied
	case errors.Is(err, ErrNoScanner), errors.Is(err, ErrScannerUnavailable):
		return integration.CodeQuarantined
	case errors.Is(err, ErrInvalidState), errors.Is(err, ErrProfileNotCertified), errors.Is(err, ErrTargetNotConfigured), errors.Is(err, ErrRetryNotAllowed):
		return integration.CodeScopeDenied
	case errors.Is(err, ErrValidation):
		return integration.CodeSchemaMismatch
	}
	return ""
}
