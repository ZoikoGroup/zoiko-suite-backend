package context_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	identityctx "zoiko.io/identity-context-svc/internal/context"
	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/lifecycle"
	"zoiko.io/identity-context-svc/internal/outbox"
	"zoiko.io/identity-context-svc/internal/store"
)

// mockLifecycleStore embeds lifecycle.Store and overrides the methods each
// handler test path exercises. Un-overridden methods panic if called.
type mockLifecycleStore struct {
	lifecycle.Store

	createErr error
	detail    *store.PrincipalDetail
	detailErr error

	findBy    *store.PrincipalDetail
	findByErr error

	consumeErr   error
	consumeTok   *domain.CredentialToken
	consumeApply func(context.Context, pgx.Tx, domain.CredentialToken) ([]outbox.Record, error)

	createResetErr error

	getResetReq    *domain.PasswordResetRequest
	getResetReqErr error
	decideErr      error
	decideOut      *domain.PasswordResetRequest

	findCredErr error

	created []domain.NewPrincipal

	mergeRes     *store.MergeResult
	mergeErr     error
	mergeVersion int64
}

func (m *mockLifecycleStore) MergeSubjects(_ context.Context, _, survivorID, duplicateID, _, _, _ string,
	duplicateVersion int64, _ ...outbox.Record) (*store.MergeResult, error) {
	m.mergeVersion = duplicateVersion
	if m.mergeErr != nil {
		return nil, m.mergeErr
	}
	if m.mergeRes != nil {
		return m.mergeRes, nil
	}
	return &store.MergeResult{SurvivorID: survivorID, DuplicateID: duplicateID}, nil
}

func (m *mockLifecycleStore) CreatePrincipal(_ context.Context, np domain.NewPrincipal, _ ...outbox.Record) error {
	m.created = append(m.created, np)
	return m.createErr
}
func (m *mockLifecycleStore) GetPrincipalDetail(_ context.Context, _, _ string) (*store.PrincipalDetail, error) {
	return m.detail, m.detailErr
}
func (m *mockLifecycleStore) FindPrincipalBy(_ context.Context, _, _, _ string) (*store.PrincipalDetail, error) {
	return m.findBy, m.findByErr
}
func (m *mockLifecycleStore) IssueCredentialToken(_ context.Context, _ domain.CredentialToken) error {
	return nil
}
func (m *mockLifecycleStore) ConsumeCredentialToken(_ context.Context, _ string, _ string, _ time.Time,
	apply func(context.Context, pgx.Tx, domain.CredentialToken) ([]outbox.Record, error)) (*domain.CredentialToken, error) {
	if m.consumeErr != nil {
		return nil, m.consumeErr
	}
	if apply != nil {
		recs, err := apply(context.Background(), nil, domain.CredentialToken{})
		_ = recs
		return m.consumeTok, err
	}
	return m.consumeTok, nil
}
func (m *mockLifecycleStore) SetPasswordTx(_ context.Context, _ pgx.Tx, _, _, _ string) error {
	return nil
}
func (m *mockLifecycleStore) ActivatePrincipalTx(_ context.Context, _ pgx.Tx, _, _ string) error {
	return nil
}
func (m *mockLifecycleStore) ChangePassword(_ context.Context, _, _, _ string, _ ...outbox.Record) error {
	return nil
}
func (m *mockLifecycleStore) RecentPasswordHashes(_ context.Context, _, _ string, _ int) ([]string, error) {
	return nil, nil
}
func (m *mockLifecycleStore) CreateResetRequest(_ context.Context, _ domain.PasswordResetRequest, _ ...outbox.Record) error {
	return m.createResetErr
}
func (m *mockLifecycleStore) GetResetRequest(_ context.Context, _, _ string) (*domain.PasswordResetRequest, error) {
	return m.getResetReq, m.getResetReqErr
}
func (m *mockLifecycleStore) DecideResetRequest(_ context.Context, _, _ string, _ domain.ResetApproval,
	apply func(context.Context, pgx.Tx, *domain.PasswordResetRequest) error, _ ...outbox.Record) (*domain.PasswordResetRequest, error) {
	if apply != nil {
		_ = apply(context.Background(), nil, m.decideOut)
	}
	return m.decideOut, m.decideErr
}
func (m *mockLifecycleStore) CompleteResetRequestTx(_ context.Context, _ pgx.Tx, _ string) error {
	return nil
}
func (m *mockLifecycleStore) InsertCredentialTokenTx(_ context.Context, _ pgx.Tx, _ domain.CredentialToken) error {
	return nil
}
func (m *mockLifecycleStore) FindActiveCredentialByEmail(_ context.Context, _, _ string) (*domain.Principal, *domain.PrincipalCredential, error) {
	return nil, nil, m.findCredErr
}

