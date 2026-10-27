package kernel

import (
	"context"
	"fmt"
	"time"
)

// RefData is the chart, mapping and dimension data of a tenant, as the store holds it.
type RefData interface {
	AccountFacts(ctx context.Context, tenant, code string) (active, postable, found bool, err error)
	MappedAccount(ctx context.Context, tenant, key string, on time.Time) (code string, found bool, err error)
	DimensionAllowed(ctx context.Context, tenant, dimension, value, legalEntityID string, on time.Time) (bool, error)
}

// CurrencyUnits answers how many minor-unit decimals a currency has. Precision is reference data owned by the currency
// registry; a kernel that guessed it would be hard-coding currencies.
type CurrencyUnits interface {
	MinorUnits(ctx context.Context, code string, asOf time.Time) (int, error)
}

// FXSource returns the rate basis for a conversion. A nil source means the caller must supply the rate.
type FXSource interface {
	Basis(ctx context.Context, from, to string, asOf time.Time) (*FXBasis, error)
}

// TenantDirectory is the engine's Directory (and the Evaluate Resolver) for one tenant.
type TenantDirectory struct {
	Tenant string
	Ref    RefData
	Units  CurrencyUnits
	FX     FXSource
}

// MinorUnits implements Directory.
func (d TenantDirectory) MinorUnits(ctx context.Context, code string, asOf time.Time) (int, error) {
	if d.Units == nil {
		return 0, fmt.Errorf("no currency registry is configured")
	}
	return d.Units.MinorUnits(ctx, code, asOf)
}

// ResolveAccount implements Directory: a mapping key resolves through the tenant's mapping in force on the date, and the
// account must exist, be active and not be a header account.
func (d TenantDirectory) ResolveAccount(ctx context.Context, ref AccountRef, _ string, asOf time.Time) (*AccountInfo, error) {
	code := ref.AccountCode
	if ref.MappingKey != "" {
		c, found, err := d.Ref.MappedAccount(ctx, d.Tenant, ref.MappingKey, asOf)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("no account mapping %q is in force on %s", ref.MappingKey, asOf.Format("2006-01-02"))
		}
		code = c
	}
	active, postable, found, err := d.Ref.AccountFacts(ctx, d.Tenant, code)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("account %q is not in the chart of accounts", code)
	}
	return &AccountInfo{Code: code, Active: active, Postable: postable}, nil
}

// DimensionAllowed implements Directory.
func (d TenantDirectory) DimensionAllowed(ctx context.Context, dim, value, legalEntityID, _ string, asOf time.Time) (bool, error) {
	return d.Ref.DimensionAllowed(ctx, d.Tenant, dim, value, legalEntityID, asOf)
}

// FXBasis implements Resolver.
func (d TenantDirectory) FXBasis(ctx context.Context, from, to string, asOf time.Time) (*FXBasis, error) {
	if d.FX == nil {
		return nil, fmt.Errorf("no exchange-rate source is configured: supply fx_basis")
	}
	return d.FX.Basis(ctx, from, to, asOf)
}
