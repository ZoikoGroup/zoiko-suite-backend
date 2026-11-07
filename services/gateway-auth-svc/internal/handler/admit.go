package handler

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"
	apicontract "zoiko.io/identity-envelope/apicontract"

	"zoiko.io/gateway-auth-svc/internal/admission"
	"zoiko.io/gateway-auth-svc/internal/siem"
)

// Admission is what protects /verify before and around authentication
// (ZS-SVC-M-001 INT-01). Every field is optional: the zero value admits
// everything, which is how handler tests that are not about admission run.
type Admission struct {
	Policy        admission.Policy
	IPLimiter     *admission.Limiter // before authentication, keyed by hashed client address
	TenantLimiter *admission.Limiter // after authentication, keyed by tenant
	Gate          *admission.Gate
	Recorder      *admission.Recorder
}

// UseAdmission installs the admission controls. Separate from New so the
// existing constructor, and every caller and test of it, is unchanged.
func (h *Handler) UseAdmission(a Admission) *Handler {
	h.adm = a
	if a.Recorder != nil {
		h.rec = a.Recorder
	}
	return h
}

// Recorder returns the decision recorder the handler writes to, for the query
// surface.
func (h *Handler) Recorder() *admission.Recorder { return h.rec }

// Admission returns the installed controls, for the query surface.
func (h *Handler) Admission() Admission { return h.adm }

// reqCtx is what is known about the request being decided, built once so every
// refusal path records the same facts.
type reqCtx struct {
	method      string
	path        string
	apiVersion  string
	clientKey   string
	correlation string
	tenantID    string
	principalID string
}

func (h *Handler) requestContext(r *http.Request) *reqCtx {
	corr := strings.TrimSpace(r.Header.Get("X-Correlation-Id"))
	if corr == "" {
		corr = middleware.GetReqID(r.Context())
	}
	return &reqCtx{
		method:      originalMethod(r),
		path:        admission.RoutePath(r.Header.Get("X-Forwarded-Uri")),
		clientKey:   admission.ClientKey(clientHost(r)),
		correlation: corr,
	}
}

// admit runs the controls that apply before the token is looked at, cheapest
// first, and returns the release function for the concurrency slot. ok=false
// means a response has already been written.
func (h *Handler) admit(w http.ResponseWriter, r *http.Request, rc *reqCtx) (release func(), ok bool) {
	noop := func() {}
	if !h.adm.Gate.Acquire() {
		h.refuse(w, r, rc, refusal{
			outcome: outcomeOverloaded, code: "backpressure", status: http.StatusServiceUnavailable,
			detail: "the gateway is shedding load; retry shortly", retryable: true, retryAfter: 1,
			event: "integration_backpressure_active", severity: siem.SeverityHigh,
		})
		return noop, false
	}
	release = h.adm.Gate.Release

	// Rate limit first: a flood of malformed or oversized requests must spend
	// quota like any other, or the cheap checks below become the attack surface.
	if allowed, wait := h.adm.IPLimiter.Allow(rc.clientKey); !allowed {
		h.refuse(w, r, rc, refusal{
			outcome: outcomeRateLimited, code: "rate_limited", status: http.StatusTooManyRequests,
			detail: "request rate for this client exceeds its quota", retryable: true, retryAfter: ceilSeconds(wait),
			event: "api_rate_limited", severity: siem.SeverityMedium,
		})
		release()
		return noop, false
	}

	v, version := h.adm.Policy.Check(r.Header, r.Header.Get("X-Forwarded-Uri"))
	rc.apiVersion = version
	if v != nil {
		f := refusal{outcome: string(v.Kind), status: v.Status, detail: v.Detail, severity: siem.SeverityLow}
		switch v.Kind {
		case admission.KindVersionUnsupported:
			f.code = "version_unsupported"
			f.event = "api_route_version_unavailable"
		default:
			f.code = "schema_mismatch"
			f.event = "api_request_denied"
		}
		h.refuse(w, r, rc, f)
		release()
		return noop, false
	}
	return release, true
}

// admitTenant applies the per-tenant quota once the token has identified one.
func (h *Handler) admitTenant(w http.ResponseWriter, r *http.Request, rc *reqCtx) bool {
	allowed, wait := h.adm.TenantLimiter.Allow(rc.tenantID)
	if allowed {
		return true
	}
	h.refuse(w, r, rc, refusal{
		outcome: outcomeRateLimited, code: "rate_limited", status: http.StatusTooManyRequests,
		detail: "request rate for this tenant exceeds its quota", retryable: true, retryAfter: ceilSeconds(wait),
		event: "api_rate_limited", severity: siem.SeverityMedium,
	})
	return false
}

