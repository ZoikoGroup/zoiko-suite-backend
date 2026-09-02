package handler_test

import (
	"context"
	"time"

	"github.com/google/uuid"

	"zoiko.io/privacy-consent-svc/internal/domain"
)

// stubStore is a real, working in-memory implementation of store.Store —
// replicates the actual state-machine and derivation semantics PgStore
// enforces in Postgres.
type stubStore struct {
	notices              map[string]*domain.Notice
	noticeVersions       map[string]*domain.NoticeVersion
	presentationReceipts []domain.PresentationReceipt
	consentReceipts      map[string]*domain.ConsentReceipt
	withdrawalReceipts   map[string]*domain.WithdrawalReceipt // keyed by consent_receipt_id
	preferences          []domain.PreferenceAssertion

	// seq mirrors the real PgStore's sequence_no column — see
	// privacy-purpose-registry-svc's identical field for the full
	// rationale: two notice versions can share an identical wall-clock
	// EffectiveFrom under Windows' coarse time.Now() resolution, and
	// ResolveNoticeAsOf needs a collision-proof tiebreaker.
	nextSeq int
	seq     map[string]int
}

func newStubStore() *stubStore {
	return &stubStore{
		notices:            map[string]*domain.Notice{},
		noticeVersions:     map[string]*domain.NoticeVersion{},
		consentReceipts:    map[string]*domain.ConsentReceipt{},
		withdrawalReceipts: map[string]*domain.WithdrawalReceipt{},
		seq:                map[string]int{},
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

// ── notices ──────────────────────────────────────────────────────────────────

func (s *stubStore) CreateNotice(ctx context.Context, tenantID string, req domain.CreateNoticeRequest, principalID string) (*domain.Notice, *domain.NoticeVersion, error) {
	noticeID := uuid.New().String()
	n := &domain.Notice{NoticeID: noticeID, TenantID: strp(tenantID), CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID}
	s.notices[noticeID] = n

	v := &domain.NoticeVersion{
		NoticeVersionID: uuid.New().String(), NoticeID: noticeID, Locale: req.Locale, Audience: req.Audience,
		ContentHash: req.ContentHash, VersionStatus: domain.NoticeStatusDraft,
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	s.noticeVersions[v.NoticeVersionID] = v
	s.nextSequence(v.NoticeVersionID)
	return n, v, nil
}

func (s *stubStore) CreateNoticeVersion(ctx context.Context, noticeID string, req domain.CreateNoticeVersionRequest, principalID string) (*domain.NoticeVersion, error) {
	parent, ok := s.noticeVersions[req.ParentVersionID]
	if !ok || parent.NoticeID != noticeID {
		return nil, domain.ErrNoticeVersionNotFound
	}
	v := &domain.NoticeVersion{
		NoticeVersionID: uuid.New().String(), NoticeID: noticeID, Locale: req.Locale, Audience: req.Audience,
		ContentHash: req.ContentHash, VersionStatus: domain.NoticeStatusDraft,
		SupersedesVersionID: strp(req.ParentVersionID),
		CreatedAt:           time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	s.noticeVersions[v.NoticeVersionID] = v
	s.nextSequence(v.NoticeVersionID)
	return v, nil
}

func (s *stubStore) FindNoticeVersion(ctx context.Context, noticeID, versionID string) (*domain.NoticeVersion, error) {
	v, ok := s.noticeVersions[versionID]
	if !ok || v.NoticeID != noticeID {
		return nil, domain.ErrNoticeVersionNotFound
	}
	return v, nil
}

func (s *stubStore) ApproveNoticeVersion(ctx context.Context, noticeID, versionID, principalID string) (*domain.NoticeVersion, error) {
	v, ok := s.noticeVersions[versionID]
	if !ok || v.NoticeID != noticeID || v.VersionStatus != domain.NoticeStatusDraft {
		return nil, domain.ErrInvalidNoticeTransition
	}
	v.VersionStatus = domain.NoticeStatusApproved
	v.ApprovedByPrincipalID = strp(principalID)
	return v, nil
}

func (s *stubStore) PublishNoticeVersion(ctx context.Context, noticeID, versionID string) (*domain.NoticeVersion, error) {
	v, ok := s.noticeVersions[versionID]
	if !ok || v.NoticeID != noticeID || v.VersionStatus != domain.NoticeStatusApproved {
		return nil, domain.ErrInvalidNoticeTransition
	}
	for _, other := range s.noticeVersions {
		if other.NoticeID == noticeID && other.VersionStatus == domain.NoticeStatusPublished {
			other.VersionStatus = domain.NoticeStatusSuperseded
		}
	}
	now := time.Now().UTC()
	v.VersionStatus = domain.NoticeStatusPublished
	v.EffectiveFrom = &now
	return v, nil
}

func (s *stubStore) WithdrawNoticeVersion(ctx context.Context, noticeID, versionID string) (*domain.NoticeVersion, error) {
	v, ok := s.noticeVersions[versionID]
	if !ok || v.NoticeID != noticeID || v.VersionStatus != domain.NoticeStatusPublished {
		return nil, domain.ErrInvalidNoticeTransition
	}
	v.VersionStatus = domain.NoticeStatusWithdrawn
	return v, nil
}

func (s *stubStore) ResolveNoticeAsOf(ctx context.Context, noticeID string, asOf time.Time) (*domain.NoticeVersion, error) {
	var best *domain.NoticeVersion
	for _, v := range s.noticeVersions {
		if v.NoticeID != noticeID {
			continue
		}
		if v.VersionStatus != domain.NoticeStatusPublished && v.VersionStatus != domain.NoticeStatusSuperseded && v.VersionStatus != domain.NoticeStatusWithdrawn {
			continue
		}
		if v.EffectiveFrom == nil || v.EffectiveFrom.After(asOf) {
			continue
		}
		if best == nil || v.EffectiveFrom.After(*best.EffectiveFrom) ||
			(v.EffectiveFrom.Equal(*best.EffectiveFrom) && s.seq[v.NoticeVersionID] > s.seq[best.NoticeVersionID]) {
			best = v
		}
	}
	if best == nil {
		return nil, domain.ErrNoticeNotFound
	}
	return best, nil
}

// ── presentation receipts ────────────────────────────────────────────────────

func (s *stubStore) RecordPresentation(ctx context.Context, tenantID, noticeID, versionID string, req domain.RecordPresentationRequest) (*domain.PresentationReceipt, error) {
	v, ok := s.noticeVersions[versionID]
	if !ok || v.NoticeID != noticeID {
		return nil, domain.ErrNoticeVersionNotFound
	}
	r := domain.PresentationReceipt{
		PresentationReceiptID: uuid.New().String(), TenantID: strp(tenantID), NoticeVersionID: versionID,
		SubjectRef: req.SubjectRef, Channel: req.Channel, Locale: req.Locale, CreatedAt: time.Now().UTC(),
	}
	s.presentationReceipts = append(s.presentationReceipts, r)
	return &r, nil
}

// ── consent ──────────────────────────────────────────────────────────────────

func (s *stubStore) RecordConsent(ctx context.Context, tenantID string, req domain.RecordConsentRequest, principalID, correlationID string) (*domain.ConsentReceipt, error) {
	r := &domain.ConsentReceipt{
		ConsentReceiptID: uuid.New().String(), TenantID: strp(tenantID), SubjectRef: req.SubjectRef,
		PurposeID: req.PurposeID, NoticeVersionID: strp(req.NoticeVersionID), Action: domain.ConsentAction(req.Action),
		CaptureChannel: req.CaptureChannel, ActorPrincipalID: principalID, CorrelationID: correlationID,
		CreatedAt: time.Now().UTC(),
	}
	s.consentReceipts[r.ConsentReceiptID] = r
	return r, nil
}

func (s *stubStore) FindConsentReceipt(ctx context.Context, receiptID string) (*domain.ConsentReceipt, error) {
	r, ok := s.consentReceipts[receiptID]
	if !ok {
		return nil, domain.ErrConsentReceiptNotFound
	}
	return r, nil
}

func (s *stubStore) WithdrawConsent(ctx context.Context, receiptID, channel, principalID string) (*domain.WithdrawalReceipt, error) {
	receipt, ok := s.consentReceipts[receiptID]
	if !ok {
		return nil, domain.ErrConsentReceiptNotFound
	}
	if _, already := s.withdrawalReceipts[receiptID]; already {
		return nil, domain.ErrAlreadyWithdrawn
	}
	w := &domain.WithdrawalReceipt{
		WithdrawalReceiptID: uuid.New().String(), TenantID: receipt.TenantID, ConsentReceiptID: receiptID,
		WithdrawnByPrincipalID: principalID, Channel: channel, CreatedAt: time.Now().UTC(),
	}
	s.withdrawalReceipts[receiptID] = w
	return w, nil
}

func (s *stubStore) ResolveConsentStatus(ctx context.Context, subjectRef, purposeID string) (*domain.ConsentResolution, error) {
	res := &domain.ConsentResolution{SubjectRef: subjectRef, PurposeID: purposeID, Status: domain.ConsentStatusNotRequested}
	var latest *domain.ConsentReceipt
	for _, r := range s.consentReceipts {
		if r.SubjectRef != subjectRef || r.PurposeID != purposeID {
			continue
		}
		if latest == nil || r.CreatedAt.After(latest.CreatedAt) {
			latest = r
		}
	}
	if latest == nil {
		return res, nil
	}
	res.LatestReceipt = latest
	if w, withdrawn := s.withdrawalReceipts[latest.ConsentReceiptID]; withdrawn {
		res.WithdrawalReceipt = w
		res.Status = domain.ConsentStatusWithdrawn
	} else {
		res.Status = domain.ConsentStatus(latest.Action)
	}
	return res, nil
}

// ── preferences ──────────────────────────────────────────────────────────────

func (s *stubStore) SetPreference(ctx context.Context, tenantID string, req domain.SetPreferenceRequest) (*domain.PreferenceAssertion, error) {
	p := domain.PreferenceAssertion{
		PreferenceAssertionID: uuid.New().String(), TenantID: strp(tenantID), SubjectRef: req.SubjectRef,
		ChannelOrPurpose: req.ChannelOrPurpose, Value: domain.PreferenceValue(req.Value), Source: req.Source,
		CreatedAt: time.Now().UTC(),
	}
	s.preferences = append(s.preferences, p)
	return &p, nil
}

func (s *stubStore) ResolvePreference(ctx context.Context, subjectRef, channelOrPurpose string) (*domain.PreferenceAssertion, error) {
	var latest *domain.PreferenceAssertion
	for i := range s.preferences {
		p := &s.preferences[i]
		if p.SubjectRef != subjectRef || p.ChannelOrPurpose != channelOrPurpose {
			continue
		}
		if latest == nil || p.CreatedAt.After(latest.CreatedAt) {
			latest = p
		}
	}
	if latest == nil {
		return &domain.PreferenceAssertion{SubjectRef: subjectRef, ChannelOrPurpose: channelOrPurpose, Value: domain.PreferenceNotApplicable}, nil
	}
	return latest, nil
}
