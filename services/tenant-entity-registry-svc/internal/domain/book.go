package domain

// REF-06 Accounting Book / Ledger Basis (ZS-SVC-I-001 s4.16).
//
// A book is an identity: statutory, management, tax, consolidation and regulatory books stay separate even when their
// charts overlap. Its basis (framework, functional and presentation currencies, chart reference) is a versioned,
// effective-dated attribute, and a basis change is prospective, so it never rewrites how earlier periods were booked.
// A book owns no ledger lines or balances and no accounting policy content.

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// BookType is what a book is for. The types are never conflated.
type BookType string

const (
	BookStatutory     BookType = "STATUTORY"
	BookManagement    BookType = "MANAGEMENT"
	BookTax           BookType = "TAX"
	BookConsolidation BookType = "CONSOLIDATION"
	BookRegulatory    BookType = "REGULATORY"
)

// Valid reports whether t is a known type.
func (t BookType) Valid() bool {
	switch t {
	case BookStatutory, BookManagement, BookTax, BookConsolidation, BookRegulatory:
		return true
	}
	return false
}

// Material reports whether the book carries legal or tax consequences, so that its activation is by someone other than
// its creator.
func (t BookType) Material() bool { return t == BookStatutory || t == BookTax || t == BookRegulatory }

// BookFramework is the accounting framework or basis of a book.
type BookFramework string

const (
	FrameworkIFRS       BookFramework = "IFRS"
	FrameworkIFRSSME    BookFramework = "IFRS_SME"
	FrameworkUSGAAP     BookFramework = "US_GAAP"
	FrameworkLocalGAAP  BookFramework = "LOCAL_GAAP"
	FrameworkTax        BookFramework = "TAX_BASIS"
	FrameworkManagement BookFramework = "MANAGEMENT_BASIS"
	FrameworkRegulatory BookFramework = "REGULATORY_BASIS"
)

// allowedFrameworks is s4.16's "allowed frameworks/bases" by book type. A tax book is on a tax basis, not on IFRS; a
// management book may follow a financial framework or its own.
var allowedFrameworks = map[BookType][]BookFramework{
	BookStatutory:     {FrameworkIFRS, FrameworkIFRSSME, FrameworkUSGAAP, FrameworkLocalGAAP},
	BookManagement:    {FrameworkManagement, FrameworkIFRS, FrameworkUSGAAP, FrameworkLocalGAAP},
	BookTax:           {FrameworkTax},
	BookConsolidation: {FrameworkIFRS, FrameworkIFRSSME, FrameworkUSGAAP, FrameworkLocalGAAP},
	BookRegulatory:    {FrameworkRegulatory, FrameworkLocalGAAP},
}

// FrameworkAllowedFor reports whether a book of this type may use the framework.
func FrameworkAllowedFor(t BookType, f BookFramework) bool {
	for _, a := range allowedFrameworks[t] {
		if a == f {
			return true
		}
	}
	return false
}

// AllowedFrameworks lists the frameworks a book type may use.
func AllowedFrameworks(t BookType) []BookFramework { return allowedFrameworks[t] }

// Book statuses: Draft, Active, PostingSuspended, Retired.
const (
	BookDraft            = "DRAFT"
	BookActive           = "ACTIVE"
	BookPostingSuspended = "POSTING_SUSPENDED"
	BookRetired          = "RETIRED"
)

// BookCanMoveTo reports whether the lifecycle allows from to to. A suspended book is re-activated; RETIRED is final.
func BookCanMoveTo(from, to string) bool {
	switch from {
	case BookDraft:
		return to == BookActive || to == BookRetired
	case BookActive:
		return to == BookPostingSuspended || to == BookRetired
	case BookPostingSuspended:
		return to == BookActive || to == BookRetired
	}
	return false
}

var bookCode = regexp.MustCompile(`^[A-Z0-9][A-Z0-9_.-]{0,39}$`)

// BookBasisVersion is the basis a book had over an interval.
type BookBasisVersion struct {
	VersionNo            int           `json:"version_no"`
	Framework            BookFramework `json:"framework"`
	FrameworkDetail      string        `json:"framework_detail,omitempty"`
	FunctionalCurrency   string        `json:"functional_currency"`
	PresentationCurrency string        `json:"presentation_currency"`
	ChartRef             string        `json:"chart_ref,omitempty"`
	PolicyRef            string        `json:"policy_ref,omitempty"`
	ValidFrom            string        `json:"valid_from"`
	ValidTo              string        `json:"valid_to,omitempty"`
	Reason               string        `json:"reason,omitempty"`
	EvidenceRef          string        `json:"evidence_ref"`
	RecordedBy           string        `json:"recorded_by"`
	RecordedAt           time.Time     `json:"recorded_at"`
}

