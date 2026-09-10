package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/handler"
)

// The three §8.3 inbound APIs that had no HTTP surface until now.
//
// The property every one of these tests is ultimately about is that NONE of
// them records a decision artifact. access_decision_log means "one row per
// authorization of a material act", and these three answer questions about the
// grant graph rather than authorizing anything — so a row from any of them
// would make the log's meaning false, which is the meaning an auditor reads it
// for.

func postJSON(t *testing.T, r chi.Router, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func adminHeaders() map[string]string {
	return map[string]string{"X-Principal-Id": "p-caller", "X-Tenant-Id": "11111111-1111-4111-8111-111111111111"}
}

// ── POST /v1/entity-scope/validate ──────────────────────────────────────────

func TestValidateEntityScope_RequiresPrincipalAndTenant(t *testing.T) {
	body := `{"principal_id":"p-1","legal_entity_ids":["11111111-1111-4111-8111-aaaaaaaaaaa1"]}`

	r := newTestRouter(&stubStore{})
	if w := postJSON(t, r, handler.EntityScopeValidatePath, body, map[string]string{"X-Tenant-Id": "11111111-1111-4111-8111-111111111111"}); w.Code != http.StatusUnauthorized {
		t.Errorf("no principal: expected 401, got %d", w.Code)
	}

	r = newTestRouter(&stubStore{})
	if w := postJSON(t, r, handler.EntityScopeValidatePath, body, map[string]string{"X-Principal-Id": "p-caller"}); w.Code != http.StatusUnauthorized {
		t.Errorf("no tenant: expected 401, got %d", w.Code)
	}
}

func TestValidateEntityScope_MissingFields(t *testing.T) {
	for _, body := range []string{
		`{"legal_entity_ids":["11111111-1111-4111-8111-aaaaaaaaaaa1"]}`,
		`{"principal_id":"p-1"}`,
		`{"principal_id":"p-1","legal_entity_ids":[]}`,
		`{"principal_id":"p-1","legal_entity_ids":["11111111-1111-4111-8111-aaaaaaaaaaa1",""]}`,
	} {
		r := newTestRouter(&stubStore{rbacActions: []string{"X"}})
		w := postJSON(t, r, handler.EntityScopeValidatePath, body, adminHeaders())
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d: %s", body, w.Code, w.Body.String())
		}
	}
}

// The broad question — "which entities may this principal act in at all" —
// returns the whole permitted set, so a caller can decide what to offer
// without a second round trip per action.
func TestValidateEntityScope_NoActionReturnsWholePermittedSet(t *testing.T) {
	store := &stubStore{
		rbacActions:      []string{"PAYMENT_APPROVE", "GL_POST"},
		rbacBasis:        "rbac:role=FINANCE",
		delegatedActions: []string{"PAYMENT_INITIATE"},
		delegatedBasis:   "delegated:from=p-boss",
	}
	r := newTestRouter(store)

	w := postJSON(t, r, handler.EntityScopeValidatePath,
		`{"principal_id":"p-1","legal_entity_ids":["11111111-1111-4111-8111-aaaaaaaaaaa1"]}`, adminHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Results []struct {
			LegalEntityID    string   `json:"legal_entity_id"`
			InScope          bool     `json:"in_scope"`
			Basis            string   `json:"basis"`
			PermittedActions []string `json:"permitted_actions"`
		} `json:"results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(resp.Results))
	}
	got := resp.Results[0]
	if !got.InScope {
		t.Error("in_scope = false for a principal holding two actions")
	}
	// Sorted and deduped, and the DELEGATED action is in there: a delegate
	// acting in an entity is in scope for it, and excluding delegations would
	// grey out exactly the buttons a delegation was created to enable.
	want := []string{"GL_POST", "PAYMENT_APPROVE", "PAYMENT_INITIATE"}
	if len(got.PermittedActions) != len(want) {
		t.Fatalf("permitted_actions = %v, want %v", got.PermittedActions, want)
	}
	for i := range want {
		if got.PermittedActions[i] != want[i] {
			t.Fatalf("permitted_actions = %v, want %v (sorted, deduped)", got.PermittedActions, want)
		}
	}
}

// The narrow question suppresses the full set: a caller that asked about one
// action has not asked for the principal's whole grant map.
func TestValidateEntityScope_WithActionSuppressesPermittedActions(t *testing.T) {
	store := &stubStore{rbacActions: []string{"PAYMENT_APPROVE"}, rbacBasis: "rbac:role=FINANCE"}
	r := newTestRouter(store)

	w := postJSON(t, r, handler.EntityScopeValidatePath,
		`{"principal_id":"p-1","legal_entity_ids":["11111111-1111-4111-8111-aaaaaaaaaaa1"],"action_type":"PAYMENT_APPROVE"}`, adminHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Results []struct {
			InScope          bool     `json:"in_scope"`
			Basis            string   `json:"basis"`
			PermittedActions []string `json:"permitted_actions"`
		} `json:"results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Results[0].InScope {
		t.Error("in_scope = false for an action the principal holds")
	}
	if resp.Results[0].Basis != "rbac:role=FINANCE" {
		t.Errorf("basis = %q, want the rbac basis", resp.Results[0].Basis)
	}
	if len(resp.Results[0].PermittedActions) != 0 {
		t.Errorf("permitted_actions = %v, want empty on the narrow question", resp.Results[0].PermittedActions)
	}
}

