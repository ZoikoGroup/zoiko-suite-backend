package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"zoiko.io/financial-close-svc/internal/domain"
)

func governanceErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var decoded map[string]string
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, body)
	}
	return decoded["error_code"]
}

func stubControlRun(t *testing.T, s *stubStore, entity, subledger, book, status, runBy string, at time.Time) domain.SubledgerControlRun {
	t.Helper()
	run := domain.SubledgerControlRun{
		ControlRunID:           "run-" + entity + "-" + subledger + "-" + status + "-" + at.Format("150405.000000000"),
		TenantID:               testTenantID,
		LegalEntityID:          entity,
		FiscalPeriod:           "2026-10",
		Subledger:              subledger,
		BookID:                 book,
		ControlAccountCode:     "CTRL-" + subledger,
		SubledgerTotalAmount:   1200,
		GLControlBalanceAmount: 1000,
		DifferenceAmount:       200,
		Status:                 status,
		RunAt:                  at,
		RunByPrincipalID:       runBy,
	}
	if status == "MATCHED" {
		run.GLControlBalanceAmount = run.SubledgerTotalAmount
		run.DifferenceAmount = 0
	}
	if err := s.CreateControlRun(context.Background(), &run, "corr-1", runBy); err != nil {
		t.Fatalf("create control run: %v", err)
	}
	return run
}

func stubExceptionFor(t *testing.T, s *stubStore, run domain.SubledgerControlRun) domain.ControlException {
	t.Helper()
	exc, err := s.GetControlExceptionByRun(context.Background(), run.ControlRunID)
	if err != nil {
		t.Fatalf("get exception for run %s: %v", run.ControlRunID, err)
	}
	return *exc
}

func stubRequirement(s *stubStore, id, entity, subledger, book string) *domain.CloseRequirement {
	cr := &domain.CloseRequirement{
		RequirementID: id, TenantID: testTenantID, LegalEntityID: entity,
		Kind: domain.CloseRequirementSubledgerControl, Subledger: subledger, BookID: book,
		Reason: "test control", CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "config-holder",
	}
	s.closeRequirements = append(s.closeRequirements, cr)
	return cr
}

