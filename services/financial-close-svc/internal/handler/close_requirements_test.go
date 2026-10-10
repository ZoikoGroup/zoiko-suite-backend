package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/financial-close-svc/internal/domain"
	"zoiko.io/financial-close-svc/internal/handler"
	"zoiko.io/financial-close-svc/internal/middleware"
)

// actionAuthZ grants everything except the listed actions, and records what
// was asked, so a test can see WHICH permission a command needed.
type actionAuthZ struct {
	deny  map[string]bool
	asked []string
}

func (a *actionAuthZ) CheckAllowed(_ context.Context, _, _, action string) error {
	a.asked = append(a.asked, action)
	if a.deny[action] {
		return domain.ErrAuthorizationDenied
	}
	return nil
}

func checklistRouter(s *stubStore, authz *actionAuthZ) chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	// Treasury knows le-1's accounts, so exclusions of them are accepted.
	cl := &stubClients{bankAccounts: []domain.BankAccountRef{
		{BankAccountID: "acct-1", LegalEntityID: "le-1", AccountStatus: "ACTIVE"},
		{BankAccountID: "acct-petty", LegalEntityID: "le-1", AccountStatus: "ACTIVE"},
	}}
	handler.RegisterRoutes(r, handler.New(s, authz, cl, testSigningKey, zap.NewNop()))
	return r
}

// checklistRouterWithClients is checklistRouter with caller-supplied clients,
// for tests that need real bank/subledger gate data as well as a specific
// authz stub.
func checklistRouterWithClients(s *stubStore, authz *actionAuthZ, cl *stubClients) chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	handler.RegisterRoutes(r, handler.New(s, authz, cl, testSigningKey, zap.NewNop()))
	return r
}

func TestAddCloseRequirement_Validation(t *testing.T) {
	cases := []struct {
		name string
		req  domain.CloseRequirementRequest
		code string
	}{
		{"AR is baseline", domain.CloseRequirementRequest{LegalEntityID: "le-1", Kind: "SUBLEDGER_CONTROL", Subledger: "ar"}, "baseline_requirement"},
		{"unknown control", domain.CloseRequirementRequest{LegalEntityID: "le-1", Kind: "SUBLEDGER_CONTROL", Subledger: "PAYROLL"}, "invalid_subledger"},
		{"ASSETS without book", domain.CloseRequirementRequest{LegalEntityID: "le-1", Kind: "SUBLEDGER_CONTROL", Subledger: "ASSETS"}, "missing_fields"},
		{"book on inventory", domain.CloseRequirementRequest{LegalEntityID: "le-1", Kind: "SUBLEDGER_CONTROL", Subledger: "INVENTORY_VALUE", BookID: "GAAP"}, "invalid_requirement"},
		{"exclusion without reason", domain.CloseRequirementRequest{LegalEntityID: "le-1", Kind: "BANK_ACCOUNT_EXCLUSION", BankAccountID: "acct-1"}, "missing_fields"},
		{"exclusion naming a control", domain.CloseRequirementRequest{LegalEntityID: "le-1", Kind: "BANK_ACCOUNT_EXCLUSION", BankAccountID: "acct-1", Reason: "x", Subledger: "ASSETS"}, "invalid_requirement"},
		{"unknown kind", domain.CloseRequirementRequest{LegalEntityID: "le-1", Kind: "WAIVE"}, "invalid_kind"},
		{"no entity", domain.CloseRequirementRequest{Kind: "SUBLEDGER_CONTROL", Subledger: "STOCK_COUNT"}, "missing_fields"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newStubStore()
			rr := doReq(checklistRouter(s, &actionAuthZ{}), http.MethodPost, "/v1/close/requirements/", c.req, "controller-1")
			var body map[string]string
			_ = json.Unmarshal(rr.Body.Bytes(), &body)
			if rr.Code != http.StatusBadRequest || body["error_code"] != c.code {
				t.Fatalf("got %d %s; want 400 %s", rr.Code, rr.Body.String(), c.code)
			}
			if len(s.closeRequirements) != 0 {
				t.Fatal("an invalid requirement was stored")
			}
		})
	}
}

// Configuring a control and waiving a bank account are different authorities.
func TestAddCloseRequirement_PermissionDependsOnKind(t *testing.T) {
	authz := &actionAuthZ{deny: map[string]bool{"PERIOD_CLOSE_EXCLUSION_APPROVE": true}}
	s := newStubStore()
	r := checklistRouter(s, authz)

	ctl := doReq(r, http.MethodPost, "/v1/close/requirements/", domain.CloseRequirementRequest{
		LegalEntityID: "le-1", Kind: "SUBLEDGER_CONTROL", Subledger: "inventory_value"}, "controller-1")
	if ctl.Code != http.StatusCreated {
		t.Fatalf("adding a control with PERIOD_CLOSE_CONFIG: got %d %s", ctl.Code, ctl.Body.String())
	}
	excl := doReq(r, http.MethodPost, "/v1/close/requirements/", domain.CloseRequirementRequest{
		LegalEntityID: "le-1", Kind: "BANK_ACCOUNT_EXCLUSION", BankAccountID: "acct-1", Reason: "dormant"}, "controller-1")
	if excl.Code != http.StatusForbidden {
		t.Fatalf("an exclusion without PERIOD_CLOSE_EXCLUSION_APPROVE must be refused, got %d", excl.Code)
	}
	if len(s.closeRequirements) != 1 || s.closeRequirements[0].Subledger != "INVENTORY_VALUE" {
		t.Fatalf("stored %+v; want only the inventory control, normalised to upper case", s.closeRequirements)
	}
	if authz.asked[0] != "PERIOD_CLOSE_CONFIG" || authz.asked[1] != "PERIOD_CLOSE_EXCLUSION_APPROVE" {
		t.Fatalf("asked %v", authz.asked)
	}
}

