package store_test

// REF-06 accounting books against a real Postgres: separate books and parallel books, versioned effective-dated bases
// that are prospective, a status lifecycle reconstructable by date, segregation of duties on material books, and what
// the database holds whatever the service does.

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

const (
	bookCreator   = "p-book-creator"
	bookActivator = "p-book-activator"
)

func (f *orgFixture) bookReq(code string, typ domain.BookType, fw domain.BookFramework) domain.CreateAccountingBookRequest {
	b := domain.BookBasisFields{Framework: fw, FunctionalCurrency: "USD", PresentationCurrency: "USD", ValidFrom: "2025-01-01", EvidenceRef: "board-minute-1"}
	if fw == domain.FrameworkLocalGAAP {
		b.FrameworkDetail = "UK GAAP FRS 102"
	}
	return domain.CreateAccountingBookRequest{LegalEntityID: f.entityID, BookCode: code, Name: code + " book", BookType: typ, Purpose: "test", BookBasisFields: b}
}

func (f *orgFixture) createBook(t *testing.T, req domain.CreateAccountingBookRequest) *registry.BookOutcome {
	t.Helper()
	out, err := f.s.CreateAccountingBook(f.ctx, req, bookCreator)
	require.NoError(t, err)
	return out
}

func (f *orgFixture) bookStatus(t *testing.T, id, command, to string, version int64, actor, eff string) (*registry.BookOutcome, error) {
	t.Helper()
	return f.s.ChangeBookStatus(f.ctx, id, command, to, domain.BookStatusRequest{ExpectedVersion: version, Reason: "reason", EvidenceRef: "evidence", EffectiveDate: eff}, actor, false)
}

func (f *orgFixture) liveBook(t *testing.T, code string, typ domain.BookType, fw domain.BookFramework) *domain.AccountingBook {
	t.Helper()
	b := f.createBook(t, f.bookReq(code, typ, fw))
	out, err := f.bookStatus(t, b.Book.BookID, "ActivateBook", domain.BookActive, 1, bookActivator, "2025-02-01")
	require.NoError(t, err)
	return &out.Book
}

