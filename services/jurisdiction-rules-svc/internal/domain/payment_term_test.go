package domain

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// REF-10 payment terms: due dates against fixed golden cases, every one worked
// out by hand from the 2026 calendar (see gb() in business_calendar_test.go).

func ip(n int) *int { return &n }

func net(days int, convention string) PaymentTermVersionInput {
	in := PaymentTermVersionInput{
		BasisDateRule: BasisInvoiceDate, DueMethod: DueNetDays, DueDays: days, BusinessDayConvention: convention,
		Rationale: "standard terms", EffectiveFrom: "2026-01-01",
	}
	if convention != ConventionNone {
		in.CalendarCode = "gb-bank"
	}
	return in
}

func version(in PaymentTermVersionInput) PaymentTermVersion {
	return PaymentTermVersion{PaymentTermVersionInput: in, TermCode: "NET30", Version: 1, TermVersionID: "tv-1", FormulaText: in.FormulaText(), ContentDigest: in.ContentDigest("NET30")}
}

func gbLookup(calls *int) CalendarLookup {
	return func(on time.Time) (*CalendarVersion, *CalendarProvenance, error) {
		*calls++
		c := gb()
		return &c, &CalendarProvenance{CalendarCode: "gb-bank", CalendarVersion: 1, CalendarVersionID: "cv-1", ContentDigest: CalendarContentDigest(c)}, nil
	}
}

func facts(invoice string) DueDateFacts { return DueDateFacts{InvoiceDate: day(invoice)} }

