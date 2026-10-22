// Package store implements privacy-transfer-svc's persistence.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/privacy-transfer-svc/internal/domain"
	"zoiko.io/privacy-transfer-svc/internal/events"
	"zoiko.io/privacy-transfer-svc/internal/middleware"
)

// §19 events owned by PRV-05, enqueued through the transactional outbox
// (§19.1: events communicate already-committed state and must survive a bus
// outage) — in the SAME transaction as the state change they announce.
const (
	// EventTransferAuthorizationChanged — "authorization, route/provider
	// scope, state, conditions, expiry". Emitted when a decision is recorded.
	EventTransferAuthorizationChanged = "privacy.transfer.authorization_changed"
	// EventProcessorChanged — "processor/subprocessor, activity scope, state,
	// effective time". Emitted on relationship create, status change and
	// subprocessor attach.
	EventProcessorChanged = "privacy.processor.changed"
	// EventAssessmentExpiring — "assessment, scope, expiry/review trigger".
	// Emitted when an assessment that declares a review trigger is recorded.
	EventAssessmentExpiring = "privacy.assessment.expiring"
)

// isInvalidUUID reports whether err is Postgres's own "invalid input
// syntax for type uuid" error (SQLSTATE 22P02).
func isInvalidUUID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

// nullString bridges a nullable TEXT column onto a plain (non-pointer)
// string field — several domain types deliberately model "not present" as ""
// on the wire rather than a JSON null.
type nullString struct{ dest *string }

func (n *nullString) Scan(src interface{}) error {
	if src == nil {
		*n.dest = ""
		return nil
	}
	s, _ := src.(string)
	*n.dest = s
	return nil
}

// Meta is the actor/trace context of one mutation, carried into its event.
type Meta struct {
	PrincipalID   string
	CorrelationID string
}

// Idem is the idempotency claim of one mutation (§18.1). The claim, the
// business change, the outbox event and the stored replay response are written
// in ONE transaction: either all of them exist or none does, so a retry after
// any failure re-executes cleanly and concurrent identical requests execute
// exactly once.
type Idem struct {
	Key         string
	PrincipalID string
	Endpoint    string
	RequestHash string
	// Render builds the HTTP status and body to replay from the operation's
	// typed result.
	Render func(result any) (status int, body []byte, err error)
}

// Store is the interface the handler depends on. Every mutating method takes
// the idempotency claim and returns the already-stored record instead of
// executing when the key was claimed before.
type Store interface {
	CreateRelationship(ctx context.Context, tenantID string, req domain.CreateProcessorRelationshipRequest, m Meta, idem *Idem) (*domain.ProcessorRelationship, *domain.IdempotencyRecord, error)
	FindRelationship(ctx context.Context, relationshipID string) (*domain.ProcessorRelationship, error)
	ListRelationships(ctx context.Context) ([]domain.ProcessorRelationship, error)
	UpdateRelationshipStatus(ctx context.Context, relationshipID string, status domain.RelationshipStatus, m Meta, idem *Idem) (*domain.ProcessorRelationship, *domain.IdempotencyRecord, error)

	AttachSubprocessor(ctx context.Context, relationshipID string, req domain.AttachSubprocessorRequest, m Meta, idem *Idem) (*domain.Subprocessor, *domain.IdempotencyRecord, error)
	ListSubprocessors(ctx context.Context, relationshipID string) ([]domain.Subprocessor, error)

	CreateMechanism(ctx context.Context, tenantID string, req domain.CreateTransferMechanismRequest, m Meta, idem *Idem) (*domain.TransferMechanism, *domain.IdempotencyRecord, error)
	FindMechanism(ctx context.Context, mechanismID string) (*domain.TransferMechanism, error)

	RecordAssessment(ctx context.Context, tenantID string, req domain.RecordTransferAssessmentRequest, m Meta, idem *Idem) (*domain.TransferAssessment, *domain.IdempotencyRecord, error)
	FindLatestAssessment(ctx context.Context, relationshipID string) (*domain.TransferAssessment, error)

	// RecordDecision persists the decision, its authorization_changed event and
	// the replay response atomically.
	RecordDecision(ctx context.Context, tenantID string, d *domain.TransferDecision, m Meta, idem *Idem) (*domain.IdempotencyRecord, error)
	FindDecision(ctx context.Context, decisionID string) (*domain.TransferDecision, error)

	// GetIdempotency looks up a stored response by (tenant, principal, key).
	GetIdempotency(ctx context.Context, tenantID, principalID, key string) (*domain.IdempotencyRecord, error)
}

