package searchclient

import (
	"errors"
	"fmt"
	"strings"
)

// TenantField is the projection field every tenant scope filters on. It is the
// same governance keyword field GovernanceProperties maps for every generation,
// so the filter is an exact match and never an analysed one.
const TenantField = "tenant_id"

// ErrMandatoryFilterEmpty is returned by ExecutePlan when a mandatory filter
// (or mandatory exclusion) names no usable value.
//
// INV-04 makes the mandatory filters unremovable and INV-07 makes an
// unresolved scope fail closed. A filter whose value list is empty carries no
// constraint at all, and silently skipping it — which compile() used to do —
// turned an unresolved residency or ACL scope into a wider query rather than
// into no query. It is refused here, before any network call.
var ErrMandatoryFilterEmpty = errors.New("searchclient: mandatory filter has no usable value")

// ErrInvalidTarget is returned by ExecutePlan when the target is not a single
// concrete index or alias name.
var ErrInvalidTarget = errors.New("searchclient: invalid search target")

// validate refuses a plan that could execute with less scope than its
// mandatory filters promise. Pure and local: no plan that fails here reaches
// the engine.
//
// Rules, all from the ESR contract:
//   - every MandatoryFilters / MandatoryMustNot entry names a field and at
//     least one non-blank value (INV-04, INV-07);
//   - the plan carries a tenant filter and it names exactly ONE tenant
//     (INV-02: tenant identity is trusted, not inferred; INV-11: cross-tenant
//     retrieval is prohibited unless a cross-tenant authority permits it,
//     which this package has no way to express).
//
// UserFilters are deliberately not validated here: they only ever narrow, so a
// dropped one cannot widen the mandatory scope.
func (p ExecutionPlan) validate() error {
	tenantFilters := 0
	for _, f := range p.MandatoryFilters {
		if f.Field == TenantField {
			// An unusable tenant is the README's ErrTenantIDRequired, not the
			// generic empty-filter error.
			if err := f.validateMandatory("filter"); err != nil {
				return fmt.Errorf("%w: %v", ErrTenantIDRequired, err)
			}
		} else if err := f.validateMandatory("filter"); err != nil {
			return err
		}
		if f.Field == TenantField {
			tenantFilters++
			if len(f.Values) != 1 {
				return fmt.Errorf("%w: the tenant filter must name exactly one tenant, got %d",
					ErrTenantIDRequired, len(f.Values))
			}
		}
	}
	for _, f := range p.MandatoryMustNot {
		if err := f.validateMandatory("exclusion"); err != nil {
			return err
		}
	}
	if tenantFilters == 0 {
		return fmt.Errorf("%w: the plan has no %s filter", ErrTenantIDRequired, TenantField)
	}
	return nil
}

func (f TermFilter) validateMandatory(kind string) error {
	if strings.TrimSpace(f.Field) == "" {
		return fmt.Errorf("%w: mandatory %s with no field", ErrMandatoryFilterEmpty, kind)
	}
	if len(f.Values) == 0 {
		return fmt.Errorf("%w: mandatory %s on %q has no values", ErrMandatoryFilterEmpty, kind, f.Field)
	}
	for _, v := range f.Values {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%w: mandatory %s on %q has a blank value", ErrMandatoryFilterEmpty, kind, f.Field)
		}
	}
	return nil
}

// validateTarget accepts one concrete name and nothing that expands.
//
// INV-03: a name must never select a broader partition than the one the
// control plane chose. Wildcards, comma lists, _all, hidden/system names,
// date-math and cluster-qualified names all expand or redirect, so none can
// be a target. The names this package itself produces — an Alias(scope), or a
// GenerationIndex(scope, id) for the generation-scoped retrieval evaluation —
// contain only [a-z0-9._-] and pass.
//
// What this cannot do is tell an alias from a concrete generation index, or
// know whether a name came from a caller's request: both are the caller's
// knowledge, and the ESR contract defines no alias naming rule to check
// against. The caller must pass server-owned names only.
func validateTarget(target string) error {
	if target == "" || target != strings.TrimSpace(target) {
		return fmt.Errorf("%w: %q", ErrInvalidTarget, target)
	}
	if strings.ContainsAny(target, "*?,/\\:\"<>|# \t\r\n") {
		return fmt.Errorf("%w: %q must be a single concrete name", ErrInvalidTarget, target)
	}
	switch target[0] {
	case '_', '-', '+', '.':
		return fmt.Errorf("%w: %q may not start with %q", ErrInvalidTarget, target, target[:1])
	}
	return nil
}

// validate is the one input check both projection writers share, so a path
// cannot be added that skips it. ESR §2.1 defines a ProjectionDocument as
// keyed to tenant and source_type/id; legal_entity_id is not part of that
// definition and is not required here.
func (p Projection) validate(op string) error {
	if strings.TrimSpace(p.DocID) == "" {
		return fmt.Errorf("searchclient: %s: DocID must not be empty", op)
	}
	if strings.TrimSpace(p.TenantID) == "" {
		return ErrTenantIDRequired
	}
	if strings.TrimSpace(p.SourceType) == "" || strings.TrimSpace(p.SourceID) == "" {
		return fmt.Errorf("searchclient: %s: SourceType and SourceID must not be empty", op)
	}
	return nil
}
