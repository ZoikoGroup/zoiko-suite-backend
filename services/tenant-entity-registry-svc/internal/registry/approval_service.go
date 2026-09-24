package registry

// Verified maker-checker for ORG-02 §4.2 and ORG-03 §4.3.
//
// Before this file, every SoD control in the service took its approver from
// approved_by_principal_id in the maker's own request body and checked only
// that it differed from the actor. One person typing a second name satisfied
// it. Now a maker-checker command is PROPOSED — filed as an approval request
// and answered 202 — and runs only when a different verified principal, the
// caller of /approve, releases it. The approver is always the gateway-verified
// identity of that call.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

// DefaultApprovalTTL is how long a proposal waits for a decision.
const DefaultApprovalTTL = 7 * 24 * time.Hour

var (
	// ErrApprovalPending — the subject already has a live proposal. Wraps
	// ErrConflict so it is a 409 wherever conflicts are.
	ErrApprovalPending = fmt.Errorf("%w: an approval request is already pending for this subject", ErrConflict)

	// ErrApprovalNotPending — the request was already decided, went stale or
	// expired. Deliberately NOT wrapping ErrConflict: callers of the approval
	// path treat ErrConflict as "the subject moved", and this is not that.
	ErrApprovalNotPending = errors.New("approval request is no longer pending")

	// ErrApprovalFingerprintMismatch — the approver did not present the
	// fingerprint of the stored proposal, or the stored proposal no longer
	// hashes to its own fingerprint.
	ErrApprovalFingerprintMismatch = errors.New("payload_fingerprint does not match the proposal")

	// ErrSelfApproval — the maker tried to decide their own proposal, or the
	// principal whose registry claim was quarantined tried to approve its
	// resolution.
	ErrSelfApproval = errors.New("segregation of duties: a maker cannot approve their own request")

	// errBodyApprover is the refusal for the field the old flow relied on.
	errBodyApprover = fmt.Errorf("%w: approved_by_principal_id is no longer accepted — "+
		"the command is filed for approval and the approver is whoever calls "+
		"POST /v1/approval-requests/{id}/approve", ErrApprovalRequired)
)

// PendingApprovalError is returned by a maker-checker command that was filed
// rather than executed. It is an error only so that every command keeps its
// signature; the handler answers it 202 with the request in the body.
type PendingApprovalError struct {
	Request *domain.ApprovalRequest
}

func (e *PendingApprovalError) Error() string {
	return "accepted: awaiting independent approval " + e.Request.ApprovalRequestID
}

// ConfigureMakerChecker sets the approval TTL and the legacy switch.
//
// legacyBodyApprover restores the pre-000007 behaviour — a body-supplied
// approver, checked only for inequality. It exists solely so the frontend can
// be migrated without a hard cutover, and config refuses to start with it in
// staging or production.
func (s *Service) ConfigureMakerChecker(legacyBodyApprover bool, ttl time.Duration) {
	s.legacyBodyApprover = legacyBodyApprover
	if ttl > 0 {
		s.approvalTTL = ttl
	}
	if legacyBodyApprover {
		s.log.Warn("MAKER_CHECKER_LEGACY_BODY_APPROVER is on: approvals are self-asserted " +
			"and NOT independently verified. Development use only.")
	}
}

// legacyApproverCheck is the pre-000007 check, used only in legacy mode.
func (s *Service) legacyApproverCheck(approver, actor, what string) error {
	if strings.TrimSpace(approver) == "" {
		return fmt.Errorf("%w: %s requires approved_by_principal_id", ErrApprovalRequired, what)
	}
	if approver == actor {
		return fmt.Errorf("%w: %s cannot be self-approved", ErrApprovalRequired, what)
	}
	s.log.Warn("legacy self-asserted approver accepted", zap.String("command", what))
	return nil
}

// proposal is one command to file for approval.
type proposal struct {
	subjectType     domain.ApprovalSubjectType
	tenantID        string
	subjectID       string
	command         string
	expectedVersion int64
	reason          string
	payload         any
	correlationID   string
	// requestedBy overrides the verified caller. Used only for tenant
	// creation, whose maker is the tenant's creator even when the request is
	// (re)filed during an activation attempt by someone else.
	requestedBy string
}