func newLifecycleService(st *mockLifecycleStore) *lifecycle.Service {
	return lifecycle.New(st, nil, lifecycle.Policy{MinLength: 12}, nil, nil, lifecycle.Config{StepUpMaxAge: time.Minute}, nil, zap.NewNop())
}

func lifecycleRouter(ls *lifecycle.Service, az identityctx.AuthzChecker) chi.Router {
	h := identityctx.NewHandler(nil, nil, nil, nil, az, zap.NewNop())
	if ls != nil {
		h.WithLifecycle(ls)
	}
	r := chi.NewRouter()
	identityctx.RegisterRoutes(r, h)
	return r
}

func postLifecycle(t *testing.T, r chi.Router, path, tenantID, principalID string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf *bytes.Buffer
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		buf = bytes.NewBuffer(b)
	} else {
		buf = bytes.NewBuffer(nil)
	}
	req := httptest.NewRequest(http.MethodPost, path, buf)
	req.Header.Set("Content-Type", "application/json")
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	req.Header.Set("X-Correlation-ID", "test-correlation")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestAU09_CreateSubjectRequiresPermission(t *testing.T) {
	st := &mockLifecycleStore{detail: &store.PrincipalDetail{Principal: domain.Principal{PrincipalID: "p-new"}}}
	ls := newLifecycleService(st)
	r := lifecycleRouter(ls, &denyingAuthz{})

	w := postLifecycle(t, r, "/v1/principals", "tenant-a", "admin-1",
		map[string]any{"email": "a@zoiko.io", "display_name": "A Person", "principal_type": "HUMAN"})

	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
}

func TestAU09_CreateSubject_Success(t *testing.T) {
	st := &mockLifecycleStore{detail: &store.PrincipalDetail{Principal: domain.Principal{
		PrincipalID: "p-new", TenantID: "tenant-a", PrincipalType: domain.PrincipalTypeHuman,
		Status: domain.PrincipalStatusPendingActivation, DisplayName: "A Person"}}}
	ls := newLifecycleService(st)
	r := lifecycleRouter(ls, &permittingAuthz{})

	w := postLifecycle(t, r, "/v1/principals", "tenant-a", "admin-1",
		map[string]any{"email": "a@zoiko.io", "display_name": "A Person", "principal_type": "HUMAN"})

	assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var out map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, "p-new", out["principal_id"])
	assert.Equal(t, string(domain.PrincipalStatusPendingActivation), out["status"])
}

func TestAU09_CreateSubject_WorkloadActiveWithoutPassword(t *testing.T) {
	st := &mockLifecycleStore{detail: &store.PrincipalDetail{Principal: domain.Principal{
		PrincipalID: "w-1", TenantID: "tenant-a", PrincipalType: domain.PrincipalTypeServiceAccount,
		Status: domain.PrincipalStatusActive, DisplayName: "Workload"}}}
	ls := newLifecycleService(st)
	r := lifecycleRouter(ls, &permittingAuthz{})

	w := postLifecycle(t, r, "/v1/principals", "tenant-a", "admin-1",
		map[string]any{"display_name": "Workload", "principal_type": "SERVICE_ACCOUNT"})

	assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var out map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, string(domain.PrincipalStatusActive), out["status"])
}

