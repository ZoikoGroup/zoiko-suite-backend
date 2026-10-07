package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/handler"
	"zoiko.io/notification-svc/internal/middleware"
	"zoiko.io/notification-svc/internal/retry"
)

// stubIntents is an in-memory intent registry that applies the REAL domain rules, so
// handler tests exercise the same validation the database-backed store does.
type stubIntents struct {
	intents  map[string]*domain.CommunicationIntent
	versions map[string]*domain.IntentVersion
	seq      int
}

func newStubIntents() *stubIntents {
	return &stubIntents{intents: map[string]*domain.CommunicationIntent{}, versions: map[string]*domain.IntentVersion{}}
}

func (s *stubIntents) CreateIntent(_ context.Context, p domain.CreateIntentParams) (*domain.CommunicationIntent, error) {
	if err := domain.ValidateIntentKey(p.IntentKey); err != nil {
		return nil, err
	}
	for _, i := range s.intents {
		if i.IntentKey == p.IntentKey {
			return nil, domain.ErrIntentKeyTaken
		}
	}
	s.seq++
	i := &domain.CommunicationIntent{IntentID: fmt.Sprintf("intent-%d", s.seq), TenantID: "tenant-abc", LegalEntityID: p.LegalEntityID,
		IntentKey: p.IntentKey, DisplayName: p.DisplayName, DomainOwner: p.DomainOwner, Status: "ACTIVE", CreatedByPrincipalID: p.CreatedByPrincipalID, CreatedAt: time.Now().UTC()}
	s.intents[i.IntentID] = i
	return i, nil
}

func (s *stubIntents) GetIntent(_ context.Context, id string) (*domain.CommunicationIntent, error) {
	if i, ok := s.intents[id]; ok {
		return i, nil
	}
	return nil, domain.ErrIntentNotFound
}

func (s *stubIntents) CreateIntentVersion(_ context.Context, p domain.CreateIntentVersionParams) (*domain.IntentVersion, error) {
	i, ok := s.intents[p.IntentID]
	if !ok {
		return nil, domain.ErrIntentNotFound
	}
	if i.Status == "RETIRED" {
		return nil, domain.ErrIntentRetired
	}
	p.VariableContract = domain.NormalizeContract(p.VariableContract)
	if err := domain.ValidateIntentVersion(p); err != nil {
		return nil, err
	}
	n := 1
	for _, v := range s.versions {
		if v.IntentID == p.IntentID {
			n++
		}
	}
	s.seq++
	v := &domain.IntentVersion{VersionID: fmt.Sprintf("iv-%d", s.seq), IntentID: p.IntentID, TenantID: "tenant-abc", LegalEntityID: i.LegalEntityID,
		VersionNumber: n, PurposeClass: p.PurposeClass, EvidenceClass: p.EvidenceClass, AllowedChannels: p.AllowedChannels,
		MarketingAllowed: p.MarketingAllowed, RecordRequirement: p.RecordRequirement, VariableContract: p.VariableContract,
		Status: domain.IntentVersionDraft, CreatedByPrincipalID: p.CreatedByPrincipalID, CreatedAt: time.Now().UTC()}
	s.versions[v.VersionID] = v
	return v, nil
}

func (s *stubIntents) GetIntentVersion(_ context.Context, id string) (*domain.IntentVersion, error) {
	if v, ok := s.versions[id]; ok {
		return v, nil
	}
	return nil, domain.ErrIntentVersionNotFound
}

func (s *stubIntents) ValidateIntentVersion(_ context.Context, id string) (*domain.IntentVersion, error) {
	v, ok := s.versions[id]
	if !ok {
		return nil, domain.ErrIntentVersionNotFound
	}
	if v.Status != domain.IntentVersionDraft {
		return nil, domain.ErrIntentVersionNotDraft
	}
	now := time.Now().UTC()
	v.Status, v.ValidatedAt = domain.IntentVersionReview, &now
	return v, nil
}

