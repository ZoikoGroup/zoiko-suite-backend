package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/counterparty-management-svc/internal/authz"
	"zoiko.io/counterparty-management-svc/internal/domain"
	"zoiko.io/counterparty-management-svc/internal/events"
	"zoiko.io/counterparty-management-svc/internal/store"
)

// crmStubStore embeds the interface so only the methods a test needs are
// implemented; calling any other method panics, which fails the test loudly.
type crmStubStore struct {
	store.CRMStore
	rel      *domain.Relationship
	opp      *domain.Opportunity
	getErr   error
	created  int
	dataRead int // incremented whenever a data-returning read runs
}

func (s *crmStubStore) GetRelationship(_ context.Context, _ string) (*domain.Relationship, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.rel, nil
}
func (s *crmStubStore) GetOpportunity(_ context.Context, _ string) (*domain.Opportunity, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.opp, nil
}
func (s *crmStubStore) GetTimeline(_ context.Context, _, _ string) ([]domain.Interaction, error) {
	s.dataRead++
	return []domain.Interaction{{InteractionID: "int-1", Notes: "secret-notes"}}, nil
}
func (s *crmStubStore) GetLinkedFinancialObjects(_ context.Context, _, _ string) ([]domain.CommercialObjectLink, error) {
	s.dataRead++
	return []domain.CommercialObjectLink{{LinkID: "lnk-1"}}, nil
}
func (s *crmStubStore) GetConsentContext(_ context.Context, _, _ string) ([]domain.ConsentRecord, error) {
	s.dataRead++
	return []domain.ConsentRecord{{ConsentID: "con-1", ConsentType: "marketing"}}, nil
}
func (s *crmStubStore) CreateRelationship(_ context.Context, p domain.CreateRelationshipParams) (*domain.Relationship, error) {
	s.created++
	return &domain.Relationship{RelationshipID: "rel-new", TenantID: p.TenantID, LegalEntityID: p.LegalEntityID}, nil
}

// recordingAuthz records every check and answers with a fixed error.
type recordingAuthz struct {
	err   error
	calls []authzCall
}
type authzCall struct{ principal, entity, action string }

func (a *recordingAuthz) CheckAllowed(_ context.Context, principal, entity, action string) error {
	a.calls = append(a.calls, authzCall{principal, entity, action})
	return a.err
}

type recordingPublisher struct{ types []string }

func (p *recordingPublisher) Publish(_ context.Context, params events.PublishParams) error {
	p.types = append(p.types, params.EventType)
	return nil
}

func crmRouter(st *crmStubStore, az *recordingAuthz, pub *recordingPublisher) http.Handler {
	h := New(newStubStore(), pub, az, zap.NewNop())
	r := chi.NewRouter()
	RegisterCRMRoutes(r, h, st)
	return r
}

func newCRMStub() *crmStubStore {
	return &crmStubStore{
		rel: &domain.Relationship{RelationshipID: "rel-1", TenantID: "tenant-test-01", LegalEntityID: "le-1"},
		opp: &domain.Opportunity{OpportunityID: "opp-1", RelationshipID: "rel-1", TenantID: "tenant-test-01", LegalEntityID: "le-1"},
	}
}

func do(h http.Handler, method, path, body string, withPrincipal bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", "tenant-test-01")
	if withPrincipal {
		req.Header.Set("X-Principal-Id", "principal-test-01")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// The five relationship/opportunity reads and the action each must check.
var crmReads = []struct {
	name, path, action string
	returnsData        bool // true when the read returns rows from a data-returning store call
}{
	{"GetRelationship", "/v1/relationships/rel-1", actionCRMRead, false},
	{"GetTimeline", "/v1/relationships/rel-1/timeline", actionCRMRead, true},
	{"GetLinkedFinancialObjects", "/v1/relationships/rel-1/commercial-links", actionCRMRead, true},
	{"GetConsentContext", "/v1/relationships/rel-1/consent", actionCRMConsentRead, true},
	{"GetOpportunity", "/v1/opportunities/opp-1", actionCRMRead, false},
}

func TestCRMReads_RequirePrincipal(t *testing.T) {
	for _, c := range crmReads {
		t.Run(c.name, func(t *testing.T) {
			st, az := newCRMStub(), &recordingAuthz{}
			w := do(crmRouter(st, az, &recordingPublisher{}), http.MethodGet, c.path, "", false)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("want 401 without X-Principal-Id, got %d", w.Code)
			}
			if st.dataRead != 0 || len(az.calls) != 0 {
				t.Fatalf("no store read or authz check should happen before authentication")
			}
		})
	}
}

func TestCRMReads_AuthorizeOnEntityWithExpectedAction(t *testing.T) {
	for _, c := range crmReads {
		t.Run(c.name, func(t *testing.T) {
			st, az := newCRMStub(), &recordingAuthz{}
			w := do(crmRouter(st, az, &recordingPublisher{}), http.MethodGet, c.path, "", true)
			if w.Code != http.StatusOK {
				t.Fatalf("want 200, got %d — %s", w.Code, w.Body.String())
			}
			if len(az.calls) != 1 || az.calls[0] != (authzCall{"principal-test-01", "le-1", c.action}) {
				t.Fatalf("want one check {principal-test-01 le-1 %s}, got %+v", c.action, az.calls)
			}
		})
	}
}

func TestCRMReads_DeniedReturns403AndNoData(t *testing.T) {
	for _, c := range crmReads {
		t.Run(c.name, func(t *testing.T) {
			st, az := newCRMStub(), &recordingAuthz{err: authz.ErrAuthorizationDenied}
			w := do(crmRouter(st, az, &recordingPublisher{}), http.MethodGet, c.path, "", true)
			if w.Code != http.StatusForbidden {
				t.Fatalf("want 403, got %d — %s", w.Code, w.Body.String())
			}
			if st.dataRead != 0 {
				t.Fatalf("data-returning store call ran despite denial")
			}
			for _, leak := range []string{"secret-notes", "con-1", "lnk-1", "le-1\""} {
				if strings.Contains(w.Body.String(), leak) {
					t.Fatalf("denied response leaked %q: %s", leak, w.Body.String())
				}
			}
		})
	}
}

func TestCRMReads_AuthzUnavailableFailsClosedWith503(t *testing.T) {
	for _, c := range crmReads {
		t.Run(c.name, func(t *testing.T) {
			st, az := newCRMStub(), &recordingAuthz{err: errors.New("connection refused")}
			w := do(crmRouter(st, az, &recordingPublisher{}), http.MethodGet, c.path, "", true)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("want 503, got %d", w.Code)
			}
			if st.dataRead != 0 {
				t.Fatalf("data-returning store call ran while authz was unavailable")
			}
		})
	}
}

