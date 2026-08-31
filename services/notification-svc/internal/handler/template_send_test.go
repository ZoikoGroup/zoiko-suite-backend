package handler_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"zoiko.io/notification-svc/internal/domain"
)

func TestSendNotification_RendersTemplate(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})

	rr := doReq(r, http.MethodPost, "/v1/notifications/", map[string]any{
		"recipient_principal_id": "principal-2",
		"legal_entity_id":        "le-us",
		"channel":                "EMAIL",
		"correlation_id":         "corr-tmpl-approved",
		"template":               "approved",
		"variables": map[string]string{
			"organization_name": "Acme Logistics",
			"login_url":         "https://app.example.com/login",
		},
	}, "principal-1")

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}

	var n domain.Notification
	if err := json.NewDecoder(rr.Body).Decode(&n); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if n.Subject == "" {
		t.Error("template should have supplied a subject")
	}
	if !strings.Contains(n.Body, "Acme Logistics") {
		t.Errorf("body should carry the rendered organization name: %s", n.Body)
	}
	if strings.Contains(n.Body, "{{") {
		t.Errorf("body still contains an unrendered placeholder: %s", n.Body)
	}
}

func TestSendNotification_TemplateAndBodyConflict(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})

	rr := doReq(r, http.MethodPost, "/v1/notifications/", map[string]any{
		"recipient_principal_id": "principal-2",
		"legal_entity_id":        "le-us",
		"channel":                "EMAIL",
		"correlation_id":         "corr-tmpl-conflict",
		"template":               "approved",
		"subject":                "Something else entirely",
	}, "principal-1")

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSendNotification_UnknownTemplate(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})

	rr := doReq(r, http.MethodPost, "/v1/notifications/", map[string]any{
		"recipient_principal_id": "principal-2",
		"legal_entity_id":        "le-us",
		"channel":                "EMAIL",
		"correlation_id":         "corr-tmpl-unknown",
		"template":               "welcome_aboard",
	}, "principal-1")

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

// A message missing the organization name is worse than no message at all, so
// it is refused rather than sent blank.
func TestSendNotification_MissingTemplateVariables(t *testing.T) {
	store := newStubStore()
	r := newRouter(store, &stubPublisher{}, &stubAuthZ{})

	rr := doReq(r, http.MethodPost, "/v1/notifications/", map[string]any{
		"recipient_principal_id": "principal-2",
		"legal_entity_id":        "le-us",
		"channel":                "EMAIL",
		"correlation_id":         "corr-tmpl-missing-vars",
		"template":               "approved",
		"variables":              map[string]string{"organization_name": "Acme"},
	}, "principal-1")

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "login_url") {
		t.Errorf("the error should name the missing variable: %s", rr.Body.String())
	}
	if len(store.byID) != 0 {
		t.Errorf("nothing should be recorded for a message that was never rendered, got %d", len(store.byID))
	}
}

// Supplying subject and body directly still works — the template form is additive.
func TestSendNotification_RawSubjectAndBodyStillWork(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})

	rr := doReq(r, http.MethodPost, "/v1/notifications/", map[string]any{
		"recipient_principal_id": "principal-2",
		"legal_entity_id":        "le-us",
		"channel":                "EMAIL",
		"correlation_id":         "corr-tmpl-raw",
		"subject":                "Handwritten subject",
		"body":                   "Handwritten body",
	}, "principal-1")

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}

	var n domain.Notification
	_ = json.NewDecoder(rr.Body).Decode(&n)
	if n.Subject != "Handwritten subject" {
		t.Errorf("expected the supplied subject, got %q", n.Subject)
	}
}