func (s *stubIntents) ApproveIntentVersion(_ context.Context, p domain.ApproveIntentVersionParams) (*domain.IntentVersion, error) {
	v, ok := s.versions[p.VersionID]
	if !ok {
		return nil, domain.ErrIntentVersionNotFound
	}
	if v.Status != domain.IntentVersionReview {
		return nil, domain.ErrIntentVersionNotReview
	}
	if v.CreatedByPrincipalID == p.ApprovedByPrincipalID {
		return nil, domain.ErrIntentVersionSelfApproval
	}
	now := time.Now().UTC()
	by := p.ApprovedByPrincipalID
	v.Status, v.ApprovedAt, v.ApprovedByPrincipalID = domain.IntentVersionApproved, &now, &by
	return v, nil
}

func (s *stubIntents) PublishIntentVersion(_ context.Context, p domain.PublishIntentVersionParams) (*domain.IntentVersion, error) {
	v, ok := s.versions[p.VersionID]
	if !ok {
		return nil, domain.ErrIntentVersionNotFound
	}
	if v.Status != domain.IntentVersionApproved {
		return nil, domain.ErrIntentVersionNotApproved
	}
	now := time.Now().UTC()
	eff := now
	if p.EffectiveFrom != nil {
		if p.EffectiveFrom.Before(now) {
			return nil, domain.ErrIntentEffectiveFromInvalid
		}
		eff = *p.EffectiveFrom
	}
	v.Status, v.PublishedAt, v.EffectiveFrom = domain.IntentVersionPublished, &now, &eff
	return v, nil
}

func (s *stubIntents) RetireIntent(_ context.Context, id, _ string) (*domain.CommunicationIntent, error) {
	i, ok := s.intents[id]
	if !ok {
		return nil, domain.ErrIntentNotFound
	}
	if i.Status == "RETIRED" {
		return nil, domain.ErrIntentRetired
	}
	now := time.Now().UTC()
	i.Status, i.RetiredAt = "RETIRED", &now
	return i, nil
}

func (s *stubIntents) EffectiveIntentVersion(_ context.Context, id string, at, knownAt time.Time) (*domain.IntentVersion, error) {
	i, ok := s.intents[id]
	if !ok {
		return nil, domain.ErrIntentNotFound
	}
	var best *domain.IntentVersion
	for _, v := range s.versions {
		if v.IntentID != id || v.PublishedAt == nil || v.PublishedAt.After(knownAt) || v.EffectiveFrom.After(at) {
			continue
		}
		if best == nil || v.EffectiveFrom.After(*best.EffectiveFrom) {
			best = v
		}
	}
	if best == nil || (i.RetiredAt != nil && !i.RetiredAt.After(at) && !i.RetiredAt.After(knownAt)) {
		return nil, domain.ErrIntentNotEffective
	}
	return best, nil
}

func intentRouter(t *testing.T, ints handler.IntentStore, del handler.Deliverer, authz *stubAuthZ) (chi.Router, *stubStore) {
	t.Helper()
	s := newStubStore()
	s.events = &stubPublisher{}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc")))
		})
	})
	if authz == nil {
		authz = &stubAuthZ{}
	}
	handler.RegisterRoutes(r, handler.New(handler.Deps{Store: s, AuthZ: authz, Deliverer: del, Recipient: &stubResolver{email: "r@example.com"},
		Intents: ints, RetryPolicy: retry.DefaultPolicy, Log: zap.NewNop()}))
	return r, s
}

func decodeInto[T any](t *testing.T, body []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decode %q: %v", string(body), err)
	}
	return v
}

