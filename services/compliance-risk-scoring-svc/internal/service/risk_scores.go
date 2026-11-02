package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"zoiko.io/compliance-risk-scoring-svc/internal/domain"
	"zoiko.io/compliance-risk-scoring-svc/internal/events"
	"zoiko.io/compliance-risk-scoring-svc/internal/store"
)

const aggregateRiskScore = "RiskScoreAssessment"

// ---------------------------------------------------------------------------
// Calculate
// ---------------------------------------------------------------------------

// CalculateScore validates the request, authorizes RISK_SCORE_CREATE,
// resolves the thresholds in force today, computes the score, supersedes the
// subject's previous ACTIVE score and stores the new one with its factor
// breakdown — all in one transaction with the idempotency record and the
// outbox events.
func (s *Service) CalculateScore(ctx context.Context, cmd CommandContext, request domain.CalculateRiskScoreRequest) (*CommandResult, error) {
	// Step 1: validate the body.
	request.Normalize()
	if problems := request.Validate(); len(problems) > 0 {
		return nil, domain.ValidationError(problems)
	}

	// Step 2: tenant and legal-entity scope must agree with the headers.
	if err := checkBodyTenant(request.TenantID, cmd); err != nil {
		return nil, err
	}
	if cmd.LegalEntityScope != "" && cmd.LegalEntityScope != request.LegalEntityID {
		return nil, domain.Forbidden("legal_entity_scope_mismatch",
			"legal_entity_id in the body does not match the X-Legal-Entity-Id header")
	}

	// Step 3: authorization (fail closed).
	if err := s.authorize(ctx, cmd, request.LegalEntityID, ActionRiskScoreCreate); err != nil {
		return nil, err
	}

	// Step 4: compute and persist under the Idempotency-Key.
	return s.executeIdempotently(ctx, cmd, "risk_score.calculate", func(q *store.TenantQueries, now time.Time) (*workResult, error) {
		assessment, err := s.scoreAndStore(ctx, q, cmd, request, now)
		if err != nil {
			return nil, err
		}
		return &workResult{statusCode: 201, response: assessment, version: assessment.Version}, nil
	})
}

