package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/connectivity-api-bridge-svc/internal/domain"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
)

// RegistryRepo is the Postgres implementation of registry.Repo (migration 004). It is a separate type from PgStore
// because the registry and the tenant's connector rows share method names with different meaning.
type RegistryRepo struct{ p *PgStore }

// NewRegistryRepo wraps a PgStore.
func NewRegistryRepo(p *PgStore) *RegistryRepo { return &RegistryRepo{p: p} }

var _ registry.Repo = (*RegistryRepo)(nil)

func (r *RegistryRepo) isPlatform(ctx context.Context) bool {
	return middleware.GetTenantID(ctx) == registry.PlatformScopeID
}

func (r *RegistryRepo) enqueue(ctx context.Context, tx pgx.Tx, e registry.Event) error {
	return r.p.enqueue(ctx, tx, r.p.tenant(ctx), domain.OutboxEvent{
		Type: e.Type, AggregateType: e.AggregateType, AggregateID: e.AggregateID, ActorID: e.ActorID,
		CorrelationID: e.CorrelationID, Data: e.Data,
	})
}

// registryErr maps the database's own refusals to the registry's errors.
func registryErr(err error) error {
	switch pgCode(err) {
	case "42501":
		return registry.ErrNotPlatformScope
	case "23505":
		return registry.ErrVersionExists
	}
	return err
}

// claimIdempotency records (scope,key) for this request. It returns the resource of an earlier identical request,
// or ErrIdempotency when the key was used for a different one.
func claimIdempotency(ctx context.Context, tx pgx.Tx, scope, key, hash, resourceID string) (existing string, err error) {
	if key == "" {
		return "", nil
	}
	tag, err := tx.Exec(ctx, `INSERT INTO xic_idempotency (scope, idem_key, request_hash, resource_id) VALUES ($1,$2,$3,$4)
		ON CONFLICT (scope, idem_key) DO NOTHING`, scope, key, hash, resourceID)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 1 {
		return "", nil
	}
	var h, res string
	if err := tx.QueryRow(ctx, `SELECT request_hash, resource_id FROM xic_idempotency WHERE scope=$1 AND idem_key=$2`, scope, key).Scan(&h, &res); err != nil {
		return "", err
	}
	if h != hash {
		return "", registry.ErrIdempotency
	}
	return res, nil
}

// ── connectors ───────────────────────────────────────────────────────────────

const xicConnectorCols = `connector_id, family, provider_class, owner, lifecycle, supported_until, version, created_by, created_at, updated_at`

func scanXICConnector(row pgx.Row, c *registry.Connector) error {
	return row.Scan(&c.ConnectorID, &c.Family, &c.ProviderClass, &c.Owner, &c.Lifecycle, &c.SupportedUntil, &c.Version,
		&c.CreatedBy, &c.CreatedAt, &c.UpdatedAt)
}

