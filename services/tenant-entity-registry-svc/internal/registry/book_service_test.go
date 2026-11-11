package registry_test

// REF-06 service: validation, authorization at the book's own owner, currencies confirmed read-only, no default book,
// and a store without books answering "unavailable".

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

const bookEntity = "0190a000-0000-7000-8000-00000000dddd"

type bookFake struct {
	*memStore
	registry.BookStore
	calls  int
	actor  string
	book   *domain.AccountingBook
	lastTo string
	view   *domain.BookView
}

func (b *bookFake) CreateAccountingBook(_ context.Context, _ domain.CreateAccountingBookRequest, actor string) (*registry.BookOutcome, error) {
	b.calls++
	b.actor = actor
	return &registry.BookOutcome{}, nil
}
func (b *bookFake) AmendBookBasis(_ context.Context, _ string, _ domain.AmendBookBasisRequest, actor string, _ bool) (*registry.BookOutcome, error) {
	b.calls++
	b.actor = actor
	return &registry.BookOutcome{}, nil
}
func (b *bookFake) ChangeBookStatus(_ context.Context, _, command, to string, _ domain.BookStatusRequest, actor string, _ bool) (*registry.BookOutcome, error) {
	b.calls++
	b.actor, b.lastTo = actor, command+":"+to
	return &registry.BookOutcome{}, nil
}
func (b *bookFake) GetBook(_ context.Context, id string) (*domain.AccountingBook, error) {
	b.calls++
	if id == "missing" {
		return nil, nil
	}
	cp := *b.book
	return &cp, nil
}
func (b *bookFake) GetBookView(_ context.Context, id string, _ time.Time, _ bool) (*domain.BookView, error) {
	b.calls++
	if id == "missing" {
		return nil, nil
	}
	return b.view, nil
}
func (b *bookFake) ListBooks(context.Context, domain.BookSearchQuery) ([]domain.AccountingBook, error) {
	b.calls++
	return nil, nil
}
func (b *bookFake) ExplainBookBasis(_ context.Context, id string, _ time.Time) (*domain.BookBasisExplanation, error) {
	b.calls++
	if id == "missing" {
		return nil, nil
	}
	return &domain.BookBasisExplanation{}, nil
}

func bookSvc(t *testing.T, az registry.AuthorizationClient, owner string, typ domain.BookType) (*registry.Service, *bookFake) {
	t.Helper()
	f := &bookFake{memStore: newMemStore(), book: &domain.AccountingBook{BookID: "b1", BookType: typ, Status: domain.BookDraft},
		view: &domain.BookView{Eligibility: domain.PostingEligibility{BookID: "b1", Eligible: true}}}
	if owner == "entity" {
		f.book.LegalEntityID = bookEntity
	} else {
		f.book.GroupCode = "GRP"
	}
	f.entities[bookEntity] = &domain.LegalEntity{LegalEntityID: bookEntity, TenantID: "tenant-001", EntityStatus: domain.EntityStatusActive}
	return newSvc(t, f, az, acceptAllJurisd{}).WithCurrencyValidator(goodCurrencies()), f
}

func validBookReq() domain.CreateAccountingBookRequest {
	return domain.CreateAccountingBookRequest{LegalEntityID: bookEntity, BookCode: "STAT", Name: "n", BookType: domain.BookStatutory, Purpose: "p",
		BookBasisFields: domain.BookBasisFields{Framework: domain.FrameworkIFRS, FunctionalCurrency: "USD", PresentationCurrency: "EUR", EvidenceRef: "e"}}
}