// publishedIntent drives the whole API lifecycle and returns the intent id.
func publishedIntent(t *testing.T, r chi.Router, key string, mutate func(map[string]any)) string {
	t.Helper()
	rr := doReq(r, http.MethodPost, "/v1/communication-intents/", map[string]any{
		"legal_entity_id": "le-us", "intent_key": key, "display_name": "Payslip", "domain_owner": "Payroll"}, "owner-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create intent: %d %s", rr.Code, rr.Body.String())
	}
	intent := decodeInto[domain.CommunicationIntent](t, rr.Body.Bytes())
	body := map[string]any{
		"purpose_class": "T0", "evidence_class": "E2", "allowed_channels": []string{"EMAIL", "IN_APP"},
		"variable_contract": map[string]any{
			"first_name": map[string]any{"type": "STRING", "required": true, "sensitivity": "S1"},
			"period":     map[string]any{"type": "STRING", "required": true, "sensitivity": "S0", "max_length": 30},
			"net_pay":    map[string]any{"type": "NUMBER", "required": true, "sensitivity": "S3"},
		},
	}
	if mutate != nil {
		mutate(body)
	}
	vr := doReq(r, http.MethodPost, "/v1/communication-intents/"+intent.IntentID+"/versions", body, "maker")
	if vr.Code != http.StatusCreated {
		t.Fatalf("create version: %d %s", vr.Code, vr.Body.String())
	}
	v := decodeInto[domain.IntentVersion](t, vr.Body.Bytes())
	for _, step := range []struct{ path, who string }{{"validate", "maker"}, {"approve", "checker"}, {"publish", "checker"}} {
		if out := doReq(r, http.MethodPost, "/v1/communication-intents/versions/"+v.VersionID+"/"+step.path, nil, step.who); out.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", step.path, out.Code, out.Body.String())
		}
	}
	return intent.IntentID
}

func TestIntentAPI_RegistryIsUnavailableWithoutAStore(t *testing.T) {
	r, _ := intentRouter(t, nil, &stubDeliverer{delivered: true}, nil)
	rr := doReq(r, http.MethodPost, "/v1/communication-intents/", map[string]any{"legal_entity_id": "le-us", "intent_key": "a.b", "display_name": "x", "domain_owner": "y"}, "owner-1")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rr.Code)
	}
}

func TestIntentAPI_LifecycleErrorsAndAuthorization(t *testing.T) {
	r, _ := intentRouter(t, newStubIntents(), &stubDeliverer{delivered: true}, nil)
	id := publishedIntent(t, r, "payroll.payslip_available", nil)

	// Required fields, duplicate key, bad key.
	if rr := doReq(r, http.MethodPost, "/v1/communication-intents/", map[string]any{"intent_key": "x.y"}, "owner-1"); rr.Code != http.StatusBadRequest {
		t.Errorf("missing fields: %d", rr.Code)
	}
	if rr := doReq(r, http.MethodPost, "/v1/communication-intents/", map[string]any{"legal_entity_id": "le-us", "intent_key": "payroll.payslip_available", "display_name": "x", "domain_owner": "y"}, "owner-1"); rr.Code != http.StatusConflict {
		t.Errorf("duplicate key: %d", rr.Code)
	}
	if rr := doReq(r, http.MethodPost, "/v1/communication-intents/", map[string]any{"legal_entity_id": "le-us", "intent_key": "Bad Key", "display_name": "x", "domain_owner": "y"}, "owner-1"); rr.Code != http.StatusBadRequest {
		t.Errorf("bad key: %d", rr.Code)
	}
	// An invalid contract is a 400 before it is stored.
	rr := doReq(r, http.MethodPost, "/v1/communication-intents/"+id+"/versions", map[string]any{"purpose_class": "S0", "evidence_class": "E1",
		"allowed_channels": []string{"EMAIL"}, "marketing_allowed": true}, "maker")
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "invalid_intent") {
		t.Errorf("marketing on a security purpose: %d %s", rr.Code, rr.Body.String())
	}
	// Maker-checker through the API.
	vr := doReq(r, http.MethodPost, "/v1/communication-intents/"+id+"/versions", map[string]any{"purpose_class": "T0", "evidence_class": "E1",
		"allowed_channels": []string{"EMAIL"}}, "maker")
	v := decodeInto[domain.IntentVersion](t, vr.Body.Bytes())
	doReq(r, http.MethodPost, "/v1/communication-intents/versions/"+v.VersionID+"/validate", nil, "maker")
	if out := doReq(r, http.MethodPost, "/v1/communication-intents/versions/"+v.VersionID+"/approve", nil, "maker"); out.Code != http.StatusForbidden {
		t.Errorf("self approval: want 403, got %d", out.Code)
	}
	if out := doReq(r, http.MethodPost, "/v1/communication-intents/versions/nope/approve", nil, "checker"); out.Code != http.StatusNotFound {
		t.Errorf("unknown version: %d", out.Code)
	}
	if out := doReq(r, http.MethodGet, "/v1/communication-intents/nope", nil, "owner-1"); out.Code != http.StatusNotFound {
		t.Errorf("unknown intent: %d", out.Code)
	}
	// The publish date cannot be in the past.
	doReq(r, http.MethodPost, "/v1/communication-intents/versions/"+v.VersionID+"/approve", nil, "checker")
	past := time.Now().Add(-time.Hour)
	if out := doReq(r, http.MethodPost, "/v1/communication-intents/versions/"+v.VersionID+"/publish", map[string]any{"effective_from": past}, "checker"); out.Code != http.StatusBadRequest {
		t.Errorf("backdated publish: %d %s", out.Code, out.Body.String())
	}
	// No principal: 401.
	if out := doReq(r, http.MethodGet, "/v1/communication-intents/"+id, nil, ""); out.Code != http.StatusUnauthorized {
		t.Errorf("no principal: %d", out.Code)
	}
}

