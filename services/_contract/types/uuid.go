package types

import (
	"crypto/rand"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var uuidRegex = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// UUID represents a canonical 128-bit identifier formatted as 8-4-4-4-12 hex string.
// In accordance with ZS-DATA-001 Section 5.1 & Invariant D02, internal identities
// are time-ordered RFC 9562 UUIDv7 values.
type UUID string

// NilUUID represents an empty UUID.
const NilUUID UUID = "00000000-0000-0000-0000-000000000000"

// NewV7 generates a new RFC 9562 UUIDv7 based on the current Unix timestamp in milliseconds.
// Layout:
// - 48 bits: Unix timestamp in ms
// - 4 bits: Version 7 (0b0111)
// - 12 bits: Random sequence
// - 2 bits: Variant 10 (0b10)
// - 62 bits: Random bits
func NewV7() (UUID, error) {
	return NewV7AtTime(time.Now())
}

// MustNewV7 generates a new UUIDv7 or panics on entropy failure.
func MustNewV7() UUID {
	u, err := NewV7()
	if err != nil {
		panic(err)
	}
	return u
}

// NewV7AtTime generates an RFC 9562 UUIDv7 with a specific timestamp (useful for backfill/testing).
func NewV7AtTime(t time.Time) (UUID, error) {
	var b [16]byte

	// 48-bit timestamp in milliseconds
	milli := uint64(t.UnixMilli())
	b[0] = byte(milli >> 40)
	b[1] = byte(milli >> 32)
	b[2] = byte(milli >> 24)
	b[3] = byte(milli >> 16)
	b[4] = byte(milli >> 8)
	b[5] = byte(milli)

	// Random entropy for the remaining 10 bytes
	if _, err := rand.Read(b[6:]); err != nil {
		return NilUUID, fmt.Errorf("failed to read secure random entropy: %w", err)
	}

	// Set version 7 in bits 48..51 (0111)
	b[6] = (b[6] & 0x0f) | 0x70

	// Set variant in bits 64..65 (10)
	b[8] = (b[8] & 0x3f) | 0x80

	return formatUUID(b), nil
}

// ParseUUID parses and validates a UUID string.
func ParseUUID(s string) (UUID, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if !uuidRegex.MatchString(s) {
		return NilUUID, fmt.Errorf("invalid UUID format: %q", s)
	}
	return UUID(s), nil
}

// MustParseUUID parses a UUID or panics if invalid.
func MustParseUUID(s string) UUID {
	u, err := ParseUUID(s)
	if err != nil {
		panic(err)
	}
	return u
}

// String returns the string representation.
func (u UUID) String() string {
	return string(u)
}

// IsNil returns true if the UUID is empty or all zeros.
func (u UUID) IsNil() bool {
	return u == "" || u == NilUUID
}

// Version returns the UUID version (e.g. 7 for UUIDv7, 4 for UUIDv4).
func (u UUID) Version() (int, error) {
	clean := strings.ReplaceAll(string(u), "-", "")
	if len(clean) != 32 {
		return 0, errors.New("invalid UUID length")
	}
	var b [16]byte
	if _, err := hex.Decode(b[:], []byte(clean)); err != nil {
		return 0, err
	}
	return int(b[6] >> 4), nil
}

// Timestamp extracts the Unix millisecond time from a UUIDv7.
func (u UUID) Timestamp() (time.Time, error) {
	v, err := u.Version()
	if err != nil {
		return time.Time{}, err
	}
	if v != 7 {
		return time.Time{}, fmt.Errorf("cannot extract timestamp from UUID version %d (only supported on v7)", v)
	}

	clean := strings.ReplaceAll(string(u), "-", "")
	var b [16]byte
	if _, err := hex.Decode(b[:], []byte(clean)); err != nil {
		return time.Time{}, err
	}

	milli := (uint64(b[0]) << 40) |
		(uint64(b[1]) << 32) |
		(uint64(b[2]) << 24) |
		(uint64(b[3]) << 16) |
		(uint64(b[4]) << 8) |
		uint64(b[5])

	return time.UnixMilli(int64(milli)), nil
}

func formatUUID(b [16]byte) UUID {
	return UUID(fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4],
		b[4:6],
		b[6:8],
		b[8:10],
		b[10:16],
	))
}

// Value implements driver.Valuer for PostgreSQL UUID column persistence.
func (u UUID) Value() (driver.Value, error) {
	if u.IsNil() {
		return nil, nil
	}
	return string(u), nil
}

// Scan implements sql.Scanner for PostgreSQL UUID column retrieval.
func (u *UUID) Scan(src any) error {
	if src == nil {
		*u = NilUUID
		return nil
	}

	switch v := src.(type) {
	case string:
		parsed, err := ParseUUID(v)
		if err != nil {
			return err
		}
		*u = parsed
		return nil
	case []byte:
		parsed, err := ParseUUID(string(v))
		if err != nil {
			return err
		}
		*u = parsed
		return nil
	default:
		return errors.New("unsupported database driver type for UUID scan")
	}
}
