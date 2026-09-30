package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/configuration-feature-flag-svc/internal/domain"
	"zoiko.io/configuration-feature-flag-svc/internal/events"
)

// ── SERVICE / ORG_UNIT / USER_PREFERENCE layers (INV-07, migration 000013) ────

const layerColumns = `override_id, key, value, environment, tenant_id, effective_from, effective_to, created_by_principal_id, created_at`

func scanLayerOverride(row pgx.Row) (*domain.ConfigEntry, error) {
	e := &domain.ConfigEntry{}
	err := row.Scan(&e.ConfigID, &e.Key, &e.Value, &e.Environment, &e.TenantID,
		&e.EffectiveFrom, &e.EffectiveTo, &e.CreatedByPrincipalID, &e.CreatedAt)
	return e, err
}

// upsertLayerOverride sets the current value of one extended-layer scope:
// the current row, if any, is ended and a new one inserted — the same
// append-only history config_entries keeps.
func upsertLayerOverride(ctx context.Context, tx pgx.Tx, key, environment string, tenantID *string, layer, scopeID string, value json.RawMessage, actor string) (*domain.ConfigEntry, error) {
	if err := endLayerOverride(ctx, tx, key, environment, tenantID, layer, scopeID); err != nil {
		return nil, err
	}
	entry, err := scanLayerOverride(tx.QueryRow(ctx, `
		INSERT INTO config_layer_overrides (key, environment, tenant_id, layer, scope_id, value, created_by_principal_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING `+layerColumns,
		key, environment, tenantID, layer, scopeID, value, actor))
	if err != nil {
		return nil, mapWriteInsertError(err, "config_layer_overrides")
	}
	return entry, nil
}

// endLayerOverride ends one extended-layer scope's current value, if any.
func endLayerOverride(ctx context.Context, tx pgx.Tx, key, environment string, tenantID *string, layer, scopeID string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE config_layer_overrides SET effective_to = NOW()
		WHERE key = $1 AND environment = $2
		  AND COALESCE(tenant_id, '`+nilScopeUUID+`'::UUID) = COALESCE($3::uuid, '`+nilScopeUUID+`'::UUID)
		  AND layer = $4 AND scope_id = $5 AND effective_to IS NULL`,
		key, environment, tenantID, layer, scopeID); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

// activateLayerOverride is ActivateOverride for the three extended layers.
// The tenancy of each is fixed (SERVICE tenantless; ORG_UNIT and
// USER_PREFERENCE in the caller's tenant), and a user preference may be set
// only by that user — a preference set by someone else is not a preference.
func (s *PgStore) activateLayerOverride(ctx context.Context, params domain.ActivateOverrideParams, layer string) (*domain.ConfigEntry, error) {
	scopeID := ""
	if params.ScopeID != nil {
		scopeID = *params.ScopeID
	}
	var tenantID *string
	if layer != domain.LayerService {
		t := params.CallerTenantID
		tenantID = &t
	}
	if err := domain.ValidLayerTenancy(layer, tenantID, scopeID); err != nil {
		return nil, err
	}
	if layer == domain.LayerUserPreference && scopeID != params.ActorPrincipalID {
		return nil, domain.ErrScopeNotAllowed
	}

	var out *domain.ConfigEntry
	err := s.withTenantTx(ctx, params.CallerTenantID, func(tx pgx.Tx) error {
		def, err := publishedDefinition(ctx, tx, params.Key)
		if err != nil {
			return err
		}
		if domain.IsMaterial(def.SafetyClass) {
			return domain.ErrMaterialKeyRequiresChange
		}
		if err := domain.ValidateScope(def, layer); err != nil {
			return err
		}
		if err := domain.ValidateValue(params.Value, def, params.Environment); err != nil {
			return err
		}
		entry, err := upsertLayerOverride(ctx, tx, params.Key, params.Environment, tenantID, layer, scopeID, params.Value, params.ActorPrincipalID)
		if err != nil {
			return err
		}
		outEv, evErr := events.OverrideActivated(params, entry.EffectiveFrom)
		if err := enqueueEvent(ctx, tx, params.CallerTenantID, outEv, evErr); err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		if err := s.mintAndPublish(ctx, tx, "ActivateOverride", params.Environment, params.ActorPrincipalID, params.CallerTenantID, params.CorrelationID); err != nil {
			return err
		}
		out = entry
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// resolutionScopes is the trusted context resolve applies precedence with:
// who the caller is at each extended layer. Each id comes from the gateway,
// never the request body.
type resolutionScopes struct {
	tenant  *string
	service string // X-Workload-Id
	orgUnit string // X-Org-Unit-Id
	subject string // X-Principal-Id
}

// pickByPrecedence returns the highest-precedence value the snapshot holds
// for key in this context, and the layer it came from. A layer the key's
// declaration does not admit is skipped even if a value is stored there — the
// declaration may have narrowed since the value was written.
func pickByPrecedence(entries map[string]domain.ManifestEntry, def *domain.ConfigDefinition, key string, sc resolutionScopes) (domain.ManifestEntry, string, bool) {
	admits := func(layer string) bool { return def == nil || domain.ValidateScope(def, layer) == nil }
	for _, layer := range domain.PrecedenceHighestFirst {
		var mk string
		switch layer {
		case domain.LayerUserPreference:
			if sc.tenant == nil || sc.subject == "" {
				continue
			}
			mk = domain.ManifestLayerKey(key, sc.tenant, layer, sc.subject)
		case domain.LayerOrgUnit:
			if sc.tenant == nil || sc.orgUnit == "" {
				continue
			}
			mk = domain.ManifestLayerKey(key, sc.tenant, layer, sc.orgUnit)
		case domain.ScopeTenant:
			if sc.tenant == nil {
				continue
			}
			mk = domain.ManifestKey(key, sc.tenant)
		case domain.LayerService:
			if sc.service == "" {
				continue
			}
			mk = domain.ManifestLayerKey(key, nil, layer, sc.service)
		case domain.ScopeEnvironment:
			mk = domain.ManifestKey(key, nil)
		}
		if e, ok := entries[mk]; ok && e.Kind == domain.ManifestKindConfig && (layer == domain.ScopeEnvironment || admits(layer)) {
			return e, layer, true
		}
	}
	return domain.ManifestEntry{}, "", false
}

// applyLayeredPart applies (or, for Remove, ends) a change part aimed at an
// extended layer.
func applyLayeredPart(ctx context.Context, tx pgx.Tx, p domain.ChangePart, actor string) error {
	layer := domain.PartLayer(p)
	if p.Remove {
		return endLayerOverride(ctx, tx, p.Key, p.Scope.Environment, p.Scope.TenantID, layer, p.Scope.ScopeID)
	}
	_, err := upsertLayerOverride(ctx, tx, p.Key, p.Scope.Environment, p.Scope.TenantID, layer, p.Scope.ScopeID, p.NewValue, actor)
	return err
}

var errLayeredFlag = errors.New("feature flags resolve by environment and tenant; extended layers apply to configuration keys")