func (s *Service) propose(ctx context.Context, p proposal) (*domain.ApprovalRequest, error) {
	a, err := s.buildProposal(ctx, p)
	if err != nil {
		return nil, err
	}
	if err := s.store.CreateApprovalRequest(ctx, a); err != nil {
		return nil, err
	}
	s.log.Info("governed command filed for independent approval",
		zap.String("approval_request_id", a.ApprovalRequestID),
		zap.String("subject_type", string(a.SubjectType)),
		zap.String("subject_id", a.SubjectID),
		zap.String("command", a.CommandName),
		zap.String("correlation_id", p.correlationID),
	)
	return a, nil
}

// buildProposal constructs an approval request without filing it, for the
// callers that file it inside a larger transaction.
func (s *Service) buildProposal(ctx context.Context, p proposal) (*domain.ApprovalRequest, error) {
	requester := p.requestedBy
	if requester == "" {
		requester = domain.PrincipalFromContext(ctx)
	}
	if requester == "" {
		return nil, ErrUnauthenticated
	}
	fp, err := domain.ApprovalFingerprint(p.subjectType, p.subjectID, p.command, p.expectedVersion, p.payload)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(p.payload)
	if err != nil {
		return nil, fmt.Errorf("marshal approval payload: %w", err)
	}
	now := time.Now().UTC()
	a := &domain.ApprovalRequest{
		ApprovalRequestID:      newID(),
		TenantID:               p.tenantID,
		SubjectType:            p.subjectType,
		SubjectID:              p.subjectID,
		CommandName:            p.command,
		Payload:                raw,
		ExpectedVersion:        p.expectedVersion,
		PayloadFingerprint:     fp,
		Reason:                 p.reason,
		RequestedByPrincipalID: requester,
		RequestedAt:            now,
		ExpiresAt:              now.Add(s.approvalTTL),
		Status:                 domain.ApprovalPending,
		CorrelationID:          nullableString(p.correlationID),
	}
	return a, nil
}

// ---------------------------------------------------------------------------
// Tenant creation (ORG-02 §4.2: "Tenant creation … require[s] maker-checker")
// ---------------------------------------------------------------------------

// proposeTenantCreation files the creation approval for t. The maker is the
// tenant's creator.
func (s *Service) proposeTenantCreation(ctx context.Context, t *domain.Tenant, correlationID string) (*domain.ApprovalRequest, error) {
	a, err := s.creationApproval(ctx, t, correlationID)
	if err != nil {
		return nil, err
	}
	if err := s.store.CreateApprovalRequest(ctx, a); err != nil {
		return nil, err
	}
	return a, nil
}

// requireCreationApproval is the activation gate: a tenant leaves ONBOARDING
// only once a second principal has approved its creation.
//
// Self-healing by design. A tenant provisioned before migration 000007, one
// whose creation request expired or was rejected, or one whose request failed
// to file at provisioning time has no live request; the gate files one and
// refuses, rather than leaving the tenant permanently unactivatable.
func (s *Service) requireCreationApproval(ctx context.Context, t *domain.Tenant) error {
	if s.legacyBodyApprover {
		return nil
	}
	latest, err := s.store.LatestApprovalForSubject(ctx, domain.ApprovalSubjectTenantCreation, t.TenantID)
	if err != nil {
		return fmt.Errorf("store.LatestApprovalForSubject: %w", err)
	}
	if latest != nil {
		switch {
		case latest.Status == domain.ApprovalApproved:
			return nil
		case latest.Status == domain.ApprovalPending && time.Now().UTC().Before(latest.ExpiresAt):
			return fmt.Errorf("%w: tenant creation approval %s is pending — a second principal must approve it before the tenant can be activated",
				ErrApprovalRequired, latest.ApprovalRequestID)
		}
	}
	a, err := s.proposeTenantCreation(ctx, t, "")
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: tenant creation has not been independently approved; approval request %s has been filed",
		ErrApprovalRequired, a.ApprovalRequestID)
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// ListApprovalRequests lists the caller's tenant's approval requests.
func (s *Service) ListApprovalRequests(ctx context.Context, pendingOnly bool) ([]*domain.ApprovalRequest, error) {
	if domain.TenantFromContext(ctx) == "" {
		return nil, ErrNotFound
	}
	return s.store.ListApprovalRequests(ctx, pendingOnly)
}