func TestValidateEntityScope_NotInScopeReadsAsNoGrant(t *testing.T) {
	r := newTestRouter(&stubStore{})

	w := postJSON(t, r, handler.EntityScopeValidatePath,
		`{"principal_id":"p-nobody","legal_entity_ids":["11111111-1111-4111-8111-aaaaaaaaaaa1"],"action_type":"PAYMENT_APPROVE"}`, adminHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Results []struct {
			InScope bool   `json:"in_scope"`
			Basis   string `json:"basis"`
		} `json:"results"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Results[0].InScope {
		t.Error("in_scope = true for a principal holding nothing")
	}
	if resp.Results[0].Basis != "no_grant" {
		t.Errorf("basis = %q, want no_grant — the same vocabulary decision_basis uses", resp.Results[0].Basis)
	}
}

// The reason this endpoint exists: asking about N entities is ONE call and
// writes NO decision artifacts. Through /v1/authorize it would have been N
// calls and N rows in the audit log for a question nobody acted on.
func TestValidateEntityScope_BatchWritesNoDecisionArtifact(t *testing.T) {
	store := &stubStore{rbacActions: []string{"PAYMENT_APPROVE"}, rbacBasis: "rbac:role=FINANCE"}
	r := newTestRouter(store)

	w := postJSON(t, r, handler.EntityScopeValidatePath,
		`{"principal_id":"p-1","legal_entity_ids":["11111111-1111-4111-8111-aaaaaaaaaaa1","11111111-1111-4111-8111-aaaaaaaaaaa2","11111111-1111-4111-8111-aaaaaaaaaaa3"],"action_type":"PAYMENT_APPROVE"}`, adminHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Results []struct{} `json:"results"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Results) != 3 {
		t.Fatalf("results = %d, want one per requested entity", len(resp.Results))
	}
	if store.recordedParams.ActionType != "" {
		t.Fatalf("a decision artifact was recorded (%+v) — this endpoint authorizes nothing and must not write to access_decision_log",
			store.recordedParams)
	}
}

func TestValidateEntityScope_RefusesOversizedBatch(t *testing.T) {
	entities := make([]string, 101)
	for i := range entities {
		entities[i] = "le"
	}
	raw, _ := json.Marshal(map[string]any{"principal_id": "p-1", "legal_entity_ids": entities})

	r := newTestRouter(&stubStore{})
	w := postJSON(t, r, handler.EntityScopeValidatePath, string(raw), adminHeaders())
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for 101 entities, got %d: %s", w.Code, w.Body.String())
	}
}

func TestValidateEntityScope_RefusesForeignBodyTenant(t *testing.T) {
	r := newTestRouter(&stubStore{})
	w := postJSON(t, r, handler.EntityScopeValidatePath,
		`{"principal_id":"p-1","legal_entity_ids":["11111111-1111-4111-8111-aaaaaaaaaaa1"],"tenant_id":"t-someone-else"}`, adminHeaders())
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a body tenant that disagrees with the header, got %d: %s", w.Code, w.Body.String())
	}
}

