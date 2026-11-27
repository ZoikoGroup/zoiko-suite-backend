package handler_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/certreliance"
	"zoiko.io/general-ledger-svc/internal/domain"
	svcenvelope "zoiko.io/general-ledger-svc/internal/envelope"
	"zoiko.io/general-ledger-svc/internal/evidencereq"
	"zoiko.io/general-ledger-svc/internal/handler"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
)

// ZS-SVC-Z-001 RB-05 / DQC-017: a journal whose evidence cites a certification is a recorded consumer of it, so a later
// revocation can name the journal. Posting fails closed when that cannot be recorded.

type call struct {
	ref                                certreliance.Ref
	tenant, principal, journal, entity string
}

type stubRecorder struct {
	calls []call
	// errs answers by certification reference string; absent means success.
	errs map[string]error
}

func (s *stubRecorder) RecordReliance(_ context.Context, tenantID, principalID, _ string, ref certreliance.Ref, obj certreliance.Object) error {
	s.calls = append(s.calls, call{ref: ref, tenant: tenantID, principal: principalID, journal: obj.ID, entity: obj.LegalEntityID})
	return s.errs[ref.String()]
}

func routerWithReliance(s *stubStore, rec handler.CertificationRelianceRecorder, ev handler.EvidenceChecker) chi.Router {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	r.Use(svcenvelope.MiddlewareWithMode(svcenvelope.ServicePolicy(), svcenvelope.ModeObserve, nil))
	h := handler.New(s, &stubPublisher{}, &stubAuthZ{}, &stubClose{}, zap.NewNop())
	if ev != nil {
		h = h.WithEvidenceGate(ev)
	}
	if rec != nil {
		h = h.WithCertificationReliance(rec)
	}
	handler.RegisterRoutes(r, h)
	return r
}

const (
	dqRef  = "certification:data-quality-svc:dqn_1"
	recRef = "certification:reconciliation-engine-svc:rcr_2"
)

// A journal can be finalized by two routes; every case below must hold for both.
type route struct {
	name  string
	setup func(refs ...string) *stubStore
	post  func(r chi.Router) int
	state func(s *stubStore) domain.JournalStatus
}

func routes() []route {
	return []route{
		{
			name:  "POST /v1/journals/{id}/post",
			setup: validatedJournalWithEvidence,
			post: func(r chi.Router) int {
				return doRequest(r, http.MethodPost, "/v1/journals/j1/post", nil, "principal-1").Code
			},
			state: func(s *stubStore) domain.JournalStatus { return s.journals["j1"].Status },
		},
		{
			name: "POST /v1/postings/journals (approved journal)",
			setup: func(refs ...string) *stubStore {
				s := newStubStore()
				s.journals["j1"] = &domain.JournalHeader{JournalID: "j1", TenantID: testTenantID, LegalEntityID: "e1", FiscalPeriod: "2026-07",
					Status: domain.JournalStatusValidated, ApprovalStatus: domain.ApprovalStatusPostingRequested, EvidenceRefs: refs}
				return s
			},
			post: func(r chi.Router) int {
				return doRequest(r, http.MethodPost, "/v1/postings/journals", domain.PostApprovedJournalRequest{JournalID: "j1"}, "svc-ap").Code
			},
			state: func(s *stubStore) domain.JournalStatus { return s.journals["j1"].Status },
		},
	}
}

func TestCertReliance_JournalCitingACertificationIsRecordedThenPosted(t *testing.T) {
	for _, rt := range routes() {
		t.Run(rt.name, func(t *testing.T) {
			s := rt.setup("doc-1", dqRef, "invoice:INV-1", recRef)
			rec := &stubRecorder{}
			if code := rt.post(routerWithReliance(s, rec, nil)); code != http.StatusOK {
				t.Fatalf("status %d, want 200", code)
			}
			if rt.state(s) != domain.JournalStatusFinalized {
				t.Fatalf("journal is %s, want FINALIZED", rt.state(s))
			}
			if len(rec.calls) != 2 || rec.calls[0].ref.String() != dqRef || rec.calls[1].ref.String() != recRef {
				t.Fatalf("recorded %+v, want both certifications in order", rec.calls)
			}
			for _, c := range rec.calls {
				if c.journal != "j1" || c.entity != "e1" || c.principal == "" || c.tenant == "" {
					t.Fatalf("the relying object or identity is wrong: %+v", c)
				}
			}
		})
	}
}

