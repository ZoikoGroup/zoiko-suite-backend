package arctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/ar07"
	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/kit"
	svcmiddleware "zoiko.io/accounts-receivable-svc/internal/middleware"
	"zoiko.io/accounts-receivable-svc/internal/svcclient"
)

type fakeAuthz struct{ allow map[string]bool }

func (f fakeAuthz) CheckAllowed(_ context.Context, p, _, action string) error {
	if f.allow[p+"|"+action] {
		return nil
	}
	return domain.ErrAuthorizationDenied
}

type fakeEntities struct{}

func (fakeEntities) VerifyInTenant(context.Context, string, string) error { return nil }

type fakeDocs struct{ err error }

func (f fakeDocs) ExpectedOpenItemDocuments(context.Context, string, string) ([]ExpectedDoc, error) {
	return nil, f.err
}

type fakeComp struct{}

func (fakeComp) Completeness(context.Context, string, string, []ar07.ExpectedDocument) (*ar07.CompletenessReport, error) {
	return &ar07.CompletenessReport{Balanced: true}, nil
}

func testModule(docErr error) *Module {
	deps := kit.Deps{Log: zap.NewNop(), Authz: fakeAuthz{allow: map[string]bool{"runner|" + ActionRunReconciliation: true}}, Entities: fakeEntities{},
		Clock: func() time.Time { return tNow }}
	return New(deps, Sources{Documents: fakeDocs{docErr}, Completeness: fakeComp{}, Postings: nil, Subledger: nil, GL: nil})
}

func post(m *Module, h http.HandlerFunc, principal string, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(b))
	req.Header.Set("X-Tenant-Id", "t1")
	req = req.WithContext(svcmiddleware.WithTenant(req.Context(), "t1"))
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func code(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct {
		Code string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &e), rec.Body.String())
	return e.Code
}

func TestRunCommand_Validation(t *testing.T) {
	m := testModule(nil)
	ent := "11111111-1111-4111-8111-111111111111"
	cases := []struct {
		name      string
		principal string
		body      map[string]any
		status    int
		code      string
	}{
		{"no principal", "", map[string]any{"legal_entity_id": ent, "as_of_date": "2026-09-30"}, 401, kit.CodeForbidden},
		{"no entity", "runner", map[string]any{"as_of_date": "2026-09-30"}, 400, kit.CodeValidationFailed},
		{"not authorised", "nobody", map[string]any{"legal_entity_id": ent, "as_of_date": "2026-09-30"}, 403, kit.CodeForbidden},
		{"bad date", "runner", map[string]any{"legal_entity_id": ent, "as_of_date": "30/09/2026"}, 400, kit.CodeValidationFailed},
		{"future date", "runner", map[string]any{"legal_entity_id": ent, "as_of_date": "2027-01-01"}, 400, kit.CodeValidationFailed},
		{"version without id", "runner", map[string]any{"legal_entity_id": ent, "as_of_date": "2026-09-30", "tolerance_policy_version": "3"}, 400, kit.CodeValidationFailed},
		{"bad supersedes", "runner", map[string]any{"legal_entity_id": ent, "as_of_date": "2026-09-30", "supersedes_run_id": "x"}, 400, kit.CodeValidationFailed},
		{"unknown field", "runner", map[string]any{"legal_entity_id": ent, "as_of_date": "2026-09-30", "tolerance": 5}, 400, kit.CodeValidationFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := post(m, m.runARGLReconciliation, tc.principal, tc.body)
			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			require.Equal(t, tc.code, code(t, rec))
		})
	}
}

// A source that cannot be read fails the run (503); nothing partial is recorded as complete.
func TestRunCommand_SourceFailureIs503(t *testing.T) {
	m := testModule(errors.New("db down"))
	rec := post(m, m.runARGLReconciliation, "runner", map[string]any{"legal_entity_id": "11111111-1111-4111-8111-111111111111", "as_of_date": "2026-09-30"})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	require.Equal(t, kit.CodeDependencyUnavailable, code(t, rec))
}

func TestAckCommand_Validation(t *testing.T) {
	m := testModule(nil)
	fid := "22222222-2222-4222-8222-222222222222"
	for name, body := range map[string]map[string]any{
		"bad finding id": {"finding_id": "x", "reason_code": "R", "evidence_id": "E"},
		"no reason":      {"finding_id": fid, "evidence_id": "E"},
		"no evidence":    {"finding_id": fid, "reason_code": "R"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := post(m, m.acknowledgeFinding, "ack", body)
			require.Equal(t, 400, rec.Code, rec.Body.String())
		})
	}
}

var _ GLBalanceSource = (*HTTPGLBalance)(nil)
var _ = svcclient.Caller{}
