package handler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zoiko.io/notification-svc/internal/domain"
)

func TestSendNotification_RendersTemplate(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthZ{})

	rr := doReq(r, http.MethodPost, "/v1/notifications/", map[string]any{
		"recipient_principal_id": "principal-2",
		"legal_entity_id":        "le-us",
		"channel":                "EMAIL",
		"correlation_id":         "corr-tmpl-approved",
		"purpose_context":        "TEST_PURPOSE",
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
	r := newRouter(newStubStore(), &stubAuthZ{})

	rr := doReq(r, http.MethodPost, "/v1/notifications/", map[string]any{
		"recipient_principal_id": "principal-2",
		"legal_entity_id":        "le-us",
		"channel":                "EMAIL",
		"correlation_id":         "corr-tmpl-conflict",
		"purpose_context":        "TEST_PURPOSE",
		"template":               "approved",
		"subject":                "Something else entirely",
	}, "principal-1")

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSendNotification_UnknownTemplate(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthZ{})

	rr := doReq(r, http.MethodPost, "/v1/notifications/", map[string]any{
		"recipient_principal_id": "principal-2",
		"legal_entity_id":        "le-us",
		"channel":                "EMAIL",
		"correlation_id":         "corr-tmpl-unknown",
		"purpose_context":        "TEST_PURPOSE",
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
	r := newRouter(store, &stubAuthZ{})

	rr := doReq(r, http.MethodPost, "/v1/notifications/", map[string]any{
		"recipient_principal_id": "principal-2",
		"legal_entity_id":        "le-us",
		"channel":                "EMAIL",
		"correlation_id":         "corr-tmpl-missing-vars",
		"purpose_context":        "TEST_PURPOSE",
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
	r := newRouter(newStubStore(), &stubAuthZ{})

	rr := doReq(r, http.MethodPost, "/v1/notifications/", map[string]any{
		"recipient_principal_id": "principal-2",
		"legal_entity_id":        "le-us",
		"channel":                "EMAIL",
		"correlation_id":         "corr-tmpl-raw",
		"purpose_context":        "TEST_PURPOSE",
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

// The catalogue endpoint fails closed.
//
// It used to not. The handler's own comment said "the envelope middleware ahead
// of it refuses an unattributed request", and in write-strict mode — the
// default — the middleware parses and REPORTS a read's envelope and then admits
// it. A bare curl with no headers at all returned 200 and the whole catalogue
// against the running service. The one route documented as authenticated was
// the one route that was not.
func TestListTemplates_RequiresIdentity(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthZ{})

	// No principal: the router factory installs a tenant, so this isolates the
	// principal check.
	req := httptest.NewRequest(http.MethodGet, "/v1/notifications/templates", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated catalogue read = %d, want 401 (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestListTemplates_RequiresTenant(t *testing.T) {
	// tenantID "" means the factory installs none.
	r := newRouterWith(newStubStore(), &stubAuthZ{},
		&stubDeliverer{delivered: true}, "")

	req := httptest.NewRequest(http.MethodGet, "/v1/notifications/templates", nil)
	req.Header.Set("X-Principal-Id", "principal-1")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("catalogue read with no tenant = %d, want 401 (body: %s)", rr.Code, rr.Body.String())
	}
}