// A run that lands EXCEPTION gets a resolution workflow a person can actually
// work: assign an owner, record the resolution, then certify the run.
func TestSubledgerControlGovernance_HappyPath(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubAuthZ{}, &stubClients{})
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	run := stubControlRun(t, s, "le-1", "AR", "", "EXCEPTION", "preparer", at)
	exc := stubExceptionFor(t, s, run)

	// Every read route sees the run and its OPEN exception.
	getRun := doReq(r, http.MethodGet, "/v1/subledger-control/runs/"+run.ControlRunID, nil, "viewer")
	if getRun.Code != http.StatusOK {
		t.Fatalf("get run: %d %s", getRun.Code, getRun.Body.String())
	}
	var gotRun domain.SubledgerControlRun
	if err := json.Unmarshal(getRun.Body.Bytes(), &gotRun); err != nil || gotRun.Status != "EXCEPTION" {
		t.Fatalf("get run body: %v %s", err, getRun.Body.String())
	}
	getExc := doReq(r, http.MethodGet, "/v1/subledger-control/exceptions/"+exc.ControlExceptionID, nil, "viewer")
	if getExc.Code != http.StatusOK {
		t.Fatalf("get exception: %d %s", getExc.Code, getExc.Body.String())
	}
	list := doReq(r, http.MethodGet, "/v1/subledger-control/exceptions/?legal_entity_id=le-1", nil, "viewer")
	if list.Code != http.StatusOK {
		t.Fatalf("list exceptions: %d %s", list.Code, list.Body.String())
	}
	var open []domain.ControlException
	if err := json.Unmarshal(list.Body.Bytes(), &open); err != nil || len(open) != 1 || open[0].Status != "OPEN" {
		t.Fatalf("open exceptions: %v %s", err, list.Body.String())
	}

	assign := doReq(r, http.MethodPost, "/v1/subledger-control/exceptions/"+exc.ControlExceptionID+"/assign",
		map[string]string{"assigned_to_principal_id": "owner-1"}, "resolver")
	if assign.Code != http.StatusOK {
		t.Fatalf("assign: %d %s", assign.Code, assign.Body.String())
	}
	var assigned domain.ControlException
	if err := json.Unmarshal(assign.Body.Bytes(), &assigned); err != nil ||
		assigned.Status != "ASSIGNED" || assigned.AssignedToPrincipalID != "owner-1" || assigned.AssignedAt == nil {
		t.Fatalf("assigned exception: %v %s", err, assign.Body.String())
	}

	resolve := doReq(r, http.MethodPost, "/v1/subledger-control/exceptions/"+exc.ControlExceptionID+"/resolve",
		map[string]string{"resolution_note": "AR ledger corrected and re-tied"}, "resolver")
	if resolve.Code != http.StatusOK {
		t.Fatalf("resolve: %d %s", resolve.Code, resolve.Body.String())
	}
	var resolved domain.ControlException
	if err := json.Unmarshal(resolve.Body.Bytes(), &resolved); err != nil ||
		resolved.Status != "RESOLVED" || resolved.ResolvedByPrincipalID != "resolver" ||
		resolved.ResolutionNote != "AR ledger corrected and re-tied" || resolved.ResolvedAt == nil {
		t.Fatalf("resolved exception: %v %s", err, resolve.Body.String())
	}

	// ExplainVariance carries the run's arithmetic and the full trail.
	variance := doReq(r, http.MethodGet, "/v1/subledger-control/runs/"+run.ControlRunID+"/variance", nil, "viewer")
	if variance.Code != http.StatusOK {
		t.Fatalf("explain variance: %d %s", variance.Code, variance.Body.String())
	}
	var explanation domain.VarianceExplanation
	if err := json.Unmarshal(variance.Body.Bytes(), &explanation); err != nil {
		t.Fatalf("decode variance: %v", err)
	}
	if explanation.ControlRun == nil || explanation.ControlRun.ControlRunID != run.ControlRunID {
		t.Fatalf("variance run: %+v", explanation.ControlRun)
	}
	if explanation.DifferenceBreakdown.DifferenceAmount != 200 ||
		explanation.DifferenceBreakdown.WithinTolerance ||
		explanation.DifferenceBreakdown.ToleranceAmount != 0.01 {
		t.Fatalf("variance breakdown: %+v", explanation.DifferenceBreakdown)
	}
	if explanation.ControlException == nil || explanation.ControlException.Status != "RESOLVED" ||
		explanation.ControlException.ResolvedByPrincipalID != "resolver" {
		t.Fatalf("variance exception trail: %+v", explanation.ControlException)
	}

	certify := doReq(r, http.MethodPost, "/v1/subledger-control/runs/"+run.ControlRunID+"/certify",
		map[string]string{"note": "reviewed"}, "certifier")
	if certify.Code != http.StatusOK {
		t.Fatalf("certify: %d %s", certify.Code, certify.Body.String())
	}
	var certified domain.SubledgerControlRun
	if err := json.Unmarshal(certify.Body.Bytes(), &certified); err != nil ||
		certified.CertificationStatus == nil || *certified.CertificationStatus != "CERTIFIED" ||
		certified.CertifiedByPrincipalID == nil || *certified.CertifiedByPrincipalID != "certifier" {
		t.Fatalf("certified run: %v %s", err, certify.Body.String())
	}

	// The run's own immutable finding was never overwritten by certification.
	stored := s.findControlRun(run.ControlRunID)
	if stored == nil || stored.Status != "EXCEPTION" {
		t.Fatalf("certification overwrote the finding: %+v", stored)
	}
}