func TestCalculatePaymentDueDate_GoldenCases(t *testing.T) {
	for _, tc := range []struct {
		name     string
		in       PaymentTermVersionInput
		invoice  string
		unadj    string
		due      string
		adjusted bool
	}{
		// 1 Oct 2026 + 30 days = Saturday 31 Oct.
		{"net 30, no convention", net(30, ConventionNone), "2026-10-01", "2026-10-31", "2026-10-31", false},
		{"net 30, following", net(30, ConventionFollowing), "2026-10-01", "2026-10-31", "2026-11-02", true},
		{"net 30, preceding", net(30, ConventionPreceding), "2026-10-01", "2026-10-31", "2026-10-30", true},
		{"net 30, modified following stays in the month", net(30, ConventionModifiedFollowing), "2026-10-01", "2026-10-31", "2026-10-30", true},
		{"net 30, modified preceding, the month holds", net(30, ConventionModifiedPreceding), "2026-10-01", "2026-10-31", "2026-10-30", true},
		// 25 Nov + 30 = Friday 25 Dec (Christmas), then the weekend and Boxing Day observed on the Monday.
		{"net 30 onto Christmas, following", net(30, ConventionFollowing), "2026-11-25", "2026-12-25", "2026-12-29", true},
		{"net 30 onto Christmas, preceding", net(30, ConventionPreceding), "2026-11-25", "2026-12-25", "2026-12-24", true},
		{"a business day is untouched", net(30, ConventionFollowing), "2026-10-05", "2026-11-04", "2026-11-04", false},
		{"net 0 is the basis date", net(0, ConventionNone), "2026-10-05", "2026-10-05", "2026-10-05", false},
		{"net 60 across a year end", net(60, ConventionNone), "2026-11-15", "2027-01-14", "2027-01-14", false},
		// End of month: February 2026 has 28 days, 2028 has 29.
		{"end of month, Feb 2026, following", PaymentTermVersionInput{BasisDateRule: BasisInvoiceDate, DueMethod: DueEndOfMonthPlus, BusinessDayConvention: ConventionFollowing, CalendarCode: "gb-bank", Rationale: "r", EffectiveFrom: "2026-01-01"}, "2026-02-10", "2026-02-28", "2026-03-02", true},
		{"end of month, leap Feb 2028", PaymentTermVersionInput{BasisDateRule: BasisInvoiceDate, DueMethod: DueEndOfMonthPlus, BusinessDayConvention: ConventionNone, Rationale: "r", EffectiveFrom: "2026-01-01"}, "2028-02-10", "2028-02-29", "2028-02-29", false},
		{"end of month plus 15", PaymentTermVersionInput{BasisDateRule: BasisInvoiceDate, DueMethod: DueEndOfMonthPlus, DueDays: 15, BusinessDayConvention: ConventionFollowing, CalendarCode: "gb-bank", Rationale: "r", EffectiveFrom: "2026-01-01"}, "2026-01-20", "2026-02-15", "2026-02-16", true},
		{"end of month, invoiced on the last day", PaymentTermVersionInput{BasisDateRule: BasisInvoiceDate, DueMethod: DueEndOfMonthPlus, BusinessDayConvention: ConventionNone, Rationale: "r", EffectiveFrom: "2026-01-01"}, "2026-01-31", "2026-01-31", "2026-01-31", false},
		// Day of next month.
		{"the 31st of next month clamps to February", PaymentTermVersionInput{BasisDateRule: BasisInvoiceDate, DueMethod: DueDayOfNextMonth, DueDayOfMonth: ip(31), BusinessDayConvention: ConventionNone, Rationale: "r", EffectiveFrom: "2026-01-01"}, "2026-01-15", "2026-02-28", "2026-02-28", false},
		{"the 10th of next month across the year, following", PaymentTermVersionInput{BasisDateRule: BasisInvoiceDate, DueMethod: DueDayOfNextMonth, DueDayOfMonth: ip(10), BusinessDayConvention: ConventionFollowing, CalendarCode: "gb-bank", Rationale: "r", EffectiveFrom: "2026-01-01"}, "2026-12-20", "2027-01-10", "2027-01-11", true},
		{"the 30th of next month in a leap February", PaymentTermVersionInput{BasisDateRule: BasisInvoiceDate, DueMethod: DueDayOfNextMonth, DueDayOfMonth: ip(30), BusinessDayConvention: ConventionNone, Rationale: "r", EffectiveFrom: "2026-01-01"}, "2028-01-31", "2028-02-29", "2028-02-29", false},
	} {
		var calls int
		got, err := CalculatePaymentDueDate(version(tc.in), facts(tc.invoice), gbLookup(&calls))
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got.UnadjustedDueDate != tc.unadj || got.DueDate != tc.due || got.Adjusted != tc.adjusted {
			t.Errorf("%s: unadjusted %s due %s adjusted %v, want %s %s %v", tc.name, got.UnadjustedDueDate, got.DueDate, got.Adjusted, tc.unadj, tc.due, tc.adjusted)
		}
		wantCalls := 1
		if tc.in.BusinessDayConvention == ConventionNone {
			wantCalls = 0
		}
		if calls != wantCalls {
			t.Errorf("%s: the calendar was looked up %d times, want %d", tc.name, calls, wantCalls)
		}
		if got.Convention != tc.in.BusinessDayConvention || got.BasisDate != tc.invoice || got.BasisDateRule != tc.in.BasisDateRule {
			t.Errorf("%s: convention %q basis %q (%s), want the convention, basis date and rule returned", tc.name, got.Convention, got.BasisDate, got.BasisDateRule)
		}
		if (tc.in.BusinessDayConvention == ConventionNone) != (got.Calendar == nil) {
			t.Errorf("%s: calendar provenance %v", tc.name, got.Calendar)
		}
	}
}

// The calendar is chosen for the date the due date falls on, which is only known
// once the rule has been applied.
func TestCalculatePaymentDueDate_TheCalendarIsLookedUpForTheUnadjustedDate(t *testing.T) {
	var asked time.Time
	lookup := func(on time.Time) (*CalendarVersion, *CalendarProvenance, error) {
		asked = on
		c := gb()
		return &c, &CalendarProvenance{CalendarCode: "gb-bank", CalendarVersion: 7}, nil
	}
	got, err := CalculatePaymentDueDate(version(net(30, ConventionFollowing)), facts("2026-10-01"), lookup)
	if err != nil || asked.Format(dateLayout) != "2026-10-31" {
		t.Fatalf("asked for %s (%v), want the date the term lands on, 2026-10-31", asked.Format(dateLayout), err)
	}
	if got.Calendar == nil || got.Calendar.CalendarVersion != 7 {
		t.Errorf("calendar = %+v, want the version that answered", got.Calendar)
	}
}

