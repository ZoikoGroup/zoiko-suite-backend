package handler_test

import (
	"context"
	"time"

	"github.com/google/uuid"

	"zoiko.io/privacy-purpose-registry-svc/internal/domain"
	"zoiko.io/privacy-purpose-registry-svc/internal/middleware"
)

// stubStore is a working in-memory implementation of store.Store — not a
// canned-response mock. It mirrors the semantics PgStore enforces in Postgres
// that the HANDLER's behaviour depends on:
//
//   - row visibility follows the RLS SELECT policy (a tenant sees its own rows
//     and platform rows; the platform plane sees only platform rows), so
//     cross-tenant tests get a genuine 404, not a canned one;
//   - the lifecycle state machine and the one-ACTIVE-version rule;
//   - valid-time (effective_from/effective_to) and knowledge-time resolution.
//
// What it deliberately does NOT claim to prove: the write policies, the unique
// index, the outbox and the audit table. Those live in Postgres and are
// proven against a real database in internal/store/*_integration_test.go.
type stubStore struct {
	purposes         map[string]*domain.Purpose
	purposeVersions  map[string]*domain.PurposeVersion
	activities       map[string]*domain.ProcessingActivity
	activityVersions map[string]*domain.ProcessingActivityVersion
	idempotency      map[string]*domain.IdempotencyRecord

	// idempotencyErr, when set, makes GetIdempotency fail (fail-closed test).
	idempotencyErr error

	// seq mirrors the real PgStore's sequence_no column: a monotonic,
	// collision-proof insertion ordinal used to break effective_from ties.
	nextSeq int
	seq     map[string]int
}

func newStubStore() *stubStore {
	return &stubStore{
		purposes:         map[string]*domain.Purpose{},
		purposeVersions:  map[string]*domain.PurposeVersion{},
		activities:       map[string]*domain.ProcessingActivity{},
		activityVersions: map[string]*domain.ProcessingActivityVersion{},
		idempotency:      map[string]*domain.IdempotencyRecord{},
		seq:              map[string]int{},
	}
}

func (s *stubStore) nextSequence(id string) { s.nextSeq++; s.seq[id] = s.nextSeq }

