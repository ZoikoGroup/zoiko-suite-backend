package handler_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"zoiko.io/notification-svc/internal/domain"
)

// publishWithSubject takes a template with a reviewed subject through the full
// lifecycle and returns its template id.
func publishWithSubject(t *testing.T, r chi.Router, subject string, subjectVars []string) string {
	t.Helper()
	rr := doReq(r, http.MethodPost, "/v1/document-templates/", map[string]any{
		"legal_entity_id": "le-us", "name": "Payslip notice", "business_purpose": "subject tests",
	}, "owner-1")
	var tmpl domain.TemplateDefinition
	if err := json.Unmarshal(rr.Body.Bytes(), &tmpl); err != nil || rr.Code != http.StatusCreated {
		t.Fatalf("create template: %d %s", rr.Code, rr.Body.String())
	}
	vr := doReq(r, http.MethodPost, "/v1/document-templates/"+tmpl.TemplateID+"/versions", map[string]any{
		"locale": "en-US", "content": "<p>Hello {{.first_name}}, your payslip for {{.period}} is ready. Net pay: {{.net_pay}}</p>",
		"variable_schema": []string{"first_name", "period", "net_pay"}, "subject": subject, "subject_variables": subjectVars,
	}, "owner-1")
	if vr.Code != http.StatusCreated {
		t.Fatalf("create version: %d %s", vr.Code, vr.Body.String())
	}
	var v domain.TemplateVersion
	_ = json.Unmarshal(vr.Body.Bytes(), &v)
	for _, step := range []struct{ path, who string }{{"validate", "owner-1"}, {"approve", "approver-1"}, {"publish", "approver-1"}} {
		if rr := doReq(r, http.MethodPost, "/v1/document-templates/versions/"+v.VersionID+"/"+step.path, nil, step.who); rr.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", step.path, rr.Code, rr.Body.String())
		}
	}
	return tmpl.TemplateID
}

func governedSend(templateID, correlation string, subject string, vars map[string]string) map[string]any {
	b := map[string]any{
		"recipient_principal_id": "emp-1", "legal_entity_id": "le-us", "channel": "EMAIL", "correlation_id": correlation,
		"template_id": templateID, "locale": "en-US", "variables": vars,
	}
	if subject != "" {
		b["subject"] = subject
	}
	return b
}

var payslipVars = map[string]string{"first_name": "Asha", "period": "October 2026", "net_pay": "4,210.55"}

// F-05: the reviewed subject is what is sent; the sensitive value never reaches it.
func TestSend_GovernedSubjectIsTheReviewedOneAndOnlyThat(t *testing.T) {
	del := &stubDeliverer{delivered: true}
	r := newRouterWith(newStubStore(), &stubPublisher{}, &stubAuthZ{}, del, "tenant-abc")
	id := publishWithSubject(t, r, "Your payslip for {{.period}} is ready", []string{"period"})

	rr := doReq(r, http.MethodPost, "/v1/notifications/", governedSend(id, "corr-subj-1", "", payslipVars), "p-1")
	if rr.Code != http.StatusCreated || del.seen == nil {
		t.Fatalf("want 201 and a delivery, got %d seen=%v: %s", rr.Code, del.seen != nil, rr.Body.String())
	}
	if del.seen.Subject != "Your payslip for October 2026 is ready" {
		t.Errorf("subject = %q", del.seen.Subject)
	}
	if strings.Contains(del.seen.Subject, "4,210.55") {
		t.Error("a salary figure reached the subject")
	}
}

// A version that owns its subject refuses a caller-supplied one: reviewed wording
// cannot be sent under unreviewed text.
func TestSend_GovernedSubjectRefusesACallerSubject(t *testing.T) {
	del := &stubDeliverer{delivered: true}
	r := newRouterWith(newStubStore(), &stubPublisher{}, &stubAuthZ{}, del, "tenant-abc")
	id := publishWithSubject(t, r, "Your payslip for {{.period}} is ready", []string{"period"})

	rr := doReq(r, http.MethodPost, "/v1/notifications/", governedSend(id, "corr-subj-2", "Asha, your net pay is 4,210.55", payslipVars), "p-1")
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "conflicting_content") {
		t.Fatalf("want 400 conflicting_content, got %d: %s", rr.Code, rr.Body.String())
	}
	if del.seen != nil {
		t.Fatal("the provider was called")
	}
}

