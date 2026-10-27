// Package periodmirror is financial-close-svc's best-effort DUAL-WRITE of period
// close/reopen into accounting-period-svc (REF-05), REF-05 cutover phase 1.
//
// financial-close-svc REMAINS AUTHORITATIVE in this phase. Nothing here may
// change, fail, or materially delay the local operation: every method swallows
// its errors (logging at error level and counting them), runs under a short
// total deadline, and does nothing at all -- not even a DNS lookup -- unless
// the mirror is enabled (PERIOD_SERVICE_MIRROR=on).
//
// The protocol per command is: record an ACC-14 workflow ref
// (close_workflow_refs) in its own committed transaction, THEN call REF-05 with
// that ref; REF-05 calls back GET /v1/close/workflow-refs/{ref} to verify it.
package periodmirror

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/financial-close-svc/internal/domain"
	svcmiddleware "zoiko.io/financial-close-svc/internal/middleware"
)

// WorkloadID is the identity this service presents to REF-05 (X-Workload-Id) and
// the requested_by of the SOFT_CLOSE step. See SPEC_DEVIATIONS_REF05.md.
const WorkloadID = "financial-close-svc"

// Outcomes of one mirrored step.
const (
	OutcomeApplied = "applied"
	OutcomeSkipped = "skipped"
	OutcomeFailed  = "failed"
)

// Failure reasons (the `reason` label of close_period_mirror_failures_total).
const (
	ReasonPeriodNotFound    = "period_not_found"
	ReasonAmbiguous         = "ambiguous"
	ReasonUnreachable       = "unreachable"
	ReasonTimeout           = "timeout"
	ReasonHTTP5xx           = "http_5xx"
	ReasonVersionConflict   = "version_conflict"
	ReasonInvalidTransition = "invalid_transition"
	ReasonSourceUnverified  = "source_unverified"
	ReasonSoDDenied         = "sod_denied"
	ReasonForbidden         = "forbidden"
	ReasonUnauthenticated   = "unauthenticated"
	ReasonRejected          = "rejected"
	ReasonBadResponse       = "bad_response"
	ReasonRefStore          = "ref_store"
	ReasonStateMismatch     = "state_mismatch"
	ReasonReadinessUnavail  = "readiness_unavailable"
	ReasonDisabled          = "disabled"
)

const (
	defaultTotalTimeout      = 5 * time.Second
	defaultPerRequestTimeout = 3 * time.Second
	maxResponseBytes         = 1 << 20
	idempotencyKeyPrefix     = "close-mirror-"
	headerIdempotencyKey     = "Idempotency-Key"
	reopenScopeEntirePeriod  = `{}`
	resolveDateLayout        = "2006-01-02"
	reasonSoftCloseFmt       = "ACC-14 period close: soft close of %s (readiness passed; evidence document %q)"
	reasonHardCloseFmt       = "ACC-14 period lock: hard close of %s (evidence document %q)"
	reasonRecloseFmt         = "ACC-14 period lock after authorised reopen: reclose of %s (evidence document %q)"
	reasonReplayPrefix       = "Replay of legacy ACC-14 state into REF-05: "
)

// RefStore persists workflow refs. Each CreateWorkflowRef MUST commit before it
// returns (the PgStore implementation does), because REF-05 reads the row back
// over HTTP while the command is still in flight.
type RefStore interface {
	CreateWorkflowRef(ctx context.Context, wr *domain.WorkflowRef) error
}

// Config configures a Mirror. Zero Timeout/PerRequestTimeout take safe defaults.
type Config struct {
	Enabled           bool
	BaseURL           string
	ReopenWindow      time.Duration
	Timeout           time.Duration // total budget of one mirror operation
	PerRequestTimeout time.Duration
}

// Request is one mirror operation's input.
type Request struct {
	TenantID      string
	CorrelationID string
	// Principal is the HUMAN principal: the requester of the HARD_CLOSE /
	// RECLOSE / AUTHORIZE_REOPEN step (the SOFT_CLOSE step is the workload's).
	Principal string
	// Period is the legacy fiscal period (its start date resolves the REF-05 period).
	Period domain.FiscalPeriod
	// EvidenceDocumentID is the close evidence document (for a reopen: the one
	// being superseded).
	EvidenceDocumentID string
	// Readiness is the readiness snapshot as of the command (nil for a reopen).
	Readiness *ReadinessSnapshot
	// Reason is the reopen reason; unused by closes.
	Reason string
	// Replay marks a backfill replay (changes the recorded reason text only).
	Replay bool
}

