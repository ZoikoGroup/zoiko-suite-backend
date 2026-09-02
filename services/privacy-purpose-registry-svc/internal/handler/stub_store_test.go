package handler_test

import (
	"context"
	"time"

	"github.com/google/uuid"

	"zoiko.io/privacy-purpose-registry-svc/internal/domain"
)

// stubStore is a real, working in-memory implementation of store.Store —
// not a canned-response mock. It replicates the actual state-machine and
// tenant-scoping semantics PgStore enforces in Postgres, so handler tests
// exercise real emergent behavior (illegal transitions genuinely rejected,
// tenant isolation genuinely enforced) rather than pre-scripted outcomes.
type stubStore struct {
	purposes         map[string]*domain.Purpose
	purposeVersions  map[string]*domain.PurposeVersion
	activities       map[string]*domain.ProcessingActivity
	activityVersions map[string]*domain.ProcessingActivityVersion

	// seq mirrors the real PgStore's sequence_no column: a monotonic,
	// collision-proof insertion ordinal. Two versions created moments
	// apart can legitimately share an identical wall-clock EffectiveFrom
	// under Windows' coarse time.Now() resolution — this is what
	// ResolvePurposeAsOf/ResolveActivityAsOf break ties with instead,
	// same as the real Postgres ORDER BY ... sequence_no DESC.
	nextSeq int
	seq     map[string]int
}

func newStubStore() *stubStore {
	return &stubStore{
		purposes:         map[string]*domain.Purpose{},
		purposeVersions:  map[string]*domain.PurposeVersion{},
		activities:       map[string]*domain.ProcessingActivity{},
		activityVersions: map[string]*domain.ProcessingActivityVersion{},
		seq:              map[string]int{},
	}
}

func (s *stubStore) nextSequence(versionID string) int {
	s.nextSeq++
	s.seq[versionID] = s.nextSeq
	return s.nextSeq
}

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
		PurposeVersionID: uuid.New().String(), PurposeID: purposeID, Statement: req.Statement,
		CompatibilityClass: req.CompatibilityClass, LawfulBasisRefs: copyStrs(req.LawfulBasisRefs),
		VersionStatus: domain.PurposeStatusDraft, EffectiveFrom: effectiveFrom,
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	s.purposeVersions[v.PurposeVersionID] = v
	s.nextSequence(v.PurposeVersionID)
	return p, v, nil
}

