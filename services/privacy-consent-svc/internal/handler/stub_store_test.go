package handler_test

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"zoiko.io/privacy-consent-svc/internal/domain"
)

// stubStore is a real, working in-memory implementation of store.Store —
// replicates the actual state-machine and derivation semantics PgStore
// enforces in Postgres, INCLUDING enqueuing an outbox event record for
// every fact that PgStore now enqueues inside its own transaction (see
// outboxEvents below) — a test asserting an event was produced is
// asserting the same thing the real store's transaction guarantees, not a
// separate publisher's behavior.
type stubStore struct {
	notices              map[string]*domain.Notice
	noticeVersions       map[string]*domain.NoticeVersion
	presentationReceipts []domain.PresentationReceipt
	consentReceipts      map[string]*domain.ConsentReceipt
	consentRequests      map[string]*domain.ConsentRequest // keyed by consent_receipt_id
	withdrawalReceipts   map[string]*domain.WithdrawalReceipt
	invalidations        map[string]*domain.ConsentInvalidation
	preferences          []domain.PreferenceAssertion
	idempotency          map[string]*domain.IdempotencyRecord

	// outboxEvents records every event this stub "enqueued" — the event
	// types only, in order, mirroring what a test could learn by querying
	// the real event_outbox table.
	outboxEvents []string

	// Failure injection and call counters for the §18.1 / Gap-9 tests.
	idempotencyGetErr  error
	idempotencySaveErr error
	findLatestCalls    int
	findReceiptCalls   int

	// seq mirrors the real PgStore's sequence_no column — see
	// privacy-purpose-registry-svc's identical field for the full
	// rationale: two notice versions can share an identical wall-clock
	// EffectiveFrom under Windows' coarse time.Now() resolution, and
	// ResolveNoticeAsOf needs a collision-proof tiebreaker.
	nextSeq int
	seq     map[string]int
}

const (
	testPublishedNoticeID        = "00000000-0000-0000-0000-000000000002"
	testPublishedNoticeVersionID = "00000000-0000-0000-0000-000000000001"
)

