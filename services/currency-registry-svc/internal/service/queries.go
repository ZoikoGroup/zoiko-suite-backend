package service

import (
	"context"
	"time"

	"zoiko.io/currency-registry-svc/internal/domain"
)

// MinorUnitVersions is the response of the version-history query.
type MinorUnitVersions struct {
	CurrencyID string                    `json:"currency_id"`
	AlphaCode  string                    `json:"alpha_code"`
	Versions   []domain.MinorUnitVersion `json:"versions"`
}

// GetCurrency resolves a currency by alpha code. With asOf == nil it returns
// the current record and the minor-unit version in force now; with asOf it
// returns the record that was valid at that instant and the minor-unit version
// in force then. RETIRED currencies stay readable. A code that never existed,
// or did not yet exist at asOf, is NOT_FOUND.
func (s *Service) GetCurrency(ctx context.Context, code string, asOf *time.Time) (*domain.Currency, error) {
	var out *domain.Currency
	err := s.store.InTx(ctx, "", func(tx Tx) error {
		cs, err := tx.FindCurrenciesByAlpha(ctx, code)
		if err != nil {
			return err
		}
		t := s.now()
		var c *domain.Currency
		if asOf != nil {
			t = asOf.UTC()
			c = pickAsOf(cs, t)
		} else {
			c = pickCurrent(cs)
		}
		if c == nil {
			return nil
		}
		if err := s.attachMinorUnit(ctx, tx, c, t); err != nil {
			return err
		}
		out = c
		return nil
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, domain.Errf(domain.CodeNotFound, "no currency %q is known to the registry%s", code, asOfSuffix(asOf))
	}
	return out, nil
}

func asOfSuffix(asOf *time.Time) string {
	if asOf == nil {
		return ""
	}
	return " as of " + asOf.UTC().Format(time.RFC3339)
}

// ListCurrencies lists currencies, optionally by status.
func (s *Service) ListCurrencies(ctx context.Context, status string, limit, offset int) ([]domain.Currency, error) {
	if status != "" && !domain.Status(status).Valid() {
		return nil, domain.Errf(domain.CodeContextInvalid, "status must be one of KNOWN, SUPPORTED, RESTRICTED, RETIRED")
	}
	var out []domain.Currency
	err := s.store.InTx(ctx, "", func(tx Tx) error {
		cs, err := tx.ListCurrencies(ctx, status, limit, offset)
		if err != nil {
			return err
		}
		now := s.now()
		for i := range cs {
			if err := s.attachMinorUnit(ctx, tx, &cs[i], now); err != nil {
				return err
			}
		}
		out = cs
		return nil
	})
	return out, err
}

// ResolveNumeric resolves a numeric code to the current currency.
func (s *Service) ResolveNumeric(ctx context.Context, numeric string) (*domain.Currency, error) {
	var out *domain.Currency
	err := s.store.InTx(ctx, "", func(tx Tx) error {
		cs, err := tx.FindCurrenciesByNumeric(ctx, numeric)
		if err != nil {
			return err
		}
		c := pickCurrent(cs)
		if c == nil {
			return nil
		}
		if err := s.attachMinorUnit(ctx, tx, c, s.now()); err != nil {
			return err
		}
		out = c
		return nil
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, domain.Errf(domain.CodeNotFound, "no currency with numeric code %q is known to the registry", numeric)
	}
	return out, nil
}

// ListMinorUnitVersions returns the full append-only minor-unit history.
func (s *Service) ListMinorUnitVersions(ctx context.Context, code string) (*MinorUnitVersions, error) {
	var out *MinorUnitVersions
	err := s.store.InTx(ctx, "", func(tx Tx) error {
		cs, err := tx.FindCurrenciesByAlpha(ctx, code)
		if err != nil {
			return err
		}
		c := pickCurrent(cs)
		if c == nil {
			return nil
		}
		vs, err := tx.ListMinorUnitVersions(ctx, c.CurrencyID)
		if err != nil {
			return err
		}
		out = &MinorUnitVersions{CurrencyID: c.CurrencyID, AlphaCode: c.AlphaCode, Versions: withDerivedValidTo(vs, c.ValidTo)}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, domain.Errf(domain.CodeNotFound, "no currency %q is known to the registry", code)
	}
	return out, nil
}

// ListTenantSupport lists one tenant's overlay rows. tenantID must be the
// caller's own tenant; the store additionally filters by it (and by RLS).
func (s *Service) ListTenantSupport(ctx context.Context, tenantID string) ([]domain.TenantSupport, error) {
	if tenantID == "" {
		return nil, domain.Errf(domain.CodeContextInvalid, "tenant context is required")
	}
	var out []domain.TenantSupport
	err := s.store.InTx(ctx, tenantID, func(tx Tx) error {
		r, err := tx.ListTenantSupport(ctx, tenantID)
		out = r
		return err
	})
	return out, err
}

// Validate answers whether code may be used for operation. It never guesses:
// anything unknown, not activated, restricted, retired or not yet effective is
// reported supported=false with a reason, and minor_unit is null unless the
// registry actually holds one.
//
// With tenantScoped, a post additionally requires that tenantID has enabled the
// currency in its overlay.
func (s *Service) Validate(ctx context.Context, code, operation, tenantID string, tenantScoped bool) (*domain.ValidateResult, error) {
	if operation != domain.OperationPost && operation != domain.OperationRead {
		return nil, domain.Errf(domain.CodeContextInvalid, "operation must be %q or %q", domain.OperationPost, domain.OperationRead)
	}
	if tenantScoped && tenantID == "" {
		return nil, domain.Errf(domain.CodeContextInvalid, "tenant_scoped validation needs a tenant context")
	}
	res := &domain.ValidateResult{Code: code, Operation: operation}
	scopeTenant := ""
	if tenantScoped {
		scopeTenant = tenantID
	}
	err := s.store.InTx(ctx, scopeTenant, func(tx Tx) error {
		cs, err := tx.FindCurrenciesByAlpha(ctx, code)
		if err != nil {
			return err
		}
		c := pickCurrent(cs)
		if c == nil {
			res.Reason = domain.ReasonUnknownCurrency
			return nil
		}
		now := s.now()
		if err := s.attachMinorUnit(ctx, tx, c, now); err != nil {
			return err
		}
		res.CurrencyID, res.CurrencyVersion, res.Status = c.CurrencyID, c.Version, c.Status
		if c.MinorUnit != nil {
			mu := c.MinorUnit.MinorUnit
			from := c.MinorUnit.ValidFrom
			res.MinorUnit, res.MinorUnitFrom = &mu, &from
		}

		if operation == domain.OperationRead {
			res.Supported, res.Reason = true, domain.ReasonOK
			return nil
		}
		// operation == post: a NEW financial posting needs a live, SUPPORTED currency.
		switch {
		case c.MinorUnit == nil || c.ValidFrom.After(now):
			res.Reason = domain.ReasonNotYetEffective
		case c.Status == domain.StatusKnown:
			res.Reason = domain.ReasonKnownNotActivated
		case c.Status == domain.StatusRestricted:
			res.Reason = domain.ReasonRestricted
		case c.Status == domain.StatusRetired:
			res.Reason = domain.ReasonRetired
		default: // SUPPORTED
			res.Supported, res.Reason = true, domain.ReasonOK
			if tenantScoped {
				ts, err := tx.GetTenantSupport(ctx, tenantID, c.CurrencyID)
				if err != nil {
					return err
				}
				if ts == nil || !ts.Enabled {
					res.Supported, res.Reason = false, domain.ReasonNotEnabledForTenant
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}