// GetApprovalRequest returns one approval request in the caller's tenant.
func (s *Service) GetApprovalRequest(ctx context.Context, id string) (*domain.ApprovalRequest, error) {
	if domain.TenantFromContext(ctx) == "" {
		return nil, ErrNotFound
	}
	a, err := s.store.GetApprovalRequest(ctx, id)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, ErrNotFound
	}
	if err := s.assertTenantScope(ctx, a.TenantID); err != nil {
		return nil, err
	}
	return a, nil
}

// ---------------------------------------------------------------------------
// Decisions
// ---------------------------------------------------------------------------

// approvalAuthz is the permission a principal needs to decide a request.
// Each is distinct from the permission to PROPOSE the same command, so the
// two roles can be granted to different people — which is the point.
func (s *Service) approvalAuthz(ctx context.Context, a *domain.ApprovalRequest) error {
	switch a.SubjectType {
	case domain.ApprovalSubjectTenantCreation:
		// Evaluated in the platform scope, like provisioning itself.
		return s.authorizeIn(ctx, s.platformScopeID, "tenant", "provision.approve")
	case domain.ApprovalSubjectTenantCommand:
		return s.authorize(ctx, "tenant", domain.TenantCommand(a.CommandName).AuthzAction()+".approve")
	case domain.ApprovalSubjectLegalProfileAmendment:
		return s.authorize(ctx, "entity", "profile.amend.approve")
	case domain.ApprovalSubjectRegistryConflictResolution:
		return s.authorize(ctx, "entity.registry-conflict", "resolve.approve")
	case domain.ApprovalSubjectLegalEntityVerification:
		return s.authorize(ctx, "entity", "verify.approve")
	case domain.ApprovalSubjectLegalEntityMerge:
		return s.authorize(ctx, "entity", "merge.approve")
	case domain.ApprovalSubjectLegalEntityUnmerge:
		return s.authorize(ctx, "entity", "unmerge.approve")
	}
	return fmt.Errorf("%w: unknown approval subject %q", ErrInvalidInput, a.SubjectType)
}

// loadDecidable loads a request and applies every check common to approve
// and reject: tenant scope, still pending, not expired, the decider holds the
// approval permission, and the decider is not the maker.
func (s *Service) loadDecidable(ctx context.Context, id string) (*domain.ApprovalRequest, string, error) {
	decider := domain.PrincipalFromContext(ctx)
	if decider == "" {
		return nil, "", ErrUnauthenticated
	}
	a, err := s.GetApprovalRequest(ctx, id)
	if err != nil {
		return nil, "", err
	}
	if a.Status != domain.ApprovalPending {
		return nil, "", fmt.Errorf("%w: it is %s", ErrApprovalNotPending, a.Status)
	}
	if !time.Now().UTC().Before(a.ExpiresAt) {
		if err := s.store.DecideApprovalRequest(ctx, domain.ApprovalDecision{
			ApprovalRequestID: a.ApprovalRequestID, TenantID: a.TenantID,
			Note: "expired before a decision was made",
		}, domain.ApprovalExpired); err != nil && !errors.Is(err, ErrApprovalNotPending) {
			s.log.Error("failed to mark approval expired", zap.Error(err))
		}
		return nil, "", fmt.Errorf("%w: it expired at %s", ErrApprovalNotPending, a.ExpiresAt.Format(time.RFC3339))
	}
	if err := s.approvalAuthz(ctx, a); err != nil {
		return nil, "", err
	}
	if decider == a.RequestedByPrincipalID {
		return nil, "", ErrSelfApproval
	}
	return a, decider, nil
}

