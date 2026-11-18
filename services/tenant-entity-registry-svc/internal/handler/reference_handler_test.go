package handler_test

// MDM-04 routes. partySvc also implements handler.ReferenceService, so the OpenAPI parity test covers them in both directions.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

type refState struct {
	err    error
	gotSet string
	gotVer string
	gotStg domain.StageReferenceReleaseRequest
	gotQ   domain.ReferenceQuery
}

var refStates = map[*partySvc]*refState{}

func (p *partySvc) rf() *refState {
	if refStates[p] == nil {
		refStates[p] = &refState{}
	}
	return refStates[p]
}

func (p *partySvc) CreateReferenceSet(context.Context, domain.CreateReferenceSetRequest) (*domain.ReferenceSet, error) {
	return &domain.ReferenceSet{}, p.rf().err
}
func (p *partySvc) ListReferenceSets(context.Context) ([]domain.ReferenceSet, error) {
	return []domain.ReferenceSet{}, p.rf().err
}
func (p *partySvc) StageReferenceRelease(_ context.Context, set string, req domain.StageReferenceReleaseRequest) (*domain.ReferenceRelease, error) {
	p.rf().gotSet, p.rf().gotStg = set, req
	return &domain.ReferenceRelease{}, p.rf().err
}
func (p *partySvc) PublishReferenceRelease(_ context.Context, set, version string, _ domain.PublishReferenceReleaseRequest) (*domain.ReferenceRelease, error) {
	p.rf().gotSet, p.rf().gotVer = set, version
	return &domain.ReferenceRelease{}, p.rf().err
}
func (p *partySvc) ListReferenceReleases(_ context.Context, set string) ([]domain.ReferenceRelease, error) {
	p.rf().gotSet = set
	return []domain.ReferenceRelease{}, p.rf().err
}
func (p *partySvc) ResolveReference(_ context.Context, q domain.ReferenceQuery) (*domain.ReferenceResolution, error) {
	p.rf().gotQ = q
	return &domain.ReferenceResolution{}, p.rf().err
}

func TestReferenceRoutes_AbsentWhenTheServiceOffersNone(t *testing.T) {
	assert.Equal(t, http.StatusNotFound, do(newRouter(&stubSvc{}), http.MethodPost, "/v1/reference-sets", `{"set_code":"AB"}`).Code)
}

func TestReferenceRoutes_Commands(t *testing.T) {
	p := &partySvc{}
	r := newPartyRouter(p)
	assert.Equal(t, http.StatusCreated, do(r, http.MethodPost, "/v1/reference-sets", `{"set_code":"ISO_3166_COUNTRY","name":"n","source_authority":"ISO"}`).Code)
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodPost, "/v1/reference-sets", "nope").Code)
	assert.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/reference-sets", "").Code)
	assert.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/reference-sets/ISO_3166_COUNTRY/releases", "").Code)
	assert.Equal(t, "ISO_3166_COUNTRY", p.rf().gotSet)

	body := `{"release_version":"2026.1","source_published_at":"2026-01-01T00:00:00Z","effective_from":"2026-01-01","compatibility":"AUTO_ADOPT","evidence_ref":"e","entries":[{"code":"GB","change_type":"ADD","label":"United Kingdom"}]}`
	assert.Equal(t, http.StatusCreated, do(r, http.MethodPost, "/v1/reference-sets/ISO_3166_COUNTRY/releases", body).Code)
	assert.Equal(t, "2026.1", p.rf().gotStg.ReleaseVersion)
	require.Len(t, p.rf().gotStg.Entries, 1)
	assert.Equal(t, domain.ChangeAdd, p.rf().gotStg.Entries[0].ChangeType)

	assert.Equal(t, http.StatusOK, do(r, http.MethodPost, "/v1/reference-sets/ISO_3166_COUNTRY/releases/2026.1:publish", `{"evidence_ref":"e"}`).Code)
	assert.Equal(t, "2026.1", p.rf().gotVer)
	assert.Equal(t, "ISO_3166_COUNTRY", p.rf().gotSet)
}

func TestReferenceRoutes_Resolve(t *testing.T) {
	p := &partySvc{}
	r := newPartyRouter(p)
	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/reference/ISO_3166_COUNTRY/GB?as_of=2026-06-01&release=2026.1&history=true", "").Code)
	assert.Equal(t, domain.ReferenceQuery{SetCode: "ISO_3166_COUNTRY", Code: "GB", AsOf: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), Release: "2026.1", History: true}, p.rf().gotQ)
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodGet, "/v1/reference/ISO_3166_COUNTRY/GB?as_of=junk", "").Code)
}

func TestReferenceRoutes_TypedErrors(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{registry.ErrNotFound, http.StatusNotFound, "NOT_FOUND"},
		{registry.ErrSelfApproval, http.StatusForbidden, "SOD_DENIED"},
		{registry.ErrStateConflict, http.StatusConflict, "INVALID_TRANSITION"},
		{registry.ErrConflict, http.StatusConflict, "DUPLICATE_CANDIDATE"},
		{registry.ErrInvalidInput, http.StatusBadRequest, "VALIDATION_FAILED"},
	} {
		p := &partySvc{}
		p.rf().err = tc.err
		rec := do(newPartyRouter(p), http.MethodPost, "/v1/reference-sets/ISO_3166_COUNTRY/releases/2026.1:publish", `{"evidence_ref":"e"}`)
		assert.Equal(t, tc.status, rec.Code, tc.err.Error())
		assert.Contains(t, rec.Body.String(), tc.code, tc.err.Error())
	}
}