func TestIntentAPI_AuthorizationUsesTheTemplateRoles(t *testing.T) {
	authz := &stubAuthZ{err: domain.ErrAuthorizationDenied}
	r, _ := intentRouter(t, newStubIntents(), &stubDeliverer{delivered: true}, authz)
	rr := doReq(r, http.MethodPost, "/v1/communication-intents/", map[string]any{"legal_entity_id": "le-us", "intent_key": "a.b", "display_name": "x", "domain_owner": "y"}, "owner-1")
	if rr.Code != http.StatusForbidden || len(authz.calls) != 1 || authz.calls[0] != "TEMPLATE_MANAGE" {
		t.Fatalf("want 403 via TEMPLATE_MANAGE, got %d calls=%v", rr.Code, authz.calls)
	}
}

func TestIntentAPI_EffectiveResolution(t *testing.T) {
	r, _ := intentRouter(t, newStubIntents(), &stubDeliverer{delivered: true}, nil)
	id := publishedIntent(t, r, "payroll.payslip_available", nil)

	rr := doReq(r, http.MethodGet, "/v1/communication-intents/"+id+"/effective", nil, "owner-1")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"purpose_class":"T0"`) {
		t.Fatalf("effective now: %d %s", rr.Code, rr.Body.String())
	}
	before := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	if out := doReq(r, http.MethodGet, "/v1/communication-intents/"+id+"/effective?at="+before, nil, "owner-1"); out.Code != http.StatusConflict {
		t.Errorf("before anything was in force: want 409 intent_not_effective, got %d %s", out.Code, out.Body.String())
	}
	if out := doReq(r, http.MethodGet, "/v1/communication-intents/"+id+"/effective?at=yesterday", nil, "owner-1"); out.Code != http.StatusBadRequest {
		t.Errorf("bad timestamp: %d", out.Code)
	}
}

// boundTemplate creates a template bound to the intent, with the given content and
// variables, and takes it to PUBLISHED.
func boundTemplate(t *testing.T, r chi.Router, intentID string, schema []string, content string) string {
	t.Helper()
	cr := doReq(r, http.MethodPost, "/v1/document-templates/", map[string]any{"legal_entity_id": "le-us", "name": "Payslip", "business_purpose": "notice", "intent_id": intentID}, "owner-1")
	if cr.Code != http.StatusCreated {
		t.Fatalf("create bound template: %d %s", cr.Code, cr.Body.String())
	}
	tmpl := decodeInto[domain.TemplateDefinition](t, cr.Body.Bytes())
	vr := doReq(r, http.MethodPost, "/v1/document-templates/"+tmpl.TemplateID+"/versions", map[string]any{"locale": "en-US", "content": content, "variable_schema": schema,
		"subject": "Your payslip for {{.period}} is ready", "subject_variables": []string{"period"}}, "owner-1")
	if vr.Code != http.StatusCreated {
		t.Fatalf("create version: %d %s", vr.Code, vr.Body.String())
	}
	v := decodeInto[domain.TemplateVersion](t, vr.Body.Bytes())
	for _, step := range []struct{ path, who string }{{"validate", "owner-1"}, {"approve", "approver-1"}, {"publish", "approver-1"}} {
		if out := doReq(r, http.MethodPost, "/v1/document-templates/versions/"+v.VersionID+"/"+step.path, nil, step.who); out.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", step.path, out.Code, out.Body.String())
		}
	}
	return tmpl.TemplateID
}

func intentSend(templateID, corr, channel string, vars map[string]string) map[string]any {
	return map[string]any{"recipient_principal_id": "emp-1", "legal_entity_id": "le-us", "channel": channel, "correlation_id": corr,
		"template_id": templateID, "locale": "en-US", "variables": vars}
}

var payslipSendVars = map[string]string{"first_name": "Asha", "period": "October 2026", "net_pay": "4210.55"}

const payslipBody = "<p>Hello {{.first_name}}, your payslip for {{.period}} is ready. Net pay {{.net_pay}}</p>"

func TestSend_BoundTemplateIsSentUnderTheIntentAndPinnedToIt(t *testing.T) {
	del := &stubDeliverer{delivered: true}
	r, _ := intentRouter(t, newStubIntents(), del, nil)
	intentID := publishedIntent(t, r, "payroll.payslip_available", nil)
	tmpl := boundTemplate(t, r, intentID, []string{"first_name", "period", "net_pay"}, payslipBody)

	rr := doReq(r, http.MethodPost, "/v1/notifications/", intentSend(tmpl, "corr-int-1", "EMAIL", payslipSendVars), "p-1")
	if rr.Code != http.StatusCreated || del.seen == nil {
		t.Fatalf("want 201 and a delivery, got %d: %s", rr.Code, rr.Body.String())
	}
	n := decodeInto[domain.Notification](t, rr.Body.Bytes())
	if n.CommunicationClass != "T0" {
		t.Errorf("the purpose class comes from the intent, got %q", n.CommunicationClass)
	}
	if n.IntentVersionID == "" || !strings.HasPrefix(n.IntentVersionID, "iv-") {
		t.Errorf("the message must be pinned to the exact intent version, got %q", n.IntentVersionID)
	}
	if del.seen.Subject != "Your payslip for October 2026 is ready" {
		t.Errorf("subject = %q", del.seen.Subject)
	}
}

func TestSend_BoundTemplateRefusals(t *testing.T) {
	cases := []struct {
		name    string
		channel string
		vars    map[string]string
		extra   map[string]any
		want    int
		code    string
	}{
		{"channel the intent does not allow", "WEBHOOK", payslipSendVars, nil, http.StatusUnprocessableEntity, "no_compliant_channel"},
		{"a different purpose class chosen by the sender", "EMAIL", payslipSendVars, map[string]any{"communication_class": "S0"}, http.StatusBadRequest, "communication_class_conflict"},
		{"a number that is not a number", "EMAIL", map[string]string{"first_name": "A", "period": "P", "net_pay": "4,210.55"}, nil, http.StatusBadRequest, "template_variable_invalid"},
		{"a value longer than the contract allows", "EMAIL", map[string]string{"first_name": "A", "period": strings.Repeat("x", 31), "net_pay": "1"}, nil, http.StatusBadRequest, "template_variable_invalid"},
		{"a required variable missing", "EMAIL", map[string]string{"first_name": "A", "period": "P"}, nil, http.StatusBadRequest, "template_variable_invalid"},
	}
	for _, c := range cases {
		del := &stubDeliverer{delivered: true}
		r, _ := intentRouter(t, newStubIntents(), del, nil)
		intentID := publishedIntent(t, r, "payroll.payslip_available", func(b map[string]any) { b["allowed_channels"] = []string{"EMAIL", "IN_APP"} })
		tmpl := boundTemplate(t, r, intentID, []string{"first_name", "period", "net_pay"}, payslipBody)
		body := intentSend(tmpl, "corr-ref-"+c.code, c.channel, c.vars)
		for k, v := range c.extra {
			body[k] = v
		}
		rr := doReq(r, http.MethodPost, "/v1/notifications/", body, "p-1")
		if rr.Code != c.want || !strings.Contains(rr.Body.String(), c.code) {
			t.Errorf("%s: want %d %s, got %d: %s", c.name, c.want, c.code, rr.Code, rr.Body.String())
		}
		if del.seen != nil {
			t.Errorf("%s: the provider was called", c.name)
		}
	}
}

// Marketing and lifecycle intents are not sendable on the direct path.
func TestSend_IntentWithAPurposeTheDirectPathDoesNotOfferIsRefused(t *testing.T) {
	del := &stubDeliverer{delivered: true}
	r, _ := intentRouter(t, newStubIntents(), del, nil)
	intentID := publishedIntent(t, r, "marketing.offer", func(b map[string]any) { b["purpose_class"], b["marketing_allowed"] = "M1", true })
	tmpl := boundTemplate(t, r, intentID, []string{"first_name", "period", "net_pay"}, payslipBody)
	rr := doReq(r, http.MethodPost, "/v1/notifications/", intentSend(tmpl, "corr-m1", "EMAIL", payslipSendVars), "p-1")
	if rr.Code != http.StatusUnprocessableEntity || !strings.Contains(rr.Body.String(), "intent_class_not_direct") || del.seen != nil {
		t.Fatalf("want 422 intent_class_not_direct and no delivery, got %d: %s", rr.Code, rr.Body.String())
	}
}

// Fail closed: a bound template is never sent when the registry cannot say what governs it.
func TestSend_BoundTemplateFailsClosed(t *testing.T) {
	stub := newStubIntents()
	del := &stubDeliverer{delivered: true}
	r, s := intentRouter(t, stub, del, nil)
	intentID := publishedIntent(t, r, "payroll.payslip_available", nil)
	tmpl := boundTemplate(t, r, intentID, []string{"first_name", "period", "net_pay"}, payslipBody)

	// The intent is retired: nothing is in force.
	if rr := doReq(r, http.MethodPost, "/v1/communication-intents/"+intentID+"/retire", nil, "approver-1"); rr.Code != http.StatusOK {
		t.Fatalf("retire: %d %s", rr.Code, rr.Body.String())
	}
	time.Sleep(5 * time.Millisecond)
	rr := doReq(r, http.MethodPost, "/v1/notifications/", intentSend(tmpl, "corr-retired", "EMAIL", payslipSendVars), "p-1")
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "intent_not_effective") || del.seen != nil {
		t.Errorf("retired intent: want 409 intent_not_effective, got %d: %s", rr.Code, rr.Body.String())
	}

	// No registry wired at all: 503.
	r2, s2 := intentRouter(t, nil, del, nil)
	s2.templates = s.templates
	s2.versions = s.versions
	rr = doReq(r2, http.MethodPost, "/v1/notifications/", intentSend(tmpl, "corr-noreg", "EMAIL", payslipSendVars), "p-1")
	if rr.Code != http.StatusServiceUnavailable || del.seen != nil {
		t.Errorf("no registry: want 503 and no delivery, got %d: %s", rr.Code, rr.Body.String())
	}
}
