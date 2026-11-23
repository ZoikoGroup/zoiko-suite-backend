package domain

// REF-10 Payment Terms (ZS-SVC-I-001 s4.20): versioned payment-term calculation
// rules and the deterministic due-date calculation.
//
// "Due-date calculation returns due date plus term version, calendar version,
// basis date and adjustment convention." Nothing here reads a clock: a due date
// is a pure function of the term version, the dates given, and the calendar
// version that governs the date it falls on, and the answer names all of them.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Which date a term counts from. The caller supplies the dates.
const (
	BasisInvoiceDate  = "INVOICE_DATE"
	BasisReceiptDate  = "RECEIPT_DATE"
	BasisDeliveryDate = "DELIVERY_DATE"
)

// How a due date is worked out from the basis date.
const (
	// DueNetDays: the basis date plus due_days.
	DueNetDays = "NET_DAYS"
	// DueEndOfMonthPlus: the end of the basis date's month plus due_days.
	DueEndOfMonthPlus = "END_OF_MONTH_PLUS"
	// DueDayOfNextMonth: the due_day_of_month of the month after the basis
	// date's, clamped to that month's length.
	DueDayOfNextMonth = "DAY_OF_NEXT_MONTH"
)

// Lifecycle of a payment term version: Draft -> Active -> Superseded/Retired.
const (
	TermDraft      = "DRAFT"
	TermActive     = "ACTIVE"
	TermSuperseded = "SUPERSEDED"
	TermRetired    = "RETIRED"
)

// Bounds.
const (
	MaxDueDays = 365
	// MaxStandardCreditDays is the longest credit period a term may give without
	// saying it changes controlled credit policy.
	MaxStandardCreditDays = 90
	MaxGraceDays          = 90
)

