package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/treasury-svc/internal/domain"
	"zoiko.io/treasury-svc/internal/handler"
)

func newTransferRouter(s *mockStore, authz *mockAuthz, tc handler.TransferClients) chi.Router {
	h := handler.New(s, authz, &mockClients{}, tc, nil, zap.NewNop())
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)
	return r
}

// Every command on an existing transfer is authorized in the legal
// entities that own its accounts — both of them when it crosses entities.
// Before, approve/authorize/execute/amend/cancel/resolve all sent the
// tenant ID in the legal_entity_id slot.
func TestHandler_TransferCommands_AuthorizeInAccountEntities(t *testing.T) {
	s := newMockStore()
	s.transfers["t1"] = withAccounts(s, &domain.TreasuryTransfer{
		TransferID: "t1", TenantID: "tenant-abc", Status: domain.TransferPendingApproval, IsCrossEntity: true,
		Amount: 100, CurrencyCode: "USD", MakerPrincipalID: "maker-1",
	})
	authz := &mockAuthz{allowed: true}
	r := newTransferRouter(s, authz, &mockTransferClients{})

	if rr := doTransferJSONRequest(t, r, http.MethodPost, "/v1/treasury/transfers/t1/approve", nil, "checker-1"); rr.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rr.Code, rr.Body.String())
	}
	if len(authz.entities) != 2 || authz.entities[0] != "ent-src" || authz.entities[1] != "ent-tgt" {
		t.Fatalf("approve checked legal entities %v, want [ent-src ent-tgt]", authz.entities)
	}
	if authz.lastAction != "TREASURY_TRANSFER_APPROVE" {
		t.Fatalf("approve checked %q", authz.lastAction)
	}
}

// Rejecting is the checker's other answer to a pending transfer, so it
// needs the approve permission. Before, any principal could reject.
func TestHandler_RejectTreasuryTransfer_RequiresPermission(t *testing.T) {
	s := newMockStore()
	s.transfers["t1"] = withAccounts(s, &domain.TreasuryTransfer{
		TransferID: "t1", TenantID: "tenant-abc", Status: domain.TransferPendingApproval, MakerPrincipalID: "maker-1",
	})
	r := newTransferRouter(s, &mockAuthz{allowed: false}, &mockTransferClients{})

	rr := doTransferJSONRequest(t, r, http.MethodPost, "/v1/treasury/transfers/t1/reject", map[string]string{"reason": "no"}, "someone")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("reject without permission: %d %s, want 403", rr.Code, rr.Body.String())
	}
	if s.transfers["t1"].Status != domain.TransferPendingApproval {
		t.Fatalf("an unauthorized reject changed the transfer to %s", s.transfers["t1"].Status)
	}
}

// Amending a same-entity draft to an account in another legal entity makes
// it cross-entity, so it must be authorized in that entity and then go
// through AuthorizeTreasuryTransfer, the ledger posting and intercompany
// pairing. Before, it kept is_cross_entity=false and executed as a
// same-entity movement.
func TestHandler_AmendTreasuryTransfer_ReclassifiesCrossEntity(t *testing.T) {
	s := newMockStore()
	s.transfers["t1"] = withAccounts(s, &domain.TreasuryTransfer{
		TransferID: "t1", TenantID: "tenant-abc", Status: domain.TransferDraft, MakerPrincipalID: "maker-1", Amount: 100, CurrencyCode: "USD",
	})
	s.bankAccounts["other-entity"] = &domain.BankAccount{BankAccountID: "other-entity", TenantID: "tenant-abc", LegalEntityID: "ent-other", CurrencyCode: "USD", AccountStatus: domain.BankAccountActive}
	authz := &mockAuthz{allowed: true}
	r := newTransferRouter(s, authz, &mockTransferClients{})

	rr := doTransferJSONRequest(t, r, http.MethodPost, "/v1/treasury/transfers/t1/amend",
		domain.InitiateTransferRequest{SourceBankAccountID: "acct-src-t1", TargetBankAccountID: "other-entity", Amount: 100, CurrencyCode: "USD"}, "maker-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("amend: %d %s", rr.Code, rr.Body.String())
	}
	var amended domain.TreasuryTransfer
	_ = json.Unmarshal(rr.Body.Bytes(), &amended)
	if !amended.IsCrossEntity {
		t.Fatal("a transfer amended to another legal entity's account is still classified same-entity")
	}
	sawOther := false
	for _, e := range authz.entities {
		sawOther = sawOther || e == "ent-other"
	}
	if !sawOther {
		t.Fatalf("amend never checked permission in the new target's legal entity: %v", authz.entities)
	}

	// An amended account must be usable, as on create.
	s.bankAccounts["suspended"] = &domain.BankAccount{BankAccountID: "suspended", TenantID: "tenant-abc", LegalEntityID: "ent-src", CurrencyCode: "USD", AccountStatus: domain.BankAccountSuspended}
	rr = doTransferJSONRequest(t, r, http.MethodPost, "/v1/treasury/transfers/t1/amend",
		domain.InitiateTransferRequest{SourceBankAccountID: "acct-src-t1", TargetBankAccountID: "suspended", Amount: 100, CurrencyCode: "USD"}, "maker-1")
	if rr.Code != http.StatusPreconditionFailed {
		t.Fatalf("amend to a suspended account: %d %s, want 412", rr.Code, rr.Body.String())
	}
}

