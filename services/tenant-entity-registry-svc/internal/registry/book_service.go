package registry

// REF-06 Accounting Book / Ledger Basis: CreateAccountingBook, ActivateBook, AmendBookBasis, SuspendPosting, RetireBook
// and the reads GetAccountingBook, ListEntityBooks, GetBookAsOf, ExplainBookBasis and the posting-eligibility answer
// (ZS-SVC-I-001 s4.16).
//
// A command on a book is authorized against the legal entity it belongs to, or the tenant for a group's book. A book is
// never defaulted or inferred: a posting service names the book and asks whether it may post. This service owns no ledger
// lines or balances and no policy content.

import (
	"context"
	"fmt"
	"time"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

func (s *Service) bookStore() (BookStore, error) {
	bs, ok := s.store.(BookStore)
	if !ok {
		return nil, ErrServiceUnavailable
	}
	return bs, nil
}

// bookIdentity loads the book to authorize against its owner; ErrNotFound when it is not the tenant's.
func (s *Service) bookIdentity(ctx context.Context, bs BookStore, id string) (*domain.AccountingBook, error) {
	b, err := bs.GetBook(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("store.GetBook: %w", err)
	}
	if b == nil {
		return nil, ErrNotFound
	}
	return b, nil
}

func (s *Service) authorizeBook(ctx context.Context, b *domain.AccountingBook, action string) error {
	if b.LegalEntityID != "" {
		return s.authorizeEntity(ctx, b.LegalEntityID, "accounting.book", action)
	}
	return s.authorize(ctx, "accounting.book", action)
}

// CreateAccountingBook is CreateAccountingBook.
func (s *Service) CreateAccountingBook(ctx context.Context, req domain.CreateAccountingBookRequest) (*BookOutcome, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if req.LegalEntityID != "" {
		if err := s.authorizeEntity(ctx, req.LegalEntityID, "accounting.book", "create"); err != nil {
			return nil, err
		}
	} else if err := s.authorize(ctx, "accounting.book", "create"); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	if req.LegalEntityID != "" {
		if err := s.assertEntityOperational(ctx, req.LegalEntityID); err != nil {
			return nil, err
		}
	}
	if err := s.checkCurrencies(ctx, req.FunctionalCurrency, req.PresentationCurrency); err != nil {
		return nil, err
	}
	bs, err := s.bookStore()
	if err != nil {
		return nil, err
	}
	out, err := bs.CreateAccountingBook(ctx, req, domain.PrincipalFromContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("store.CreateAccountingBook: %w", err)
	}
	return out, nil
}

// AmendBookBasis is AmendBookBasis.
func (s *Service) AmendBookBasis(ctx context.Context, bookID string, req domain.AmendBookBasisRequest) (*BookOutcome, error) {
	bs, err := s.bookStore()
	if err != nil {
		return nil, err
	}
	b, err := s.bookIdentity(ctx, bs, bookID)
	if err != nil {
		return nil, err
	}
	if err := req.ValidateFor(b.BookType); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.authorizeBook(ctx, b, "amend-basis"); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	if err := s.checkCurrencies(ctx, req.FunctionalCurrency, req.PresentationCurrency); err != nil {
		return nil, err
	}
	out, err := bs.AmendBookBasis(ctx, bookID, req, domain.PrincipalFromContext(ctx), s.expectedVersionOptional)
	if err != nil {
		return nil, fmt.Errorf("store.AmendBookBasis: %w", err)
	}
	return out, nil
}

// ChangeBookStatus is ActivateBook, SuspendPosting and RetireBook: to is ACTIVE, POSTING_SUSPENDED or RETIRED.
func (s *Service) ChangeBookStatus(ctx context.Context, bookID, to string, req domain.BookStatusRequest) (*BookOutcome, error) {
	command, action := "", ""
	switch to {
	case domain.BookActive:
		command, action = "ActivateBook", "activate"
	case domain.BookPostingSuspended:
		command, action = "SuspendPosting", "suspend-posting"
	case domain.BookRetired:
		command, action = "RetireBook", "retire"
	default:
		return nil, fmt.Errorf("%w: a book is activated, has posting suspended, or is retired by command", ErrInvalidInput)
	}
	if err := req.ValidateFor(to); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	bs, err := s.bookStore()
	if err != nil {
		return nil, err
	}
	b, err := s.bookIdentity(ctx, bs, bookID)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeBook(ctx, b, action); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	if to == domain.BookActive && b.LegalEntityID != "" {
		if err := s.assertEntityOperational(ctx, b.LegalEntityID); err != nil {
			return nil, err
		}
	}
	out, err := bs.ChangeBookStatus(ctx, bookID, command, to, req, domain.PrincipalFromContext(ctx), s.expectedVersionOptional)
	if err != nil {
		return nil, fmt.Errorf("store.ChangeBookStatus: %w", err)
	}
	return out, nil
}

// GetAccountingBook is GetAccountingBook and GetBookAsOf: the book as it was on asOf (now when zero).
func (s *Service) GetAccountingBook(ctx context.Context, bookID string, asOf time.Time, history bool) (*domain.BookView, error) {
	bs, err := s.bookStore()
	if err != nil {
		return nil, err
	}
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}
	v, err := bs.GetBookView(ctx, bookID, asOf, history)
	if err != nil {
		return nil, fmt.Errorf("store.GetBookView: %w", err)
	}
	if v == nil {
		return nil, ErrNotFound
	}
	return v, nil
}

// BookPostingEligibility says whether a book may take a posting on a date, and why not.
func (s *Service) BookPostingEligibility(ctx context.Context, bookID string, asOf time.Time) (*domain.PostingEligibility, error) {
	v, err := s.GetAccountingBook(ctx, bookID, asOf, false)
	if err != nil {
		return nil, err
	}
	return &v.Eligibility, nil
}

// ListEntityBooks is ListEntityBooks (and a group's books when the query names a group).
func (s *Service) ListEntityBooks(ctx context.Context, q domain.BookSearchQuery) ([]domain.AccountingBook, error) {
	if q.Type != "" && !q.Type.Valid() {
		return nil, fmt.Errorf("%w: type is not a book type", ErrInvalidInput)
	}
	switch q.Status {
	case "", domain.BookDraft, domain.BookActive, domain.BookPostingSuspended, domain.BookRetired:
	default:
		return nil, fmt.Errorf("%w: status is not a book status", ErrInvalidInput)
	}
	if (q.LegalEntityID == "") == (q.GroupCode == "") {
		return nil, fmt.Errorf("%w: name exactly one owner: a legal entity or a group", ErrInvalidInput)
	}
	bs, err := s.bookStore()
	if err != nil {
		return nil, err
	}
	out, err := bs.ListBooks(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store.ListBooks: %w", err)
	}
	return out, nil
}

// ExplainBookBasis is ExplainBookBasis.
func (s *Service) ExplainBookBasis(ctx context.Context, bookID string, asOf time.Time) (*domain.BookBasisExplanation, error) {
	bs, err := s.bookStore()
	if err != nil {
		return nil, err
	}
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}
	out, err := bs.ExplainBookBasis(ctx, bookID, asOf)
	if err != nil {
		return nil, fmt.Errorf("store.ExplainBookBasis: %w", err)
	}
	if out == nil {
		return nil, ErrNotFound
	}
	return out, nil
}
