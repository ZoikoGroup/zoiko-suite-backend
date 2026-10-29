package handler_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/financial-close-svc/internal/domain"
	"zoiko.io/financial-close-svc/internal/handler"
	"zoiko.io/financial-close-svc/internal/middleware"
)

// The accounting kernel's part in a period close: its evidence is a readiness input and its seal makes the close hold against any
// later posting. Both are off by default, and neither can be skipped silently when on.

type stubKernel struct {
	evidence    *domain.KernelCloseEvidence
	evidenceErr error
	sealErr     error
	// sealChanged is what a SEAL or RESEAL reports; a REOPEN reports sealReopenChanged.
	calls     []sealCall
	asked     []string
	askedWith [][]string
}

type sealCall struct{ Action, Authority, Reason, Period string }

func (k *stubKernel) GetKernelCloseEvidence(_ context.Context, _, _, entity, period string, require []string) (*domain.KernelCloseEvidence, error) {
	k.asked = append(k.asked, entity+"/"+period)
	k.askedWith = append(k.askedWith, require)
	if k.evidenceErr != nil {
		return nil, k.evidenceErr
	}
	return k.evidence, nil
}

func (k *stubKernel) ApplyKernelPeriodSeal(_ context.Context, _, _, _, period, action, authority, reason string) (bool, error) {
	k.calls = append(k.calls, sealCall{action, authority, reason, period})
	if k.sealErr != nil && action != "REOPEN" {
		return false, k.sealErr
	}
	return true, nil
}

func kernelRouter(s *stubStore, cl *stubClients, k *stubKernel, gate, seal bool, require []string) chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	h := handler.New(s, &stubAuthZ{}, cl, testSigningKey, zap.NewNop()).SetKernelClose(k, gate, seal, require)
	handler.RegisterRoutes(r, h)
	return r
}

func reviewPeriod(s *stubStore, id string) {
	newOpenPeriod(s, id)
	s.periods[id].CloseStatus = domain.PeriodCloseReview
}

func (k *stubKernel) actions() string {
	var out []string
	for _, c := range k.calls {
		out = append(out, c.Action)
	}
	return strings.Join(out, ",")
}