func TestBook_CreateValidatesThenAuthorizesThenChecksCurrenciesThenStores(t *testing.T) {
	svc, f := bookSvc(t, permitAllAuthZ{}, "entity", domain.BookStatutory)
	bad := validBookReq()
	bad.Purpose = ""
	_, err := svc.CreateAccountingBook(tenantCtx("tenant-001"), bad)
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	assert.Zero(t, f.calls)

	az := &recordingAuthZ{err: registry.ErrUnauthorized}
	denied, f2 := bookSvc(t, az, "entity", domain.BookStatutory)
	_, err = denied.CreateAccountingBook(tenantCtx("tenant-001"), validBookReq())
	require.ErrorIs(t, err, registry.ErrUnauthorized)
	assert.Equal(t, "accounting.book", az.resource)
	assert.Equal(t, "create", az.action)
	assert.Equal(t, bookEntity, az.scopeID, "an entity's book is authorized at that entity")
	assert.Zero(t, f2.calls)

	unknown := validBookReq()
	unknown.PresentationCurrency = "ZZZ"
	_, err = svc.CreateAccountingBook(tenantCtx("tenant-001"), unknown)
	require.ErrorIs(t, err, registry.ErrReferenceInvalid, "both currencies are confirmed before anything is stored")
	assert.Zero(t, f.calls)

	_, err = svc.CreateAccountingBook(tenantCtx("tenant-001"), validBookReq())
	require.NoError(t, err)
	assert.Equal(t, testPrincipal, f.actor)

	f.tenants["tenant-001"].LifecycleState = domain.TenantLifecycleSuspended
	calls := f.calls
	_, err = svc.CreateAccountingBook(tenantCtx("tenant-001"), validBookReq())
	require.Error(t, err)
	assert.Equal(t, calls, f.calls)

	// A group's book is authorized at the tenant, not at an entity.
	gaz := &recordingAuthZ{}
	gsvc, _ := bookSvc(t, gaz, "group", domain.BookConsolidation)
	g := validBookReq()
	g.LegalEntityID, g.GroupCode, g.BookType = "", "GRP", domain.BookConsolidation
	_, err = gsvc.CreateAccountingBook(tenantCtx("tenant-001"), g)
	require.NoError(t, err)
	assert.NotEqual(t, bookEntity, gaz.scopeID)
}

func TestBook_CommandsAreAuthorizedAsTheirOwnActionsAtTheBooksOwner(t *testing.T) {
	status := func(to string) func(*registry.Service) error {
		return func(s *registry.Service) error {
			_, err := s.ChangeBookStatus(tenantCtx("tenant-001"), "b1", to, domain.BookStatusRequest{ExpectedVersion: 1, Reason: "r", EvidenceRef: "e"})
			return err
		}
	}
	for name, tc := range map[string]struct {
		action, command string
		run             func(*registry.Service) error
	}{
		"activate": {"activate", "ActivateBook:ACTIVE", status(domain.BookActive)},
		"suspend":  {"suspend-posting", "SuspendPosting:POSTING_SUSPENDED", status(domain.BookPostingSuspended)},
		"retire":   {"retire", "RetireBook:RETIRED", status(domain.BookRetired)},
		"amend": {"amend-basis", "", func(s *registry.Service) error {
			_, err := s.AmendBookBasis(tenantCtx("tenant-001"), "b1", domain.AmendBookBasisRequest{ExpectedVersion: 1, Reason: "r",
				BookBasisFields: domain.BookBasisFields{Framework: domain.FrameworkIFRS, FunctionalCurrency: "EUR", PresentationCurrency: "EUR", EvidenceRef: "e"}})
			return err
		}},
	} {
		t.Run(name, func(t *testing.T) {
			az := &recordingAuthZ{}
			svc, f := bookSvc(t, az, "entity", domain.BookStatutory)
			require.NoError(t, tc.run(svc))
			assert.Equal(t, "accounting.book", az.resource)
			assert.Equal(t, tc.action, az.action)
			assert.Equal(t, bookEntity, az.scopeID)
			assert.Equal(t, tc.command, f.lastTo)
			assert.Equal(t, testPrincipal, f.actor)

			deny, f2 := bookSvc(t, &recordingAuthZ{err: registry.ErrUnauthorized}, "entity", domain.BookStatutory)
			require.ErrorIs(t, tc.run(deny), registry.ErrUnauthorized)
			assert.Empty(t, f2.lastTo, "a denied caller changes nothing")
		})
	}

	svc, _ := bookSvc(t, permitAllAuthZ{}, "entity", domain.BookStatutory)
	_, err := svc.ChangeBookStatus(tenantCtx("tenant-001"), "missing", domain.BookActive, domain.BookStatusRequest{ExpectedVersion: 1, EvidenceRef: "e"})
	require.ErrorIs(t, err, registry.ErrNotFound)
	_, err = svc.ChangeBookStatus(tenantCtx("tenant-001"), "b1", domain.BookDraft, domain.BookStatusRequest{ExpectedVersion: 1, EvidenceRef: "e"})
	require.ErrorIs(t, err, registry.ErrInvalidInput, "nothing moves back to draft")
	_, err = svc.ChangeBookStatus(tenantCtx("tenant-001"), "b1", domain.BookRetired, domain.BookStatusRequest{ExpectedVersion: 1, EvidenceRef: "e"})
	require.ErrorIs(t, err, registry.ErrInvalidInput, "retiring states why")

	// The amend request is checked against the book's own type: a tax book is not on IFRS.
	tax, f := bookSvc(t, permitAllAuthZ{}, "entity", domain.BookTax)
	_, err = tax.AmendBookBasis(tenantCtx("tenant-001"), "b1", domain.AmendBookBasisRequest{ExpectedVersion: 1, Reason: "r",
		BookBasisFields: domain.BookBasisFields{Framework: domain.FrameworkIFRS, FunctionalCurrency: "EUR", PresentationCurrency: "EUR", EvidenceRef: "e"}})
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	assert.Empty(t, f.actor)
}

