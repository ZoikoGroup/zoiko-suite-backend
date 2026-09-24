package registry_test

// The 000008 half of the in-memory store: onboarding keys, provisioning
// completion and failure, entity verification/activation and merge lineage.
// Faithful to Postgres in the guards the service relies on — state and version
// preconditions, and approval decisions taken with the write.

import (
	"context"
	"errors"
	"time"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/outbox"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

type onboardingKey struct {
	tenantID    string
	fingerprint string
}

type gapState struct {
	onboardingKeys map[string]onboardingKey
	merges         []*domain.EntityMergeRecord
	// failCompletion makes CompleteProvisioning fail, to exercise
	// FAILED_PROVISIONING.
	failCompletion error
}

func (m *memStore) gaps() *gapState {
	o := m.org()
	if o.gaps == nil {
		o.gaps = &gapState{onboardingKeys: map[string]onboardingKey{}}
	}
	return o.gaps
}

func (m *memStore) ResolveOnboardingKey(_ context.Context, key string) (string, string, error) {
	k, ok := m.gaps().onboardingKeys[key]
	if !ok {
		return "", "", nil
	}
	return k.tenantID, k.fingerprint, nil
}

func (m *memStore) CompleteProvisioning(_ context.Context, p registry.ProvisioningCompletion) error {
	if err := m.gaps().failCompletion; err != nil {
		return err
	}
	t, ok := m.tenants[p.TenantID]
	if !ok {
		return registry.ErrNotFound
	}
	if p.FromFailed {
		if t.LifecycleState != domain.TenantLifecycleFailedProvisioning || t.RecordVersion != p.ExpectedVersion {
			return registry.ErrConflict
		}
	}
	if p.Approval != nil {
		if err := m.CreateApprovalRequest(context.Background(), p.Approval); err != nil {
			return err
		}
	}
	if p.FromFailed {
		t.LifecycleState = domain.TenantLifecycleOnboarding
		t.ProvisioningFailureReason, t.ProvisioningFailedAt = nil, nil
		t.RecordVersion++
		from := domain.TenantLifecycleFailedProvisioning
		m.org().lifecycle[p.TenantID] = append(m.org().lifecycle[p.TenantID], &domain.TenantLifecycleEvent{
			TenantID: p.TenantID, FromState: &from, ToState: domain.TenantLifecycleOnboarding,
			CommandName: domain.TenantCommandRetryProvisioning, Reason: p.Reason, ActorPrincipalID: p.ActorID,
			OccurredAt: time.Now().UTC(),
		})
	}
	m.record(p.Event)
	return nil
}

func (m *memStore) MarkProvisioningFailed(_ context.Context, tenantID, reason, _ string) error {
	t, ok := m.tenants[tenantID]
	if !ok || t.LifecycleState != domain.TenantLifecycleOnboarding {
		return registry.ErrConflict
	}
	now := time.Now().UTC()
	t.LifecycleState = domain.TenantLifecycleFailedProvisioning
	t.ProvisioningFailureReason = &reason
	t.ProvisioningFailedAt = &now
	t.RecordVersion++
	return nil
}

// abandonProvisioning mirrors abandonProvisioningTx.
func (m *memStore) abandonProvisioning(tenantID string) {
	for _, b := range m.org().hostBindings {
		if b.TenantID == tenantID {
			b.ActiveFlag = false
		}
	}
	for _, p := range m.residencyPolicies {
		if p.TenantID == tenantID {
			p.ActiveFlag = false
		}
	}
}

func (m *memStore) VerifyLegalEntity(_ context.Context, v registry.EntityVerification, ev *outbox.Record) error {
	e, ok := m.entities[v.LegalEntityID]
	if !ok || e.EntityStatus != domain.EntityStatusDraft || e.RecordVersion != v.ExpectedVersion {
		return registry.ErrConflict
	}
	// le_no_self_verification.
	if v.VerifiedBy == e.CreatedByPrincipalID {
		return errors.New("violates check constraint le_no_self_verification")
	}
	if err := m.decide(v.Decision, domain.ApprovalApproved); err != nil {
		return err
	}
	now := time.Now().UTC()
	by, ref := v.VerifiedBy, v.EvidenceRef
	e.EntityStatus = domain.EntityStatusVerified
	e.VerifiedByPrincipalID, e.VerifiedAt, e.VerificationEvidenceRef = &by, &now, &ref
	e.RecordVersion++
	m.record(ev)
	return nil
}

func (m *memStore) ActivateLegalEntity(_ context.Context, id, _ string, expected int64, ev *outbox.Record) error {
	e, ok := m.entities[id]
	if !ok || e.EntityStatus != domain.EntityStatusVerified || e.RecordVersion != expected {
		return registry.ErrConflict
	}
	e.EntityStatus = domain.EntityStatusActive
	e.RecordVersion++
	m.record(ev)
	return nil
}

func (m *memStore) MergeEntities(_ context.Context, r *domain.EntityMergeRecord, d domain.ApprovalDecision, expected int64, ev *outbox.Record) error {
	dup, ok1 := m.entities[r.DuplicateLegalEntityID]
	surv, ok2 := m.entities[r.SurvivorLegalEntityID]
	if !ok1 || !ok2 {
		return registry.ErrNotFound
	}
	if surv.EntityStatus != domain.EntityStatusActive || surv.MergedIntoLegalEntityID != nil ||
		dup.MergedIntoLegalEntityID != nil || dup.RecordVersion != expected {
		return registry.ErrConflict
	}
	// emr_no_self_approval.
	if r.MergeApprovedByPrincipalID == r.MergedByPrincipalID {
		return errors.New("violates check constraint emr_no_self_approval")
	}
	if err := m.decide(d, domain.ApprovalApproved); err != nil {
		return err
	}
	now := time.Now().UTC()
	r.PriorEntityStatus = dup.EntityStatus
	r.MergedAt = now
	into := surv.LegalEntityID
	dup.EntityStatus = domain.EntityStatusDormant
	dup.MergedIntoLegalEntityID, dup.MergedAt = &into, &now
	dup.RecordVersion++
	cp := *r
	m.gaps().merges = append(m.gaps().merges, &cp)
	m.record(ev)
	return nil
}

func (m *memStore) UnmergeEntity(_ context.Context, id, by, reason string, d domain.ApprovalDecision, expected int64, ev *outbox.Record) error {
	e, ok := m.entities[id]
	if !ok || e.MergedIntoLegalEntityID == nil || e.RecordVersion != expected {
		return registry.ErrConflict
	}
	var rec *domain.EntityMergeRecord
	for _, r := range m.gaps().merges {
		if r.DuplicateLegalEntityID == id && r.UnmergedAt == nil {
			rec = r
		}
	}
	if rec == nil {
		return registry.ErrConflict
	}
	if err := m.decide(d, domain.ApprovalApproved); err != nil {
		return err
	}
	now := time.Now().UTC()
	approver, aid := d.DecidedByPrincipalID, d.ApprovalRequestID
	rec.UnmergedByPrincipalID, rec.UnmergeApprovedByPrincipalID = &by, &approver
	rec.UnmergeApprovalRequestID, rec.UnmergedAt, rec.UnmergeReason = &aid, &now, &reason
	e.EntityStatus = rec.PriorEntityStatus
	e.MergedIntoLegalEntityID, e.MergedAt = nil, nil
	e.RecordVersion++
	m.record(ev)
	return nil
}

func (m *memStore) ListEntityMergeRecords(_ context.Context, id string) ([]*domain.EntityMergeRecord, error) {
	out := []*domain.EntityMergeRecord{}
	for _, r := range m.gaps().merges {
		if r.DuplicateLegalEntityID == id || r.SurvivorLegalEntityID == id {
			out = append(out, r)
		}
	}
	return out, nil
}