// scoreAndStore is the transactional body of CalculateScore.
func (s *Service) scoreAndStore(ctx context.Context, q *store.TenantQueries, cmd CommandContext,
	request domain.CalculateRiskScoreRequest, now time.Time) (*domain.RiskScoreAssessment, error) {

	// Thresholds in force on today's date.
	effectiveRules, err := q.EffectiveThresholdRules(ctx, todayUTC(now))
	if err != nil {
		return nil, err
	}
	thresholds := scoringThresholdsFrom(effectiveRules)

	// Deterministic computation.
	subject := request.Subject()
	result := domain.ComputeScore(domain.CurrentModel, subject, request.Inputs(), thresholds)

	// The subject's previous ACTIVE score (if any) is superseded.
	previous, err := q.FindActiveScoreForSubject(ctx, subject.Type, subject.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if previous != nil && previous.LegalEntityID != subject.LegalEntityID {
		return nil, domain.Conflict("subject_legal_entity_mismatch",
			"subject "+string(subject.Type)+"/"+subject.ID+" was previously scored under legal entity "+previous.LegalEntityID)
	}

	assessment := newAssessment(cmd, request, result, thresholds.Composite, now)
	if previous != nil {
		delta := assessment.CompositeRiskScore - previous.CompositeRiskScore
		assessment.PreviousScoreID = &previous.ID
		assessment.ScoreDelta = &delta

		if err := q.MarkSuperseded(ctx, previous.ID, assessment.ID, now); err != nil {
			return nil, err
		}
	}

	if err := q.InsertAssessment(ctx, assessment); err != nil {
		return nil, err
	}

	// Events: calculated, optionally superseded, optionally threshold exceeded.
	if err := enqueueEvent(ctx, q, cmd, now, events.TypeRiskScoreCalculated, aggregateRiskScore,
		assessment.ID, assessment.LegalEntityID, calculatedEventPayload(assessment)); err != nil {
		return nil, err
	}
	if previous != nil {
		if err := enqueueEvent(ctx, q, cmd, now, events.TypeRiskScoreSuperseded, aggregateRiskScore,
			previous.ID, previous.LegalEntityID, map[string]any{
				"assessment_id": previous.ID, "superseded_by_id": assessment.ID, "status": domain.ScoreSuperseded,
			}); err != nil {
			return nil, err
		}
	}
	if assessment.RequiresHumanReview {
		if err := enqueueEvent(ctx, q, cmd, now, events.TypeRiskThresholdExceeded, aggregateRiskScore,
			assessment.ID, assessment.LegalEntityID, thresholdExceededPayload(assessment)); err != nil {
			return nil, err
		}
	}
	return assessment, nil
}

// scoringThresholdsFrom turns the effective tenant rules into the ladders
// ComputeScore needs: the COMPOSITE rule (or the platform default) for the
// overall band, and any per-category rules for the factor bands.
func scoringThresholdsFrom(rules map[domain.RiskCategory]domain.RiskThresholdRule) domain.ScoringThresholds {
	thresholds := domain.ScoringThresholds{
		Composite: domain.AppliedThresholds{
			Source:  domain.ThresholdSourcePlatformDefault,
			Cutoffs: domain.CurrentModel.DefaultBands,
		},
		ByCategory: map[domain.RiskCategory]domain.BandCutoffs{},
	}

	for category, rule := range rules {
		if category == domain.CategoryComposite {
			ruleID := rule.ID
			ruleVersion := rule.Version
			thresholds.Composite = domain.AppliedThresholds{
				Source:      domain.ThresholdSourceTenantRule,
				RuleID:      &ruleID,
				RuleVersion: &ruleVersion,
				Cutoffs:     rule.Cutoffs(),
			}
			continue
		}
		thresholds.ByCategory[category] = rule.Cutoffs()
	}
	return thresholds
}

// newAssessment assembles the row to insert from the computation result.
func newAssessment(cmd CommandContext, request domain.CalculateRiskScoreRequest, result domain.ScoreResult,
	applied domain.AppliedThresholds, now time.Time) *domain.RiskScoreAssessment {

	reviewStatus := domain.ReviewNotRequired
	if result.Tier.RequiresHumanReview() {
		reviewStatus = domain.ReviewPending
	}

	sourceReferences := []string{}
	for _, reference := range request.SourceReferences {
		sourceReferences = append(sourceReferences, strings.TrimSpace(reference))
	}

	assessment := &domain.RiskScoreAssessment{
		ID:                    uuid.New().String(),
		TenantID:              cmd.TenantID,
		LegalEntityID:         request.LegalEntityID,
		SubjectType:           domain.SubjectType(request.SubjectType),
		SubjectID:             request.SubjectID,
		AssessmentName:        request.AssessmentName,
		OpenObligationsCount:  *request.OpenObligationsCount,
		PolicyViolationsCount: *request.PolicyViolationsCount,
		AuditExceptionsCount:  *request.AuditExceptionsCount,
		PrivacyIncidentsCount: *request.PrivacyIncidentsCount,
		TaxPenaltiesCount:     *request.TaxPenaltiesCount,
		CompositeRiskScore:    result.CompositeScore,
		RiskTier:              result.Tier,
		ModelVersion:          result.ModelVersion,
		InputHash:             result.InputHash,
		ThresholdsApplied:     applied,
		BandRationale:         result.BandRationale,
		AssuranceState:        domain.AssuranceAdvisory,
		RequiresHumanReview:   result.Tier.RequiresHumanReview(),
		ReviewStatus:          reviewStatus,
		Status:                domain.ScoreActive,
		SourceReferences:      sourceReferences,
		CalculatedBy:          cmd.PrincipalID,
		Version:               1,
		EvaluatedAt:           now,
		CreatedAt:             now,
		UpdatedAt:             now,
	}

	for _, factor := range result.Factors {
		assessment.FactorBreakdowns = append(assessment.FactorBreakdowns, domain.FactorBreakdown{
			ID:                    uuid.New().String(),
			RiskCategory:          factor.Category,
			InputCount:            factor.InputCount,
			SaturationCount:       factor.SaturationCount,
			CategoryWeight:        factor.Weight,
			RawScore:              factor.RawScore,
			WeightedScore:         factor.WeightedScore,
			CategoryThresholdBand: factor.CategoryThresholdBand,
			RiskDriverSummary:     factor.DriverSummary,
			CreatedAt:             now,
		})
	}
	return assessment
}

func calculatedEventPayload(a *domain.RiskScoreAssessment) map[string]any {
	return map[string]any{
		"assessment_id":         a.ID,
		"subject_type":          a.SubjectType,
		"subject_id":            a.SubjectID,
		"composite_risk_score":  a.CompositeRiskScore,
		"risk_tier":             a.RiskTier,
		"model_version":         a.ModelVersion,
		"input_hash":            a.InputHash,
		"thresholds_applied":    a.ThresholdsApplied,
		"previous_score_id":     a.PreviousScoreID,
		"score_delta":           a.ScoreDelta,
		"assurance_state":       a.AssuranceState,
		"requires_human_review": a.RequiresHumanReview,
	}
}

// thresholdExceededPayload states explicitly that the signal is advisory:
// consumers must not take adverse action on the score alone.
func thresholdExceededPayload(a *domain.RiskScoreAssessment) map[string]any {
	return map[string]any{
		"assessment_id":         a.ID,
		"risk_tier":             a.RiskTier,
		"score":                 a.CompositeRiskScore,
		"thresholds_applied":    a.ThresholdsApplied,
		"assurance_state":       domain.AssuranceAdvisory,
		"requires_human_review": true,
		"review_status":         a.ReviewStatus,
	}
}

// ---------------------------------------------------------------------------
// Review
// ---------------------------------------------------------------------------

// ReviewScore records a human disposition (CONFIRMED / DISMISSED /
// ESCALATED) on an advisory HIGH/CRITICAL score. The reviewer must not be
// the principal who calculated it.
func (s *Service) ReviewScore(ctx context.Context, cmd CommandContext, id string, request domain.ReviewRiskScoreRequest) (*CommandResult, error) {
	if problems := request.Validate(); len(problems) > 0 {
		return nil, domain.ValidationError(problems)
	}
	if err := checkBodyTenant(request.TenantID, cmd); err != nil {
		return nil, err
	}

	current, err := s.GetScore(ctx, QueryContext{TenantID: cmd.TenantID}, id)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, cmd, current.LegalEntityID, ActionRiskScoreReview); err != nil {
		return nil, err
	}

	disposition := domain.ReviewDisposition(request.Disposition)
	return s.executeIdempotently(ctx, cmd, "risk_score.review", func(q *store.TenantQueries, now time.Time) (*workResult, error) {
		locked, err := q.GetAssessment(ctx, id, true)
		if err != nil {
			return nil, err
		}
		if err := checkIfMatch(cmd, locked.Version); err != nil {
			return nil, err
		}
		if transitionErr := locked.CanReview(disposition, cmd.PrincipalID); transitionErr != nil {
			return nil, transitionErr
		}

		if err := q.RecordReview(ctx, id, domain.ReviewStatus(disposition), cmd.PrincipalID, request.Note, now); err != nil {
			return nil, err
		}
		reviewed, err := q.GetAssessment(ctx, id, false)
		if err != nil {
			return nil, err
		}

		if err := enqueueEvent(ctx, q, cmd, now, events.TypeRiskScoreReviewed, aggregateRiskScore, id,
			reviewed.LegalEntityID, map[string]any{
				"assessment_id":   id,
				"disposition":     disposition,
				"previous_status": locked.ReviewStatus,
				"review_status":   reviewed.ReviewStatus,
				"risk_tier":       reviewed.RiskTier,
				"note":            request.Note,
			}); err != nil {
			return nil, err
		}
		return &workResult{statusCode: 200, response: reviewed, version: reviewed.Version}, nil
	})
}