func TestBook_ActivationOfAnInactiveEntityIsRefused(t *testing.T) {
	svc, f := bookSvc(t, permitAllAuthZ{}, "entity", domain.BookStatutory)
	f.entities[bookEntity].EntityStatus = domain.EntityStatusDraft
	_, err := svc.ChangeBookStatus(tenantCtx("tenant-001"), "b1", domain.BookActive, domain.BookStatusRequest{ExpectedVersion: 1, EvidenceRef: "e"})
	require.ErrorIs(t, err, registry.ErrEntityNotOperational)
	assert.Empty(t, f.lastTo)
}

func TestBook_ReadsAndTheEligibilityAnswer(t *testing.T) {
	svc, f := bookSvc(t, permitAllAuthZ{}, "entity", domain.BookStatutory)
	v, err := svc.GetAccountingBook(tenantCtx("tenant-001"), "b1", time.Time{}, false)
	require.NoError(t, err)
	assert.NotNil(t, v)
	_, err = svc.GetAccountingBook(tenantCtx("tenant-001"), "missing", time.Time{}, false)
	require.ErrorIs(t, err, registry.ErrNotFound)

	e, err := svc.BookPostingEligibility(tenantCtx("tenant-001"), "b1", time.Time{})
	require.NoError(t, err)
	assert.True(t, e.Eligible)
	f.view = &domain.BookView{Eligibility: domain.PostingEligibility{BookID: "b1", Reason: "RETIRED"}}
	e, err = svc.BookPostingEligibility(tenantCtx("tenant-001"), "b1", time.Time{})
	require.NoError(t, err)
	assert.False(t, e.Eligible)
	assert.Equal(t, "RETIRED", e.Reason)
	_, err = svc.BookPostingEligibility(tenantCtx("tenant-001"), "missing", time.Time{})
	require.ErrorIs(t, err, registry.ErrNotFound)

	_, err = svc.ExplainBookBasis(tenantCtx("tenant-001"), "missing", time.Time{})
	require.ErrorIs(t, err, registry.ErrNotFound)
	_, err = svc.ExplainBookBasis(tenantCtx("tenant-001"), "b1", time.Time{})
	require.NoError(t, err)

	for name, q := range map[string]domain.BookSearchQuery{
		"no owner":   {},
		"two owners": {LegalEntityID: bookEntity, GroupCode: "G"},
		"bad type":   {LegalEntityID: bookEntity, Type: "NOPE"},
		"bad status": {LegalEntityID: bookEntity, Status: "NOPE"},
	} {
		_, err := svc.ListEntityBooks(tenantCtx("tenant-001"), q)
		assert.ErrorIs(t, err, registry.ErrInvalidInput, name)
	}
	_, err = svc.ListEntityBooks(tenantCtx("tenant-001"), domain.BookSearchQuery{LegalEntityID: bookEntity, Type: domain.BookTax, Status: domain.BookActive})
	require.NoError(t, err)

	bare := newSvc(t, newMemStore(), permitAllAuthZ{}, acceptAllJurisd{})
	_, err = bare.GetAccountingBook(tenantCtx("tenant-001"), "b1", time.Time{}, false)
	require.ErrorIs(t, err, registry.ErrServiceUnavailable)
	_, err = bare.ListEntityBooks(tenantCtx("tenant-001"), domain.BookSearchQuery{LegalEntityID: bookEntity})
	require.ErrorIs(t, err, registry.ErrServiceUnavailable)
}