// NP-08: a value that would break the header is refused, not cleaned and sent.
func TestSend_GovernedSubjectRefusesAHeaderInjectingValue(t *testing.T) {
	del := &stubDeliverer{delivered: true}
	r := newRouterWith(newStubStore(), &stubPublisher{}, &stubAuthZ{}, del, "tenant-abc")
	id := publishWithSubject(t, r, "Your payslip for {{.period}} is ready", []string{"period"})

	vars := map[string]string{"first_name": "Asha", "period": "Oct\r\nBcc: attacker@example.com", "net_pay": "1"}
	rr := doReq(r, http.MethodPost, "/v1/notifications/", governedSend(id, "corr-subj-3", "", vars), "p-1")
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "invalid_subject") {
		t.Fatalf("want 400 invalid_subject, got %d: %s", rr.Code, rr.Body.String())
	}
	if del.seen != nil {
		t.Fatal("the provider was called")
	}
}

// A subject may only use variables its author declared safe for a subject.
func TestCreateVersion_SubjectUsingAnUndeclaredSafeVariableIsRefused(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rr := doReq(r, http.MethodPost, "/v1/document-templates/", map[string]any{
		"legal_entity_id": "le-us", "name": "x", "business_purpose": "y"}, "owner-1")
	var tmpl domain.TemplateDefinition
	_ = json.Unmarshal(rr.Body.Bytes(), &tmpl)
	for name, body := range map[string]map[string]any{
		"net pay is not listed as subject-safe": {"subject": "Net pay {{.net_pay}}", "subject_variables": []string{"period"}},
		"subject-safe list names an unknown":    {"subject": "Hello", "subject_variables": []string{"ghost"}},
		"logic in a subject":                    {"subject": "{{if .period}}x{{end}}", "subject_variables": []string{"period"}},
		"variables without a subject":           {"subject_variables": []string{"period"}},
	} {
		body["locale"], body["content"] = "en-US", "<p>{{.period}} {{.net_pay}}</p>"
		body["variable_schema"] = []string{"period", "net_pay"}
		out := doReq(r, http.MethodPost, "/v1/document-templates/"+tmpl.TemplateID+"/versions", body, "owner-1")
		if out.Code != http.StatusBadRequest || !strings.Contains(out.Body.String(), "invalid_subject") {
			t.Errorf("%s: want 400 invalid_subject, got %d: %s", name, out.Code, out.Body.String())
		}
	}
}

// A version with no subject is the old behaviour: the caller supplies one.
func TestSend_VersionWithoutSubjectStillTakesTheCallersSubject(t *testing.T) {
	del := &stubDeliverer{delivered: true}
	r := newRouterWith(newStubStore(), &stubPublisher{}, &stubAuthZ{}, del, "tenant-abc")
	id := publishTestTemplate(t, r, "le-us", "en-US", "<p>Hi {{.name}}</p>", []string{"name"})

	body := governedSend(id, "corr-subj-5", "Caller subject", map[string]string{"name": "Asha"})
	if rr := doReq(r, http.MethodPost, "/v1/notifications/", body, "p-1"); rr.Code != http.StatusCreated || del.seen == nil || del.seen.Subject != "Caller subject" {
		t.Fatalf("legacy send: %d %s", rr.Code, rr.Body.String())
	}
	noSubject := governedSend(id, "corr-subj-6", "", map[string]string{"name": "Asha"})
	if rr := doReq(r, http.MethodPost, "/v1/notifications/", noSubject, "p-1"); rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "missing_fields") {
		t.Fatalf("want 400 missing_fields, got %d: %s", rr.Code, rr.Body.String())
	}
}

// F-09: a preview needs no real data; the variables not supplied are shown as markers.
func TestPreview_FillsMissingVariablesWithVisibleMarkers(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	id := publishWithSubject(t, r, "Your payslip for {{.period}} is ready", []string{"period"})
	var versionID string
	rr := doReq(r, http.MethodGet, "/v1/document-templates/"+id+"/published?locale=en-US", nil, "owner-1")
	var v domain.TemplateVersion
	if err := json.Unmarshal(rr.Body.Bytes(), &v); err != nil {
		t.Fatalf("published: %v %s", err, rr.Body.String())
	}
	versionID = v.VersionID

	out := doReq(r, http.MethodPost, "/v1/document-templates/versions/"+versionID+"/preview", map[string]any{"variables": map[string]string{}}, "owner-1")
	if out.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", out.Code, out.Body.String())
	}
	var res domain.RenderPreviewResult
	_ = json.Unmarshal(out.Body.Bytes(), &res)
	if !strings.Contains(res.RenderedContent, "[first_name]") || res.RenderedSubject != "Your payslip for [period] is ready" {
		t.Errorf("markers expected, got body=%q subject=%q", res.RenderedContent, res.RenderedSubject)
	}
	if len(res.PlaceholdersUsed) != 3 {
		t.Errorf("placeholders_used = %v", res.PlaceholdersUsed)
	}
}
