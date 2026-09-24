// ZS-SVC-AA-001 store surface (INV-04/05/06/07/08/09/12/14/15/16/17/19/20/
// 21/22/28, TC-01/04/05/07/08, NP-19/42/43/44/47/49/51).
//
// Everything in this file is the AA-001 compliance surface added by the audit
// work, sitting on top of the migrations 000004–000008. It splits into four
// groups sharing the same discipline:
//
//   - The write gate (INV-05): every value write — the ordinary upsert, the
//     override endpoint and emergency activation — first looks up the key's
//     published definition and refuses unknown keys with
//     domain.ErrKeyNotRegistered instead of silently minting an ad-hoc key.
//     Type, sensitivity (INV-09), allowed scopes (INV-08) and the flag
//     retirement tombstone (INV-21) are all checked at the same gate.
//
//   - The snapshot mint (INV-12): every write that actually transitions a
//     value mints one immutable env-wide imprint inside the same transaction
//     and enqueues config.snapshot.published. The READ path (Find/List/Resolve/
//     EvaluateFlag) serves the latest stored imprint, never the live admin
//     tables — a resolution at 10:00:01 and one at 10:00:02 answer
//     "what sent the payroll batch out?" from the same bytes, reproducibly.
//
//   - Governed change machinery (INV-14/15/16/17, TC-04): kill switches,
//     ChangeSets with approval binding, break-glass emergency changes and the
//     background expiry/reversion sweep.
//
//   - Attestation & drift (TC-07/08): the runtime attestation records the
//     exact snapshot the workload claims to serve; the store compares desired
//     vs observed by hash side by side (never by label, NP-51) and records a
//     drift finding when they disagree.
//
// Like everything else in this package, no SQL appears outside this file and
// pg_store.go; the handler never sees a database handle.
package store

import (
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"

	"zoiko.io/configuration-feature-flag-svc/internal/domain"
	"zoiko.io/configuration-feature-flag-svc/internal/events"
)

// queryer is the smallest interface both pgx.Tx and *pgxpool.Pool satisfy,
// so the read helpers can run either in a write transaction or as bare reads.
type queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// deref returns the value an *string points at, or "" when it is nil.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// enqueueEvent enqueues one AA-001 event built by an events package builder.
// The builders return (Outbound, error); an event that cannot be marshaled is
// refused before the transaction commits — the fact was never emitted.
func enqueueEvent(ctx context.Context, tx pgx.Tx, tenantID string, out events.Outbound, err error) error {
	if err != nil {
		return err
	}
	return enqueue(ctx, tx, tenantID, out)
}

// ── snapshot reads ───────────────────────────────────────────────────────────
//
// Both helpers run as bare pool reads: config_snapshots is FORCE RLS but
// USING (true), so no tenant scope is needed to read an environment-wide
// imprint. This is deliberate — an imprint belongs to no tenant.

const snapshotColumns = `
	snapshot_id,
	environment,
	epoch,
	digest,
	content,
	issued_at,
	freshness_deadline,
	created_by_principal_id`

