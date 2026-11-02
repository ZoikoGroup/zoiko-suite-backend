package service

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"zoiko.io/compliance-risk-scoring-svc/internal/domain"
	"zoiko.io/compliance-risk-scoring-svc/internal/events"
	"zoiko.io/compliance-risk-scoring-svc/internal/store"
)

const aggregateThresholdRule = "RiskThresholdRule"

// thresholdAuthzScope is the legal entity sent to authorization-svc for
// threshold actions. Threshold rules are tenant-wide, so the check is made
// against the caller's X-Legal-Entity-Id scope, which the envelope requires
// on every write.
func thresholdAuthzScope(cmd CommandContext) (string, error) {
	if cmd.LegalEntityScope == "" {
		return "", domain.BadRequest("legal_entity_scope_required",
			"X-Legal-Entity-Id header is required for threshold rule changes")
	}
	return cmd.LegalEntityScope, nil
}

// ---------------------------------------------------------------------------
// Create
// ---------------------------------------------------------------------------

// CreateThresholdRule validates the body, authorizes
// RISK_THRESHOLD_RULE_CREATE and stores a new DRAFT rule. A draft has no
// effect on scoring until a different principal approves it.
func (s *Service) CreateThresholdRule(ctx context.Context, cmd CommandContext, input domain.ThresholdRuleInput) (*CommandResult, error) {
	input.Normalize()
	if problems := input.Validate(); len(problems) > 0 {
		return nil, domain.ValidationError(problems)
	}
	if err := checkBodyTenant(input.TenantID, cmd); err != nil {
		return nil, err
	}
	scope, err := thresholdAuthzScope(cmd)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, cmd, scope, ActionRiskThresholdRuleCreate); err != nil {
		return nil, err
	}

	return s.executeIdempotently(ctx, cmd, "threshold_rule.create", func(q *store.TenantQueries, now time.Time) (*workResult, error) {
		cutoffs := input.Cutoffs()
		rule := &domain.RiskThresholdRule{
			ID:                  uuid.New().String(),
			TenantID:            cmd.TenantID,
			RuleName:            input.RuleName,
			RiskCategory:        domain.RiskCategory(input.RiskCategory),
			ModerateThreshold:   cutoffs.Moderate,
			HighThreshold:       cutoffs.High,
			CriticalThreshold:   cutoffs.Critical,
			NotificationChannel: input.NotificationChannel,
			Status:              domain.ThresholdDraft,
			Version:             1,
			EffectiveFrom:       input.EffectiveFrom,
			CreatedBy:           cmd.PrincipalID,
			UpdatedBy:           cmd.PrincipalID,
			CreatedAt:           now,
			UpdatedAt:           now,
		}
		if err := q.InsertThresholdRule(ctx, rule); err != nil {
			return nil, err
		}
		if err := enqueueEvent(ctx, q, cmd, now, events.TypeThresholdRuleCreated, aggregateThresholdRule,
			rule.ID, scope, rule); err != nil {
			return nil, err
		}
		return &workResult{statusCode: 201, response: rule, version: rule.Version}, nil
	})
}

// ---------------------------------------------------------------------------
// Update (DRAFT only, If-Match required)
// ---------------------------------------------------------------------------

// UpdateThresholdRule replaces the editable fields of a DRAFT rule. If-Match
// is mandatory (428 without it, 412 on a stale version); a non-draft is 409.
func (s *Service) UpdateThresholdRule(ctx context.Context, cmd CommandContext, id string, input domain.ThresholdRuleInput) (*CommandResult, error) {
	input.Normalize()
	if problems := input.Validate(); len(problems) > 0 {
		return nil, domain.ValidationError(problems)
	}
	if err := checkBodyTenant(input.TenantID, cmd); err != nil {
		return nil, err
	}
	if !cmd.IfMatchPresent {
		return nil, domain.PreconditionRequired("If-Match header with the rule's current ETag is required to update a threshold rule")
	}
	if _, err := s.GetThresholdRule(ctx, QueryContext{TenantID: cmd.TenantID}, id); err != nil {
		return nil, err
	}
	scope, err := thresholdAuthzScope(cmd)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, cmd, scope, ActionRiskThresholdRuleUpdate); err != nil {
		return nil, err
	}

	return s.executeIdempotently(ctx, cmd, "threshold_rule.update", func(q *store.TenantQueries, now time.Time) (*workResult, error) {
		current, err := q.GetThresholdRule(ctx, id, true)
		if err != nil {
			return nil, err
		}
		if transitionErr := current.CanEdit(); transitionErr != nil {
			return nil, transitionErr
		}
		if err := checkIfMatch(cmd, current.Version); err != nil {
			return nil, err
		}

		cutoffs := input.Cutoffs()
		edited := *current
		edited.RuleName = input.RuleName
		edited.RiskCategory = domain.RiskCategory(input.RiskCategory)
		edited.ModerateThreshold = cutoffs.Moderate
		edited.HighThreshold = cutoffs.High
		edited.CriticalThreshold = cutoffs.Critical
		edited.NotificationChannel = input.NotificationChannel
		edited.EffectiveFrom = input.EffectiveFrom
		edited.UpdatedBy = cmd.PrincipalID
		edited.UpdatedAt = now

		if err := q.UpdateThresholdDraft(ctx, &edited, current.Version); err != nil {
			return nil, err
		}
		updated, err := q.GetThresholdRule(ctx, id, false)
		if err != nil {
			return nil, err
		}
		if err := enqueueEvent(ctx, q, cmd, now, events.TypeThresholdRuleUpdated, aggregateThresholdRule,
			id, scope, updated); err != nil {
			return nil, err
		}
		return &workResult{statusCode: 200, response: updated, version: updated.Version}, nil
	})
}

