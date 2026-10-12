package domain

import (
	"encoding/json"
	"fmt"
	"time"
)

const dateLayout = "2006-01-02"

// Date is a calendar date with no time of day, always UTC. Fiscal-calendar
// boundaries are dates, never instants; using time.Time for them invites
// time-zone and DST bugs. It marshals as "YYYY-MM-DD".
type Date struct{ time.Time }

// NewDate builds a Date.
func NewDate(year int, month time.Month, day int) Date {
	return Date{time.Date(year, month, day, 0, 0, 0, 0, time.UTC)}
}

// ParseDate parses "YYYY-MM-DD" strictly.
func ParseDate(s string) (Date, error) {
	t, err := time.ParseInLocation(dateLayout, s, time.UTC)
	if err != nil {
		return Date{}, fmt.Errorf("date %q must be YYYY-MM-DD", s)
	}
	return Date{t}, nil
}

// FromTime truncates an instant to its UTC date.
func FromTime(t time.Time) Date {
	u := t.UTC()
	return NewDate(u.Year(), u.Month(), u.Day())
}

// String is YYYY-MM-DD.
func (d Date) String() string { return d.Time.UTC().Format(dateLayout) }

// AddDays returns d shifted by n days.
func (d Date) AddDays(n int) Date { return Date{d.Time.AddDate(0, 0, n)} }

// Before / After / Equal compare dates only.
func (d Date) Before(o Date) bool { return d.Time.Before(o.Time) }
func (d Date) After(o Date) bool  { return d.Time.After(o.Time) }
func (d Date) Equal(o Date) bool  { return d.Time.Equal(o.Time) }

// IsZero reports the zero Date.
func (d Date) IsZero() bool { return d.Time.IsZero() }

// DaysUntil is the number of days from d to o (o - d).
func (d Date) DaysUntil(o Date) int {
	return int(o.Time.Sub(d.Time).Hours() / 24)
}

// MarshalJSON renders "YYYY-MM-DD".
func (d Date) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// UnmarshalJSON accepts only "YYYY-MM-DD".
func (d *Date) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("date must be a YYYY-MM-DD string")
	}
	p, err := ParseDate(s)
	if err != nil {
		return err
	}
	*d = p
	return nil
}

// Interval is a half-open date interval [From, To). To == nil means unbounded.
type Interval struct {
	From Date
	To   *Date
}

// Contains reports whether date falls inside the interval.
func (i Interval) Contains(d Date) bool {
	if d.Before(i.From) {
		return false
	}
	return i.To == nil || d.Before(*i.To)
}

// Overlaps reports whether two half-open intervals share at least one day.
func (i Interval) Overlaps(o Interval) bool {
	// i starts before o ends, and o starts before i ends (nil = +infinity).
	if o.To != nil && !i.From.Before(*o.To) {
		return false
	}
	if i.To != nil && !o.From.Before(*i.To) {
		return false
	}
	return true
}
