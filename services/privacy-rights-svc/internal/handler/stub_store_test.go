package handler_test

import (
	"context"
	"time"

	"github.com/google/uuid"

	"zoiko.io/privacy-rights-svc/internal/domain"
)

// stubStore is a real, working in-memory implementation of store.Store —
// replicates PgStore's actual state-machine and DISCLOSURE GATE
// semantics, not canned responses.
type stubStore struct {
	requests  map[string]*domain.RightsRequest
	events    map[string][]domain.IdentityVerificationEvent // by request_id
	manifests map[string][]domain.DiscoveryManifest         // by request_id
}

func newStubStore() *stubStore {
	return &stubStore{
		requests:  map[string]*domain.RightsRequest{},
		events:    map[string][]domain.IdentityVerificationEvent{},
		manifests: map[string][]domain.DiscoveryManifest{},
	}
}

func strp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *stubStore) CreateRequest(_ context.Context, tenantID string, req domain.CreateRightsRequestRequest, principalID string) (*domain.RightsRequest, error) {
	r := &domain.RightsRequest{
		RequestID: uuid.New().String(), TenantID: strp(tenantID), SubjectRef: req.SubjectRef,
		RightFamily: req.RightFamily, Jurisdiction: req.Jurisdiction, RequesterRef: req.RequesterRef,
		SubmittedVia: req.SubmittedVia, Status: domain.StatusReceived,
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	s.requests[r.RequestID] = r
	return r, nil
}

func (s *stubStore) FindRequest(_ context.Context, requestID string) (*domain.RightsRequest, error) {
	r, ok := s.requests[requestID]
	if !ok {
		return nil, domain.ErrRequestNotFound
	}
	return r, nil
}

func (s *stubStore) ListRequestsBySubject(_ context.Context, subjectRef string) ([]domain.RightsRequest, error) {
	var out []domain.RightsRequest
	for _, r := range s.requests {
		if r.SubjectRef == subjectRef {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (s *stubStore) RecordIdentityVerification(_ context.Context, requestID string, req domain.RecordIdentityVerificationRequest, principalID string) (*domain.IdentityVerificationEvent, *domain.RightsRequest, error) {
	r, ok := s.requests[requestID]
	if !ok {
		return nil, nil, domain.ErrRequestNotFound
	}
	ev := domain.IdentityVerificationEvent{
		EventID: uuid.New().String(), TenantID: r.TenantID, RequestID: requestID,
		Verified: req.Verified, Method: req.Method, Note: req.Note,
		VerifiedByPrincipalID: principalID, CreatedAt: time.Now().UTC(),
	}
	s.events[requestID] = append(s.events[requestID], ev)

	if req.Verified {
		r.IdentityVerified = true
		if r.Status == domain.StatusReceived {
			r.Status = domain.StatusIdentityVerified
		}
	}
	return &ev, r, nil
}

func (s *stubStore) AttachDiscoveryManifest(_ context.Context, requestID string, req domain.AttachDiscoveryManifestRequest, principalID string) (*domain.DiscoveryManifest, *domain.RightsRequest, error) {
	r, ok := s.requests[requestID]
	if !ok {
		return nil, nil, domain.ErrRequestNotFound
	}
	m := domain.DiscoveryManifest{
		ManifestID: uuid.New().String(), TenantID: r.TenantID, RequestID: requestID,
		Domain: req.Domain, ContentHash: req.ContentHash, CandidateCount: req.CandidateCount,
		EvidenceRef: req.EvidenceRef, SubmittedByPrincipalID: principalID, CreatedAt: time.Now().UTC(),
	}
	s.manifests[requestID] = append(s.manifests[requestID], m)

	if r.Status == domain.StatusIdentityVerified {
		r.Status = domain.StatusInDiscovery
	}
	return &m, r, nil
}

func (s *stubStore) ListDiscoveryManifests(_ context.Context, requestID string) ([]domain.DiscoveryManifest, error) {
	return s.manifests[requestID], nil
}

func (s *stubStore) CloseRequest(_ context.Context, requestID string, req domain.CloseRequestRequest, _ string) (*domain.RightsRequest, error) {
	r, ok := s.requests[requestID]
	if !ok {
		return nil, domain.ErrRequestNotFound
	}
	if r.Status == domain.StatusClosed {
		return nil, domain.ErrRequestAlreadyClosed
	}
	if req.Outcome == domain.OutcomeFulfilled {
		if !r.IdentityVerified {
			return nil, domain.ErrIdentityNotVerified
		}
		if len(s.manifests[requestID]) == 0 {
			return nil, domain.ErrNoDiscoveryManifest
		}
	}
	now := time.Now().UTC()
	r.Status = domain.StatusClosed
	r.Outcome = &req.Outcome
	if req.ResponseEvidenceHash != "" {
		r.ResponseEvidenceHash = strp(req.ResponseEvidenceHash)
	}
	r.ClosedAt = &now
	return r, nil
}

func (s *stubStore) AttachWFCProcessRef(_ context.Context, requestID, wfcProcessRef string) (*domain.RightsRequest, error) {
	r, ok := s.requests[requestID]
	if !ok {
		return nil, domain.ErrRequestNotFound
	}
	r.WFCProcessRef = strp(wfcProcessRef)
	return r, nil
}
