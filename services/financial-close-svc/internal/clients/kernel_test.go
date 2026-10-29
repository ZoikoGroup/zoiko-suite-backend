package clients_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/financial-close-svc/internal/clients"
	"zoiko.io/financial-close-svc/internal/domain"
)

// The kernel client is driven against a stub general-ledger-svc, because what matters is the request it makes (which entity and
// period, which action, which identity) and how it reads the answer — in particular that "could not ask" is never an empty result.

type fakeKernelLedger struct {
	seals   []map[string]string
	headers []http.Header
	// answers by action; the default is 200
	status map[string]int
	// existing marks a SEAL as a repeat
	existing bool
	evidence string
	evStatus int
}

func (f *fakeKernelLedger) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/period-seals", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]string
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.seals = append(f.seals, b)
		f.headers = append(f.headers, r.Header.Clone())
		st := http.StatusOK
		if s, ok := f.status[b["action"]]; ok {
			st = s
		}
		w.WriteHeader(st)
		_ = json.NewEncoder(w).Encode(map[string]any{"existing": f.existing})
	})
	mux.HandleFunc("/v1/close-evidence", func(w http.ResponseWriter, r *http.Request) {
		f.headers = append(f.headers, r.Header.Clone())
		f.seals = append(f.seals, map[string]string{"entity": r.URL.Query().Get("legal_entity_id"), "period": r.URL.Query().Get("fiscal_period"), "require": r.URL.Query().Get("require")})
		if f.evStatus != 0 {
			w.WriteHeader(f.evStatus)
			return
		}
		_, _ = w.Write([]byte(f.evidence))
	})
	return mux
}

func kernelClient(f *fakeKernelLedger) (*clients.Clients, func()) {
	srv := httptest.NewServer(f.handler())
	c := clients.New("http://unused", srv.URL, "http://unused", "http://unused", "http://unused", "http://unused", "http://unused", "http://unused", zap.NewNop())
	return c, srv.Close
}

func TestKernelCloseEvidenceIsReadAsTheKernelSaysIt(t *testing.T) {
	f := &fakeKernelLedger{evidence: `{"legal_entity_id":"le-1","fiscal_period":"2026-03","ready":false,"blocking_issues":["STAT: revaluation_not_run: 1"]}`}
	c, stop := kernelClient(f)
	defer stop()
	ev, err := c.GetKernelCloseEvidence(context.Background(), "t1", "controller", "le-1", "2026-03", []string{"AR_GL", "AP_GL"})
	if err != nil || ev.Ready || len(ev.BlockingIssues) != 1 {
		t.Fatalf("%+v %v", ev, err)
	}
	if f.seals[0]["entity"] != "le-1" || f.seals[0]["period"] != "2026-03" || f.seals[0]["require"] != "AR_GL,AP_GL" {
		t.Fatalf("%v", f.seals[0])
	}
	h := f.headers[0]
	if h.Get("X-Tenant-Id") != "t1" || h.Get("X-Principal-Id") != "controller" || h.Get("X-Legal-Entity-Id") != "le-1" {
		t.Fatalf("the caller's identity and scope are forwarded: %v", h)
	}
}

func TestKernelCloseEvidenceThatCannotBeReadIsAnError(t *testing.T) {
	for _, st := range []int{http.StatusForbidden, http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusNotImplemented} {
		f := &fakeKernelLedger{evStatus: st}
		c, stop := kernelClient(f)
		ev, err := c.GetKernelCloseEvidence(context.Background(), "t1", "p", "le-1", "2026-03", nil)
		stop()
		if err == nil || ev != nil || !errors.Is(err, domain.ErrGLServiceUnavailable) {
			t.Fatalf("%d: could not check must be an error, got %+v %v", st, ev, err)
		}
	}
	c := clients.New("http://unused", "http://127.0.0.1:1", "", "", "", "", "", "", zap.NewNop())
	if _, err := c.GetKernelCloseEvidence(context.Background(), "t1", "p", "le-1", "2026-03", nil); !errors.Is(err, domain.ErrGLServiceUnavailable) {
		t.Fatalf("an unreachable kernel: %v", err)
	}
	f := &fakeKernelLedger{evidence: "not json"}
	c, stop := kernelClient(f)
	defer stop()
	if _, err := c.GetKernelCloseEvidence(context.Background(), "t1", "p", "le-1", "2026-03", nil); err == nil {
		t.Fatal("an answer that is not the evidence is an error")
	}
}