func (r *RegistryRepo) CreateConnector(ctx context.Context, c *registry.Connector, idemKey, idemHash string, e registry.Event) (bool, error) {
	if !r.isPlatform(ctx) {
		return false, registry.ErrNotPlatformScope
	}
	created := true
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		existing, err := claimIdempotency(ctx, tx, "connector", idemKey, idemHash, c.ConnectorID)
		if err != nil {
			return err
		}
		if existing != "" {
			created = false
			return scanXICConnector(tx.QueryRow(ctx, `SELECT `+xicConnectorCols+` FROM xic_connectors WHERE connector_id=$1`, existing), c)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO xic_connectors (`+xicConnectorCols+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			c.ConnectorID, c.Family, c.ProviderClass, c.Owner, c.Lifecycle, c.SupportedUntil, c.Version, c.CreatedBy, c.CreatedAt, c.UpdatedAt); err != nil {
			return err
		}
		return r.enqueue(ctx, tx, e)
	})
	if pgCode(err) == "23505" {
		return false, registry.ErrConnectorExists
	}
	if err != nil {
		return false, registryErr(err)
	}
	return created, nil
}

func (r *RegistryRepo) GetConnector(ctx context.Context, id string) (*registry.Connector, error) {
	var c registry.Connector
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		return scanXICConnector(tx.QueryRow(ctx, `SELECT `+xicConnectorCols+` FROM xic_connectors WHERE connector_id=$1`, id), &c)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, registry.ErrConnectorNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (r *RegistryRepo) ListConnectors(ctx context.Context, family, lifecycle string) ([]registry.Connector, error) {
	out := []registry.Connector{}
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+xicConnectorCols+` FROM xic_connectors
			WHERE ($1 = '' OR family = $1) AND ($2 = '' OR lifecycle = $2) ORDER BY connector_id`, family, lifecycle)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c registry.Connector
			if err := scanXICConnector(rows, &c); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

func (r *RegistryRepo) MutateConnector(ctx context.Context, id string, fn func(c *registry.Connector) (*registry.Event, error)) (*registry.Connector, error) {
	if !r.isPlatform(ctx) {
		return nil, registry.ErrNotPlatformScope
	}
	var c registry.Connector
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		if err := scanXICConnector(tx.QueryRow(ctx, `SELECT `+xicConnectorCols+` FROM xic_connectors WHERE connector_id=$1 FOR UPDATE`, id), &c); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return registry.ErrConnectorNotFound
			}
			return err
		}
		e, err := fn(&c)
		if err != nil {
			return err
		}
		c.Version++
		c.UpdatedAt = time.Now().UTC()
		if _, err := tx.Exec(ctx, `UPDATE xic_connectors SET provider_class=$2, owner=$3, lifecycle=$4, supported_until=$5, version=$6, updated_at=$7
			WHERE connector_id=$1`, c.ConnectorID, c.ProviderClass, c.Owner, c.Lifecycle, c.SupportedUntil, c.Version, c.UpdatedAt); err != nil {
			return err
		}
		if e != nil {
			return r.enqueue(ctx, tx, *e)
		}
		return nil
	})
	if err != nil {
		return nil, registryErr(err)
	}
	return &c, nil
}

// ── contract versions ────────────────────────────────────────────────────────

const xicVersionCols = `contract_version_id::text, connector_id, semver, status, effective_from, supported_until,
	migration_plan_ref, replay_analysis_ref, rollback_ref, content, content_hash, COALESCE(baseline_version_id::text, ''),
	compat_class, compat_report, blocked_reason, row_version, created_by, created_at, validated_at, published_by,
	published_at, updated_at`

func scanXICVersion(row pgx.Row, v *registry.ContractVersion) error {
	var content, report []byte
	if err := row.Scan(&v.ContractVersionID, &v.ConnectorID, &v.Semver, &v.Status, &v.EffectiveFrom, &v.SupportedUntil,
		&v.MigrationPlanRef, &v.ReplayAnalysisRef, &v.RollbackRef, &content, &v.ContentHash, &v.BaselineVersionID,
		&v.CompatClass, &report, &v.BlockedReason, &v.RowVersion, &v.CreatedBy, &v.CreatedAt, &v.ValidatedAt, &v.PublishedBy,
		&v.PublishedAt, &v.UpdatedAt); err != nil {
		return err
	}
	if err := json.Unmarshal(content, &v.Content); err != nil {
		return err
	}
	if len(report) > 0 && string(report) != "null" {
		v.CompatReport = &registry.CompatReport{}
		if err := json.Unmarshal(report, v.CompatReport); err != nil {
			return err
		}
	}
	return nil
}

func loadMappings(ctx context.Context, tx pgx.Tx, versionID string) ([]registry.MappingVersion, error) {
	rows, err := tx.Query(ctx, `SELECT mapping_version_id::text, contract_version_id::text, mapping_key, version, source_schema,
		canonical_contract_ref, spec, transform_hash, status FROM xic_mapping_versions WHERE contract_version_id=$1::uuid
		ORDER BY mapping_key, version`, versionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []registry.MappingVersion
	for rows.Next() {
		var m registry.MappingVersion
		var spec []byte
		if err := rows.Scan(&m.MappingVersionID, &m.ContractVersionID, &m.Key, &m.Version, &m.SourceSchema, &m.CanonicalContract,
			&spec, &m.TransformHash, &m.Status); err != nil {
			return nil, err
		}
		m.Spec = json.RawMessage(spec)
		out = append(out, m)
	}
	return out, rows.Err()
}

func insertMappings(ctx context.Context, tx pgx.Tx, ms []registry.MappingVersion) error {
	for _, m := range ms {
		if _, err := tx.Exec(ctx, `INSERT INTO xic_mapping_versions (mapping_version_id, contract_version_id, mapping_key, version,
			source_schema, canonical_contract_ref, spec, transform_hash, status) VALUES ($1::uuid,$2::uuid,$3,$4,$5,$6,$7,$8,$9)`,
			m.MappingVersionID, m.ContractVersionID, m.Key, m.Version, m.SourceSchema, m.CanonicalContract, []byte(m.Spec), m.TransformHash, m.Status); err != nil {
			return err
		}
	}
	return nil
}

func (r *RegistryRepo) CreateVersion(ctx context.Context, v *registry.ContractVersion, idemKey, idemHash string, e registry.Event) (bool, error) {
	if !r.isPlatform(ctx) {
		return false, registry.ErrNotPlatformScope
	}
	created := true
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		existing, err := claimIdempotency(ctx, tx, "version", idemKey, idemHash, v.ContractVersionID)
		if err != nil {
			return err
		}
		if existing != "" {
			created = false
			v.ContractVersionID = existing
			return nil
		}
		var one int
		if err := tx.QueryRow(ctx, `SELECT 1 FROM xic_connectors WHERE connector_id=$1 FOR SHARE`, v.ConnectorID).Scan(&one); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return registry.ErrConnectorNotFound
			}
			return err
		}
		content, err := json.Marshal(v.Content)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO xic_contract_versions (contract_version_id, connector_id, semver, status, effective_from,
			supported_until, migration_plan_ref, replay_analysis_ref, rollback_ref, content, row_version, created_by, created_at, updated_at)
			VALUES ($1::uuid,$2,$3,'DRAFT',$4,$5,$6,$7,$8,$9,1,$10,$11,$12)`,
			v.ContractVersionID, v.ConnectorID, v.Semver, v.EffectiveFrom, v.SupportedUntil, v.MigrationPlanRef, v.ReplayAnalysisRef,
			v.RollbackRef, content, v.CreatedBy, v.CreatedAt, v.UpdatedAt); err != nil {
			return err
		}
		if err := insertMappings(ctx, tx, v.MappingVersions); err != nil {
			return err
		}
		return r.enqueue(ctx, tx, e)
	})
	if err != nil {
		return false, registryErr(err)
	}
	return created, nil
}