func TestCRMReads_UnknownResourceIs404BeforeAuthz(t *testing.T) {
	for _, c := range crmReads {
		t.Run(c.name, func(t *testing.T) {
			st, az := newCRMStub(), &recordingAuthz{}
			st.getErr = domain.ErrRelationshipNotFound
			if c.name == "GetOpportunity" {
				st.getErr = domain.ErrOpportunityNotFound
			}
			w := do(crmRouter(st, az, &recordingPublisher{}), http.MethodGet, c.path, "", true)
			if w.Code != http.StatusNotFound {
				t.Fatalf("want 404, got %d", w.Code)
			}
			if len(az.calls) != 0 {
				t.Fatalf("authz must not be consulted for a resource that does not exist")
			}
		})
	}
}

// Consent data is purpose-controlled: a principal allowed CRM_READ but not
// CRM_CONSENT_READ must still be refused the consent context.
func TestGetConsentContext_NeedsItsOwnAction(t *testing.T) {
	st := newCRMStub()
	az := &actionScopedAuthz{allow: map[string]bool{actionCRMRead: true}}
	h := crmRouter2(st, az)
	if w := do(h, http.MethodGet, "/v1/relationships/rel-1/timeline", "", true); w.Code != http.StatusOK {
		t.Fatalf("CRM_READ should allow the timeline, got %d", w.Code)
	}
	if w := do(h, http.MethodGet, "/v1/relationships/rel-1/consent", "", true); w.Code != http.StatusForbidden {
		t.Fatalf("CRM_READ alone must not allow consent context, got %d", w.Code)
	}
}

type actionScopedAuthz struct{ allow map[string]bool }

func (a *actionScopedAuthz) CheckAllowed(_ context.Context, _, _, action string) error {
	if a.allow[action] {
		return nil
	}
	return authz.ErrAuthorizationDenied
}

func crmRouter2(st *crmStubStore, az AuthZClient) http.Handler {
	h := New(newStubStore(), &recordingPublisher{}, az, zap.NewNop())
	r := chi.NewRouter()
	RegisterCRMRoutes(r, h, st)
	return r
}

func TestCreateRelationship_Validation(t *testing.T) {
	st, az, pub := newCRMStub(), &recordingAuthz{}, &recordingPublisher{}
	h := crmRouter(st, az, pub)

	if w := do(h, http.MethodPost, "/v1/relationships/", `{"source":"web"}`, true); w.Code != http.StatusBadRequest {
		t.Fatalf("missing legal_entity_id: want 400, got %d", w.Code)
	}
	if w := do(h, http.MethodPost, "/v1/relationships/", `not-json`, true); w.Code != http.StatusBadRequest {
		t.Fatalf("bad body: want 400, got %d", w.Code)
	}
	if w := do(h, http.MethodPost, "/v1/relationships/", `{"legal_entity_id":"le-1"}`, false); w.Code != http.StatusUnauthorized {
		t.Fatalf("no principal: want 401, got %d", w.Code)
	}
	if st.created != 0 || len(pub.types) != 0 {
		t.Fatalf("rejected requests must not create or publish")
	}
}

func TestCreateRelationship_DeniedCreatesNothing(t *testing.T) {
	st, az, pub := newCRMStub(), &recordingAuthz{err: authz.ErrAuthorizationDenied}, &recordingPublisher{}
	w := do(crmRouter(st, az, pub), http.MethodPost, "/v1/relationships/", `{"legal_entity_id":"le-1"}`, true)
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", w.Code)
	}
	if st.created != 0 || len(pub.types) != 0 {
		t.Fatalf("denied create must not write or publish")
	}
	if len(az.calls) != 1 || az.calls[0].action != actionCRMCreate || az.calls[0].entity != "le-1" {
		t.Fatalf("want one CRM_CREATE check on le-1, got %+v", az.calls)
	}
}

// BIZ-06 events are written to the eventing outbox by the store, in the same
// transaction as the change. The handler must not also publish them directly,
// or every event would be emitted twice (once without a transactional guarantee).
func TestCreateRelationship_HandlerDoesNotPublishDirectly(t *testing.T) {
	st, az, pub := newCRMStub(), &recordingAuthz{}, &recordingPublisher{}
	w := do(crmRouter(st, az, pub), http.MethodPost, "/v1/relationships/", `{"legal_entity_id":"le-1"}`, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d — %s", w.Code, w.Body.String())
	}
	if st.created != 1 {
		t.Fatalf("want the store called once, got %d", st.created)
	}
	if len(pub.types) != 0 {
		t.Fatalf("handler must not publish BIZ-06 events directly, got %v", pub.types)
	}
}
