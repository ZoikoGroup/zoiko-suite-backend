package domain

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// Dimensions is AP-05's per-line analysis axes — cost centre, project,
// department — stored as JSONB.
//
// A named type rather than a bare map so it carries its own Scan/Value: pgx will
// not marshal a map[string]string into jsonb on its own, and doing it inline at
// each call site is how the encode and decode halves drift apart. Same posture
// and same reasoning as general-ledger-svc's journal line dimensions.
//
// Free-form keys because REF-08 Financial Dimension Registry does not exist.
// Nothing can say which dimensions a tenant has defined or which values are
// valid for one, so they are recorded as supplied and validated by nothing.
type Dimensions map[string]string

// Scan implements sql.Scanner for vendor_invoice_lines.dimensions.
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

// Value implements driver.Valuer for vendor_invoice_lines.dimensions.
//
// An empty map stores as SQL NULL rather than "{}". "This line has no
// dimensions" and "this line has an empty dimension set" are the same fact, and
// keeping one representation means a reader never has to test for both.
func (d Dimensions) Value() (driver.Value, error) {
	if len(d) == 0 {
		return nil, nil
	}
	return json.Marshal(map[string]string(d))
}
