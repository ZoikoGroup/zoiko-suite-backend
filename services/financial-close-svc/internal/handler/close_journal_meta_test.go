package handler_test

// ACC-14's designated-close-journal context, as this service sends it to
// general-ledger-svc. The designation is always carried on ACC-07/08
// recognition and termination journals; the separate SOFT_CLOSE override is
// added only while the period is actually SOFT_CLOSE. CLOSE_REVIEW gets the
// designation and no override, because GL has a different rule for that state.

import (
	"net/http"
	"strings"
	"testing"

	"zoiko.io/financial-close-svc/internal/domain"
)

func setClosePeriod(s *stubStore, periodName, status string) {
	s.periods["fp-"+periodName] = &domain.FiscalPeriod{
		FiscalPeriodID: "fp-" + periodName,
		TenantID:       testTenantID,
		LegalEntityID:  "le-1",
		PeriodName:     periodName,
		PeriodStart:    day(2026, 1, 1),
		PeriodEnd:      day(2026, 12, 31),
		CloseStatus:    status,
	}
}

func assertCloseJournalMeta(t *testing.T, cl *stubClients, wantDesignation string, wantOverride bool) {
	t.Helper()
	if cl.metaCalls != 1 {
		t.Fatalf("expected exactly one posting, got %d", cl.metaCalls)
	}
	m := cl.lastJournalMeta
	if m.CloseJournalDesignation != wantDesignation {
		t.Fatalf("close_journal_designation = %q, want %q", m.CloseJournalDesignation, wantDesignation)
	}
	if m.OverrideSoftClose != wantOverride {
		t.Fatalf("override_soft_close = %v, want %v", m.OverrideSoftClose, wantOverride)
	}
	if wantOverride {
		if strings.TrimSpace(m.SoftCloseOverrideReason) == "" {
			t.Fatal("SOFT_CLOSE override must carry a non-empty reason")
		}
	} else if m.SoftCloseOverrideReason != "" {
		t.Fatalf("SOFT_CLOSE override reason must be empty outside SOFT_CLOSE, got %q", m.SoftCloseOverrideReason)
	}
}

func TestCloseJournalMeta_ACC07_CloseReview(t *testing.T) {
	s, cl := newStubStore(), &stubClients{}
	setClosePeriod(s, "2026-01", domain.PeriodCloseReview)
	r := newRouter(s, &stubAuthZ{}, cl)
	id := createApprovedAccrual(t, s, r, 900, 3, "2026-01")

	rr := doReq(r, http.MethodPost, "/v1/accruals/"+id+"/recognize",
		domain.RunAccrualRecognitionRequest{FiscalPeriod: "2026-01"}, "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("recognize: %d %s", rr.Code, rr.Body.String())
	}
	assertCloseJournalMeta(t, cl, domain.GLCloseJournalAccrualRecognition, false)
}

func TestCloseJournalMeta_ACC07_SoftCloseUsesOverride(t *testing.T) {
	s, cl := newStubStore(), &stubClients{}
	setClosePeriod(s, "2026-01", domain.PeriodSoftClose)
	r := newRouter(s, &stubAuthZ{}, cl)
	id := createApprovedAccrual(t, s, r, 900, 3, "2026-01")

	rr := doReq(r, http.MethodPost, "/v1/accruals/"+id+"/recognize",
		domain.RunAccrualRecognitionRequest{FiscalPeriod: "2026-01"}, "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("recognize: %d %s", rr.Code, rr.Body.String())
	}
	assertCloseJournalMeta(t, cl, domain.GLCloseJournalAccrualRecognition, true)
	if !strings.Contains(cl.lastJournalMeta.SoftCloseOverrideReason, domain.GLCloseJournalAccrualRecognition) {
		t.Fatalf("override reason should name the designation, got %q", cl.lastJournalMeta.SoftCloseOverrideReason)
	}
}

func TestCloseJournalMeta_ACC08Recognition_CloseReview(t *testing.T) {
	s, cl := newStubStore(), &stubClients{}
	setClosePeriod(s, "2026-02", domain.PeriodCloseReview)
	r := newRouter(s, &stubAuthZ{}, cl)
	id := createApprovedPrepayment(t, s, r, 1200, 12, "2026-01")

	rr := doReq(r, http.MethodPost, "/v1/prepayments/"+id+"/recognize",
		domain.RunPrepaymentRecognitionRequest{FiscalPeriod: "2026-02"}, "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("recognize: %d %s", rr.Code, rr.Body.String())
	}
	assertCloseJournalMeta(t, cl, domain.GLCloseJournalPrepaymentRecognition, false)
}

func TestCloseJournalMeta_ACC08Termination_CloseReview(t *testing.T) {
	s, cl := newStubStore(), &stubClients{}
	setClosePeriod(s, "2026-03", domain.PeriodCloseReview)
	r := newRouter(s, &stubAuthZ{}, cl)
	id := createApprovedPrepayment(t, s, r, 1200, 12, "2026-01")

	rr := doReq(r, http.MethodPost, "/v1/prepayments/"+id+"/terminate",
		domain.TerminatePrepaymentRequest{
			Reason:                "policy replaced",
			FinalBalanceTreatment: domain.TerminationTreatmentRecognizeRemaining,
			FiscalPeriod:          "2026-03",
		}, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("terminate: %d %s", rr.Code, rr.Body.String())
	}
	assertCloseJournalMeta(t, cl, domain.GLCloseJournalPrepaymentTermination, false)
}

func TestCloseJournalMeta_HardClosedRefusedBeforeGL(t *testing.T) {
	s, cl := newStubStore(), &stubClients{}
	setClosePeriod(s, "2026-01", domain.PeriodHardClosed)
	r := newRouter(s, &stubAuthZ{}, cl)
	id := createApprovedAccrual(t, s, r, 900, 3, "2026-01")

	rr := doReq(r, http.MethodPost, "/v1/accruals/"+id+"/recognize",
		domain.RunAccrualRecognitionRequest{FiscalPeriod: "2026-01"}, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("hard-closed recognition must be refused before GL, got %d: %s", rr.Code, rr.Body.String())
	}
	if cl.metaCalls != 0 {
		t.Fatalf("GL must not be called for a hard-closed period, got %d posting(s)", cl.metaCalls)
	}
}