// An account suspended after approval blocks the payment from leaving:
// nothing is sent to BNK-06 and the transfer stays APPROVED.
func TestHandler_ExecuteTreasuryTransfer_RefusesInactiveAccount(t *testing.T) {
	s := newMockStore()
	s.transfers["t1"] = withAccounts(s, &domain.TreasuryTransfer{
		TransferID: "t1", TenantID: "tenant-abc", Status: domain.TransferApproved,
		Amount: 100, CurrencyCode: "USD", MakerPrincipalID: "maker-1", CheckerPrincipalID: "checker-1",
	})
	s.bankAccounts["acct-src-t1"].AccountStatus = domain.BankAccountSuspended
	tc := &capturingTransferClients{}
	r := newTransferRouter(s, &mockAuthz{allowed: true}, tc)

	rr := doTransferJSONRequest(t, r, http.MethodPost, "/v1/treasury/transfers/t1/execute", nil, "checker-1")
	if rr.Code != http.StatusPreconditionFailed {
		t.Fatalf("execute from a suspended account: %d %s, want 412", rr.Code, rr.Body.String())
	}
	if tc.lastFingerprint != "" {
		t.Fatal("a payment attempt was sent from a suspended account")
	}
	if s.transfers["t1"].Status != domain.TransferApproved {
		t.Fatalf("transfer moved to %s", s.transfers["t1"].Status)
	}
}

// A transfer whose stored classification disagrees with its accounts'
// legal entities would run the wrong saga, so it is refused before any
// payment leaves.
func TestHandler_ExecuteTreasuryTransfer_RefusesMisclassifiedTransfer(t *testing.T) {
	s := newMockStore()
	s.bankAccounts["src"] = &domain.BankAccount{BankAccountID: "src", TenantID: "tenant-abc", LegalEntityID: "ent-a", CurrencyCode: "USD", AccountStatus: domain.BankAccountActive}
	s.bankAccounts["tgt"] = &domain.BankAccount{BankAccountID: "tgt", TenantID: "tenant-abc", LegalEntityID: "ent-b", CurrencyCode: "USD", AccountStatus: domain.BankAccountActive}
	s.transfers["t1"] = &domain.TreasuryTransfer{
		TransferID: "t1", TenantID: "tenant-abc", Status: domain.TransferApproved, IsCrossEntity: false,
		SourceBankAccountID: "src", TargetBankAccountID: "tgt",
		Amount: 100, CurrencyCode: "USD", MakerPrincipalID: "maker-1", CheckerPrincipalID: "checker-1",
	}
	tc := &capturingTransferClients{}
	r := newTransferRouter(s, &mockAuthz{allowed: true}, tc)

	rr := doTransferJSONRequest(t, r, http.MethodPost, "/v1/treasury/transfers/t1/execute", nil, "checker-1")
	if rr.Code != http.StatusConflict {
		t.Fatalf("execute a misclassified cross-entity transfer: %d %s, want 409", rr.Code, rr.Body.String())
	}
	if tc.lastFingerprint != "" {
		t.Fatal("a misclassified transfer's payment attempt was sent")
	}
}

// Transfer reads need a principal with view permission in the transfer's
// legal entity, and a list needs a legal entity to authorize against.
// Before, a tenant header was enough to read or list every transfer.
func TestHandler_TransferReads_RequirePrincipalAndPermission(t *testing.T) {
	s := newMockStore()
	s.transfers["t1"] = withAccounts(s, &domain.TreasuryTransfer{
		TransferID: "t1", TenantID: "tenant-abc", Status: domain.TransferPendingApproval, MakerPrincipalID: "maker-1",
	})
	allowed := newTransferRouter(s, &mockAuthz{allowed: true}, &mockTransferClients{})
	denied := newTransferRouter(s, &mockAuthz{allowed: false}, &mockTransferClients{})

	for _, path := range []string{
		"/v1/treasury/transfers/t1", "/v1/treasury/transfers/t1/approval",
		"/v1/treasury/transfers/t1/execution", "/v1/treasury/transfers/t1/available-actions",
	} {
		if rr := doTransferJSONRequest(t, allowed, http.MethodGet, path, nil, ""); rr.Code != http.StatusUnauthorized {
			t.Errorf("%s without a principal: %d, want 401", path, rr.Code)
		}
		if rr := doTransferJSONRequest(t, denied, http.MethodGet, path, nil, "usr-1"); rr.Code != http.StatusForbidden {
			t.Errorf("%s without permission: %d, want 403", path, rr.Code)
		}
		if rr := doTransferJSONRequest(t, allowed, http.MethodGet, path, nil, "usr-1"); rr.Code != http.StatusOK {
			t.Errorf("%s with permission: %d %s", path, rr.Code, rr.Body.String())
		}
	}
	if rr := doTransferJSONRequest(t, allowed, http.MethodGet, "/v1/treasury/transfers", nil, "usr-1"); rr.Code != http.StatusBadRequest {
		t.Errorf("tenant-wide list: %d, want 400", rr.Code)
	}
	if rr := doTransferJSONRequest(t, denied, http.MethodGet, "/v1/treasury/transfers?legal_entity_id=ent-src", nil, "usr-1"); rr.Code != http.StatusForbidden {
		t.Errorf("list without permission: %d, want 403", rr.Code)
	}
}
