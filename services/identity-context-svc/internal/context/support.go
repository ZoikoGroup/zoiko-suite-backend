package context

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/events"
	"zoiko.io/identity-context-svc/internal/outbox"
	"zoiko.io/identity-context-svc/internal/siem"
	"zoiko.io/identity-context-svc/internal/sod"
	"zoiko.io/identity-context-svc/internal/telemetry"
)

// SupportContextStore is the persistence contract for privileged elevations.
type SupportContextStore interface {
	InsertSupportContextWithEvent(ctx context.Context, sc domain.SupportContext, rec outbox.Record) error
	FindSupportContext(ctx context.Context, supportContextID, tenantID string) (*domain.SupportContext, error)
	FindLiveSupportContext(ctx context.Context, supportPrincipalID, tenantID string, at time.Time) (*domain.SupportContext, error)
	RevokeSupportContextWithEvent(ctx context.Context, supportContextID, tenantID, reason string, at time.Time, rec outbox.Record) (bool, error)
	FindUnreviewedExpiredSupportContexts(ctx context.Context, tenantID string, before time.Time, limit int) ([]domain.SupportContext, error)

	// FindUnreviewedExpiredSupportContextsAllTenants backs the background
	// reconciler. A sweep has no tenant to be scoped to, and a per-tenant
	// sweep would only ever cover tenants somebody thought to ask about.
	FindUnreviewedExpiredSupportContextsAllTenants(ctx context.Context, before time.Time, limit int) ([]domain.SupportContext, error)
	MarkSupportContextReviewedWithEvent(ctx context.Context, supportContextID, tenantID, reviewer string, at time.Time, rec outbox.Record) (bool, error)
	ApproveSupportContextWithEvent(ctx context.Context, supportContextID, tenantID, approverPrincipalID string, grantedAt, expiresAt time.Time, rec outbox.Record) (bool, error)
}

// SupportPolicy bounds what a support elevation may be.
type SupportPolicy struct {
	// MaxTTL is the longest window that may be granted. The spec's invariant
	// is "time-limited"; without a ceiling, "time-limited" means "expires
	// eventually", which a caller can set to a decade.
	MaxTTL time.Duration
	// DefaultTTL applies when a request names no window.
	DefaultTTL time.Duration
	// MinJustificationLength stops "asdf" satisfying the evidence obligation.
	// A justification nobody can act on is not evidence, it is a checkbox.
	MinJustificationLength int
	// ApprovalWindow is how long a request may wait for its approver. A
	// request approved a day later is approving a situation that has moved.
	ApprovalWindow time.Duration
}

// DefaultSupportPolicy is deliberately tight. Widening it is a decision
// somebody should have to make explicitly and in configuration.
func DefaultSupportPolicy() SupportPolicy {
	return SupportPolicy{
		MaxTTL:                 4 * time.Hour,
		DefaultTTL:             1 * time.Hour,
		MinJustificationLength: 20,
		ApprovalWindow:         1 * time.Hour,
	}
}

// SupportService implements GOV-01's AttachSupportContext command.
//
// WHAT THIS IS NOT. It is not a role, it does not grant permissions, and it
// does not bypass authorization. Invariant 8 is explicit: "Break-glass cannot
// bypass tenant isolation, residency, accounting integrity, tax validity or
// other domain invariants."
//
// What it does is narrower and sufficient: it makes a support principal's
// session RESOLVABLE in a tenant they do not belong to, for a bounded window,
// with an independent approver on record, and with every session issued under
// it tagged by its id. Authorization still runs on every request afterwards,
// unchanged. A support engineer with an attached context and no permissions
// can do exactly nothing, which is the correct starting position.
type SupportService struct {
	store   SupportContextStore
	events  *events.Publisher
	sod     sod.Checker
	siem    *siem.Client
	policy  SupportPolicy
	log     *zap.Logger
	metrics *telemetry.GovMetrics
}

