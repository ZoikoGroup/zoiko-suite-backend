package domain

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBookTypesAndMateriality(t *testing.T) {
	for _, tp := range []BookType{BookStatutory, BookTax, BookRegulatory} {
		assert.True(t, tp.Material(), tp)
	}
	for _, tp := range []BookType{BookManagement, BookConsolidation} {
		assert.False(t, tp.Material(), tp)
	}
	assert.False(t, BookType("DEFAULT").Valid(), "there is no default book")
	assert.False(t, BookType("").Valid())
}

func TestFrameworksAreAllowedByBookType(t *testing.T) {
	assert.True(t, FrameworkAllowedFor(BookStatutory, FrameworkIFRS))
	assert.True(t, FrameworkAllowedFor(BookStatutory, FrameworkLocalGAAP))
	assert.False(t, FrameworkAllowedFor(BookStatutory, FrameworkTax), "a statutory book is not on a tax basis")
	assert.False(t, FrameworkAllowedFor(BookStatutory, FrameworkManagement))
	assert.True(t, FrameworkAllowedFor(BookTax, FrameworkTax))
	assert.False(t, FrameworkAllowedFor(BookTax, FrameworkIFRS), "a tax book is not on IFRS")
	assert.True(t, FrameworkAllowedFor(BookManagement, FrameworkManagement))
	assert.True(t, FrameworkAllowedFor(BookManagement, FrameworkIFRS))
	assert.True(t, FrameworkAllowedFor(BookConsolidation, FrameworkIFRS))
	assert.False(t, FrameworkAllowedFor(BookConsolidation, FrameworkManagement))
	assert.True(t, FrameworkAllowedFor(BookRegulatory, FrameworkRegulatory))
	assert.False(t, FrameworkAllowedFor(BookRegulatory, FrameworkIFRS))
	assert.False(t, FrameworkAllowedFor("NOPE", FrameworkIFRS))
	assert.NotEmpty(t, AllowedFrameworks(BookTax))
}

func TestBookLifecycle(t *testing.T) {
	allowed := map[[2]string]bool{
		{BookDraft, BookActive}: true, {BookDraft, BookRetired}: true,
		{BookActive, BookPostingSuspended}: true, {BookActive, BookRetired}: true,
		{BookPostingSuspended, BookActive}: true, {BookPostingSuspended, BookRetired}: true,
	}
	all := []string{BookDraft, BookActive, BookPostingSuspended, BookRetired}
	for _, from := range all {
		for _, to := range all {
			assert.Equal(t, allowed[[2]string{from, to}], BookCanMoveTo(from, to), "%s -> %s", from, to)
		}
	}
}

func TestEvaluatePostingNeverGuesses(t *testing.T) {
	day := time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)
	basis := &BookBasisVersion{VersionNo: 1}
	for name, tc := range map[string]struct {
		status   string
		basis    *BookBasisVersion
		eligible bool
		reason   string
	}{
		"active with a basis":            {BookActive, basis, true, ""},
		"active with none in force":      {BookActive, nil, false, "NO_BASIS_IN_FORCE"},
		"draft":                          {BookDraft, basis, false, "NOT_ACTIVE"},
		"suspended":                      {BookPostingSuspended, basis, false, "POSTING_SUSPENDED"},
		"retired":                        {BookRetired, basis, false, "RETIRED"},
		"no status yet":                  {"", basis, false, "NOT_YET_EFFECTIVE"},
		"an unknown status is not ready": {"WEIRD", basis, false, "NOT_ACTIVE"},
	} {
		got := EvaluatePosting("b1", day, tc.status, tc.basis)
		assert.Equal(t, tc.eligible, got.Eligible, name)
		assert.Equal(t, tc.reason, got.Reason, name)
		assert.Equal(t, "2026-06-12", got.AsOf)
		assert.Equal(t, "b1", got.BookID)
	}
}

func validBook() CreateAccountingBookRequest {
	return CreateAccountingBookRequest{LegalEntityID: "e", BookCode: "STAT-IFRS", Name: "Statutory IFRS", BookType: BookStatutory, Purpose: "statutory reporting",
		BookBasisFields: BookBasisFields{Framework: FrameworkIFRS, FunctionalCurrency: "USD", PresentationCurrency: "USD", ValidFrom: "2026-01-01", EvidenceRef: "minute"}}
}