func TestCalculatePaymentDueDate_BasisDates(t *testing.T) {
	rule := net(30, ConventionNone)
	rule.BasisDateRule = BasisReceiptDate
	receipt, delivery := day("2026-10-10"), day("2026-10-20")

	got, err := CalculatePaymentDueDate(version(rule), DueDateFacts{InvoiceDate: day("2026-10-01"), ReceiptDate: &receipt}, nil)
	if err != nil || got.BasisDate != "2026-10-10" || got.DueDate != "2026-11-09" {
		t.Errorf("from the receipt date: %+v %v", got, err)
	}
	if _, err := CalculatePaymentDueDate(version(rule), facts("2026-10-01"), nil); !errors.Is(err, ErrBasisDateMissing) || !strings.Contains(err.Error(), "receipt_date") {
		t.Errorf("no receipt date: %v, want ErrBasisDateMissing naming it", err)
	}
	rule.BasisDateRule = BasisDeliveryDate
	if got, err := CalculatePaymentDueDate(version(rule), DueDateFacts{InvoiceDate: day("2026-10-01"), DeliveryDate: &delivery}, nil); err != nil || got.BasisDate != "2026-10-20" {
		t.Errorf("from the delivery date: %+v %v", got, err)
	}
	if _, err := CalculatePaymentDueDate(version(rule), DueDateFacts{InvoiceDate: day("2026-10-01"), ReceiptDate: &receipt}, nil); !errors.Is(err, ErrBasisDateMissing) {
		t.Errorf("a receipt date is not a delivery date: %v", err)
	}
}

func TestCalculatePaymentDueDate_DiscountAndGrace(t *testing.T) {
	// 2/10 net 30 with grace: invoice Thu 1 Oct; discount to Sun 11 Oct, due Sat 31 Oct.
	in := net(30, ConventionFollowing)
	in.DiscountPercent, in.DiscountDays, in.GraceDays = "2.0", ip(10), 5
	got, err := CalculatePaymentDueDate(version(in), facts("2026-10-01"), gbLookup(new(int)))
	if err != nil {
		t.Fatal(err)
	}
	if got.Discount == nil || got.Discount.Percent != "2" || got.Discount.Until != "2026-10-12" {
		t.Errorf("discount = %+v, want 2%% to Monday 12 Oct: the deadline follows the term's convention", got.Discount)
	}
	if got.DueDate != "2026-11-02" || got.GraceUntil != "2026-11-07" {
		t.Errorf("due %s grace until %s, want 2026-11-02 and 2026-11-07 (grace counts calendar days from the due date)", got.DueDate, got.GraceUntil)
	}

	// With no convention the deadline is the plain day.
	in = net(30, ConventionNone)
	in.DiscountPercent, in.DiscountDays = "2.5", ip(10)
	got, _ = CalculatePaymentDueDate(version(in), facts("2026-10-01"), nil)
	if got.Discount.Until != "2026-10-11" || got.Discount.Percent != "2.5" || got.GraceUntil != "" {
		t.Errorf("discount = %+v grace %q", got.Discount, got.GraceUntil)
	}

	// A discount can never outlast the due date it is for.
	in = PaymentTermVersionInput{BasisDateRule: BasisInvoiceDate, DueMethod: DueEndOfMonthPlus, BusinessDayConvention: ConventionNone,
		DiscountPercent: "1", DiscountDays: ip(40), Rationale: "r", EffectiveFrom: "2026-01-01"}
	got, _ = CalculatePaymentDueDate(version(in), facts("2026-02-10"), nil)
	if got.DueDate != "2026-02-28" || got.Discount.Until != "2026-02-28" {
		t.Errorf("due %s discount until %s, want the discount capped at the due date", got.DueDate, got.Discount.Until)
	}
	if !strings.Contains(strings.Join(got.Steps, " | "), "cannot outlast") {
		t.Error("the cap is not explained")
	}
}