var (
	termCodeRe     = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,31}$`)
	discountPctRe  = regexp.MustCompile(`^(100(\.0{1,4})?|[0-9]{1,2}(\.[0-9]{1,4})?)$`)
	maxTermText    = 2000
	maxTermNameLen = 512
)

// ValidTermCode reports whether s is a payment term code, such as NET30.
func ValidTermCode(s string) bool { return termCodeRe.MatchString(s) }

// PaymentTermVersionInput is the rule of a payment term version as drafted.
type PaymentTermVersionInput struct {
	BasisDateRule string `json:"basis_date_rule"`
	DueMethod     string `json:"due_method"`
	DueDays       int    `json:"due_days"`
	// DueDayOfMonth is for DAY_OF_NEXT_MONTH only.
	DueDayOfMonth *int `json:"due_day_of_month,omitempty"`

	BusinessDayConvention string `json:"business_day_convention"`
	// CalendarCode names the calendar the convention applies by; required with
	// a convention, and only then.
	CalendarCode string `json:"calendar_code,omitempty"`

	GraceDays int `json:"grace_days"`
	// DiscountPercent is a decimal string such as "2.5", with DiscountDays: the
	// percent off if paid within that many days of the basis date.
	DiscountPercent string `json:"discount_percent,omitempty"`
	DiscountDays    *int   `json:"discount_days,omitempty"`

	// ControlledCredit says the term changes controlled credit policy: required
	// beyond the standard credit period.
	ControlledCredit bool   `json:"controlled_credit"`
	Rationale        string `json:"rationale"`
	SourceRef        string `json:"source_ref,omitempty"`
	EffectiveFrom    string `json:"effective_from"`
}

// Validate checks a drafted term version's shape, reporting every problem.
func (in PaymentTermVersionInput) Validate() []string {
	var errs []string
	add := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	switch in.BasisDateRule {
	case BasisInvoiceDate, BasisReceiptDate, BasisDeliveryDate:
	default:
		add("basis_date_rule must be %s, %s or %s", BasisInvoiceDate, BasisReceiptDate, BasisDeliveryDate)
	}
	switch in.DueMethod {
	case DueNetDays, DueEndOfMonthPlus, DueDayOfNextMonth:
	default:
		add("due_method must be %s, %s or %s", DueNetDays, DueEndOfMonthPlus, DueDayOfNextMonth)
	}
	if in.DueDays < 0 || in.DueDays > MaxDueDays {
		add("due_days must be between 0 and %d", MaxDueDays)
	}
	if in.DueMethod == DueDayOfNextMonth {
		if in.DueDayOfMonth == nil || *in.DueDayOfMonth < 1 || *in.DueDayOfMonth > 31 {
			add("a %s term needs due_day_of_month between 1 and 31", DueDayOfNextMonth)
		}
		if in.DueDays != 0 {
			add("a %s term has no due_days: the due date is a day of the next month", DueDayOfNextMonth)
		}
	} else if in.DueDayOfMonth != nil {
		add("due_day_of_month belongs to a %s term only", DueDayOfNextMonth)
	}

	if !ValidConvention(in.BusinessDayConvention) {
		add("business_day_convention must be NONE, FOLLOWING, PRECEDING, MODIFIED_FOLLOWING or MODIFIED_PRECEDING")
	} else if in.BusinessDayConvention == ConventionNone {
		if in.CalendarCode != "" {
			add("calendar_code is for a term with a business-day convention: with NONE there is nothing to apply it to")
		}
	} else if !ValidCalendarCode(in.CalendarCode) {
		add("a business-day convention needs the calendar_code of the calendar to apply it by")
	}

	if in.GraceDays < 0 || in.GraceDays > MaxGraceDays {
		add("grace_days must be between 0 and %d", MaxGraceDays)
	}
	switch {
	case in.DiscountPercent == "" && in.DiscountDays == nil:
	case in.DiscountPercent == "" || in.DiscountDays == nil:
		add("a discount needs both discount_percent and discount_days")
	default:
		if !discountPctRe.MatchString(in.DiscountPercent) || strings.Trim(in.DiscountPercent, "0.") == "" {
			add("discount_percent must be a decimal above 0 and at most 100, with at most 4 decimal places")
		}
		if *in.DiscountDays < 1 || *in.DiscountDays > MaxDueDays {
			add("discount_days must be between 1 and %d", MaxDueDays)
		} else if in.DueMethod == DueNetDays && *in.DiscountDays >= in.DueDays {
			add("discount_days must be fewer than due_days: a discount for paying by the due date is no discount")
		}
	}

	if in.DueDays > MaxStandardCreditDays && !in.ControlledCredit {
		add("a term beyond %d days of credit changes controlled credit policy: set controlled_credit", MaxStandardCreditDays)
	}
	if strings.TrimSpace(in.Rationale) == "" {
		add("rationale is required")
	} else if len(in.Rationale) > maxTermText {
		add("rationale is longer than %d characters", maxTermText)
	}
	if len(in.SourceRef) > maxTermText {
		add("source_ref is longer than %d characters", maxTermText)
	}
	if _, err := time.Parse(dateLayout, in.EffectiveFrom); err != nil {
		add("effective_from must be YYYY-MM-DD")
	}
	return errs
}

// FormulaText is the rule in words, derived from the rule alone so it can never
// disagree with it.
func (in PaymentTermVersionInput) FormulaText() string {
	basis := map[string]string{BasisInvoiceDate: "invoice date", BasisReceiptDate: "receipt date", BasisDeliveryDate: "delivery date"}[in.BasisDateRule]
	var s string
	switch in.DueMethod {
	case DueNetDays:
		s = fmt.Sprintf("%s + %d days", basis, in.DueDays)
	case DueEndOfMonthPlus:
		s = fmt.Sprintf("end of the month of the %s + %d days", basis, in.DueDays)
	case DueDayOfNextMonth:
		day := 0
		if in.DueDayOfMonth != nil {
			day = *in.DueDayOfMonth
		}
		s = fmt.Sprintf("day %d of the month after the %s", day, basis)
	}
	if in.BusinessDayConvention != ConventionNone {
		s += fmt.Sprintf(", %s business day by calendar %s", strings.ToLower(strings.ReplaceAll(in.BusinessDayConvention, "_", " ")), in.CalendarCode)
	}
	if in.DiscountPercent != "" && in.DiscountDays != nil {
		s += fmt.Sprintf("; %s%% discount if paid within %d days", in.DiscountPercent, *in.DiscountDays)
	}
	if in.GraceDays > 0 {
		s += fmt.Sprintf("; %d days grace", in.GraceDays)
	}
	return s
}

// ContentDigest is the SHA-256 of the rule: what the term does, not its
// rationale or evidence. Two versions with the same digest give the same due
// dates.
func (in PaymentTermVersionInput) ContentDigest(termCode string) string {
	day := 0
	if in.DueDayOfMonth != nil {
		day = *in.DueDayOfMonth
	}
	dd := 0
	if in.DiscountDays != nil {
		dd = *in.DiscountDays
	}
	raw, _ := json.Marshal(map[string]any{
		"term_code": termCode, "basis_date_rule": in.BasisDateRule, "due_method": in.DueMethod, "due_days": in.DueDays,
		"due_day_of_month": day, "business_day_convention": in.BusinessDayConvention, "calendar_code": in.CalendarCode,
		"grace_days": in.GraceDays, "discount_percent": normalisePercent(in.DiscountPercent), "discount_days": dd,
		"effective_from": in.EffectiveFrom,
	})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// normalisePercent makes "2", "2.0" and "2.0000" the same discount.
// NormalisePercent makes "2", "2.0" and "2.0000" the same discount.
func NormalisePercent(s string) string { return normalisePercent(s) }

func normalisePercent(s string) string {
	if s == "" {
		return ""
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return s
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// PaymentTerm is the family of versions of one term.
type PaymentTerm struct {
	TermID         string    `json:"term_id"`
	TermCode       string    `json:"term_code"`
	TermName       string    `json:"term_name"`
	JurisdictionID *string   `json:"jurisdiction_id"`
	CreatedAt      time.Time `json:"created_at"`
	CreatedBy      string    `json:"created_by"`
}

// PaymentTermVersion is one version of a term as stored.
type PaymentTermVersion struct {
	PaymentTermVersionInput
	TermVersionID string  `json:"term_version_id"`
	TermCode      string  `json:"term_code"`
	Version       int     `json:"version"`
	Status        string  `json:"status"`
	FormulaText   string  `json:"formula_text"`
	ContentDigest string  `json:"content_digest"`
	EndsOn        *string `json:"ends_on,omitempty"`

	CreatedAt             time.Time  `json:"created_at"`
	CreatedBy             string     `json:"created_by"`
	ApprovedAt            *time.Time `json:"approved_at,omitempty"`
	ApprovedBy            *string    `json:"approved_by,omitempty"`
	SupersededAt          *time.Time `json:"superseded_at,omitempty"`
	SupersededByVersionID *string    `json:"superseded_by_version_id,omitempty"`
	RetiredAt             *time.Time `json:"retired_at,omitempty"`
	RetiredBy             *string    `json:"retired_by,omitempty"`
	RetireReason          *string    `json:"retire_reason,omitempty"`
}

// TermProvenance says which term version a due date was worked out by.
type TermProvenance struct {
	TermCode      string  `json:"term_code"`
	TermVersion   int     `json:"term_version"`
	TermVersionID string  `json:"term_version_id"`
	ContentDigest string  `json:"content_digest"`
	FormulaText   string  `json:"formula_text"`
	EffectiveFrom string  `json:"effective_from"`
	EndsOn        *string `json:"ends_on,omitempty"`
	Pinned        bool    `json:"pinned"`
}

// DueDateFacts are the dates a due date is worked out from.
type DueDateFacts struct {
	// InvoiceDate is required: it is also the date the term version in force is
	// chosen by. The others are needed only by a term that counts from them.
	InvoiceDate  time.Time
	ReceiptDate  *time.Time
	DeliveryDate *time.Time
}

// CalendarLookup finds the calendar version that governs a date and says which
// it was. It is called at most once, for the date the due date falls on.
type CalendarLookup func(on time.Time) (*CalendarVersion, *CalendarProvenance, error)

// Discount is the early-payment discount of a calculation.
type Discount struct {
	Percent string `json:"percent"`
	// Until is the last day the discount can be taken, adjusted by the term's
	// business-day convention and never later than the due date.
	Until string `json:"until"`
}

// DueDateResult is the answer to CalculateDueDate. "Due-date calculation returns
// due date plus term version, calendar version, basis date and adjustment
// convention."
type DueDateResult struct {
	BasisDateRule string `json:"basis_date_rule"`
	BasisDate     string `json:"basis_date"`
	// UnadjustedDueDate is the due date before any business-day convention.
	UnadjustedDueDate string `json:"unadjusted_due_date"`
	DueDate           string `json:"due_date"`
	// Convention is the business-day convention applied, and Adjusted whether it
	// moved the date.
	Convention string       `json:"adjustment_convention"`
	Adjusted   bool         `json:"adjusted"`
	Skipped    []SkippedDay `json:"skipped"`
	// GraceUntil is the last day before late-payment consequences begin.
	GraceUntil string    `json:"grace_until,omitempty"`
	Discount   *Discount `json:"discount,omitempty"`

	Term     TermProvenance      `json:"term"`
	Calendar *CalendarProvenance `json:"calendar,omitempty"`
	// Steps is how the date was worked out, in order, for ExplainDueDate.
	Steps []string `json:"steps,omitempty"`
}

// BasisDateOf picks the date a term counts from.
func (f DueDateFacts) BasisDateOf(rule string) (time.Time, error) {
	switch rule {
	case BasisInvoiceDate:
		return f.InvoiceDate, nil
	case BasisReceiptDate:
		if f.ReceiptDate != nil {
			return *f.ReceiptDate, nil
		}
		return time.Time{}, fmt.Errorf("%w: receipt_date", ErrBasisDateMissing)
	case BasisDeliveryDate:
		if f.DeliveryDate != nil {
			return *f.DeliveryDate, nil
		}
		return time.Time{}, fmt.Errorf("%w: delivery_date", ErrBasisDateMissing)
	}
	return time.Time{}, oblBad("unknown basis_date_rule %q", rule)
}

// CalculatePaymentDueDate works out a due date. It is a pure function of the term
// version, the facts and the calendar the lookup returns.
func CalculatePaymentDueDate(v PaymentTermVersion, facts DueDateFacts, lookup CalendarLookup) (DueDateResult, error) {
	res := DueDateResult{BasisDateRule: v.BasisDateRule, Convention: v.BusinessDayConvention, Skipped: []SkippedDay{}}
	step := func(format string, a ...any) { res.Steps = append(res.Steps, fmt.Sprintf(format, a...)) }
	step("term %s version %d: %s", v.TermCode, v.Version, v.FormulaText)

	basis, err := facts.BasisDateOf(v.BasisDateRule)
	if err != nil {
		return res, err
	}
	res.BasisDate = basis.Format(dateLayout)
	step("basis date (%s) = %s", v.BasisDateRule, res.BasisDate)

	var unadjusted time.Time
	switch v.DueMethod {
	case DueNetDays:
		unadjusted = basis.AddDate(0, 0, v.DueDays)
		step("%s + %d days = %s", res.BasisDate, v.DueDays, unadjusted.Format(dateLayout))
	case DueEndOfMonthPlus:
		eom := time.Date(basis.Year(), basis.Month(), daysInMonth(basis.Year(), basis.Month()), 0, 0, 0, 0, time.UTC)
		unadjusted = eom.AddDate(0, 0, v.DueDays)
		step("end of month %s + %d days = %s", eom.Format(dateLayout), v.DueDays, unadjusted.Format(dateLayout))
	case DueDayOfNextMonth:
		next := time.Date(basis.Year(), basis.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
		day := 1
		if v.DueDayOfMonth != nil {
			day = *v.DueDayOfMonth
		}
		if dim := daysInMonth(next.Year(), next.Month()); day > dim {
			step("day %d does not exist in %s: the last day, %d", day, next.Format("2006-01"), dim)
			day = dim
		}
		unadjusted = time.Date(next.Year(), next.Month(), day, 0, 0, 0, 0, time.UTC)
		step("day %d of %s = %s", day, next.Format("2006-01"), unadjusted.Format(dateLayout))
	default:
		return res, oblBad("unknown due_method %q", v.DueMethod)
	}
	res.UnadjustedDueDate = unadjusted.Format(dateLayout)

	due, discountUntil := unadjusted, time.Time{}
	if v.DiscountPercent != "" && v.DiscountDays != nil {
		discountUntil = basis.AddDate(0, 0, *v.DiscountDays)
	}
	if v.BusinessDayConvention != ConventionNone {
		if lookup == nil {
			return res, ErrNoCalendarVersion
		}
		cal, prov, err := lookup(unadjusted)
		if err != nil {
			return res, err
		}
		res.Calendar = prov
		step("calendar %s version %d governs %s", prov.CalendarCode, prov.CalendarVersion, unadjusted.Format(dateLayout))
		adj, skipped, err := cal.Adjust(unadjusted, v.BusinessDayConvention)
		if err != nil {
			return res, err
		}
		if len(skipped) > 0 {
			res.Adjusted, res.Skipped = true, skipped
			for _, s := range skipped {
				step("%s is not a business day (%s)", s.Date, strings.ToLower(s.Reason))
			}
			step("%s convention: %s -> %s", v.BusinessDayConvention, unadjusted.Format(dateLayout), adj.Format(dateLayout))
		} else {
			step("%s is a business day", unadjusted.Format(dateLayout))
		}
		due = adj
		if !discountUntil.IsZero() {
			if adjD, _, err := cal.Adjust(discountUntil, v.BusinessDayConvention); err == nil {
				discountUntil = adjD
			}
		}
	}
	res.DueDate = due.Format(dateLayout)

	if !discountUntil.IsZero() {
		if discountUntil.After(due) {
			discountUntil = due
			step("the discount cannot outlast the due date: until %s", due.Format(dateLayout))
		}
		res.Discount = &Discount{Percent: normalisePercent(v.DiscountPercent), Until: discountUntil.Format(dateLayout)}
		step("%s%% discount if paid by %s", res.Discount.Percent, res.Discount.Until)
	}
	if v.GraceDays > 0 {
		res.GraceUntil = due.AddDate(0, 0, v.GraceDays).Format(dateLayout)
		step("%d days grace: until %s", v.GraceDays, res.GraceUntil)
	}
	step("due %s", res.DueDate)
	return res, nil
}

var (
	// ErrPaymentTermNotFound: no such payment term.
	ErrPaymentTermNotFound = errorString("payment term not found")
	// ErrPaymentTermVersionNotFound: no such version of the term.
	ErrPaymentTermVersionNotFound = errorString("payment term version not found")
	// ErrNoPaymentTermVersion: no approved version of the term governs that date.
	ErrNoPaymentTermVersion = errorString("no approved version of the payment term governs that date")
	// ErrPaymentTermNotDraft: the version is past DRAFT.
	ErrPaymentTermNotDraft = errorString("the payment term version is not a draft")
	// ErrPaymentTermNotActive: the version is not the active one.
	ErrPaymentTermNotActive = errorString("the payment term version is not active")
	// ErrPaymentTermOverlap: an approved version already governs part of that
	// period; replacing it is stated with SupersedePaymentTerm.
	ErrPaymentTermOverlap = errorString("an approved version of this payment term already governs part of that period: supersede it")
	// ErrInvalidSupersession: the version named is not the active version this
	// one can replace.
	ErrInvalidSupersession = errorString("the version to supersede must be this term's active version, which this version takes over from")
	// ErrBasisDateMissing: the term counts from a date the request did not give.
	ErrBasisDateMissing = errorString("the term counts from a date that was not given")
	// ErrCalendarNotReady: the calendar a term is applied by has no published version.
	ErrCalendarNotReady = errorString("the calendar has no published version")
	// ErrInvalidPaymentTerm: the term or its version is not acceptable.
	ErrInvalidPaymentTerm = errorString("invalid payment term")
)
