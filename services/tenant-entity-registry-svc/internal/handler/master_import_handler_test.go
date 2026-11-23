package handler_test

// ORG-9 routes. partySvc also implements handler.MasterImportService, so the OpenAPI parity test covers them in both directions.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/identity-envelope/apicontract"
	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

const importID = "0190a000-0000-7000-8000-0000000a0001"

type importState struct {
	err     error
	gotKey  string
	gotID   string
	gotUp   domain.CreateMasterImportRequest
	gotAppr domain.ApproveMasterImportRequest
	gotRows domain.ImportRowQuery
	opState apicontract.OperationState
	rows    int
}

var importStates = map[*partySvc]*importState{}

func (p *partySvc) im() *importState {
	if importStates[p] == nil {
		importStates[p] = &importState{opState: apicontract.OperationQueued}
	}
	return importStates[p]
}

func (p *partySvc) op() apicontract.Operation {
	return apicontract.Operation{ID: "op_abc", TenantID: "t", Type: registry.OpMasterImportValidate, State: p.im().opState, Cancelable: true}
}

func (p *partySvc) CreateMasterImport(_ context.Context, req domain.CreateMasterImportRequest, key string) (*registry.MasterImportOutcome, error) {
	p.im().gotUp, p.im().gotKey = req, key
	if p.im().err != nil {
		return nil, p.im().err
	}
	return &registry.MasterImportOutcome{Import: &domain.MasterImport{ImportID: importID}, Operation: p.op(), Created: true}, nil
}
func (p *partySvc) ApproveMasterImport(_ context.Context, id string, req domain.ApproveMasterImportRequest, key string) (*registry.MasterImportOutcome, error) {
	p.im().gotID, p.im().gotAppr, p.im().gotKey = id, req, key
	if p.im().err != nil {
		return nil, p.im().err
	}
	return &registry.MasterImportOutcome{Import: &domain.MasterImport{ImportID: id}, Operation: p.op(), Created: true}, nil
}
func (p *partySvc) GetMasterImport(_ context.Context, id string) (*domain.MasterImport, error) {
	p.im().gotID = id
	return &domain.MasterImport{ImportID: id}, p.im().err
}
func (p *partySvc) ListImportRows(_ context.Context, id string, q domain.ImportRowQuery) ([]domain.ImportRow, error) {
	p.im().gotID, p.im().gotRows = id, q
	rows := make([]domain.ImportRow, p.im().rows)
	for i := range rows {
		rows[i] = domain.ImportRow{RowNo: i + 1}
	}
	return rows, p.im().err
}
func (p *partySvc) GetOperation(_ context.Context, id string) (apicontract.Operation, error) {
	p.im().gotID = id
	return p.op(), p.im().err
}
func (p *partySvc) CancelOperation(_ context.Context, id string) (apicontract.Operation, error) {
	p.im().gotID = id
	op := p.op()
	op.State = apicontract.OperationCancelRequested
	return op, p.im().err
}

func TestMasterImportRoutes_AbsentWhenTheServiceOffersNone(t *testing.T) {
	assert.Equal(t, http.StatusNotFound, do(newRouter(&stubSvc{}), http.MethodPost, "/v1/master-imports", `{}`).Code)
}

func TestMasterImportRoutes_UploadAnswers202WithALocationAndPassesTheKey(t *testing.T) {
	p := &partySvc{}
	r := newPartyRouter(p)
	body := `{"kind":"PARTY","source_name":"crm","source_version":"1","evidence_ref":"e","valid_from":"2026-01-01","rows":[{"row_ref":"a","party_kind":"LEGAL_ENTITY","names":[]}]}`
	rec := doWithHeader(r, http.MethodPost, "/v1/master-imports", body, "Idempotency-Key", "k-1")
	require.Equal(t, http.StatusAccepted, rec.Code)
	assert.Equal(t, "/v1/operations/op_abc", rec.Header().Get("Location"))
	assert.Contains(t, rec.Body.String(), `"state":"queued"`)
	assert.Equal(t, "k-1", p.im().gotKey)
	assert.Equal(t, "crm", p.im().gotUp.SourceName)
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodPost, "/v1/master-imports", "nope").Code)
}

func TestMasterImportRoutes_ApproveReadsAndOperations(t *testing.T) {
	p := &partySvc{}
	r := newPartyRouter(p)
	rec := do(r, http.MethodPost, "/v1/master-imports/"+importID+":approve", `{"expected_version":3,"evidence_ref":"e"}`)
	require.Equal(t, http.StatusAccepted, rec.Code)
	assert.Equal(t, "/v1/operations/op_abc", rec.Header().Get("Location"))
	assert.Equal(t, importID, p.im().gotID)
	assert.Equal(t, int64(3), p.im().gotAppr.ExpectedVersion)

	assert.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/master-imports/"+importID, "").Code)
	p.im().rows = 2
	rec = do(r, http.MethodGet, "/v1/master-imports/"+importID+"/rows?status=INVALID&after_row=4&limit=2", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, domain.ImportRowQuery{Status: "INVALID", AfterRow: 4, Limit: 2}, p.im().gotRows)
	assert.Contains(t, rec.Body.String(), `"next_after_row":2`, "a full page says where to continue")
	rec = do(r, http.MethodGet, "/v1/master-imports/"+importID+"/rows?limit=5", "")
	assert.NotContains(t, rec.Body.String(), "next_after_row", "a short page is the last")
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodGet, "/v1/master-imports/"+importID+"/rows?limit=x", "").Code)

	p.im().opState = apicontract.OperationRunning
	rec = do(r, http.MethodGet, "/v1/operations/op_abc", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"state":"running"`)
	assert.Empty(t, rec.Header().Get("Location"), "only an accepted request names where to poll")
	rec = do(r, http.MethodPost, "/v1/operations/op_abc:cancel", "")
	require.Equal(t, http.StatusAccepted, rec.Code)
	assert.Contains(t, rec.Body.String(), `"state":"cancel_requested"`)
}

func TestMasterImportRoutes_TypedErrors(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{registry.ErrNotFound, http.StatusNotFound, "NOT_FOUND"},
		{registry.ErrSelfApproval, http.StatusForbidden, "SOD_DENIED"},
		{registry.ErrStateConflict, http.StatusConflict, "INVALID_TRANSITION"},
		{registry.ErrIdempotencyKeyMismatch, http.StatusConflict, "IDEMPOTENCY_MISMATCH"},
		{registry.ErrVersionConflict, http.StatusConflict, "VERSION_CONFLICT"},
		{registry.ErrInvalidInput, http.StatusBadRequest, "VALIDATION_FAILED"},
	} {
		p := &partySvc{}
		p.im().err = tc.err
		rec := do(newPartyRouter(p), http.MethodPost, "/v1/operations/op_abc:cancel", "")
		assert.Equal(t, tc.status, rec.Code, tc.err.Error())
		assert.Contains(t, rec.Body.String(), tc.code, tc.err.Error())
	}
}

func doWithHeader(r http.Handler, method, path, body, header, value string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(header, value)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}
