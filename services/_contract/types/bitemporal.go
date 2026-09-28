package types

import (
	"errors"
	"fmt"
	"time"
)

// BitemporalRecord encapsulates the 4 temporal dimensions required by ZS-DATA-001 Section 6.
//
// 1. Valid Time (Business Time): When the fact is true in the real world (valid_from .. valid_to).
// 2. System Time (Knowledge Time): When ZoikoSuite recorded/superseded that version (recorded_at .. superseded_at).
type BitemporalRecord struct {
	// ValidFrom is the start of real-world business validity.
	ValidFrom time.Time `json:"valid_from"`
	// ValidTo is the end of real-world business validity (nil if currently active).
	ValidTo *time.Time `json:"valid_to,omitempty"`
	// RecordedAt is the immutable UTC timestamp when this record version was committed into the database.
	RecordedAt time.Time `json:"recorded_at"`
	// SupersededAt is the UTC timestamp when a newer correction superseded this version (nil if current known truth).
	SupersededAt *time.Time `json:"superseded_at,omitempty"`
}

// NewBitemporalRecord creates a new active bitemporal record starting from validFrom.
func NewBitemporalRecord(validFrom time.Time, recordedAt time.Time) BitemporalRecord {
	return BitemporalRecord{
		ValidFrom:  validFrom.UTC(),
		RecordedAt: recordedAt.UTC(),
	}
}

// Validate checks temporal consistency:
// - ValidFrom must be <= ValidTo (if ValidTo is specified)
// - RecordedAt must be <= SupersededAt (if SupersededAt is specified)
func (b BitemporalRecord) Validate() error {
	if b.ValidFrom.IsZero() {
		return errors.New("bitemporal record requires non-zero valid_from")
	}
	if b.RecordedAt.IsZero() {
		return errors.New("bitemporal record requires non-zero recorded_at")
	}
	if b.ValidTo != nil && b.ValidTo.Before(b.ValidFrom) {
		return fmt.Errorf("valid_to (%s) cannot be before valid_from (%s)", b.ValidTo.Format(time.RFC3339), b.ValidFrom.Format(time.RFC3339))
	}
	if b.SupersededAt != nil && b.SupersededAt.Before(b.RecordedAt) {
		return fmt.Errorf("superseded_at (%s) cannot be before recorded_at (%s)", b.SupersededAt.Format(time.RFC3339), b.RecordedAt.Format(time.RFC3339))
	}
	return nil
}

// IsCurrentKnowledge returns true if this record version has not been superseded.
func (b BitemporalRecord) IsCurrentKnowledge() bool {
	return b.SupersededAt == nil
}

// IsBusinessActiveAt checks whether the fact was effective in the real world on targetDate.
func (b BitemporalRecord) IsBusinessActiveAt(targetDate time.Time) bool {
	t := targetDate.UTC()
	if t.Before(b.ValidFrom) {
		return false
	}
	if b.ValidTo != nil && !t.Before(*b.ValidTo) {
		return false
	}
	return true
}

// WasKnownAt checks whether the system knew this record version at systemTime.
func (b BitemporalRecord) WasKnownAt(systemTime time.Time) bool {
	t := systemTime.UTC()
	if t.Before(b.RecordedAt) {
		return false
	}
	if b.SupersededAt != nil && !t.Before(*b.SupersededAt) {
		return false
	}
	return true
}

// AsOf evaluates whether this record is the authoritative answer for an "As-Of" bitemporal query:
// - businessDate: what was effective on that date
// - asOfSystemTime: what the system knew at that point in time
func (b BitemporalRecord) AsOf(businessDate time.Time, asOfSystemTime time.Time) bool {
	return b.IsBusinessActiveAt(businessDate) && b.WasKnownAt(asOfSystemTime)
}