func TestCertReliance_JournalCitingNoCertificationIsUntouched(t *testing.T) {
	for _, rt := range routes() {
		t.Run(rt.name, func(t *testing.T) {
			s := rt.setup("doc-1", "invoice:INV-1")
			rec := &stubRecorder{}
			if code := rt.post(routerWithReliance(s, rec, nil)); code != http.StatusOK || rt.state(s) != domain.JournalStatusFinalized {
				t.Fatalf("status %d, journal %s", code, rt.state(s))
			}
			if len(rec.calls) != 0 {
				t.Fatalf("a journal that cites no certification recorded reliance: %+v", rec.calls)
			}
			// And with no recorder wired at all, such a journal posts exactly as before.
			s2 := rt.setup()
			if code := rt.post(routerWithReliance(s2, nil, nil)); code != http.StatusOK || rt.state(s2) != domain.JournalStatusFinalized {
				t.Fatalf("no recorder, no certification: status %d, journal %s", code, rt.state(s2))
			}
		})
	}
}

func TestCertReliance_PostingFailsClosedAndTheJournalIsNotFinalized(t *testing.T) {
	for _, rt := range routes() {
		for name, tc := range map[string]struct {
			err  error
			want int
		}{
			"withdrawn, superseded or stale": {certreliance.ErrNotInForce, http.StatusUnprocessableEntity},
			"no such certification":          {certreliance.ErrUnknown, http.StatusUnprocessableEntity},
			"not permitted":                  {certreliance.ErrDenied, http.StatusForbidden},
			"service unavailable":            {certreliance.ErrUnavailable, http.StatusServiceUnavailable},
			"anything unexpected":            {context.DeadlineExceeded, http.StatusServiceUnavailable},
		} {
			t.Run(rt.name+"/"+name, func(t *testing.T) {
				s := rt.setup(dqRef)
				rec := &stubRecorder{errs: map[string]error{dqRef: tc.err}}
				if code := rt.post(routerWithReliance(s, rec, nil)); code != tc.want {
					t.Fatalf("status %d, want %d", code, tc.want)
				}
				if rt.state(s) != domain.JournalStatusValidated {
					t.Fatalf("a journal whose certification cannot be recorded was finalized: %s", rt.state(s))
				}
			})
		}
	}
}

func TestCertReliance_OneBadCertificationAmongSeveralRefusesThePosting(t *testing.T) {
	for _, rt := range routes() {
		t.Run(rt.name, func(t *testing.T) {
			s := rt.setup(dqRef, recRef)
			rec := &stubRecorder{errs: map[string]error{recRef: certreliance.ErrNotInForce}}
			if code := rt.post(routerWithReliance(s, rec, nil)); code != http.StatusUnprocessableEntity || rt.state(s) != domain.JournalStatusValidated {
				t.Fatalf("status %d, journal %s", code, rt.state(s))
			}
		})
	}
}

func TestCertReliance_MalformedReferenceIsRefusedNotSkipped(t *testing.T) {
	for _, rt := range routes() {
		for _, bad := range []string{"certification:data-qualty-svc:dqn_1", "certification:data-quality-svc:", "certification:data-quality-svc:../x"} {
			t.Run(rt.name+"/"+bad, func(t *testing.T) {
				s := rt.setup("doc-1", bad)
				rec := &stubRecorder{}
				if code := rt.post(routerWithReliance(s, rec, nil)); code != http.StatusUnprocessableEntity || rt.state(s) != domain.JournalStatusValidated {
					t.Fatalf("status %d, journal %s", code, rt.state(s))
				}
				if len(rec.calls) != 0 {
					t.Fatalf("a malformed reference reached the recorder: %+v", rec.calls)
				}
			})
		}
	}
}

func TestCertReliance_CitingACertificationWithNoRecorderWiredFailsClosed(t *testing.T) {
	for _, rt := range routes() {
		t.Run(rt.name, func(t *testing.T) {
			s := rt.setup(dqRef)
			if code := rt.post(routerWithReliance(s, nil, nil)); code != http.StatusServiceUnavailable || rt.state(s) != domain.JournalStatusValidated {
				t.Fatalf("status %d, journal %s: a journal resting on a certification posted with nothing to tell anyone", code, rt.state(s))
			}
		})
	}
}

// Nothing is recorded for a journal that fails the evidence gate: the reliance would be for a posting that never happens.
func TestCertReliance_EvidenceGateRunsFirst(t *testing.T) {
	s := validatedJournalWithEvidence(dqRef)
	rec := &stubRecorder{}
	r := routerWithReliance(s, rec, &stubEvidence{err: evidencereq.ErrEvidenceMissing})
	if rr := doRequest(r, http.MethodPost, "/v1/journals/j1/post", nil, "principal-1"); rr.Code != http.StatusUnprocessableEntity || !strings.Contains(rr.Body.String(), "evidence_missing") {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if len(rec.calls) != 0 {
		t.Fatalf("reliance was recorded for a journal that failed the evidence gate: %+v", rec.calls)
	}
}
