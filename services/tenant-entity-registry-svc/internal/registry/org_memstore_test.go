package registry_test

// The ORG-02/ORG-03 half of the in-memory store used by the service tests.
//
// Kept faithful to the Postgres implementation in the ways the service depends
// on, and deliberately no further:
//
//   - the version guard really rejects a stale expected_version, because every
//     command path branches on that;
//   - the as-of lookup really uses the half-open interval [from, to), because
//     §8 NP6 turns on which version a boundary instant belongs to;
//   - the registry probe really only matches ACTIVE entities, because NP5's
//     wording is "two active entities" and a stub matching dissolved ones would
//     make a passing test out of a wrong rule.
//
// What it does NOT emulate is transactionality: outbox records are collected in
// a slice so a test can assert an event was produced, but nothing here can show
// that the event and the fact commit together. That property is only observable
// against a real database and is asserted in the store integration tests.

import (
	"context"
	"sort"
	"time"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/outbox"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

// orgState is embedded into memStore by the fields below.
type orgState struct {
	profileVersions map[string][]*domain.LegalEntityProfileVersion // by legal_entity_id
	lifecycle       map[string][]*domain.TenantLifecycleEvent      // by tenant_id
	hostBindings    map[string]*domain.TenantHostBinding           // by hostname
	conflicts       map[string]*domain.EntityRegistryConflict      // by conflict_id
	outboxRecords   []outbox.Record
}

func newOrgState() *orgState {
	return &orgState{
		profileVersions: make(map[string][]*domain.LegalEntityProfileVersion),
		lifecycle:       make(map[string][]*domain.TenantLifecycleEvent),
		hostBindings:    make(map[string]*domain.TenantHostBinding),
		conflicts:       make(map[string]*domain.EntityRegistryConflict),
	}
}

func (m *memStore) org() *orgState {
	if m.orgs == nil {
		m.orgs = newOrgState()
	}
	return m.orgs
}

func (m *memStore) record(ev *outbox.Record) {
	if ev != nil {
		m.org().outboxRecords = append(m.org().outboxRecords, *ev)
	}
}

// publishedEvents returns the event types enqueued so far, for assertions.
func (m *memStore) publishedEvents() []string {
	out := make([]string, 0, len(m.org().outboxRecords))
	for _, r := range m.org().outboxRecords {
		out = append(out, r.EventType)
	}
	return out
}

// ── ORG-02: named commands ──────────────────────────────────────────────────

func (m *memStore) ExecuteTenantCommand(_ context.Context, p registry.TenantCommandParams, ev *outbox.Record) (*registry.TenantCommandResult, error) {
	t, ok := m.tenants[p.TenantID]
	if !ok {
		return nil, registry.ErrConflict
	}
	if t.RecordVersion != p.ExpectedVersion {
		return nil, registry.ErrConflict
	}
	allowed := false
	for _, st := range p.AllowedFrom {
		if t.LifecycleState == st {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, registry.ErrConflict
	}

	from := t.LifecycleState
	t.LifecycleState = p.TargetState
	t.RecordVersion++
	switch p.TargetState {
	case domain.TenantLifecycleActive:
		t.Status = domain.TenantStatusActive
	case domain.TenantLifecycleSuspended:
		t.Status = domain.TenantStatusSuspended
	case domain.TenantLifecycleOffboarding, domain.TenantLifecycleTerminated:
		t.Status = domain.TenantStatusArchived
	}

	var approver *string
	if p.ApprovedBy != "" {
		approver = &p.ApprovedBy
	}
	m.org().lifecycle[p.TenantID] = append(m.org().lifecycle[p.TenantID], &domain.TenantLifecycleEvent{
		LifecycleEventID:      "lce-" + p.TenantID,
		TenantID:              p.TenantID,
		FromState:             &from,
		ToState:               p.TargetState,
		CommandName:           p.Command,
		Reason:                p.Reason,
		ActorPrincipalID:      p.ActorID,
		ApprovedByPrincipalID: approver,
		OccurredAt:            time.Now().UTC(),
	})
	m.record(ev)

	return &registry.TenantCommandResult{
		FromState:  from,
		ToState:    p.TargetState,
		NewVersion: t.RecordVersion,
		Status:     t.Status,
	}, nil
}

func (m *memStore) ChangeDefaultLocale(_ context.Context, tenantID, locale, timezone, reason, actorID, _ string, expectedVersion int64, ev *outbox.Record) (*domain.Tenant, error) {
	t, ok := m.tenants[tenantID]
	if !ok || t.RecordVersion != expectedVersion {
		return nil, registry.ErrConflict
	}
	if locale != "" {
		t.PrimaryLocale = locale
	}
	if timezone != "" {
		t.PrimaryTimezone = timezone
	}
	t.RecordVersion++
	t.UpdatedByPrincipalID = actorID
	m.org().lifecycle[tenantID] = append(m.org().lifecycle[tenantID], &domain.TenantLifecycleEvent{
		LifecycleEventID: "lce-locale-" + tenantID,
		TenantID:         tenantID,
		ToState:          t.LifecycleState,
		CommandName:      domain.TenantCommandChangeDefaultLocale,
		Reason:           reason,
		ActorPrincipalID: actorID,
		OccurredAt:       time.Now().UTC(),
	})
	m.record(ev)
	return t, nil
}

func (m *memStore) ListTenantLifecycleHistory(_ context.Context, tenantID string) ([]*domain.TenantLifecycleEvent, error) {
	out := m.org().lifecycle[tenantID]
	if out == nil {
		return []*domain.TenantLifecycleEvent{}, nil
	}
	return out, nil
}

func (m *memStore) GetTenantDefaults(_ context.Context, tenantID string) (*domain.TenantDefaults, error) {
	t, ok := m.tenants[tenantID]
	if !ok {
		return nil, nil
	}
	return &domain.TenantDefaults{
		TenantID:                     t.TenantID,
		DefaultCurrencyCode:          t.DefaultCurrencyCode,
		PrimaryTimezone:              t.PrimaryTimezone,
		PrimaryLocale:                t.PrimaryLocale,
		DefaultDataResidencyPolicyID: t.DefaultDataResidencyPolicyID,
		RecordVersion:                t.RecordVersion,
	}, nil
}

// ── ORG-02: host bindings ───────────────────────────────────────────────────

func (m *memStore) BindTenantHost(_ context.Context, b *domain.TenantHostBinding) error {
	if _, exists := m.org().hostBindings[b.Hostname]; exists {
		return registry.ErrConflict
	}
	m.org().hostBindings[b.Hostname] = b
	return nil
}

func (m *memStore) ResolveTenantByHost(_ context.Context, hostname string) (*domain.ResolvedTenantByHost, error) {
	b, ok := m.org().hostBindings[hostname]
	if !ok || !b.ActiveFlag {
		return nil, nil
	}
	t, ok := m.tenants[b.TenantID]
	if !ok {
		return nil, nil
	}
	return &domain.ResolvedTenantByHost{
		Hostname:       b.Hostname,
		TenantID:       b.TenantID,
		TenantCode:     t.TenantCode,
		Status:         t.Status,
		LifecycleState: t.LifecycleState,
		IsPrimary:      b.IsPrimary,
	}, nil
}

func (m *memStore) ListTenantHostBindings(_ context.Context, tenantID string) ([]*domain.TenantHostBinding, error) {
	out := []*domain.TenantHostBinding{}
	for _, b := range m.org().hostBindings {
		if b.TenantID == tenantID {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hostname < out[j].Hostname })
	return out, nil
}

// ── ORG-03: profile versions ────────────────────────────────────────────────

func (m *memStore) CreateInitialProfileVersion(_ context.Context, v *domain.LegalEntityProfileVersion) error {
	v.VersionNumber = 1
	v.RecordedAt = time.Now().UTC()
	m.org().profileVersions[v.LegalEntityID] = []*domain.LegalEntityProfileVersion{v}
	return nil
}

func (m *memStore) AmendLegalProfile(_ context.Context, legalEntityID string, next *domain.LegalEntityProfileVersion, expectedVersion int64, ev *outbox.Record) (*domain.LegalEntityProfileVersion, error) {
	e, ok := m.entities[legalEntityID]
	if !ok {
		return nil, registry.ErrNotFound
	}
	versions := m.org().profileVersions[legalEntityID]

	maxNum := 0
	for _, v := range versions {
		if v.VersionNumber > maxNum {
			maxNum = v.VersionNumber
		}
	}
	next.VersionNumber = maxNum + 1
	next.RecordedAt = time.Now().UTC()

	// Close the version in force at next.EffectiveFrom, mirroring the SQL.
	if prev := inForceAt(versions, next.EffectiveFrom); prev != nil {
		if prev.EffectiveTo != nil {
			next.EffectiveTo = prev.EffectiveTo
		}
		eff := next.EffectiveFrom
		now := time.Now().UTC()
		prev.EffectiveTo = &eff
		prev.SupersededAt = &now
	}

	m.org().profileVersions[legalEntityID] = append(versions, next)

	now := time.Now().UTC()
	isInForce := !next.EffectiveFrom.After(now) && (next.EffectiveTo == nil || next.EffectiveTo.After(now))
	if isInForce {
		if e.RecordVersion != expectedVersion {
			return nil, registry.ErrConflict
		}
		e.LegalName = next.LegalName
		e.TradingName = next.TradingName
		e.RegistrationNumber = next.RegistrationNumber
		e.RecordVersion++
	}
	m.record(ev)
	return next, nil
}

// inForceAt returns the version whose half-open interval [from, to) contains t.
func inForceAt(versions []*domain.LegalEntityProfileVersion, t time.Time) *domain.LegalEntityProfileVersion {
	var best *domain.LegalEntityProfileVersion
	for _, v := range versions {
		if v.EffectiveFrom.After(t) {
			continue
		}
		if v.EffectiveTo != nil && !v.EffectiveTo.After(t) {
			continue
		}
		if best == nil ||
			v.EffectiveFrom.After(best.EffectiveFrom) ||
			(v.EffectiveFrom.Equal(best.EffectiveFrom) && v.VersionNumber > best.VersionNumber) {
			best = v
		}
	}
	return best
}

func (m *memStore) ListEntityProfileVersions(_ context.Context, legalEntityID string) ([]*domain.LegalEntityProfileVersion, error) {
	src := m.org().profileVersions[legalEntityID]
	out := make([]*domain.LegalEntityProfileVersion, len(src))
	copy(out, src)
	sort.Slice(out, func(i, j int) bool { return out[i].VersionNumber > out[j].VersionNumber })
	return out, nil
}

func (m *memStore) GetEntityProfileAsOf(_ context.Context, legalEntityID string, asOf time.Time) (*domain.EntityAsOf, error) {
	e, ok := m.entities[legalEntityID]
	if !ok {
		return nil, nil
	}
	return &domain.EntityAsOf{
		LegalEntityID: e.LegalEntityID,
		TenantID:      e.TenantID,
		EntityCode:    e.EntityCode,
		EntityType:    e.EntityType,
		EntityStatus:  e.EntityStatus,
		AsOf:          asOf,
		Profile:       inForceAt(m.org().profileVersions[legalEntityID], asOf),
	}, nil
}

func (m *memStore) FindEntitiesByRegistryNumber(_ context.Context, registrationNumber, jurisdictionID string) ([]*domain.LegalEntity, error) {
	out := []*domain.LegalEntity{}
	for _, e := range m.entities {
		if e.RegistrationNumber == nil || *e.RegistrationNumber != registrationNumber {
			continue
		}
		if jurisdictionID != "" && e.PrimaryJurisdictionID != jurisdictionID {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LegalEntityID < out[j].LegalEntityID })
	return out, nil
}

// ── ORG-03: registry conflicts ──────────────────────────────────────────────

func (m *memStore) FindActiveEntityByRegistry(_ context.Context, registrationNumber, jurisdictionID string) (*domain.LegalEntity, error) {
	if registrationNumber == "" || jurisdictionID == "" {
		return nil, nil
	}
	var found *domain.LegalEntity
	for _, e := range m.entities {
		if e.RegistrationNumber == nil || *e.RegistrationNumber != registrationNumber {
			continue
		}
		if e.PrimaryJurisdictionID != jurisdictionID || e.EntityStatus != domain.EntityStatusActive {
			continue
		}
		// Lowest id wins, so the result is stable across map iteration order.
		if found == nil || e.LegalEntityID < found.LegalEntityID {
			found = e
		}
	}
	return found, nil
}

func (m *memStore) RecordRegistryConflict(_ context.Context, c *domain.EntityRegistryConflict) error {
	m.org().conflicts[c.ConflictID] = c
	return nil
}

func (m *memStore) ListRegistryConflicts(_ context.Context, openOnly bool) ([]*domain.EntityRegistryConflict, error) {
	out := []*domain.EntityRegistryConflict{}
	for _, c := range m.org().conflicts {
		if openOnly && c.Status != domain.RegistryConflictOpen {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ConflictID < out[j].ConflictID })
	return out, nil
}

func (m *memStore) ResolveRegistryConflict(_ context.Context, conflictID string, status domain.RegistryConflictStatus, note, actorID string) error {
	c, ok := m.org().conflicts[conflictID]
	if !ok || c.Status != domain.RegistryConflictOpen {
		return registry.ErrConflict
	}
	now := time.Now().UTC()
	c.Status = status
	c.ResolutionNote = &note
	c.ResolvedByPrincipalID = &actorID
	c.ResolvedAt = &now
	return nil
}