func newStubStore() *stubStore {
	now := time.Now().UTC()
	tenant := testTenant
	eff := now.Add(-time.Hour)
	return &stubStore{
		notices: map[string]*domain.Notice{
			testPublishedNoticeID: {
				NoticeID:             testPublishedNoticeID,
				TenantID:             &tenant,
				CreatedByPrincipalID: "maker-default",
				CreatedAt:            now,
			},
		},
		noticeVersions: map[string]*domain.NoticeVersion{
			testPublishedNoticeVersionID: {
				NoticeVersionID:      testPublishedNoticeVersionID,
				NoticeID:             testPublishedNoticeID,
				Locale:               "en-US",
				Audience:             "CUSTOMER",
				ContentHash:          "sha256:preseeded",
				VersionStatus:        domain.NoticeStatusPublished,
				EffectiveFrom:        &eff,
				CreatedByPrincipalID: "maker-default",
				CreatedAt:            now,
			},
		},
		consentReceipts:    map[string]*domain.ConsentReceipt{},
		consentRequests:    map[string]*domain.ConsentRequest{},
		withdrawalReceipts: map[string]*domain.WithdrawalReceipt{},
		invalidations:      map[string]*domain.ConsentInvalidation{},
		idempotency:        map[string]*domain.IdempotencyRecord{},
		seq:                map[string]int{testPublishedNoticeVersionID: 1},
		nextSeq:            1,
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

func (s *stubStore) FindLatestNoticeVersion(ctx context.Context, noticeID string) (*domain.NoticeVersion, error) {
	s.findLatestCalls++
	var best *domain.NoticeVersion
	for _, v := range s.noticeVersions {
		if v.NoticeID != noticeID {
			continue
		}
		if best == nil || v.CreatedAt.After(best.CreatedAt) || (v.CreatedAt.Equal(best.CreatedAt) && s.seq[v.NoticeVersionID] > s.seq[best.NoticeVersionID]) {
			best = v
		}
	}
	if best == nil {
		return nil, domain.ErrNoticeVersionNotFound
	}
	return best, nil
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

// PublishNoticeVersion mirrors PgStore's: records publisher evidence,
// closes the superseded version's effective window, and enqueues the
// privacy.notice.published event — all as one unit, matching the real
// store's single transaction.
func (s *stubStore) PublishNoticeVersion(ctx context.Context, noticeID, versionID, principalID, correlationID string) (*domain.NoticeVersion, error) {
	v, ok := s.noticeVersions[versionID]
	if !ok || v.NoticeID != noticeID || v.VersionStatus != domain.NoticeStatusApproved {
		return nil, domain.ErrInvalidNoticeTransition
	}
	now := time.Now().UTC()
	for _, other := range s.noticeVersions {
		if other.NoticeID == noticeID && other.VersionStatus == domain.NoticeStatusPublished {
			other.VersionStatus = domain.NoticeStatusSuperseded
			other.EffectiveTo = &now
		}
	}
	v.VersionStatus = domain.NoticeStatusPublished
	v.EffectiveFrom = &now
	v.PublishedByPrincipalID = strp(principalID)
	v.PublishedAt = &now
	s.outboxEvents = append(s.outboxEvents, "privacy.notice.published")
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

// requirePublished mirrors store.requirePublishedNoticeVersion: only a
// PUBLISHED version passes; everything else — and a nonexistent id, and a
// version belonging to another notice — is ErrNoticeVersionInvalid.
func (s *stubStore) requirePublished(noticeID, versionID string) error {
	v, ok := s.noticeVersions[versionID]
	if !ok || (noticeID != "" && v.NoticeID != noticeID) || v.VersionStatus != domain.NoticeStatusPublished {
		return domain.ErrNoticeVersionInvalid
	}
	return nil
}

func (s *stubStore) FindPublishedNoticeVersion(ctx context.Context, noticeID string) (*domain.NoticeVersion, error) {
	for _, v := range s.noticeVersions {
		if v.NoticeID == noticeID && v.VersionStatus == domain.NoticeStatusPublished {
			return v, nil
		}
	}
	return nil, domain.ErrNoticeVersionInvalid
}

func (s *stubStore) RecordPresentation(ctx context.Context, tenantID, noticeID, versionID string, req domain.RecordPresentationRequest) (*domain.PresentationReceipt, error) {
	if err := s.requirePublished(noticeID, versionID); err != nil {
		return nil, err
	}
	r := domain.PresentationReceipt{
		PresentationReceiptID: uuid.New().String(),
		TenantID:              strp(tenantID),
		NoticeVersionID:       versionID,
		SubjectRef:            req.SubjectRef,
		Channel:               req.Channel,
		Locale:                req.Locale,
		SessionRef:            strp(req.SessionRef),
		TemplateVersion:       strp(req.TemplateVersion),
		DeliveryEvidence:      strp(req.DeliveryEvidence),
		CreatedAt:             time.Now().UTC(),
	}
	s.presentationReceipts = append(s.presentationReceipts, r)
	return &r, nil
}

// ── consent ──────────────────────────────────────────────────────────────────

func (s *stubStore) RecordConsent(ctx context.Context, tenantID string, req domain.RecordConsentRequest, principalID, correlationID string) (*domain.ConsentReceipt, error) {
	// Before PRV-N04 dedup, like the real store: an existing receipt must
	// never answer a request that cites an unpublished notice version,
	// and notice_version_id must exist and be PUBLISHED.
	if strings.TrimSpace(req.NoticeVersionID) == "" {
		return nil, domain.ErrNoticeVersionInvalid
	}
	if err := s.requirePublished("", req.NoticeVersionID); err != nil {
		return nil, err
	}
	// PRV-N04 replay — mirrors PgStore: only while the LATEST receipt for the
	// (subject, purpose) pair is the live outcome of this exact request (same
	// action and notice version, still derived GRANTED/DENIED — not
	// withdrawn, invalidated or expired).
	if res, _ := s.ResolveConsentStatus(ctx, req.SubjectRef, req.PurposeID); res != nil && res.LatestReceipt != nil {
		l := res.LatestReceipt
		nvMatch := (l.NoticeVersionID == nil && req.NoticeVersionID == "") ||
			(l.NoticeVersionID != nil && *l.NoticeVersionID == req.NoticeVersionID)
		if string(l.Action) == req.Action && nvMatch && res.Status == domain.ConsentStatus(l.Action) {
			return l, nil
		}
	}

	affAction := req.AffirmativeActionType
	if affAction == "" {
		affAction = "EXPLICIT_CHECKBOX"
	}

	now := time.Now().UTC()
	r := &domain.ConsentReceipt{
		ConsentReceiptID:           uuid.New().String(),
		TenantID:                   strp(tenantID),
		SubjectRef:                 req.SubjectRef,
		PurposeID:                  req.PurposeID,
		NoticeVersionID:            strp(req.NoticeVersionID),
		Action:                     domain.ConsentAction(req.Action),
		CaptureChannel:             req.CaptureChannel,
		ActorPrincipalID:           principalID,
		CorrelationID:              correlationID,
		CreatedAt:                  now,
		IsProxy:                    req.IsProxy,
		RepresentativeSubjectRef:   strp(req.RepresentativeSubjectRef),
		RepresentativeAuthorityRef: strp(req.RepresentativeAuthorityRef),
		RepresentativeEvidence:     strp(req.RepresentativeEvidence),
		AffirmativeActionType:      affAction,
		AffirmativeEvidence:        strp(req.AffirmativeEvidence),
		AgeBand:                    strp(req.AgeBand),
		IsOfflineImport:            req.IsOfflineImport,
		ImportSource:               strp(req.ImportSource),
		ImportCaptureMethod:        strp(req.ImportCaptureMethod),
		ImportEvidenceRef:          strp(req.ImportEvidenceRef),
		ImportedAt:                 req.ImportedAt,
		ExpiresAt:                  req.ExpiresAt,
	}
	s.consentReceipts[r.ConsentReceiptID] = r
	if req.ConsentRequest != nil {
		s.consentRequests[r.ConsentReceiptID] = req.ConsentRequest
		r.ConsentRequest = req.ConsentRequest
	}
	r.ReceiptIntegrityHash = r.ComputeIntegrityHash() // the real function, as PgStore uses
	s.nextSequence(r.ConsentReceiptID)
	s.outboxEvents = append(s.outboxEvents, "privacy.consent.changed")
	return r, nil
}

func (s *stubStore) FindConsentReceipt(ctx context.Context, receiptID string) (*domain.ConsentReceipt, error) {
	s.findReceiptCalls++
	r, ok := s.consentReceipts[receiptID]
	if !ok {
		return nil, domain.ErrConsentReceiptNotFound
	}
	return r, nil
}

func (s *stubStore) WithdrawConsent(ctx context.Context, receiptID, channel, principalID, correlationID string) (*domain.WithdrawalReceipt, error) {
	receipt, ok := s.consentReceipts[receiptID]
	if !ok {
		return nil, domain.ErrConsentReceiptNotFound
	}
	// PRV-N05: Replayed withdrawal returns existing withdrawal receipt idempotently
	if w, already := s.withdrawalReceipts[receiptID]; already {
		return w, nil
	}
	w := &domain.WithdrawalReceipt{
		WithdrawalReceiptID:      uuid.New().String(),
		TenantID:                 receipt.TenantID,
		ConsentReceiptID:         receiptID,
		WithdrawnByPrincipalID:   principalID,
		Channel:                  channel,
		CreatedAt:                time.Now().UTC(),
		PropagationCorrelationID: "prop-" + uuid.New().String(),
	}
	s.withdrawalReceipts[receiptID] = w
	s.outboxEvents = append(s.outboxEvents, "privacy.consent.changed")
	return w, nil
}

func (s *stubStore) InvalidateConsent(ctx context.Context, receiptID, reason, channel, principalID, correlationID string) (*domain.ConsentInvalidation, error) {
	_, ok := s.consentReceipts[receiptID]
	if !ok {
		return nil, domain.ErrConsentReceiptNotFound
	}
	if inv, already := s.invalidations[receiptID]; already {
		return inv, nil
	}
	inv := &domain.ConsentInvalidation{
		ConsentInvalidationID:    uuid.New().String(),
		TenantID:                 s.consentReceipts[receiptID].TenantID,
		ConsentReceiptID:         receiptID,
		InvalidatedByPrincipalID: principalID,
		Reason:                   reason,
		Channel:                  channel,
		CreatedAt:                time.Now().UTC(),
	}
	s.invalidations[receiptID] = inv
	s.outboxEvents = append(s.outboxEvents, "privacy.consent.changed")
	return inv, nil
}

func (s *stubStore) ResolveConsentStatus(ctx context.Context, subjectRef, purposeID string) (*domain.ConsentResolution, error) {
	res := &domain.ConsentResolution{SubjectRef: subjectRef, PurposeID: purposeID, Status: domain.ConsentStatusNotRequested}
	var latest *domain.ConsentReceipt
	for _, r := range s.consentReceipts {
		if r.SubjectRef != subjectRef || r.PurposeID != purposeID {
			continue
		}
		// Insertion order breaks ties: two receipts written in the same clock
		// tick must still have a deterministic "latest".
		if latest == nil || r.CreatedAt.After(latest.CreatedAt) ||
			(r.CreatedAt.Equal(latest.CreatedAt) && s.seq[r.ConsentReceiptID] > s.seq[latest.ConsentReceiptID]) {
			latest = r
		}
	}
	if latest == nil {
		return res, nil
	}
	res.LatestReceipt = latest
	if inv, invalidated := s.invalidations[latest.ConsentReceiptID]; invalidated {
		res.ConsentInvalidation = inv
		res.Status = domain.ConsentStatusInvalidated
		return res, nil
	}
	if w, withdrawn := s.withdrawalReceipts[latest.ConsentReceiptID]; withdrawn {
		res.WithdrawalReceipt = w
		res.Status = domain.ConsentStatusWithdrawn
		return res, nil
	}
	if latest.ExpiresAt != nil && !latest.ExpiresAt.After(time.Now().UTC()) {
		res.Status = domain.ConsentStatusExpired
		return res, nil
	}
	res.Status = domain.ConsentStatus(latest.Action)
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

// ── idempotency (§18.1) ──────────────────────────────────────────────────────

func (s *stubStore) GetIdempotency(ctx context.Context, tenantID, key string) (*domain.IdempotencyRecord, error) {
	if s.idempotencyGetErr != nil {
		return nil, s.idempotencyGetErr
	}
	k := tenantID + ":" + key
	rec, ok := s.idempotency[k]
	if !ok {
		return nil, nil
	}
	return rec, nil
}

func (s *stubStore) SaveIdempotency(ctx context.Context, rec domain.IdempotencyRecord) error {
	if s.idempotencySaveErr != nil {
		return s.idempotencySaveErr
	}
	k := rec.TenantID + ":" + rec.Key
	s.idempotency[k] = &rec
	return nil
}