func TestSubledgerControlGovernance_MissingFields(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubAuthZ{}, &stubClients{})
	run := stubControlRun(t, s, "le-1", "AR", "", "EXCEPTION", "preparer", time.Now().UTC())
	exc := stubExceptionFor(t, s, run)

	cases := []struct {
		name, path string
		body       any
	}{
		{"assign without owner", "/v1/subledger-control/exceptions/" + exc.ControlExceptionID + "/assign", map[string]string{}},
		{"resolve without note", "/v1/subledger-control/exceptions/" + exc.ControlExceptionID + "/resolve", map[string]string{}},
		{"resolve with blank note", "/v1/subledger-control/exceptions/" + exc.ControlExceptionID + "/resolve", map[string]string{"resolution_note": "   "}},
		{"reject without reason", "/v1/subledger-control/runs/" + run.ControlRunID + "/reject", map[string]string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rr := doReq(r, http.MethodPost, c.path, c.body, "actor")
			if rr.Code != http.StatusBadRequest || governanceErrorCode(t, rr.Body.Bytes()) != "missing_fields" {
				t.Fatalf("got %d %s; want 400 missing_fields", rr.Code, rr.Body.String())
			}
		})
	}

	if rr := doReq(r, http.MethodGet, "/v1/subledger-control/exceptions/", nil, "viewer"); rr.Code != http.StatusBadRequest {
		t.Fatalf("list exceptions without entity: got %d, want 400", rr.Code)
	}
	if rr := doReq(r, http.MethodGet, "/v1/subledger-control/blocking", nil, "viewer"); rr.Code != http.StatusBadRequest {
		t.Fatalf("blocking controls without entity: got %d, want 400", rr.Code)
	}
}

func TestCertifyControl_RefusesUnresolvedException(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubAuthZ{}, &stubClients{})
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	run := stubControlRun(t, s, "le-1", "AR", "", "EXCEPTION", "preparer", at)
	exc := stubExceptionFor(t, s, run)

	for _, status := range []string{"OPEN", "ASSIGNED"} {
		if status == "ASSIGNED" {
			if _, err := s.AssignException(context.Background(), exc.ControlExceptionID, "owner-1", "resolver", ""); err != nil {
				t.Fatalf("assign: %v", err)
			}
		}
		rr := doReq(r, http.MethodPost, "/v1/subledger-control/runs/"+run.ControlRunID+"/certify",
			map[string]string{"note": "looks fine"}, "certifier")
		if rr.Code != http.StatusUnprocessableEntity ||
			governanceErrorCode(t, rr.Body.Bytes()) != "control_has_unresolved_exception" {
			t.Fatalf("%s exception: got %d %s; want 422 control_has_unresolved_exception", status, rr.Code, rr.Body.String())
		}
	}
}

func TestCertifyControl_RefusesBothKindsOfSelfCertification(t *testing.T) {
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

	t.Run("the principal who ran the control", func(t *testing.T) {
		s := newStubStore()
		r := newRouter(s, &stubAuthZ{}, &stubClients{})
		run := stubControlRun(t, s, "le-1", "AR", "", "EXCEPTION", "principal-1", at)
		rr := doReq(r, http.MethodPost, "/v1/subledger-control/runs/"+run.ControlRunID+"/certify",
			map[string]string{"note": "self"}, "principal-1")
		if rr.Code != http.StatusForbidden ||
			governanceErrorCode(t, rr.Body.Bytes()) != "control_self_certification" {
			t.Fatalf("got %d %s; want 403 control_self_certification", rr.Code, rr.Body.String())
		}
	})

	t.Run("the principal who resolved the exception", func(t *testing.T) {
		s := newStubStore()
		r := newRouter(s, &stubAuthZ{}, &stubClients{})
		run := stubControlRun(t, s, "le-1", "AR", "", "EXCEPTION", "preparer", at)
		exc := stubExceptionFor(t, s, run)
		if _, err := s.RecordResolution(context.Background(), exc.ControlExceptionID,
			"fixed", "principal-1", ""); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		rr := doReq(r, http.MethodPost, "/v1/subledger-control/runs/"+run.ControlRunID+"/certify",
			map[string]string{"note": "self"}, "principal-1")
		if rr.Code != http.StatusForbidden ||
			governanceErrorCode(t, rr.Body.Bytes()) != "control_self_certification" {
			t.Fatalf("got %d %s; want 403 control_self_certification", rr.Code, rr.Body.String())
		}
	})
}