// Mirror drives REF-05. Safe for concurrent use.
type Mirror struct {
	cfg     Config
	refs    RefStore
	http    *http.Client
	metrics *Metrics
	log     *zap.Logger
	now     func() time.Time
}

// New builds a Mirror. httpClient may be nil (a default is used). metrics may be nil.
func New(cfg Config, refs RefStore, httpClient *http.Client, metrics *Metrics, log *zap.Logger) *Mirror {
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTotalTimeout
	}
	if cfg.PerRequestTimeout <= 0 {
		cfg.PerRequestTimeout = defaultPerRequestTimeout
	}
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	if log == nil {
		log = zap.NewNop()
	}
	if metrics == nil {
		metrics = NewMetrics(nil)
	}
	return &Mirror{cfg: cfg, refs: refs, http: httpClient, metrics: metrics, log: log, now: time.Now}
}

// Enabled reports whether the mirror will make outbound calls.
func (m *Mirror) Enabled() bool { return m != nil && m.cfg.Enabled }

// Fail records a mirror failure that happened outside the mirror (e.g. the
// readiness recomputation of a replay) and returns the matching action result.
func (m *Mirror) Fail(command, reason string) domain.MirrorActionResult {
	m.metrics.observe(command, OutcomeFailed, reason)
	return domain.MirrorActionResult{Command: command, Outcome: OutcomeFailed, Reason: reason}
}

// MirrorLock mirrors a successful local lock (legacy OPEN -> LOCKED): SOFT_CLOSE
// by the workload, then HARD_CLOSE by req.Principal (or RECLOSE if REF-05 has an
// authorised reopen outstanding). Never returns an error; see the package doc.
func (m *Mirror) MirrorLock(ctx context.Context, req Request) []domain.MirrorActionResult {
	return m.closeFlow(ctx, req, true)
}

// MirrorSoftClose is the soft-close-only flow, for replaying a legacy CLOSED period.
func (m *Mirror) MirrorSoftClose(ctx context.Context, req Request) []domain.MirrorActionResult {
	return m.closeFlow(ctx, req, false)
}

// MirrorReopen mirrors a successful local reopen as AUTHORIZE_REOPEN with
// reopen_scope {} (the entire period) expiring after the configured window.
func (m *Mirror) MirrorReopen(ctx context.Context, req Request) []domain.MirrorActionResult {
	if !m.Enabled() {
		return nil
	}
	ctx, cancel := m.opContext(ctx, req)
	defer cancel()
	fl := &flow{m: m, req: req}
	res, ferr := fl.resolve(ctx)
	if ferr != nil {
		return []domain.MirrorActionResult{fl.fail(domain.WorkflowCmdAuthorizeReopen, "", ferr)}
	}
	switch res.State {
	case "HARD_CLOSED", "RECLOSED":
		out, ferr := fl.command(ctx, res, domain.WorkflowCmdAuthorizeReopen, "authorize-reopen", req.Principal, req.Principal,
			fmt.Sprintf("ACC-14 period reopen of %s: %s", req.Period.PeriodName, req.Reason), nil)
		if ferr != nil {
			return []domain.MirrorActionResult{fl.fail(domain.WorkflowCmdAuthorizeReopen, out, ferr)}
		}
		return []domain.MirrorActionResult{fl.ok(domain.WorkflowCmdAuthorizeReopen, OutcomeApplied, out)}
	case "REOPEN_AUTHORIZED":
		return []domain.MirrorActionResult{fl.ok(domain.WorkflowCmdAuthorizeReopen, OutcomeSkipped, "")}
	default: // OPEN, SOFT_CLOSED: REF-05 has no transition back from here
		return []domain.MirrorActionResult{fl.fail(domain.WorkflowCmdAuthorizeReopen, "",
			&failure{reason: ReasonStateMismatch, detail: "REF-05 period is " + res.State + "; cannot authorise a reopen"})}
	}
}