// AccountingBook is the identity.
type AccountingBook struct {
	BookID        string    `json:"book_id"`
	TenantID      string    `json:"tenant_id"`
	LegalEntityID string    `json:"legal_entity_id,omitempty"`
	GroupCode     string    `json:"group_code,omitempty"`
	BookCode      string    `json:"book_code"`
	Name          string    `json:"name"`
	BookType      BookType  `json:"book_type"`
	Purpose       string    `json:"purpose"`
	Status        string    `json:"status"`
	Material      bool      `json:"material"`
	CurrentBasis  int       `json:"current_basis"`
	RecordVersion int64     `json:"record_version"`
	CreatedBy     string    `json:"created_by"`
	CreatedAt     time.Time `json:"created_at"`
	ActivatedBy   string    `json:"activated_by,omitempty"`
}

// BookTransition is one recorded move of a book's status.
type BookTransition struct {
	Seq           int       `json:"seq"`
	From          string    `json:"from_status,omitempty"`
	To            string    `json:"to_status"`
	EffectiveDate string    `json:"effective_date"`
	Command       string    `json:"command"`
	Reason        string    `json:"reason,omitempty"`
	EvidenceRef   string    `json:"evidence_ref"`
	Actor         string    `json:"actor"`
	VersionBefore int64     `json:"version_before,omitempty"`
	VersionAfter  int64     `json:"version_after"`
	OccurredAt    time.Time `json:"occurred_at"`
}

// PostingEligibility is BookPostingEligibility: whether a book may take a posting on a date, and why not. A posting
// service asks this for the book it names explicitly; there is no default book.
type PostingEligibility struct {
	BookID   string `json:"book_id"`
	AsOf     string `json:"as_of"`
	Eligible bool   `json:"eligible"`
	// Reason is empty when eligible: NOT_ACTIVE, POSTING_SUSPENDED, RETIRED, NOT_YET_EFFECTIVE or NO_BASIS_IN_FORCE.
	Reason string `json:"reason,omitempty"`
}

// BookView is a book as it was on a date: its status then, the basis in force then and whether it could post.
type BookView struct {
	Book        AccountingBook     `json:"book"`
	AsOf        string             `json:"as_of"`
	StatusAsOf  string             `json:"status_as_of"`
	Basis       *BookBasisVersion  `json:"basis"`
	Eligibility PostingEligibility `json:"posting_eligibility"`
	History     []BookTransition   `json:"history,omitempty"`
}

// ParallelBook is another book of the same entity, shown so parallel books can be told apart.
type ParallelBook struct {
	BookID    string        `json:"book_id"`
	BookCode  string        `json:"book_code"`
	BookType  BookType      `json:"book_type"`
	Status    string        `json:"status"`
	Framework BookFramework `json:"framework,omitempty"`
}

// BookBasisExplanation is ExplainBookBasis: the basis in force, how it got there and how this book differs from its
// siblings.
type BookBasisExplanation struct {
	Book                    AccountingBook     `json:"book"`
	AsOf                    string             `json:"as_of"`
	InForce                 *BookBasisVersion  `json:"basis_in_force"`
	Versions                []BookBasisVersion `json:"versions"`
	AllowedFrameworks       []BookFramework    `json:"allowed_frameworks"`
	EntityDefaultCurrency   string             `json:"entity_default_currency,omitempty"`
	FunctionalMatchesEntity *bool              `json:"functional_matches_entity,omitempty"`
	ParallelBooks           []ParallelBook     `json:"parallel_books"`
	// Notes states what the basis means and does not, in the spec's terms.
	Notes []string `json:"notes"`
}

// BookBasisFields are the basis attributes common to create and amend.
type BookBasisFields struct {
	Framework            BookFramework `json:"framework"`
	FrameworkDetail      string        `json:"framework_detail,omitempty"`
	FunctionalCurrency   string        `json:"functional_currency"`
	PresentationCurrency string        `json:"presentation_currency"`
	ChartRef             string        `json:"chart_ref,omitempty"`
	PolicyRef            string        `json:"policy_ref,omitempty"`
	ValidFrom            string        `json:"valid_from,omitempty"`
	EvidenceRef          string        `json:"evidence_ref"`
}

func (b BookBasisFields) validate(t BookType) error {
	switch {
	case !FrameworkAllowedFor(t, b.Framework):
		return fmt.Errorf("framework %q is not allowed for a %s book (allowed: %v)", b.Framework, t, AllowedFrameworks(t))
	case (b.Framework == FrameworkLocalGAAP || b.Framework == FrameworkRegulatory) && strings.TrimSpace(b.FrameworkDetail) == "":
		return fmt.Errorf("framework_detail names the standard behind %s", b.Framework)
	case !currencyShape.MatchString(b.FunctionalCurrency) || !currencyShape.MatchString(b.PresentationCurrency):
		return fmt.Errorf("functional_currency and presentation_currency must be three upper-case letters")
	case strings.TrimSpace(b.EvidenceRef) == "":
		return fmt.Errorf("evidence_ref is required")
	case len(b.FrameworkDetail) > 200 || len(b.ChartRef) > 200 || len(b.PolicyRef) > 200:
		return fmt.Errorf("framework_detail, chart_ref or policy_ref is too long")
	}
	if b.ValidFrom != "" {
		if _, err := ParseDate(b.ValidFrom); err != nil {
			return fmt.Errorf("valid_from %v", err)
		}
	}
	return nil
}