func TestKernelIsNotConsultedUnlessSwitchedOn(t *testing.T) {
	s := newStubStore()
	reviewPeriod(s, "fp-1")
	k := &stubKernel{evidence: &domain.KernelCloseEvidence{Ready: false, BlockingIssues: []string{"x"}}}
	r := kernelRouter(s, closeClients(), k, false, false, nil)
	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-1/hard-close", nil, "controller-1")
	if rr.Code != http.StatusOK || s.periods["fp-1"].CloseStatus != domain.PeriodHardClosed {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	if len(k.asked) != 0 || len(k.calls) != 0 {
		t.Fatalf("a kernel that is not switched on is never called: %v %v", k.asked, k.calls)
	}
}

func TestKernelEvidenceBlocksTheClose(t *testing.T) {
	s := newStubStore()
	reviewPeriod(s, "fp-1")
	k := &stubKernel{evidence: &domain.KernelCloseEvidence{Ready: false, BlockingIssues: []string{"STAT: reconciliation_cases_open: 1 break(s)", "STAT: revaluation_not_run: 2 open foreign monetary item(s)"}}}
	r := kernelRouter(s, closeClients(), k, true, false, []string{"AR_GL", "AP_GL"})
	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-1/hard-close", nil, "controller-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	var resp domain.ReadinessCheckResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	joined := strings.Join(resp.BlockingIssues, "|")
	if resp.IsReady || !strings.Contains(joined, "accounting_kernel: STAT: reconciliation_cases_open") || !strings.Contains(joined, "revaluation_not_run") {
		t.Fatalf("%v", resp)
	}
	if s.periods["fp-1"].CloseStatus != domain.PeriodCloseReview {
		t.Fatal("a blocked close does not move the period")
	}
	if len(k.askedWith) != 1 || strings.Join(k.askedWith[0], ",") != "AR_GL,AP_GL" || k.asked[0] != "le-1/2026-10" {
		t.Fatalf("the kernel is asked about this entity and period, and which reconciliations are required: %v %v", k.asked, k.askedWith)
	}
	// once the kernel is satisfied the close goes through
	k.evidence = &domain.KernelCloseEvidence{Ready: true, BlockingIssues: []string{}}
	if rr = doReq(r, http.MethodPost, "/v1/close/periods/fp-1/hard-close", nil, "controller-1"); rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
}

func TestAnUnreadableKernelFailsTheCloseClosed(t *testing.T) {
	s := newStubStore()
	reviewPeriod(s, "fp-1")
	k := &stubKernel{evidenceErr: errors.New("connection refused")}
	rr := doReq(kernelRouter(s, closeClients(), k, true, false, nil), http.MethodPost, "/v1/close/periods/fp-1/hard-close", nil, "controller-1")
	if rr.Code == http.StatusOK || s.periods["fp-1"].CloseStatus != domain.PeriodCloseReview {
		t.Fatalf("could not check must not read as ready: %d %s", rr.Code, rr.Body.String())
	}
	// a kernel that says not ready, with no reason given, still blocks
	k = &stubKernel{evidence: &domain.KernelCloseEvidence{Ready: false}}
	rr = doReq(kernelRouter(s, closeClients(), k, true, false, nil), http.MethodPost, "/v1/close/periods/fp-1/hard-close", nil, "controller-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
}

func TestTheCloseSealsThePeriodAndAReopenLiftsTheSeal(t *testing.T) {
	s := newStubStore()
	newOpenPeriod(s, "fp-1")
	k := &stubKernel{}
	cl := closeClients()
	r := kernelRouter(s, cl, k, false, true, nil)
	for _, step := range []string{"soft-close", "close-review", "hard-close"} {
		if rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-1/"+step, nil, "controller-1"); rr.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", step, rr.Code, rr.Body.String())
		}
	}
	if k.actions() != "SEAL" || k.calls[0].Period != "2026-10" {
		t.Fatalf("closing seals the period once: %v", k.calls)
	}
	if s.periods["fp-1"].CloseStatus != domain.PeriodHardClosed {
		t.Fatal("closed")
	}

	rr := doReq(r, http.MethodPost, "/v1/close/periods/fp-1/reopen-requests", domain.RequestReopenRequest{Reason: "misposting found", ReopenUntil: time.Now().UTC().Add(48 * time.Hour)}, "accountant-1")
	var req domain.ReopenRequest
	_ = json.Unmarshal(rr.Body.Bytes(), &req)
	if rr = doReq(r, http.MethodPost, "/v1/close/reopen-requests/"+req.RequestID+"/approve", nil, "cfo-1"); rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	last := k.calls[len(k.calls)-1]
	if last.Action != "REOPEN" || last.Authority != "reopen-request:"+req.RequestID || last.Reason != "misposting found" && last.Reason != "" {
		t.Fatalf("the reopen lifts the seal under the request's own authority: %+v", last)
	}

	at := s.periods["fp-1"].ReopenedAt.Add(time.Hour)
	bothMatchedAt(s, at)
	cl.bankRecon = []domain.BankAccountReconStatus{certifiedAt("acct-barclays", "2026-10-30", at)}
	if rr = doReq(r, http.MethodPost, "/v1/close/periods/fp-1/reclose", nil, "controller-1"); rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	if k.actions() != "SEAL,REOPEN,RESEAL" {
		t.Fatalf("seal, reopen, re-seal: %s", k.actions())
	}
}

func TestACloseThatCannotSealDoesNotClose(t *testing.T) {
	s := newStubStore()
	reviewPeriod(s, "fp-1")
	k := &stubKernel{sealErr: errors.New("kernel unavailable")}
	rr := doReq(kernelRouter(s, closeClients(), k, false, true, nil), http.MethodPost, "/v1/close/periods/fp-1/hard-close", nil, "controller-1")
	if rr.Code != http.StatusServiceUnavailable || s.periods["fp-1"].CloseStatus != domain.PeriodCloseReview {
		t.Fatalf("a period that cannot be sealed must not close: %d %s", rr.Code, rr.Body.String())
	}
}

func TestASealTakenByAFailedCloseIsLiftedAgain(t *testing.T) {
	s := newStubStore()
	reviewPeriod(s, "fp-1")
	cl := closeClients()
	cl.uploadErr = errors.New("vault down") // the close fails after the seal was taken
	k := &stubKernel{}
	rr := doReq(kernelRouter(s, cl, k, false, true, nil), http.MethodPost, "/v1/close/periods/fp-1/hard-close", nil, "controller-1")
	if rr.Code == http.StatusOK || s.periods["fp-1"].CloseStatus != domain.PeriodCloseReview {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	if k.actions() != "SEAL,REOPEN" || !strings.HasPrefix(k.calls[1].Authority, "close-aborted:") {
		t.Fatalf("the seal of a close that did not complete is lifted, and says why: %v", k.calls)
	}
}