type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

func NewPgStore(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

// withTenant runs fn in a transaction scoped by the verified tenant carried in
// ctx (transaction-local app.tenant_id). A missing tenant is refused outright
// rather than degrading to an empty scope.
func (s *PgStore) withTenant(ctx context.Context, fn func(pgx.Tx) error) error {
	tenant := strings.TrimSpace(middleware.TenantFromContext(ctx))
	if tenant == "" {
		return errors.New("verified tenant is required for store operations")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenant); err != nil {
		return fmt.Errorf("set_config app.tenant_id: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

var errReplay = errors.New("idempotent replay")

func isDomainError(err error) bool {
	for _, e := range []error{domain.ErrRelationshipNotFound, domain.ErrMechanismNotFound, domain.ErrDecisionNotFound} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// mutate runs fn atomically with the idempotency claim (when idem != nil).
// On a prior claim it executes nothing and returns the stored record.
func (s *PgStore) mutate(ctx context.Context, op string, idem *Idem, fn func(tx pgx.Tx) (any, error)) (any, *domain.IdempotencyRecord, error) {
	tenant := strings.TrimSpace(middleware.TenantFromContext(ctx))
	var result any
	var existing *domain.IdempotencyRecord
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		if idem != nil {
			tag, err := tx.Exec(ctx, `
				INSERT INTO transfer_idempotency_keys
					(idempotency_key, tenant_id, principal_id, endpoint, request_hash, response_code, response_body, created_at)
				VALUES ($1, $2, $3, $4, $5, 0, ''::bytea, NOW())
				ON CONFLICT (tenant_id, principal_id, idempotency_key) DO NOTHING`,
				idem.Key, tenant, idem.PrincipalID, idem.Endpoint, idem.RequestHash)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				rec, err := getIdempotencyTx(ctx, tx, tenant, idem.PrincipalID, idem.Key)
				if err != nil {
					return err
				}
				existing = rec
				return errReplay
			}
		}
		r, err := fn(tx)
		if err != nil {
			return err
		}
		result = r
		if idem != nil {
			code, body, err := idem.Render(r)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE transfer_idempotency_keys SET response_code = $1, response_body = $2
				WHERE tenant_id = $3 AND principal_id = $4 AND idempotency_key = $5`,
				code, body, tenant, idem.PrincipalID, idem.Key); err != nil {
				return err
			}
		}
		return nil
	})
	switch {
	case errors.Is(err, errReplay):
		return nil, existing, nil
	case err == nil:
		return result, nil, nil
	case isDomainError(err):
		return nil, nil, err
	default:
		s.log.Error("pg "+op+" failed", zap.Error(err))
		return nil, nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func marshalSlice(s []string) []byte {
	if s == nil {
		s = []string{}
	}
	raw, _ := json.Marshal(s)
	return raw
}

func unmarshalSlice(raw []byte, dest *[]string) {
	if len(raw) == 0 {
		*dest = []string{}
		return
	}
	_ = json.Unmarshal(raw, dest)
}

func enqueue(ctx context.Context, tx pgx.Tx, eventType, entityID, tenantID string, m Meta, payload any) error {
	return events.EnqueueTx(ctx, tx, events.PublishParams{
		EventType: eventType, EntityID: entityID, TenantID: tenantID,
		ActorID: m.PrincipalID, CorrelationID: m.CorrelationID, Payload: payload,
	})
}

// ── processor relationships ──────────────────────────────────────────────────

const relationshipColumns = `
	relationship_id, tenant_id, controller_ref, processor_ref, service, processing_instructions,
	purpose_activity_refs, data_categories, subject_classes, contract_evidence_ref, jurisdictions,
	status, created_at, created_by_principal_id`

func scanRelationship(row pgx.Row) (*domain.ProcessorRelationship, error) {
	r := &domain.ProcessorRelationship{}
	var purposeRefsRaw, categoriesRaw, subjectsRaw, jurisdictionsRaw []byte
	err := row.Scan(&r.RelationshipID, &r.TenantID, &r.ControllerRef, &r.ProcessorRef, &r.Service, &nullString{&r.ProcessingInstructions},
		&purposeRefsRaw, &categoriesRaw, &subjectsRaw, &nullString{&r.ContractEvidenceRef}, &jurisdictionsRaw,
		&r.Status, &r.CreatedAt, &r.CreatedByPrincipalID)
	if err != nil {
		return nil, err
	}
	unmarshalSlice(purposeRefsRaw, &r.PurposeActivityRefs)
	unmarshalSlice(categoriesRaw, &r.DataCategories)
	unmarshalSlice(subjectsRaw, &r.SubjectClasses)
	unmarshalSlice(jurisdictionsRaw, &r.Jurisdictions)
	return r, nil
}

func (s *PgStore) CreateRelationship(ctx context.Context, tenantID string, req domain.CreateProcessorRelationshipRequest, m Meta, idem *Idem) (*domain.ProcessorRelationship, *domain.IdempotencyRecord, error) {
	id := uuid.New().String()
	res, ex, err := s.mutate(ctx, "CreateRelationship", idem, func(tx pgx.Tx) (any, error) {
		r, err := scanRelationship(tx.QueryRow(ctx, `
			INSERT INTO processor_relationships (`+relationshipColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'ACTIVE', NOW(), $12)
			RETURNING `+relationshipColumns,
			id, strPtrOrNil(tenantID), req.ControllerRef, req.ProcessorRef, req.Service, strPtrOrNil(req.ProcessingInstructions),
			marshalSlice(req.PurposeActivityRefs), marshalSlice(req.DataCategories), marshalSlice(req.SubjectClasses),
			strPtrOrNil(req.ContractEvidenceRef), marshalSlice(req.Jurisdictions), m.PrincipalID,
		))
		if err != nil {
			return nil, err
		}
		if err := enqueue(ctx, tx, EventProcessorChanged, r.RelationshipID, tenantID, m, r); err != nil {
			return nil, err
		}
		return r, nil
	})
	if err != nil || ex != nil {
		return nil, ex, err
	}
	return res.(*domain.ProcessorRelationship), nil, nil
}

func (s *PgStore) FindRelationship(ctx context.Context, relationshipID string) (*domain.ProcessorRelationship, error) {
	var r *domain.ProcessorRelationship
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		r, err = scanRelationship(tx.QueryRow(ctx, `SELECT `+relationshipColumns+` FROM processor_relationships WHERE relationship_id = $1`, relationshipID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrRelationshipNotFound
	}
	if err != nil {
		s.log.Error("pg FindRelationship failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return r, nil
}

func (s *PgStore) ListRelationships(ctx context.Context) ([]domain.ProcessorRelationship, error) {
	var out []domain.ProcessorRelationship
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+relationshipColumns+` FROM processor_relationships ORDER BY created_at DESC`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRelationship(rows)
			if err != nil {
				return err
			}
			out = append(out, *r)
		}
		return rows.Err()
	})
	if err != nil {
		s.log.Error("pg ListRelationships failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) UpdateRelationshipStatus(ctx context.Context, relationshipID string, status domain.RelationshipStatus, m Meta, idem *Idem) (*domain.ProcessorRelationship, *domain.IdempotencyRecord, error) {
	tenant := middleware.TenantFromContext(ctx)
	res, ex, err := s.mutate(ctx, "UpdateRelationshipStatus", idem, func(tx pgx.Tx) (any, error) {
		r, err := scanRelationship(tx.QueryRow(ctx, `
			UPDATE processor_relationships SET status = $2 WHERE relationship_id = $1
			RETURNING `+relationshipColumns,
			relationshipID, status,
		))
		if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
			return nil, domain.ErrRelationshipNotFound
		}
		if err != nil {
			return nil, err
		}
		// A status transition is a processor state change consumers must see.
		if err := enqueue(ctx, tx, EventProcessorChanged, r.RelationshipID, tenant, m, r); err != nil {
			return nil, err
		}
		return r, nil
	})
	if err != nil || ex != nil {
		return nil, ex, err
	}
	return res.(*domain.ProcessorRelationship), nil, nil
}

// ── subprocessors ────────────────────────────────────────────────────────────

func (s *PgStore) AttachSubprocessor(ctx context.Context, relationshipID string, req domain.AttachSubprocessorRequest, m Meta, idem *Idem) (*domain.Subprocessor, *domain.IdempotencyRecord, error) {
	id := uuid.New().String()
	res, ex, err := s.mutate(ctx, "AttachSubprocessor", idem, func(tx pgx.Tx) (any, error) {
		var tenantID *string
		if err := tx.QueryRow(ctx, `SELECT tenant_id FROM processor_relationships WHERE relationship_id = $1`, relationshipID).Scan(&tenantID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
				return nil, domain.ErrRelationshipNotFound
			}
			return nil, err
		}

		var sp domain.Subprocessor
		var locationsRaw, onwardRaw []byte
		if err := tx.QueryRow(ctx, `
			INSERT INTO subprocessors (subprocessor_id, tenant_id, relationship_id, provider_identity, service, purpose,
				data_scope, processing_locations, onward_subprocessors, notification_approval_model, contract_evidence_ref,
				created_at, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, NOW(), $12)
			RETURNING subprocessor_id, tenant_id, relationship_id, provider_identity, service, purpose, data_scope,
				processing_locations, onward_subprocessors, notification_approval_model, contract_evidence_ref, created_at, created_by_principal_id`,
			id, tenantID, relationshipID, req.ProviderIdentity, req.Service, strPtrOrNil(req.Purpose), strPtrOrNil(req.DataScope),
			marshalSlice(req.ProcessingLocations), marshalSlice(req.OnwardSubprocessors), strPtrOrNil(req.NotificationApprovalModel),
			strPtrOrNil(req.ContractEvidenceRef), m.PrincipalID,
		).Scan(&sp.SubprocessorID, &sp.TenantID, &sp.RelationshipID, &sp.ProviderIdentity, &sp.Service, &nullString{&sp.Purpose}, &nullString{&sp.DataScope},
			&locationsRaw, &onwardRaw, &nullString{&sp.NotificationApprovalModel}, &nullString{&sp.ContractEvidenceRef}, &sp.CreatedAt, &sp.CreatedByPrincipalID); err != nil {
			return nil, err
		}
		unmarshalSlice(locationsRaw, &sp.ProcessingLocations)
		unmarshalSlice(onwardRaw, &sp.OnwardSubprocessors)
		tenant := ""
		if tenantID != nil {
			tenant = *tenantID
		}
		if err := enqueue(ctx, tx, EventProcessorChanged, sp.SubprocessorID, tenant, m, &sp); err != nil {
			return nil, err
		}
		return &sp, nil
	})
	if err != nil || ex != nil {
		return nil, ex, err
	}
	return res.(*domain.Subprocessor), nil, nil
}

func (s *PgStore) ListSubprocessors(ctx context.Context, relationshipID string) ([]domain.Subprocessor, error) {
	var out []domain.Subprocessor
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT subprocessor_id, tenant_id, relationship_id, provider_identity, service, purpose, data_scope,
				processing_locations, onward_subprocessors, notification_approval_model, contract_evidence_ref, created_at, created_by_principal_id
			FROM subprocessors WHERE relationship_id = $1 ORDER BY created_at ASC`, relationshipID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sp domain.Subprocessor
			var locationsRaw, onwardRaw []byte
			if err := rows.Scan(&sp.SubprocessorID, &sp.TenantID, &sp.RelationshipID, &sp.ProviderIdentity, &sp.Service, &nullString{&sp.Purpose}, &nullString{&sp.DataScope},
				&locationsRaw, &onwardRaw, &nullString{&sp.NotificationApprovalModel}, &nullString{&sp.ContractEvidenceRef}, &sp.CreatedAt, &sp.CreatedByPrincipalID); err != nil {
				return err
			}
			unmarshalSlice(locationsRaw, &sp.ProcessingLocations)
			unmarshalSlice(onwardRaw, &sp.OnwardSubprocessors)
			out = append(out, sp)
		}
		return rows.Err()
	})
	if err != nil {
		s.log.Error("pg ListSubprocessors failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// ── transfer mechanisms ──────────────────────────────────────────────────────

const mechanismColumns = `mechanism_id, tenant_id, mechanism_type, evidence_ref, conditions, valid_from, valid_until, created_at, created_by_principal_id`

func scanMechanism(row pgx.Row) (*domain.TransferMechanism, error) {
	m := &domain.TransferMechanism{}
	err := row.Scan(&m.MechanismID, &m.TenantID, &m.MechanismType, &nullString{&m.EvidenceRef}, &nullString{&m.Conditions}, &m.ValidFrom, &m.ValidUntil, &m.CreatedAt, &m.CreatedByPrincipalID)
	if err != nil {
		return nil, err
	}
	return m, nil
}

func (s *PgStore) CreateMechanism(ctx context.Context, tenantID string, req domain.CreateTransferMechanismRequest, m Meta, idem *Idem) (*domain.TransferMechanism, *domain.IdempotencyRecord, error) {
	id := uuid.New().String()
	validFrom := timeOrNow(req.ValidFrom)
	res, ex, err := s.mutate(ctx, "CreateMechanism", idem, func(tx pgx.Tx) (any, error) {
		return scanMechanism(tx.QueryRow(ctx, `
			INSERT INTO transfer_mechanisms (`+mechanismColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), $8)
			RETURNING `+mechanismColumns,
			id, strPtrOrNil(tenantID), req.MechanismType, strPtrOrNil(req.EvidenceRef), strPtrOrNil(req.Conditions),
			validFrom, req.ValidUntil, m.PrincipalID,
		))
	})
	if err != nil || ex != nil {
		return nil, ex, err
	}
	return res.(*domain.TransferMechanism), nil, nil
}