// WithMetrics attaches the GOV-01 instruments. Optional — a nil one means a
// test or a deployment without Prometheus, and every use below is guarded.
func (s *SupportService) WithMetrics(m *telemetry.GovMetrics) *SupportService {
	s.metrics = m
	return s
}

// countSoD records a segregation-of-duties outcome.
//
// UNAVAILABLE is the value worth watching. The command was correctly refused,
// but a sustained rate means GOV-04 is down and no privileged command can be
// issued at all — which looks like nothing at all in the HTTP metrics, because
// nobody is trying and failing, they have simply stopped.
func (s *SupportService) countSoD(outcome string) {
	if s.metrics != nil {
		s.metrics.SoDChecks.WithLabelValues(outcome).Inc()
	}
}

func NewSupportService(
	store SupportContextStore,
	publisher *events.Publisher,
	sodChecker sod.Checker,
	siemClient *siem.Client,
	policy SupportPolicy,
	log *zap.Logger,
) *SupportService {
	if policy.MaxTTL <= 0 {
		policy = DefaultSupportPolicy()
	}
	return &SupportService{
		store:  store,
		events: publisher,
		sod:    sodChecker,
		siem:   siemClient,
		policy: policy,
		log:    log,
	}
}

// ActionAttachSupportContext is the authorization action guarding the command.
const ActionAttachSupportContext = "IDENTITY_SUPPORT_CONTEXT_ATTACH"

// ActionRevokeSupportContext guards early revocation.
//
// A SEPARATE action from attach, and deliberately a weaker one to hold:
// revoking an elevation is a de-escalation. Requiring the same grant to end a
// support session as to start one means the person who notices a problem may
// not be able to stop it.
const ActionRevokeSupportContext = "IDENTITY_SUPPORT_CONTEXT_REVOKE"

// ActionReviewSupportContext guards recording the post-hoc reconciliation.
//
// Its own action rather than reusing the revoke grant: reviewing an elapsed
// elevation is an assurance duty, and the person who may end a live support
// session is not necessarily the person who should sign off that it was
// legitimate. §1 wants the review independent, and one shared permission makes
// "independent" unexpressible.
const ActionReviewSupportContext = "IDENTITY_SUPPORT_CONTEXT_REVIEW"