func (r *RegistryRepo) GetVersion(ctx context.Context, id string) (*registry.ContractVersion, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, registry.ErrVersionNotFound
	}
	var v registry.ContractVersion
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		if err := scanXICVersion(tx.QueryRow(ctx, `SELECT `+xicVersionCols+` FROM xic_contract_versions WHERE contract_version_id=$1::uuid`, id), &v); err != nil {
			return err
		}
		ms, err := loadMappings(ctx, tx, id)
		v.MappingVersions = ms
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, registry.ErrVersionNotFound
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (r *RegistryRepo) ListVersions(ctx context.Context, connectorID string) ([]registry.ContractVersion, error) {
	out := []registry.ContractVersion{}
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+xicVersionCols+` FROM xic_contract_versions WHERE connector_id=$1
			ORDER BY semver_major, semver_minor, semver_patch`, connectorID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v registry.ContractVersion
			if err := scanXICVersion(rows, &v); err != nil {
				return err
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	return out, err
}

func (r *RegistryRepo) Baseline(ctx context.Context, connectorID string) (*registry.ContractVersion, error) {
	var v registry.ContractVersion
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		return scanXICVersion(tx.QueryRow(ctx, `SELECT `+xicVersionCols+` FROM xic_contract_versions WHERE connector_id=$1 AND status='PUBLISHED'`, connectorID), &v)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (r *RegistryRepo) ReplaceDraft(ctx context.Context, v *registry.ContractVersion, expectedVersion int64, e registry.Event) (*registry.ContractVersion, error) {
	if !r.isPlatform(ctx) {
		return nil, registry.ErrNotPlatformScope
	}
	var out registry.ContractVersion
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		var status string
		var rv int64
		if err := tx.QueryRow(ctx, `SELECT status, row_version FROM xic_contract_versions WHERE contract_version_id=$1::uuid FOR UPDATE`,
			v.ContractVersionID).Scan(&status, &rv); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return registry.ErrVersionNotFound
			}
			return err
		}
		if status != registry.VersionDraft {
			return registry.ErrNotDraft
		}
		if rv != expectedVersion {
			return registry.ErrVersionConflict
		}
		content, err := json.Marshal(v.Content)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM xic_mapping_versions WHERE contract_version_id=$1::uuid`, v.ContractVersionID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE xic_contract_versions SET semver=$2, effective_from=$3, supported_until=$4, migration_plan_ref=$5,
			replay_analysis_ref=$6, rollback_ref=$7, content=$8, row_version=row_version+1, updated_at=$9 WHERE contract_version_id=$1::uuid`,
			v.ContractVersionID, v.Semver, v.EffectiveFrom, v.SupportedUntil, v.MigrationPlanRef, v.ReplayAnalysisRef, v.RollbackRef,
			content, v.UpdatedAt); err != nil {
			return err
		}
		if err := insertMappings(ctx, tx, v.MappingVersions); err != nil {
			return err
		}
		if err := r.enqueue(ctx, tx, e); err != nil {
			return err
		}
		if err := scanXICVersion(tx.QueryRow(ctx, `SELECT `+xicVersionCols+` FROM xic_contract_versions WHERE contract_version_id=$1::uuid`, v.ContractVersionID), &out); err != nil {
			return err
		}
		out.MappingVersions, err = loadMappings(ctx, tx, v.ContractVersionID)
		return err
	})
	if err != nil {
		return nil, registryErr(err)
	}
	return &out, nil
}