// RejectRequest records a verified principal's refusal of a proposal.
func (s *Service) RejectRequest(ctx context.Context, id string, body domain.RejectRequestBody) (*domain.ApprovalRequest, error) {
	if strings.TrimSpace(body.Note) == "" {
		return nil, fmt.Errorf("%w: note is required when rejecting", ErrInvalidInput)
	}
	a, decider, err := s.loadDecidable(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.store.DecideApprovalRequest(ctx, domain.ApprovalDecision{
		ApprovalRequestID: a.ApprovalRequestID, TenantID: a.TenantID,
		DecidedByPrincipalID: decider, Note: body.Note,
	}, domain.ApprovalRejected); err != nil {
		return nil, err
	}
	s.log.Info("approval request rejected",
		zap.String("approval_request_id", a.ApprovalRequestID),
		zap.String("decided_by", decider))
	return s.store.GetApprovalRequest(ctx, a.ApprovalRequestID)
}

// ApproveRequest releases a proposal: it re-validates the subject against the
// proposal and executes the stored command, recording the approval in the
// same transaction.
func (s *Service) ApproveRequest(ctx context.Context, id string, body domain.ApproveRequestBody) (*domain.ApprovalOutcome, error) {
	if strings.TrimSpace(body.PayloadFingerprint) == "" {
		return nil, fmt.Errorf("%w: payload_fingerprint is required — approve what you reviewed", ErrInvalidInput)
	}
	a, decider, err := s.loadDecidable(ctx, id)
	if err != nil {
		return nil, err
	}
	if body.PayloadFingerprint != a.PayloadFingerprint {
		return nil, ErrApprovalFingerprintMismatch
	}

	d := domain.ApprovalDecision{
		ApprovalRequestID:    a.ApprovalRequestID,
		TenantID:             a.TenantID,
		DecidedByPrincipalID: decider,
		Note:                 body.Note,
	}

	var result any
	switch a.SubjectType {
	case domain.ApprovalSubjectTenantCreation:
		result, err = s.approveTenantCreation(ctx, a, d)
	case domain.ApprovalSubjectTenantCommand:
		result, err = s.approveTenantCommand(ctx, a, d)
	case domain.ApprovalSubjectLegalProfileAmendment:
		result, err = s.approveLegalProfileAmendment(ctx, a, d)
	case domain.ApprovalSubjectRegistryConflictResolution:
		err = s.approveRegistryConflictResolution(ctx, a, d)
	case domain.ApprovalSubjectLegalEntityVerification:
		result, err = s.approveEntityVerification(ctx, a, d)
	case domain.ApprovalSubjectLegalEntityMerge:
		result, err = s.approveMerge(ctx, a, d)
	case domain.ApprovalSubjectLegalEntityUnmerge:
		result, err = s.approveUnmerge(ctx, a, d)
	default:
		err = fmt.Errorf("%w: unknown approval subject %q", ErrInvalidInput, a.SubjectType)
	}
	if err != nil {
		if subjectMoved(err) {
			s.markStale(ctx, a, err)
		}
		return nil, err
	}

	s.log.Info("approval request approved and executed",
		zap.String("approval_request_id", a.ApprovalRequestID),
		zap.String("command", a.CommandName),
		zap.String("requested_by", a.RequestedByPrincipalID),
		zap.String("decided_by", decider),
		zap.String("correlation_id", body.CorrelationID))

	decided, err := s.store.GetApprovalRequest(ctx, a.ApprovalRequestID)
	if err != nil {
		return nil, err
	}
	return &domain.ApprovalOutcome{ApprovalRequest: decided, Result: result}, nil
}

// subjectMoved reports whether an execution failure means the proposal can no
// longer apply as written, as opposed to a transient or permission failure
// after which the same approval may be retried.
func subjectMoved(err error) bool {
	return errors.Is(err, ErrConflict) ||
		errors.Is(err, ErrInvalidTransition) ||
		errors.Is(err, ErrRegistryConflict) ||
		errors.Is(err, ErrTenantNotTransactable) ||
		errors.Is(err, ErrApprovalFingerprintMismatch)
}

func (s *Service) markStale(ctx context.Context, a *domain.ApprovalRequest, cause error) {
	if err := s.store.DecideApprovalRequest(ctx, domain.ApprovalDecision{
		ApprovalRequestID: a.ApprovalRequestID, TenantID: a.TenantID,
		Note: "subject changed after proposal: " + cause.Error(),
	}, domain.ApprovalStale); err != nil && !errors.Is(err, ErrApprovalNotPending) {
		s.log.Error("failed to mark approval stale",
			zap.String("approval_request_id", a.ApprovalRequestID), zap.Error(err))
	}
}

// decodeVerified unmarshals the stored payload into dst and proves it still
// hashes to the stored fingerprint.
func decodeVerified(a *domain.ApprovalRequest, dst any) error {
	if err := json.Unmarshal(a.Payload, dst); err != nil {
		return fmt.Errorf("decode approval payload: %w", err)
	}
	fp, err := domain.ApprovalFingerprint(a.SubjectType, a.SubjectID, a.CommandName, a.ExpectedVersion, dst)
	if err != nil {
		return err
	}
	if fp != a.PayloadFingerprint {
		return fmt.Errorf("%w: stored proposal does not hash to its own fingerprint", ErrApprovalFingerprintMismatch)
	}
	return nil
}

func (s *Service) approveTenantCreation(ctx context.Context, a *domain.ApprovalRequest, d domain.ApprovalDecision) (any, error) {
	var p domain.TenantCreationPayload
	if err := decodeVerified(a, &p); err != nil {
		return nil, err
	}
	t, err := s.GetTenant(ctx, a.SubjectID)
	if err != nil {
		return nil, err
	}
	if t.LifecycleState != domain.TenantLifecycleOnboarding {
		return nil, fmt.Errorf("%w: tenant is %s, not ONBOARDING", ErrInvalidTransition, t.LifecycleState)
	}
	if t.TenantCode != p.TenantCode || t.LegalName != p.LegalName {
		return nil, fmt.Errorf("%w: tenant identity differs from the one proposed", ErrConflict)
	}
	if err := s.store.DecideApprovalRequest(ctx, d, domain.ApprovalApproved); err != nil {
		return nil, err
	}
	return nil, nil
}

func (s *Service) approveTenantCommand(ctx context.Context, a *domain.ApprovalRequest, d domain.ApprovalDecision) (any, error) {
	var req domain.ExecuteTenantCommandRequest
	if err := decodeVerified(a, &req); err != nil {
		return nil, err
	}
	command := domain.TenantCommand(a.CommandName)
	target, allowedFrom, ok := command.TargetState()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownCommand, command)
	}
	t, err := s.GetTenant(ctx, a.SubjectID)
	if err != nil {
		return nil, err
	}
	if t.RecordVersion != a.ExpectedVersion {
		return nil, fmt.Errorf("%w: proposed against version %d, tenant is now at %d",
			ErrVersionConflict, a.ExpectedVersion, t.RecordVersion)
	}
	return s.applyTenantCommand(ctx, t, command, target, allowedFrom, a.ExpectedVersion, req,
		a.RequestedByPrincipalID, &d)
}