// ---------------------------------------------------------------------------
// Approve (maker-checker)
// ---------------------------------------------------------------------------

// ApproveThresholdRule activates a DRAFT. Rules:
//   - approver must differ from the author and last editor (403 SoD);
//   - effective_from may not be in the past (422);
//   - effective_from may not precede the currently ACTIVE rule's (409);
//   - the currently ACTIVE rule for the category becomes SUPERSEDED and stays
//     in force until the new rule's effective_from.
func (s *Service) ApproveThresholdRule(ctx context.Context, cmd CommandContext, id string, request domain.ThresholdTransitionRequest) (*CommandResult, error) {
	if err := checkBodyTenant(request.TenantID, cmd); err != nil {
		return nil, err
	}
	if _, err := s.GetThresholdRule(ctx, QueryContext{TenantID: cmd.TenantID}, id); err != nil {
		return nil, err
	}
	scope, err := thresholdAuthzScope(cmd)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, cmd, scope, ActionRiskThresholdRuleApprove); err != nil {
		return nil, err
	}

	return s.executeIdempotently(ctx, cmd, "threshold_rule.approve", func(q *store.TenantQueries, now time.Time) (*workResult, error) {
		draft, err := q.GetThresholdRule(ctx, id, true)
		if err != nil {
			return nil, err
		}
		if err := checkIfMatch(cmd, draft.Version); err != nil {
			return nil, err
		}
		if transitionErr := draft.CanApprove(cmd.PrincipalID, todayUTC(now)); transitionErr != nil {
			return nil, transitionErr
		}

		var supersededRuleID *string
		active, err := q.FindActiveThresholdRule(ctx, draft.RiskCategory)
		switch {
		case errors.Is(err, store.ErrNotFound):
			// first approved rule for this category
		case err != nil:
			return nil, err
		default:
			if draft.EffectiveFrom < active.EffectiveFrom {
				return nil, domain.Conflict("effective_from_before_active_rule",
					"effective_from "+draft.EffectiveFrom+" precedes the active rule's effective_from "+active.EffectiveFrom)
			}
			if err := q.SupersedeThresholdRule(ctx, active.ID, draft.EffectiveFrom, now); err != nil {
				return nil, err
			}
			supersededRuleID = &active.ID
			if err := enqueueEvent(ctx, q, cmd, now, events.TypeThresholdRuleSuperseded, aggregateThresholdRule,
				active.ID, scope, map[string]any{
					"rule_id": active.ID, "superseded_by_rule_id": draft.ID, "effective_to": draft.EffectiveFrom,
				}); err != nil {
				return nil, err
			}
		}

		if err := q.ApproveThresholdRule(ctx, id, cmd.PrincipalID, supersededRuleID, now); err != nil {
			return nil, err
		}
		approved, err := q.GetThresholdRule(ctx, id, false)
		if err != nil {
			return nil, err
		}
		if err := enqueueEvent(ctx, q, cmd, now, events.TypeThresholdRuleApproved, aggregateThresholdRule,
			id, scope, approved); err != nil {
			return nil, err
		}
		return &workResult{statusCode: 200, response: approved, version: approved.Version}, nil
	})
}

// ---------------------------------------------------------------------------
// Retire
// ---------------------------------------------------------------------------

