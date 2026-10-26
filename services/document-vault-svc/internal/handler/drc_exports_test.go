package handler_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/document-vault-svc/internal/authz"
	"zoiko.io/document-vault-svc/internal/domain"
	"zoiko.io/document-vault-svc/internal/handler"
	svcmiddleware "zoiko.io/document-vault-svc/internal/middleware"
)

// POST /v1/exports (ZS-SVC-S-001 §9.2) over the real handler.

type exportStub struct {
	handler.PreservationStore
	created []domain.CreateExportPackageParams
	err     error
}

func (s *exportStub) CreateExportPackage(_ context.Context, p domain.CreateExportPackageParams) (*domain.ExportPackage, error) {
	if s.err != nil {
		return nil, s.err
	}
	s.created = append(s.created, p)
	return &domain.ExportPackage{PackageID: "pkg-1", PackageHash: strings.Repeat("a", 64), RecipientScope: p.RecipientScope, Purpose: p.Purpose}, nil
}

func exportRouter(t *testing.T, az authz.Client, ps *exportStub, identity bool) chi.Router {
	t.Helper()
	r := chi.NewRouter()
	if identity {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Header.Get("X-Principal-Id") == "" {
					req.Header.Set("X-Principal-Id", testPrincipal)
				}
				if req.Header.Get("X-Tenant-Id") == "" {
					req.Header.Set("X-Tenant-Id", testTenant)
				}
				next.ServeHTTP(w, req)
			})
		})
	}
	r.Use(svcmiddleware.TenantContext())
	h := handler.New(newStubStore(), newCounting(t), &stubResidency{}, az, newStubScanner(), zap.NewNop())
	handler.RegisterPreservationRoutes(r, h, ps)
	return r
}

const exportBody = `{"recipient_scope":" regulator:FCA ","purpose":"regulatory request R-77","items":[{"item_type":"DOCUMENT_VERSION","ref_id":"ver-1"},{"item_type":"RENDITION","ref_id":"rnd-1","omit":true,"omission_reason":"privileged"}]}`

func TestExports_BuildsAControlledPackage_WithRecipientAndPurpose(t *testing.T) {
	ps := &exportStub{}
	az := &entityActionAuthz{}
	w := call(exportRouter(t, az, ps, true), http.MethodPost, "/v1/exports", exportBody, nil)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"recipient_scope":"regulator:FCA"`)
	assert.Contains(t, w.Body.String(), `"purpose":"regulatory request R-77"`)
	require.Len(t, ps.created, 1)
	got := ps.created[0]
	assert.Equal(t, "regulator:FCA", got.RecipientScope, "trimmed")
	assert.Equal(t, testPrincipal, got.RequestedByPrincipalID)
	require.Len(t, got.Items, 2)
	assert.Equal(t, "ver-1", got.Items[0].RefID, "exact scope: the caller's ids, never 'latest'")
	assert.True(t, got.Items[1].Omit)
	assert.Equal(t, []string{"|EXPORT_PACKAGE_MANAGE"}, az.asked, "the same grant as the older route")
}

func TestExports_RecipientAndPurposeAreRequired(t *testing.T) {
	ps := &exportStub{}
	r := exportRouter(t, &entityActionAuthz{}, ps, true)
	for name, body := range map[string]string{
		"no recipient":    `{"purpose":"p","items":[{"item_type":"DOCUMENT_VERSION","ref_id":"v"}]}`,
		"blank recipient": `{"recipient_scope":"  ","purpose":"p","items":[{"item_type":"DOCUMENT_VERSION","ref_id":"v"}]}`,
		"no purpose":      `{"recipient_scope":"r","items":[{"item_type":"DOCUMENT_VERSION","ref_id":"v"}]}`,
		"blank purpose":   `{"recipient_scope":"r","purpose":" ","items":[{"item_type":"DOCUMENT_VERSION","ref_id":"v"}]}`,
	} {
		w := call(r, http.MethodPost, "/v1/exports", body, nil)
		assert.Equal(t, http.StatusBadRequest, w.Code, name)
		code, _ := errorCode(t, w)
		assert.Equal(t, "missing_field", code, name)
	}
	assert.Equal(t, http.StatusBadRequest, call(r, http.MethodPost, "/v1/exports",
		`{"recipient_scope":"`+strings.Repeat("r", 256)+`","purpose":"p","items":[{"item_type":"DOCUMENT_VERSION","ref_id":"v"}]}`, nil).Code, "over-long label")
	assert.Equal(t, http.StatusBadRequest, call(r, http.MethodPost, "/v1/exports",
		`{"recipient_scope":"r","purpose":"p","redaction_policy":"x","items":[]}`, nil).Code, "unknown fields are refused")
	assert.Empty(t, ps.created, "nothing is built for an incomplete request")
}

func TestExports_FailClosedAndStoreErrors(t *testing.T) {
	ps := &exportStub{}
	assert.Equal(t, http.StatusForbidden, call(exportRouter(t, &entityActionAuthz{denyAll: true}, ps, true), http.MethodPost, "/v1/exports", exportBody, nil).Code)
	assert.Equal(t, http.StatusServiceUnavailable, call(exportRouter(t, &entityActionAuthz{err: domain.ErrAuthzServiceUnavailable}, ps, true), http.MethodPost, "/v1/exports", exportBody, nil).Code)
	assert.Equal(t, http.StatusUnauthorized, call(exportRouter(t, &entityActionAuthz{}, ps, false), http.MethodPost, "/v1/exports", exportBody, nil).Code)
	assert.Empty(t, ps.created)

	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"no items":           {domain.ErrNoExportPackageItems, http.StatusBadRequest},
		"bad item type":      {domain.ErrInvalidExportItemType, http.StatusBadRequest},
		"omission no reason": {domain.ErrExportOmissionReasonRequired, http.StatusBadRequest},
		"version not found":  {domain.ErrExportItemRefNotFound, http.StatusNotFound},
	} {
		ps = &exportStub{err: tc.err}
		w := call(exportRouter(t, &entityActionAuthz{}, ps, true), http.MethodPost, "/v1/exports", exportBody, nil)
		assert.Equal(t, tc.want, w.Code, name)
	}
}

func TestExportPackages_OldRouteStillWorks_RecipientOptional(t *testing.T) {
	ps := &exportStub{}
	r := exportRouter(t, &entityActionAuthz{}, ps, true)
	w := call(r, http.MethodPost, "/v1/export-packages", `{"items":[{"item_type":"DOCUMENT_VERSION","ref_id":"ver-1"}]}`, nil)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	require.Len(t, ps.created, 1)
	assert.Empty(t, ps.created[0].RecipientScope)
	assert.NotContains(t, w.Body.String(), "recipient_scope")

	w = call(r, http.MethodPost, "/v1/export-packages", exportBody, nil)
	require.Equal(t, http.StatusCreated, w.Code, "the older route accepts them too")
	assert.Equal(t, "regulator:FCA", ps.created[1].RecipientScope)
}