func (m *Mirror) opContext(ctx context.Context, req Request) (context.Context, context.CancelFunc) {
	// Detached from the caller's cancellation (a client that hangs up must not
	// abandon half a mirror), but bounded, and still carrying the tenant.
	ctx = svcmiddleware.WithTenant(context.WithoutCancel(ctx), req.TenantID)
	return context.WithTimeout(ctx, m.cfg.Timeout)
}

func (m *Mirror) closeFlow(ctx context.Context, req Request, hard bool) []domain.MirrorActionResult {
	if !m.Enabled() {
		return nil
	}
	ctx, cancel := m.opContext(ctx, req)
	defer cancel()
	fl := &flow{m: m, req: req}
	res, ferr := fl.resolve(ctx)
	if ferr != nil {
		return []domain.MirrorActionResult{fl.fail(domain.WorkflowCmdSoftClose, "", ferr)}
	}
	name := req.Period.PeriodName
	doc := req.EvidenceDocumentID
	var out []domain.MirrorActionResult

	switch res.State {
	case "OPEN":
		ref, ferr := fl.command(ctx, res, domain.WorkflowCmdSoftClose, "request-soft-close", WorkloadID, "",
			fl.reason(reasonSoftCloseFmt, name, doc), req.Readiness)
		if ferr != nil {
			return append(out, fl.fail(domain.WorkflowCmdSoftClose, ref, ferr))
		}
		out = append(out, fl.ok(domain.WorkflowCmdSoftClose, OutcomeApplied, ref))
		if !hard {
			return out
		}
		// Re-read the version (and confirm the state) before the second command.
		if res, ferr = fl.resolve(ctx); ferr != nil {
			return append(out, fl.fail(domain.WorkflowCmdHardClose, "", ferr))
		}
		if res.State != "SOFT_CLOSED" {
			return append(out, fl.fail(domain.WorkflowCmdHardClose, "",
				&failure{reason: ReasonStateMismatch, detail: "REF-05 period is " + res.State + " after soft close"}))
		}
		return append(out, fl.hardClose(ctx, res))
	case "SOFT_CLOSED":
		out = append(out, fl.ok(domain.WorkflowCmdSoftClose, OutcomeSkipped, ""))
		if !hard {
			return out
		}
		return append(out, fl.hardClose(ctx, res))
	case "REOPEN_AUTHORIZED":
		// The legacy period was reopened (mirrored as AUTHORIZE_REOPEN) and has now
		// been locked again: that is REF-05's RECLOSE.
		if !hard {
			return []domain.MirrorActionResult{fl.ok(domain.WorkflowCmdSoftClose, OutcomeSkipped, "")}
		}
		ref, ferr := fl.command(ctx, res, domain.WorkflowCmdReclose, "reclose", req.Principal, req.Principal,
			fl.reason(reasonRecloseFmt, name, doc), req.Readiness)
		if ferr != nil {
			return []domain.MirrorActionResult{fl.fail(domain.WorkflowCmdReclose, ref, ferr)}
		}
		return []domain.MirrorActionResult{fl.ok(domain.WorkflowCmdReclose, OutcomeApplied, ref)}
	default: // HARD_CLOSED, RECLOSED: REF-05 is already closed. Idempotent no-op.
		out = append(out, fl.ok(domain.WorkflowCmdSoftClose, OutcomeSkipped, ""))
		if hard {
			out = append(out, fl.ok(domain.WorkflowCmdHardClose, OutcomeSkipped, ""))
		}
		return out
	}
}

// ── one mirror operation ─────────────────────────────────────────────────────

type failure struct {
	reason string
	detail string
}

func (f *failure) Error() string { return f.reason + ": " + f.detail }

type flow struct {
	m   *Mirror
	req Request
}

type resolution struct {
	PeriodID  string `json:"period_id"`
	PeriodKey string `json:"period_key"`
	State     string `json:"state"`
	Version   int64  `json:"version"`
}

func (fl *flow) reason(format, name, doc string) string {
	s := fmt.Sprintf(format, name, doc)
	if fl.req.Replay {
		return reasonReplayPrefix + s
	}
	return s
}

func (fl *flow) ok(command, outcome, ref string) domain.MirrorActionResult {
	fl.m.metrics.observe(command, outcome, "")
	return domain.MirrorActionResult{Command: command, Outcome: outcome, Ref: ref}
}