func TestAU09_ForgotPasswordDoesNotEnumerate(t *testing.T) {
	known := &store.PrincipalDetail{Principal: domain.Principal{PrincipalID: "p-1", Email: "a@zoiko.io", Status: domain.PrincipalStatusActive}}
	r := lifecycleRouter(newLifecycleService(&mockLifecycleStore{findBy: known}), &permittingAuthz{})

	wKnown := postLifecycle(t, r, "/v1/account/password/forgot", "tenant-a", "", map[string]any{"email": "a@zoiko.io"})
	wUnknown := postLifecycle(t, r, "/v1/account/password/forgot", "tenant-a", "", map[string]any{"email": "nobody@zoiko.io"})

	assert.Equal(t, http.StatusAccepted, wKnown.Code, wKnown.Body.String())
	assert.Equal(t, http.StatusAccepted, wUnknown.Code, wUnknown.Body.String())
	assert.Equal(t, wKnown.Body.String(), wUnknown.Body.String(), "known and unknown accounts must answer identically")
}

func TestAU09_ActivationLinkSingleUse(t *testing.T) {
	st := &mockLifecycleStore{consumeErr: domain.ErrCredentialTokenUsed}
	r := lifecycleRouter(newLifecycleService(st), &permittingAuthz{})

	w := postLifecycle(t, r, "/v1/account/activate", "tenant-a", "", map[string]any{"token": "used-token", "password": "Secret12345678"})

	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "link_invalid")
}

func TestAU09_ResetRequesterCannotApprove(t *testing.T) {
	fx := newMFAFixture(t)
	st := &mockLifecycleStore{getResetReq: &domain.PasswordResetRequest{
		RequestID: "rr-1", RequestedBy: admin, PrincipalID: alice, Status: "PENDING_APPROVAL"}}
	r := steppedUpRouter(newLifecycleService(st), &permittingAuthz{}, fx)

	w := postAs(t, r, "/v1/password-resets/rr-1/decide", admin, fx.session(admin, 10*time.Second, true), "",
		map[string]any{"decision": "APPROVE"})

	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String(), "the requester cannot approve their own reset")
}

func TestAU09_ResetDecideRequiresTheApproversOwnStepUp(t *testing.T) {
	fx := newMFAFixture(t)
	st := &mockLifecycleStore{getResetReq: &domain.PasswordResetRequest{
		RequestID: "rr-1", RequestedBy: "P-OTHER", PrincipalID: alice, Status: "PENDING_APPROVAL"}}
	r := steppedUpRouter(newLifecycleService(st), &permittingAuthz{}, fx)

	// A password-only session: the approval needs a second factor the
	// session itself proves, not one the handler assumes.
	w := postAs(t, r, "/v1/password-resets/rr-1/decide", admin, fx.session(admin, 10*time.Second, false), "",
		map[string]any{"decision": "APPROVE"})

	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "step_up_required")
}

func TestAU09_ResetDecideNeedsApproveAction(t *testing.T) {
	r := lifecycleRouter(newLifecycleService(&mockLifecycleStore{}), &denyingAuthz{})

	w := postLifecycle(t, r, "/v1/password-resets/rr-1/decide", "tenant-a", "approver-1",
		map[string]any{"decision": "APPROVE"})

	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
}

func TestAU09_GetReset_RequiresReadGrant(t *testing.T) {
	r := lifecycleRouter(newLifecycleService(&mockLifecycleStore{}), &denyingAuthz{})

	req := httptest.NewRequest(http.MethodGet, "/v1/password-resets/rr-1", nil)
	req.Header.Set("X-Tenant-Id", "tenant-a")
	req.Header.Set("X-Principal-Id", "approver-1")
	req.Header.Set("X-Correlation-ID", "test-correlation")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
}
