package registry

import (
	"context"
	"time"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

// BookStore is the data-access contract for REF-06 accounting books (migration 000023). Asserted by the service like
// PartyStore. Every method runs in the caller's tenant under row-level security.
type BookStore interface {
	// CreateAccountingBook opens a DRAFT book with its first basis version. A book code is unique within its entity or
	// group for ever (ErrConflict).
	CreateAccountingBook(ctx context.Context, req domain.CreateAccountingBookRequest, actor string) (*BookOutcome, error)
	// AmendBookBasis adds the next basis version under the book's row lock. It is prospective: for an active book the
	// new basis starts today or later, so how earlier periods were booked is never rewritten.
	AmendBookBasis(ctx context.Context, bookID string, req domain.AmendBookBasisRequest, actor string, allowUnversioned bool) (*BookOutcome, error)
	// ChangeBookStatus is ActivateBook, SuspendPosting or RetireBook. Activating a material book is not done by its
	// creator (ErrSelfApproval), and a second live book of the same entity, type and framework is refused.
	ChangeBookStatus(ctx context.Context, bookID, command, to string, req domain.BookStatusRequest, actor string, allowUnversioned bool) (*BookOutcome, error)

	// GetBook returns the book's identity without a lock; (nil, nil) when absent.
	GetBook(ctx context.Context, bookID string) (*domain.AccountingBook, error)
	// GetBookView returns the book as it was on asOf: its status then, the basis in force then and whether it could
	// post. (nil, nil) when absent.
	GetBookView(ctx context.Context, bookID string, asOf time.Time, history bool) (*domain.BookView, error)
	ListBooks(ctx context.Context, q domain.BookSearchQuery) ([]domain.AccountingBook, error)
	ExplainBookBasis(ctx context.Context, bookID string, asOf time.Time) (*domain.BookBasisExplanation, error)
}

// BookOutcome is a book after a command, with the basis and the step it recorded.
type BookOutcome struct {
	Book       domain.AccountingBook   `json:"book"`
	Basis      domain.BookBasisVersion `json:"basis"`
	Transition *domain.BookTransition  `json:"transition,omitempty"`
}
