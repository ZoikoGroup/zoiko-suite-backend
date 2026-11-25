// Package store is the PgStore persistence layer for data-lineage-svc
// (DATA-03, ZS-SVC-N-001 §4). Every method runs inside one transaction
// that first declares app.tenant_id for RLS, then performs the write —
// tenant scoping is enforced by the database, never an application-level
// WHERE clause the caller could get wrong.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/data-lineage-svc/internal/domain"
	"zoiko.io/data-lineage-svc/internal/outbox"
)

// Store is the DATA-03 persistence contract.
type Store interface {
	RecordDerivation(ctx context.Context, tenantID string, req domain.RecordDerivationRequest, actor string, claim domain.IdempotencyClaim) (*domain.LineageEdge, error)
	SealManifest(ctx context.Context, tenantID, entityID, actor string, claim domain.IdempotencyClaim) (*domain.ProvenanceManifest, error)
	SupersedeLineage(ctx context.Context, tenantID string, req domain.SupersedeLineageRequest, actor string, claim domain.IdempotencyClaim) (*domain.LineageEdge, error)
	AttachEvidence(ctx context.Context, tenantID string, req domain.AttachEvidenceRequest, actor string, claim domain.IdempotencyClaim) (*domain.EvidenceAttachment, error)

	GetUpstreamLineage(ctx context.Context, tenantID, entityID string) (*domain.LineageGraphSnapshot, error)
	GetDownstreamImpact(ctx context.Context, tenantID, entityID string) (*domain.LineageGraphSnapshot, error)
	GetAsOfLineage(ctx context.Context, tenantID, entityID string, asOf time.Time) (*domain.LineageGraphSnapshot, error)
	GetSourceToReportPath(ctx context.Context, tenantID, entityID string) ([]domain.LineageEntity, error)
	GetEntity(ctx context.Context, tenantID, entityID string) (*domain.LineageEntity, error)
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

var _ Store = (*PgStore)(nil)

func (s *PgStore) withTenant(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	if tenantID == "" {
		return errors.New("tenant_id is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return mapErr(err)
	}
	return tx.Commit(ctx)
}

func mapErr(err error) error {
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) {
		return err
	}
	if strings.Contains(pgErr.Message, "immutable") {
		return fmt.Errorf("%w: %s", domain.ErrLineageEdgeAlreadySuperseded, pgErr.Message)
	}
	return err
}

func claimIdempotency(ctx context.Context, tx pgx.Tx, c domain.IdempotencyClaim) error {
	tag, err := tx.Exec(ctx, `
		INSERT INTO idempotency_keys (tenant_id, owner_scope, principal_id, idempotency_key, operation, request_sha256, resource_id)
		VALUES (current_setting('app.tenant_id', true), $1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, owner_scope, principal_id, idempotency_key) DO NOTHING`,
		c.OwnerScope, c.PrincipalID, c.Key, c.Operation, c.RequestSHA256, c.ResourceID)
	if err != nil {
		return fmt.Errorf("claim idempotency key: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var reqHash, resourceID string
	if err := tx.QueryRow(ctx, `
		SELECT request_sha256, resource_id FROM idempotency_keys
		WHERE tenant_id = current_setting('app.tenant_id', true) AND owner_scope = $1 AND principal_id = $2 AND idempotency_key = $3`,
		c.OwnerScope, c.PrincipalID, c.Key).Scan(&reqHash, &resourceID); err != nil {
		return fmt.Errorf("read idempotency claim: %w", err)
	}
	if reqHash != c.RequestSHA256 {
		return domain.ErrIdempotencyKeyReused
	}
	return &domain.IdempotentReplayError{ResourceID: resourceID}
}

func newID(prefix string) string {
	return prefix + uuid.NewString()
}

// ── Entities / transformation versions (find-or-create) ─────────────────────

func findOrCreateEntity(ctx context.Context, tx pgx.Tx, tenantID string, ref domain.EntityRef) (*domain.LineageEntity, error) {
	id := newID(domain.PrefixLineageEntity)
	got, err := scanEntity(tx.QueryRow(ctx, `
		INSERT INTO lineage_entities (entity_id, tenant_id, entity_type, external_ref)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, entity_type, external_ref) DO NOTHING
		RETURNING `+entityColumns,
		id, tenantID, ref.EntityType, ref.ExternalRef))
	if errors.Is(err, pgx.ErrNoRows) {
		return scanEntity(tx.QueryRow(ctx, `SELECT `+entityColumns+`
			FROM lineage_entities WHERE tenant_id = $1 AND entity_type = $2 AND external_ref = $3`,
			tenantID, ref.EntityType, ref.ExternalRef))
	}
	return got, err
}

const entityColumns = `entity_id, tenant_id, entity_type, external_ref, created_at`

func scanEntity(row pgx.Row) (*domain.LineageEntity, error) {
	var e domain.LineageEntity
	if err := row.Scan(&e.EntityID, &e.TenantID, &e.EntityType, &e.ExternalRef, &e.CreatedAt); err != nil {
		return nil, err
	}
	return &e, nil
}

func (s *PgStore) GetEntity(ctx context.Context, tenantID, entityID string) (*domain.LineageEntity, error) {
	var out *domain.LineageEntity
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		e, err := scanEntity(tx.QueryRow(ctx, `SELECT `+entityColumns+` FROM lineage_entities WHERE entity_id = $1`, entityID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrLineageEntityNotFound
		}
		out = e
		return err
	})
	return out, err
}

// findOrCreateTransformationVersion registers name the first time it is
// used by any RecordDerivation call — an append-only registry, never
// mutated once a name exists.
func findOrCreateTransformationVersion(ctx context.Context, tx pgx.Tx, tenantID, name, actor string) (*domain.TransformationVersion, error) {
	id := newID(domain.PrefixTransformationVersion)
	got, err := scanVersion(tx.QueryRow(ctx, `
		INSERT INTO transformation_versions (version_id, tenant_id, name, registered_by)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, name) DO NOTHING
		RETURNING version_id, tenant_id, name, registered_at, registered_by`,
		id, tenantID, name, actor))
	if errors.Is(err, pgx.ErrNoRows) {
		return scanVersion(tx.QueryRow(ctx, `SELECT version_id, tenant_id, name, registered_at, registered_by
			FROM transformation_versions WHERE tenant_id = $1 AND name = $2`, tenantID, name))
	}
	return got, err
}

func scanVersion(row pgx.Row) (*domain.TransformationVersion, error) {
	var v domain.TransformationVersion
	if err := row.Scan(&v.VersionID, &v.TenantID, &v.Name, &v.RegisteredAt, &v.RegisteredBy); err != nil {
		return nil, err
	}
	return &v, nil
}

// ── RecordDerivation ─────────────────────────────────────────────────────────

const edgeColumns = `edge_id, tenant_id, source_entity_id, activity_id, derived_entity_id, supersedes_edge_id, reason, created_at, created_by, source_version, derived_version`

func scanEdge(row pgx.Row) (*domain.LineageEdge, error) {
	var e domain.LineageEdge
	if err := row.Scan(&e.EdgeID, &e.TenantID, &e.SourceEntityID, &e.ActivityID, &e.DerivedEntityID,
		&e.SupersedesEdgeID, &e.Reason, &e.CreatedAt, &e.CreatedBy, &e.SourceVersion, &e.DerivedVersion); err != nil {
		return nil, err
	}
	return &e, nil
}

const activityColumns = `activity_id, tenant_id, activity_type, transformation_version, agent, occurred_at, run_id, build_version, config_digest, environment, region`

func scanActivity(row pgx.Row) (*domain.LineageActivity, error) {
	var a domain.LineageActivity
	if err := row.Scan(&a.ActivityID, &a.TenantID, &a.ActivityType, &a.TransformationVersion, &a.Agent, &a.OccurredAt,
		&a.RunID, &a.BuildVersion, &a.ConfigDigest, &a.Environment, &a.Region); err != nil {
		return nil, err
	}
	return &a, nil
}

// RecordDerivation finds-or-creates the source/derived entities, then
// checks for an existing CURRENT edge matching this exact derivation
// (same source, derived, activity_type, transformation_version) — if one
// already exists, it is returned as-is: the doc's own requirement that
// "the same derivation recorded twice must not create two edges." This
// content-based dedup is separate from (and checked before) the
// idempotency-key claim, which only catches a literal retry of the same
// HTTP request.
func (s *PgStore) RecordDerivation(ctx context.Context, tenantID string, req domain.RecordDerivationRequest, actor string, claim domain.IdempotencyClaim) (*domain.LineageEdge, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.LineageEdge
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		source, err := findOrCreateEntity(ctx, tx, tenantID, req.SourceEntity)
		if err != nil {
			return fmt.Errorf("source entity: %w", err)
		}
		derived, err := findOrCreateEntity(ctx, tx, tenantID, req.DerivedEntity)
		if err != nil {
			return fmt.Errorf("derived entity: %w", err)
		}

		existing, err := findCurrentMatchingEdge(ctx, tx, tenantID, source.EntityID, derived.EntityID, req.ActivityType, req.TransformationVersion, req.ProvenanceFields)
		if err != nil {
			return err
		}
		if existing != nil {
			// Same run, same pins: a retry. But one run_id cannot have run two
			// builds or two configurations — that is two different facts
			// claiming one identity, and silently returning the first would
			// hide the second (NP-33/NP-34).
			if req.RunID != nil {
				prev, err := scanActivity(tx.QueryRow(ctx, `SELECT `+activityColumns+` FROM lineage_activities WHERE activity_id = $1`, existing.ActivityID))
				if err != nil {
					return err
				}
				if !sameStrPtr(prev.BuildVersion, req.BuildVersion) || !sameStrPtr(prev.ConfigDigest, req.ConfigDigest) {
					return domain.ErrRunProvenanceConflict
				}
			}
			out = existing
			return nil
		}

		var versionName *string
		if req.TransformationVersion != nil {
			v, err := findOrCreateTransformationVersion(ctx, tx, tenantID, *req.TransformationVersion, actor)
			if err != nil {
				return fmt.Errorf("transformation version: %w", err)
			}
			versionName = &v.Name
		}

		edgeID := newID(domain.PrefixLineageEdge)
		claim.ResourceID = edgeID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		activityID := newID(domain.PrefixLineageActivity)
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `
			INSERT INTO lineage_activities (activity_id, tenant_id, activity_type, transformation_version, agent, occurred_at,
				run_id, build_version, config_digest, environment, region)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			activityID, tenantID, req.ActivityType, versionName, req.Agent, now,
			req.RunID, req.BuildVersion, req.ConfigDigest, req.Environment, req.Region); err != nil {
			return fmt.Errorf("insert activity: %w", err)
		}

		got, err := scanEdge(tx.QueryRow(ctx, `
			INSERT INTO lineage_edges (edge_id, tenant_id, source_entity_id, activity_id, derived_entity_id, created_by,
				source_version, derived_version)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			RETURNING `+edgeColumns,
			edgeID, tenantID, source.EntityID, activityID, derived.EntityID, actor,
			req.SourceVersion, req.DerivedVersion))
		if err != nil {
			return fmt.Errorf("insert edge: %w", err)
		}
		out = got

		if err := outbox.Insert(ctx, tx, outbox.Event{AggregateType: "lineage_edge", AggregateID: got.EdgeID,
			EventType: "DATA.LineageRecorded", TenantID: &tenantID, Payload: got}); err != nil {
			return err
		}
		// The spec's own event (ZS-SVC-Z-001 section 12): run, input and output node IDs, and the code and
		// configuration digest, which the edge-only DATA.LineageRecorded payload does not carry.
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "lineage_edge", AggregateID: got.EdgeID,
			EventType: "dqc.lineage.recorded", TenantID: &tenantID, Payload: map[string]any{
				"edge_id": got.EdgeID, "activity_id": activityID, "activity_type": req.ActivityType,
				"run_id": req.RunID, "input_node_id": source.EntityID, "output_node_id": derived.EntityID,
				"input_version": req.SourceVersion, "output_version": req.DerivedVersion,
				"build_version": req.BuildVersion, "config_digest": req.ConfigDigest, "transformation_version": versionName,
				"environment": req.Environment, "region": req.Region, "recorded_by": actor, "recorded_at": got.CreatedAt,
			}})
	})
	return out, err
}

// findCurrentMatchingEdge looks for a non-superseded edge between the
// given source/derived entities whose activity matches activityType and
// transformationVersion exactly.
func findCurrentMatchingEdge(ctx context.Context, tx pgx.Tx, tenantID, sourceEntityID, derivedEntityID, activityType string, transformationVersion *string, p domain.ProvenanceFields) (*domain.LineageEdge, error) {
	// run_id and the two version pins are part of the identity of a derivation:
	// the same transformation run again is a NEW run (and a new edge), and the
	// same pair at a different version is a different fact. Legacy callers that
	// send none of them still match each other exactly as before (NULL = NULL).
	row := tx.QueryRow(ctx, `
		SELECT `+prefixed("e", edgeColumns)+`
		FROM lineage_edges e
		JOIN lineage_activities a ON a.activity_id = e.activity_id
		WHERE e.tenant_id = $1 AND e.source_entity_id = $2 AND e.derived_entity_id = $3
		  AND a.activity_type = $4
		  AND a.transformation_version IS NOT DISTINCT FROM $5
		  AND a.run_id IS NOT DISTINCT FROM $6
		  AND e.source_version IS NOT DISTINCT FROM $7
		  AND e.derived_version IS NOT DISTINCT FROM $8
		  AND NOT EXISTS (SELECT 1 FROM lineage_edges s WHERE s.supersedes_edge_id = e.edge_id)
		LIMIT 1`,
		tenantID, sourceEntityID, derivedEntityID, activityType, transformationVersion,
		p.RunID, p.SourceVersion, p.DerivedVersion)
	edge, err := scanEdge(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return edge, err
}

// prefixed qualifies a comma-separated column list with an alias, so a
// column list constant can be reused in both an unaliased and a
// table-aliased query without hand-duplicating it.
func prefixed(alias, columns string) string {
	parts := strings.Split(columns, ", ")
	for i, p := range parts {
		parts[i] = alias + "." + p
	}
	return strings.Join(parts, ", ")
}

// ── SupersedeLineage ─────────────────────────────────────────────────────────

// SupersedeLineage never mutates the old edge's own row — it locks the old
// edge (serializing concurrent supersession attempts against the SAME
// edge, so two callers can't both successfully supersede it) and inserts
// a brand new edge carrying supersedes_edge_id, leaving the original
// intact and still historically queryable.
func (s *PgStore) SupersedeLineage(ctx context.Context, tenantID string, req domain.SupersedeLineageRequest, actor string, claim domain.IdempotencyClaim) (*domain.LineageEdge, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.LineageEdge
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var oldSource, oldActivity, oldDerived string
		var oldSourceVersion, oldDerivedVersion *string
		err := tx.QueryRow(ctx, `SELECT source_entity_id, activity_id, derived_entity_id, source_version, derived_version
			FROM lineage_edges WHERE edge_id = $1 FOR UPDATE`, req.EdgeID).Scan(&oldSource, &oldActivity, &oldDerived,
			&oldSourceVersion, &oldDerivedVersion)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrLineageEdgeNotFound
		}
		if err != nil {
			return err
		}
		var alreadySuperseded bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM lineage_edges WHERE supersedes_edge_id = $1)`,
			req.EdgeID).Scan(&alreadySuperseded); err != nil {
			return err
		}
		if alreadySuperseded {
			return domain.ErrLineageEdgeAlreadySuperseded
		}

		newEdgeID := newID(domain.PrefixLineageEdge)
		claim.ResourceID = newEdgeID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		// By default the superseding edge reuses the old activity and pins. When
		// the caller supplies corrected provenance, a NEW activity is written
		// from the old one overlaid with the corrections — the old activity row
		// is never edited (it is immutable), so history stays intact and the
		// correction is itself a recorded fact (NP-39).
		activityID := oldActivity
		sourceVersion, derivedVersion := oldSourceVersion, oldDerivedVersion
		if req.SourceVersion != nil {
			sourceVersion = req.SourceVersion
		}
		if req.DerivedVersion != nil {
			derivedVersion = req.DerivedVersion
		}
		if req.HasReplacement() {
			prev, err := scanActivity(tx.QueryRow(ctx, `SELECT `+activityColumns+` FROM lineage_activities WHERE activity_id = $1`, oldActivity))
			if err != nil {
				return fmt.Errorf("load activity being corrected: %w", err)
			}
			next := *prev
			if req.TransformationVersion != nil {
				v, err := findOrCreateTransformationVersion(ctx, tx, tenantID, *req.TransformationVersion, actor)
				if err != nil {
					return fmt.Errorf("transformation version: %w", err)
				}
				next.TransformationVersion = &v.Name
			}
			if req.RunID != nil {
				next.RunID = req.RunID
			}
			if req.BuildVersion != nil {
				next.BuildVersion = req.BuildVersion
			}
			if req.ConfigDigest != nil {
				next.ConfigDigest = req.ConfigDigest
			}
			if req.Environment != nil {
				next.Environment = req.Environment
			}
			if req.Region != nil {
				next.Region = req.Region
			}
			activityID = newID(domain.PrefixLineageActivity)
			if _, err := tx.Exec(ctx, `
				INSERT INTO lineage_activities (activity_id, tenant_id, activity_type, transformation_version, agent, occurred_at,
					run_id, build_version, config_digest, environment, region)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
				activityID, tenantID, next.ActivityType, next.TransformationVersion, next.Agent, time.Now().UTC(),
				next.RunID, next.BuildVersion, next.ConfigDigest, next.Environment, next.Region); err != nil {
				return fmt.Errorf("insert replacement activity: %w", err)
			}
		}

		got, err := scanEdge(tx.QueryRow(ctx, `
			INSERT INTO lineage_edges (edge_id, tenant_id, source_entity_id, activity_id, derived_entity_id,
				supersedes_edge_id, reason, created_by, source_version, derived_version)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			RETURNING `+edgeColumns,
			newEdgeID, tenantID, oldSource, activityID, oldDerived, req.EdgeID, req.Reason, actor,
			sourceVersion, derivedVersion))
		if err != nil {
			return fmt.Errorf("insert superseding edge: %w", err)
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "lineage_edge", AggregateID: got.EdgeID,
			EventType: "DATA.LineageSuperseded", TenantID: &tenantID, Payload: got})
	})
	return out, err
}

// ── AttachEvidence ───────────────────────────────────────────────────────────

func (s *PgStore) AttachEvidence(ctx context.Context, tenantID string, req domain.AttachEvidenceRequest, actor string, claim domain.IdempotencyClaim) (*domain.EvidenceAttachment, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.EvidenceAttachment
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		entity, err := findOrCreateEntity(ctx, tx, tenantID, req.Entity)
		if err != nil {
			return err
		}
		attachmentID := newID(domain.PrefixEvidenceAttachment)
		claim.ResourceID = attachmentID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		var a domain.EvidenceAttachment
		if err := tx.QueryRow(ctx, `
			INSERT INTO evidence_attachments (attachment_id, tenant_id, entity_id, evidence_ref, attached_by)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING attachment_id, tenant_id, entity_id, evidence_ref, attached_at, attached_by`,
			attachmentID, tenantID, entity.EntityID, req.EvidenceRef, actor,
		).Scan(&a.AttachmentID, &a.TenantID, &a.EntityID, &a.EvidenceRef, &a.AttachedAt, &a.AttachedBy); err != nil {
			return fmt.Errorf("insert evidence attachment: %w", err)
		}
		out = &a
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "evidence_attachment", AggregateID: a.AttachmentID,
			EventType: "DATA.EvidenceAttached", TenantID: &tenantID, Payload: a})
	})
	return out, err
}

// ── Graph traversal ──────────────────────────────────────────────────────────

// walkGraph recursively follows edges from startEntityID, either backward
// (upstream: derived -> source) or forward (downstream: source ->
// derived). When asOf is nil, only currently-non-superseded edges are
// followed and only "not superseded" is checked against now; when asOf is
// set, both the edge's own creation and any superseding edge are bounded
// by asOf, so a query for a past moment sees exactly the graph as it
// existed then — a superseded edge remains historically walkable if the
// supersession happened after asOf.
func walkGraph(ctx context.Context, tx pgx.Tx, tenantID, startEntityID, direction string, asOf *time.Time) ([]domain.LineageEdge, error) {
	var followCol, matchCol string
	if direction == "upstream" {
		followCol, matchCol = "derived_entity_id", "source_entity_id"
	} else {
		followCol, matchCol = "source_entity_id", "derived_entity_id"
	}

	notSuperseded := `NOT EXISTS (SELECT 1 FROM lineage_edges s WHERE s.supersedes_edge_id = e.edge_id)`
	createdFilter := ""
	args := []interface{}{tenantID, startEntityID}
	if asOf != nil {
		notSuperseded = `NOT EXISTS (SELECT 1 FROM lineage_edges s WHERE s.supersedes_edge_id = e.edge_id AND s.created_at <= $3)`
		createdFilter = ` AND e.created_at <= $3`
		args = append(args, *asOf)
	}

	query := fmt.Sprintf(`
		WITH RECURSIVE walk AS (
			SELECT e.edge_id, e.source_entity_id, e.activity_id, e.derived_entity_id
			FROM lineage_edges e
			WHERE e.tenant_id = $1 AND e.%s = $2 AND %s%s
			UNION
			SELECT e.edge_id, e.source_entity_id, e.activity_id, e.derived_entity_id
			FROM lineage_edges e
			JOIN walk w ON e.%s = w.%s
			WHERE e.tenant_id = $1 AND %s%s
		)
		SELECT `+prefixed("w", edgeColumnsNoAudit)+`, w.edge_id
		FROM (SELECT DISTINCT edge_id, source_entity_id, activity_id, derived_entity_id FROM walk) w`,
		followCol, notSuperseded, createdFilter, followCol, matchCol, notSuperseded, createdFilter)

	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("walk graph: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var sourceEntityID, activityID, derivedEntityID, edgeID, dupEdgeID string
		if err := rows.Scan(&edgeID, &sourceEntityID, &activityID, &derivedEntityID, &dupEdgeID); err != nil {
			return nil, err
		}
		ids = append(ids, edgeID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return loadEdgesByID(ctx, tx, tenantID, ids)
}

// edgeColumnsNoAudit is edgeColumns without the audit trailer, matching
// the 4 columns walkGraph's CTE actually selects (edge_id is repeated as
// the final scan target so the driver has an unambiguous column list).
const edgeColumnsNoAudit = `edge_id, source_entity_id, activity_id, derived_entity_id`

func loadEdgesByID(ctx context.Context, tx pgx.Tx, tenantID string, ids []string) ([]domain.LineageEdge, error) {
	rows, err := tx.Query(ctx, `SELECT `+edgeColumns+` FROM lineage_edges WHERE tenant_id = $1 AND edge_id = ANY($2)`, tenantID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.LineageEdge
	for rows.Next() {
		e, err := scanEdge(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// buildSnapshot loads every entity and activity referenced by edges, and
// assembles the full LineageGraphSnapshot.
func buildSnapshot(ctx context.Context, tx pgx.Tx, tenantID, rootEntityID string, edges []domain.LineageEdge) (*domain.LineageGraphSnapshot, error) {
	entityIDs := map[string]bool{rootEntityID: true}
	activityIDs := map[string]bool{}
	for _, e := range edges {
		entityIDs[e.SourceEntityID] = true
		entityIDs[e.DerivedEntityID] = true
		activityIDs[e.ActivityID] = true
	}

	entities, err := loadEntitiesByID(ctx, tx, tenantID, keys(entityIDs))
	if err != nil {
		return nil, err
	}
	activities, err := loadActivitiesByID(ctx, tx, tenantID, keys(activityIDs))
	if err != nil {
		return nil, err
	}

	return &domain.LineageGraphSnapshot{RootEntityID: rootEntityID, Entities: entities, Activities: activities, Edges: edges}, nil
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func loadEntitiesByID(ctx context.Context, tx pgx.Tx, tenantID string, ids []string) ([]domain.LineageEntity, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `SELECT `+entityColumns+` FROM lineage_entities WHERE tenant_id = $1 AND entity_id = ANY($2)`, tenantID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.LineageEntity
	for rows.Next() {
		e, err := scanEntity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

func loadActivitiesByID(ctx context.Context, tx pgx.Tx, tenantID string, ids []string) ([]domain.LineageActivity, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `SELECT `+activityColumns+` FROM lineage_activities WHERE tenant_id = $1 AND activity_id = ANY($2)`, tenantID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.LineageActivity
	for rows.Next() {
		a, err := scanActivity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

func (s *PgStore) GetUpstreamLineage(ctx context.Context, tenantID, entityID string) (*domain.LineageGraphSnapshot, error) {
	return s.getLineageGraph(ctx, tenantID, entityID, "upstream", nil)
}

func (s *PgStore) GetDownstreamImpact(ctx context.Context, tenantID, entityID string) (*domain.LineageGraphSnapshot, error) {
	return s.getLineageGraph(ctx, tenantID, entityID, "downstream", nil)
}

func (s *PgStore) GetAsOfLineage(ctx context.Context, tenantID, entityID string, asOf time.Time) (*domain.LineageGraphSnapshot, error) {
	return s.getLineageGraph(ctx, tenantID, entityID, "upstream", &asOf)
}

func (s *PgStore) getLineageGraph(ctx context.Context, tenantID, entityID, direction string, asOf *time.Time) (*domain.LineageGraphSnapshot, error) {
	var out *domain.LineageGraphSnapshot
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := scanEntity(tx.QueryRow(ctx, `SELECT `+entityColumns+` FROM lineage_entities WHERE entity_id = $1`, entityID)); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrLineageEntityNotFound
			}
			return err
		}
		edges, err := walkGraph(ctx, tx, tenantID, entityID, direction, asOf)
		if err != nil {
			return err
		}
		snap, err := buildSnapshot(ctx, tx, tenantID, entityID, edges)
		if err != nil {
			return err
		}
		out = snap
		return nil
	})
	return out, err
}

// GetSourceToReportPath returns the ROOT entities of entityID's upstream
// graph — entities that appear as a source within the graph but never
// themselves as a derived entity within it, i.e. where the provenance
// chain terminates (the ultimate original sources behind entityID).
func (s *PgStore) GetSourceToReportPath(ctx context.Context, tenantID, entityID string) ([]domain.LineageEntity, error) {
	snap, err := s.GetUpstreamLineage(ctx, tenantID, entityID)
	if err != nil {
		return nil, err
	}
	isSource := map[string]bool{}
	isDerived := map[string]bool{}
	for _, e := range snap.Edges {
		isSource[e.SourceEntityID] = true
		isDerived[e.DerivedEntityID] = true
	}
	var roots []domain.LineageEntity
	for _, e := range snap.Entities {
		if isSource[e.EntityID] && !isDerived[e.EntityID] {
			roots = append(roots, e)
		}
	}
	return roots, nil
}

// ── SealManifest ─────────────────────────────────────────────────────────────

const manifestColumns = `manifest_id, tenant_id, entity_id, manifest, manifest_sha256, sealed_at, sealed_by`

func sameStrPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func blank(s *string) bool { return s == nil || strings.TrimSpace(*s) == "" }

// requireSealableGraph is the certification gate. Recording accepts partial
// evidence; a SEALED manifest must not paper over a gap in the evidence it
// certifies (ZS-SVC-Z-001 DQC-04, TC-09, NP-31..NP-34):
//
//   - an entity with no upstream edges has no lineage to certify — the manifest
//     would be a hash of nothing, indistinguishable from "never recorded";
//   - every activity must name its transformation, run, build and config digest;
//   - every edge must pin the exact source and derived versions.
//
// Order matters only for which error a caller sees first; the transformation
// version check keeps its original precedence.
func requireSealableGraph(snap *domain.LineageGraphSnapshot) error {
	// The gaps are enumerated once, in domain.LineageGaps, so what a reader is TOLD is missing and what
	// sealing REFUSES for can never diverge. Sealing reports the first gap by precedence.
	gaps := domain.LineageGaps(snap)
	if len(gaps) == 0 {
		return nil
	}
	has := func(kind string) bool {
		for _, g := range gaps {
			if g.Kind == kind {
				return true
			}
		}
		return false
	}
	switch {
	case has(domain.GapNoInputLineage):
		return domain.ErrNoInputLineage
	case has(domain.GapMissingTransformationVer):
		return domain.ErrMissingTransformationVersion
	}
	field := map[string]string{
		domain.GapMissingRunID: "run_id", domain.GapMissingBuildVersion: "build_version", domain.GapMissingConfigDigest: "config_digest",
	}
	for _, g := range gaps {
		if f, ok := field[g.Kind]; ok {
			return fmt.Errorf("%w: activity %s has no %s", domain.ErrMissingRunProvenance, g.ActivityID, f)
		}
	}
	for _, g := range gaps {
		switch g.Kind {
		case domain.GapMissingSourceVersion:
			return fmt.Errorf("%w: edge %s has no source_version", domain.ErrMissingVersionPin, g.EdgeID)
		case domain.GapMissingDerivedVersion:
			return fmt.Errorf("%w: edge %s has no derived_version", domain.ErrMissingVersionPin, g.EdgeID)
		}
	}
	return nil
}

// SealManifest walks entityID's upstream graph and writes an immutable
// snapshot. Refuses (ErrMissingTransformationVersion) if any activity in
// that graph has no transformation_version recorded — the doc's own named
// acceptance test: a material lineage certification cannot paper over a
// gap in the evidence it's supposed to certify.
func (s *PgStore) SealManifest(ctx context.Context, tenantID, entityID, actor string, claim domain.IdempotencyClaim) (*domain.ProvenanceManifest, error) {
	var out *domain.ProvenanceManifest
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := scanEntity(tx.QueryRow(ctx, `SELECT `+entityColumns+` FROM lineage_entities WHERE entity_id = $1`, entityID)); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrLineageEntityNotFound
			}
			return err
		}
		edges, err := walkGraph(ctx, tx, tenantID, entityID, "upstream", nil)
		if err != nil {
			return err
		}
		snap, err := buildSnapshot(ctx, tx, tenantID, entityID, edges)
		if err != nil {
			return err
		}
		if err := requireSealableGraph(snap); err != nil {
			return err
		}

		manifestID := newID(domain.PrefixProvenanceManifest)
		claim.ResourceID = manifestID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		manifestJSON, err := json.Marshal(snap)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(manifestJSON)
		manifestSHA256 := hex.EncodeToString(sum[:])
		now := time.Now().UTC()

		var m domain.ProvenanceManifest
		if err := tx.QueryRow(ctx, `
			INSERT INTO provenance_manifests (manifest_id, tenant_id, entity_id, manifest, manifest_sha256, sealed_at, sealed_by)
			VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7)
			RETURNING manifest_id, tenant_id, entity_id, manifest_sha256, sealed_at, sealed_by`,
			manifestID, tenantID, entityID, manifestJSON, manifestSHA256, now, actor,
		).Scan(&m.ManifestID, &m.TenantID, &m.EntityID, &m.ManifestSHA256, &m.SealedAt, &m.SealedBy); err != nil {
			return fmt.Errorf("insert provenance manifest: %w", err)
		}
		m.Manifest = *snap
		out = &m

		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "provenance_manifest", AggregateID: m.ManifestID,
			EventType: "DATA.ProvenanceManifestSealed", TenantID: &tenantID, Payload: m})
	})
	return out, err
}
