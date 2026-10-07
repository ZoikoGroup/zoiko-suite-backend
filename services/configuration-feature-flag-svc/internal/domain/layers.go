package domain

// ── Precedence layers (INV-07) ───────────────────────────────────────────────

// PrecedenceHighestFirst is the schema-defined resolution order — the order
// AA-001 lists the layers, most specific last, applied most specific first.
// It is fixed here rather than taken from each key's allowed_scopes order, so
// two keys can never resolve the same scope in different orders.
var PrecedenceHighestFirst = []string{
	LayerUserPreference, LayerOrgUnit, ScopeTenant, LayerService, ScopeEnvironment,
}

// IsExtendedLayer reports whether layer is one of the three layers stored in
// config_layer_overrides (000013) rather than config_entries.
func IsExtendedLayer(layer string) bool {
	return layer == LayerService || layer == LayerOrgUnit || layer == LayerUserPreference
}

// ManifestLayerKey is the snapshot key of an extended-layer value:
// key|tenant|LAYER|scope, where tenant is "*" for the tenantless SERVICE
// layer. It never collides with ManifestKey's key|tenant form.
func ManifestLayerKey(key string, tenantID *string, layer, scopeID string) string {
	return ManifestKey(key, tenantID) + "|" + layer + "|" + scopeID
}

// PartLayer is the layer a change part writes: its explicit layer when it
// names one, otherwise TENANT or ENVIRONMENT by whether it names a tenant.
func PartLayer(p ChangePart) string {
	if p.Scope.Layer != "" {
		return p.Scope.Layer
	}
	if p.Scope.TenantID != nil {
		return ScopeTenant
	}
	return ScopeEnvironment
}

// PartManifestKey is the snapshot key of the scope a change part targets.
func PartManifestKey(p ChangePart) string {
	if IsExtendedLayer(PartLayer(p)) {
		return ManifestLayerKey(p.Key, p.Scope.TenantID, PartLayer(p), p.Scope.ScopeID)
	}
	return ManifestKey(p.Key, p.Scope.TenantID)
}

// ValidLayerTenancy enforces the fixed tenancy of each extended layer: SERVICE
// is tenantless (a per-service default across tenants), ORG_UNIT and
// USER_PREFERENCE live inside one tenant; all three need a scope id.
func ValidLayerTenancy(layer string, tenantID *string, scopeID string) error {
	if !IsExtendedLayer(layer) {
		return nil
	}
	if scopeID == "" {
		return ErrScopeNotAllowed
	}
	if (layer == LayerService) != (tenantID == nil) {
		return ErrScopeNotAllowed
	}
	return nil
}