func loadGates(ctx context.Context, tx pgx.Tx, id string) ([]registry.Gate, error) {
	rows, err := tx.Query(ctx, `SELECT gate_key, outcome, evidence_ref, detail, attested_by, at FROM xic_contract_gates
		WHERE contract_version_id=$1::uuid ORDER BY ord`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []registry.Gate{}
	for rows.Next() {
		var g registry.Gate
		var detail []byte
		if err := rows.Scan(&g.GateKey, &g.Outcome, &g.EvidenceRef, &detail, &g.AttestedBy, &g.At); err != nil {
			return nil, err
		}
		if len(detail) > 0 {
			g.Detail = json.RawMessage(detail)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (r *RegistryRepo) MutateVersion(ctx context.Context, id string, fn func(tx *registry.VersionTx) error) (*registry.ContractVersion, error) {
	if !r.isPlatform(ctx) {
		return nil, registry.ErrNotPlatformScope
	}
	if _, err := uuid.Parse(id); err != nil {
		return nil, registry.ErrVersionNotFound
	}
	var out registry.ContractVersion
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		// lock order: connector, then version, so publishers of one connector are serialized
		var connectorID string
		if err := tx.QueryRow(ctx, `SELECT connector_id FROM xic_contract_versions WHERE contract_version_id=$1::uuid`, id).Scan(&connectorID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return registry.ErrVersionNotFound
			}
			return err
		}
		var conn registry.Connector
		if err := scanXICConnector(tx.QueryRow(ctx, `SELECT `+xicConnectorCols+` FROM xic_connectors WHERE connector_id=$1 FOR UPDATE`, connectorID), &conn); err != nil {
			return err
		}
		var cur registry.ContractVersion
		if err := scanXICVersion(tx.QueryRow(ctx, `SELECT `+xicVersionCols+` FROM xic_contract_versions WHERE contract_version_id=$1::uuid FOR UPDATE`, id), &cur); err != nil {
			return err
		}
		ms, err := loadMappings(ctx, tx, id)
		if err != nil {
			return err
		}
		cur.MappingVersions = ms
		gates, err := loadGates(ctx, tx, id)
		if err != nil {
			return err
		}
		var base registry.ContractVersion
		var baseline *registry.ContractVersion
		switch err := scanXICVersion(tx.QueryRow(ctx, `SELECT `+xicVersionCols+` FROM xic_contract_versions WHERE connector_id=$1 AND status='PUBLISHED'`, connectorID), &base); {
		case err == nil:
			baseline = &base
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}

		work := cur
		vt := &registry.VersionTx{V: &work, Connector: &conn, Gates: gates, Baseline: baseline}
		if err := fn(vt); err != nil {
			return err
		}

		// evidence first, while the parent is still in the state it was attested in
		if vt.NewRun != nil {
			issues, _ := json.Marshal(nonNilIssues(vt.NewRun.Issues))
			var report []byte
			if vt.NewRun.Report != nil {
				report, _ = json.Marshal(vt.NewRun.Report)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO xic_validation_runs (run_id, contract_version_id, outcome, content_hash, issues, report, run_by, at)
				VALUES ($1::uuid,$2::uuid,$3,$4,$5,$6,$7,$8)`, vt.NewRun.RunID, id, vt.NewRun.Outcome, vt.NewRun.ContentHash, issues, report,
				vt.NewRun.RunBy, vt.NewRun.At); err != nil {
				return err
			}
		}
		if vt.NewGate != nil {
			if _, err := tx.Exec(ctx, `INSERT INTO xic_contract_gates (gate_id, contract_version_id, gate_key, outcome, evidence_ref, detail, attested_by, at)
				VALUES ($1::uuid,$2::uuid,$3,$4,$5,$6,$7,$8)`, uuid.NewString(), id, vt.NewGate.GateKey, vt.NewGate.Outcome, vt.NewGate.EvidenceRef,
				nullableJSON(vt.NewGate.Detail), vt.NewGate.AttestedBy, vt.NewGate.At); err != nil {
				return err
			}
		}
		if vt.Transition != nil {
			if _, err := tx.Exec(ctx, `INSERT INTO xic_contract_transitions (contract_version_id, from_status, to_status, actor, reason, at)
				VALUES ($1::uuid,$2,$3,$4,$5,$6)`, id, vt.Transition.From, vt.Transition.To, vt.Transition.Actor, vt.Transition.Reason, vt.Transition.At); err != nil {
				return err
			}
		}

		// a publish demotes the version it replaces first: only one version per connector may be PUBLISHED
		if vt.Supersede != "" {
			if _, err := tx.Exec(ctx, `UPDATE xic_contract_versions SET status='SUPERSEDED', row_version=row_version+1, updated_at=now()
				WHERE contract_version_id=$1::uuid AND status='PUBLISHED'`, vt.Supersede); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE xic_mapping_versions SET status='SUPERSEDED' WHERE contract_version_id=$1::uuid AND status='PUBLISHED'`, vt.Supersede); err != nil {
				return err
			}
		}
		if changed(&cur, &work) {
			var report []byte
			if work.CompatReport != nil {
				report, _ = json.Marshal(work.CompatReport)
			}
			var baselineID any
			if work.BaselineVersionID != "" {
				baselineID = work.BaselineVersionID
			}
			if _, err := tx.Exec(ctx, `UPDATE xic_contract_versions SET status=$2, content_hash=$3, baseline_version_id=$4::uuid, compat_class=$5,
				compat_report=$6, blocked_reason=$7, validated_at=$8, published_by=$9, published_at=$10, row_version=row_version+1, updated_at=now()
				WHERE contract_version_id=$1::uuid`, id, work.Status, work.ContentHash, baselineID, work.CompatClass, report, work.BlockedReason,
				work.ValidatedAt, work.PublishedBy, work.PublishedAt); err != nil {
				return err
			}
			if work.Status == registry.VersionPublished && cur.Status != registry.VersionPublished {
				if _, err := tx.Exec(ctx, `UPDATE xic_mapping_versions SET status='PUBLISHED' WHERE contract_version_id=$1::uuid AND status='DRAFT'`, id); err != nil {
					return err
				}
			}
		}
		if vt.ConnectorLifecycle != "" && vt.ConnectorLifecycle != conn.Lifecycle {
			if _, err := tx.Exec(ctx, `UPDATE xic_connectors SET lifecycle=$2, version=version+1, updated_at=now() WHERE connector_id=$1`,
				connectorID, vt.ConnectorLifecycle); err != nil {
				return err
			}
		}
		for _, e := range vt.Events {
			if err := r.enqueue(ctx, tx, e); err != nil {
				return err
			}
		}
		if err := scanXICVersion(tx.QueryRow(ctx, `SELECT `+xicVersionCols+` FROM xic_contract_versions WHERE contract_version_id=$1::uuid`, id), &out); err != nil {
			return err
		}
		out.MappingVersions, err = loadMappings(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, mapGuardErr(err)
	}
	return &out, nil
}

// mapGuardErr turns a refusal by one of the guard triggers into the registry error a caller can act on. The
// service checks the same rules first; this is the database holding them when something else got past it.
func mapGuardErr(err error) error {
	if registryErr(err) != err {
		return registryErr(err)
	}
	if pgCode(err) == "23000" {
		return registry.ErrGatesIncomplete
	}
	return err
}

func changed(a, b *registry.ContractVersion) bool {
	return a.Status != b.Status || a.ContentHash != b.ContentHash || a.CompatClass != b.CompatClass ||
		a.BaselineVersionID != b.BaselineVersionID || a.BlockedReason != b.BlockedReason ||
		!sameTime(a.ValidatedAt, b.ValidatedAt) || a.PublishedBy != b.PublishedBy || !sameTime(a.PublishedAt, b.PublishedAt)
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func nonNilIssues(is []registry.Issue) []registry.Issue {
	if is == nil {
		return []registry.Issue{}
	}
	return is
}

func nullableJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return []byte(raw)
}

// ── evidence reads ───────────────────────────────────────────────────────────

// visible checks the version exists for this tenant (RLS decides), so evidence of an invisible version is
// "not found" and never an empty list.
func visible(ctx context.Context, tx pgx.Tx, id string) error {
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM xic_contract_versions WHERE contract_version_id=$1::uuid`, id).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return registry.ErrVersionNotFound
	}
	return err
}

func (r *RegistryRepo) ListGates(ctx context.Context, id string) ([]registry.Gate, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, registry.ErrVersionNotFound
	}
	var out []registry.Gate
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		if err := visible(ctx, tx, id); err != nil {
			return err
		}
		var err error
		out, err = loadGates(ctx, tx, id)
		return err
	})
	return out, err
}

func (r *RegistryRepo) ListTransitions(ctx context.Context, id string) ([]registry.Transition, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, registry.ErrVersionNotFound
	}
	out := []registry.Transition{}
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		if err := visible(ctx, tx, id); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT from_status, to_status, actor, reason, at FROM xic_contract_transitions
			WHERE contract_version_id=$1::uuid ORDER BY ord`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t registry.Transition
			if err := rows.Scan(&t.From, &t.To, &t.Actor, &t.Reason, &t.At); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

func (r *RegistryRepo) ListValidationRuns(ctx context.Context, id string) ([]registry.ValidationRun, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, registry.ErrVersionNotFound
	}
	out := []registry.ValidationRun{}
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		if err := visible(ctx, tx, id); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT run_id::text, outcome, content_hash, issues, report, run_by, at FROM xic_validation_runs
			WHERE contract_version_id=$1::uuid ORDER BY ord`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var vr registry.ValidationRun
			var issues, report []byte
			if err := rows.Scan(&vr.RunID, &vr.Outcome, &vr.ContentHash, &issues, &report, &vr.RunBy, &vr.At); err != nil {
				return err
			}
			if err := json.Unmarshal(issues, &vr.Issues); err != nil {
				return err
			}
			if len(report) > 0 && string(report) != "null" {
				vr.Report = &registry.CompatReport{}
				if err := json.Unmarshal(report, vr.Report); err != nil {
					return err
				}
			}
			out = append(out, vr)
		}
		return rows.Err()
	})
	return out, err
}

func (r *RegistryRepo) GetMapping(ctx context.Context, id string) (*registry.MappingVersion, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, registry.ErrMappingNotFound
	}
	var m registry.MappingVersion
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		var spec []byte
		err := tx.QueryRow(ctx, `SELECT mapping_version_id::text, contract_version_id::text, mapping_key, version, source_schema,
			canonical_contract_ref, spec, transform_hash, status FROM xic_mapping_versions WHERE mapping_version_id=$1::uuid`, id).
			Scan(&m.MappingVersionID, &m.ContractVersionID, &m.Key, &m.Version, &m.SourceSchema, &m.CanonicalContract, &spec, &m.TransformHash, &m.Status)
		m.Spec = json.RawMessage(spec)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, registry.ErrMappingNotFound
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}