func (s *PgStore) FindMechanism(ctx context.Context, mechanismID string) (*domain.TransferMechanism, error) {
	var m *domain.TransferMechanism
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		m, err = scanMechanism(tx.QueryRow(ctx, `SELECT `+mechanismColumns+` FROM transfer_mechanisms WHERE mechanism_id = $1`, mechanismID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrMechanismNotFound
	}
	if err != nil {
		s.log.Error("pg FindMechanism failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return m, nil
}

// ── transfer assessments ─────────────────────────────────────────────────────

func (s *PgStore) RecordAssessment(ctx context.Context, tenantID string, req domain.RecordTransferAssessmentRequest, m Meta, idem *Idem) (*domain.TransferAssessment, *domain.IdempotencyRecord, error) {
	id := uuid.New().String()
	res, ex, err := s.mutate(ctx, "RecordAssessment", idem, func(tx pgx.Tx) (any, error) {
		var a domain.TransferAssessment
		if err := tx.QueryRow(ctx, `
			INSERT INTO transfer_assessments (assessment_id, tenant_id, relationship_id, outcome, reviewer_principal_id, residual_risk, evidence_ref, government_access_risk, technical_measures, organizational_measures, review_trigger_at, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, NOW())
			RETURNING assessment_id, tenant_id, relationship_id, outcome, reviewer_principal_id, residual_risk, evidence_ref, government_access_risk, technical_measures, organizational_measures, review_trigger_at, created_at`,
			id, strPtrOrNil(tenantID), req.RelationshipID, req.Outcome, m.PrincipalID, strPtrOrNil(req.ResidualRisk),
			strPtrOrNil(req.EvidenceRef), strPtrOrNil(req.GovernmentAccessRisk), strPtrOrNil(req.TechnicalMeasures), strPtrOrNil(req.OrganizationalMeasures), req.ReviewTriggerAt,
		).Scan(&a.AssessmentID, &a.TenantID, &a.RelationshipID, &a.Outcome, &a.ReviewerPrincipalID, &nullString{&a.ResidualRisk}, &nullString{&a.EvidenceRef}, &nullString{&a.GovernmentAccessRisk}, &nullString{&a.TechnicalMeasures}, &nullString{&a.OrganizationalMeasures}, &a.ReviewTriggerAt, &a.CreatedAt); err != nil {
			return nil, err
		}
		// §19 privacy.assessment.expiring — announced when the assessment
		// declares its §17.1 expiry/review trigger.
		if a.ReviewTriggerAt != nil {
			if err := enqueue(ctx, tx, EventAssessmentExpiring, a.AssessmentID, tenantID, m, &a); err != nil {
				return nil, err
			}
		}
		return &a, nil
	})
	if err != nil || ex != nil {
		return nil, ex, err
	}
	return res.(*domain.TransferAssessment), nil, nil
}

func (s *PgStore) FindLatestAssessment(ctx context.Context, relationshipID string) (*domain.TransferAssessment, error) {
	var a domain.TransferAssessment
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT assessment_id, tenant_id, relationship_id, outcome, reviewer_principal_id, residual_risk, evidence_ref, government_access_risk, technical_measures, organizational_measures, review_trigger_at, created_at
			FROM transfer_assessments WHERE relationship_id = $1 ORDER BY created_at DESC LIMIT 1`, relationshipID,
		).Scan(&a.AssessmentID, &a.TenantID, &a.RelationshipID, &a.Outcome, &a.ReviewerPrincipalID, &nullString{&a.ResidualRisk}, &nullString{&a.EvidenceRef}, &nullString{&a.GovernmentAccessRisk}, &nullString{&a.TechnicalMeasures}, &nullString{&a.OrganizationalMeasures}, &a.ReviewTriggerAt, &a.CreatedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, nil
	}
	if err != nil {
		s.log.Error("pg FindLatestAssessment failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &a, nil
}

// ── transfer decisions ───────────────────────────────────────────────────────

func (s *PgStore) RecordDecision(ctx context.Context, tenantID string, d *domain.TransferDecision, m Meta, idem *Idem) (*domain.IdempotencyRecord, error) {
	if d.DecisionID == "" {
		d.DecisionID = uuid.New().String()
	}
	if d.AuthorizationID == "" {
		d.AuthorizationID = d.DecisionID
	}
	reasonRaw := marshalSlice(d.ReasonCodes)
	_, ex, err := s.mutate(ctx, "RecordDecision", idem, func(tx pgx.Tx) (any, error) {
		if err := tx.QueryRow(ctx, `
			INSERT INTO transfer_decisions (decision_id, tenant_id, relationship_id, transfer_mechanism_id, destination_jurisdiction,
				assessment_id, result, reason_codes, actor_principal_id, correlation_id, conditions, expires_at,
				exporter_role, importer_role, origin_jurisdiction, route_provider, frequency_volume_class, decided_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, NOW())
			RETURNING decided_at`,
			d.DecisionID, strPtrOrNil(tenantID), d.RelationshipID, d.TransferMechanismID, strPtrOrNil(d.DestinationJurisdiction),
			d.AssessmentID, d.Result, reasonRaw, d.ActorPrincipalID, strPtrOrNil(d.CorrelationID),
			strPtrOrNil(d.Conditions), d.ExpiresAt,
			strPtrOrNil(d.ExporterRole), strPtrOrNil(d.ImporterRole), strPtrOrNil(d.OriginJurisdiction),
			strPtrOrNil(d.RouteProvider), strPtrOrNil(d.FrequencyVolumeClass),
		).Scan(&d.DecidedAt); err != nil {
			return nil, err
		}
		d.TenantID = strPtrOrNil(tenantID)
		if err := enqueue(ctx, tx, EventTransferAuthorizationChanged, d.DecisionID, tenantID, m, d); err != nil {
			return nil, err
		}
		return d, nil
	})
	return ex, err
}

func (s *PgStore) FindDecision(ctx context.Context, decisionID string) (*domain.TransferDecision, error) {
	var d domain.TransferDecision
	var reasonRaw []byte
	var correlationID, destJurisdiction, conditions *string
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT decision_id, tenant_id, relationship_id, transfer_mechanism_id, destination_jurisdiction, assessment_id,
				result, reason_codes, actor_principal_id, correlation_id, conditions, expires_at,
				exporter_role, importer_role, origin_jurisdiction, route_provider, frequency_volume_class, decided_at
			FROM transfer_decisions WHERE decision_id = $1`, decisionID,
		).Scan(&d.DecisionID, &d.TenantID, &d.RelationshipID, &d.TransferMechanismID, &destJurisdiction, &d.AssessmentID,
			&d.Result, &reasonRaw, &d.ActorPrincipalID, &correlationID, &conditions, &d.ExpiresAt,
			&nullString{&d.ExporterRole}, &nullString{&d.ImporterRole}, &nullString{&d.OriginJurisdiction},
			&nullString{&d.RouteProvider}, &nullString{&d.FrequencyVolumeClass}, &d.DecidedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrDecisionNotFound
	}
	if err != nil {
		s.log.Error("pg FindDecision failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	unmarshalSlice(reasonRaw, &d.ReasonCodes)
	d.AuthorizationID = d.DecisionID
	if correlationID != nil {
		d.CorrelationID = *correlationID
	}
	if destJurisdiction != nil {
		d.DestinationJurisdiction = *destJurisdiction
	}
	if conditions != nil {
		d.Conditions = *conditions
	}
	return &d, nil
}

func timeOrNow(t *time.Time) time.Time {
	if t == nil {
		return time.Now().UTC()
	}
	return *t
}

// ── Idempotency (§18.1) ──────────────────────────────────────────────────────

func getIdempotencyTx(ctx context.Context, tx pgx.Tx, tenantID, principalID, key string) (*domain.IdempotencyRecord, error) {
	var rec domain.IdempotencyRecord
	err := tx.QueryRow(ctx, `
		SELECT idempotency_key, tenant_id, principal_id, endpoint, request_hash, response_code, response_body, created_at
		FROM transfer_idempotency_keys
		WHERE tenant_id = $1 AND principal_id = $2 AND idempotency_key = $3`,
		tenantID, principalID, key,
	).Scan(&rec.IdempotencyKey, &rec.TenantID, &rec.PrincipalID, &rec.Endpoint, &rec.RequestHash, &rec.ResponseCode, &rec.ResponseBody, &rec.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

func (s *PgStore) GetIdempotency(ctx context.Context, tenantID, principalID, key string) (*domain.IdempotencyRecord, error) {
	var rec *domain.IdempotencyRecord
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		r, err := getIdempotencyTx(ctx, tx, tenantID, principalID, key)
		rec = r
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		s.log.Error("pg GetIdempotency failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return rec, nil
}
