package handler

// HTTP surface for REF-06 accounting books.
//
//	POST /v1/books                               CreateAccountingBook (201): a DRAFT book with its first basis
//	GET  /v1/books?group_code=&type=&status=     a group's books
//	GET  /v1/entities/{entityID}/books?type=&status=     ListEntityBooks
//	GET  /v1/books/{id}?as_of=&history=true      GetAccountingBook: the book as it was on that date
//	GET  /v1/books/{id}/as-of?as_of=             GetBookAsOf (as_of required)
//	GET  /v1/books/{id}/posting-eligibility?as_of=   whether the book may post on that date, and why not
//	GET  /v1/books/{id}/basis?as_of=             ExplainBookBasis
//	POST /v1/books/{id}/basis                    AmendBookBasis (201): the next basis version, prospective
//	POST /v1/books/{id}:activate                 ActivateBook
//	POST /v1/books/{id}:suspend-posting          SuspendPosting
//	POST /v1/books/{id}:retire                   RetireBook
//
// Mounted only when the service satisfies BookService, which *registry.Service does.

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

// BookService is the REF-06 half of the service contract.
type BookService interface {
	CreateAccountingBook(ctx context.Context, req domain.CreateAccountingBookRequest) (*registry.BookOutcome, error)
	AmendBookBasis(ctx context.Context, bookID string, req domain.AmendBookBasisRequest) (*registry.BookOutcome, error)
	ChangeBookStatus(ctx context.Context, bookID, to string, req domain.BookStatusRequest) (*registry.BookOutcome, error)
	GetAccountingBook(ctx context.Context, bookID string, asOf time.Time, history bool) (*domain.BookView, error)
	BookPostingEligibility(ctx context.Context, bookID string, asOf time.Time) (*domain.PostingEligibility, error)
	ListEntityBooks(ctx context.Context, q domain.BookSearchQuery) ([]domain.AccountingBook, error)
	ExplainBookBasis(ctx context.Context, bookID string, asOf time.Time) (*domain.BookBasisExplanation, error)
}

func registerBookRoutes(r chi.Router, h *Handler) {
	if _, ok := h.svc.(BookService); !ok {
		return
	}
	r.Post("/books", h.CreateAccountingBook)
	r.Get("/books", h.ListGroupBooks)
	r.Get("/entities/{entityID}/books", h.ListEntityBooks)
	r.Get("/books/{bookID}", h.GetAccountingBook)
	r.Get("/books/{bookID}/as-of", h.GetBookAsOf)
	r.Get("/books/{bookID}/posting-eligibility", h.GetBookPostingEligibility)
	r.Get("/books/{bookID}/basis", h.ExplainBookBasis)
	r.Post("/books/{bookID}/basis", h.AmendBookBasis)
	r.Post("/books/{bookID}:activate", h.bookStatus(domain.BookActive))
	r.Post("/books/{bookID}:suspend-posting", h.bookStatus(domain.BookPostingSuspended))
	r.Post("/books/{bookID}:retire", h.bookStatus(domain.BookRetired))
}

func (h *Handler) books() BookService {
	bs, _ := h.svc.(BookService)
	return bs
}

// CreateAccountingBook opens a DRAFT book.
func (h *Handler) CreateAccountingBook(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateAccountingBookRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.books().CreateAccountingBook(r.Context(), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// AmendBookBasis adds the next basis version.
func (h *Handler) AmendBookBasis(w http.ResponseWriter, r *http.Request) {
	var req domain.AmendBookBasisRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.books().AmendBookBasis(r.Context(), chi.URLParam(r, "bookID"), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// bookStatus is one named status command: the route, not the body, names the target status.
func (h *Handler) bookStatus(to string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req domain.BookStatusRequest
		if !decode(w, r, &req) {
			return
		}
		if req.CorrelationID == "" {
			req.CorrelationID = correlationID(r)
		}
		out, err := h.books().ChangeBookStatus(r.Context(), chi.URLParam(r, "bookID"), to, req)
		if err != nil {
			h.writeErr(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// GetAccountingBook returns the book as it was on as_of.
func (h *Handler) GetAccountingBook(w http.ResponseWriter, r *http.Request) {
	asOf, ok := asOfParam(w, r)
	if !ok {
		return
	}
	v, err := h.books().GetAccountingBook(r.Context(), chi.URLParam(r, "bookID"), asOf, r.URL.Query().Get("history") == "true")
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// GetBookAsOf is GetAccountingBook with the date required.
func (h *Handler) GetBookAsOf(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(r.URL.Query().Get("as_of")) == "" {
		writeErrJSON(w, r, http.StatusBadRequest, "as_of is required", correlationID(r))
		return
	}
	h.GetAccountingBook(w, r)
}

// GetBookPostingEligibility says whether the book may post on as_of.
func (h *Handler) GetBookPostingEligibility(w http.ResponseWriter, r *http.Request) {
	asOf, ok := asOfParam(w, r)
	if !ok {
		return
	}
	out, err := h.books().BookPostingEligibility(r.Context(), chi.URLParam(r, "bookID"), asOf)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ExplainBookBasis explains the basis in force.
func (h *Handler) ExplainBookBasis(w http.ResponseWriter, r *http.Request) {
	asOf, ok := asOfParam(w, r)
	if !ok {
		return
	}
	out, err := h.books().ExplainBookBasis(r.Context(), chi.URLParam(r, "bookID"), asOf)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) listBooks(w http.ResponseWriter, r *http.Request, q domain.BookSearchQuery) {
	q.Type = domain.BookType(r.URL.Query().Get("type"))
	q.Status = r.URL.Query().Get("status")
	out, err := h.books().ListEntityBooks(r.Context(), q)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// ListEntityBooks lists a legal entity's books.
func (h *Handler) ListEntityBooks(w http.ResponseWriter, r *http.Request) {
	h.listBooks(w, r, domain.BookSearchQuery{LegalEntityID: chi.URLParam(r, "entityID")})
}

// ListGroupBooks lists a group's books.
func (h *Handler) ListGroupBooks(w http.ResponseWriter, r *http.Request) {
	h.listBooks(w, r, domain.BookSearchQuery{GroupCode: r.URL.Query().Get("group_code")})
}

// The production service offers books; if it ever stops, the routes would silently vanish.
var _ BookService = (*registry.Service)(nil)