func TestCalculatePaymentDueDate_RefusalsAndDeterminism(t *testing.T) {
	v := version(net(30, ConventionFollowing))
	if _, err := CalculatePaymentDueDate(v, facts("2026-10-01"), nil); !errors.Is(err, ErrNoCalendarVersion) {
		t.Errorf("a convention with no calendar to look up: %v", err)
	}
	boom := errors.New("lookup failed")
	if _, err := CalculatePaymentDueDate(v, facts("2026-10-01"), func(time.Time) (*CalendarVersion, *CalendarProvenance, error) { return nil, nil, boom }); !errors.Is(err, boom) {
		t.Errorf("a failed lookup: %v", err)
	}
	if _, err := CalculatePaymentDueDate(v, facts("2026-10-01"), func(time.Time) (*CalendarVersion, *CalendarProvenance, error) {
		return nil, nil, ErrNoCalendarVersion
	}); !errors.Is(err, ErrNoCalendarVersion) {
		t.Errorf("no calendar version governs: %v", err)
	}

	a, _ := CalculatePaymentDueDate(v, facts("2026-11-25"), gbLookup(new(int)))
	b, _ := CalculatePaymentDueDate(v, facts("2026-11-25"), gbLookup(new(int)))
	if !reflect.DeepEqual(a, b) {
		t.Error("the same inputs gave different answers")
	}
}

func TestCalculatePaymentDueDate_TheExplanationIsTheCalculation(t *testing.T) {
	got, err := CalculatePaymentDueDate(version(net(30, ConventionFollowing)), facts("2026-11-25"), gbLookup(new(int)))
	if err != nil {
		t.Fatal(err)
	}
	steps := strings.Join(got.Steps, "\n")
	for _, want := range []string{"term NET30 version 1", "basis date (INVOICE_DATE) = 2026-11-25", "= 2026-12-25", "calendar gb-bank version 1",
		"2026-12-25 is not a business day (holiday)", "2026-12-26 is not a business day (weekend)", "2026-12-28 is not a business day (holiday)",
		"FOLLOWING convention: 2026-12-25 -> 2026-12-29", "due 2026-12-29"} {
		if !strings.Contains(steps, want) {
			t.Errorf("the explanation lacks %q:\n%s", want, steps)
		}
	}
	if len(got.Skipped) != 4 || got.Skipped[0].Name != "Christmas Day" {
		t.Errorf("skipped = %+v", got.Skipped)
	}
}

