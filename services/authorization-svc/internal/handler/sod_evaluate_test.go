package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

// sodStore answers the two calls EvaluateSoD makes; anything else would hit
// the nil embedded interface and panic.
type sodStore struct {
	AuthorizationStore
	held     map[string][]string // principal → actions held in the tenant
	conflict map[string]string   // action → the held action it conflicts with
	fail     bool
}

func (s sodStore) FindGrantedActionsInTenant(_ context.Context, principalID, _ string) ([]string, error) {
	if s.fail {
		return nil, errors.New("down")
	}
	return s.held[principalID], nil
}

func (s sodStore) CheckSoDConflict(_ context.Context, held []string, candidate, _ string) (string, bool, error) {
	want, ok := s.conflict[candidate]
	if !ok {
		return "", false, nil
	}
	for _, h := range held {
		if h == want {
			return h, true, nil
		}
	}
	return "", false, nil
}

func evaluate(t *testing.T, store AuthorizationStore, body map[string]string) (int, sodEvaluateResponse) {
	t.Helper()
	h := &Handler{store: store, log: zap.NewNop()}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, SoDEvaluatePath, bytes.NewReader(b))
	req.Header.Set("X-Principal-Id", "maker")
	req.Header.Set("X-Tenant-Id", "7a000000-0000-4000-8000-000000000001")
	rr := httptest.NewRecorder()
	h.EvaluateSoD(rr, req)
	var out sodEvaluateResponse
	_ = json.NewDecoder(rr.Body).Decode(&out)
	return rr.Code, out
}

func TestEvaluateSoD(t *testing.T) {
	base := sodStore{
		held:     map[string][]string{"auditor": {"SECURITY_AUDIT"}, "admin": {"IDENTITY_ADMIN"}},
		conflict: map[string]string{"ATTACH_SUPPORT_CONTEXT": "SECURITY_AUDIT"},
	}
	cases := []struct {
		name       string
		body       map[string]string
		wantResult string
		wantID     string
	}{
		{"clean maker-checker", map[string]string{"action_type": "ATTACH_SUPPORT_CONTEXT", "maker_principal_id": "admin", "checker_principal_id": "boss", "subject_principal_id": "support-1"}, "NO_CONFLICT", ""},
		{"maker approves own act", map[string]string{"action_type": "ATTACH_SUPPORT_CONTEXT", "maker_principal_id": "admin", "checker_principal_id": "admin", "subject_principal_id": "support-1"}, "CONFLICT", "maker_is_checker"},
		{"approver approves own access", map[string]string{"action_type": "ATTACH_SUPPORT_CONTEXT", "maker_principal_id": "admin", "checker_principal_id": "support-1", "subject_principal_id": "support-1"}, "CONFLICT", "checker_is_subject"},
		{"suspending yourself", map[string]string{"action_type": "UPDATE_PRINCIPAL_STATUS", "maker_principal_id": "admin", "subject_principal_id": "admin"}, "CONFLICT", "maker_is_subject"},
		{"checker holds a conflicting duty", map[string]string{"action_type": "ATTACH_SUPPORT_CONTEXT", "maker_principal_id": "admin", "checker_principal_id": "auditor", "subject_principal_id": "support-1"}, "CONFLICT", "held_duty_conflict"},
		{"maker holds a conflicting duty", map[string]string{"action_type": "ATTACH_SUPPORT_CONTEXT", "maker_principal_id": "auditor", "checker_principal_id": "boss", "subject_principal_id": "support-1"}, "CONFLICT", "held_duty_conflict"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, out := evaluate(t, base, c.body)
			if code != http.StatusOK || out.Result != c.wantResult || out.ConflictID != c.wantID || out.RuleVersion == "" {
				t.Errorf("got %d %+v, want %s %s", code, out, c.wantResult, c.wantID)
			}
		})
	}

	// A check that could not run is 503 — the caller fails closed — never NO_CONFLICT.
	if code, out := evaluate(t, sodStore{fail: true}, map[string]string{"action_type": "X", "maker_principal_id": "admin"}); code != http.StatusServiceUnavailable || out.Result == "NO_CONFLICT" {
		t.Errorf("store failure: got %d %+v", code, out)
	}
	if code, _ := evaluate(t, base, map[string]string{"action_type": "X"}); code != http.StatusBadRequest {
		t.Errorf("missing maker: want 400, got %d", code)
	}
}