// CreateAccountingBookRequest is CreateAccountingBook. The book starts DRAFT.
type CreateAccountingBookRequest struct {
	LegalEntityID string   `json:"legal_entity_id,omitempty"`
	GroupCode     string   `json:"group_code,omitempty"`
	BookCode      string   `json:"book_code"`
	Name          string   `json:"name"`
	BookType      BookType `json:"book_type"`
	Purpose       string   `json:"purpose"`
	BookBasisFields
	CorrelationID string `json:"correlation_id,omitempty"`
}

// Validate checks the request. There is no default book and no default type: a book says what it is for.
func (r CreateAccountingBookRequest) Validate() error {
	switch {
	case (strings.TrimSpace(r.LegalEntityID) == "") == (strings.TrimSpace(r.GroupCode) == ""):
		return fmt.Errorf("name exactly one owner: legal_entity_id or group_code")
	case !r.BookType.Valid():
		return fmt.Errorf("book_type must be STATUTORY, MANAGEMENT, TAX, CONSOLIDATION or REGULATORY")
	case r.GroupCode != "" && r.BookType != BookConsolidation && r.BookType != BookManagement:
		return fmt.Errorf("a group owns only consolidation and management books; a %s book belongs to a legal entity", r.BookType)
	case !bookCode.MatchString(r.BookCode):
		return fmt.Errorf("book_code must be upper case letters, digits, dot, dash or underscore (1 to 40 characters)")
	case len(r.GroupCode) > 40:
		return fmt.Errorf("group_code is too long")
	case strings.TrimSpace(r.Name) == "" || len(r.Name) > 200:
		return fmt.Errorf("name is required (1 to 200 characters)")
	case strings.TrimSpace(r.Purpose) == "" || len(r.Purpose) > 500:
		return fmt.Errorf("purpose is required: say what the book is for")
	}
	return r.BookBasisFields.validate(r.BookType)
}

// AmendBookBasisRequest is AmendBookBasis: the next basis version, prospective only.
type AmendBookBasisRequest struct {
	ExpectedVersion int64  `json:"expected_version"`
	Reason          string `json:"reason"`
	BookBasisFields
	CorrelationID string `json:"correlation_id,omitempty"`
}

// ValidateFor checks the request for a book of type t.
func (r AmendBookBasisRequest) ValidateFor(t BookType) error {
	if strings.TrimSpace(r.Reason) == "" {
		return fmt.Errorf("reason is required")
	}
	return r.BookBasisFields.validate(t)
}

// BookStatusRequest is ActivateBook, SuspendPosting and RetireBook.
type BookStatusRequest struct {
	ExpectedVersion int64  `json:"expected_version"`
	Reason          string `json:"reason,omitempty"`
	EvidenceRef     string `json:"evidence_ref"`
	EffectiveDate   string `json:"effective_date,omitempty"`
	CorrelationID   string `json:"correlation_id,omitempty"`
}

// ValidateFor checks the request for a move to status to: suspending and retiring state why.
func (r BookStatusRequest) ValidateFor(to string) error {
	if strings.TrimSpace(r.EvidenceRef) == "" {
		return fmt.Errorf("evidence_ref is required")
	}
	if to != BookActive && strings.TrimSpace(r.Reason) == "" {
		return fmt.Errorf("reason is required to suspend posting or retire a book")
	}
	if r.EffectiveDate != "" {
		if _, err := ParseDate(r.EffectiveDate); err != nil {
			return fmt.Errorf("effective_date %v", err)
		}
	}
	return nil
}

// BookSearchQuery filters ListEntityBooks.
type BookSearchQuery struct {
	LegalEntityID string
	GroupCode     string
	Type          BookType
	Status        string
}

// EvaluatePosting is BookPostingEligibility as a pure function of the book's status on the date and the basis in
// force then. A book posts only when it is ACTIVE with a basis in force; every other case says why not, and there is
// no default and no inference.
func EvaluatePosting(bookID string, day time.Time, statusAsOf string, basis *BookBasisVersion) PostingEligibility {
	out := PostingEligibility{BookID: bookID, AsOf: day.UTC().Format("2006-01-02")}
	switch statusAsOf {
	case "":
		out.Reason = "NOT_YET_EFFECTIVE"
	case BookActive:
		if basis == nil {
			out.Reason = "NO_BASIS_IN_FORCE"
		} else {
			out.Eligible = true
		}
	case BookDraft:
		out.Reason = "NOT_ACTIVE"
	case BookPostingSuspended:
		out.Reason = "POSTING_SUSPENDED"
	case BookRetired:
		out.Reason = "RETIRED"
	default:
		out.Reason = "NOT_ACTIVE"
	}
	return out
}
