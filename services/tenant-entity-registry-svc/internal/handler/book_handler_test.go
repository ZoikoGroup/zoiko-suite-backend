package handler_test

// REF-06 routes. partySvc also implements handler.BookService, so the OpenAPI parity test covers them in both directions.

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

const bookID = "0190a000-0000-7000-8000-0000000b0001"

type bookState struct {
	err      error
	gotTo    string
	gotReq   domain.BookStatusRequest
	gotAmend domain.AmendBookBasisRequest
	gotCr    domain.CreateAccountingBookRequest
	gotID    string
	gotAsOf  time.Time
	gotHist  bool
	gotList  domain.BookSearchQuery
}

var bookStates = map[*partySvc]*bookState{}

func (p *partySvc) bk() *bookState {
	if bookStates[p] == nil {
		bookStates[p] = &bookState{}
	}
	return bookStates[p]
}

func (p *partySvc) CreateAccountingBook(_ context.Context, req domain.CreateAccountingBookRequest) (*registry.BookOutcome, error) {
	p.bk().gotCr = req
	return &registry.BookOutcome{}, p.bk().err
}
func (p *partySvc) AmendBookBasis(_ context.Context, id string, req domain.AmendBookBasisRequest) (*registry.BookOutcome, error) {
	p.bk().gotID, p.bk().gotAmend = id, req
	return &registry.BookOutcome{}, p.bk().err
}
func (p *partySvc) ChangeBookStatus(_ context.Context, id, to string, req domain.BookStatusRequest) (*registry.BookOutcome, error) {
	p.bk().gotID, p.bk().gotTo, p.bk().gotReq = id, to, req
	return &registry.BookOutcome{}, p.bk().err
}
func (p *partySvc) GetAccountingBook(_ context.Context, id string, asOf time.Time, history bool) (*domain.BookView, error) {
	p.bk().gotID, p.bk().gotAsOf, p.bk().gotHist = id, asOf, history
	return &domain.BookView{}, p.bk().err
}
func (p *partySvc) BookPostingEligibility(_ context.Context, id string, asOf time.Time) (*domain.PostingEligibility, error) {
	p.bk().gotID, p.bk().gotAsOf = id, asOf
	return &domain.PostingEligibility{}, p.bk().err
}
func (p *partySvc) ListEntityBooks(_ context.Context, q domain.BookSearchQuery) ([]domain.AccountingBook, error) {
	p.bk().gotList = q
	return []domain.AccountingBook{}, p.bk().err
}
func (p *partySvc) ExplainBookBasis(_ context.Context, id string, asOf time.Time) (*domain.BookBasisExplanation, error) {
	p.bk().gotID, p.bk().gotAsOf = id, asOf
	return &domain.BookBasisExplanation{}, p.bk().err
}

const bookBody = `{"legal_entity_id":"e","book_code":"STAT","name":"n","book_type":"STATUTORY","purpose":"p","framework":"IFRS","functional_currency":"USD","presentation_currency":"USD","evidence_ref":"e"}`

func TestBookRoutes_AbsentWhenTheServiceOffersNone(t *testing.T) {
	assert.Equal(t, http.StatusNotFound, do(newRouter(&stubSvc{}), http.MethodPost, "/v1/books", bookBody).Code)
}

func TestBookRoutes_Commands(t *testing.T) {
	p := &partySvc{}
	r := newPartyRouter(p)
	assert.Equal(t, http.StatusCreated, do(r, http.MethodPost, "/v1/books", bookBody).Code)
	assert.Equal(t, domain.BookStatutory, p.bk().gotCr.BookType)
	assert.Equal(t, domain.FrameworkIFRS, p.bk().gotCr.Framework)
	assert.NotEmpty(t, p.bk().gotCr.CorrelationID)
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodPost, "/v1/books", "nope").Code)

	assert.Equal(t, http.StatusCreated, do(r, http.MethodPost, "/v1/books/"+bookID+"/basis", `{"expected_version":2,"reason":"r","framework":"LOCAL_GAAP","framework_detail":"FRS 102","functional_currency":"GBP","presentation_currency":"USD","evidence_ref":"e"}`).Code)
	assert.Equal(t, bookID, p.bk().gotID)
	assert.Equal(t, int64(2), p.bk().gotAmend.ExpectedVersion)
	assert.Equal(t, "FRS 102", p.bk().gotAmend.FrameworkDetail)

	for path, to := range map[string]string{":activate": domain.BookActive, ":suspend-posting": domain.BookPostingSuspended, ":retire": domain.BookRetired} {
		rec := do(r, http.MethodPost, "/v1/books/"+bookID+path, `{"expected_version":3,"reason":"r","evidence_ref":"e","status":"ACTIVE"}`)
		assert.Equal(t, http.StatusOK, rec.Code, path)
		assert.Equal(t, to, p.bk().gotTo, "the route, not the body, names the target status")
		assert.Equal(t, int64(3), p.bk().gotReq.ExpectedVersion)
	}
}

func TestBookRoutes_Reads(t *testing.T) {
	p := &partySvc{}
	r := newPartyRouter(p)
	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/books/"+bookID+"?as_of=2025-06-01&history=true", "").Code)
	assert.Equal(t, time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC), p.bk().gotAsOf)
	assert.True(t, p.bk().gotHist)
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodGet, "/v1/books/"+bookID+"?as_of=junk", "").Code)

	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodGet, "/v1/books/"+bookID+"/as-of", "").Code, "GetBookAsOf needs its date")
	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/books/"+bookID+"/as-of?as_of=2025-09-01", "").Code)
	assert.Equal(t, time.Date(2025, 9, 1, 0, 0, 0, 0, time.UTC), p.bk().gotAsOf)
	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/books/"+bookID+"/posting-eligibility?as_of=2025-09-01", "").Code)
	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/books/"+bookID+"/basis?as_of=2025-09-01", "").Code)

	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/entities/"+estEntityID+"/books?type=TAX&status=ACTIVE", "").Code)
	assert.Equal(t, domain.BookSearchQuery{LegalEntityID: estEntityID, Type: domain.BookTax, Status: "ACTIVE"}, p.bk().gotList)
	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/books?group_code=GRP", "").Code)
	assert.Equal(t, domain.BookSearchQuery{GroupCode: "GRP"}, p.bk().gotList)
}

func TestBookRoutes_TypedErrors(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{registry.ErrNotFound, http.StatusNotFound, "NOT_FOUND"},
		{registry.ErrSelfApproval, http.StatusForbidden, "SOD_DENIED"},
		{registry.ErrStateConflict, http.StatusConflict, "INVALID_TRANSITION"},
		{registry.ErrConflict, http.StatusConflict, "DUPLICATE_CANDIDATE"},
		{registry.ErrVersionConflict, http.StatusConflict, "VERSION_CONFLICT"},
		{registry.ErrReferenceInvalid, http.StatusBadRequest, "REFERENCE_RETIRED"},
		{registry.ErrEntityNotOperational, http.StatusConflict, "INVALID_TRANSITION"},
	} {
		p := &partySvc{}
		p.bk().err = tc.err
		rec := do(newPartyRouter(p), http.MethodPost, "/v1/books/"+bookID+":activate", `{"expected_version":1,"evidence_ref":"e"}`)
		assert.Equal(t, tc.status, rec.Code, tc.err.Error())
		assert.Contains(t, rec.Body.String(), tc.code, tc.err.Error())
	}
}
