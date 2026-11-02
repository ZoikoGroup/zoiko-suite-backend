package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"zoiko.io/decision-support-svc/internal/clients"
	"zoiko.io/decision-support-svc/internal/domain"
	"zoiko.io/decision-support-svc/internal/events"
)

// Governance decision log vocabulary for a human disposition.
const (
	governanceActionDisposition = "DECISION_SUPPORT_DISPOSITION"
	governanceRuleBasis         = "ZS-SVC-X-001 AIG-04 human oversight output disposition"
)

// Decide applies one human disposition — accept, reject, expire or
// supersede — to a recommendation.
//
// The checks run in this order, and each has exactly one error:
//
//  1. body is valid for this disposition            -> FieldError (400)
//  2. recommendation exists in the caller's tenant  -> ErrRecommendationNotFound (404)
//  3. X-Legal-Entity-Id, if sent, matches it        -> ErrLegalEntityScopeMismatch (403)
//  4. principal may decide on that legal entity     -> ErrAuthorizationDenied (403) / ErrAuthzServiceUnavailable (503)
//  5. same Idempotency-Key + same request: replay   -> stored response; different request -> ErrIdempotencyKeyReused (422)
//  6. If-Match equals the current version           -> ErrVersionMismatch (412)
//  7. the state machine allows the move             -> *TransitionError (409)
//  8. segregation of duties (accept only)           -> ErrSegregationOfDuties (403)
//  9. review deadline not passed (accept/reject)    -> ErrReviewDeadlinePassed (409)
//  10. disposition-specific rules (O3 approval ref, early-expiry reason,
//     replacement checks)                           -> 400 / 409 / 422
//  11. persist row + history + outbox atomically; a concurrent change
//     between steps 6 and 11 is caught by the versioned UPDATE -> 412
//
// Accepting never executes the recommended action: the row records the
// human decision and downstream_action_executed stays false (AIG-04 §7.3).
func (s *Service) Decide(ctx context.Context, caller domain.CallerContext, idem domain.IdempotencyInput, cmd domain.DecisionCommand) (*domain.Recommendation, bool, error) {
	now := s.now()

	// Step 1.
	if err := validateDecision(cmd); err != nil {
		return nil, false, err
	}

	// Step 2.
	current, err := s.store.GetRecommendation(ctx, caller.TenantID, cmd.RecommendationID)
	if err != nil {
		return nil, false, err
	}

	// Step 3.
	if caller.LegalEntityID != "" && caller.LegalEntityID != current.LegalEntityID {
		return nil, false, domain.ErrLegalEntityScopeMismatch
	}

	// Step 4.
	if err := s.authorize(ctx, caller, current.LegalEntityID, ActionDecide); err != nil {
		return nil, false, err
	}

	// Step 5.
	if replayed, found, err := s.findReplay(ctx, caller.TenantID, idem); err != nil || found {
		return replayed, found, err
	}

	// Step 6.
	if cmd.ExpectedVersion != current.Version {
		return nil, false, domain.ErrVersionMismatch
	}

	// Step 7.
	if !domain.CanTransition(current.Status, cmd.TargetStatus) {
		return nil, false, &domain.TransitionError{From: current.Status, To: cmd.TargetStatus}
	}

	// Step 8.
	if err := checkSegregationOfDuties(*current, caller, cmd.TargetStatus); err != nil {
		return nil, false, err
	}

	// Step 9.
	deadlinePassed := !now.Before(current.ExpiresAt)
	isHumanJudgement := cmd.TargetStatus == domain.StatusAccepted || cmd.TargetStatus == domain.StatusRejected
	if deadlinePassed && isHumanJudgement {
		return nil, false, domain.ErrReviewDeadlinePassed
	}

	// Step 10.
	var replacement *domain.Recommendation
	switch cmd.TargetStatus {
	case domain.StatusAccepted:
		err = checkAcceptRules(*current, cmd.Request)
	case domain.StatusExpired:
		err = checkExpireRules(cmd.Request, deadlinePassed)
	case domain.StatusSuperseded:
		replacement, err = s.loadReplacement(ctx, caller, *current, cmd.Request.ReplacementRecommendationID)
	}
	if err != nil {
		return nil, false, err
	}

	// Step 11.
	write, updated, err := buildTransitionWrite(*current, caller, idem, cmd, replacement, now)
	if err != nil {
		return nil, false, err
	}
	err = s.store.ApplyTransition(ctx, write)
	if errors.Is(err, domain.ErrIdempotencyRace) {
		return s.replayAfterRace(ctx, caller.TenantID, idem)
	}
	if err != nil {
		return nil, false, err
	}
	return &updated, false, nil
}

