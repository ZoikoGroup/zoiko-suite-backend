package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"zoiko.io/general-ledger-svc/internal/certreliance"
)

// ZS-SVC-Z-001 RB-05 / DQC-017: an accounting event that cites a certification is a recorded consumer of it when it is posted,
// and nothing is posted on a certification that is no longer in force. Against real Postgres, through the real posting path.

type recordedCall struct {
	ref                               certreliance.Ref
	tenant, principal, objType, objID string
	entity                            string
}

type fakeCertService struct {
	mu    sync.Mutex
	calls []recordedCall
	errs  map[string]error // by certification reference string; absent means success
}

func (f *fakeCertService) RecordReliance(_ context.Context, tenantID, principalID, _ string, ref certreliance.Ref, obj certreliance.Object) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, recordedCall{ref: ref, tenant: tenantID, principal: principalID, objType: obj.Type, objID: obj.ID, entity: obj.LegalEntityID})
	return f.errs[ref.String()]
}

func (f *fakeCertService) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

const (
	certDQ  = "certification:data-quality-svc:dqn_1"
	certRec = "certification:reconciliation-engine-svc:rcr_2"
)

func (e *kenv) registerCiting(t *testing.T, src string, refs ...string) (int, map[string]any) {
	t.Helper()
	body := eventBody(e.entity, src, "120.00", "100.00", "20.00")
	body["evidence_refs"] = refs
	return e.call("POST", "/v1/accounting-events", e.tenant, "ar-svc", body)
}

func (e *kenv) mustRegisterCiting(t *testing.T, src string, refs ...string) string {
	t.Helper()
	st, ev := e.registerCiting(t, src, refs...)
	if st != 201 {
		t.Fatalf("register %s: %d %v", src, st, ev)
	}
	return ev["accounting_event_id"].(string)
}

func TestKernelCertReliance_PostingRecordsTheEventsReliance(t *testing.T) {
	e := newKEnv(t)
	book, _ := e.postSetup(t)
	cs := &fakeCertService{}
	e.svc.Reliance = cs
	ev := e.mustRegisterCiting(t, "se-cr-1", "doc-1", certDQ, certRec)

	res := e.mustPost(t, ev, book)
	if e.count(t, `SELECT count(*) FROM journal_headers`) != 1 {
		t.Fatal("the event did not post")
	}
	if cs.n() != 2 || cs.calls[0].ref.String() != certDQ || cs.calls[1].ref.String() != certRec {
		t.Fatalf("recorded %+v", cs.calls)
	}
	want := ev + ":" + book + ":PRIMARY"
	for _, c := range cs.calls {
		if c.objType != certreliance.ObjectTypeAccountingPosting || c.objID != want || c.tenant != e.tenant || c.principal != "poster" || c.entity != e.entity {
			t.Fatalf("relying object or identity: %+v (want id %s)", c, want)
		}
	}
	// A replay returns the consequence that exists and records nothing new: the posting was already recorded.
	st, again := e.post(ev, book, nil)
	if st != 200 || again["existing"] != true || again["journal_id"] != res["journal_id"] || cs.n() != 2 {
		t.Fatalf("replay: %d %v, recorder calls %d", st, again, cs.n())
	}
}

func TestKernelCertReliance_EventCitingNoCertificationNeverCallsTheService(t *testing.T) {
	e := newKEnv(t)
	book, _ := e.postSetup(t)
	cs := &fakeCertService{}
	e.svc.Reliance = cs
	ev := e.mustRegisterCiting(t, "se-cr-none", "doc-1", "invoice:INV-1")
	e.mustPost(t, ev, book)
	if cs.n() != 0 {
		t.Fatalf("an event that cites no certification called the service: %+v", cs.calls)
	}
	// And a deployment with nothing wired posts such an event exactly as before.
	e.svc.Reliance = nil
	ev2 := e.registerEvent(t, "se-cr-none-2", "120.00", "100.00", "20.00")
	e.mustPost(t, ev2, book)
}

