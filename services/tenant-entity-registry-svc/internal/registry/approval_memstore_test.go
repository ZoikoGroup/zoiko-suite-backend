package registry_test

// The approval half of the in-memory store.
//
// Faithful to Postgres in the properties the service's maker-checker depends
// on: one PENDING request per subject, decisions guarded on PENDING (and on
// the TTL for APPROVED), and ar_no_self_decision — a decider equal to the
// requester is refused here exactly as the CHECK refuses it, so a service bug
// that let self-approval through would fail these tests rather than pass them.

import (
	"context"
	"errors"
	"sort"
	"time"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

// errSelfDecisionCheck stands in for the ar_no_self_decision CHECK violation.
var errSelfDecisionCheck = errors.New("violates check constraint ar_no_self_decision")

func (m *memStore) approvals() map[string]*domain.ApprovalRequest {
	o := m.org()
	if o.approvals == nil {
		o.approvals = make(map[string]*domain.ApprovalRequest)
	}
	return o.approvals
}

func (m *memStore) CreateApprovalRequest(_ context.Context, a *domain.ApprovalRequest) error {
	now := time.Now().UTC()
	for _, x := range m.approvals() {
		if x.SubjectType != a.SubjectType || x.SubjectID != a.SubjectID || x.Status != domain.ApprovalPending {
			continue
		}
		if !x.ExpiresAt.After(now) {
			x.Status = domain.ApprovalExpired
			x.DecidedAt = &now
			continue
		}
		return registry.ErrApprovalPending
	}
	cp := *a
	m.approvals()[a.ApprovalRequestID] = &cp
	return nil
}

func (m *memStore) GetApprovalRequest(_ context.Context, id string) (*domain.ApprovalRequest, error) {
	a, ok := m.approvals()[id]
	if !ok {
		return nil, nil
	}
	cp := *a
	return &cp, nil
}

func (m *memStore) ListApprovalRequests(ctx context.Context, pendingOnly bool) ([]*domain.ApprovalRequest, error) {
	tid := domain.TenantFromContext(ctx)
	now := time.Now().UTC()
	out := []*domain.ApprovalRequest{}
	for _, a := range m.approvals() {
		if a.TenantID != tid {
			continue
		}
		if pendingOnly && (a.Status != domain.ApprovalPending || !a.ExpiresAt.After(now)) {
			continue
		}
		cp := *a
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RequestedAt.After(out[j].RequestedAt) })
	return out, nil
}

func (m *memStore) LatestApprovalForSubject(_ context.Context, st domain.ApprovalSubjectType, subjectID string) (*domain.ApprovalRequest, error) {
	var best *domain.ApprovalRequest
	for _, a := range m.approvals() {
		if a.SubjectType != st || a.SubjectID != subjectID {
			continue
		}
		if best == nil || a.RequestedAt.After(best.RequestedAt) {
			best = a
		}
	}
	if best == nil {
		return nil, nil
	}
	cp := *best
	return &cp, nil
}

func (m *memStore) DecideApprovalRequest(_ context.Context, d domain.ApprovalDecision, status domain.ApprovalStatus) error {
	return m.decide(d, status)
}

// decide mirrors decideApprovalTx.
func (m *memStore) decide(d domain.ApprovalDecision, status domain.ApprovalStatus) error {
	a, ok := m.approvals()[d.ApprovalRequestID]
	now := time.Now().UTC()
	if !ok || a.Status != domain.ApprovalPending ||
		(status == domain.ApprovalApproved && !a.ExpiresAt.After(now)) {
		return registry.ErrApprovalNotPending
	}
	if d.DecidedByPrincipalID != "" && d.DecidedByPrincipalID == a.RequestedByPrincipalID {
		return errSelfDecisionCheck
	}
	a.Status = status
	a.DecidedAt = &now
	if d.DecidedByPrincipalID != "" {
		by := d.DecidedByPrincipalID
		a.DecidedByPrincipalID = &by
	}
	if d.Note != "" {
		note := d.Note
		a.DecisionNote = &note
	}
	return nil
}

func (m *memStore) GetRegistryConflict(_ context.Context, id string) (*domain.EntityRegistryConflict, error) {
	c, ok := m.org().conflicts[id]
	if !ok {
		return nil, nil
	}
	return c, nil
}

func (m *memStore) ResolveRegistryConflictApproved(_ context.Context, id string, status domain.RegistryConflictStatus, note, resolvedBy string, d domain.ApprovalDecision) error {
	c, ok := m.org().conflicts[id]
	if !ok || c.Status != domain.RegistryConflictOpen {
		return registry.ErrConflict
	}
	// erc_no_self_approval.
	if d.DecidedByPrincipalID == resolvedBy || d.DecidedByPrincipalID == c.DetectedByPrincipalID {
		return errors.New("violates check constraint erc_no_self_approval")
	}
	if err := m.decide(d, domain.ApprovalApproved); err != nil {
		return err
	}
	now := time.Now().UTC()
	c.Status = status
	c.ResolutionNote = &note
	c.ResolvedByPrincipalID = &resolvedBy
	c.ResolvedAt = &now
	approver := d.DecidedByPrincipalID
	c.ApprovedByPrincipalID = &approver
	c.ApprovalRequestID = &d.ApprovalRequestID
	return nil
}