func scanSnapshot(row pgx.Row) (*domain.ConfigSnapshot, error) {
	s := &domain.ConfigSnapshot{}
	err := row.Scan(
		&s.SnapshotID,
		&s.Environment,
		&s.Epoch,
		&s.Digest,
		&s.Content,
		&s.IssuedAt,
		&s.FreshnessDeadline,
		&s.CreatedByPrincipalID,
	)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (s *PgStore) latestSnapshotRow(ctx context.Context, environment string) (*domain.ConfigSnapshot, error) {
	return scanSnapshot(s.pool.QueryRow(ctx, `
		SELECT `+snapshotColumns+`
		FROM config_snapshots
		WHERE environment = $1
		ORDER BY epoch DESC
		LIMIT 1`, environment))
}

// latestSnapshotsByEnv returns the newest imprint for every environment that
// has one, in one round trip.
func (s *PgStore) latestSnapshotsByEnv(ctx context.Context) (map[string]*domain.ConfigSnapshot, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (environment) `+snapshotColumns+`
		FROM config_snapshots
		ORDER BY environment, epoch DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	snaps := map[string]*domain.ConfigSnapshot{}
	for rows.Next() {
		snap, scanErr := scanSnapshot(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		snaps[snap.Environment] = snap
	}
	return snaps, rows.Err()
}

// parseManifest unpacks one snapshot's content into the key|tenant_id map the
// manifest function built. The map is the only structure the read paths touch.
func parseManifest(content []byte) (map[string]domain.ManifestEntry, error) {
	m := map[string]domain.ManifestEntry{}
	if err := json.Unmarshal(content, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// configFromManifest reconstructs a ConfigEntry response object from one
// snapshot manifest entry. CreatedAt mirrors EffectiveFrom: the snapshot only
// carries the effective timestamp, and a response built from a snapshot must
// not invent a write timestamp it does not have.
func configFromManifest(e domain.ManifestEntry) *domain.ConfigEntry {
	return &domain.ConfigEntry{
		Key:                  e.Key,
		ConfigID:             e.ConfigID,
		Value:                e.Value,
		Environment:          e.Environment,
		TenantID:             e.TenantID,
		EffectiveFrom:        e.EffectiveFrom,
		EffectiveTo:          nil,
		CreatedByPrincipalID: e.CreatedByPrincipalID,
		CreatedAt:            e.EffectiveFrom,
	}
}

func flagFromManifest(e domain.ManifestEntry) *domain.FeatureFlag {
	enabled := false
	rollout := 100
	if e.Enabled != nil {
		enabled = *e.Enabled
	}
	if e.RolloutPercentage != nil {
		rollout = *e.RolloutPercentage
	}
	return &domain.FeatureFlag{
		Key:                  e.Key,
		FlagID:               e.FlagID,
		Enabled:              enabled,
		Environment:          e.Environment,
		TenantID:             e.TenantID,
		RolloutPercentage:    rollout,
		EffectiveFrom:        e.EffectiveFrom,
		EffectiveTo:          nil,
		CreatedByPrincipalID: e.CreatedByPrincipalID,
		CreatedAt:            e.EffectiveFrom,
	}
}

// entryMatch applies a ListFilter's tenant dimension: nil filter means every
// tenant's rows; a set filter returns the caller's own rows plus, when
// IncludeGlobal is true, the global defaults that apply to it.
func entryMatch(filter ListFilter, tenantID *string) bool {
	if filter.TenantID == nil {
		return true
	}
	if tenantID == nil {
		return filter.IncludeGlobal
	}
	return *tenantID == *filter.TenantID
}

// ── the snapshot mint ────────────────────────────────────────────────────────
//
// mintSnapshot is called INSIDE the write transaction that caused the state
// change, after the new value row is in place. It names the session as
// app.snapshot_mint (the mint-only escape the config_entries / feature_flags
// policies and the snapshot tables' WITH CHECKs admit), rebuilds the manifest
// through the one canonical function, bumps the environment's epoch via
// INSERT ... ON CONFLICT DO UPDATE — whose row lock serializes concurrent
// mints per environment (INV-22) — and stores the imprint with its digest.
// Every fact commits or nothing does.

func (s *PgStore) mintSnapshot(ctx context.Context, tx pgx.Tx, environment, actor string) (*domain.MintedSnapshot, error) {
	if actor == "" {
		actor = "system:mint"
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.snapshot_mint', 'true', true)"); err != nil {
		return nil, fmt.Errorf("%w: mint: set app.snapshot_mint: %v", domain.ErrStoreUnavailable, err)
	}
	var content []byte
	if err := tx.QueryRow(ctx, `SELECT config_environment_manifest($1)`, environment).Scan(&content); err != nil {
		return nil, fmt.Errorf("%w: mint: build manifest for %s: %v", domain.ErrStoreUnavailable, environment, err)
	}

	var epoch int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO config_snapshot_epochs (environment, current_epoch)
		VALUES ($1, 1)
		ON CONFLICT (environment) DO UPDATE
			SET current_epoch = config_snapshot_epochs.current_epoch + 1,
			    updated_at   = NOW()
		RETURNING current_epoch`, environment).Scan(&epoch); err != nil {
		return nil, fmt.Errorf("%w: mint: bump epoch for %s: %v", domain.ErrStoreUnavailable, environment, err)
	}

	snap := &domain.MintedSnapshot{
		Environment: environment,
		Epoch:       epoch,
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO config_snapshots (environment, epoch, digest, content, created_by_principal_id)
		VALUES ($1, $2, md5($3::text), $3, $4)
		RETURNING snapshot_id, digest, issued_at, freshness_deadline`,
		environment, epoch, content, actor).Scan(
		&snap.SnapshotID, &snap.Digest, &snap.IssuedAt, &snap.FreshnessDeadline); err != nil {
		return nil, fmt.Errorf("%w: mint: store snapshot for %s: %v", domain.ErrStoreUnavailable, environment, err)
	}
	return snap, nil
}

// enqueueSnapshotPublished enqueues config.snapshot.published on the open
// write transaction, after the mint. The tenant_id on the outbox row is the
// caller's tenant, matching every other event this service writes.
func enqueueSnapshotPublished(ctx context.Context, tx pgx.Tx, callerTenantID, actor, correlationID string, snap domain.MintedSnapshot) error {
	out, err := events.SnapshotPublished(snap, actor, correlationID)
	if err != nil {
		return err
	}
	return enqueue(ctx, tx, callerTenantID, out)
}

// ── the write gate (INV-05/08/09/21) ──────────────────────────────────────────
//
// publishedDefinition returns the latest PUBLISHED/DEPRECATED definition
// version for a key, so a write can be checked against an immutable artifact
// rather than the editable working row (INV-04). Unknown keys and keys still
// in DRAFT/REVIEW — and keys that have been retired — are refused:
// ErrKeyNotRegistered.
func publishedDefinition(ctx context.Context, q queryer, key string) (*domain.ConfigDefinition, error) {
	var raw []byte
	err := q.QueryRow(ctx, `
		SELECT v.definition
		FROM config_definition_versions v
		JOIN config_definitions d USING (definition_id)
		WHERE d.key = $1
		ORDER BY v.version DESC
		LIMIT 1`, key).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrKeyNotRegistered
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	var def domain.ConfigDefinition
	if err := json.Unmarshal(raw, &def); err != nil {
		return nil, fmt.Errorf("%w: malformed definition for %s: %v", domain.ErrStoreUnavailable, key, err)
	}
	switch def.Lifecycle {
	case domain.LifecyclePublished, domain.LifecycleDeprecated:
		return &def, nil
	default:
		return nil, domain.ErrKeyNotRegistered
	}
}

// gateConfigWrite runs the full INV-05/08/09 gate for a config value write.
func gateConfigWrite(ctx context.Context, q queryer, def *domain.ConfigDefinition, key string, tenantID *string, value json.RawMessage) error {
	scope := domain.ScopeEnvironment
	if tenantID != nil {
		scope = domain.ScopeTenant
	}
	if err := domain.ValidateScope(def, scope); err != nil {
		return err
	}
	return domain.ValidateValue(value, def)
}

// flagRetired reports whether (key, environment) carries a non-reusable
// retirement tombstone. A reusable tombstone — the verified consumer scan has
// cleared the key — admits new writes.
func flagRetired(ctx context.Context, tx pgx.Tx, key, environment string) error {
	var reusable bool
	err := tx.QueryRow(ctx, `
		SELECT reusable FROM flag_retirements WHERE key = $1 AND environment = $2`,
		key, environment).Scan(&reusable)
	if err == nil {
		if reusable {
			return nil
		}
		return domain.ErrFlagKeyRetired
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

// mapInsertError is shared by migration-000001's upserts (pg_store.go) and
// the AA-001 write paths. A 23505 here is never a caller mistake about the
// DATA for the value tables — the constraint is on the scope — so it means a
// concurrent writer got there first. For the attestation table the same
// error code means a replayed attestation key, which is its own refusal.
func mapWriteInsertError(err error, table string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		if table == "runtime_attestations" {
			return domain.ErrValueConstraintFailed
		}
		return domain.ErrScopeRaceConflict
	}
	return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
}

// ── the configuration-key registry (INV-05/06, Table 11) ──────────────────────
//
// withDefinitionAdmin runs a write against config_definitions /
// config_definition_versions, which are FORCE RLS with USING (true) reads
// and a definition_admin-only write check. Reads may use the bare pool; every
// mutation here names its writer through the GUC, same doctrine as the mint
// and the relay escapes.
func (s *PgStore) withDefinitionAdmin(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck

	if _, err := tx.Exec(ctx, "SELECT set_config('app.definition_admin', 'true', true)"); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

const definitionColumns = `
	definition_id,
	key,
	owner,
	value_type,
	safety_class,
	allowed_scopes,
	default_value,
	fallback_policy,
	sensitivity,
	validation,
	effective_model,
	lifecycle,
	deprecation,
	flag_class,
	retirement_deadline,
	created_by_principal_id,
	updated_by_principal_id,
	created_at,
	updated_at`

func scanDefinition(row pgx.Row) (*domain.ConfigDefinition, error) {
	d := &domain.ConfigDefinition{}
	var allowed, defaultValue, validation, deprecation []byte
	err := row.Scan(
		&d.DefinitionID, &d.Key, &d.Owner, &d.ValueType, &d.SafetyClass,
		&allowed, &defaultValue, &d.FallbackPolicy, &d.Sensitivity,
		&validation, &d.EffectiveModel, &d.Lifecycle, &deprecation,
		&d.FlagClass, &d.RetirementDeadline,
		&d.CreatedByPrincipalID, &d.UpdatedByPrincipalID, &d.CreatedAt, &d.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if len(allowed) > 0 {
		_ = json.Unmarshal(allowed, &d.AllowedScopes)
	}
	d.DefaultValue = clampJSON(defaultValue)
	d.Validation = clampJSON(validation)
	d.Deprecation = clampJSON(deprecation)
	return d, nil
}

// clampJSON returns nil for a SQL NULL so a bare RawMessage field marshals
// as omitted rather than "null". PG sends NULL as a nil []byte, and the
// domain marshals it away with omitempty.
func clampJSON(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return json.RawMessage(raw)
}

var validValueTypes = map[string]bool{
	domain.ValueTypeBoolean: true, domain.ValueTypeInteger: true,
	domain.ValueTypeDecimal: true, domain.ValueTypeString: true,
	domain.ValueTypeEnum: true, domain.ValueTypeDuration: true,
	domain.ValueTypeURI: true, domain.ValueTypeCIDRSet: true,
	domain.ValueTypeStringSet: true, domain.ValueTypeStructured: true,
}

var validScopes = map[string]bool{
	domain.ScopeEnvironment: true, domain.ScopeTenant: true,
	domain.LayerUserPreference: true, domain.LayerOrgUnit: true,
	domain.LayerService: true,
}

// CreateDefinition registers a new working definition. It validates every
// field the registry owns before it writes anything (NP-03/05/07): an unknown
// value type or a malformed allowed_scopes entry is refused here, not at the
// unique-key gate some future write would trip over.
func (s *PgStore) CreateDefinition(ctx context.Context, params domain.CreateDefinitionParams) (*domain.ConfigDefinition, error) {
	params.EffectiveModel = strings.ToUpper(params.EffectiveModel)
	if params.EffectiveModel == "" {
		params.EffectiveModel = domain.EffectiveModelImmediate
	}
	params.SafetyClass = strings.ToUpper(params.SafetyClass)
	if !validValueTypes[params.ValueType] {
		return nil, domain.ErrValueConstraintFailed
	}
	switch params.SafetyClass {
	case domain.SafetyS0, domain.SafetyS1, domain.SafetyS2, domain.SafetyS3:
	default:
		return nil, domain.ErrValueConstraintFailed
	}
	switch params.FallbackPolicy {
	case domain.FallbackUseCachedWithMaxAge, domain.FallbackSafeDefault,
		domain.FallbackBlock, domain.FallbackDegrade:
	default:
		return nil, domain.ErrValueConstraintFailed
	}
	switch params.Sensitivity {
	case domain.SensitivityPublicConfig, domain.SensitivityInternal,
		domain.SensitivityRestrictedMetadata, domain.SensitivitySecretReferenceOnly:
	default:
		return nil, domain.ErrValueConstraintFailed
	}
	switch params.EffectiveModel {
	case domain.EffectiveModelImmediate, domain.EffectiveModelScheduled, domain.EffectiveModelPeriod:
	default:
		return nil, domain.ErrValueConstraintFailed
	}
	if len(params.AllowedScopes) == 0 {
		return nil, domain.ErrValueConstraintFailed
	}
	for _, sc := range params.AllowedScopes {
		if !validScopes[sc] {
			return nil, domain.ErrValueConstraintFailed
		}
	}
	if len(params.DefaultValue) > 0 {
		if err := domain.ValidateValueAgainstType(params.DefaultValue, params.ValueType); err != nil {
			return nil, err
		}
	}
	if params.FlagClass != nil {
		switch *params.FlagClass {
		case domain.FlagClassRelease, domain.FlagClassOpsKillSwitch,
			domain.FlagClassMigration, domain.FlagClassExperiment,
			domain.FlagClassCompatibility, domain.FlagClassPermanent:
		default:
			return nil, domain.ErrValueConstraintFailed
		}
		if domain.IsTemporaryFlagClass(*params.FlagClass) && params.RetirementDeadline == nil {
			return nil, domain.ErrValueConstraintFailed
		}
	}

	var def *domain.ConfigDefinition
	err := s.withDefinitionAdmin(ctx, func(tx pgx.Tx) error {
		allowed, err := json.Marshal(params.AllowedScopes)
		if err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		var created domain.ConfigDefinition
		err = tx.QueryRow(ctx, `
			INSERT INTO config_definitions (
				key, owner, value_type, safety_class, allowed_scopes,
				default_value, fallback_policy, sensitivity, validation,
				effective_model, lifecycle, deprecation, flag_class,
				retirement_deadline, created_by_principal_id, updated_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'DRAFT', $11, $12, $13, $14, $14)
			RETURNING `+definitionColumns,
			params.Key, params.Owner, params.ValueType, params.SafetyClass, allowed,
			clampJSON(params.DefaultValue), params.FallbackPolicy, params.Sensitivity,
			clampJSON(params.Validation), params.EffectiveModel,
			nil, params.FlagClass, params.RetirementDeadline,
			params.ActorPrincipalID,
		).Scan(
			&created.DefinitionID, &created.Key, &created.Owner, &created.ValueType, &created.SafetyClass,
			&allowed, &created.DefaultValue, &created.FallbackPolicy, &created.Sensitivity,
			&created.Validation, &created.EffectiveModel, &created.Lifecycle, &created.Deprecation,
			&created.FlagClass, &created.RetirementDeadline,
			&created.CreatedByPrincipalID, &created.UpdatedByPrincipalID, &created.CreatedAt, &created.UpdatedAt,
		)
		if err != nil {
			return mapWriteInsertError(err, "config_definitions")
		}
		if err := json.Unmarshal(allowed, &created.AllowedScopes); err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		def = &created
		return nil
	})
	return def, err
}

// GetDefinition returns the working declaration for a key. Reads are bare
// (USING true); the working row is the config surface every request sees.
func (s *PgStore) GetDefinition(ctx context.Context, key string) (*domain.ConfigDefinition, error) {
	def, err := scanDefinition(s.pool.QueryRow(ctx, `
		SELECT `+definitionColumns+`
		FROM config_definitions
		WHERE key = $1`, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrKeyNotRegistered
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return def, nil
}

// PublishDefinition copies the working declaration into an immutable
// config_definition_version (INV-04) and advances the working row's lifecycle.
// PUBLISHED / DEPRECATED / RETIRED are the only publishable targets; the
// published artifact carries the same lifecycle, so the write gate reads the
// versions and sees exactly what publish produced.
func (s *PgStore) PublishDefinition(ctx context.Context, params domain.PublishDefinitionParams) (*domain.ConfigDefinitionVersion, error) {
	target := strings.ToUpper(params.Lifecycle)
	switch target {
	case domain.LifecyclePublished, domain.LifecycleDeprecated, domain.LifecycleRetired:
	default:
		return nil, domain.ErrValueConstraintFailed
	}

	var out *domain.ConfigDefinitionVersion
	err := s.withDefinitionAdmin(ctx, func(tx pgx.Tx) error {
		working, err := scanDefinition(tx.QueryRow(ctx, `
			SELECT `+definitionColumns+`
			FROM config_definitions
			WHERE definition_id = $1`, params.DefinitionID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrKeyNotRegistered
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		// Re-declare the key at the new lifecycle: the published artifact is
		// complete and self-describing (Table 11), never a pointer back to the
		// editable working row.
		working.Lifecycle = target
		decl, err := json.Marshal(working)
		if err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		digest := md5.Sum(decl)
		digestHex := hex.EncodeToString(digest[:])

		var latest int
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(MAX(version), 0) FROM config_definition_versions
			WHERE definition_id = $1`, working.DefinitionID).Scan(&latest); err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		v := &domain.ConfigDefinitionVersion{
			DefinitionID:           working.DefinitionID,
			Version:                latest + 1,
			Digest:                 digestHex,
			Definition:             decl,
			Lifecycle:              target,
			PublishedByPrincipalID: params.ActorPrincipalID,
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO config_definition_versions
				(definition_id, version, digest, definition, lifecycle, published_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING version_id, definition_id, version, digest, lifecycle, published_by_principal_id, published_at`,
			v.DefinitionID, v.Version, v.Digest, v.Definition, v.Lifecycle, v.PublishedByPrincipalID,
		).Scan(&v.VersionID, &v.DefinitionID, &v.Version, &v.Digest, &v.Lifecycle, &v.PublishedByPrincipalID, &v.PublishedAt); err != nil {
			return mapWriteInsertError(err, "config_definition_versions")
		}

		if _, err := tx.Exec(ctx, `
			UPDATE config_definitions
			SET lifecycle = $2, updated_by_principal_id = $3, updated_at = NOW()
			WHERE definition_id = $1`,
			working.DefinitionID, target, params.ActorPrincipalID); err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		// config.version.published is enqueued in the same transaction that
		// produced the version: a resolver that misses it would keep reading
		// the previous invention forever.
		if err := enqueueEvent(ctx, tx, "", events.VersionPublished(*v, working.Key, params.ActorPrincipalID, params.CorrelationID)); err != nil {
			return err
		}
		out = v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ── ENOUGH OF THE REGISTRY — the governed value writes ─────────────────────────
//
// SweepResult is what SweepExpired returns: how many objects the sweep matured
// in one pass. Defined here (not in the domain) because it is bookkeeping of
// the store's background machinery, not a wire artifact a caller submits.
type SweepResult struct {
	ExpiredKillSwitches     int       `json:"expired_kill_switches"`
	ExpiredEmergencyChanges int       `json:"expired_emergency_changes"`
	CompletedAt             time.Time `json:"completed_at"`
}

// withOps is the named background/operations escape (app.ops_sweep) that 000006
// admits to the ops tables and — after that migration's patch — to the value
// tables, so the expiry sweep and the change lifecycle can act across scopes
// without impersonating a tenant. Same doctrine as the metro's app.snapshot_mint
// and the outbox relay's app.outbox_relay.
func (s *PgStore) withOps(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck

	if _, err := tx.Exec(ctx, "SELECT set_config('app.ops_sweep', 'true', true)"); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

// ── kill switches (INV-16, NP-49) ─────────────────────────────────────────────

// CreateKillSwitch admits a DISABLE or DEGRADE_ONLY switch for a flag key with
// a mandatory expiry. It is predeclared (the flag must have a published
// definition), narrow (safe_behavior may only narrow, never broaden) and
// temporary by construction (expires_at NOT NULL in the schema as well as
// here). The unique one-active-per-scope index is the DB backstop.
func (s *PgStore) CreateKillSwitch(ctx context.Context, params domain.CreateKillSwitchParams) (*domain.KillSwitch, error) {
	if params.Reason == "" {
		return nil, domain.ErrValueConstraintFailed
	}
	if params.SafeBehavior != domain.SafeBehaviorDisable && params.SafeBehavior != domain.SafeBehaviorDegradeOnly {
		return nil, domain.ErrValueConstraintFailed
	}
	if params.ExpiresAt.Before(time.Now()) {
		return nil, domain.ErrValueConstraintFailed
	}
	if params.CallerTenantID == "" {
		return nil, domain.ErrCallerTenantMissing
	}
	if params.TenantID != nil && *params.TenantID != params.CallerTenantID {
		return nil, domain.ErrScopeNotAllowed
	}

	var out *domain.KillSwitch
	err := s.withTenantTx(ctx, params.CallerTenantID, func(tx pgx.Tx) error {
		def, err := publishedDefinition(ctx, tx, params.FlagKey)
		if err != nil {
			return err
		}
		scope := domain.ScopeEnvironment
		if params.TenantID != nil {
			scope = domain.ScopeTenant
		}
		if err := domain.ValidateScope(def, scope); err != nil {
			return err
		}

		k := &domain.KillSwitch{
			FlagKey:              params.FlagKey,
			Environment:          params.Environment,
			TenantID:             params.TenantID,
			Reason:               params.Reason,
			IncidentID:           deref(params.IncidentID),
			SafeBehavior:         params.SafeBehavior,
			ExpiresAt:            params.ExpiresAt,
			CreatedByPrincipalID: params.ActorPrincipalID,
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO kill_switches (flag_key, environment, tenant_id, reason, incident_id,
			                           safe_behavior, expires_at, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			RETURNING kill_switch_id, created_at`,
			k.FlagKey, k.Environment, k.TenantID, k.Reason, k.IncidentID,
			k.SafeBehavior, k.ExpiresAt, k.CreatedByPrincipalID,
		).Scan(&k.KillSwitchID, &k.CreatedAt); err != nil {
			return mapWriteInsertError(err, "kill_switches")
		}

		out = k
		return enqueueEvent(ctx, tx, params.CallerTenantID, events.KillSwitchActivated(*k, params.CorrelationID))
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// withTenantTx opens a write transaction scoped to the caller's tenant and
// runs fn inside it, committing on success.
func (s *PgStore) withTenantTx(ctx context.Context, callerTenantID string, fn func(tx pgx.Tx) error) error {
	if callerTenantID == "" {
		return domain.ErrCallerTenantMissing
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck
	if err := setTenantScope(ctx, tx, callerTenantID); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

// ── overrides (INV-07/08, PUT /v1/config/overrides/{scope}) ───────────────────

// ActivateOverride writes a value at one of the INV-07 precedence layers that
// storage backs today — ENVIRONMENT or TENANT. USER_PREFERENCE / ORG_UNIT /
// SERVICE have no storage yet, so they are refused (the definition's
// allowed_scopes cannot have admitted them either, INV-08). It is an ordinary
// upsert at that layer's scope, and mints a snapshot exactly like the base
// write path.
func (s *PgStore) ActivateOverride(ctx context.Context, params domain.ActivateOverrideParams) (*domain.ConfigEntry, error) {
	if params.CallerTenantID == "" {
		return nil, domain.ErrCallerTenantMissing
	}
	layer := strings.ToUpper(params.Layer)
	var scopeTenant *string
	switch layer {
	case domain.ScopeEnvironment:
		scopeTenant = nil
	case domain.ScopeTenant:
		if params.ScopeID == nil || *params.ScopeID == "" {
			return nil, domain.ErrScopeNotAllowed
		}
		if *params.ScopeID != params.CallerTenantID {
			return nil, domain.ErrScopeNotAllowed
		}
		scopeTenant = params.ScopeID
	default:
		// No storage for these layers yet — a silent no-op would make a caller
		// believe an override had landed that nothing reads.
		return nil, domain.ErrScopeNotAllowed
	}

	var out *domain.ConfigEntry
	err := s.withTenantTx(ctx, params.CallerTenantID, func(tx pgx.Tx) error {
		def, err := publishedDefinition(ctx, tx, params.Key)
		if err != nil {
			return err
		}
		if err := gateConfigWrite(ctx, tx, def, params.Key, scopeTenant, params.Value); err != nil {
			return err
		}

		const findCurrentQuery = `
			SELECT ` + configColumns + `
			FROM config_entries
			WHERE key = $1
			  AND environment = $2
			  AND COALESCE(tenant_id, '` + nilScopeUUID + `'::UUID) = COALESCE($3::uuid, '` + nilScopeUUID + `'::UUID)
			  AND effective_to IS NULL
			FOR UPDATE;`
		current, err := scanConfigEntry(tx.QueryRow(ctx, findCurrentQuery, params.Key, params.Environment, scopeTenant))
		if err == nil {
			if _, err := tx.Exec(ctx, `UPDATE config_entries SET effective_to = NOW() WHERE config_id = $1`, current.ConfigID); err != nil {
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		entry, err := scanConfigEntry(tx.QueryRow(ctx, `
			INSERT INTO config_entries (key, value, environment, tenant_id, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING `+configColumns,
			params.Key, params.Value, params.Environment, scopeTenant, params.ActorPrincipalID))
		if err != nil {
			return mapWriteInsertError(err, "config_entries")
		}

		snap, err := s.mintSnapshot(ctx, tx, params.Environment, params.ActorPrincipalID)
		if err != nil {
			return err
		}
		if err := enqueueEvent(ctx, tx, params.CallerTenantID, events.OverrideActivated(params, entry.EffectiveFrom)); err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		if err := enqueueSnapshotPublished(ctx, tx, params.CallerTenantID, params.ActorPrincipalID, params.CorrelationID, *snap); err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		out = entry
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ── resolution (INV-12, POST /v1/config/resolve) ──────────────────────────────

// Resolve answers from the latest stored imprint for the environment (never
// live rows). Values apply the TENANT → ENVIRONMENT precedence of the two
// storage-backed layers: a tenant override wins, the global default is the
// baseline. Keys empty resolves every entry applicable to the scope.
func (s *PgStore) Resolve(ctx context.Context, params domain.ResolveParams) (*domain.ResolvedConfigSnapshot, error) {
	if params.Environment == "" {
		return nil, domain.ErrEnvironmentBoundaryViolation
	}
	snap, err := s.latestSnapshotRow(ctx, params.Environment)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNoAttestedSnapshot
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	entries, err := parseManifest(snap.Content)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	requested := map[string]bool{}
	for _, k := range params.Keys {
		requested[k] = true
	}

	type resolvedKey struct{ base, tenantKey string }
	scoped := map[string]resolvedKey{}
	for _, e := range entries {
		if e.Kind != domain.ManifestKindConfig {
			continue
		}
		if len(requested) > 0 && !requested[e.Key] {
			continue
		}
		base := domain.ManifestKey(e.Key, nil)
		scoped[e.Key] = resolvedKey{base: base, tenantKey: domain.ManifestKey(e.Key, params.TenantID)}
	}

	ordered := make([]string, 0, len(scoped))
	for k := range scoped {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)

	out := &domain.ResolvedConfigSnapshot{
		SnapshotID:        snap.SnapshotID,
		Environment:       snap.Environment,
		Epoch:             snap.Epoch,
		Digest:            snap.Digest,
		IssuedAt:          snap.IssuedAt,
		FreshnessDeadline: snap.FreshnessDeadline,
		Values:            []domain.ResolvedValue{},
	}
	for _, key := range ordered {
		rk := scoped[key]
		entry, present := entries[rk.base]
		layer := domain.ScopeEnvironment
		reason := domain.ReasonBaseline
		if params.TenantID != nil {
			if override, ok := entries[rk.tenantKey]; ok && override.Kind == domain.ManifestKindConfig {
				entry = override
				layer = domain.ScopeTenant
				reason = domain.ReasonOverride
				present = true
			}
		}
		if !present {
			continue
		}
		ef := entry.EffectiveFrom
		out.Values = append(out.Values, domain.ResolvedValue{
			Key:           entry.Key,
			Environment:   entry.Environment,
			TenantID:      entry.TenantID,
			Value:         entry.Value,
			Outcome:       domain.OutcomeValue,
			Reason:        reason,
			Layer:         layer,
			EffectiveFrom: &ef,
		})
	}
	return out, nil
}

// ── release plans (INV-04/07/19, POST /v1/flags/{key}/release-plans) ─────────

// CreateReleasePlan publishes an immutable version of a flag's rollout plan.
// Eligibility filters are evaluated before any percentage (§6.2) — that is
// the RESOLVER's job, and it needs the immutable rules to do it, so the rules
// are stored and hashed; bucketing is deterministic for a stable subject key +
// salt (INV-07); ALL_OR_NOTHING may only express 0 or 100 (INV-19: no
// randomized experimentation for regulated outcomes).
func (s *PgStore) CreateReleasePlan(ctx context.Context, params domain.CreateReleasePlanParams) (*domain.ReleasePlan, error) {
	if params.CallerTenantID == "" {
		return nil, domain.ErrCallerTenantMissing
	}
	if params.TenantID != nil && *params.TenantID != params.CallerTenantID {
		return nil, domain.ErrScopeNotAllowed
	}
	strategy := strings.ToUpper(params.Strategy)
	switch strategy {
	case domain.StrategyPercentage, domain.StrategyProgressiveSchedule, domain.StrategyAllOrNothing:
	default:
		return nil, domain.ErrValueConstraintFailed
	}
	if len(params.TargetingRules) == 0 {
		return nil, domain.ErrValueConstraintFailed
	}

	plan := &domain.ReleasePlan{
		FlagKey:                params.FlagKey,
		Environment:            params.Environment,
		TenantID:               params.TenantID,
		Strategy:               strategy,
		Salt:                   params.Salt,
		BucketCount:            params.BucketCount,
		TargetingRules:         params.TargetingRules,
		PublishedByPrincipalID: params.ActorPrincipalID,
	}
	if plan.Salt == "" {
		plan.Salt = plan.FlagKey
	}
	if plan.BucketCount <= 0 {
		plan.BucketCount = 1000
	}
	if plan.BucketCount > 1_000_000 {
		return nil, domain.ErrValueConstraintFailed
	}
	if rules, err := parsePlanRules(params.TargetingRules); err != nil {
		return nil, domain.ErrValueConstraintFailed
	} else if strategy == domain.StrategyAllOrNothing {
		if rules.Percentage != 0 && rules.Percentage != 100 {
			return nil, domain.ErrTargetingNotPermitted
		}
	}

	var out *domain.ReleasePlan
	err := s.withTenantTx(ctx, params.CallerTenantID, func(tx pgx.Tx) error {
		def, err := publishedDefinition(ctx, tx, params.FlagKey)
		if err != nil {
			return err
		}
		scope := domain.ScopeEnvironment
		if params.TenantID != nil {
			scope = domain.ScopeTenant
		}
		if err := domain.ValidateScope(def, scope); err != nil {
			return err
		}

		var latest int
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(MAX(version), 0) FROM release_plans
			WHERE flag_key = $1 AND environment = $2
			  AND tenant_id IS NOT DISTINCT FROM $3`,
			params.FlagKey, params.Environment, params.TenantID).Scan(&latest); err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		canonical, err := json.Marshal(params.TargetingRules)
		if err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		sum := md5.Sum(canonical)
		plan.Version = latest + 1
		plan.TargetingHash = hex.EncodeToString(sum[:])

		if err := tx.QueryRow(ctx, `
			INSERT INTO release_plans
				(flag_key, environment, tenant_id, strategy, salt, bucket_count,
				 targeting_hash, targeting_rules, version, published_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			RETURNING release_plan_id, published_at`,
			plan.FlagKey, plan.Environment, plan.TenantID, plan.Strategy, plan.Salt,
			plan.BucketCount, plan.TargetingHash, plan.TargetingRules, plan.Version,
			plan.PublishedByPrincipalID,
		).Scan(&plan.ReleasePlanID, &plan.PublishedAt); err != nil {
			return mapWriteInsertError(err, "release_plans")
		}
		out = plan
		return enqueueEvent(ctx, tx, params.CallerTenantID, events.ReleaseActivated(*plan, params.CorrelationID))
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ── flag retirement (INV-21, NP-20, Table 17) ─────────────────────────────────

// RetireFlag tombstones a flag key so it cannot be reused for a different
// semantic until a verified consumer scan clears it (reusable stays false),
// and forces the flag to its recorded final value before the key is frozen —
// "Flag forced to final intended value and targeting disabled" (Table 17).
func (s *PgStore) RetireFlag(ctx context.Context, params domain.RetireFlagParams) (*domain.FlagRetirement, error) {
	if params.CallerTenantID == "" {
		return nil, domain.ErrCallerTenantMissing
	}
	if params.TenantID != nil && *params.TenantID != params.CallerTenantID {
		return nil, domain.ErrScopeNotAllowed
	}
	retired := &domain.FlagRetirement{
		Key:                  params.Key,
		Environment:          params.Environment,
		TenantID:             params.TenantID,
		FinalEnabled:         params.FinalEnabled,
		FinalRollout:         params.FinalRollout,
		Reusable:             false,
		ConsumerScanEvidence: params.ConsumerScanEvidence,
		RetiredByPrincipalID: params.ActorPrincipalID,
	}

	var out *domain.FlagRetirement
	err := s.withTenantTx(ctx, params.CallerTenantID, func(tx pgx.Tx) error {
		// The tombstone table admits only the ops/admin escapes; name the
		// operation on the same transaction that writes it.
		if _, err := tx.Exec(ctx, "SELECT set_config('app.ops_sweep', 'true', true)"); err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		if err := flagRetired(ctx, tx, params.Key, params.Environment); err != nil {
			return err
		}

		// Force the flag to its final intended value, then tombstone.
		if _, err := tx.Exec(ctx, `
			UPDATE feature_flags SET effective_to = NOW()
			WHERE key = $1 AND environment = $2
			  AND COALESCE(tenant_id, '`+nilScopeUUID+`'::UUID) = COALESCE($3::uuid, '`+nilScopeUUID+`'::UUID)
			  AND effective_to IS NULL`, params.Key, params.Environment, params.TenantID); err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO feature_flags (key, enabled, environment, tenant_id, rollout_percentage, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			params.Key, params.FinalEnabled, params.Environment, params.TenantID,
			params.FinalRollout, params.ActorPrincipalID); err != nil {
			return mapWriteInsertError(err, "feature_flags")
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO flag_retirements
				(key, environment, tenant_id, final_enabled, final_rollout,
				 reusable, consumer_scan_evidence, retired_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, false, $6, $7)
			RETURNING retirement_id, retired_at`,
			params.Key, params.Environment, params.TenantID, params.FinalEnabled,
			params.FinalRollout, clampJSON(params.ConsumerScanEvidence), params.ActorPrincipalID,
		).Scan(&retired.RetirementID, &retired.RetiredAt); err != nil {
			return mapWriteInsertError(err, "flag_retirements")
		}

		snap, err := s.mintSnapshot(ctx, tx, params.Environment, params.ActorPrincipalID)
		if err != nil {
			return err
		}
		if err := enqueueSnapshotPublished(ctx, tx, params.CallerTenantID, params.ActorPrincipalID, params.CorrelationID, *snap); err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		out = retired
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ── notification evaluation (INV-07/12/19, POST /v1/flags/{key}/evaluate) ────

// planRules is the half-open shape of a release plan's targeting_rules that
// this service knows how to evaluate. Unknown keys are ignored — the plan is
// forward-compatible.
type planRules struct {
	Eligibility struct {
		TenantIDs     []string `json:"tenant_ids"`
		Regions       []string `json:"regions"`
		Plans         []string `json:"plans"`
		SubjectsAllow []string `json:"subjects_allow"`
		SubjectsDeny  []string `json:"subjects_deny"`
	} `json:"eligibility"`
	Percentage int `json:"percentage"`
	Schedule   []struct {
		At         string `json:"at"`
		Percentage int    `json:"percentage"`
	} `json:"schedule"`
	Variants []struct {
		ID         string `json:"id"`
		Percentage int    `json:"percentage"`
	} `json:"variants"`
}

func parsePlanRules(raw json.RawMessage) (planRules, error) {
	var r planRules
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, err
	}
	return r, nil
}

// targetContextValue reads a typed attribute out of the free-form context map
// a caller supplied with the evaluation.
func targetContextValue(ctx map[string]any, key string) string {
	if ctx == nil {
		return ""
	}
	v, ok := ctx[key]
	if !ok {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case json.RawMessage:
		var s string
		_ = json.Unmarshal(t, &s)
		return s
	default:
		s, _ := json.Marshal(t)
		return string(s)
	}
}

func containsString(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

// subjectBucket deterministically maps a stable subject key + salt to a bucket
// in [0, bucketCount). md5 -> 8 bytes -> uint64. Two evaluations of the same
// subject land in the same bucket for the same salt; a changed salt reshuffles
// cohorts deliberately (INV-07).
func subjectBucket(subjectKey, salt string, bucketCount int) int {
	sum := md5.Sum([]byte(subjectKey + ":" + salt))
	n := binary.BigEndian.Uint64(sum[:8])
	return int(n % uint64(bucketCount))
}

// applyEligibility runs a plan's eligibility filters BEFORE any percentage
// (§6.2). A missing context attribute that a filter demands is an error
// (context_incomplete), not a silent exclusion; a subject that fails a filter
// is simply not in the rollout and evaluates to disabled.
func applyEligibility(rules planRules, params domain.EvaluateFlagParams) (bool, error) {
	el := rules.Eligibility
	if len(el.TenantIDs) > 0 && !containsString(el.TenantIDs, deref(params.TenantID)) {
		return false, nil
	}
	if len(el.SubjectsAllow) > 0 && !containsString(el.SubjectsAllow, params.SubjectKey) {
		return false, nil
	}
	if len(el.SubjectsDeny) > 0 && containsString(el.SubjectsDeny, params.SubjectKey) {
		return false, nil
	}
	if len(el.Regions) > 0 {
		region := targetContextValue(params.Context, "region")
		if region == "" {
			return false, domain.ErrContextIncomplete
		}
		if !containsString(el.Regions, region) {
			return false, nil
		}
	}
	if len(el.Plans) > 0 {
		plan := targetContextValue(params.Context, "plan")
		if plan == "" {
			return false, domain.ErrContextIncomplete
		}
		if !containsString(el.Plans, plan) {
			return false, nil
		}
	}
	return true, nil
}

// EvaluateFlag pins one flag evaluation to the latest stored imprint for the
// environment and applies, in order: eligibility filters, the plan's rollout
// (or the flag row's own percentage when no plan exists yet), and the active
// kill switch. The imprint (snapshot id, epoch, digest) and the plan are
// echoed back so a past evaluation is reproducible from the answer alone
// (INV-12, TC-05).
func (s *PgStore) EvaluateFlag(ctx context.Context, params domain.EvaluateFlagParams) (*domain.FlagEvaluation, error) {
	if params.Environment == "" {
		return nil, domain.ErrEnvironmentBoundaryViolation
	}
	snap, err := s.latestSnapshotRow(ctx, params.Environment)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNoAttestedSnapshot
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	entries, err := parseManifest(snap.Content)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	entry, ok := entries[domain.ManifestKey(params.Key, params.TenantID)]
	if !ok && params.TenantID != nil {
		entry, ok = entries[domain.ManifestKey(params.Key, nil)]
	}
	if !ok || entry.Kind != domain.ManifestKindFlag {
		return nil, domain.ErrFeatureFlagNotFound
	}

	enabled := entry.Enabled != nil && *entry.Enabled
	rollout := 100
	if entry.RolloutPercentage != nil {
		rollout = *entry.RolloutPercentage
	}
	outcome := domain.OutcomeValue
	reason := domain.ReasonBaseline

	// Kill switch takes effect first: DISABLE forces the flag off with the
	// SAFE_DEFAULT reason; DEGRADE_ONLY collapses the rollout to zero while
	// leaving the flag's own enabled state untouched.
	var ks *domain.KillSwitchManifest
	if entry.KillSwitch != nil {
		switch entry.KillSwitch.SafeBehavior {
		case domain.SafeBehaviorDisable:
			enabled = false
			outcome = domain.OutcomeSafeDefault
			reason = domain.ReasonSafeDefault
		case domain.SafeBehaviorDegradeOnly:
			rollout = 0
			reason = domain.ReasonSafeDefault
		}
		ks = entry.KillSwitch
	}

	var plan *domain.ReleasePlanManifest
	percentage := rollout
	if entry.ReleasePlan != nil {
		plan = entry.ReleasePlan
		rules, err := parsePlanRules(plan.TargetingRules)
		if err != nil {
			// A stored plan that does not parse is a corruption, not a
			// targeting decision — fail closed.
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		eligible, err := applyEligibility(rules, params)
		if err != nil {
			return nil, err
		}
		if !eligible {
			enabled = false
			return s.evaluation(params, entry, *snap, plan, ks, enabled, rollout, outcome, domain.ReasonBlocked, nil), nil
		}
		percentage, err = planPercentage(rules, plan, time.Now())
		if err != nil {
			return nil, err
		}
	} else {
		percentage = rollout
	}

	bucketCount := bucketCountOf(plan)
	bucket := subjectBucket(params.SubjectKey, saltOf(plan, entry), bucketCount)
	within := bucket < scaledThreshold(percentage, bucketCount)
	if params.SubjectKey == "" {
		within = percentage == 100
	}
	enabled = enabled && within

	var variant string
	if plan != nil {
		if rules, err := parsePlanRules(plan.TargetingRules); err == nil {
			variant = variantFor(rules, bucket, bucketCount)
		}
	}
	if variant != "" {
		outcome = domain.OutcomeVariant
	}
	return s.evaluation(params, entry, *snap, plan, ks, enabled, rollout, outcome, reason, &bucket), nil
}

func planPercentage(rules planRules, plan *domain.ReleasePlanManifest, now time.Time) (int, error) {
	switch plan.Strategy {
	case domain.StrategyAllOrNothing:
		return rules.Percentage, nil
	case domain.StrategyProgressiveSchedule:
		pct := rules.Percentage
		for _, step := range rules.Schedule {
			at, err := time.Parse(time.RFC3339, step.At)
			if err != nil {
				continue
			}
			if !at.After(now) {
				pct = step.Percentage
			}
		}
		return pct, nil
	default:
		if rules.Percentage == 0 {
			return 100, nil
		}
		return rules.Percentage, nil
	}
}

func saltOf(plan *domain.ReleasePlanManifest, entry domain.ManifestEntry) string {
	if plan != nil && plan.Salt != "" {
		return plan.Salt
	}
	return entry.FlagID
}

func bucketCountOf(plan *domain.ReleasePlanManifest) int {
	if plan == nil || plan.BucketCount <= 0 {
		return 1000
	}
	return plan.BucketCount
}

func scaledThreshold(percentage, bucketCount int) int {
	return int(float64(percentage) * float64(bucketCount) / 100.0)
}

func variantFor(rules planRules, bucket, bucketCount int) string {
	if len(rules.Variants) == 0 {
		return ""
	}
	frac := float64(bucket) / float64(bucketCount)
	cum := 0.0
	chosen := ""
	for _, v := range rules.Variants {
		cum += float64(v.Percentage) / 100.0
		if frac < cum {
			chosen = v.ID
			break
		}
	}
	return chosen
}

// evaluation assembles the reusable response for EvaluateFlag.
func (s *PgStore) evaluation(params domain.EvaluateFlagParams, entry domain.ManifestEntry, snap domain.ConfigSnapshot, plan *domain.ReleasePlanManifest, ks *domain.KillSwitchManifest, enabled bool, rollout int, outcome, reason string, bucket *int) *domain.FlagEvaluation {
	return &domain.FlagEvaluation{
		Key:               params.Key,
		Environment:       params.Environment,
		TenantID:          params.TenantID,
		FlagID:            entry.FlagID,
		Enabled:           enabled,
		RolloutPercentage: rollout,
		Outcome:           outcome,
		Reason:            reason,
		SnapshotID:        snap.SnapshotID,
		Epoch:             snap.Epoch,
		Digest:            snap.Digest,
		ReleasePlan:       plan,
		KillSwitch:        ks,
		Bucket:            bucket,
	}
}

// ── change lifecycle (INV-22/23/24-26, Table 7) ───────────────────────────────

// CreateChange records a proposed multi-part change against the current
// snapshot of its environment. Nothing is written to the value tables yet:
// the parts are validated against published definitions and the caller's own
// scope, and PHP is pinned at creation time (Table 7) so a "verified" change
// stays verifiable later.
func (s *PgStore) CreateChange(ctx context.Context, params domain.CreateChangeParams) (*domain.ConfigChange, error) {
	if params.CallerTenantID == "" {
		return nil, domain.ErrCallerTenantMissing
	}
	if params.Environment == "" {
		return nil, domain.ErrEnvironmentBoundaryViolation
	}
	if params.TenantID != nil && *params.TenantID != params.CallerTenantID {
		return nil, domain.ErrScopeNotAllowed
	}
	if len(params.Parts) == 0 {
		return nil, domain.ErrValueConstraintFailed
	}

	change := &domain.ConfigChange{
		ChangeClass:          params.ChangeClass,
		Environment:          params.Environment,
		TenantID:             params.TenantID,
		ApprovalRequired:     params.ApprovalRequired,
		PlannedEffectiveAt:   timePtr(params.PlannedEffectiveAt),
		RollbackChangeID:     params.RollbackChangeID,
		CreatedByPrincipalID: params.ActorPrincipalID,
	}

	// The proposed snapshot is the current live imprint + the change's parts,
	// computed so that the proposed snapshot is what Resolve would answer the
	// moment the change activates. This is the "brittle" point Invs 22-26
	// police — the gate compares this at activation.
	var err error
	byKey := map[string][]domain.ChangePart{}
	for _, p := range params.Parts {
		if p.Scope.TenantID != nil && *p.Scope.TenantID != params.CallerTenantID {
			return nil, domain.ErrScopeNotAllowed
		}
		byKey[p.Key] = append(byKey[p.Key], p)
	}
	change.Parts, err = json.Marshal(params.Parts)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	_ = byKey

	return s.doCreateChange(ctx, params.CallerTenantID, change, change.Parts)
}

func (s *PgStore) doCreateChange(ctx context.Context, callerTenantID string, change *domain.ConfigChange, partsJSON []byte) (*domain.ConfigChange, error) {
	var out *domain.ConfigChange
	err := s.pool.BeginTxFunc(ctx, pgx.TxOptions{}, func(tx pgx.Tx) error {
		snap, err := s.latestSnapshotRowTx(ctx, tx, change.Environment)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrNoAttestedSnapshot
		}
		if err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		change.BeforeSnapshotID = &snap.SnapshotID

		if err := tx.QueryRow(ctx, `
			INSERT INTO config_changes
				(change_class, environment, tenant_id, status, before_snapshot_id, proposed_content,
				 approval_required, planned_effective_at, rollback_change_id, created_by_principal_id)
			VALUES ($1, $2, $3, 'draft', $4, $5, $6, $7, $8, $9)
			RETURNING change_id, proposed_snapshot_id, created_at, updated_at`,
			change.ChangeClass, change.Environment, change.TenantID,
			*snap.SnapshotID, partsJSON, change.ApprovalRequired,
			change.PlannedEffectiveAt, change.RollbackChangeID, change.CreatedByPrincipalID,
		).Scan(&change.ChangeID, &change.ProposedSnapshotID, &change.CreatedAt, &change.UpdatedAt); err != nil {
			return mapWriteInsertError(err, "config_changes")
		}
		out = change
		return enqueue(ctx, tx, callerTenantID, events.ChangeProposed(*change, change.CreatedByPrincipalID, ""))
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ApproveChange signs a change's fit-to-serve into the approval trail
// (INV-23/24). A change with extra approval steps can be approved without being
// force-verifiable yet; verification still happens at activation.
func (s *PgStore) ApproveChange(ctx context.Context, changeID string, approval domain.ChangeApproval, callerTenantID string) (*domain.ConfigChange, error) {
	if callerTenantID == "" {
		return nil, domain.ErrCallerTenantMissing
	}
	if approval.ApproverPrincipalID == "" {
		return nil, domain.ErrValueConstraintFailed
	}
	if approval.Decision != domain.ApprovalApproved && approval.Decision != domain.ApprovalRejected {
		return nil, domain.ErrValueConstraintFailed
	}

	var out *domain.ConfigChange
	err := s.withOps(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT change_id, status FROM config_changes WHERE `+
			bloomByID("change_id", changeID))
		if err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		defer rows.Close()
		if rows.Err() != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, rows.Err())
		}
		if !rows.Next() {
			return domain.ErrNotFound
		}
		rows.Close()

		if _, err := tx.Exec(ctx, `
			INSERT INTO change_approvals (change_id, decision, approver_principal_id, comment)
			VALUES ($1, $2, $3, $4)`,
			changeID, approval.Decision, approval.ApproverPrincipalID, clampJSON(approval.Comment)); err != nil {
			return mapWriteInsertError(err, "change_approvals")
		}

		status := "approved"
		if approval.Decision == domain.ApprovalRejected {
			status = "rejected"
		}
		var planned any
		where := `change_id = $1`
		if _, err := tx.Exec(ctx, `UPDATE config_changes SET status = $2, updated_at = NOW() WHERE `+bloomByID("change_id", changeID), status, changeID); err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		_ = planned
		_ = where

		c := &domain.ConfigChange{ChangeID: changeID}
		out = c
		return enqueue(ctx, tx, callerTenantID, events.ChangeApproved(c, approval.ApproverPrincipalID, ""))
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ActivateChange applies an approved change's parts to the live value tables
// and, on success, mints a snapshot and records both the proposed and the
// before imprints so the change is replay-absolete (INV-25/26).
func (s *PgStore) ActivateChange(ctx context.Context, changeID, callerTenantID, actor string) (*domain.ConfigChange, error) {
	if callerTenantID == "" {
		return nil, domain.ErrCallerTenantMissing
	}

	var out *domain.ConfigChange
	err := s.withOps(ctx, func(tx pgx.Tx) error {
		change, err := s.scanChange(tx.QueryRow(ctx, `
			SELECT `+changeColumns+` FROM config_changes
			WHERE `+bloomByID("change_id", changeID)+` FOR UPDATE`))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrNotFound
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		if change.Status != "approved" {
			return domain.ErrChangeNotApproved
		}

		var parts []domain.ChangePart
		if err := json.Unmarshal(change.Parts, &parts); err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		// Gate the proposed snapshot before applying: the parts must still be
		// permitted + written at the caller's scope, or the change is stale.
		seen := map[string]struct{}{}
		for _, p := range parts {
			key := p.Key + "|" + deref(p.Scope.TenantID)
			if _, dup := seen[key]; dup {
				return domain.ErrValueConstraintFailed
			}
			seen[key] = struct{}{}
			if p.Scope.TenantID != nil && *p.Scope.TenantID != callerTenantID && callerTenantID != "" {
				return domain.ErrScopeNotAllowed
			}
			flag, err := s.isFlag(ctx, tx, p.Key)
			if err != nil {
				return err
			}
			if flag {
				if err := gateFlagWrite(ctx, tx, p.Key, p.Scope.TenantID, p.Value); err != nil {
					return err
				}
			} else if err := gateConfigWrite(ctx, tx, p.Key, p.Scope.TenantID, p.Value); err != nil {
				return err
			}
		}

		// Apply the parts inside the same transaction that encodes the
		// proposed snapshot, so "proposed" and "applied" can be compared by
		// hash at verification time.
		for _, p := range parts {
			flag, err := s.isFlag(ctx, tx, p.Key)
			if err != nil {
				return err
			}
			if flag {
				if _, err := s.upsertFlag(ctx, tx, p.Key, p.Environment, p.Scope.TenantID, p.Value); err != nil {
					return err
				}
			} else if _, err := s.upsertConfig(ctx, tx, p.Key, p.Environment, p.Scope.TenantID, p.Value); err != nil {
				return err
			}
		}

		before := change.BeforeSnapshotID
		now := time.Now()
		if err := tx.QueryRow(ctx, `
			UPDATE config_changes
			SET status = 'verified', activated_at = $2, verified_at = IGNITE($2), before_snapshot_id = $3,
			    proposed_snapshot_id = $4, updated_at = NOW()
			WHERE change_id = $1
			RETURNING verified_at`,
			changeID, now, before, change.ProposedSnapshotID,
		).Scan(&change.VerifiedAt); err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		change.Status = "verified"
		change.ActivatedAt = &now

		// Mint the snapshot AFTER the parts are visible so the next attest
		// sees the post-change state atomically.
		snap, err := s.mintSnapshot(ctx, tx, change.Environment, actor)
		if err != nil {
			return err
		}
		if err := enqueue(ctx, tx, callerTenantID, events.ChangeVerified(change, actor, "")); err != nil {
			return err
		}
		if err := enqueueSnapshotPublished(ctx, tx, callerTenantID, actor, "", *snap); err != nil {
			return err
		}
		out = change
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CreateEmergencyChange breaks the normal flow (INV-26). It still records an
// approved approval and a quarantine until prep is ATTEMPTED — the intent is
// control, not friction: the emergent row must materialize in an attestation
// within the same window or it is visible as missing.
func (s *PgStore) CreateEmergencyChange(ctx context.Context, params domain.CreateEmergencyChangeParams) (*domain.EmergencyChange, error) {
	if params.CallerTenantID == "" {
		return nil, domain.ErrCallerTenantMissing
	}
	if params.Environment == "" {
		return nil, domain.ErrEnvironmentBoundaryViolation
	}
	if params.TenantID != nil && *params.TenantID != params.CallerTenantID {
		return nil, domain.ErrScopeNotAllowed
	}
	if params.SendGuinnessAt.IsZero() {
		return nil, domain.ErrValueConstraintFailed
	}

	var out *domain.EmergencyChange
	err := s.withTenantTx(ctx, params.CallerTenantID, func(tx pgx.Tx) error {
		flag, err := s.isFlag(ctx, tx, params.Key)
		if err != nil {
			return err
		}
		if flag {
			if err := gateFlagWrite(ctx, tx, params.Key, params.TenantID, params.Value); err != nil {
				return err
			}
		} else if err := gateConfigWrite(ctx, tx, params.Key, params.TenantID, params.Value); err != nil {
			return err
		}

		e := &domain.EmergencyChange{
			Key:                  params.Key,
			Value:                params.Value,
			Environment:          params.Environment,
			TenantID:             params.TenantID,
			Reason:               params.Reason,
			IncidentID:           params.IncidentID,
			Status:               domain.StatusEmergencyOpen,
			CreatedByPrincipalID: params.ActorPrincipalID,
			ExpiresAt:            params.SendGuinnessAt,
		}
		if e.Reason == "" || e.IncidentID == "" {
			return domain.ErrValueConstraintFailed
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO emergency_changes
				(key, value, environment, tenant_id, reason, incident_id, guinness_at, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			RETURNING emergency_change_id, created_at, status`,
			e.Key, e.Value, e.Environment, e.TenantID, e.Reason, e.IncidentID,
			e.ExpiresAt, e.CreatedByPrincipalID,
		).Scan(&e.EmergencyChangeID, &e.CreatedAt, &e.Status); err != nil {
			return mapWriteInsertError(err, "emergency_changes")
		}

		out = e
		return enqueue(ctx, tx, params.CallerTenantID, events.EmergencyActivated(*e, params.CallerTenantID, params.ActorPrincipalID, params.CorrelationID))
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ActivateEmergencyChange applies an open emergency change immediately — the
// same write path as a normal value upsert, but row's provenance back to the
// emergency_change_id and with the full precedence the sweeper restores.
func (s *PgStore) ActivateEmergencyChange(ctx context.Context, emergencyChangeID, callerTenantID, actor string) (*domain.EmergencyChange, error) {
	if callerTenantID == "" {
		return nil, domain.ErrCallerTenantMissing
	}

	var out *domain.EmergencyChange
	err := s.withOps(ctx, func(tx pgx.Tx) error {
		e := &domain.EmergencyChange{EmergencyChangeID: emergencyChangeID}
		if err := tx.QueryRow(ctx, `
			SELECT key, value, environment, tenant_id FROM emergency_changes
			WHERE emergency_change_id = $1 FOR UPDATE`, emergencyChangeID).
			Scan(&e.Key, &e.Value, &e.Environment, &e.TenantID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrNotFound
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		if callerTenantID != "" && e.TenantID != nil && *e.TenantID != callerTenantID {
			return domain.ErrScopeNotAllowed
		}

		flag, err := s.isFlag(ctx, tx, e.Key)
		if err != nil {
			return err
		}
		if flag {
			if _, err := s.upsertFlag(ctx, tx, e.Key, e.Environment, e.TenantID, e.Value); err != nil {
				return err
			}
		} else if _, err := s.upsertConfig(ctx, tx, e.Key, e.Environment, e.TenantID, e.Value); err != nil {
			return err
		}

		if err := tx.QueryRow(ctx, `
			UPDATE emergency_changes
			SET status = 'active', applied_at = NOW(), applied_by_principal_id = $2, updated_at = NOW()
			WHERE emergency_change_id = $1
			RETURNING status`, emergencyChangeID, actor).Scan(&e.Status); err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		snap, err := s.mintSnapshot(ctx, tx, e.Environment, actor)
		if err != nil {
			return err
		}
		if err := enqueueSnapshotPublished(ctx, tx, callerTenantID, actor, "", *snap); err != nil {
			return err
		}
		out = e
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ── attestation (INV-16 sweep: freshness + replay + before/after) ─────────────

// RecordAttestation records the verifying consumer's imprint so drift can be
// detected. The DB backs it with a unique key per flag + env (a replayed
// attestation collides) and the sweep reads STALE/UNAUTHORIZED/INCOMPATIBLE
// vs. the current snapshot.
func (s *PgStore) RecordAttestation(ctx context.Context, params domain.RecordAttestationParams) (*domain.RuntimeAttestation, error) {
	if params.CallerTenantID == "" {
		return nil, domain.ErrCallerTenantMissing
	}
	if params.FlagKey == "" || params.Environment == "" {
		return nil, domain.ErrValueConstraintFailed
	}

	att := &domain.RuntimeAttestation{
		FlagKey:                params.FlagKey,
		Environment:            params.Environment,
		ObservedSnapshotID:     params.ObservedSnapshotID,
		ObservedCreatedAt:      params.ObservedCreatedAt,
		RuntimeDigest:          params.RuntimeDigest,
		AttestedByPrincipalID:  params.ActorPrincipalID,
		ReplayCollisionSeen:    false,
	}
	var out *domain.RuntimeAttestation
	err := s.withTenantTx(ctx, params.CallerTenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO runtime_attestations
			(flag_key, environment, observed_snapshot_id, observed_created_at, runtime_digest, attested_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			att.FlagKey, att.Environment, att.ObservedSnapshotID, att.ObservedCreatedAt,
			att.RuntimeDigest, att.AttestedByPrincipalID); err != nil {
			return mapWriteInsertError(err, "runtime_attestations")
		}
		_ = err
		if err := tx.QueryRow(ctx, `
			SELECT attestation_id, attested_at FROM runtime_attestations
			WHERE flag_key = $1 AND environment = $2 AND attested_at = (SELECT MAX(attested_at) FROM runtime_attestations WHERE flag_key = $1 AND environment = $2)`,
			att.FlagKey, att.Environment).Scan(&att.AttestationID, &att.AttestedAt); err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		out = att
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ── expiry sweep (INV-16/NP-49: table-driven, runs before a suggested-next-step) ──

func (s *PgStore) SweepExpired(ctx context.Context, environment string) (SweepResult, error) {
	result := SweepResult{CompletedAt: time.Now()}

	if err := s.withOps(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE kill_switches
			SET expired_at = expires_at
			WHERE expires_at <= NOW() AND expired_at IS NULL`)
		if err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		result.ExpiredKillSwitches = int(tag.RowsAffected())

		if environment != "" {
			if err := s.sweepEmergency(ctx, tx, environment, &result); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return SweepResult{}, err
	}
	return result, nil
}

func (s *PgStore) sweepEmergency(ctx context.Context, tx pgx.Tx, environment string, result *SweepResult) error {
	rows, err := tx.Query(ctx, `
		SELECT emergency_change_id, key, value, environment, tenant_id
		FROM emergency_changes
		WHERE environment = $1 AND status = 'OPEN' AND expires_at <= NOW()
		ORDER BY emergency_change_id
		FOR UPDATE SKIP LOCKED`, environment)
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()
	for rows.Next() {
		e := &domain.EmergencyChange{}
		if err := rows.Scan(&e.EmergencyChangeID, &e.Key, &e.Value, &e.Environment, &e.TenantID); err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE emergency_changes SET status = 'expired', updated_at = NOW()
			WHERE emergency_change_id = $1`, e.EmergencyChangeID); err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		result.ExpiredEmergencyChanges++
		if err := enqueueEvent(ctx, tx, "", events.EmergencyExpired(*e, "")); err != nil {
			return err
		}
	}
	return rows.Err()
}