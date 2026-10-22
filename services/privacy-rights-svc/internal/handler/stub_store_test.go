package handler_test

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"zoiko.io/privacy-rights-svc/internal/domain"
	"zoiko.io/privacy-rights-svc/internal/middleware"
	"zoiko.io/privacy-rights-svc/internal/store"
)

// stubStore is a real, working in-memory implementation of store.Store —
// replicates PgStore's state machine, disclosure gate, tenant scoping (reads
// of another tenant's case are "not found", as under RLS) and its all-or-
// nothing idempotency contract, not canned responses.
type stubStore struct {
	requests    map[string]*domain.RightsRequest
	events      map[string][]domain.IdentityVerificationEvent // by request_id
	manifests   map[string][]domain.DiscoveryManifest         // by request_id
	idempotency map[string]*domain.IdempotencyRecord          // tenant:principal:key

	failLookup bool // GetIdempotency fails
	failCommit bool // the atomic mutation fails (nothing persisted)
	execs      int  // mutations actually executed
}

func newStubStore() *stubStore {
	return &stubStore{
		requests:    map[string]*domain.RightsRequest{},
		events:      map[string][]domain.IdentityVerificationEvent{},
		manifests:   map[string][]domain.DiscoveryManifest{},
		idempotency: map[string]*domain.IdempotencyRecord{},
	}
}

func strp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func idemKey(tenant string, i *store.Idem) string { return tenant + ":" + i.PrincipalID + ":" + i.Key }

// run mirrors PgStore.mutate: claim → operation → store the replay response,
// all-or-nothing. Domain errors and failCommit leave nothing behind.
func (s *stubStore) run(ctx context.Context, idem *store.Idem, fn func() (any, error)) (any, *domain.IdempotencyRecord, error) {
	tenant := middleware.TenantFromContext(ctx)
	if idem != nil {
		if rec, ok := s.idempotency[idemKey(tenant, idem)]; ok {
			return nil, rec, nil
		}
	}
	if s.failCommit {
		return nil, nil, domain.ErrStoreUnavailable
	}
	// Snapshot so a failing operation rolls back.
	snapReq := map[string]domain.RightsRequest{}
	for k, v := range s.requests {
		snapReq[k] = *v
	}
	res, err := fn()
	if err != nil {
		s.requests = map[string]*domain.RightsRequest{}
		for k, v := range snapReq {
			c := v
			s.requests[k] = &c
		}
		return nil, nil, err
	}
	s.execs++
	if idem != nil {
		code, body, rerr := idem.Render(res)
		if rerr != nil {
			return nil, nil, rerr
		}
		s.idempotency[idemKey(tenant, idem)] = &domain.IdempotencyRecord{
			Key: idem.Key, TenantID: tenant, PrincipalID: idem.PrincipalID, Endpoint: idem.Endpoint,
			RequestHash: idem.RequestHash, ResponseCode: code, ResponseBody: body,
		}
	}
	return res, nil, nil
}

// visible implements the RLS read scope: only the caller's tenant's cases.
func (s *stubStore) visible(ctx context.Context, requestID string) (*domain.RightsRequest, bool) {
	r, ok := s.requests[requestID]
	if !ok || r.TenantID == nil || *r.TenantID != middleware.TenantFromContext(ctx) {
		return nil, false
	}
	return r, true
}

func (s *stubStore) openRequest(ctx context.Context, requestID string) (*domain.RightsRequest, error) {
	r, ok := s.visible(ctx, requestID)
	if !ok {
		return nil, domain.ErrRequestNotFound
	}
	if r.Status == domain.StatusClosed {
		return nil, domain.ErrRequestAlreadyClosed
	}
	return r, nil
}

func (s *stubStore) CreateRequest(ctx context.Context, tenantID string, req domain.CreateRightsRequestRequest, principalID string, idem *store.Idem) (*domain.RightsRequest, *domain.IdempotencyRecord, error) {
	res, ex, err := s.run(ctx, idem, func() (any, error) {
		r := &domain.RightsRequest{
			RequestID: uuid.New().String(), TenantID: strp(tenantID), SubjectRef: req.SubjectRef,
			RightFamily: req.RightFamily, Jurisdiction: req.Jurisdiction, RequesterRef: req.RequesterRef,
			SubmittedVia: req.SubmittedVia, Status: domain.StatusReceived,
			CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID,
		}
		s.requests[r.RequestID] = r
		return r, nil
	})
	if err != nil || ex != nil {
		return nil, ex, err
	}
	return res.(*domain.RightsRequest), nil, nil
}