func TestPaymentTermVersionInput_Validate(t *testing.T) {
	ok := net(30, ConventionFollowing)
	ok.DiscountPercent, ok.DiscountDays = "2", ip(10)
	if errs := ok.Validate(); len(errs) != 0 {
		t.Fatalf("a good term refused: %v", errs)
	}
	dom := PaymentTermVersionInput{BasisDateRule: BasisInvoiceDate, DueMethod: DueDayOfNextMonth, DueDayOfMonth: ip(10), BusinessDayConvention: ConventionNone, Rationale: "r", EffectiveFrom: "2026-01-01"}
	if errs := dom.Validate(); len(errs) != 0 {
		t.Fatalf("a day-of-next-month term refused: %v", errs)
	}

	cases := map[string]struct {
		mutate func(*PaymentTermVersionInput)
		want   string
	}{
		"no basis":                        {func(i *PaymentTermVersionInput) { i.BasisDateRule = "" }, "basis_date_rule"},
		"unknown method":                  {func(i *PaymentTermVersionInput) { i.DueMethod = "NET_WEEKS" }, "due_method"},
		"negative days":                   {func(i *PaymentTermVersionInput) { i.DueDays = -1 }, "due_days"},
		"too many days":                   {func(i *PaymentTermVersionInput) { i.DueDays = 366; i.ControlledCredit = true }, "due_days"},
		"a day of month on net days":      {func(i *PaymentTermVersionInput) { i.DueDayOfMonth = ip(10) }, "belongs to"},
		"unknown convention":              {func(i *PaymentTermVersionInput) { i.BusinessDayConvention = "NEAREST" }, "business_day_convention"},
		"a convention with no calendar":   {func(i *PaymentTermVersionInput) { i.CalendarCode = "" }, "calendar_code"},
		"a bad calendar code":             {func(i *PaymentTermVersionInput) { i.CalendarCode = "GB Bank" }, "calendar_code"},
		"a calendar with no convention":   {func(i *PaymentTermVersionInput) { i.BusinessDayConvention = ConventionNone }, "nothing to apply"},
		"negative grace":                  {func(i *PaymentTermVersionInput) { i.GraceDays = -1 }, "grace_days"},
		"too much grace":                  {func(i *PaymentTermVersionInput) { i.GraceDays = MaxGraceDays + 1 }, "grace_days"},
		"a percent with no days":          {func(i *PaymentTermVersionInput) { i.DiscountPercent, i.DiscountDays = "2", nil }, "both"},
		"days with no percent":            {func(i *PaymentTermVersionInput) { i.DiscountPercent, i.DiscountDays = "", ip(10) }, "both"},
		"zero percent":                    {func(i *PaymentTermVersionInput) { i.DiscountPercent, i.DiscountDays = "0", ip(10) }, "discount_percent"},
		"over 100 percent":                {func(i *PaymentTermVersionInput) { i.DiscountPercent, i.DiscountDays = "100.5", ip(10) }, "discount_percent"},
		"a percent that is no number":     {func(i *PaymentTermVersionInput) { i.DiscountPercent, i.DiscountDays = "two", ip(10) }, "discount_percent"},
		"five decimals":                   {func(i *PaymentTermVersionInput) { i.DiscountPercent, i.DiscountDays = "1.12345", ip(10) }, "discount_percent"},
		"a discount no sooner than due":   {func(i *PaymentTermVersionInput) { i.DiscountPercent, i.DiscountDays = "2", ip(30) }, "fewer than due_days"},
		"credit beyond 90 days unflagged": {func(i *PaymentTermVersionInput) { i.DueDays = 120 }, "controlled_credit"},
		"no rationale":                    {func(i *PaymentTermVersionInput) { i.Rationale = " " }, "rationale is required"},
		"over-long rationale":             {func(i *PaymentTermVersionInput) { i.Rationale = strings.Repeat("x", 2001) }, "rationale is longer"},
		"a bad effective date":            {func(i *PaymentTermVersionInput) { i.EffectiveFrom = "soon" }, "effective_from"},
	}
	for name, c := range cases {
		in := net(30, ConventionFollowing)
		c.mutate(&in)
		if errs := in.Validate(); len(errs) == 0 || !strings.Contains(strings.Join(errs, " | "), c.want) {
			t.Errorf("%s: %v, want a problem mentioning %q", name, errs, c.want)
		}
	}
	for name, c := range map[string]struct {
		mutate func(*PaymentTermVersionInput)
		want   string
	}{
		"no day of month":       {func(i *PaymentTermVersionInput) { i.DueDayOfMonth = nil }, "due_day_of_month"},
		"day 32":                {func(i *PaymentTermVersionInput) { i.DueDayOfMonth = ip(32) }, "due_day_of_month"},
		"day 0":                 {func(i *PaymentTermVersionInput) { i.DueDayOfMonth = ip(0) }, "due_day_of_month"},
		"days as well as a day": {func(i *PaymentTermVersionInput) { i.DueDays = 5 }, "no due_days"},
	} {
		in := dom
		c.mutate(&in)
		if errs := in.Validate(); len(errs) == 0 || !strings.Contains(strings.Join(errs, " | "), c.want) {
			t.Errorf("%s: %v, want %q", name, errs, c.want)
		}
	}

	// Credit beyond the standard period is allowed once it says so.
	long := net(120, ConventionNone)
	long.ControlledCredit = true
	if errs := long.Validate(); len(errs) != 0 {
		t.Errorf("a flagged long-credit term refused: %v", errs)
	}
	// Every problem is reported at once.
	if errs := (PaymentTermVersionInput{}).Validate(); len(errs) < 5 {
		t.Errorf("an empty term: %d problems %v", len(errs), errs)
	}
}