func TestCreateAccountingBookValidate(t *testing.T) {
	require.NoError(t, validBook().Validate())
	mut := func(f func(*CreateAccountingBookRequest)) CreateAccountingBookRequest {
		r := validBook()
		f(&r)
		return r
	}
	for name, r := range map[string]CreateAccountingBookRequest{
		"no owner":          mut(func(r *CreateAccountingBookRequest) { r.LegalEntityID = "" }),
		"two owners":        mut(func(r *CreateAccountingBookRequest) { r.GroupCode = "G" }),
		"no type":           mut(func(r *CreateAccountingBookRequest) { r.BookType = "" }),
		"a group statutory": mut(func(r *CreateAccountingBookRequest) { r.LegalEntityID, r.GroupCode = "", "G" }),
		"lower case code":   mut(func(r *CreateAccountingBookRequest) { r.BookCode = "stat" }),
		"spaces in code":    mut(func(r *CreateAccountingBookRequest) { r.BookCode = "A B" }),
		"long code":         mut(func(r *CreateAccountingBookRequest) { r.BookCode = strings.Repeat("A", 41) }),
		"no name":           mut(func(r *CreateAccountingBookRequest) { r.Name = " " }),
		"no purpose":        mut(func(r *CreateAccountingBookRequest) { r.Purpose = "" }),
		"wrong framework":   mut(func(r *CreateAccountingBookRequest) { r.BookType, r.Framework = BookTax, FrameworkIFRS }),
		"local unnamed":     mut(func(r *CreateAccountingBookRequest) { r.Framework = FrameworkLocalGAAP }),
		"bad currency":      mut(func(r *CreateAccountingBookRequest) { r.FunctionalCurrency = "usd" }),
		"no presentation":   mut(func(r *CreateAccountingBookRequest) { r.PresentationCurrency = "" }),
		"no evidence":       mut(func(r *CreateAccountingBookRequest) { r.EvidenceRef = " " }),
		"bad date":          mut(func(r *CreateAccountingBookRequest) { r.ValidFrom = "soon" }),
		"long chart ref":    mut(func(r *CreateAccountingBookRequest) { r.ChartRef = strings.Repeat("c", 201) }),
	} {
		assert.Error(t, r.Validate(), name)
	}
	grp := mut(func(r *CreateAccountingBookRequest) {
		r.LegalEntityID, r.GroupCode, r.BookType = "", "GRP", BookConsolidation
	})
	assert.NoError(t, grp.Validate(), "a group owns consolidation books")
	grp.BookType = BookManagement
	assert.NoError(t, grp.Validate(), "and management books")
	local := mut(func(r *CreateAccountingBookRequest) {
		r.Framework, r.FrameworkDetail = FrameworkLocalGAAP, "UK GAAP FRS 102"
	})
	assert.NoError(t, local.Validate())
	assert.NoError(t, mut(func(r *CreateAccountingBookRequest) { r.FunctionalCurrency, r.PresentationCurrency = "GBP", "USD" }).Validate(), "presentation may differ from functional")
}

func TestAmendAndStatusRequestsValidate(t *testing.T) {
	ok := AmendBookBasisRequest{Reason: "move", BookBasisFields: BookBasisFields{Framework: FrameworkIFRS, FunctionalCurrency: "EUR", PresentationCurrency: "EUR", EvidenceRef: "e"}}
	assert.NoError(t, ok.ValidateFor(BookStatutory))
	assert.Error(t, ok.ValidateFor(BookTax), "the framework is checked against the book's own type")
	noReason := ok
	noReason.Reason = ""
	assert.Error(t, noReason.ValidateFor(BookStatutory))

	assert.NoError(t, BookStatusRequest{EvidenceRef: "e"}.ValidateFor(BookActive), "activation needs evidence, not a reason")
	assert.Error(t, BookStatusRequest{}.ValidateFor(BookActive))
	assert.Error(t, BookStatusRequest{EvidenceRef: "e"}.ValidateFor(BookPostingSuspended), "suspending states why")
	assert.Error(t, BookStatusRequest{EvidenceRef: "e"}.ValidateFor(BookRetired))
	assert.NoError(t, BookStatusRequest{EvidenceRef: "e", Reason: "r", EffectiveDate: "2026-01-01"}.ValidateFor(BookRetired))
	assert.Error(t, BookStatusRequest{EvidenceRef: "e", Reason: "r", EffectiveDate: "x"}.ValidateFor(BookRetired))
}