// checkSegregationOfDuties enforces "no self-bypass" (AIG-04 §7.1 O2/O3):
// the requester of a material recommendation may not accept it.
func checkSegregationOfDuties(rec domain.Recommendation, caller domain.CallerContext, target domain.Status) error {
	isAccept := target == domain.StatusAccepted
	isSelfDecision := caller.PrincipalID == rec.RequestedByPrincipalID
	if isAccept && isSelfDecision && rec.OversightClass.RequiresIndependentReviewer() {
		return domain.ErrSegregationOfDuties
	}
	return nil
}

// checkAcceptRules: O3 (dual/control review) acceptance must cite the
// maker-checker / quorum approval that authorised it.
func checkAcceptRules(rec domain.Recommendation, req domain.DecisionRequest) error {
	if rec.OversightClass.RequiresApprovalReference() && strings.TrimSpace(req.ApprovalReference) == "" {
		return domain.NewFieldError("approval_reference", "is required to accept an O3 (dual/control review) recommendation")
	}
	return nil
}

// checkExpireRules: once the deadline has passed anyone allowed to decide
// may record the expiry; closing the window early needs a reason.
func checkExpireRules(req domain.DecisionRequest, deadlinePassed bool) error {
	if !deadlinePassed && strings.TrimSpace(req.Reason) == "" {
		return domain.NewFieldError("reason", "is required to expire a recommendation before its expires_at")
	}
	return nil
}

// loadReplacement fetches and checks the recommendation that supersedes
// current: it must be a different recommendation for the same legal
// entity, subject and action, and still awaiting review.
func (s *Service) loadReplacement(ctx context.Context, caller domain.CallerContext, current domain.Recommendation, replacementID string) (*domain.Recommendation, error) {
	if replacementID == current.RecommendationID {
		return nil, domain.ErrReplacementMismatched
	}

	replacement, err := s.store.GetRecommendation(ctx, caller.TenantID, replacementID)
	if errors.Is(err, domain.ErrRecommendationNotFound) {
		return nil, domain.ErrReplacementNotFound
	}
	if err != nil {
		return nil, err
	}

	sameScope := replacement.LegalEntityID == current.LegalEntityID &&
		replacement.SubjectType == current.SubjectType &&
		replacement.SubjectReference == current.SubjectReference &&
		replacement.ActionType == current.ActionType
	if !sameScope {
		return nil, domain.ErrReplacementMismatched
	}
	if replacement.Status != domain.StatusReviewRequired {
		return nil, domain.ErrReplacementNotPending
	}
	return replacement, nil
}

// buildTransitionWrite assembles the updated row, its history entry, its
// outbox messages and its idempotency record.
func buildTransitionWrite(current domain.Recommendation, caller domain.CallerContext, idem domain.IdempotencyInput, cmd domain.DecisionCommand, replacement *domain.Recommendation, now time.Time) (domain.TransitionWrite, domain.Recommendation, error) {
	fromStatus := current.Status

	updated := current
	updated.Status = cmd.TargetStatus
	updated.Version = current.Version + 1
	updated.UpdatedAt = now
	updated.DecidedByPrincipalID = &caller.PrincipalID
	updated.DecidedAt = &now
	updated.DecisionReason = optionalText(strings.TrimSpace(cmd.Request.Reason))
	updated.DownstreamActionRef = optionalText(cmd.Request.DownstreamActionRef)
	updated.ApprovalReference = optionalText(cmd.Request.ApprovalReference)
	updated.DownstreamActionExecuted = false
	if replacement != nil {
		updated.SupersededByRecommendationID = &replacement.RecommendationID
	}

	disposition := newDisposition(updated, &fromStatus, caller, cmd.Request, now)

	outbox, err := transitionOutbox(updated, disposition, caller)
	if err != nil {
		return domain.TransitionWrite{}, domain.Recommendation{}, err
	}

	idemRecord, err := newIdempotencyRecord(caller.TenantID, idem, operationForStatus(cmd.TargetStatus), updated)
	if err != nil {
		return domain.TransitionWrite{}, domain.Recommendation{}, err
	}

	write := domain.TransitionWrite{
		Updated:         updated,
		FromStatus:      fromStatus,
		ExpectedVersion: current.Version,
		Disposition:     disposition,
		Outbox:          outbox,
		Idempotency:     idemRecord,
	}
	if replacement != nil {
		write.ReplacementID = &replacement.RecommendationID
	}
	return write, updated, nil
}