// RejectControl is the explicit "this control did not pass" path. A control
// that cannot be fixed must still be failed, so the exception need not be
// resolved first.
func TestRejectControl_DoesNotRequireResolvedException(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubAuthZ{}, &stubClients{})
	run := stubControlRun(t, s, "le-1", "AR", "", "EXCEPTION", "preparer", time.Now().UTC())
	exc := stubExceptionFor(t, s, run)

	rr := doReq(r, http.MethodPost, "/v1/subledger-control/runs/"+run.ControlRunID+"/reject",
		map[string]string{"reason": "source data cannot be reconstructed"}, "certifier")
	if rr.Code != http.StatusOK {
		t.Fatalf("reject: %d %s", rr.Code, rr.Body.String())
	}
	var rejected domain.SubledgerControlRun
	if err := json.Unmarshal(rr.Body.Bytes(), &rejected); err != nil ||
		rejected.CertificationStatus == nil || *rejected.CertificationStatus != "FAILED" ||
		rejected.CertificationNote != "source data cannot be reconstructed" {
		t.Fatalf("rejected run: %v %s", err, rr.Body.String())
	}
	stored := s.findControlException(exc.ControlExceptionID)
	if stored == nil || stored.Status != "OPEN" {
		t.Fatalf("reject must not resolve the exception: %+v", stored)
	}
}

func TestCertifyControl_RefusesSecondCertification(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubAuthZ{}, &stubClients{})
	run := stubControlRun(t, s, "le-1", "AR", "", "EXCEPTION", "preparer", time.Now().UTC())
	exc := stubExceptionFor(t, s, run)
	if _, err := s.RecordResolution(context.Background(), exc.ControlExceptionID, "fixed", "resolver", ""); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	first := doReq(r, http.MethodPost, "/v1/subledger-control/runs/"+run.ControlRunID+"/certify", map[string]string{}, "certifier")
	if first.Code != http.StatusOK {
		t.Fatalf("first certify: %d %s", first.Code, first.Body.String())
	}
	second := doReq(r, http.MethodPost, "/v1/subledger-control/runs/"+run.ControlRunID+"/reject",
		map[string]string{"reason": "changed my mind"}, "another")
	if second.Code != http.StatusConflict ||
		governanceErrorCode(t, second.Body.Bytes()) != "control_already_certified" {
		t.Fatalf("second decision: got %d %s; want 409 control_already_certified", second.Code, second.Body.String())
	}
}

// The resolve and certify permissions are distinct authorities — a holder of
// RUN/VIEW alone cannot work or sign off an exception.
func TestSubledgerControlGovernance_UsesResolveAndCertifyActions(t *testing.T) {
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

	s := newStubStore()
	run := stubControlRun(t, s, "le-1", "AR", "", "EXCEPTION", "preparer", at)
	exc := stubExceptionFor(t, s, run)
	resolveAuthz := &actionAuthZ{deny: map[string]bool{"SUBLEDGER_CONTROL_RESOLVE": true}}
	r := checklistRouterWithClients(s, resolveAuthz, &stubClients{})
	assign := doReq(r, http.MethodPost, "/v1/subledger-control/exceptions/"+exc.ControlExceptionID+"/assign",
		map[string]string{"assigned_to_principal_id": "owner-1"}, "resolver")
	if assign.Code != http.StatusForbidden {
		t.Fatalf("assign without SUBLEDGER_CONTROL_RESOLVE: got %d, want 403", assign.Code)
	}
	if len(resolveAuthz.asked) != 1 || resolveAuthz.asked[0] != "SUBLEDGER_CONTROL_RESOLVE" {
		t.Fatalf("assign asked %v", resolveAuthz.asked)
	}
	if s.findControlException(exc.ControlExceptionID).Status != "OPEN" {
		t.Fatal("a denied assignment changed the exception")
	}

	s2 := newStubStore()
	run2 := stubControlRun(t, s2, "le-1", "AR", "", "EXCEPTION", "preparer", at)
	certifyAuthz := &actionAuthZ{deny: map[string]bool{"SUBLEDGER_CONTROL_CERTIFY": true}}
	r2 := checklistRouterWithClients(s2, certifyAuthz, &stubClients{})
	certify := doReq(r2, http.MethodPost, "/v1/subledger-control/runs/"+run2.ControlRunID+"/reject",
		map[string]string{"reason": "cannot fix"}, "certifier")
	if certify.Code != http.StatusForbidden {
		t.Fatalf("reject without SUBLEDGER_CONTROL_CERTIFY: got %d, want 403", certify.Code)
	}
	if len(certifyAuthz.asked) != 1 || certifyAuthz.asked[0] != "SUBLEDGER_CONTROL_CERTIFY" {
		t.Fatalf("reject asked %v", certifyAuthz.asked)
	}
	if s2.findControlRun(run2.ControlRunID).CertificationStatus != nil {
		t.Fatal("a denied rejection certified the run")
	}
}