func TestBook_CreateStartsDraftWithItsFirstBasis(t *testing.T) {
	f := newORGFixture(t)
	out := f.createBook(t, f.bookReq("STAT-IFRS", domain.BookStatutory, domain.FrameworkIFRS))
	assert.Equal(t, domain.BookDraft, out.Book.Status)
	assert.True(t, out.Book.Material)
	assert.Equal(t, 1, out.Basis.VersionNo)
	assert.Equal(t, domain.FrameworkIFRS, out.Basis.Framework)
	require.NotNil(t, out.Transition)
	assert.Equal(t, 1, out.Transition.Seq)
	assert.Equal(t, domain.BookDraft, out.Transition.To)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'accounting.book.created' AND tenant_id = $1`, f.tenantID))
	assert.False(t, f.createBook(t, f.bookReq("MGMT-1", domain.BookManagement, domain.FrameworkManagement)).Book.Material, "a management book is not material")
}

func TestBook_ACodeIsUniqueWithinItsOwnerAndNeverReused(t *testing.T) {
	f := newORGFixture(t)
	a := f.createBook(t, f.bookReq("MAIN", domain.BookStatutory, domain.FrameworkIFRS))
	_, err := f.s.CreateAccountingBook(f.ctx, f.bookReq("MAIN", domain.BookManagement, domain.FrameworkManagement), bookCreator)
	require.ErrorIs(t, err, registry.ErrConflict, "unique within the entity, whatever the type")

	_, err = f.bookStatus(t, a.Book.BookID, "RetireBook", domain.BookRetired, 1, bookActivator, "2025-03-01")
	require.NoError(t, err)
	_, err = f.s.CreateAccountingBook(f.ctx, f.bookReq("MAIN", domain.BookStatutory, domain.FrameworkIFRS), bookCreator)
	require.ErrorIs(t, err, registry.ErrConflict, "a retired code is not reused")

	// Another group and another entity have their own namespace.
	g := domain.CreateAccountingBookRequest{GroupCode: "GRP-A", BookCode: "MAIN", Name: "n", BookType: domain.BookConsolidation, Purpose: "p",
		BookBasisFields: domain.BookBasisFields{Framework: domain.FrameworkIFRS, FunctionalCurrency: "USD", PresentationCurrency: "USD", EvidenceRef: "e"}}
	f.createBook(t, g)
	_, err = f.s.CreateAccountingBook(f.ctx, g, bookCreator)
	require.ErrorIs(t, err, registry.ErrConflict)
	g.GroupCode = "GRP-B"
	f.createBook(t, g)

	nobody := f.bookReq("X", domain.BookStatutory, domain.FrameworkIFRS)
	nobody.LegalEntityID = uuid.NewString()
	_, err = f.s.CreateAccountingBook(f.ctx, nobody, bookCreator)
	require.ErrorIs(t, err, registry.ErrNotFound)
}

func TestBook_TheTableHoldsTheShapeRules(t *testing.T) {
	f := newORGFixture(t)
	b := f.createBook(t, f.bookReq("MAIN", domain.BookStatutory, domain.FrameworkIFRS))
	id := b.Book.BookID
	require.Error(t, f.execTenant(t, `INSERT INTO accounting_books (book_id, tenant_id, legal_entity_id, group_code, book_code, name, book_type, purpose, material, created_by)
		VALUES (gen_random_uuid(), $1, $2, 'X', 'B1', 'n', 'CONSOLIDATION', 'p', false, 'x')`, f.tenantID, f.entityID), "an entity and a group together")
	require.Error(t, f.execTenant(t, `INSERT INTO accounting_books (book_id, tenant_id, book_code, name, book_type, purpose, material, created_by)
		VALUES (gen_random_uuid(), $1, 'B2', 'n', 'CONSOLIDATION', 'p', false, 'x')`, f.tenantID), "neither")
	require.Error(t, f.execTenant(t, `INSERT INTO accounting_books (book_id, tenant_id, group_code, book_code, name, book_type, purpose, material, created_by)
		VALUES (gen_random_uuid(), $1, 'G', 'B3', 'n', 'STATUTORY', 'p', true, 'x')`, f.tenantID), "a group owns no statutory book")
	require.Error(t, f.execTenant(t, `INSERT INTO accounting_books (book_id, tenant_id, legal_entity_id, book_code, name, book_type, purpose, material, created_by)
		VALUES (gen_random_uuid(), $1, $2, 'lower case', 'n', 'STATUTORY', 'p', true, 'x')`, f.tenantID, f.entityID), "code shape")
	require.Error(t, f.execTenant(t, `UPDATE accounting_books SET book_code = 'CHANGED' WHERE book_id = $1`, id), "identity is immutable")
	require.Error(t, f.execTenant(t, `UPDATE accounting_books SET book_type = 'MANAGEMENT' WHERE book_id = $1`, id))
	require.Error(t, f.execTenant(t, `DELETE FROM accounting_books WHERE book_id = $1`, id))
	require.Error(t, f.execTenant(t, `UPDATE accounting_books SET status = 'ACTIVE', activated_by = created_by WHERE book_id = $1`, id),
		"a material book cannot be activated by its creator even if the service is bypassed")
	require.Error(t, f.execTenant(t, `UPDATE book_basis_versions SET functional_currency = 'EUR' WHERE book_id = $1`, id), "a basis version is immutable")
	require.Error(t, f.execTenant(t, `DELETE FROM book_basis_versions WHERE book_id = $1`, id))
	require.Error(t, f.execTenant(t, `UPDATE book_transitions SET reason = 'x' WHERE book_id = $1`, id), "transitions are append-only")
	require.Error(t, f.execTenant(t, `DELETE FROM book_transitions WHERE book_id = $1`, id))
	require.Error(t, f.execTenant(t, `INSERT INTO book_basis_versions (basis_id, tenant_id, book_id, version_no, framework, functional_currency, presentation_currency, valid_from, evidence_ref, recorded_by)
		VALUES (gen_random_uuid(), $1, $2, 2, 'IFRS', 'USD', 'USD', '2026-01-01', 'e', 'x')`, f.tenantID, id), "one open basis version at a time")
	require.Error(t, f.execTenant(t, `INSERT INTO book_basis_versions (basis_id, tenant_id, book_id, version_no, framework, functional_currency, presentation_currency, valid_from, evidence_ref, recorded_by)
		VALUES (gen_random_uuid(), $1, $2, 2, 'LOCAL_GAAP', 'USD', 'USD', '2026-01-01', 'e', 'x')`, f.tenantID, id), "a local framework is named")
}

func TestBook_AMaterialBookIsNotActivatedByItsCreator(t *testing.T) {
	f := newORGFixture(t)
	stat := f.createBook(t, f.bookReq("STAT", domain.BookStatutory, domain.FrameworkIFRS))
	_, err := f.bookStatus(t, stat.Book.BookID, "ActivateBook", domain.BookActive, 1, bookCreator, "2025-02-01")
	require.ErrorIs(t, err, registry.ErrSelfApproval)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM book_transitions WHERE book_id = $1`, stat.Book.BookID), "a refused activation records nothing")

	ok, err := f.bookStatus(t, stat.Book.BookID, "ActivateBook", domain.BookActive, 1, bookActivator, "2025-02-01")
	require.NoError(t, err)
	assert.Equal(t, domain.BookActive, ok.Book.Status)
	assert.Equal(t, bookActivator, ok.Book.ActivatedBy)
	assert.Equal(t, int64(2), ok.Book.RecordVersion)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'accounting.book.activated' AND tenant_id = $1`, f.tenantID))

	mgmt := f.createBook(t, f.bookReq("MGMT", domain.BookManagement, domain.FrameworkManagement))
	_, err = f.bookStatus(t, mgmt.Book.BookID, "ActivateBook", domain.BookActive, 1, bookCreator, "2025-02-01")
	require.NoError(t, err, "a management book may be activated by its creator")
}

func TestBook_ParallelBooksStaySeparateAndAmbiguityIsRefused(t *testing.T) {
	f := newORGFixture(t)
	first := f.liveBook(t, "STAT-IFRS", domain.BookStatutory, domain.FrameworkIFRS)

	// A second live statutory book on the same framework would make "the" book ambiguous.
	dup := f.createBook(t, f.bookReq("STAT-IFRS-2", domain.BookStatutory, domain.FrameworkIFRS))
	_, err := f.bookStatus(t, dup.Book.BookID, "ActivateBook", domain.BookActive, 1, bookActivator, "2025-02-01")
	require.ErrorIs(t, err, registry.ErrConflict)
	assert.Equal(t, domain.BookDraft, f.bookView(t, dup.Book.BookID, time.Now().UTC()).Book.Status)

	// A parallel book on another framework, a tax book and a management book can be live beside it.
	f.liveBook(t, "STAT-LOCAL", domain.BookStatutory, domain.FrameworkLocalGAAP)
	f.liveBook(t, "TAX-1", domain.BookTax, domain.FrameworkTax)
	f.liveBook(t, "MGMT-1", domain.BookManagement, domain.FrameworkIFRS)
	f.liveBook(t, "MGMT-2", domain.BookManagement, domain.FrameworkIFRS)

	// Retiring the first frees its framework.
	_, err = f.bookStatus(t, first.BookID, "RetireBook", domain.BookRetired, first.RecordVersion, bookActivator, "2025-06-01")
	require.NoError(t, err)
	_, err = f.bookStatus(t, dup.Book.BookID, "ActivateBook", domain.BookActive, 1, bookActivator, "2025-07-01")
	require.NoError(t, err)

	// Two activations racing for one framework make one live book.
	a := f.createBook(t, f.bookReq("TAXA", domain.BookTax, domain.FrameworkTax))
	b := f.createBook(t, f.bookReq("TAXB", domain.BookTax, domain.FrameworkTax))
	_, err = f.bookStatus(t, f.liveBookID(t, "TAX-1"), "RetireBook", domain.BookRetired, 2, bookActivator, "2025-08-01")
	require.NoError(t, err)
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for _, id := range []string{a.Book.BookID, b.Book.BookID, a.Book.BookID, b.Book.BookID} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_, err := f.s.ChangeBookStatus(f.ctx, id, "ActivateBook", domain.BookActive, domain.BookStatusRequest{Reason: "r", EvidenceRef: "e", EffectiveDate: "2025-09-01"}, bookActivator, true)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				wins++
			}
		}(id)
	}
	wg.Wait()
	assert.Equal(t, 1, wins)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM accounting_books WHERE tenant_id = $1 AND book_type = 'TAX' AND status = 'ACTIVE'`, f.tenantID))
}