// transitionOutbox returns the Kafka event for every transition, plus — for
// the two human judgements, ACCEPTED and REJECTED — a governance decision
// log record. Both are written in the same transaction as the state change:
// the decision cannot commit unless its evidence is durably queued
// (ZS-SVC-X-001 "Do not finalize material downstream acceptance until
// required evidence commitment succeeds; fail/queue safely").
func transitionOutbox(updated domain.Recommendation, disposition domain.Disposition, caller domain.CallerContext) ([]domain.OutboxMessage, error) {
	event, err := newLifecycleEvent(updated, caller.PrincipalID)
	if err != nil {
		return nil, err
	}
	messages := []domain.OutboxMessage{event}

	isHumanJudgement := updated.Status == domain.StatusAccepted || updated.Status == domain.StatusRejected
	if !isHumanJudgement {
		return messages, nil
	}

	governanceRecord, err := newGovernanceRecord(updated, disposition)
	if err != nil {
		return nil, err
	}
	return append(messages, governanceRecord), nil
}

func newLifecycleEvent(rec domain.Recommendation, actorID string) (domain.OutboxMessage, error) {
	return events.NewRecommendationEvent(events.EventTypeForStatus(rec.Status), rec, actorID)
}

func newGovernanceRecord(rec domain.Recommendation, disposition domain.Disposition) (domain.OutboxMessage, error) {
	evaluationContext, err := json.Marshal(map[string]any{
		"recommendation_id":          rec.RecommendationID,
		"recommended_action":         rec.RecommendedAction,
		"confidence_score":           rec.ConfidenceScore,
		"source":                     rec.Source,
		"model_id":                   rec.ModelID,
		"model_version":              rec.ModelVersion,
		"oversight_class":            rec.OversightClass,
		"requested_by_principal_id":  rec.RequestedByPrincipalID,
		"governed_action_type":       rec.ActionType,
		"reason":                     disposition.Reason,
		"downstream_action_ref":      disposition.DownstreamActionRef,
		"approval_reference":         disposition.ApprovalReference,
		"downstream_action_executed": false,
	})
	if err != nil {
		return domain.OutboxMessage{}, err
	}

	record := clients.DecisionRecord{
		TenantID:          rec.TenantID,
		LegalEntityID:     rec.LegalEntityID,
		ActorID:           disposition.DecidedByPrincipalID,
		CorrelationID:     disposition.CorrelationID,
		DecisionID:        disposition.DispositionID,
		ActionType:        governanceActionDisposition,
		Outcome:           string(rec.Status),
		RuleBasis:         governanceRuleBasis,
		ActionSubjectType: "recommendation",
		ActionSubjectID:   rec.RecommendationID,
		EvaluationContext: evaluationContext,
		DecidedAt:         disposition.DecidedAt,
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return domain.OutboxMessage{}, err
	}

	return domain.OutboxMessage{
		OutboxID:    disposition.DispositionID,
		TenantID:    rec.TenantID,
		Destination: domain.OutboxDestinationGovernanceLog,
		MessageType: governanceActionDisposition,
		MessageKey:  rec.RecommendationID,
		Payload:     payload,
	}, nil
}

func operationForStatus(status domain.Status) string {
	switch status {
	case domain.StatusAccepted:
		return "accept_recommendation"
	case domain.StatusRejected:
		return "reject_recommendation"
	case domain.StatusExpired:
		return "expire_recommendation"
	default:
		return "supersede_recommendation"
	}
}