func TestCloseChecklist_ShowsBaselineAndActiveItems(t *testing.T) {
	s := newStubStore()
	r := checklistRouter(s, &actionAuthZ{})
	add := func(req domain.CloseRequirementRequest) domain.CloseRequirement {
		rr := doReq(r, http.MethodPost, "/v1/close/requirements/", req, "controller-1")
		var cr domain.CloseRequirement
		_ = json.Unmarshal(rr.Body.Bytes(), &cr)
		return cr
	}
	assets := add(domain.CloseRequirementRequest{LegalEntityID: "le-1", Kind: "SUBLEDGER_CONTROL", Subledger: "ASSETS", BookID: "STATUTORY"})
	add(domain.CloseRequirementRequest{LegalEntityID: "le-1", Kind: "BANK_ACCOUNT_EXCLUSION", BankAccountID: "acct-petty", Reason: "petty cash float"})
	add(domain.CloseRequirementRequest{LegalEntityID: "le-2", Kind: "SUBLEDGER_CONTROL", Subledger: "STOCK_COUNT"})

	// Replay returns the existing item.
	replay := doReq(r, http.MethodPost, "/v1/close/requirements/", domain.CloseRequirementRequest{
		LegalEntityID: "le-1", Kind: "SUBLEDGER_CONTROL", Subledger: "ASSETS", BookID: "STATUTORY"}, "controller-1")
	if replay.Code != http.StatusOK {
		t.Fatalf("replay: got %d", replay.Code)
	}

	rr := doReq(r, http.MethodGet, "/v1/close/requirements/?legal_entity_id=le-1", nil, "controller-1")
	var cl domain.CloseChecklist
	if err := json.Unmarshal(rr.Body.Bytes(), &cl); err != nil || rr.Code != http.StatusOK {
		t.Fatalf("checklist: %d %s", rr.Code, rr.Body.String())
	}
	if len(cl.BaselineSubledgerControls) != 2 || len(cl.RequiredSubledgerControls) != 1 || len(cl.ExcludedBankAccounts) != 1 {
		t.Fatalf("checklist %+v", cl)
	}

	rem := doReq(r, http.MethodPost, "/v1/close/requirements/"+assets.RequirementID+"/remove",
		domain.RemoveCloseRequirementRequest{Reason: "assets moved to a sister entity"}, "controller-2")
	if rem.Code != http.StatusOK {
		t.Fatalf("remove: %d %s", rem.Code, rem.Body.String())
	}
	again := doReq(r, http.MethodPost, "/v1/close/requirements/"+assets.RequirementID+"/remove",
		domain.RemoveCloseRequirementRequest{Reason: "again"}, "controller-2")
	if again.Code != http.StatusConflict {
		t.Fatalf("second removal: got %d, want 409", again.Code)
	}
	noReason := doReq(r, http.MethodPost, "/v1/close/requirements/"+assets.RequirementID+"/remove",
		domain.RemoveCloseRequirementRequest{}, "controller-2")
	if noReason.Code != http.StatusBadRequest {
		t.Fatalf("removal without a reason: got %d, want 400", noReason.Code)
	}
}

// Removing an exclusion needs the same authority that granted it.
func TestRemoveCloseRequirement_UsesTheKindsPermission(t *testing.T) {
	s := newStubStore()
	excl := &domain.CloseRequirement{RequirementID: "req-1", TenantID: testTenantID, LegalEntityID: "le-1",
		Kind: "BANK_ACCOUNT_EXCLUSION", BankAccountID: "acct-1", Reason: "dormant"}
	s.closeRequirements = append(s.closeRequirements, excl)
	authz := &actionAuthZ{deny: map[string]bool{"PERIOD_CLOSE_EXCLUSION_APPROVE": true}}
	rr := doReq(checklistRouter(s, authz), http.MethodPost, "/v1/close/requirements/req-1/remove",
		domain.RemoveCloseRequirementRequest{Reason: "account reactivated"}, "controller-1")
	if rr.Code != http.StatusForbidden || excl.RemovedAt != nil {
		t.Fatalf("got %d, removed=%v; want 403 and untouched", rr.Code, excl.RemovedAt)
	}
}

// An exclusion must name one of the entity's own accounts, as treasury knows
// them; if treasury cannot say, nothing is waived.
func TestAddBankExclusion_AccountMustBelongToTheEntity(t *testing.T) {
	s := newStubStore()
	r := checklistRouter(s, &actionAuthZ{})
	unknown := doReq(r, http.MethodPost, "/v1/close/requirements/", domain.CloseRequirementRequest{
		LegalEntityID: "le-1", Kind: "BANK_ACCOUNT_EXCLUSION", BankAccountID: "acct-of-another-entity", Reason: "dormant"}, "controller-1")
	if unknown.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown account: got %d %s, want 422", unknown.Code, unknown.Body.String())
	}

	down := chi.NewRouter()
	down.Use(middleware.TenantContext())
	handler.RegisterRoutes(down, handler.New(s, &actionAuthZ{},
		&stubClients{bankAccountsErr: domain.ErrTreasuryUnavailable}, testSigningKey, zap.NewNop()))
	rr := doReq(down, http.MethodPost, "/v1/close/requirements/", domain.CloseRequirementRequest{
		LegalEntityID: "le-1", Kind: "BANK_ACCOUNT_EXCLUSION", BankAccountID: "acct-1", Reason: "dormant"}, "controller-1")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("treasury down: got %d, want 503", rr.Code)
	}
	if len(s.closeRequirements) != 0 {
		t.Fatal("an unverified exclusion was stored")
	}
}