// Attach grants a scoped, time-limited, independently-approved elevation.
//
// The order of checks is not arbitrary. Cheap local validation runs first so a
// malformed request never reaches GOV-04, and the SoD call runs before the
// write so a conflict costs nothing. The write and its event are atomic.
func (s *SupportService) Attach(
	ctx context.Context,
	req domain.AttachSupportContextRequest,
	callerPrincipalID string,
) (*domain.SupportContext, error) {
	// ── Local validation ────────────────────────────────────────────────────
	if req.TenantID == "" || req.SupportPrincipalID == "" {
		return nil, fmt.Errorf("%w: tenant_id and support_principal_id are required", ErrRequestInvalid)
	}
	if req.ApproverPrincipalID == "" {
		return nil, fmt.Errorf("%w: approver_principal_id is required — there is no single-party form of this command", ErrRequestInvalid)
	}
	if req.ApproverPrincipalID == req.SupportPrincipalID || req.ApproverPrincipalID == callerPrincipalID {
		// The caller naming themselves passed this check before (S1-1): the
		// approver is the person who approves, so it cannot be the requester.
		return nil, domain.ErrSupportSelfApproval
	}
	if !domain.ValidSupportReason(req.ReasonCode) {
		return nil, fmt.Errorf("%w: reason_code %q is not one of the recognised reasons", ErrRequestInvalid, req.ReasonCode)
	}
	if len(strings.TrimSpace(req.Justification)) < s.policy.MinJustificationLength {
		return nil, fmt.Errorf("%w: justification must be at least %d characters — it is the evidence a reviewer reads",
			ErrRequestInvalid, s.policy.MinJustificationLength)
	}
	if strings.TrimSpace(req.TicketRef) == "" {
		return nil, fmt.Errorf("%w: ticket_ref is required", ErrRequestInvalid)
	}

	ttl := time.Duration(req.TTLSeconds) * time.Second
	if req.TTLSeconds <= 0 {
		ttl = s.policy.DefaultTTL
	}
	if ttl > s.policy.MaxTTL {
		// Refused, not clamped. Clamping would issue a grant with a window
		// nobody requested and nobody reviewed, and the requester would
		// believe they held it for longer than they do.
		return nil, fmt.Errorf("%w: requested %s, maximum is %s",
			domain.ErrSupportTTLExceeded, ttl, s.policy.MaxTTL)
	}

	// ── Segregation of duties (GOV-04) ──────────────────────────────────────
	//
	// Asked even though the caller already passed authorization, because they
	// are different questions with different owners. GOV-03 answers "may this
	// principal attach support contexts"; GOV-04 answers "may THIS principal
	// approve an elevation for THAT one". A person can legitimately hold the
	// permission and still be the wrong approver for a specific grant.
	if s.sod != nil {
		decision, err := s.sod.CheckConflict(ctx, sod.Request{
			TenantID:           req.TenantID,
			ActionType:         ActionAttachSupportContext,
			MakerPrincipalID:   callerPrincipalID,
			CheckerPrincipalID: req.ApproverPrincipalID,
			SubjectPrincipalID: req.SupportPrincipalID,
			CorrelationID:      req.CorrelationID,
		})
		if err != nil {
			if errors.Is(err, sod.ErrConflict) {
				s.countSoD("conflict")
				s.log.Warn("support context refused — segregation of duties conflict",
					zap.String("tenant_id", req.TenantID),
					zap.String("maker", callerPrincipalID),
					zap.String("checker", req.ApproverPrincipalID))
				return nil, err
			}
			// Unavailable. Fail closed: a conflict check that did not run is
			// not a check that passed.
			s.countSoD("unavailable")
			return nil, err
		}
		s.countSoD("no_conflict")
		if decision != nil && decision.ExceptionRef != "" {
			s.log.Info("support context proceeding under an approved compensating control",
				zap.String("exception_ref", decision.ExceptionRef))
		}
	}

	now := time.Now().UTC()
	sc := domain.SupportContext{
		SupportContextID:       "sup-" + ulid.Make().String(),
		TenantID:               req.TenantID,
		SupportPrincipalID:     req.SupportPrincipalID,
		SubjectPrincipalID:     req.SubjectPrincipalID,
		ReasonCode:             req.ReasonCode,
		Justification:          strings.TrimSpace(req.Justification),
		TicketRef:              req.TicketRef,
		ApproverPrincipalID:    req.ApproverPrincipalID,
		RequestedByPrincipalID: callerPrincipalID,
		ApprovalStatus:         domain.SupportPendingApproval,
		RequestedTTLSeconds:    int(ttl / time.Second),
		// Placeholders until approval restarts them; a pending request is
		// never Live and never returned by FindLiveSupportContext.
		GrantedAt:     now,
		ExpiresAt:     now.Add(ttl),
		EvidenceID:    "ev-" + ulid.Make().String(),
		CorrelationID: req.CorrelationID,
	}

	// No attached event yet: nothing is granted. The request is the row.
	if err := s.store.InsertSupportContextWithEvent(ctx, sc, outbox.Record{}); err != nil {
		return nil, fmt.Errorf("persist support request: %w", err)
	}
	s.siem.Stream(ctx, sc.TenantID, "identity.support_context.requested",
		siem.SeverityHigh,
		fmt.Sprintf("Support context %s requested by %s for %s in tenant %s, awaiting approval by %s (ticket %s): %s",
			sc.SupportContextID, callerPrincipalID, sc.SupportPrincipalID, sc.TenantID,
			sc.ApproverPrincipalID, sc.TicketRef, sc.Justification))
	s.log.Warn("SUPPORT CONTEXT REQUESTED — awaiting approval",
		zap.String("support_context_id", sc.SupportContextID),
		zap.String("tenant_id", sc.TenantID),
		zap.String("support_principal_id", sc.SupportPrincipalID),
		zap.String("requested_by", callerPrincipalID),
		zap.String("approver_principal_id", sc.ApproverPrincipalID))
	return &sc, nil
}

