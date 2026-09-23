package handler_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"zoiko.io/notification-svc/internal/domain"
)

// publishTestTemplate walks a template through the full BIZ-03 lifecycle
// (create -> version -> validate -> approve -> publish) and returns its
// template_id, so SendNotification tests can exercise the governed
// template_id/locale path against something actually PUBLISHED.
func publishTestTemplate(t *testing.T, r chi.Router, legalEntityID, locale, content string, variableSchema []string) string {
	t.Helper()
	createRR := doReq(r, http.MethodPost, "/v1/document-templates/", map[string]any{
		"legal_entity_id":  legalEntityID,
		"name":             "Send Test Template",
		"business_purpose": "Used by SendNotification governed-template tests",
	}, "owner-1")
	if createRR.Code != http.StatusCreated {
		t.Fatalf("create template: expected 201 got %d: %s", createRR.Code, createRR.Body.String())
	}
	var tmpl domain.TemplateDefinition
	if err := json.Unmarshal(createRR.Body.Bytes(), &tmpl); err != nil {
		t.Fatalf("decode template: %v", err)
	}

	versionRR := doReq(r, http.MethodPost, "/v1/document-templates/"+tmpl.TemplateID+"/versions", map[string]any{
		"locale": locale, "content": content, "variable_schema": variableSchema,
	}, "owner-1")
	if versionRR.Code != http.StatusCreated {
		t.Fatalf("create version: expected 201 got %d: %s", versionRR.Code, versionRR.Body.String())
	}
	var v domain.TemplateVersion
	if err := json.Unmarshal(versionRR.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode version: %v", err)
	}

	if rr := doReq(r, http.MethodPost, "/v1/document-templates/versions/"+v.VersionID+"/validate", nil, "owner-1"); rr.Code != http.StatusOK {
		t.Fatalf("validate: expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	if rr := doReq(r, http.MethodPost, "/v1/document-templates/versions/"+v.VersionID+"/approve", nil, "approver-1"); rr.Code != http.StatusOK {
		t.Fatalf("approve: expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	if rr := doReq(r, http.MethodPost, "/v1/document-templates/versions/"+v.VersionID+"/publish", nil, "approver-1"); rr.Code != http.StatusOK {
		t.Fatalf("publish: expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	return tmpl.TemplateID
}

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

// ── SendNotification via a governed BIZ-03 template (Wave 3) ────────────────

func TestSendNotification_RendersGovernedTemplate_Returns201(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	templateID := publishTestTemplate(t, r, "le-us", "en-US", "<p>Hello {{.first_name}}</p>", []string{"first_name"})

	rr := doReq(r, http.MethodPost, "/v1/notifications/", map[string]any{
		"recipient_principal_id": "principal-2",
		"legal_entity_id":        "le-us",
		"channel":                "EMAIL",
		"subject":                "Welcome",
		"correlation_id":         "corr-governed-1",
		"template_id":            templateID,
		"locale":                 "en-US",
		"variables":              map[string]string{"first_name": "Ada"},
	}, "principal-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	var n domain.Notification
	if err := json.NewDecoder(rr.Body).Decode(&n); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(n.Body, "Ada") {
		t.Errorf("body should carry the rendered variable: %s", n.Body)
	}
	if n.Subject != "Welcome" {
		t.Errorf("expected the caller-supplied subject to survive, got %q", n.Subject)
	}
}

func TestSendNotification_GovernedTemplateAndBodyConflict_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	templateID := publishTestTemplate(t, r, "le-us", "en-US", "<p>hi</p>", nil)

	rr := doReq(r, http.MethodPost, "/v1/notifications/", map[string]any{
		"recipient_principal_id": "principal-2",
		"legal_entity_id":        "le-us",
		"channel":                "EMAIL",
		"subject":                "Welcome",
		"body":                   "Handwritten body",
		"correlation_id":         "corr-governed-conflict",
		"template_id":            templateID,
		"locale":                 "en-US",
	}, "principal-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSendNotification_TemplateAndTemplateIDConflict_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	templateID := publishTestTemplate(t, r, "le-us", "en-US", "<p>hi</p>", nil)

	rr := doReq(r, http.MethodPost, "/v1/notifications/", map[string]any{
		"recipient_principal_id": "principal-2",
		"legal_entity_id":        "le-us",
		"channel":                "EMAIL",
		"correlation_id":         "corr-governed-both",
		"template":               "approved",
		"template_id":            templateID,
		"locale":                 "en-US",
	}, "principal-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSendNotification_GovernedTemplateMissingLocale_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	templateID := publishTestTemplate(t, r, "le-us", "en-US", "<p>hi</p>", nil)

	rr := doReq(r, http.MethodPost, "/v1/notifications/", map[string]any{
		"recipient_principal_id": "principal-2",
		"legal_entity_id":        "le-us",
		"channel":                "EMAIL",
		"subject":                "Welcome",
		"correlation_id":         "corr-governed-no-locale",
		"template_id":            templateID,
	}, "principal-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSendNotification_GovernedTemplateNotPublished_Returns404(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	templateID := createTestTemplate(t, r, "owner-1") // created but never published

	rr := doReq(r, http.MethodPost, "/v1/notifications/", map[string]any{
		"recipient_principal_id": "principal-2",
		"legal_entity_id":        "le-us",
		"channel":                "EMAIL",
		"subject":                "Welcome",
		"correlation_id":         "corr-governed-unpublished",
		"template_id":            templateID,
		"locale":                 "en-US",
	}, "principal-1")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSendNotification_GovernedTemplateMissingVariables_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	templateID := publishTestTemplate(t, r, "le-us", "en-US", "<p>Hello {{.first_name}}</p>", []string{"first_name"})

	rr := doReq(r, http.MethodPost, "/v1/notifications/", map[string]any{
		"recipient_principal_id": "principal-2",
		"legal_entity_id":        "le-us",
		"channel":                "EMAIL",
		"subject":                "Welcome",
		"correlation_id":         "corr-governed-missing-vars",
		"template_id":            templateID,
		"locale":                 "en-US",
	}, "principal-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

// A template published under a different legal entity than the one this
// notification is being sent under must be refused, not silently used.
func TestSendNotification_GovernedTemplateLegalEntityMismatch_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	templateID := publishTestTemplate(t, r, "le-us", "en-US", "<p>hi</p>", nil)

	rr := doReq(r, http.MethodPost, "/v1/notifications/", map[string]any{
		"recipient_principal_id": "principal-2",
		"legal_entity_id":        "le-uk", // different from the template's le-us
		"channel":                "EMAIL",
		"subject":                "Welcome",
		"correlation_id":         "corr-governed-mismatch",
		"template_id":            templateID,
		"locale":                 "en-US",
	}, "principal-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

// The governed-template fetch must not run before authorization — a caller
// denied NOTIFICATION_SEND for a legal entity must never see that entity's
// template content, not even indirectly through an error message.
func TestSendNotification_GovernedTemplateAuthzDenied_NeverFetchesTemplate(t *testing.T) {
	store := newStubStore()
	setupRouter := newRouter(store, &stubPublisher{}, &stubAuthZ{})
	templateID := publishTestTemplate(t, setupRouter, "le-us", "en-US", "<p>hi</p>", nil)
	store.getPublishedVersionCalls = 0 // reset after setup's own publish flow

	denyingAuthz := &stubAuthZ{err: domain.ErrAuthorizationDenied}
	denyRouter := newRouter(store, &stubPublisher{}, denyingAuthz)
	rr := doReq(denyRouter, http.MethodPost, "/v1/notifications/", map[string]any{
		"recipient_principal_id": "principal-2",
		"legal_entity_id":        "le-us",
		"channel":                "EMAIL",
		"subject":                "Welcome",
		"correlation_id":         "corr-governed-denied",
		"template_id":            templateID,
		"locale":                 "en-US",
	}, "principal-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d: %s", rr.Code, rr.Body.String())
	}
	if store.getPublishedVersionCalls != 0 {
		t.Fatalf("expected GetPublishedVersion never to be called before authorization, got %d calls", store.getPublishedVersionCalls)
	}
}