func (f *orgFixture) liveBookID(t *testing.T, code string) string {
	t.Helper()
	var id string
	require.NoError(t, f.pool.QueryRow(context.Background(), `SELECT book_id::text FROM accounting_books WHERE tenant_id = $1 AND book_code = $2`, f.tenantID, code).Scan(&id))
	return id
}

func (f *orgFixture) bookView(t *testing.T, id string, asOf time.Time) *domain.BookView {
	t.Helper()
	v, err := f.s.GetBookView(f.ctx, id, asOf, true)
	require.NoError(t, err)
	require.NotNil(t, v)
	return v
}

func TestBook_LifecycleAndWhetherItCouldPostOnAnyDate(t *testing.T) {
	f := newORGFixture(t)
	b := f.createBook(t, f.bookReq("STAT", domain.BookStatutory, domain.FrameworkIFRS))
	id := b.Book.BookID

	_, err := f.bookStatus(t, id, "SuspendPosting", domain.BookPostingSuspended, 1, bookActivator, "2025-02-01")
	require.ErrorIs(t, err, registry.ErrStateConflict, "a draft has no posting to suspend")
	_, err = f.bookStatus(t, id, "ActivateBook", domain.BookActive, 9, bookActivator, "2025-02-01")
	require.ErrorIs(t, err, registry.ErrVersionConflict)
	_, err = f.s.ChangeBookStatus(f.ctx, id, "ActivateBook", domain.BookActive, domain.BookStatusRequest{EvidenceRef: "e"}, bookActivator, false)
	require.ErrorIs(t, err, registry.ErrVersionRequired)

	act, err := f.bookStatus(t, id, "ActivateBook", domain.BookActive, 1, bookActivator, "2025-02-01")
	require.NoError(t, err)
	_, err = f.bookStatus(t, id, "SuspendPosting", domain.BookPostingSuspended, 2, bookActivator, "2025-01-15")
	require.ErrorIs(t, err, registry.ErrInvalidInput, "not before the last change")
	sus, err := f.bookStatus(t, id, "SuspendPosting", domain.BookPostingSuspended, act.Book.RecordVersion, bookActivator, "2025-06-01")
	require.NoError(t, err)
	re, err := f.bookStatus(t, id, "ActivateBook", domain.BookActive, sus.Book.RecordVersion, bookActivator, "2025-09-01")
	require.NoError(t, err)
	ret, err := f.bookStatus(t, id, "RetireBook", domain.BookRetired, re.Book.RecordVersion, bookActivator, "2026-01-01")
	require.NoError(t, err)
	_, err = f.bookStatus(t, id, "ActivateBook", domain.BookActive, ret.Book.RecordVersion, bookActivator, "2026-02-01")
	require.ErrorIs(t, err, registry.ErrStateConflict, "RETIRED is final")

	for name, tc := range map[string]struct {
		date     string
		status   string
		eligible bool
		reason   string
	}{
		"before the book began": {"2024-12-01", "", false, "NOT_YET_EFFECTIVE"},
		"a draft":               {"2025-01-15", domain.BookDraft, false, "NOT_ACTIVE"},
		"active":                {"2025-03-01", domain.BookActive, true, ""},
		"posting suspended":     {"2025-07-01", domain.BookPostingSuspended, false, "POSTING_SUSPENDED"},
		"active again":          {"2025-10-01", domain.BookActive, true, ""},
		"the day it retired":    {"2026-01-01", domain.BookRetired, false, "RETIRED"},
	} {
		d, _ := domain.ParseDate(tc.date)
		v := f.bookView(t, id, d)
		assert.Equal(t, tc.status, v.StatusAsOf, name)
		assert.Equal(t, tc.eligible, v.Eligibility.Eligible, name)
		assert.Equal(t, tc.reason, v.Eligibility.Reason, name)
	}
	now := f.bookView(t, id, time.Now().UTC())
	assert.Equal(t, domain.BookRetired, now.Book.Status)
	assert.False(t, now.Eligibility.Eligible, "a retired book takes no new posting")
	require.NotNil(t, now.Basis, "but its basis is retained for history and report lineage")
	require.Len(t, now.History, 5)
	assert.Equal(t, "SuspendPosting", now.History[2].Command)
	for name, n := range map[string]int{"accounting.book.activated": 2, "accounting.book.posting_suspended": 1, "accounting.book.retired": 1} {
		assert.Equal(t, n, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = $2 AND tenant_id = $1`, f.tenantID, name), name)
	}
}

func TestBook_BasisIsVersionedAndAChangeNeverRewritesHistory(t *testing.T) {
	f := newORGFixture(t)
	b := f.liveBook(t, "STAT", domain.BookStatutory, domain.FrameworkIFRS)
	id := b.BookID
	future := time.Now().UTC().AddDate(0, 0, 30).Format("2006-01-02")
	nb := domain.BookBasisFields{Framework: domain.FrameworkLocalGAAP, FrameworkDetail: "UK GAAP FRS 102", FunctionalCurrency: "GBP", PresentationCurrency: "USD",
		ChartRef: "chart-uk-2027", PolicyRef: "policy-v3", ValidFrom: future, EvidenceRef: "board-minute-9"}
	amend := func(v int64, f2 domain.BookBasisFields) (*registry.BookOutcome, error) {
		return f.s.AmendBookBasis(f.ctx, id, domain.AmendBookBasisRequest{ExpectedVersion: v, Reason: "move to local statutory reporting", BookBasisFields: f2}, bookActivator, false)
	}

	// Prospective only: nothing may start before today for a book in use, and nothing may reach back.
	back := nb
	back.ValidFrom = "2025-06-01"
	_, err := amend(b.RecordVersion, back)
	require.ErrorIs(t, err, registry.ErrInvalidInput, "a basis change cannot rewrite how earlier periods were booked")
	same := domain.BookBasisFields{Framework: domain.FrameworkIFRS, FunctionalCurrency: "USD", PresentationCurrency: "USD", ValidFrom: future, EvidenceRef: "e"}
	_, err = amend(b.RecordVersion, same)
	require.ErrorIs(t, err, registry.ErrInvalidInput, "an amendment that changes nothing is refused")
	bad := nb
	bad.Framework = domain.FrameworkTax
	_, err = amend(b.RecordVersion, bad)
	require.ErrorIs(t, err, registry.ErrInvalidInput, "a statutory book is not on a tax basis")
	_, err = amend(9, nb)
	require.ErrorIs(t, err, registry.ErrVersionConflict)

	out, err := amend(b.RecordVersion, nb)
	require.NoError(t, err)
	assert.Equal(t, 2, out.Basis.VersionNo)
	assert.Equal(t, 2, out.Book.CurrentBasis)
	assert.Equal(t, "chart-uk-2027", out.Basis.ChartRef)
	assert.Equal(t, b.RecordVersion+1, out.Book.RecordVersion)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'accounting.book.basis_changed' AND tenant_id = $1`, f.tenantID))

	before := f.bookView(t, id, time.Now().UTC().AddDate(0, 0, 10))
	require.NotNil(t, before.Basis)
	assert.Equal(t, domain.FrameworkIFRS, before.Basis.Framework, "until the change takes effect, the old basis")
	assert.Equal(t, "USD", before.Basis.FunctionalCurrency)
	assert.Equal(t, future, before.Basis.ValidTo)
	after := f.bookView(t, id, time.Now().UTC().AddDate(0, 0, 31))
	assert.Equal(t, domain.FrameworkLocalGAAP, after.Basis.Framework)
	assert.Equal(t, "GBP", after.Basis.FunctionalCurrency)
	past := f.bookView(t, id, time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC))
	assert.Equal(t, domain.FrameworkIFRS, past.Basis.Framework, "a historical basis version is reconstructable")

	// A retired book keeps its basis and takes no new one.
	_, err = f.bookStatus(t, id, "RetireBook", domain.BookRetired, out.Book.RecordVersion, bookActivator, "2025-12-31")
	require.NoError(t, err)
	_, err = amend(out.Book.RecordVersion+1, domain.BookBasisFields{Framework: domain.FrameworkIFRS, FunctionalCurrency: "EUR", PresentationCurrency: "EUR", ValidFrom: time.Now().UTC().AddDate(0, 0, 60).Format("2006-01-02"), EvidenceRef: "e"})
	require.ErrorIs(t, err, registry.ErrStateConflict)
}

