package cache

import (
	"context"

	"zoiko.io/authorization-svc/internal/domain"
)

// Governed changes (000034-000036). The handler finds these by type
// assertion, so each is forwarded explicitly; a missing one would answer 503
// in production while every test over the bare store passed.
//
// Only an APPROVED rule change alters what evaluation reads, so only the
// decisions invalidate. A request changes nothing a decision reads.

type governedInner interface {
	ApproveBreakGlassSession(ctx context.Context, sessionID, tenantID, approver string, stepUp *domain.StepUpEvidence) (*domain.BreakGlassSession, error)
	RejectBreakGlassSession(ctx context.Context, sessionID, tenantID, decider string) (*domain.BreakGlassSession, error)
	FindSoDRule(ctx context.Context, sodRuleID, tenantID string) (*domain.SoDRule, error)
	RequestSoDRuleChange(ctx context.Context, sodRuleID, tenantID, change, requester, reason string, expectedVersion int64) (*domain.SoDRule, bool, error)
	DecideSoDRuleChange(ctx context.Context, sodRuleID, tenantID string, platformWide bool, decider string, approve bool) (*domain.SoDRule, error)
	FindABACRule(ctx context.Context, abacRuleID, tenantID string) (*domain.ABACRule, error)
	RequestABACRuleChange(ctx context.Context, abacRuleID, tenantID, change, requester, reason string, expectedVersion int64) (*domain.ABACRule, bool, error)
	DecideABACRuleChange(ctx context.Context, abacRuleID, tenantID string, platformWide bool, decider string, approve bool) (*domain.ABACRule, error)
	RequestAuthorityLimitRetire(ctx context.Context, limitID, tenantID, requester, reason string, expectedVersion int64) (*domain.AuthorityLimit, error)
	DecideAuthorityLimit(ctx context.Context, limitID, tenantID, decider string, approve bool) (*domain.AuthorityLimit, error)
}

func (s *Store) governed() (governedInner, bool) {
	g, ok := s.inner.(governedInner)
	return g, ok
}

func (s *Store) ApproveBreakGlassSession(ctx context.Context, sessionID, tenantID, approver string, stepUp *domain.StepUpEvidence) (*domain.BreakGlassSession, error) {
	g, ok := s.governed()
	if !ok {
		return nil, domain.ErrStoreUnavailable
	}
	return g.ApproveBreakGlassSession(ctx, sessionID, tenantID, approver, stepUp)
}

func (s *Store) RejectBreakGlassSession(ctx context.Context, sessionID, tenantID, decider string) (*domain.BreakGlassSession, error) {
	g, ok := s.governed()
	if !ok {
		return nil, domain.ErrStoreUnavailable
	}
	return g.RejectBreakGlassSession(ctx, sessionID, tenantID, decider)
}

func (s *Store) FindSoDRule(ctx context.Context, sodRuleID, tenantID string) (*domain.SoDRule, error) {
	g, ok := s.governed()
	if !ok {
		return nil, domain.ErrStoreUnavailable
	}
	return g.FindSoDRule(ctx, sodRuleID, tenantID)
}

func (s *Store) RequestSoDRuleChange(ctx context.Context, sodRuleID, tenantID, change, requester, reason string, expectedVersion int64) (*domain.SoDRule, bool, error) {
	g, ok := s.governed()
	if !ok {
		return nil, false, domain.ErrStoreUnavailable
	}
	return g.RequestSoDRuleChange(ctx, sodRuleID, tenantID, change, requester, reason, expectedVersion)
}

// DecideSoDRuleChange: an approval changes what CheckSoDConflict reads. A
// platform-wide rule binds every tenant, so its decision invalidates them all.
func (s *Store) DecideSoDRuleChange(ctx context.Context, sodRuleID, tenantID string, platformWide bool, decider string, approve bool) (*domain.SoDRule, error) {
	g, ok := s.governed()
	if !ok {
		return nil, domain.ErrStoreUnavailable
	}
	rule, err := g.DecideSoDRuleChange(ctx, sodRuleID, tenantID, platformWide, decider, approve)
	if err == nil {
		s.invalidate(nsSoD, scopeOf(tenantID, platformWide))
	}
	return rule, err
}

func (s *Store) FindABACRule(ctx context.Context, abacRuleID, tenantID string) (*domain.ABACRule, error) {
	g, ok := s.governed()
	if !ok {
		return nil, domain.ErrStoreUnavailable
	}
	return g.FindABACRule(ctx, abacRuleID, tenantID)
}

func (s *Store) RequestABACRuleChange(ctx context.Context, abacRuleID, tenantID, change, requester, reason string, expectedVersion int64) (*domain.ABACRule, bool, error) {
	g, ok := s.governed()
	if !ok {
		return nil, false, domain.ErrStoreUnavailable
	}
	return g.RequestABACRuleChange(ctx, abacRuleID, tenantID, change, requester, reason, expectedVersion)
}

func (s *Store) DecideABACRuleChange(ctx context.Context, abacRuleID, tenantID string, platformWide bool, decider string, approve bool) (*domain.ABACRule, error) {
	g, ok := s.governed()
	if !ok {
		return nil, domain.ErrStoreUnavailable
	}
	rule, err := g.DecideABACRuleChange(ctx, abacRuleID, tenantID, platformWide, decider, approve)
	if err == nil {
		s.invalidate(nsABAC, scopeOf(tenantID, platformWide))
	}
	return rule, err
}

func (s *Store) RequestAuthorityLimitRetire(ctx context.Context, limitID, tenantID, requester, reason string, expectedVersion int64) (*domain.AuthorityLimit, error) {
	g, ok := s.governed()
	if !ok {
		return nil, domain.ErrStoreUnavailable
	}
	return g.RequestAuthorityLimitRetire(ctx, limitID, tenantID, requester, reason, expectedVersion)
}

// DecideAuthorityLimit: limits are read uncached (ListAuthorityLimits passes
// through), so nothing here is invalidated.
func (s *Store) DecideAuthorityLimit(ctx context.Context, limitID, tenantID, decider string, approve bool) (*domain.AuthorityLimit, error) {
	g, ok := s.governed()
	if !ok {
		return nil, domain.ErrStoreUnavailable
	}
	return g.DecideAuthorityLimit(ctx, limitID, tenantID, decider, approve)
}

// scopeOf is the invalidation scope of a rule decision: "" (every tenant)
// for a platform-wide rule.
func scopeOf(tenantID string, platformWide bool) string {
	if platformWide {
		return ""
	}
	return tenantID
}