// The exact A1 finding: a PERIOD_CLOSE_CONFIG holder could remove a checklist
// control while its latest run was an unresolved EXCEPTION.
func TestRemoveCloseRequirement_RefusedWhileLatestRunUnresolved(t *testing.T) {
	s := newStubStore()
	r := checklistRouter(s, &actionAuthZ{})
	req := stubRequirement(s, "req-inventory", "le-1", "INVENTORY_VALUE", "")
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	run := stubControlRun(t, s, "le-1", "INVENTORY_VALUE", "", "EXCEPTION", "preparer", at)
	exc := stubExceptionFor(t, s, run)

	remove := func() *httptest.ResponseRecorder {
		return doReq(r, http.MethodPost, "/v1/close/requirements/"+req.RequirementID+"/remove",
			domain.RemoveCloseRequirementRequest{Reason: "no longer material"}, "config-holder")
	}
	refused := remove()
	if refused.Code != http.StatusUnprocessableEntity ||
		governanceErrorCode(t, refused.Body.Bytes()) != "control_has_unresolved_exception" {
		t.Fatalf("got %d %s; want 422 control_has_unresolved_exception", refused.Code, refused.Body.String())
	}
	if stored := s.closeRequirements[0]; stored.RemovedAt != nil {
		t.Fatal("the unresolved requirement was removed despite the refusal")
	}

	if _, err := s.RecordResolution(context.Background(), exc.ControlExceptionID, "counted and corrected", "resolver", ""); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	ok := remove()
	if ok.Code != http.StatusOK {
		t.Fatalf("remove after resolution: %d %s", ok.Code, ok.Body.String())
	}
}

// HasUnresolvedException must follow only the latest run: an old exception
// that was never formally resolved cannot block a control that has since been
// re-run and matched.
func TestRemoveCloseRequirement_SupersededExceptionDoesNotBlock(t *testing.T) {
	s := newStubStore()
	r := checklistRouter(s, &actionAuthZ{})
	req := stubRequirement(s, "req-inventory", "le-1", "INVENTORY_VALUE", "")
	early := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	stubControlRun(t, s, "le-1", "INVENTORY_VALUE", "", "EXCEPTION", "preparer", early)
	stubControlRun(t, s, "le-1", "INVENTORY_VALUE", "", "MATCHED", "preparer", early.Add(time.Hour))

	rr := doReq(r, http.MethodPost, "/v1/close/requirements/"+req.RequirementID+"/remove",
		domain.RemoveCloseRequirementRequest{Reason: "control retired"}, "config-holder")
	if rr.Code != http.StatusOK {
		t.Fatalf("a superseded exception blocked removal: %d %s", rr.Code, rr.Body.String())
	}
}