func TestKernelSealRequestsAndWhatTheyReport(t *testing.T) {
	f := &fakeKernelLedger{status: map[string]int{"SEAL": http.StatusCreated}}
	c, stop := kernelClient(f)
	defer stop()
	changed, err := c.ApplyKernelPeriodSeal(context.Background(), "t1", "controller", "le-1", "2026-03", "SEAL", "", "")
	if err != nil || !changed {
		t.Fatalf("a new seal: %v %v", changed, err)
	}
	if f.seals[0]["legal_entity_id"] != "le-1" || f.seals[0]["fiscal_period"] != "2026-03" || f.seals[0]["action"] != "SEAL" {
		t.Fatalf("%v", f.seals[0])
	}
	if f.headers[0].Get("Idempotency-Key") == "" || f.headers[0].Get("X-Request-Id") == "" || f.headers[0].Get("X-Correlation-ID") == "" {
		t.Fatal("a write to the kernel carries its envelope")
	}
	f.existing, f.status["SEAL"] = true, http.StatusOK
	if changed, err = c.ApplyKernelPeriodSeal(context.Background(), "t1", "controller", "le-1", "2026-03", "SEAL", "", ""); err != nil || changed {
		t.Fatalf("a repeat changes nothing: %v %v", changed, err)
	}
	f.existing = false

	// a reopen under authority carries the authority and the reason
	if _, err = c.ApplyKernelPeriodSeal(context.Background(), "t1", "cfo", "le-1", "2026-03", "REOPEN", "reopen-request:r-1", "misposting"); err != nil {
		t.Fatal(err)
	}
	last := f.seals[len(f.seals)-1]
	if last["action"] != "REOPEN" || last["authority_ref"] != "reopen-request:r-1" || last["reason"] != "misposting" {
		t.Fatalf("%v", last)
	}
	// a period the kernel never sealed (closed before it kept seals) has nothing to reopen: that is not a failure
	f.status["REOPEN"] = http.StatusConflict
	if changed, err = c.ApplyKernelPeriodSeal(context.Background(), "t1", "cfo", "le-1", "2026-03", "REOPEN", "a", "r"); err != nil || changed {
		t.Fatalf("%v %v", changed, err)
	}
	// a refusal or an outage is an error
	f.status["SEAL"] = http.StatusForbidden
	if _, err = c.ApplyKernelPeriodSeal(context.Background(), "t1", "controller", "le-1", "2026-03", "SEAL", "", ""); err == nil {
		t.Fatal("a refused seal must stop the close")
	}
	f.status["SEAL"] = http.StatusServiceUnavailable
	if _, err = c.ApplyKernelPeriodSeal(context.Background(), "t1", "controller", "le-1", "2026-03", "SEAL", "", ""); err == nil {
		t.Fatal("an outage must stop the close")
	}
}

func TestResealFallsBackToSealForAPeriodClosedBeforeSeals(t *testing.T) {
	f := &fakeKernelLedger{status: map[string]int{"RESEAL": http.StatusConflict, "SEAL": http.StatusCreated}}
	c, stop := kernelClient(f)
	defer stop()
	changed, err := c.ApplyKernelPeriodSeal(context.Background(), "t1", "controller", "le-1", "2026-03", "RESEAL", "", "")
	if err != nil || !changed || len(f.seals) != 2 || f.seals[0]["action"] != "RESEAL" || f.seals[1]["action"] != "SEAL" {
		t.Fatalf("%v %v %v", changed, err, f.seals)
	}
}