// Approve is the named approver's own act (GOV-01 §1 "independently
// approved"; S1-1 / R-2). Only the approver the request names may approve,
// never its requester or its grantee; GOV-04 is asked again with the real
// checker; and the window starts now, so time spent waiting for approval is
// not time the grant was usable.
func (s *SupportService) Approve(
	ctx context.Context,
	supportContextID, tenantID, callerPrincipalID, correlationID string,
) (*domain.SupportContext, error) {
	found, err := s.store.FindSupportContext(ctx, supportContextID, tenantID)
	if err != nil {
		return nil, fmt.Errorf("read support context: %w", err)
	}
	if found == nil || found.RevokedAt != nil {
		return nil, domain.ErrSupportContextNotFound
	}
	// A copy: the approval is applied by the store's conditional write, never
	// by mutating what the read returned.
	cp := *found
	sc := &cp
	if sc.ApprovalStatus != domain.SupportPendingApproval {
		return nil, domain.ErrSupportNotPending
	}
	if callerPrincipalID == sc.RequestedByPrincipalID || callerPrincipalID == sc.SupportPrincipalID {
		return nil, fmt.Errorf("%w: %w", sod.ErrConflict, domain.ErrSupportSelfApproval)
	}
	if callerPrincipalID != sc.ApproverPrincipalID {
		return nil, domain.ErrSupportNotApprover
	}
	now := time.Now().UTC()
	window := s.policy.ApprovalWindow
	if window <= 0 {
		window = DefaultSupportPolicy().ApprovalWindow
	}
	if now.Sub(sc.GrantedAt) > window {
		return nil, domain.ErrSupportApprovalLapsed
	}

	if s.sod != nil {
		if _, err := s.sod.CheckConflict(ctx, sod.Request{
			TenantID:           tenantID,
			ActionType:         ActionAttachSupportContext,
			MakerPrincipalID:   sc.RequestedByPrincipalID,
			CheckerPrincipalID: callerPrincipalID,
			SubjectPrincipalID: sc.SupportPrincipalID,
			CorrelationID:      firstNonEmptyString(correlationID, sc.CorrelationID),
		}); err != nil {
			if errors.Is(err, sod.ErrConflict) {
				s.countSoD("conflict")
			} else {
				s.countSoD("unavailable")
			}
			return nil, err
		}
		s.countSoD("no_conflict")
	}

	ttl := time.Duration(sc.RequestedTTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = s.policy.DefaultTTL
	}
	approvedAt := now
	sc.ApprovalStatus, sc.ApprovedAt, sc.GrantedAt, sc.ExpiresAt = domain.SupportApproved, &approvedAt, now, now.Add(ttl)

	rec, err := events.Render(
		events.EventSupportContextAttached,
		sc.TenantID, "", callerPrincipalID, firstNonEmptyString(correlationID, sc.CorrelationID), sc.SupportContextID,
		supportAttachedPayload(*sc))
	if err != nil {
		return nil, fmt.Errorf("render support attached event: %w", err)
	}
	changed, err := s.store.ApproveSupportContextWithEvent(ctx, sc.SupportContextID, tenantID, callerPrincipalID, sc.GrantedAt, sc.ExpiresAt, rec)
	if err != nil {
		return nil, fmt.Errorf("persist support approval: %w", err)
	}
	if !changed {
		// Raced: approved or revoked between the read and the write.
		return nil, domain.ErrSupportNotPending
	}

	// A privileged elevation into a customer tenant is the single highest
	// signal this service produces. Streamed at CRITICAL, synchronously, and
	// its failure is logged rather than swallowed — a break-glass the security
	// team never saw is the scenario the control exists to prevent.
	s.siem.Stream(ctx, sc.TenantID, "identity.support_context.attached",
		siem.SeverityCritical,
		fmt.Sprintf("Support context %s granted to %s in tenant %s until %s (requested by %s, approved by %s, ticket %s): %s",
			sc.SupportContextID, sc.SupportPrincipalID, sc.TenantID,
			sc.ExpiresAt.Format(time.RFC3339), sc.RequestedByPrincipalID, callerPrincipalID, sc.TicketRef, sc.Justification))
	if s.metrics != nil {
		s.metrics.SupportContextsGranted.WithLabelValues(sc.ReasonCode).Inc()
	}
	s.log.Warn("SUPPORT CONTEXT APPROVED — grant live",
		zap.String("support_context_id", sc.SupportContextID),
		zap.String("tenant_id", sc.TenantID),
		zap.String("support_principal_id", sc.SupportPrincipalID),
		zap.String("approver_principal_id", callerPrincipalID),
		zap.Time("expires_at", sc.ExpiresAt))
	return sc, nil
}