// ---------------------------------------------------------------------------
// Archive
// ---------------------------------------------------------------------------

// ArchiveScore moves a score to ARCHIVED (never deletes it). Refused while a
// HIGH/CRITICAL review is outstanding.
func (s *Service) ArchiveScore(ctx context.Context, cmd CommandContext, id string, request domain.ArchiveRiskScoreRequest) (*CommandResult, error) {
	if err := checkBodyTenant(request.TenantID, cmd); err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(request.Reason)
	if len(reason) > domain.MaxArchiveReasonChars {
		return nil, domain.ValidationError([]domain.FieldError{{Field: "reason", Detail: "must be at most 500 characters"}})
	}

	current, err := s.GetScore(ctx, QueryContext{TenantID: cmd.TenantID}, id)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, cmd, current.LegalEntityID, ActionRiskAssessmentArchive); err != nil {
		return nil, err
	}

	return s.executeIdempotently(ctx, cmd, "risk_score.archive", func(q *store.TenantQueries, now time.Time) (*workResult, error) {
		locked, err := q.GetAssessment(ctx, id, true)
		if err != nil {
			return nil, err
		}
		if err := checkIfMatch(cmd, locked.Version); err != nil {
			return nil, err
		}
		if transitionErr := locked.CanArchive(); transitionErr != nil {
			return nil, transitionErr
		}

		var reasonPointer *string
		if reason != "" {
			reasonPointer = &reason
		}
		if err := q.ArchiveAssessment(ctx, id, cmd.PrincipalID, reasonPointer, now); err != nil {
			return nil, err
		}
		archived, err := q.GetAssessment(ctx, id, false)
		if err != nil {
			return nil, err
		}

		if err := enqueueEvent(ctx, q, cmd, now, events.TypeRiskAssessmentArchived, aggregateRiskScore, id,
			archived.LegalEntityID, map[string]any{
				"id": id, "status": domain.ScoreArchived, "previous_status": locked.Status, "reason": reasonPointer,
			}); err != nil {
			return nil, err
		}
		return &workResult{statusCode: 200, response: archived, version: archived.Version}, nil
	})
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// GetScore returns one score with its factor breakdown; another tenant's
// score is reported as not found.
func (s *Service) GetScore(ctx context.Context, query QueryContext, id string) (*domain.RiskScoreAssessment, error) {
	var assessment *domain.RiskScoreAssessment
	err := s.store.InTenantTx(ctx, query.TenantID, func(q *store.TenantQueries) error {
		found, err := q.GetAssessment(ctx, id, false)
		assessment = found
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil, domain.NotFound("risk score " + id + " not found")
	}
	if err != nil {
		return nil, err
	}
	return assessment, nil
}

// ScorePage is one page of scores.
type ScorePage struct {
	Data       []domain.RiskScoreAssessment `json:"data"`
	Count      int                          `json:"count"`
	NextCursor *string                      `json:"next_cursor"`
}

// ListScores returns a filtered page of scores, newest first.
func (s *Service) ListScores(ctx context.Context, query QueryContext, filter store.AssessmentFilter, page store.PageRequest) (*ScorePage, error) {
	var assessments []domain.RiskScoreAssessment
	var next *store.PageCursor
	err := s.store.InTenantTx(ctx, query.TenantID, func(q *store.TenantQueries) error {
		var err error
		assessments, next, err = q.ListAssessments(ctx, filter, page)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &ScorePage{Data: assessments, Count: len(assessments), NextCursor: encodeNext(next)}, nil
}

// SubjectHistory is the score history of one subject.
type SubjectHistory struct {
	SubjectType  domain.SubjectType           `json:"subject_type"`
	SubjectID    string                       `json:"subject_id"`
	CurrentScore *domain.RiskScoreAssessment  `json:"current_score"`
	Trend        string                       `json:"trend"`
	Data         []domain.RiskScoreAssessment `json:"data"`
	Count        int                          `json:"count"`
	NextCursor   *string                      `json:"next_cursor"`
}

// GetSubjectHistory returns every score ever calculated for a subject
// (ACTIVE, SUPERSEDED and ARCHIVED), newest first, plus the current ACTIVE
// score and a trend derived from its delta to the previous score.
func (s *Service) GetSubjectHistory(ctx context.Context, query QueryContext, subjectType domain.SubjectType, subjectID string, page store.PageRequest) (*SubjectHistory, error) {
	history := &SubjectHistory{SubjectType: subjectType, SubjectID: subjectID, Trend: "NO_HISTORY"}

	err := s.store.InTenantTx(ctx, query.TenantID, func(q *store.TenantQueries) error {
		filter := store.AssessmentFilter{SubjectType: string(subjectType), SubjectID: subjectID}
		scores, next, err := q.ListAssessments(ctx, filter, page)
		if err != nil {
			return err
		}
		history.Data = scores
		history.Count = len(scores)
		history.NextCursor = encodeNext(next)

		current, err := q.FindActiveScoreForSubject(ctx, subjectType, subjectID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		history.CurrentScore = current
		history.Trend = trendOf(current)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if history.Count == 0 && page.Cursor == nil {
		return nil, domain.NotFound("no risk scores for subject " + string(subjectType) + "/" + subjectID)
	}
	return history, nil
}

// trendOf classifies the current score's movement against its predecessor.
func trendOf(current *domain.RiskScoreAssessment) string {
	if current.ScoreDelta == nil {
		return "FIRST_SCORE"
	}
	switch {
	case *current.ScoreDelta > 0:
		return "WORSENING"
	case *current.ScoreDelta < 0:
		return "IMPROVING"
	default:
		return "STABLE"
	}
}

func encodeNext(next *store.PageCursor) *string {
	if next == nil {
		return nil
	}
	token := store.EncodeCursor(*next)
	return &token
}
