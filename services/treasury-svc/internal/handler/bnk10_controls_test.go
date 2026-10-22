package handler_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/treasury-svc/internal/domain"
	"zoiko.io/treasury-svc/internal/handler"
)

func newTransferRouterWithClients(s *mockStore, c *mockClients) chi.Router {
	h := handler.New(s, &mockAuthz{allowed: true}, c, &mockTransferClients{}, nil, zap.NewNop())
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)
	return r
}

// RecordFXRate is a platform-level decision and asks authorization-svc for
// the PLATFORM scope. Before, it sent an empty legal_entity_id, which
// authorization-svc refuses with a 400, so every call failed as 503.
func TestHandler_RecordFXRate_AuthorizesAtPlatformScope(t *testing.T) {
	s := newMockStore()
	authz := &mockAuthz{allowed: true}
	r := newTransferRouter(s, authz, &mockTransferClients{})

	rr := doTransferJSONRequest(t, r, http.MethodPost, "/v1/treasury/fx/rates", map[string]any{"currency_pair": "EUR/USD", "rate": 1.1}, "treasurer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("record rate: %d %s", rr.Code, rr.Body.String())
	}
	if len(authz.entities) != 1 || authz.entities[0] != "PLATFORM" {
		t.Fatalf("record rate checked legal entities %v, want [PLATFORM]", authz.entities)
	}
}

// The lineage read needs view permission in the snapshot's legal entity,
// like every other FX exposure read. Before, a tenant header was enough.
func TestHandler_GetSourceLineage_RequiresPermission(t *testing.T) {
	s := newMockStore()
	s.fxExposures = map[string]*domain.FXExposureSnapshot{
		"fx-1": {SnapshotID: "fx-1", TenantID: "tenant-abc", LegalEntityID: "ent-1", ExposureCurrency: "EUR", FunctionalCurrency: "USD", Status: domain.FXExposureCalculated},
	}
	authz := &mockAuthz{allowed: false}
	denied := newTransferRouter(s, authz, &mockTransferClients{})
	allowed := newTransferRouter(s, &mockAuthz{allowed: true}, &mockTransferClients{})

	if rr := doTransferJSONRequest(t, allowed, http.MethodGet, "/v1/treasury/fx/exposure-snapshot/fx-1/lineage", nil, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("lineage without a principal: %d, want 401", rr.Code)
	}
	if rr := doTransferJSONRequest(t, denied, http.MethodGet, "/v1/treasury/fx/exposure-snapshot/fx-1/lineage", nil, "outsider"); rr.Code != http.StatusForbidden {
		t.Fatalf("lineage without permission: %d, want 403", rr.Code)
	}
	if len(authz.entities) != 1 || authz.entities[0] != "ent-1" {
		t.Fatalf("lineage checked legal entities %v, want [ent-1]", authz.entities)
	}
	if rr := doTransferJSONRequest(t, allowed, http.MethodGet, "/v1/treasury/fx/exposure-snapshot/fx-1/lineage", nil, "analyst-1"); rr.Code != http.StatusOK {
		t.Fatalf("lineage with permission: %d %s", rr.Code, rr.Body.String())
	}
}

// With no rate recorded, a scenario has no current exposure to compare
// against and is refused as GetFXExposure refuses it. Before, the current
// side silently used 1.0 and reported a delta against parity.
func TestHandler_RunFXScenario_RequiresRecordedRate(t *testing.T) {
	s := newMockStore()
	c := &mockClients{inflowsData: []domain.ExpectedCashFlow{{Amount: 1000, DueDate: time.Now().UTC().AddDate(0, 0, 10), Category: "RECEIVABLE"}}}
	h := newTransferRouterWithClients(s, c)

	rr := doTransferJSONRequest(t, h, http.MethodPost, "/v1/treasury/fx/scenario",
		map[string]any{"legal_entity_id": "ent-1", "exposure_currency": "EUR", "functional_currency": "USD", "hypothetical_rate": 1.2}, "analyst-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("scenario with no recorded rate: %d %s, want 422", rr.Code, rr.Body.String())
	}
}