func TestBook_ADraftBasisCanBeCorrectedOnItsFirstDay(t *testing.T) {
	f := newORGFixture(t)
	today := time.Now().UTC().Format("2006-01-02")
	req := f.bookReq("DRAFTY", domain.BookStatutory, domain.FrameworkIFRS)
	req.ValidFrom = today
	b := f.createBook(t, req)
	fix := domain.BookBasisFields{Framework: domain.FrameworkIFRS, FunctionalCurrency: "EUR", PresentationCurrency: "EUR", ValidFrom: today, EvidenceRef: "corrected"}
	out, err := f.s.AmendBookBasis(f.ctx, b.Book.BookID, domain.AmendBookBasisRequest{ExpectedVersion: 1, Reason: "wrong currency entered", BookBasisFields: fix}, bookCreator, false)
	require.NoError(t, err, "a draft was never in use, so its basis may be corrected on the day it starts")
	assert.Equal(t, "EUR", out.Basis.FunctionalCurrency)
	v := f.bookView(t, b.Book.BookID, time.Now().UTC())
	require.NotNil(t, v.Basis)
	assert.Equal(t, 2, v.Basis.VersionNo, "the replaced version is zero-length and never in force")

	earlier := fix
	earlier.ValidFrom = "2024-01-01"
	earlier.FunctionalCurrency = "GBP"
	_, err = f.s.AmendBookBasis(f.ctx, b.Book.BookID, domain.AmendBookBasisRequest{ExpectedVersion: 2, Reason: "r", BookBasisFields: earlier}, bookCreator, false)
	require.ErrorIs(t, err, registry.ErrInvalidInput)
}