func ceilSeconds(d time.Duration) int {
	s := int((d + time.Second - 1) / time.Second)
	if s < 1 {
		s = 1
	}
	return s
}

// refusal describes one terminal refusal of /verify.
type refusal struct {
	outcome    string
	code       string
	status     int
	detail     string
	retryable  bool
	retryAfter int
	headers    [][2]string // extra response headers, e.g. X-Carta-Decision
	event      string      // canonical event name; "" means api_request_denied
	// dedicated is true when this refusal already streams its own, richer SIEM
	// event (auth.tenant_hostname_mismatch, session_risk.*, tenant_context.denied),
	// so the generic one is not raised on top of it. It is still logged,
	// counted and recorded.
	dedicated bool
	severity  siem.Severity
}

// eventThrottle bounds how often one kind of refusal from one source raises an
// event. Every refusal is still counted in metrics and kept in the recorder.
const eventThrottle = 30 * time.Second

// refuse is the single exit for every refusal. It answers with RFC 9457
// problem details (INT-NP-003), keeps the X-Auth-Denial-Reason header that
// consoles already read, records the decision, counts the outcome exactly once
// and raises the canonical event.
func (h *Handler) refuse(w http.ResponseWriter, r *http.Request, rc *reqCtx, f refusal) {
	d, abusive := h.rec.Record(admission.Decision{
		TenantID: rc.tenantID, PrincipalID: rc.principalID, ClientKey: rc.clientKey,
		Method: rc.method, Path: rc.path, APIVersion: rc.apiVersion,
		Outcome: f.outcome, Code: f.code, Status: f.status, Detail: f.detail,
		CorrelationID: rc.correlation,
	})
	h.metrics.VerifyDecision(f.outcome)

	w.Header().Set(denialReasonHeader, f.outcome)
	for _, kv := range f.headers {
		w.Header().Set(kv[0], kv[1])
	}
	apicontract.WriteProblem(w, r, apicontract.Problem{
		Status:        f.status,
		Code:          f.code,
		Detail:        f.detail,
		Instance:      rc.path,
		Retryable:     f.retryable,
		RetryAfter:    f.retryAfter,
		CorrelationID: rc.correlation,
		DecisionID:    d.ID,
		CurrentVersion: nil,
	})

	name := f.event
	if name == "" {
		name = "api_request_denied"
	}
	source := rc.tenantID
	if source == "" {
		source = rc.clientKey
	}
	if !f.dedicated && h.rec.MayEmit(name+"|"+source+"|"+f.outcome, eventThrottle) {
		sev := f.severity
		if sev == "" {
			sev = siem.SeverityLow
		}
		h.emit(r.Context(), rc.tenantID, name, sev, f.outcome+": "+f.detail)
	}
	if abusive {
		h.emit(r.Context(), rc.tenantID, "api_abuse_threshold_exceeded", siem.SeverityHigh,
			"client "+rc.clientKey+" collected repeated refusals within a minute")
	}
}

// emit logs a canonical gateway event and, when the tenant is known, streams it
// to SIEM. SIEM export is tenant-scoped, so a refusal before authentication has
// no tenant and is evidenced by the log, the metrics and the decision recorder
// instead.
func (h *Handler) emit(ctx context.Context, tenantID, event string, sev siem.Severity, msg string) {
	h.log.Warn("gateway event", zap.String("event", event), zap.String("severity", string(sev)),
		zap.String("tenant_id", tenantID), zap.String("detail", msg))
	h.siem.Stream(ctx, tenantID, event, sev, msg)
}

// refuseAuth answers 401: the caller could not be authenticated.
func (h *Handler) refuseAuth(w http.ResponseWriter, r *http.Request, rc *reqCtx, outcome, detail string) {
	h.refuse(w, r, rc, refusal{outcome: outcome, code: "auth_denied", status: http.StatusUnauthorized, detail: detail})
}

// clientHost is the client address WITHOUT a port, for the per-client quota and
// for evidence. clientIP falls back to the peer's RemoteAddr, which carries the
// connection's ephemeral port: keying a quota on that gave every new connection
// its own bucket, so a client could never be limited. Found by running the real
// image, not by a unit test (which always set X-Forwarded-For).
func clientHost(r *http.Request) string {
	addr := clientIP(r)
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return strings.Trim(addr, "[]")
}