func TestGetCloseBlockingControls_ReusesReadinessClassification(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubAuthZ{}, &stubClients{})
	req := stubRequirement(s, "req-inventory", "le-1", "INVENTORY_VALUE", "")

	find := func(body []byte, subledger string) *domain.CloseBlockingControl {
		t.Helper()
		var controls []domain.CloseBlockingControl
		if err := json.Unmarshal(body, &controls); err != nil {
			t.Fatalf("decode blocking controls: %v (%s)", err, body)
		}
		for i := range controls {
			if controls[i].Subledger == subledger {
				return &controls[i]
			}
		}
		return nil
	}

	// No run at all: both the checklist control and the AR/AP baseline block.
	rr := doReq(r, http.MethodGet, "/v1/subledger-control/blocking?legal_entity_id=le-1", nil, "viewer")
	if rr.Code != http.StatusOK {
		t.Fatalf("blocking controls: %d %s", rr.Code, rr.Body.String())
	}
	missing := find(rr.Body.Bytes(), "INVENTORY_VALUE")
	if missing == nil || missing.BlockingReason != domain.CloseBlockingReasonNoRun || missing.RequirementID != req.RequirementID {
		t.Fatalf("missing control: %+v (%s)", missing, rr.Body.String())
	}
	if ar := find(rr.Body.Bytes(), "AR"); ar == nil || !ar.Baseline || ar.BlockingReason != domain.CloseBlockingReasonNoRun {
		t.Fatalf("AR baseline: %+v", ar)
	}

	// An unresolved EXCEPTION blocks and carries its workflow row.
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	run := stubControlRun(t, s, "le-1", "INVENTORY_VALUE", "", "EXCEPTION", "preparer", at)
	exc := stubExceptionFor(t, s, run)
	rr = doReq(r, http.MethodGet, "/v1/subledger-control/blocking?legal_entity_id=le-1", nil, "viewer")
	unresolved := find(rr.Body.Bytes(), "INVENTORY_VALUE")
	if unresolved == nil || unresolved.BlockingReason != domain.CloseBlockingReasonUnresolvedException ||
		unresolved.ControlException == nil || unresolved.ControlException.ControlExceptionID != exc.ControlExceptionID {
		t.Fatalf("unresolved exception: %+v (%s)", unresolved, rr.Body.String())
	}

	// Resolving it removes it from the blocking read.
	if _, err := s.RecordResolution(context.Background(), exc.ControlExceptionID, "corrected", "resolver", ""); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	rr = doReq(r, http.MethodGet, "/v1/subledger-control/blocking?legal_entity_id=le-1", nil, "viewer")
	if find(rr.Body.Bytes(), "INVENTORY_VALUE") != nil {
		t.Fatalf("a resolved exception still blocks: %s", rr.Body.String())
	}

	// A run that predates the reopen is stale and blocks even though it
	// matched — the same requiredControlIssues rule the close runs.
	s2 := newStubStore()
	r2 := newRouter(s2, &stubAuthZ{}, &stubClients{})
	stubRequirement(s2, "req-inventory-2", "le-1", "INVENTORY_VALUE", "")
	reopenedAt := at.Add(2 * time.Hour)
	s2.periods["fp-oct"] = &domain.FiscalPeriod{
		FiscalPeriodID: "fp-oct", LegalEntityID: "le-1", PeriodName: "2026-10",
		CloseStatus: domain.PeriodAuthorizedReopen, ReopenedAt: &reopenedAt,
	}
	stubControlRun(t, s2, "le-1", "INVENTORY_VALUE", "", "MATCHED", "preparer", at)

	rr = doReq(r2, http.MethodGet, "/v1/subledger-control/blocking?legal_entity_id=le-1&fiscal_period=2026-10", nil, "viewer")
	stale := find(rr.Body.Bytes(), "INVENTORY_VALUE")
	if stale == nil || stale.BlockingReason != domain.CloseBlockingReasonNotReperformed {
		t.Fatalf("stale control: %+v (%s)", stale, rr.Body.String())
	}
}