func (fl *flow) fail(command, ref string, f *failure) domain.MirrorActionResult {
	fl.m.metrics.observe(command, OutcomeFailed, f.reason)
	fl.m.log.Error("REF-05 period mirror failed; the local operation is unaffected",
		zap.String("command", command), zap.String("reason", f.reason), zap.String("detail", f.detail),
		zap.String("tenant_id", fl.req.TenantID), zap.String("fiscal_period_id", fl.req.Period.FiscalPeriodID),
		zap.String("workflow_ref", ref), zap.String("correlation_id", fl.req.CorrelationID))
	return domain.MirrorActionResult{Command: command, Outcome: OutcomeFailed, Ref: ref, Reason: f.reason}
}

func (fl *flow) hardClose(ctx context.Context, res *resolution) domain.MirrorActionResult {
	ref, ferr := fl.command(ctx, res, domain.WorkflowCmdHardClose, "hard-close", fl.req.Principal, fl.req.Principal,
		fl.reason(reasonHardCloseFmt, fl.req.Period.PeriodName, fl.req.EvidenceDocumentID), fl.req.Readiness)
	if ferr != nil {
		return fl.fail(domain.WorkflowCmdHardClose, ref, ferr)
	}
	return fl.ok(domain.WorkflowCmdHardClose, OutcomeApplied, ref)
}

func (fl *flow) correlation() string {
	if fl.req.CorrelationID != "" {
		return fl.req.CorrelationID
	}
	return uuid.NewString()
}

func (fl *flow) newRequest(ctx context.Context, method, rawURL string, body io.Reader) (*http.Request, error) {
	httpReq, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("X-Tenant-Id", fl.req.TenantID)
	httpReq.Header.Set("X-Workload-Id", WorkloadID)
	httpReq.Header.Set("X-Correlation-ID", fl.correlation())
	httpReq.Header.Set("X-Request-Id", uuid.NewString())
	httpReq.Header.Set("X-Source-Channel", "system")
	httpReq.Header.Set("X-Legal-Entity-Id", fl.req.Period.LegalEntityID)
	return httpReq, nil
}

// resolve is GET /v1/accounting-periods:resolve?legal_entity_id=&date=<period start>&purpose=read.
func (fl *flow) resolve(ctx context.Context) (*resolution, *failure) {
	q := url.Values{}
	q.Set("legal_entity_id", fl.req.Period.LegalEntityID)
	q.Set("date", fl.req.Period.PeriodStart.UTC().Format(resolveDateLayout))
	q.Set("purpose", "read")
	httpReq, err := fl.newRequest(ctx, http.MethodGet, fl.m.cfg.BaseURL+"/v1/accounting-periods:resolve?"+q.Encode(), nil)
	if err != nil {
		return nil, &failure{reason: ReasonBadResponse, detail: "cannot build resolve request: " + err.Error()}
	}
	status, body, ferr := fl.do(httpReq)
	if ferr != nil {
		return nil, ferr
	}
	if status != http.StatusOK {
		return nil, httpFailure(status, body)
	}
	var res resolution
	if err := json.Unmarshal(body, &res); err != nil || res.PeriodID == "" || res.PeriodKey == "" || res.State == "" || res.Version < 1 {
		return nil, &failure{reason: ReasonBadResponse, detail: "resolve answered 200 with an unusable body"}
	}
	return &res, nil
}

type commandBody struct {
	ExpectedVersion    int64           `json:"expected_version"`
	Reason             string          `json:"reason"`
	Acc14WorkflowRef   string          `json:"acc14_workflow_ref"`
	ControlSnapshotRef string          `json:"control_snapshot_ref"`
	ReopenScope        json.RawMessage `json:"reopen_scope,omitempty"`
	ExpiresAt          *time.Time      `json:"expires_at,omitempty"`
}