// RetireThresholdRule ends a DRAFT or ACTIVE rule. An ACTIVE rule stops
// being in force today (or never takes effect, if its effective_from is in
// the future); scoring then falls back to the platform default ladder.
func (s *Service) RetireThresholdRule(ctx context.Context, cmd CommandContext, id string, request domain.ThresholdTransitionRequest) (*CommandResult, error) {
	if err := checkBodyTenant(request.TenantID, cmd); err != nil {
		return nil, err
	}
	if _, err := s.GetThresholdRule(ctx, QueryContext{TenantID: cmd.TenantID}, id); err != nil {
		return nil, err
	}
	scope, err := thresholdAuthzScope(cmd)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, cmd, scope, ActionRiskThresholdRuleRetire); err != nil {
		return nil, err
	}

	return s.executeIdempotently(ctx, cmd, "threshold_rule.retire", func(q *store.TenantQueries, now time.Time) (*workResult, error) {
		rule, err := q.GetThresholdRule(ctx, id, true)
		if err != nil {
			return nil, err
		}
		if err := checkIfMatch(cmd, rule.Version); err != nil {
			return nil, err
		}
		if transitionErr := rule.CanRetire(); transitionErr != nil {
			return nil, transitionErr
		}

		var effectiveTo *string
		if rule.Status == domain.ThresholdActive {
			endDate := todayUTC(now)
			if rule.EffectiveFrom > endDate {
				endDate = rule.EffectiveFrom
			}
			effectiveTo = &endDate
		}

		if err := q.RetireThresholdRule(ctx, id, cmd.PrincipalID, effectiveTo, now); err != nil {
			return nil, err
		}
		retired, err := q.GetThresholdRule(ctx, id, false)
		if err != nil {
			return nil, err
		}
		if err := enqueueEvent(ctx, q, cmd, now, events.TypeThresholdRuleRetired, aggregateThresholdRule,
			id, scope, map[string]any{"rule_id": id, "previous_status": rule.Status, "effective_to": retired.EffectiveTo}); err != nil {
			return nil, err
		}
		return &workResult{statusCode: 200, response: retired, version: retired.Version}, nil
	})
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// GetThresholdRule returns one rule or 404.
func (s *Service) GetThresholdRule(ctx context.Context, query QueryContext, id string) (*domain.RiskThresholdRule, error) {
	var rule *domain.RiskThresholdRule
	err := s.store.InTenantTx(ctx, query.TenantID, func(q *store.TenantQueries) error {
		found, err := q.GetThresholdRule(ctx, id, false)
		rule = found
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil, domain.NotFound("threshold rule " + id + " not found")
	}
	if err != nil {
		return nil, err
	}
	return rule, nil
}

// ThresholdPage is one page of rules.
type ThresholdPage struct {
	Data       []domain.RiskThresholdRule `json:"data"`
	Count      int                        `json:"count"`
	NextCursor *string                    `json:"next_cursor"`
}

// ListThresholdRules returns a filtered page of rules, newest first.
func (s *Service) ListThresholdRules(ctx context.Context, query QueryContext, filter store.ThresholdFilter, page store.PageRequest) (*ThresholdPage, error) {
	var rules []domain.RiskThresholdRule
	var next *store.PageCursor
	err := s.store.InTenantTx(ctx, query.TenantID, func(q *store.TenantQueries) error {
		var err error
		rules, next, err = q.ListThresholdRules(ctx, filter, page)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &ThresholdPage{Data: rules, Count: len(rules), NextCursor: encodeNext(next)}, nil
}

// EffectiveThreshold is the ladder that applies to one category on a date.
type EffectiveThreshold struct {
	RiskCategory domain.RiskCategory       `json:"risk_category"`
	Source       string                    `json:"source"` // TENANT_RULE | PLATFORM_DEFAULT | NONE
	Cutoffs      *domain.BandCutoffs       `json:"cutoffs"`
	Rule         *domain.RiskThresholdRule `json:"rule"`
}

// EffectiveThresholds is the response of GET /thresholds/effective.
type EffectiveThresholds struct {
	AsOf         string               `json:"as_of"`
	ModelVersion string               `json:"model_version"`
	Thresholds   []EffectiveThreshold `json:"thresholds"`
}

// GetEffectiveThresholds resolves, for asOfDate, the ladder for COMPOSITE
// (tenant rule or platform default) and for each factor category (tenant
// rule or NONE: factors are only banded when a tenant rule exists).
func (s *Service) GetEffectiveThresholds(ctx context.Context, query QueryContext, asOfDate string) (*EffectiveThresholds, error) {
	if asOfDate == "" {
		asOfDate = todayUTC(s.currentTime())
	}

	var rules map[domain.RiskCategory]domain.RiskThresholdRule
	err := s.store.InTenantTx(ctx, query.TenantID, func(q *store.TenantQueries) error {
		var err error
		rules, err = q.EffectiveThresholdRules(ctx, asOfDate)
		return err
	})
	if err != nil {
		return nil, err
	}

	categories := []domain.RiskCategory{domain.CategoryComposite}
	for _, factor := range domain.CurrentModel.Factors {
		categories = append(categories, factor.Category)
	}

	response := &EffectiveThresholds{AsOf: asOfDate, ModelVersion: domain.CurrentModel.Version}
	for _, category := range categories {
		entry := EffectiveThreshold{RiskCategory: category, Source: "NONE"}
		if rule, found := rules[category]; found {
			ruleCopy := rule
			cutoffs := rule.Cutoffs()
			entry.Source = string(domain.ThresholdSourceTenantRule)
			entry.Rule = &ruleCopy
			entry.Cutoffs = &cutoffs
		} else if category == domain.CategoryComposite {
			defaults := domain.CurrentModel.DefaultBands
			entry.Source = string(domain.ThresholdSourcePlatformDefault)
			entry.Cutoffs = &defaults
		}
		response.Thresholds = append(response.Thresholds, entry)
	}
	return response, nil
}