func (s *stubStore) CreatePurposeVersion(ctx context.Context, purposeID string, req domain.CreatePurposeVersionRequest, principalID string) (*domain.PurposeVersion, error) {
	parent, ok := s.purposeVersions[req.ParentVersionID]
	if !ok || parent.PurposeID != purposeID {
		return nil, domain.ErrPurposeVersionNotFound
	}
	effectiveFrom := time.Now().UTC()
	if req.EffectiveFrom != nil {
		effectiveFrom = *req.EffectiveFrom
	}
	v := &domain.PurposeVersion{
		PurposeVersionID: uuid.New().String(), PurposeID: purposeID, Statement: req.Statement,
		CompatibilityClass: req.CompatibilityClass, LawfulBasisRefs: copyStrs(req.LawfulBasisRefs),
		VersionStatus: domain.PurposeStatusDraft, EffectiveFrom: effectiveFrom,
		SupersedesVersionID: strp(req.ParentVersionID),
		CreatedAt:           time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	s.purposeVersions[v.PurposeVersionID] = v
	s.nextSequence(v.PurposeVersionID)
	return v, nil
}

func (s *stubStore) PublishPurposeVersion(ctx context.Context, purposeID, versionID string) (*domain.PurposeVersion, error) {
	v, ok := s.purposeVersions[versionID]
	if !ok || v.PurposeID != purposeID {
		return nil, domain.ErrPurposeVersionNotFound
	}
	if v.VersionStatus == domain.PurposeStatusPublished {
		return nil, domain.ErrPurposeAlreadyPublished
	}
	v.VersionStatus = domain.PurposeStatusPublished
	return v, nil
}

func (s *stubStore) FindPurposeVersion(ctx context.Context, purposeID, versionID string) (*domain.PurposeVersion, error) {
	v, ok := s.purposeVersions[versionID]
	if !ok || v.PurposeID != purposeID {
		return nil, domain.ErrPurposeVersionNotFound
	}
	return v, nil
}

func (s *stubStore) ResolvePurposeAsOf(ctx context.Context, purposeID string, asOf time.Time) (*domain.PurposeVersion, error) {
	var best *domain.PurposeVersion
	for _, v := range s.purposeVersions {
		if v.PurposeID != purposeID || v.VersionStatus != domain.PurposeStatusPublished {
			continue
		}
		if v.EffectiveFrom.After(asOf) {
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
		if v.VersionStatus == domain.PurposeStatusPublished {
			out = append(out, *v)
		}
	}
	return out, nil
}

func (s *stubStore) IsPurposePublished(ctx context.Context, purposeID string) (bool, error) {
	for _, v := range s.purposeVersions {
		if v.PurposeID == purposeID && v.VersionStatus == domain.PurposeStatusPublished {
			return true, nil
		}
	}
	return false, nil
}

// ── processing activities ────────────────────────────────────────────────────

func (s *stubStore) CreateActivity(ctx context.Context, tenantID string, req domain.CreateActivityRequest, principalID string) (*domain.ProcessingActivity, *domain.ProcessingActivityVersion, error) {
	activityID := uuid.New().String()
	a := &domain.ProcessingActivity{ActivityID: activityID, TenantID: strp(tenantID), CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID}
	s.activities[activityID] = a

	v := &domain.ProcessingActivityVersion{
		ActivityVersionID: uuid.New().String(), ActivityID: activityID,
		PrivacyRole: domain.PrivacyRole(req.PrivacyRole), Owner: req.Owner,
		PurposeIDs: copyStrs(req.PurposeIDs), SubjectClasses: copyStrs(req.SubjectClasses),
		DataCategories: copyStrs(req.DataCategories), Sources: copyStrs(req.Sources),
		Recipients: copyStrs(req.Recipients), Jurisdictions: copyStrs(req.Jurisdictions),
		RetentionRuleRefs: copyStrs(req.RetentionRuleRefs), TransferRefs: copyStrs(req.TransferRefs),
		VersionStatus: domain.ActivityStatusDraft,
		CreatedAt:     time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	s.activityVersions[v.ActivityVersionID] = v
	s.nextSequence(v.ActivityVersionID)
	return a, v, nil
}

func (s *stubStore) CreateActivityVersion(ctx context.Context, activityID string, req domain.CreateActivityVersionRequest, principalID string) (*domain.ProcessingActivityVersion, error) {
	parent, ok := s.activityVersions[req.ParentVersionID]
	if !ok || parent.ActivityID != activityID {
		return nil, domain.ErrActivityVersionNotFound
	}
	v := &domain.ProcessingActivityVersion{
		ActivityVersionID: uuid.New().String(), ActivityID: activityID,
		PrivacyRole: domain.PrivacyRole(req.PrivacyRole), Owner: req.Owner,
		PurposeIDs: copyStrs(req.PurposeIDs), SubjectClasses: copyStrs(req.SubjectClasses),
		DataCategories: copyStrs(req.DataCategories), Sources: copyStrs(req.Sources),
		Recipients: copyStrs(req.Recipients), Jurisdictions: copyStrs(req.Jurisdictions),
		RetentionRuleRefs: copyStrs(req.RetentionRuleRefs), TransferRefs: copyStrs(req.TransferRefs),
		VersionStatus:       domain.ActivityStatusDraft,
		SupersedesVersionID: strp(req.ParentVersionID),
		CreatedAt:           time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	s.activityVersions[v.ActivityVersionID] = v
	s.nextSequence(v.ActivityVersionID)
	return v, nil
}

func (s *stubStore) FindActivityVersion(ctx context.Context, activityID, versionID string) (*domain.ProcessingActivityVersion, error) {
	v, ok := s.activityVersions[versionID]
	if !ok || v.ActivityID != activityID {
		return nil, domain.ErrActivityVersionNotFound
	}
	return v, nil
}

func (s *stubStore) ResolveActivityAsOf(ctx context.Context, activityID string, asOf time.Time) (*domain.ProcessingActivityVersion, error) {
	var best *domain.ProcessingActivityVersion
	for _, v := range s.activityVersions {
		if v.ActivityID != activityID {
			continue
		}
		if v.VersionStatus != domain.ActivityStatusActive && v.VersionStatus != domain.ActivityStatusSuspended && v.VersionStatus != domain.ActivityStatusRetired {
			continue
		}
		if v.EffectiveFrom == nil || v.EffectiveFrom.After(asOf) {
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
	for _, v := range s.activityVersions {
		if v.VersionStatus != domain.ActivityStatusActive {
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
					break
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

func (s *stubStore) SetValidationOutcome(ctx context.Context, activityID, versionID string, findings []domain.ValidationFinding) (*domain.ProcessingActivityVersion, error) {
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

func (s *stubStore) TransitionActivity(ctx context.Context, activityID, versionID string, from, to domain.ActivityVersionStatus) (*domain.ProcessingActivityVersion, error) {
	v, ok := s.activityVersions[versionID]
	if !ok || v.ActivityID != activityID {
		return nil, domain.ErrActivityVersionNotFound
	}
	if v.VersionStatus != from {
		return nil, domain.ErrInvalidTransition
	}
	v.VersionStatus = to
	return v, nil
}

func (s *stubStore) RejectActivity(ctx context.Context, activityID, versionID, reason string) (*domain.ProcessingActivityVersion, error) {
	v, ok := s.activityVersions[versionID]
	if !ok || v.ActivityID != activityID {
		return nil, domain.ErrActivityVersionNotFound
	}
	if v.VersionStatus != domain.ActivityStatusSubmitted {
		return nil, domain.ErrInvalidTransition
	}
	v.VersionStatus = domain.ActivityStatusRejected
	v.RejectionReason = &reason
	return v, nil
}

func (s *stubStore) ActivateActivity(ctx context.Context, activityID, versionID string, effectiveFrom time.Time) (*domain.ProcessingActivityVersion, error) {
	v, ok := s.activityVersions[versionID]
	if !ok || v.ActivityID != activityID {
		return nil, domain.ErrActivityVersionNotFound
	}
	if v.VersionStatus != domain.ActivityStatusApproved {
		return nil, domain.ErrInvalidTransition
	}
	v.VersionStatus = domain.ActivityStatusActive
	v.EffectiveFrom = &effectiveFrom
	return v, nil
}
