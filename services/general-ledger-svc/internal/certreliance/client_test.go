package certreliance

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseRefs_PicksOutCertificationsIgnoresOtherEvidenceRefusesMalformed(t *testing.T) {
	got, err := ParseRefs([]string{
		"doc-123", "invoice:INV-1", "",
		"certification:data-quality-svc:dqn_1",
		"  certification:reconciliation-engine-svc:rcr_2  ",
		"certification:data-quality-svc:dqn_1", // a duplicate
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != (Ref{ServiceDataQuality, "dqn_1"}) || got[1] != (Ref{ServiceReconciliation, "rcr_2"}) {
		t.Fatalf("refs = %+v", got)
	}
	if got, err := ParseRefs([]string{"doc-1", "certificate:something"}); err != nil || len(got) != 0 {
		t.Fatalf("evidence that cites no certification: %+v %v", got, err)
	}
	for name, ref := range map[string]string{
		"unknown service":      "certification:payroll-svc:run1",
		"no run id":            "certification:data-quality-svc:",
		"no separator":         "certification:data-quality-svc",
		"empty":                "certification:",
		"path traversal":       "certification:data-quality-svc:../../admin",
		"slash in run id":      "certification:data-quality-svc:a/b",
		"query in run id":      "certification:data-quality-svc:a?x=1",
		"space in run id":      "certification:data-quality-svc:a b",
		"run id too long":      "certification:data-quality-svc:" + strings.Repeat("a", 129),
		"upper-case service":   "certification:Data-Quality-Svc:dqn_1",
		"a typo is not a skip": "certification:data-qualty-svc:dqn_1",
	} {
		if _, err := ParseRefs([]string{"fine", ref}); !errors.Is(err, ErrInvalidRef) {
			t.Fatalf("%s: err = %v, want ErrInvalidRef", name, err)
		}
	}
	many := make([]string, 0, 25)
	for i := 0; i < 25; i++ {
		many = append(many, "certification:data-quality-svc:r"+strings.Repeat("x", i+1))
	}
	if _, err := ParseRefs(many); !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("too many references: %v", err)
	}
}

type seen struct {
	method, path, tenant, principal, entity, corr string
	body                                          relianceBody
}

func fakeService(t *testing.T, status int) (*httptest.Server, *[]seen) {
	t.Helper()
	var calls []seen
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := seen{method: r.Method, path: r.URL.Path, tenant: r.Header.Get("X-Tenant-Id"), principal: r.Header.Get("X-Principal-Id"),
			entity: r.Header.Get("X-Legal-Entity-Id"), corr: r.Header.Get("X-Correlation-ID")}
		_ = json.NewDecoder(r.Body).Decode(&c.body)
		calls = append(calls, c)
		w.WriteHeader(status)
	}))
	t.Cleanup(s.Close)
	return s, &calls
}

func TestRecordReliance_SendsTheJournalAsTheRelyingObjectToTheIssuingService(t *testing.T) {
	dq, dqCalls := fakeService(t, http.StatusCreated)
	rec, recCalls := fakeService(t, http.StatusOK)
	c := NewClient(dq.URL+"/", rec.URL)
	obj := Object{ID: "jrn_9", LegalEntityID: "ent-1"}

	if err := c.RecordReliance(context.Background(), "t1", "alice", "corr-1", Ref{ServiceDataQuality, "dqn_1"}, obj); err != nil {
		t.Fatal(err)
	}
	if err := c.RecordReliance(context.Background(), "t1", "alice", "corr-1", Ref{ServiceReconciliation, "rcr_2"}, obj); err != nil {
		t.Fatal(err)
	}
	if len(*dqCalls) != 1 || len(*recCalls) != 1 {
		t.Fatalf("calls: dq %d, reconciliation %d", len(*dqCalls), len(*recCalls))
	}
	d, r := (*dqCalls)[0], (*recCalls)[0]
	if d.method != "POST" || d.path != "/v1/data-quality/runs/dqn_1/certification/reliances" ||
		r.path != "/v1/reconciliation/runs/rcr_2/certification/reliances" {
		t.Fatalf("paths: %q, %q", d.path, r.path)
	}
	for _, c := range []seen{d, r} {
		if c.tenant != "t1" || c.principal != "alice" || c.entity != "ent-1" || c.corr != "corr-1" {
			t.Fatalf("identity headers: %+v", c)
		}
		if c.body.RelyingService != "general-ledger-svc" || c.body.RelyingObjectType != "journal" || c.body.RelyingObjectID != "jrn_9" || c.body.Purpose == "" {
			t.Fatalf("body: %+v", c.body)
		}
	}
}

