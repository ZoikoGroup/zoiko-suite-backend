package domain

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// DateLayout is the only accepted wire format for a business date: ISO 8601
// calendar date, no time, no offset.
const DateLayout = "2006-01-02"

// Date is a calendar date with no time-of-day and no zone.
//
// ACC-03's transaction and posting dates are days in an entity's books, not
// instants. Modelling them as time.Time was the alternative and it fails in a
// specific way: two postings made on the same business day in London and
// Auckland serialise to timestamps that fall either side of midnight UTC, and
// any period boundary computed from them puts the same day's work in two
// periods. A date carries exactly the precision the business fact has.
//
// The zero Date marshals as null and is what a missing input looks like, so
// "no date supplied" stays distinguishable from "1 January year zero".
type Date struct {
	time.Time
}

// NewDate builds a Date from y/m/d.
func NewDate(year int, month time.Month, day int) Date {
	return Date{time.Date(year, month, day, 0, 0, 0, 0, time.UTC)}
}

// ParseDate parses "2006-01-02".
//
// It rejects anything else rather than falling back to a lenient parse. A
// posting date is the field that decides which period a transaction lands in,
// and a parser that accepts both "03/04/2026" readings would silently move
// entries by nine months.
func ParseDate(s string) (Date, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Date{}, fmt.Errorf("date is empty, expected %s", DateLayout)
	}
	t, err := time.Parse(DateLayout, s)
	if err != nil {
		return Date{}, fmt.Errorf("date %q is not %s", s, DateLayout)
	}
	return Date{t}, nil
}

// IsZero reports whether no date was supplied.
func (d Date) IsZero() bool { return d.Time.IsZero() }

// String renders the date, or "" when unset.
func (d Date) String() string {
	if d.IsZero() {
		return ""
	}
	return d.Format(DateLayout)
}

// MarshalJSON emits "2026-08-24", or null when unset.
func (d Date) MarshalJSON() ([]byte, error) {
	if d.IsZero() {
		return []byte("null"), nil
	}
	return []byte(`"` + d.Format(DateLayout) + `"`), nil
}

// UnmarshalJSON accepts "2026-08-24" or null.
//
// A full RFC3339 timestamp is refused rather than truncated: truncating would
// silently discard a zone the caller thought mattered, and the caller that sent
// one has a different idea of what this field means than the ledger does.
func (d *Date) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == `""` {
		*d = Date{}
		return nil
	}
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return fmt.Errorf("date must be a JSON string in %s form", DateLayout)
	}
	parsed, err := ParseDate(s[1 : len(s)-1])
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

// Scan implements sql.Scanner for journal_headers.journal_type, so the store's
// scan-target list keeps matching its column list without threading an extra
// *string through every read path. Same reason JournalStatus does not have one:
// status predates this and is converted at the four call sites instead.
func (t *JournalType) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*t = ""
		return nil
	case string:
		*t = JournalType(v)
		return nil
	case []byte:
		*t = JournalType(v)
		return nil
	default:
		return fmt.Errorf("cannot scan %T into JournalType", src)
	}
}

// Value implements driver.Valuer for journal_headers.journal_type.
func (t JournalType) Value() (driver.Value, error) { return string(t), nil }

// Dimensions is ACC-03's per-line analysis axes, stored as JSONB.
//
// A named type rather than a bare map so it can carry its own Scan/Value —
// pgx will not marshal a map[string]string into jsonb on its own, and doing it
// inline at each call site is how the encode and decode halves drift apart.
type Dimensions map[string]string

// Scan implements sql.Scanner for journal_lines.dimensions.
func (d *Dimensions) Scan(src any) error {
	var raw []byte
	switch v := src.(type) {
	case nil:
		*d = nil
		return nil
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	default:
		return fmt.Errorf("cannot scan %T into Dimensions", src)
	}
	if len(raw) == 0 || string(raw) == "null" {
		*d = nil
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("dimensions is not a JSON object of strings: %w", err)
	}
	*d = m
	return nil
}

// Value implements driver.Valuer for journal_lines.dimensions.
//
// An empty map stores as SQL NULL, not as "{}". "This line has no dimensions"
// and "this line has an empty dimension set" are the same fact, and keeping one
// representation means a reader never has to test for both.
func (d Dimensions) Value() (driver.Value, error) {
	if len(d) == 0 {
		return nil, nil
	}
	return json.Marshal(map[string]string(d))
}

// Value implements driver.Valuer for the DATE columns.
func (d Date) Value() (driver.Value, error) {
	if d.IsZero() {
		return nil, nil
	}
	return d.Time, nil
}

// Scan implements sql.Scanner for the DATE columns.
//
// pgx hands a DATE back as time.Time; the string cases cover a driver
// configured to return dates as text, which is a supported pgx mode.
func (d *Date) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*d = Date{}
		return nil
	case time.Time:
		*d = Date{time.Date(v.Year(), v.Month(), v.Day(), 0, 0, 0, 0, time.UTC)}
		return nil
	case string:
		parsed, err := ParseDate(v)
		if err != nil {
			return err
		}
		*d = parsed
		return nil
	case []byte:
		parsed, err := ParseDate(string(v))
		if err != nil {
			return err
		}
		*d = parsed
		return nil
	default:
		return fmt.Errorf("cannot scan %T into Date", src)
	}
}