func (s *stubStore) FindRequest(ctx context.Context, requestID string) (*domain.RightsRequest, error) {
	r, ok := s.visible(ctx, requestID)
	if !ok {
		return nil, domain.ErrRequestNotFound
	}
	cp := *r
	return &cp, nil
}

func (s *stubStore) ListRequestsBySubject(ctx context.Context, subjectRef string) ([]domain.RightsRequest, error) {
	var out []domain.RightsRequest
	for _, r := range s.requests {
		if r.SubjectRef == subjectRef && r.TenantID != nil && *r.TenantID == middleware.TenantFromContext(ctx) {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (s *stubStore) RecordIdentityVerification(ctx context.Context, requestID string, req domain.RecordIdentityVerificationRequest, principalID string, idem *store.Idem) (*store.IdentityResult, *domain.IdempotencyRecord, error) {
	res, ex, err := s.run(ctx, idem, func() (any, error) {
		r, err := s.openRequest(ctx, requestID)
		if err != nil {
			return nil, err
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
		cp := *r
		return &store.IdentityResult{Event: &ev, Request: &cp}, nil
	})
	if err != nil || ex != nil {
		return nil, ex, err
	}
	return res.(*store.IdentityResult), nil, nil
}

func (s *stubStore) AttachDiscoveryManifest(ctx context.Context, requestID string, req domain.AttachDiscoveryManifestRequest, principalID string, idem *store.Idem) (*store.ManifestResult, *domain.IdempotencyRecord, error) {
	res, ex, err := s.run(ctx, idem, func() (any, error) {
		r, err := s.openRequest(ctx, requestID)
		if err != nil {
			return nil, err
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
		cp := *r
		return &store.ManifestResult{Manifest: &m, Request: &cp}, nil
	})
	if err != nil || ex != nil {
		return nil, ex, err
	}
	return res.(*store.ManifestResult), nil, nil
}

func (s *stubStore) ListDiscoveryManifests(ctx context.Context, requestID string) ([]domain.DiscoveryManifest, error) {
	if _, ok := s.visible(ctx, requestID); !ok {
		return nil, nil
	}
	return s.manifests[requestID], nil
}

func (s *stubStore) CloseRequest(ctx context.Context, requestID string, req domain.CloseRequestRequest, principalID string, idem *store.Idem) (*domain.RightsRequest, *domain.IdempotencyRecord, error) {
	res, ex, err := s.run(ctx, idem, func() (any, error) {
		r, ok := s.visible(ctx, requestID)
		if !ok {
			return nil, domain.ErrRequestNotFound
		}
		if r.Status == domain.StatusClosed {
			return nil, domain.ErrRequestAlreadyClosed
		}
		if req.Outcome == domain.OutcomeFulfilled {
			verifiedEvents := 0
			for _, e := range s.events[requestID] {
				if e.Verified {
					verifiedEvents++
				}
			}
			if !r.IdentityVerified || verifiedEvents == 0 {
				return nil, domain.ErrIdentityNotVerified
			}
			if len(s.manifests[requestID]) == 0 {
				return nil, domain.ErrNoDiscoveryManifest
			}
			r.ResponsePackageVersion++
		}
		now := time.Now().UTC()
		r.Status = domain.StatusClosed
		o := req.Outcome
		r.Outcome = &o
		if req.ResponseEvidenceHash != "" {
			r.ResponseEvidenceHash = strp(req.ResponseEvidenceHash)
		}
		r.ClosedAt = &now
		cp := *r
		return &cp, nil
	})
	if err != nil || ex != nil {
		return nil, ex, err
	}
	return res.(*domain.RightsRequest), nil, nil
}

func (s *stubStore) AttachWFCProcessRef(ctx context.Context, requestID, wfcProcessRef string, idem *store.Idem) (*domain.RightsRequest, *domain.IdempotencyRecord, error) {
	res, ex, err := s.run(ctx, idem, func() (any, error) {
		r, err := s.openRequest(ctx, requestID)
		if err != nil {
			return nil, err
		}
		r.WFCProcessRef = strp(wfcProcessRef)
		cp := *r
		return &cp, nil
	})
	if err != nil || ex != nil {
		return nil, ex, err
	}
	return res.(*domain.RightsRequest), nil, nil
}

func (s *stubStore) GetIdempotency(ctx context.Context, tenantID, principalID, key string) (*domain.IdempotencyRecord, error) {
	if s.failLookup {
		return nil, errors.New("lookup failed")
	}
	rec, ok := s.idempotency[tenantID+":"+principalID+":"+key]
	if !ok {
		return nil, nil
	}
	return rec, nil
}