func TestKernelCertReliance_NothingIsPostedOnACertificationThatCannotBeRecorded(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		status int
		family string
		code   string
		// the recorded execution status, "" when the refusal records none
		exec string
	}{
		"revoked, superseded or stale": {certreliance.ErrNotInForce, 422, "AK-RULE", "CERTIFICATION_NOT_IN_FORCE", "FAILED"},
		"no such certification":        {certreliance.ErrUnknown, 422, "AK-DATA", "CERTIFICATION_UNKNOWN", "QUARANTINED"},
		"not permitted":                {certreliance.ErrDenied, 403, "AK-AUTH", "CERTIFICATION_RELIANCE_DENIED", ""},
		"service unavailable":          {certreliance.ErrUnavailable, 503, "AK-EXT", "CERTIFICATION_SERVICE_UNAVAILABLE", "FAILED"},
		"anything unexpected":          {errors.New("boom"), 503, "AK-EXT", "CERTIFICATION_SERVICE_UNAVAILABLE", "FAILED"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newKEnv(t)
			book, _ := e.postSetup(t)
			cs := &fakeCertService{errs: map[string]error{certDQ: tc.err}}
			e.svc.Reliance = cs
			ev := e.mustRegisterCiting(t, "se-cr-refuse", certDQ)

			st, b := e.post(ev, book, nil)
			if st != tc.status || b["family"] != tc.family || b["code"] != tc.code {
				t.Fatalf("%d %v, want %d %s/%s", st, b, tc.status, tc.family, tc.code)
			}
			if e.count(t, `SELECT count(*) FROM journal_headers`) != 0 || e.count(t, `SELECT count(*) FROM ledger_entries`) != 0 ||
				e.count(t, `SELECT count(*) FROM journal_number_counters`) != 0 {
				t.Fatal("a refused certification left a financial consequence")
			}
			got := e.count(t, `SELECT count(*) FROM posting_executions WHERE accounting_event_id=$1::uuid AND status=$2`, ev, tc.exec)
			if tc.exec != "" && got != 1 {
				t.Fatalf("expected one %s execution, found %d", tc.exec, got)
			}
			if tc.exec == "" && e.count(t, `SELECT count(*) FROM posting_executions WHERE accounting_event_id=$1::uuid`, ev) != 0 {
				t.Fatal("an authorization refusal recorded an execution")
			}
		})
	}
}

// A retried posting records the SAME relying object, not a new one per attempt, so a flaky certification service cannot fill a
// blast radius with phantom postings.
func TestKernelCertReliance_RetriesRecordTheSameObjectAndThenPost(t *testing.T) {
	e := newKEnv(t)
	book, _ := e.postSetup(t)
	cs := &fakeCertService{errs: map[string]error{certDQ: certreliance.ErrUnavailable}}
	e.svc.Reliance = cs
	ev := e.mustRegisterCiting(t, "se-cr-retry", certDQ)

	for i := 0; i < 3; i++ {
		if st, b := e.post(ev, book, nil); st != 503 {
			t.Fatalf("attempt %d: %d %v", i, st, b)
		}
	}
	if cs.n() != 3 {
		t.Fatalf("calls = %d", cs.n())
	}
	ids := map[string]bool{}
	for _, c := range cs.calls {
		ids[c.objID] = true
	}
	if len(ids) != 1 {
		t.Fatalf("three attempts named %d different relying objects: %v", len(ids), ids)
	}
	cs.mu.Lock()
	cs.errs = nil // the service recovers
	cs.mu.Unlock()
	e.mustPost(t, ev, book)
	if e.count(t, `SELECT count(*) FROM journal_headers`) != 1 {
		t.Fatal("the retry did not post")
	}
}

func TestKernelCertReliance_CitingACertificationWithNothingWiredRefusesToPost(t *testing.T) {
	e := newKEnv(t)
	book, _ := e.postSetup(t)
	e.svc.Reliance = nil
	ev := e.mustRegisterCiting(t, "se-cr-unwired", certDQ)
	if st, b := e.post(ev, book, nil); st != 503 || b["code"] != "CERTIFICATION_SERVICE_UNAVAILABLE" {
		t.Fatalf("%d %v", st, b)
	}
	if e.count(t, `SELECT count(*) FROM journal_headers`) != 0 {
		t.Fatal("an event resting on a certification posted with nothing to tell anyone")
	}
}

func TestKernelCertReliance_MalformedReferenceIsRefusedWhenTheEventIsRegistered(t *testing.T) {
	e := newKEnv(t)
	e.postSetup(t)
	for i, bad := range []string{"certification:data-qualty-svc:dqn_1", "certification:data-quality-svc:", "certification:data-quality-svc:../x", "certification:payroll-svc:r1"} {
		st, b := e.registerCiting(t, "se-cr-bad-"+string(rune('a'+i)), "doc-1", bad)
		if st != 422 || b["family"] != "AK-DATA" || b["code"] != "EVENT_INVALID" {
			t.Fatalf("%q: %d %v", bad, st, b)
		}
	}
	if e.count(t, `SELECT count(*) FROM accounting_events WHERE source_event_id LIKE 'se-cr-bad-%'`) != 0 {
		t.Fatal("an event with a malformed certification reference was stored")
	}
}