// command records the workflow ref (own committed tx), THEN POSTs the command
// to REF-05. It returns the ref id whenever one was created, even on failure.
func (fl *flow) command(ctx context.Context, res *resolution, cmd, path, requestedBy, principalHeader, reason string, readiness *ReadinessSnapshot) (string, *failure) {
	refID, err := uuid.NewV7()
	if err != nil {
		return "", &failure{reason: ReasonRefStore, detail: "cannot generate ref id: " + err.Error()}
	}
	snapshot := SnapshotRef(fl.req.Period.FiscalPeriodID, cmd, fl.req.EvidenceDocumentID, readiness)
	wr := &domain.WorkflowRef{
		RefID: refID.String(), TenantID: fl.req.TenantID, LegalEntityID: fl.req.Period.LegalEntityID,
		FiscalPeriodID: fl.req.Period.FiscalPeriodID, PeriodName: fl.req.Period.PeriodName, PeriodKey: res.PeriodKey,
		Command: cmd, Status: domain.WorkflowStatusApproved, ControlSnapshotRef: snapshot,
		RequestedBy: requestedBy, Reason: reason,
	}
	if err := fl.m.refs.CreateWorkflowRef(ctx, wr); err != nil {
		return "", &failure{reason: ReasonRefStore, detail: err.Error()}
	}

	body := commandBody{ExpectedVersion: res.Version, Reason: reason, Acc14WorkflowRef: wr.RefID, ControlSnapshotRef: snapshot}
	if cmd == domain.WorkflowCmdAuthorizeReopen {
		body.ReopenScope = json.RawMessage(reopenScopeEntirePeriod)
		exp := fl.m.now().UTC().Add(fl.m.cfg.ReopenWindow)
		body.ExpiresAt = &exp
	}
	payload, _ := json.Marshal(body)
	httpReq, err := fl.newRequest(ctx, http.MethodPost,
		fl.m.cfg.BaseURL+"/v1/accounting-periods/"+url.PathEscape(res.PeriodID)+":"+path, bytes.NewReader(payload))
	if err != nil {
		return wr.RefID, &failure{reason: ReasonBadResponse, detail: "cannot build command request: " + err.Error()}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(headerIdempotencyKey, idempotencyKeyPrefix+wr.RefID)
	if principalHeader != "" {
		httpReq.Header.Set("X-Principal-Id", principalHeader)
	}
	status, respBody, ferr := fl.do(httpReq)
	if ferr != nil {
		return wr.RefID, ferr
	}
	if status != http.StatusOK {
		return wr.RefID, httpFailure(status, respBody)
	}
	return wr.RefID, nil
}

// do sends one request under the per-request timeout.
func (fl *flow) do(httpReq *http.Request) (int, []byte, *failure) {
	ctx, cancel := context.WithTimeout(httpReq.Context(), fl.m.cfg.PerRequestTimeout)
	defer cancel()
	resp, err := fl.m.http.Do(httpReq.WithContext(ctx))
	if err != nil {
		reason := ReasonUnreachable
		if errors.Is(err, context.DeadlineExceeded) {
			reason = ReasonTimeout
		}
		return 0, nil, &failure{reason: reason, detail: err.Error()}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		reason := ReasonUnreachable
		if errors.Is(err, context.DeadlineExceeded) {
			reason = ReasonTimeout
		}
		return resp.StatusCode, nil, &failure{reason: reason, detail: "reading response: " + err.Error()}
	}
	return resp.StatusCode, b, nil
}

// httpFailure maps a non-200 REF-05 answer to a failure reason, using REF-05's
// {"code","message"} error body where present.
func httpFailure(status int, body []byte) *failure {
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &e)
	detail := fmt.Sprintf("HTTP %d %s: %s", status, e.Code, e.Message)
	f := &failure{detail: detail}
	switch {
	case status == http.StatusNotFound && (e.Code == "PERIOD_NOT_FOUND" || e.Code == "NOT_FOUND" || e.Code == ""):
		f.reason = ReasonPeriodNotFound
	case status == http.StatusConflict && e.Code == "RULE_AMBIGUOUS":
		f.reason = ReasonAmbiguous
	case status == http.StatusConflict && e.Code == "VERSION_CONFLICT":
		f.reason = ReasonVersionConflict
	case status == http.StatusConflict && e.Code == "INVALID_TRANSITION":
		f.reason = ReasonInvalidTransition
	case status == http.StatusForbidden && e.Code == "SOD_DENIED":
		f.reason = ReasonSoDDenied
	case status == http.StatusForbidden:
		f.reason = ReasonForbidden
	case status == http.StatusUnauthorized:
		f.reason = ReasonUnauthenticated
	case status == http.StatusUnprocessableEntity && e.Code == "SOURCE_UNVERIFIED":
		f.reason = ReasonSourceUnverified
	case status >= 500 || status == http.StatusTooManyRequests:
		f.reason = ReasonHTTP5xx
	default:
		f.reason = ReasonRejected
	}
	return f
}
