package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/siem-integration-svc/internal/authz"
	"zoiko.io/siem-integration-svc/internal/handler"
	"zoiko.io/siem-integration-svc/internal/store"
)

// recordingAuthz grants unless deny is set, and remembers what it was asked.
type recordingAuthz struct {
	deny          error
	scope, action string
}

func (a *recordingAuthz) CheckAllowed(_ context.Context, _, legalEntityID, actionType string) error {
	a.scope, a.action = legalEntityID, actionType
	return a.deny
}

func listExporters(r http.Handler, principal, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/v1/siem/exporters"+query, nil)
	req.Header.Set("X-Tenant-ID", "t1")
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// A person listing exporters is authorized SIEM_EXPORTER_READ at the legal
// entity they name, and must name one: the list used to return every
// exporter's name and endpoint URL to anyone holding a tenant header.
func TestListExporters_PersonIsAuthorizedAtTheEntity(t *testing.T) {
	st := store.NewMemoryStore()
	seedExporter(t, handler.NewRouter(handler.New(st, &stubAuthz{}, zap.NewNop())), "t1", "tok")

	az := &recordingAuthz{}
	r := handler.NewRouter(handler.New(st, az, zap.NewNop()))
	w := listExporters(r, "analyst-1", "?legal_entity_id=le-1")
	if w.Code != http.StatusOK {
		t.Fatalf("authorized list: want 200, got %d: %s", w.Code, w.Body.String())
	}
	if az.action != handler.SiemExporterRead || az.scope != "le-1" {
		t.Fatalf("want %s at le-1, asked %s at %q", handler.SiemExporterRead, az.action, az.scope)
	}
	var out struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Data) != 1 || out.Data[0]["endpoint_url"] == nil || out.Data[0]["auth_token"] != nil {
		t.Fatalf("a permitted person sees the entity's exporter rows, never the token: %s", w.Body.String())
	}

	if w := listExporters(r, "analyst-1", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("no legal_entity_id: want 400, got %d: %s", w.Code, w.Body.String())
	}

	denied := handler.NewRouter(handler.New(st, &recordingAuthz{deny: authz.ErrAuthorizationDenied}, zap.NewNop()))
	if w := listExporters(denied, "stranger-1", "?legal_entity_id=le-1"); w.Code != http.StatusForbidden {
		t.Fatalf("denied: want 403, got %d: %s", w.Code, w.Body.String())
	}
	down := handler.NewRouter(handler.New(st, &recordingAuthz{deny: authz.ErrAuthzServiceUnavailable}, zap.NewNop()))
	if w := listExporters(down, "analyst-1", "?legal_entity_id=le-1"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("authorization unavailable: want 503 (fail closed), got %d: %s", w.Code, w.Body.String())
	}
}

// The principal-less fan-out lookup the platform's SIEM clients make is told
// only which exporters are ACTIVE — what those clients decode — and nothing
// that describes them.
func TestListExporters_ServiceLookupSeesOnlyActiveIDs(t *testing.T) {
	st := store.NewMemoryStore()
	id := seedExporter(t, handler.NewRouter(handler.New(st, &stubAuthz{}, zap.NewNop())), "t1", "tok")

	az := &recordingAuthz{}
	w := listExporters(handler.NewRouter(handler.New(st, az, zap.NewNop())), "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("service lookup: want 200, got %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		Data  []map[string]any `json:"data"`
		Count int              `json:"count"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out.Count != 1 || len(out.Data) != 1 || out.Data[0]["id"] != id || out.Data[0]["status"] != "ACTIVE" {
		t.Fatalf("want the one ACTIVE exporter by id, got %s", w.Body.String())
	}
	if len(out.Data[0]) != 2 {
		t.Fatalf("the service lookup must carry only id and status, got %v", out.Data[0])
	}
	if az.action != "" {
		t.Fatalf("the service lookup asked authorization for %s; it has no principal to ask about", az.action)
	}
}