func (s *Service) approveLegalProfileAmendment(ctx context.Context, a *domain.ApprovalRequest, d domain.ApprovalDecision) (any, error) {
	var req domain.AmendLegalProfileRequest
	if err := decodeVerified(a, &req); err != nil {
		return nil, err
	}
	e, err := s.GetEntity(ctx, a.SubjectID)
	if err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, e.TenantID); err != nil {
		return nil, err
	}
	if e.RecordVersion != a.ExpectedVersion {
		return nil, fmt.Errorf("%w: proposed against version %d, entity is now at %d",
			ErrVersionConflict, a.ExpectedVersion, e.RecordVersion)
	}
	return s.executeAmendment(ctx, e, req, a.RequestedByPrincipalID, &d)
}

func (s *Service) approveRegistryConflictResolution(ctx context.Context, a *domain.ApprovalRequest, d domain.ApprovalDecision) error {
	var req domain.ResolveRegistryConflictRequest
	if err := decodeVerified(a, &req); err != nil {
		return err
	}
	c, err := s.store.GetRegistryConflict(ctx, a.SubjectID)
	if err != nil {
		return err
	}
	if c == nil {
		return ErrNotFound
	}
	// "No self-approval of merge": the principal whose claim was quarantined
	// has the strongest interest in the outcome — RESOLVED_DISTINCT unblocks
	// their claim — and may not be the one to approve it.
	if d.DecidedByPrincipalID == c.DetectedByPrincipalID {
		return fmt.Errorf("%w: the principal whose registry claim was quarantined cannot approve its resolution", ErrSelfApproval)
	}
	if c.Status != domain.RegistryConflictOpen {
		return fmt.Errorf("%w: conflict is already %s", ErrConflict, c.Status)
	}
	return s.store.ResolveRegistryConflictApproved(ctx, c.ConflictID, req.Status, req.ResolutionNote,
		a.RequestedByPrincipalID, d)
}