func TestRecordReliance_MapsEveryAnswerAndFailsClosed(t *testing.T) {
	for status, want := range map[int]error{
		http.StatusConflict:            ErrNotInForce,
		http.StatusNotFound:            ErrUnknown,
		http.StatusForbidden:           ErrDenied,
		http.StatusUnauthorized:        ErrDenied,
		http.StatusBadRequest:          ErrUnavailable,
		http.StatusInternalServerError: ErrUnavailable,
		http.StatusBadGateway:          ErrUnavailable,
		http.StatusTeapot:              ErrUnavailable,
	} {
		s, _ := fakeService(t, status)
		err := NewClient(s.URL, s.URL).RecordReliance(context.Background(), "t", "p", "c", Ref{ServiceDataQuality, "dqn_1"}, Object{ID: "j"})
		if !errors.Is(err, want) {
			t.Fatalf("HTTP %d: err = %v, want %v", status, err, want)
		}
	}
	// Unreachable, and not configured: both fail closed, never succeed.
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	if err := NewClient(dead.URL, dead.URL).RecordReliance(context.Background(), "t", "p", "c", Ref{ServiceDataQuality, "dqn_1"}, Object{ID: "j"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unreachable: %v", err)
	}
	if err := NewClient("", "").RecordReliance(context.Background(), "t", "p", "c", Ref{ServiceReconciliation, "rcr_1"}, Object{ID: "j"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("not configured: %v", err)
	}
	// A hand-built reference cannot smuggle a path or an unknown service through the client either.
	s, calls := fakeService(t, http.StatusOK)
	c := NewClient(s.URL, s.URL)
	for _, ref := range []Ref{{ServiceDataQuality, "../x"}, {ServiceDataQuality, "a/b"}, {"payroll-svc", "r1"}, {ServiceDataQuality, ""}} {
		if err := c.RecordReliance(context.Background(), "t", "p", "c", ref, Object{ID: "j"}); err == nil {
			t.Fatalf("%+v was sent", ref)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("%d requests were made for invalid references", len(*calls))
	}
}

func TestRecordReliance_KernelPostingsAreRecordedAsAccountingPostings(t *testing.T) {
	s, calls := fakeService(t, http.StatusOK)
	c := NewClient(s.URL, s.URL)
	obj := Object{Type: ObjectTypeAccountingPosting, ID: "ev-1:book-1:PRIMARY", LegalEntityID: "ent-1"}
	if err := c.RecordReliance(context.Background(), "t1", "poster", "corr-1", Ref{ServiceDataQuality, "dqn_1"}, obj); err != nil {
		t.Fatal(err)
	}
	got := (*calls)[0].body
	if got.RelyingObjectType != "accounting-posting" || got.RelyingObjectID != "ev-1:book-1:PRIMARY" || got.RelyingService != "general-ledger-svc" ||
		!strings.Contains(got.Purpose, "accounting event") {
		t.Fatalf("body: %+v", got)
	}
	// An empty type is a journal, and an unknown type is never sent.
	if err := c.RecordReliance(context.Background(), "t1", "p", "c", Ref{ServiceDataQuality, "dqn_1"}, Object{ID: "j"}); err != nil || (*calls)[1].body.RelyingObjectType != "journal" {
		t.Fatalf("default type: %v %+v", err, (*calls)[1].body)
	}
	if err := c.RecordReliance(context.Background(), "t1", "p", "c", Ref{ServiceDataQuality, "dqn_1"}, Object{Type: "something-else", ID: "x"}); !errors.Is(err, ErrInvalidRef) || len(*calls) != 2 {
		t.Fatalf("unknown type: %v (calls %d)", err, len(*calls))
	}
}