func TestFormulaText_IsTheRuleInWords(t *testing.T) {
	in := net(30, ConventionModifiedFollowing)
	in.DiscountPercent, in.DiscountDays, in.GraceDays = "2", ip(10), 5
	for _, want := range []string{"invoice date + 30 days", "modified following business day by calendar gb-bank", "2% discount if paid within 10 days", "5 days grace"} {
		if got := in.FormulaText(); !strings.Contains(got, want) {
			t.Errorf("formula %q lacks %q", got, want)
		}
	}
	eom := PaymentTermVersionInput{BasisDateRule: BasisReceiptDate, DueMethod: DueEndOfMonthPlus, DueDays: 15, BusinessDayConvention: ConventionNone}
	if got := eom.FormulaText(); got != "end of the month of the receipt date + 15 days" {
		t.Errorf("formula = %q", got)
	}
	dom := PaymentTermVersionInput{BasisDateRule: BasisInvoiceDate, DueMethod: DueDayOfNextMonth, DueDayOfMonth: ip(10), BusinessDayConvention: ConventionNone}
	if got := dom.FormulaText(); got != "day 10 of the month after the invoice date" {
		t.Errorf("formula = %q", got)
	}
}

func TestContentDigest_IsWhatTheTermDoes(t *testing.T) {
	base := net(30, ConventionFollowing)
	d := base.ContentDigest("NET30")
	if len(d) != 64 {
		t.Fatalf("digest = %q", d)
	}
	// Rationale, evidence and how the percent is spelled are not what the term does.
	same := base
	same.Rationale, same.SourceRef = "something else entirely", "ref-9"
	same.DiscountPercent, same.DiscountDays = "2", ip(10)
	withDiscount := same.ContentDigest("NET30")
	same.DiscountPercent = "2.0000"
	if same.ContentDigest("NET30") != withDiscount {
		t.Error("2 and 2.0000 are different discounts")
	}
	same2 := base
	same2.Rationale = "different"
	if same2.ContentDigest("NET30") != d {
		t.Error("the rationale changed the digest")
	}
	for name, mutate := range map[string]func(*PaymentTermVersionInput){
		"days":       func(i *PaymentTermVersionInput) { i.DueDays = 31 },
		"basis":      func(i *PaymentTermVersionInput) { i.BasisDateRule = BasisReceiptDate },
		"method":     func(i *PaymentTermVersionInput) { i.DueMethod = DueEndOfMonthPlus },
		"convention": func(i *PaymentTermVersionInput) { i.BusinessDayConvention = ConventionPreceding },
		"calendar":   func(i *PaymentTermVersionInput) { i.CalendarCode = "us-fed" },
		"grace":      func(i *PaymentTermVersionInput) { i.GraceDays = 3 },
		"a discount": func(i *PaymentTermVersionInput) { i.DiscountPercent, i.DiscountDays = "1", ip(5) },
		"the start":  func(i *PaymentTermVersionInput) { i.EffectiveFrom = "2026-02-01" },
	} {
		in := base
		mutate(&in)
		if in.ContentDigest("NET30") == d {
			t.Errorf("changing the %s did not change the digest", name)
		}
	}
	if base.ContentDigest("NET45") == d {
		t.Error("another term's rule has the same digest")
	}
}

func TestValidTermCode(t *testing.T) {
	for _, ok := range []string{"NET30", "EOM45", "PROXIMO_10", "AB"} {
		if !ValidTermCode(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "net30", "30NET", "A", "NET 30", strings.Repeat("A", 33)} {
		if ValidTermCode(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}
