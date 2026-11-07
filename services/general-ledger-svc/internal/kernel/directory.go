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

// AccountScope is where and how an account may be used: the books it is valid in, its currency restriction, the area that
// owns it (control type) and whether its lines must name a counterparty.
type AccountScope struct {
	BookIDs              []string
	Currency             string
	ControlType          string
	CounterpartyRequired bool
	// DirectPostingRestricted, when set, states whether manual journals are barred from the account. Left unset, an account that
	// is given a control type becomes restricted. A control account that a subledger service still posts to through the legacy
	// posting command is opened by setting it false, which is evented and is the operator's decision, not a default.
	DirectPostingRestricted *bool
	// DimensionPolicy says, by dimension, whether the account's lines must carry it (REQUIRED), may (OPTIONAL) or must not (PROHIBITED).
	DimensionPolicy map[string]string
	// ReplacementAccount is the account a retired account maps forward to: a posting that resolves to the retired account lands on it, and
	// says so in its evidence. Posted lines keep the account they were posted to.
	ReplacementAccount string
}

// AccountApplicability is the optional part of the chart that says where an account may be used: the books it is valid in and
// the one currency it is restricted to (standard §12). A RefData that does not implement it treats every account as valid
// everywhere, which is what every account was before applicability existed.
type AccountApplicability interface {
	AccountApplicability(ctx context.Context, tenant, code string) (AccountScope, error)
}

// CurrencyUnits answers how many minor-unit decimals a currency has. Precision is reference data owned by the currency
// registry; a kernel that guessed it would be hard-coding currencies.
type CurrencyUnits interface {
	MinorUnits(ctx context.Context, code string, asOf time.Time) (int, error)
}

// FXSource returns the rate basis for a conversion. A nil source means the caller must supply the rate.
type FXSource interface {
	Basis(ctx context.Context, policyID, from, to string, asOf time.Time) (*FXBasis, error)
}

// ScopedAccountStatuser is the optional part of RefData that answers whether an entity- or book-scoped version of an account changes
// whether it is open for use in the book on a date. A RefData without it uses the tenant version for every book.
type ScopedAccountStatuser interface {
	ScopedAccountStatus(ctx context.Context, tenant, code, bookID string, on time.Time) (status string, overridden bool, err error)
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
func (d TenantDirectory) ResolveAccount(ctx context.Context, ref AccountRef, bookID string, asOf time.Time) (*AccountInfo, error) {
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
	info, err := d.resolveCode(ctx, code, bookID, asOf)
	if err != nil {
		return nil, err
	}
	// a retired account that names its replacement maps forward: the posting lands on the replacement, and the evidence says so. A chain
	// is followed to its end, and one that loops or runs on is refused rather than guessed at.
	var path []string
	for !info.Active && info.Replacement != "" {
		if len(path) >= maxReplacementHops || containsStr(path, info.Replacement) || info.Replacement == code {
			return nil, fmt.Errorf("account %q is retired and its replacement chain does not end", code)
		}
		path = append(path, info.Code)
		next, err := d.resolveCode(ctx, info.Replacement, bookID, asOf)
		if err != nil {
			return nil, fmt.Errorf("account %q is retired and its replacement %q cannot be used: %w", info.Code, info.Replacement, err)
		}
		info = next
	}
	if len(path) > 0 {
		info.ReplacedFrom = path
	}
	return info, nil
}

// maxReplacementHops bounds how far a retired account is followed to its replacement.
const maxReplacementHops = 5

// resolveCode reads one account: its status, where it may be used and how its lines are classified.
func (d TenantDirectory) resolveCode(ctx context.Context, code, bookID string, asOf time.Time) (*AccountInfo, error) {
	active, postable, found, err := d.Ref.AccountFacts(ctx, d.Tenant, code)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("account %q is not in the chart of accounts", code)
	}
	info := &AccountInfo{Code: code, Active: active, Postable: postable}
	// an entity- or book-scoped version can close the account for this book (or open it again) without touching the tenant version
	if ss, ok := d.Ref.(ScopedAccountStatuser); ok && bookID != "" {
		st, overridden, serr := ss.ScopedAccountStatus(ctx, d.Tenant, code, bookID, asOf)
		if serr != nil {
			return nil, serr
		}
		if overridden {
			info.Active = st == "ACTIVE"
		}
	}
	if ap, ok := d.Ref.(AccountApplicability); ok {
		sc, err := ap.AccountApplicability(ctx, d.Tenant, code)
		if err != nil {
			return nil, err
		}
		info.BookIDs, info.ControlType, info.CounterpartyRequired = sc.BookIDs, sc.ControlType, sc.CounterpartyRequired
		info.DimensionPolicy, info.Replacement = sc.DimensionPolicy, sc.ReplacementAccount
		if sc.Currency != "" {
			info.CurrencyRestricted = []string{sc.Currency}
		}
	}
	return info, nil
}

// DimensionAllowed implements Directory.
func (d TenantDirectory) DimensionAllowed(ctx context.Context, dim, value, legalEntityID, _ string, asOf time.Time) (bool, error) {
	return d.Ref.DimensionAllowed(ctx, d.Tenant, dim, value, legalEntityID, asOf)
}

// FXBasis implements Resolver.
// RejectSuppliedFX reports whether a caller-supplied rate is refused: it is wherever a rate source is configured.
func (d TenantDirectory) RejectSuppliedFX() bool { return d.FX != nil }

func (d TenantDirectory) FXBasis(ctx context.Context, policyID, from, to string, asOf time.Time) (*FXBasis, error) {
	if d.FX == nil {
		return nil, fmt.Errorf("no exchange-rate source is configured: supply fx_basis")
	}
	return d.FX.Basis(ctx, policyID, from, to, asOf)
}

// ValidateScope checks what an account's scope says before it is stored: a dimension policy names dimension codes and one of the three
// modes, and a replacement is an account code other than the account itself.
func ValidateScope(code string, sc AccountScope) error {
	bad := func(f, why string) error { return exc(FamCOA, "SCOPE_INVALID", "%s: %s", f, why).with("field", f) }
	if len(sc.DimensionPolicy) > 24 {
		return bad("dimension_policy", "at most 24 dimensions")
	}
	for d, m := range sc.DimensionPolicy {
		if !codeRE.MatchString(d) {
			return bad("dimension_policy."+d, "a dimension code")
		}
		if m != DimRequired && m != DimOptional && m != DimProhibited {
			return bad("dimension_policy."+d, "REQUIRED, OPTIONAL or PROHIBITED")
		}
	}
	if sc.ReplacementAccount != "" {
		if !codeRE.MatchString(sc.ReplacementAccount) {
			return bad("replacement_account", "an account code")
		}
		if sc.ReplacementAccount == code {
			return bad("replacement_account", "an account cannot replace itself")
		}
	}
	return nil
}
