package domain

import (
	"database/sql/driver"
	"fmt"
)

// The TAX-03 enums are named string types, and the store scans directly into
// them so its scan-target list keeps matching its column list without threading
// an extra *string per enum through every read path.
//
// Explicit Scan/Value rather than relying on the driver's reflection fallback:
// these three decide the tax treatment of a supply, and a driver that declined
// to convert them would surface as a read error on every historical
// determination rather than at compile time.

func scanString(src any, into *string, typeName string) error {
	switch v := src.(type) {
	case nil:
		*into = ""
		return nil
	case string:
		*into = v
		return nil
	case []byte:
		*into = string(v)
		return nil
	default:
		return fmt.Errorf("cannot scan %T into %s", src, typeName)
	}
}

// Scan implements sql.Scanner for tax_determinations.supply_kind.
func (k *SupplyKind) Scan(src any) error {
	var s string
	if err := scanString(src, &s, "SupplyKind"); err != nil {
		return err
	}
	*k = SupplyKind(s)
	return nil
}

// Value implements driver.Valuer for tax_determinations.supply_kind.
func (k SupplyKind) Value() (driver.Value, error) { return string(k), nil }

// Scan implements sql.Scanner for tax_determinations.supply_type.
func (t *SupplyType) Scan(src any) error {
	var s string
	if err := scanString(src, &s, "SupplyType"); err != nil {
		return err
	}
	*t = SupplyType(s)
	return nil
}

// Value implements driver.Valuer for tax_determinations.supply_type.
func (t SupplyType) Value() (driver.Value, error) { return string(t), nil }

// Scan implements sql.Scanner for tax_determinations.place_of_supply_basis.
func (b *PlaceOfSupplyBasis) Scan(src any) error {
	var s string
	if err := scanString(src, &s, "PlaceOfSupplyBasis"); err != nil {
		return err
	}
	*b = PlaceOfSupplyBasis(s)
	return nil
}

// Value implements driver.Valuer for tax_determinations.place_of_supply_basis.
func (b PlaceOfSupplyBasis) Value() (driver.Value, error) { return string(b), nil }