func TestValidateEntityScope_StoreUnavailableIs503(t *testing.T) {
	r := newTestRouter(&stubStore{rbacErr: domain.ErrStoreUnavailable})
	w := postJSON(t, r, handler.EntityScopeValidatePath,
		`{"principal_id":"p-1","legal_entity_ids":["11111111-1111-4111-8111-aaaaaaaaaaa1"]}`, adminHeaders())
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
}

// PLATFORM resolves the same way it does on /v1/authorize — one sentinel, one
// configured id, rather than each route resolving it separately.
func TestValidateEntityScope_ResolvesPlatformSentinel(t *testing.T) {
	store := &stubStore{rbacActions: []string{"SOD_RULE_MANAGE_GLOBAL"}, rbacBasis: "rbac:role=PLATFORM_ADMIN"}
	r := newTestRouter(store)

	w := postJSON(t, r, handler.EntityScopeValidatePath,
		`{"principal_id":"p-1","legal_entity_ids":["PLATFORM"]}`, adminHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	// newTestRouter configures "platform-scope-entity" as the platform id.
	if store.grantedTenantArg != "11111111-1111-4111-8111-111111111111" {
		t.Errorf("grant lookup tenant = %q, want the verified tenant", store.grantedTenantArg)
	}

	var resp struct {
		Results []struct {
			LegalEntityID string `json:"legal_entity_id"`
		} `json:"results"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	// The response echoes what the CALLER asked about, not the resolved id:
	// the caller asked about PLATFORM and matching its own request back is
	// what lets it correlate results to inputs.
	if resp.Results[0].LegalEntityID != "PLATFORM" {
		t.Errorf("legal_entity_id = %q, want the sentinel the caller sent", resp.Results[0].LegalEntityID)
	}
}

// ── POST /v1/sod/validate ───────────────────────────────────────────────────

func TestValidateSoD_RequiresCandidateActions(t *testing.T) {
	for _, body := range []string{`{}`, `{"candidate_actions":[]}`, `{"candidate_actions":["",""]}`} {
		r := newTestRouter(&stubStore{})
		w := postJSON(t, r, handler.SoDValidatePath, body, adminHeaders())
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d: %s", body, w.Code, w.Body.String())
		}
	}
}

// The most important refusal on this route. A principal without an entity
// cannot have their held actions resolved — grants are entity-scoped — and
// answering the narrower bundle-only question instead would return
// conflict_free:true for a principal who DOES conflict. A false all-clear on a
// control is worse than an error.
func TestValidateSoD_PrincipalWithoutEntityIsRefusedNotDowngraded(t *testing.T) {
	store := &stubStore{}
	r := newTestRouter(store)

	w := postJSON(t, r, handler.SoDValidatePath,
		`{"principal_id":"p-1","candidate_actions":["PAYMENT_APPROVE"]}`, adminHeaders())
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}

	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["field"] != "legal_entity_id" {
		t.Errorf("error names field %q, want legal_entity_id", body["field"])
	}
	if store.sodConflictAction != "" || store.grantedTenantArg != "" {
		t.Error("the store was consulted despite the refusal")
	}
}

// The question /v1/authorize structurally cannot answer: would granting this
// break separation of duties, BEFORE the assignment exists.
func TestValidateSoD_ReportsConflictWithAHeldAction(t *testing.T) {
	store := &stubStore{
		rbacActions:       []string{"PAYMENT_INITIATE"},
		rbacBasis:         "rbac:role=AP_CLERK",
		sodHasConflict:    true,
		sodConflictAction: "PAYMENT_INITIATE",
	}
	r := newTestRouter(store)

	w := postJSON(t, r, handler.SoDValidatePath,
		`{"principal_id":"p-1","legal_entity_id":"11111111-1111-4111-8111-aaaaaaaaaaa1","candidate_actions":["PAYMENT_APPROVE"]}`, adminHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		ConflictFree bool `json:"conflict_free"`
		Conflicts    []struct {
			CandidateAction string `json:"candidate_action"`
			ConflictsWith   string `json:"conflicts_with"`
			Source          string `json:"source"`
		} `json:"conflicts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ConflictFree {
		t.Fatal("conflict_free = true despite a reported conflict")
	}
	if len(resp.Conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want 1", resp.Conflicts)
	}
	if resp.Conflicts[0].CandidateAction != "PAYMENT_APPROVE" {
		t.Errorf("candidate_action = %q", resp.Conflicts[0].CandidateAction)
	}
	if resp.Conflicts[0].ConflictsWith != "PAYMENT_INITIATE" {
		t.Errorf("conflicts_with = %q", resp.Conflicts[0].ConflictsWith)
	}
	// "held" rather than "candidate", because the remedy is different: the
	// principal already has the other action and something must be revoked
	// before the grant is possible.
	if resp.Conflicts[0].Source != "held" {
		t.Errorf("source = %q, want held — it names what has to change to resolve it", resp.Conflicts[0].Source)
	}
}

// A bundle can be internally conflicted, which grants both actions to everyone
// holding the role and then denies both to all of them. Checking candidates
// only against currently-held actions would call such a bundle conflict-free.
func TestValidateSoD_ChecksCandidatesAgainstEachOther(t *testing.T) {
	store := &stubStore{sodHasConflict: true, sodConflictAction: "PAYMENT_INITIATE"}
	r := newTestRouter(store)

	// No principal at all — the "is this bundle internally conflicted"
	// question, asked by somebody designing a role before anyone holds it.
	w := postJSON(t, r, handler.SoDValidatePath,
		`{"candidate_actions":["PAYMENT_APPROVE","PAYMENT_INITIATE"]}`, adminHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		ConflictFree bool `json:"conflict_free"`
		Conflicts    []struct {
			Source string `json:"source"`
		} `json:"conflicts"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.ConflictFree {
		t.Fatal("conflict_free = true for a bundle containing both halves of a conflicting pair")
	}
	for _, c := range resp.Conflicts {
		if c.Source != "candidate" {
			t.Errorf("source = %q, want candidate — nobody holds anything in this request", c.Source)
		}
	}
}

func TestValidateSoD_ConflictFreeWhenNoRuleFires(t *testing.T) {
	store := &stubStore{rbacActions: []string{"GL_POST"}, rbacBasis: "rbac:role=ACCOUNTANT"}
	r := newTestRouter(store)

	w := postJSON(t, r, handler.SoDValidatePath,
		`{"principal_id":"p-1","legal_entity_id":"11111111-1111-4111-8111-aaaaaaaaaaa1","candidate_actions":["PAYMENT_APPROVE"]}`, adminHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		ConflictFree bool       `json:"conflict_free"`
		Conflicts    []struct{} `json:"conflicts"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !resp.ConflictFree {
		t.Fatal("conflict_free = false with no rule firing")
	}
	if len(resp.Conflicts) != 0 {
		t.Errorf("conflicts = %d, want 0", len(resp.Conflicts))
	}
}

// An own-object restriction is reported but does NOT make the grant
// unavailable: the role is perfectly grantable, one use of it will be refused
// when the holder is also the preparer. Folding it into conflict_free would
// have this endpoint block a legitimate assignment.
func TestValidateSoD_OwnObjectIsReportedButNotAConflict(t *testing.T) {
	store := &stubStore{ownObjectForbidden: true}
	r := newTestRouter(store)

	w := postJSON(t, r, handler.SoDValidatePath,
		`{"candidate_actions":["INVOICE_APPROVE"]}`, adminHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		ConflictFree        bool     `json:"conflict_free"`
		OwnObjectRestricted []string `json:"own_object_restricted"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !resp.ConflictFree {
		t.Error("conflict_free = false for an own-object restriction — the grant is legitimate, one use of it is not")
	}
	if len(resp.OwnObjectRestricted) != 1 || resp.OwnObjectRestricted[0] != "INVOICE_APPROVE" {
		t.Errorf("own_object_restricted = %v, want [INVOICE_APPROVE]", resp.OwnObjectRestricted)
	}
}

// Delegated grants count as held, for the same reason /v1/authorize's SoD layer
// includes them. Clearing a grant that the evaluation engine then denies is the
// worst error this endpoint can make.
func TestValidateSoD_IncludesDelegatedGrantsInHeldSet(t *testing.T) {
	store := &stubStore{
		delegatedActions: []string{"PAYMENT_INITIATE"},
		delegatedBasis:   "delegated:from=p-boss",
	}
	r := newTestRouter(store)

	w := postJSON(t, r, handler.SoDValidatePath,
		`{"principal_id":"p-1","legal_entity_id":"11111111-1111-4111-8111-aaaaaaaaaaa1","candidate_actions":["PAYMENT_APPROVE"]}`, adminHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if store.delegatedTenantArg != "11111111-1111-4111-8111-111111111111" {
		t.Fatal("the delegation lookup was not made — a conflict reached through a delegation would be missed")
	}
}

func TestValidateSoD_WritesNoDecisionArtifact(t *testing.T) {
	store := &stubStore{sodHasConflict: true, sodConflictAction: "PAYMENT_INITIATE"}
	r := newTestRouter(store)

	if w := postJSON(t, r, handler.SoDValidatePath,
		`{"candidate_actions":["PAYMENT_APPROVE"]}`, adminHeaders()); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if store.recordedParams.ActionType != "" {
		t.Fatalf("a decision artifact was recorded (%+v) — a pre-flight check authorizes nothing", store.recordedParams)
	}
}

func TestValidateSoD_RefusesOversizedCandidateSet(t *testing.T) {
	actions := make([]string, 201)
	for i := range actions {
		actions[i] = "ACTION_" + string(rune('A'+i%26)) + string(rune('a'+i/26))
	}
	raw, _ := json.Marshal(map[string]any{"candidate_actions": actions})

	r := newTestRouter(&stubStore{})
	w := postJSON(t, r, handler.SoDValidatePath, string(raw), adminHeaders())
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for 201 candidate actions, got %d: %s", w.Code, w.Body.String())
	}
}

// ── POST /v1/delegated-access/evaluate ──────────────────────────────────────

func TestEvaluateDelegatedAccess_MissingFields(t *testing.T) {
	for _, body := range []string{`{"legal_entity_id":"11111111-1111-4111-8111-aaaaaaaaaaa1"}`, `{"principal_id":"p-1"}`} {
		r := newTestRouter(&stubStore{})
		w := postJSON(t, r, handler.DelegatedAccessEvaluatePath, body, adminHeaders())
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d: %s", body, w.Code, w.Body.String())
		}
	}
}

func TestEvaluateDelegatedAccess_ReportsTheDelegator(t *testing.T) {
	store := &stubStore{
		delegatedActions: []string{"PAYMENT_APPROVE"},
		delegatedBasis:   "delegated:from=p-boss",
	}
	r := newTestRouter(store)

	w := postJSON(t, r, handler.DelegatedAccessEvaluatePath,
		`{"principal_id":"p-1","legal_entity_id":"11111111-1111-4111-8111-aaaaaaaaaaa1","action_type":"PAYMENT_APPROVE"}`, adminHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		HasDelegatedAccess bool     `json:"has_delegated_access"`
		Basis              string   `json:"basis"`
		DelegatedActions   []string `json:"delegated_actions"`
		HeldDirectly       bool     `json:"held_directly"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.HasDelegatedAccess {
		t.Error("has_delegated_access = false")
	}
	if resp.Basis != "delegated:from=p-boss" {
		t.Errorf("basis = %q, want the delegator named", resp.Basis)
	}
	if resp.HeldDirectly {
		t.Error("held_directly = true — this principal holds nothing through their own roles")
	}
}

// The field that makes this endpoint worth having. /v1/authorize returns one
// GRANTED for both paths and names RBAC as the basis when both apply, so a
// four-eyes step could be satisfied by the delegator's own authority while the
// workflow believed a delegate had acted.
func TestEvaluateDelegatedAccess_DistinguishesBorrowedFromOwnAuthority(t *testing.T) {
	store := &stubStore{
		rbacActions:      []string{"PAYMENT_APPROVE"},
		rbacBasis:        "rbac:role=FINANCE_APPROVER",
		delegatedActions: []string{"PAYMENT_APPROVE"},
		delegatedBasis:   "delegated:from=p-boss",
	}
	r := newTestRouter(store)

	w := postJSON(t, r, handler.DelegatedAccessEvaluatePath,
		`{"principal_id":"p-1","legal_entity_id":"11111111-1111-4111-8111-aaaaaaaaaaa1","action_type":"PAYMENT_APPROVE"}`, adminHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		HasDelegatedAccess bool `json:"has_delegated_access"`
		HeldDirectly       bool `json:"held_directly"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !resp.HasDelegatedAccess || !resp.HeldDirectly {
		t.Fatalf("has_delegated_access=%v held_directly=%v — both are true here, and reporting only one is the ambiguity this route removes",
			resp.HasDelegatedAccess, resp.HeldDirectly)
	}
}

func TestEvaluateDelegatedAccess_NoActionReturnsWholeDelegatedSet(t *testing.T) {
	store := &stubStore{
		delegatedActions: []string{"PAYMENT_APPROVE", "GL_POST", "PAYMENT_APPROVE"},
		delegatedBasis:   "delegated:from=p-boss",
	}
	r := newTestRouter(store)

	w := postJSON(t, r, handler.DelegatedAccessEvaluatePath,
		`{"principal_id":"p-1","legal_entity_id":"11111111-1111-4111-8111-aaaaaaaaaaa1"}`, adminHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		HasDelegatedAccess bool     `json:"has_delegated_access"`
		DelegatedActions   []string `json:"delegated_actions"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !resp.HasDelegatedAccess {
		t.Error("has_delegated_access = false with a non-empty delegated set")
	}
	if len(resp.DelegatedActions) != 2 {
		t.Errorf("delegated_actions = %v, want 2 (deduped)", resp.DelegatedActions)
	}
}

func TestEvaluateDelegatedAccess_NoDelegationReadsAsNoDelegatedGrant(t *testing.T) {
	r := newTestRouter(&stubStore{})

	w := postJSON(t, r, handler.DelegatedAccessEvaluatePath,
		`{"principal_id":"p-1","legal_entity_id":"11111111-1111-4111-8111-aaaaaaaaaaa1","action_type":"PAYMENT_APPROVE"}`, adminHeaders())
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		HasDelegatedAccess bool   `json:"has_delegated_access"`
		Basis              string `json:"basis"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.HasDelegatedAccess {
		t.Error("has_delegated_access = true with no delegation")
	}
	if resp.Basis != "no_delegated_grant" {
		t.Errorf("basis = %q, want no_delegated_grant", resp.Basis)
	}
}

func TestEvaluateDelegatedAccess_WritesNoDecisionArtifact(t *testing.T) {
	store := &stubStore{delegatedActions: []string{"PAYMENT_APPROVE"}, delegatedBasis: "delegated:from=p-boss"}
	r := newTestRouter(store)

	if w := postJSON(t, r, handler.DelegatedAccessEvaluatePath,
		`{"principal_id":"p-1","legal_entity_id":"11111111-1111-4111-8111-aaaaaaaaaaa1"}`, adminHeaders()); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if store.recordedParams.ActionType != "" {
		t.Fatalf("a decision artifact was recorded (%+v)", store.recordedParams)
	}
}

func TestEvaluateDelegatedAccess_RequiresPrincipalAndTenant(t *testing.T) {
	body := `{"principal_id":"p-1","legal_entity_id":"11111111-1111-4111-8111-aaaaaaaaaaa1"}`

	r := newTestRouter(&stubStore{})
	if w := postJSON(t, r, handler.DelegatedAccessEvaluatePath, body, map[string]string{"X-Tenant-Id": "11111111-1111-4111-8111-111111111111"}); w.Code != http.StatusUnauthorized {
		t.Errorf("no principal: expected 401, got %d", w.Code)
	}

	r = newTestRouter(&stubStore{})
	if w := postJSON(t, r, handler.DelegatedAccessEvaluatePath, body, map[string]string{"X-Principal-Id": "p-caller"}); w.Code != http.StatusUnauthorized {
		t.Errorf("no tenant: expected 401, got %d", w.Code)
	}
}