func TestBook_ExplainBookBasisShowsTheSiblingsAndTheEntityCurrency(t *testing.T) {
	f := newORGFixture(t)
	stat := f.liveBook(t, "STAT", domain.BookStatutory, domain.FrameworkIFRS)
	f.liveBook(t, "LOCAL", domain.BookStatutory, domain.FrameworkLocalGAAP)
	f.createBook(t, f.bookReq("TAX", domain.BookTax, domain.FrameworkTax))
	eur := f.bookReq("EURBOOK", domain.BookManagement, domain.FrameworkManagement)
	eur.FunctionalCurrency = "EUR"
	f.createBook(t, eur)

	ex, err := f.s.ExplainBookBasis(f.ctx, stat.BookID, time.Now().UTC())
	require.NoError(t, err)
	require.NotNil(t, ex)
	assert.Equal(t, domain.FrameworkIFRS, ex.InForce.Framework)
	assert.Len(t, ex.Versions, 1)
	assert.Equal(t, "USD", ex.EntityDefaultCurrency)
	require.NotNil(t, ex.FunctionalMatchesEntity)
	assert.True(t, *ex.FunctionalMatchesEntity)
	assert.Len(t, ex.ParallelBooks, 3)
	byCode := map[string]domain.ParallelBook{}
	for _, p := range ex.ParallelBooks {
		byCode[p.BookCode] = p
	}
	assert.Equal(t, domain.FrameworkLocalGAAP, byCode["LOCAL"].Framework)
	assert.Equal(t, domain.BookTax, byCode["TAX"].BookType)
	assert.Equal(t, domain.BookDraft, byCode["TAX"].Status)
	assert.NotEmpty(t, ex.Notes)
	assert.Contains(t, ex.AllowedFrameworks, domain.FrameworkIFRS)
	assert.NotContains(t, ex.AllowedFrameworks, domain.FrameworkTax)

	e2, err := f.s.ExplainBookBasis(f.ctx, f.liveBookID(t, "EURBOOK"), time.Now().UTC())
	require.NoError(t, err)
	require.NotNil(t, e2.FunctionalMatchesEntity)
	assert.False(t, *e2.FunctionalMatchesEntity, "a book may differ from its entity's currency; the difference is stated, not forbidden")

	none, err := f.s.ExplainBookBasis(f.ctx, uuid.NewString(), time.Now().UTC())
	require.NoError(t, err)
	assert.Nil(t, none)
}