func firstNonEmptyString(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// Revoke ends a grant early. Idempotent: revoking an already-revoked context
// reports success without a second event.
func (s *SupportService) Revoke(
	ctx context.Context,
	supportContextID, tenantID, reason, actorPrincipalID, correlationID string,
) error {
	if reason == "" {
		reason = "MANUAL_REVOCATION"
	}
	existing, err := s.store.FindSupportContext(ctx, supportContextID, tenantID)
	if err != nil {
		return fmt.Errorf("read support context: %w", err)
	}
	if existing == nil {
		return domain.ErrSupportContextNotFound
	}

	rec, err := events.Render(
		events.EventSupportContextRevoked,
		tenantID, "", actorPrincipalID, correlationID, supportContextID,
		map[string]any{
			"support_context_id":   supportContextID,
			"tenant_id":            tenantID,
			"support_principal_id": existing.SupportPrincipalID,
			"reason":               reason,
			"actor":                actorPrincipalID,
			"correlation_id":       correlationID,
		})
	if err != nil {
		return fmt.Errorf("render support revoked event: %w", err)
	}

	changed, err := s.store.RevokeSupportContextWithEvent(ctx, supportContextID, tenantID, reason, time.Now().UTC(), rec)
	if err != nil {
		return fmt.Errorf("revoke support context: %w", err)
	}
	if !changed {
		// Already revoked. A no-op, reported as success — the caller's
		// intended end state holds.
		s.log.Debug("support context already revoked — no-op",
			zap.String("support_context_id", supportContextID))
		return nil
	}

	s.siem.Stream(ctx, tenantID, "identity.support_context.revoked",
		siem.SeverityHigh,
		fmt.Sprintf("Support context %s revoked by %s: %s", supportContextID, actorPrincipalID, reason))

	s.log.Warn("SUPPORT CONTEXT REVOKED",
		zap.String("support_context_id", supportContextID),
		zap.String("tenant_id", tenantID),
		zap.String("actor", actorPrincipalID),
		zap.String("reason", reason))
	return nil
}

// Verify reports whether a support principal holds a usable grant covering
// subjectPrincipalID in a tenant.
//
// Returns the distinct expired/not-found errors so an operator can tell the
// two apart, even though the caller's access outcome is identical.
func (s *SupportService) Verify(
	ctx context.Context,
	supportContextID, tenantID, supportPrincipalID, subjectPrincipalID string,
) (*domain.SupportContext, error) {
	sc, err := s.store.FindSupportContext(ctx, supportContextID, tenantID)
	if err != nil {
		return nil, fmt.Errorf("read support context: %w", err)
	}
	if sc == nil {
		return nil, domain.ErrSupportContextNotFound
	}
	if sc.SupportPrincipalID != supportPrincipalID {
		// Somebody else's grant. Reported as not-found for the same
		// non-enumeration reason a foreign session is.
		return nil, domain.ErrSupportContextNotFound
	}
	now := time.Now().UTC()
	if sc.RevokedAt != nil {
		return nil, domain.ErrSupportContextNotFound
	}
	if !sc.Live(now) {
		return nil, domain.ErrSupportContextExpired
	}
	if subjectPrincipalID != "" && !sc.Covers(subjectPrincipalID) {
		// The grant exists and is live, but was narrowed to a different
		// principal. Widening it silently would defeat the narrowing.
		return nil, domain.ErrSupportContextNotFound
	}
	return sc, nil
}

// Reconcile reports support contexts that have ended without review.
//
// This is the "followed by reconciliation/review" half of the break-glass
// invariant, and it is the half that is usually missing: the grant expires,
// nobody looks, and the control is decorative. Run on a schedule by the
// reconciler goroutine in cmd/server.
//
// It REPORTS rather than auto-approving. An automatic review is not a review.
func (s *SupportService) Reconcile(ctx context.Context, tenantID string, limit int) ([]domain.SupportContext, error) {
	pending, err := s.store.FindUnreviewedExpiredSupportContexts(ctx, tenantID, time.Now().UTC(), limit)
	if err != nil {
		return nil, err
	}
	for _, sc := range pending {
		s.log.Warn("SUPPORT CONTEXT AWAITING REVIEW",
			zap.String("support_context_id", sc.SupportContextID),
			zap.String("tenant_id", sc.TenantID),
			zap.String("support_principal_id", sc.SupportPrincipalID),
			zap.String("approver_principal_id", sc.ApproverPrincipalID),
			zap.String("ticket_ref", sc.TicketRef),
			zap.Time("expired_at", sc.ExpiresAt))
	}
	return pending, nil
}

// ReconcileAll is Reconcile across every tenant, for the background worker.
//
// Same reporting stance as Reconcile: it names what is awaiting review and
// approves nothing. An automatic review is not a review.
func (s *SupportService) ReconcileAll(ctx context.Context, limit int) ([]domain.SupportContext, error) {
	pending, err := s.store.FindUnreviewedExpiredSupportContextsAllTenants(ctx, time.Now().UTC(), limit)
	if err != nil {
		return nil, err
	}
	for _, sc := range pending {
		s.log.Warn("SUPPORT CONTEXT AWAITING REVIEW",
			zap.String("support_context_id", sc.SupportContextID),
			zap.String("tenant_id", sc.TenantID),
			zap.String("support_principal_id", sc.SupportPrincipalID),
			zap.String("approver_principal_id", sc.ApproverPrincipalID),
			zap.String("ticket_ref", sc.TicketRef),
			zap.Time("expired_at", sc.ExpiresAt))
	}
	if s.metrics != nil {
		// This gauge has existed since GOV-01 and has never been set by
		// anything: the only writer would have been a reconciliation sweep,
		// and no sweep ran. An alert on it could not have fired.
		s.metrics.SupportContextsUnreviewed.Set(float64(len(pending)))
	}
	return pending, nil
}

// RunReconciler is the goroutine SupportService.Reconcile's comment has always
// claimed existed. It did not, so §1's "followed by reconciliation/review" was
// enforced by nothing: a grant expired, no one was told, and the control was
// decorative — precisely the outcome §1 names as unacceptable.
//
// Reports on a fixed interval and on start, so a process that has just come up
// after an outage does not wait a full interval before saying what is pending.
// Returns when ctx is cancelled.
// A non-positive interval disables the sweep, which is the meaning
// SUPPORT_REVIEW_INTERVAL_MINUTES already documented. It is logged at WARN
// rather than silently obeyed: "nobody is checking break-glass" is a decision
// somebody should be able to find in the logs afterwards.
func (s *SupportService) RunReconciler(ctx context.Context, interval time.Duration, limit int) {
	if interval <= 0 {
		s.log.Warn("break-glass reconciliation is DISABLED",
			zap.String("reason", "SUPPORT_REVIEW_INTERVAL_MINUTES is zero"),
			zap.String("consequence", "expired support contexts will not be reported for review"))
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	sweep := func() {
		if _, err := s.ReconcileAll(ctx, limit); err != nil {
			// Logged, not fatal. A reconciliation report that cannot run is an
			// operational problem; failing the process would take the service
			// down over a reporting query.
			s.log.Error("support-context reconciliation sweep failed", zap.Error(err))
		}
	}

	sweep()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}

// MarkReviewed records that a human reconciled a grant, with its event.
//
// §1 requires break-glass to be "fully evidenced and followed by
// reconciliation/review". The attach and revoke halves always emitted events;
// the review — the step that CLOSES the loop — wrote two columns and nothing
// else, so the governance stream showed every elevation as permanently
// unreviewed and a consumer could not tell a reconciled grant from an ignored
// one. The event is written atomically with the review via the outbox.
//
// A repeat is a no-op, like a repeat revoke: the first reviewer stands and no
// second event is written.
func (s *SupportService) MarkReviewed(ctx context.Context, supportContextID, tenantID, reviewer, correlationID string) error {
	if reviewer == "" {
		return fmt.Errorf("%w: reviewer is required", ErrRequestInvalid)
	}
	existing, err := s.store.FindSupportContext(ctx, supportContextID, tenantID)
	if err != nil {
		return fmt.Errorf("read support context: %w", err)
	}
	if existing == nil {
		return domain.ErrSupportContextNotFound
	}

	now := time.Now().UTC()
	rec, err := events.Render(
		events.EventSupportContextReviewed,
		tenantID, "", reviewer, correlationID, supportContextID,
		map[string]any{
			"support_context_id":    supportContextID,
			"tenant_id":             tenantID,
			"support_principal_id":  existing.SupportPrincipalID,
			"approver_principal_id": existing.ApproverPrincipalID,
			"reviewed_by":           reviewer,
			"reviewed_at":           now,
			"granted_at":            existing.GrantedAt,
			"expires_at":            existing.ExpiresAt,
			"revoked":               existing.RevokedAt != nil,
			"ticket_ref":            existing.TicketRef,
			"correlation_id":        correlationID,
		})
	if err != nil {
		return fmt.Errorf("render support reviewed event: %w", err)
	}

	changed, err := s.store.MarkSupportContextReviewedWithEvent(ctx, supportContextID, tenantID, reviewer, now, rec)
	if err != nil {
		return fmt.Errorf("record support context review: %w", err)
	}
	if !changed {
		s.log.Debug("support context already reviewed — no-op",
			zap.String("support_context_id", supportContextID))
	}
	return nil
}

func supportAttachedPayload(sc domain.SupportContext) map[string]any {
	payload := map[string]any{
		"support_context_id":    sc.SupportContextID,
		"tenant_id":             sc.TenantID,
		"support_principal_id":  sc.SupportPrincipalID,
		"approver_principal_id": sc.ApproverPrincipalID,
		"reason_code":           sc.ReasonCode,
		"justification":         sc.Justification,
		"ticket_ref":            sc.TicketRef,
		"granted_at":            sc.GrantedAt,
		"expires_at":            sc.ExpiresAt,
		"evidence_id":           sc.EvidenceID,
		"correlation_id":        sc.CorrelationID,
	}
	if sc.SubjectPrincipalID != nil {
		payload["subject_principal_id"] = *sc.SubjectPrincipalID
	}
	return payload
}
