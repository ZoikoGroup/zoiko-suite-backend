package handler_test

import (
	"errors"
	"net/http"
	"testing"
)

// The first attempt marks the submission before calling the provider, and a
// failure to mark refuses the attempt rather than making it unmarked.
func TestSend_MarksSubmissionBeforeTheProvider(t *testing.T) {
	s := newStubStore()
	del := &stubDeliverer{delivered: true, reason: "250"}
	r := newRouterWith(s, &stubPublisher{}, &stubAuthZ{}, del, "tenant-abc")
	body := sendBody("corr-sub-1", "", "emp-1")
	body["channel"] = "EMAIL"
	if rr := doReq(r, http.MethodPost, "/v1/notifications/", body, "p-1"); rr.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(s.submitted) != 1 {
		t.Fatalf("BeginSubmission calls = %d, want 1", len(s.submitted))
	}

	s2 := newStubStore()
	s2.beginSubmissionErr = errors.New("db down")
	del2 := &stubDeliverer{delivered: true}
	r2 := newRouterWith(s2, &stubPublisher{}, &stubAuthZ{}, del2, "tenant-abc")
	body2 := sendBody("corr-sub-2", "", "emp-1")
	body2["channel"] = "EMAIL"
	if rr := doReq(r2, http.MethodPost, "/v1/notifications/", body2, "p-1"); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("mark failure: want 503, got %d", rr.Code)
	}
	if del2.seen != nil {
		t.Fatal("the provider was called although the submission could not be marked")
	}
}

// IN_APP is delivered by being recorded; there is no provider to be ambiguous
// about, so it is never marked.
func TestSend_InAppIsNotMarked(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	if rr := doReq(r, http.MethodPost, "/v1/notifications/", sendBody("corr-inapp", "", "emp-1"), "p-1"); rr.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d", rr.Code)
	}
	if len(s.submitted) != 0 {
		t.Fatalf("IN_APP was marked as a provider submission: %v", s.submitted)
	}
}