func TestBook_ListAndTenantIsolation(t *testing.T) {
	a := newORGFixture(t)
	b := secondTenant(t, a)
	a.liveBook(t, "STAT", domain.BookStatutory, domain.FrameworkIFRS)
	a.createBook(t, a.bookReq("MGMT", domain.BookManagement, domain.FrameworkManagement))
	a.createBook(t, domain.CreateAccountingBookRequest{GroupCode: "GRP", BookCode: "CONS", Name: "n", BookType: domain.BookConsolidation, Purpose: "p",
		BookBasisFields: domain.BookBasisFields{Framework: domain.FrameworkIFRS, FunctionalCurrency: "USD", PresentationCurrency: "USD", EvidenceRef: "e"}})

	all, err := a.s.ListBooks(a.ctx, domain.BookSearchQuery{LegalEntityID: a.entityID})
	require.NoError(t, err)
	assert.Len(t, all, 2)
	stat, err := a.s.ListBooks(a.ctx, domain.BookSearchQuery{LegalEntityID: a.entityID, Type: domain.BookStatutory})
	require.NoError(t, err)
	assert.Len(t, stat, 1)
	active, err := a.s.ListBooks(a.ctx, domain.BookSearchQuery{LegalEntityID: a.entityID, Status: domain.BookActive})
	require.NoError(t, err)
	assert.Len(t, active, 1)
	grp, err := a.s.ListBooks(a.ctx, domain.BookSearchQuery{GroupCode: "GRP"})
	require.NoError(t, err)
	require.Len(t, grp, 1)
	assert.Equal(t, "CONS", grp[0].BookCode)

	_, err = b.s.ListBooks(b.ctx, domain.BookSearchQuery{LegalEntityID: a.entityID})
	require.ErrorIs(t, err, registry.ErrNotFound)
	other, err := b.s.GetBook(b.ctx, a.liveBookID(t, "STAT"))
	require.NoError(t, err)
	assert.Nil(t, other)
	v, err := b.s.GetBookView(b.ctx, a.liveBookID(t, "STAT"), time.Now().UTC(), false)
	require.NoError(t, err)
	assert.Nil(t, v)
	_, err = b.bookStatus(t, a.liveBookID(t, "STAT"), "RetireBook", domain.BookRetired, 2, bookActivator, "")
	require.ErrorIs(t, err, registry.ErrNotFound)
	_, err = b.s.CreateAccountingBook(b.ctx, a.bookReq("X", domain.BookStatutory, domain.FrameworkIFRS), bookCreator)
	require.ErrorIs(t, err, registry.ErrNotFound, "another tenant's entity owns no book here")
}

// Migration 000023 reverses cleanly while no book exists and applies again; with one it refuses.
func TestBook_MigrationDownAndUp(t *testing.T) {
	f := newORGFixture(t)
	ctx := context.Background()
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(thisFile), "../../deployments/migrations")
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		return string(b)
	}
	exists := func(q string) bool {
		var n int
		require.NoError(t, f.pool.QueryRow(ctx, q).Scan(&n))
		return n > 0
	}
	const table = `SELECT count(*) FROM pg_tables WHERE tablename = 'accounting_books'`

	f.createBook(t, f.bookReq("STAT", domain.BookStatutory, domain.FrameworkIFRS))
	_, err := f.pool.Exec(ctx, read("000023_accounting_books.down.sql"))
	require.Error(t, err, "down is refused while books exist")
	assert.Contains(t, err.Error(), "000023 down refused")

	f2 := newORGFixture(t)
	_, err = f2.pool.Exec(ctx, read("000023_accounting_books.down.sql"))
	require.NoError(t, err)
	assert.False(t, exists(table))
	_, err = f2.pool.Exec(ctx, read("000023_accounting_books.up.sql"))
	require.NoError(t, err, "up applies again after down")
	assert.True(t, exists(table))
}