func strp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func copyStrs(s []string) []string {
	if s == nil {
		return []string{}
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}

// visible mirrors the RLS SELECT policy: platform rows (nil tenant) are
// visible to every scope; a tenant row only to its own tenant.
func visible(ctx context.Context, tenant *string) bool {
	return tenant == nil || *tenant == middleware.TenantFromContext(ctx)
}

// ── purposes ─────────────────────────────────────────────────────────────────

func (s *stubStore) CreatePurpose(ctx context.Context, tenantID string, req domain.CreatePurposeRequest, principalID string) (*domain.Purpose, *domain.PurposeVersion, error) {
	purposeID := uuid.New().String()
	p := &domain.Purpose{PurposeID: purposeID, TenantID: strp(tenantID), CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID}
	s.purposes[purposeID] = p

	effectiveFrom := time.Now().UTC()
	if req.EffectiveFrom != nil {
		effectiveFrom = *req.EffectiveFrom
	}
	v := &domain.PurposeVersion{
		PurposeVersionID: uuid.New().String(), PurposeID: purposeID, TenantID: p.TenantID, Statement: req.Statement,
		CompatibilityClass: req.CompatibilityClass, LawfulBasisRefs: copyStrs(req.LawfulBasisRefs),
		NecessityRationale: req.NecessityRationale, SpecialConditionRefs: copyStrs(req.SpecialConditionRefs),
		VersionStatus: domain.PurposeStatusDraft, EffectiveFrom: effectiveFrom,
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	s.purposeVersions[v.PurposeVersionID] = v
	s.nextSequence(v.PurposeVersionID)
	return p, v, nil
}

func (s *stubStore) CreatePurposeVersion(ctx context.Context, purposeID string, req domain.CreatePurposeVersionRequest, principalID string) (*domain.PurposeVersion, error) {
	parent, ok := s.purposeVersions[req.ParentVersionID]
	if !ok || parent.PurposeID != purposeID || !visible(ctx, parent.TenantID) {
		return nil, domain.ErrPurposeVersionNotFound
	}
	effectiveFrom := time.Now().UTC()
	if req.EffectiveFrom != nil {
		effectiveFrom = *req.EffectiveFrom
	}
	v := &domain.PurposeVersion{
		PurposeVersionID: uuid.New().String(), PurposeID: purposeID, TenantID: parent.TenantID, Statement: req.Statement,
		CompatibilityClass: req.CompatibilityClass, LawfulBasisRefs: copyStrs(req.LawfulBasisRefs),
		NecessityRationale: req.NecessityRationale, SpecialConditionRefs: copyStrs(req.SpecialConditionRefs),
		VersionStatus: domain.PurposeStatusDraft, EffectiveFrom: effectiveFrom,
		SupersedesVersionID: strp(req.ParentVersionID),
		CreatedAt:           time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	s.purposeVersions[v.PurposeVersionID] = v
	s.nextSequence(v.PurposeVersionID)
	return v, nil
}

func (s *stubStore) PublishPurposeVersion(ctx context.Context, purposeID, versionID, principalID, correlationID string) (*domain.PurposeVersion, error) {
	v, ok := s.purposeVersions[versionID]
	if !ok || v.PurposeID != purposeID || !visible(ctx, v.TenantID) {
		return nil, domain.ErrPurposeVersionNotFound
	}
	if v.VersionStatus == domain.PurposeStatusPublished {
		return nil, domain.ErrPurposeAlreadyPublished
	}
	now := time.Now().UTC()
	v.VersionStatus = domain.PurposeStatusPublished
	if v.EffectiveFrom.Before(now) { // GREATEST(effective_from, NOW())
		v.EffectiveFrom = now
	}
	v.PublishedByPrincipalID = &principalID
	v.PublishedAt = &now
	return v, nil
}

func (s *stubStore) FindPurposeVersion(ctx context.Context, purposeID, versionID string) (*domain.PurposeVersion, error) {
	v, ok := s.purposeVersions[versionID]
	if !ok || v.PurposeID != purposeID || !visible(ctx, v.TenantID) {
		return nil, domain.ErrPurposeVersionNotFound
	}
	return v, nil
}

func (s *stubStore) ResolvePurposeAsOf(ctx context.Context, purposeID string, asOf, knownAsOf time.Time) (*domain.PurposeVersion, error) {
	var best *domain.PurposeVersion
	for _, v := range s.purposeVersions {
		if v.PurposeID != purposeID || v.VersionStatus != domain.PurposeStatusPublished || !visible(ctx, v.TenantID) {
			continue
		}
		if v.EffectiveFrom.After(asOf) {
			continue
		}
		known := v.EffectiveFrom
		if v.PublishedAt != nil {
			known = *v.PublishedAt
		}
		if known.After(knownAsOf) {
			continue
		}
		if best == nil || v.EffectiveFrom.After(best.EffectiveFrom) ||
			(v.EffectiveFrom.Equal(best.EffectiveFrom) && s.seq[v.PurposeVersionID] > s.seq[best.PurposeVersionID]) {
			best = v
		}
	}
	if best == nil {
		return nil, domain.ErrPurposeNotFound
	}
	return best, nil
}

func (s *stubStore) ListPurposes(ctx context.Context) ([]domain.PurposeVersion, error) {
	var out []domain.PurposeVersion
	for _, v := range s.purposeVersions {
		if v.VersionStatus == domain.PurposeStatusPublished && visible(ctx, v.TenantID) {
			out = append(out, *v)
		}
	}
	return out, nil
}

func (s *stubStore) IsPurposePublished(ctx context.Context, purposeID string) (bool, error) {
	for _, v := range s.purposeVersions {
		if v.PurposeID == purposeID && v.VersionStatus == domain.PurposeStatusPublished && visible(ctx, v.TenantID) {
			return true, nil
		}
	}
	return false, nil
}

// ── processing activities ────────────────────────────────────────────────────

func newActivityVersion(activityID string, tenant *string, parent string, principalID string,
	role, owner, privacyOwner string, purposeIDs, subjects, categories, sources, recipients, juris, retention, transfer, processors []string,
	notice, dpia string) *domain.ProcessingActivityVersion {
	return &domain.ProcessingActivityVersion{
		ActivityVersionID: uuid.New().String(), ActivityID: activityID, TenantID: tenant,
		PrivacyRole: domain.PrivacyRole(role), Owner: owner, PrivacyOwner: privacyOwner,
		PurposeIDs: copyStrs(purposeIDs), SubjectClasses: copyStrs(subjects), DataCategories: copyStrs(categories),
		Sources: copyStrs(sources), Recipients: copyStrs(recipients), Jurisdictions: copyStrs(juris),
		RetentionRuleRefs: copyStrs(retention), TransferRefs: copyStrs(transfer), ProcessorRefs: copyStrs(processors),
		NoticeConsentDependency: domain.NoticeConsentDependency(notice), DPIATIAStatus: domain.DPIATIAStatus(dpia),
		VersionStatus: domain.ActivityStatusDraft, SupersedesVersionID: strp(parent),
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
}

func (s *stubStore) CreateActivity(ctx context.Context, tenantID string, req domain.CreateActivityRequest, principalID string) (*domain.ProcessingActivity, *domain.ProcessingActivityVersion, error) {
	activityID := uuid.New().String()
	a := &domain.ProcessingActivity{ActivityID: activityID, TenantID: strp(tenantID), CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID}
	s.activities[activityID] = a
	v := newActivityVersion(activityID, a.TenantID, "", principalID, req.PrivacyRole, req.Owner, req.PrivacyOwner,
		req.PurposeIDs, req.SubjectClasses, req.DataCategories, req.Sources, req.Recipients, req.Jurisdictions,
		req.RetentionRuleRefs, req.TransferRefs, req.ProcessorRefs, req.NoticeConsentDependency, req.DPIATIAStatus)
	s.activityVersions[v.ActivityVersionID] = v
	s.nextSequence(v.ActivityVersionID)
	return a, v, nil
}

func (s *stubStore) CreateActivityVersion(ctx context.Context, activityID string, req domain.CreateActivityVersionRequest, principalID string) (*domain.ProcessingActivityVersion, error) {
	parent, ok := s.activityVersions[req.ParentVersionID]
	if !ok || parent.ActivityID != activityID || !visible(ctx, parent.TenantID) {
		return nil, domain.ErrActivityVersionNotFound
	}
	v := newActivityVersion(activityID, parent.TenantID, req.ParentVersionID, principalID, req.PrivacyRole, req.Owner, req.PrivacyOwner,
		req.PurposeIDs, req.SubjectClasses, req.DataCategories, req.Sources, req.Recipients, req.Jurisdictions,
		req.RetentionRuleRefs, req.TransferRefs, req.ProcessorRefs, req.NoticeConsentDependency, req.DPIATIAStatus)
	s.activityVersions[v.ActivityVersionID] = v
	s.nextSequence(v.ActivityVersionID)
	return v, nil
}

func (s *stubStore) FindActivityVersion(ctx context.Context, activityID, versionID string) (*domain.ProcessingActivityVersion, error) {
	v, ok := s.activityVersions[versionID]
	if !ok || v.ActivityID != activityID || !visible(ctx, v.TenantID) {
		return nil, domain.ErrActivityVersionNotFound
	}
	return v, nil
}

func (s *stubStore) ResolveActivityAsOf(ctx context.Context, activityID string, asOf, knownAsOf time.Time) (*domain.ProcessingActivityVersion, error) {
	var best *domain.ProcessingActivityVersion
	for _, v := range s.activityVersions {
		if v.ActivityID != activityID || !visible(ctx, v.TenantID) {
			continue
		}
		if v.VersionStatus != domain.ActivityStatusActive && v.VersionStatus != domain.ActivityStatusSuspended && v.VersionStatus != domain.ActivityStatusRetired {
			continue
		}
		if v.EffectiveFrom == nil || v.EffectiveFrom.After(asOf) {
			continue
		}
		if v.EffectiveTo != nil && !v.EffectiveTo.After(asOf) {
			continue
		}
		known := *v.EffectiveFrom
		if v.ApprovedAt != nil {
			known = *v.ApprovedAt
		}
		if known.After(knownAsOf) {
			continue
		}
		if best == nil || v.EffectiveFrom.After(*best.EffectiveFrom) ||
			(v.EffectiveFrom.Equal(*best.EffectiveFrom) && s.seq[v.ActivityVersionID] > s.seq[best.ActivityVersionID]) {
			best = v
		}
	}
	if best == nil {
		return nil, domain.ErrActivityNotFound
	}
	return best, nil
}

func (s *stubStore) ListActiveActivities(ctx context.Context, role, jurisdiction string) ([]domain.ProcessingActivityVersion, error) {
	var out []domain.ProcessingActivityVersion
	now := time.Now().UTC()
	for _, v := range s.activityVersions {
		if v.VersionStatus != domain.ActivityStatusActive || !visible(ctx, v.TenantID) {
			continue
		}
		if v.EffectiveFrom != nil && v.EffectiveFrom.After(now) {
			continue
		}
		if role != "" && string(v.PrivacyRole) != role {
			continue
		}
		if jurisdiction != "" {
			found := false
			for _, j := range v.Jurisdictions {
				if j == jurisdiction {
					found = true
				}
			}
			if !found {
				continue
			}
		}
		out = append(out, *v)
	}
	return out, nil
}

func (s *stubStore) SetValidationOutcome(ctx context.Context, activityID, versionID string, findings []domain.ValidationFinding, principalID, correlationID string) (*domain.ProcessingActivityVersion, error) {
	v, ok := s.activityVersions[versionID]
	if !ok || v.ActivityID != activityID || v.VersionStatus != domain.ActivityStatusDraft {
		return nil, domain.ErrActivityVersionNotFound
	}
	v.ValidationFindings = findings
	if len(findings) == 0 {
		v.VersionStatus = domain.ActivityStatusValidated
	}
	return v, nil
}

func (s *stubStore) TransitionActivity(ctx context.Context, activityID, versionID string, from, to domain.ActivityVersionStatus, principalID, correlationID string) (*domain.ProcessingActivityVersion, error) {
	v, ok := s.activityVersions[versionID]
	if !ok || v.ActivityID != activityID {
		return nil, domain.ErrActivityVersionNotFound
	}
	if v.VersionStatus != from {
		return nil, domain.ErrInvalidTransition
	}
	now := time.Now().UTC()
	v.VersionStatus = to
	switch to {
	case domain.ActivityStatusApproved:
		v.ApprovedByPrincipalID = &principalID
		v.ApprovedAt = &now
	case domain.ActivityStatusRetired:
		v.EffectiveTo = &now
	}
	return v, nil
}

func (s *stubStore) RejectActivity(ctx context.Context, activityID, versionID, reason, principalID, correlationID string) (*domain.ProcessingActivityVersion, error) {
	v, ok := s.activityVersions[versionID]
	if !ok || v.ActivityID != activityID {
		return nil, domain.ErrActivityVersionNotFound
	}
	if v.VersionStatus != domain.ActivityStatusSubmitted {
		return nil, domain.ErrInvalidTransition
	}
	now := time.Now().UTC()
	v.VersionStatus = domain.ActivityStatusRejected
	v.RejectionReason = &reason
	v.RejectedByPrincipalID = &principalID
	v.RejectedAt = &now
	return v, nil
}

func (s *stubStore) ActivateActivity(ctx context.Context, activityID, versionID, principalID, correlationID string) (*domain.ProcessingActivityVersion, error) {
	v, ok := s.activityVersions[versionID]
	if !ok || v.ActivityID != activityID {
		return nil, domain.ErrActivityVersionNotFound
	}
	if v.VersionStatus != domain.ActivityStatusApproved { // checked BEFORE retiring anything
		return nil, domain.ErrInvalidTransition
	}
	now := time.Now().UTC()
	for _, other := range s.activityVersions {
		if other.ActivityID == activityID && other.ActivityVersionID != versionID &&
			(other.VersionStatus == domain.ActivityStatusActive || other.VersionStatus == domain.ActivityStatusSuspended) {
			other.VersionStatus = domain.ActivityStatusRetired
			t := now
			other.EffectiveTo = &t
		}
	}
	v.VersionStatus = domain.ActivityStatusActive
	v.EffectiveFrom, v.EffectiveTo = &now, nil
	v.ActivatedByPrincipalID, v.ActivatedAt = &principalID, &now
	return v, nil
}

func (s *stubStore) FindLatestActivityVersion(ctx context.Context, activityID string) (*domain.ProcessingActivityVersion, error) {
	var best *domain.ProcessingActivityVersion
	for _, v := range s.activityVersions {
		if v.ActivityID != activityID || !visible(ctx, v.TenantID) {
			continue
		}
		if best == nil || s.seq[v.ActivityVersionID] > s.seq[best.ActivityVersionID] {
			best = v
		}
	}
	if best == nil {
		return nil, domain.ErrActivityNotFound
	}
	return best, nil
}

func (s *stubStore) GetIdempotency(ctx context.Context, tenantID, principalID, key string) (*domain.IdempotencyRecord, error) {
	if s.idempotencyErr != nil {
		return nil, s.idempotencyErr
	}
	return s.idempotency[tenantID+":"+principalID+":"+key], nil
}

func (s *stubStore) SaveIdempotency(ctx context.Context, record domain.IdempotencyRecord) error {
	tenantStr := ""
	if record.TenantID != nil {
		tenantStr = *record.TenantID
	}
	s.idempotency[tenantStr+":"+record.PrincipalID+":"+record.IdempotencyKey] = &record
	return nil
}
