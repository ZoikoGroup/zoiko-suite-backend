package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/esignature-integration-svc/internal/domain"
	"zoiko.io/esignature-integration-svc/internal/middleware"
)

// SignatureOrchestrationStore is DRC-05's own persistence contract,
// kept separate from the pre-existing Store interface (service.go /
// store.go) so the original envelope CRUD surface is untouched.
type SignatureOrchestrationStore interface {
	CreateSignatureProfile(ctx context.Context, req *domain.CreateSignatureProfileRequest, principalID string) (*domain.SignatureProfile, error)
	GetSignatureProfile(ctx context.Context, id string) (*domain.SignatureProfile, error)
	ActivateSignatureProfile(ctx context.Context, id string) (*domain.SignatureProfile, error)
	RetireSignatureProfile(ctx context.Context, id string) (*domain.SignatureProfile, error)

	BindSignatureProfile(ctx context.Context, envelopeID, profileID string) (*domain.SignatureEnvelope, error)
	AmendEnvelope(ctx context.Context, oldEnvelopeID string, req *domain.CreateEnvelopeRequest, principalID string) (*domain.SignatureEnvelope, error)

	RecordProviderAttempt(ctx context.Context, envelopeID string, req *domain.RecordProviderAttemptRequest) (*domain.ProviderAttempt, error)
	ReconcileAttempt(ctx context.Context, attemptID string, req *domain.ReconcileAttemptRequest, principalID string) (*domain.ProviderAttempt, error)
	ListProviderAttempts(ctx context.Context, envelopeID string) ([]domain.ProviderAttempt, error)

	AddParticipant(ctx context.Context, envelopeID string, req *domain.AddParticipantRequest) (*domain.Participant, error)
	MarkParticipantViewed(ctx context.Context, participantID string) (*domain.Participant, error)
	MarkParticipantSigned(ctx context.Context, participantID string) (*domain.Participant, error)
	DeclineParticipant(ctx context.Context, participantID, reason string) (*domain.Participant, error)
	ListParticipants(ctx context.Context, envelopeID string) ([]domain.Participant, error)

	SealCompletionEvidence(ctx context.Context, envelopeID string, req *domain.SealCompletionEvidenceRequest, principalID string) (*domain.CompletionEvidence, error)
	GetCompletionEvidence(ctx context.Context, envelopeID string) (*domain.CompletionEvidence, error)
}

// PgOrchestrationStore implements SignatureOrchestrationStore. It is a
// distinct type from PgStore (rather than more methods bolted onto it)
// so each store's constructor states exactly which tables it touches;
// both wrap the same *pgxpool.Pool.
type PgOrchestrationStore struct {
	pool *pgxpool.Pool
}

func NewPgOrchestrationStore(pool *pgxpool.Pool) *PgOrchestrationStore {
	return &PgOrchestrationStore{pool: pool}
}

func (p *PgOrchestrationStore) withTenant(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", middleware.GetTenantID(ctx)); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ── Signature Profiles ───────────────────────────────────────────────────────

const profileColumns = `
	profile_id, tenant_id, legal_entity_id, assurance_level, jurisdiction, identity_requirement,
	witness_required, status, created_by_principal_id, created_at, activated_at, retired_at
`

func scanProfile(row pgx.Row, pr *domain.SignatureProfile) error {
	return row.Scan(&pr.ProfileID, &pr.TenantID, &pr.LegalEntityID, &pr.AssuranceLevel, &pr.Jurisdiction, &pr.IdentityRequirement,
		&pr.WitnessRequired, &pr.Status, &pr.CreatedByPrincipalID, &pr.CreatedAt, &pr.ActivatedAt, &pr.RetiredAt)
}

func (p *PgOrchestrationStore) CreateSignatureProfile(ctx context.Context, req *domain.CreateSignatureProfileRequest, principalID string) (*domain.SignatureProfile, error) {
	if !domain.AssuranceLevel(req.AssuranceLevel).Valid() {
		return nil, domain.ErrInvalidAssuranceLevel
	}
	if !domain.IdentityRequirement(req.IdentityRequirement).Valid() {
		return nil, domain.ErrInvalidIdentityRequirement
	}
	tenantID := middleware.GetTenantID(ctx)
	profileID := uuid.New().String()

	var out domain.SignatureProfile
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO signature_profiles (profile_id, tenant_id, legal_entity_id, assurance_level, jurisdiction,
				identity_requirement, witness_required, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			RETURNING `+profileColumns,
			profileID, tenantID, req.LegalEntityID, req.AssuranceLevel, req.Jurisdiction, req.IdentityRequirement,
			req.WitnessRequired, principalID,
		)
		return scanProfile(row, &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (p *PgOrchestrationStore) GetSignatureProfile(ctx context.Context, id string) (*domain.SignatureProfile, error) {
	var out domain.SignatureProfile
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+profileColumns+` FROM signature_profiles WHERE profile_id = $1 AND tenant_id = $2`,
			id, middleware.GetTenantID(ctx))
		if err := scanProfile(row, &out); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrSignatureProfileNotFound
			}
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (p *PgOrchestrationStore) ActivateSignatureProfile(ctx context.Context, id string) (*domain.SignatureProfile, error) {
	var out domain.SignatureProfile
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM signature_profiles WHERE profile_id = $1 AND tenant_id = $2 FOR UPDATE`,
			id, middleware.GetTenantID(ctx)).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrSignatureProfileNotFound
			}
			return err
		}
		if domain.ProfileStatus(status) != domain.ProfileDraft {
			return domain.ErrSignatureProfileNotDraft
		}
		row := tx.QueryRow(ctx, `UPDATE signature_profiles SET status = 'ACTIVE', activated_at = now()
			WHERE profile_id = $1 RETURNING `+profileColumns, id)
		return scanProfile(row, &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (p *PgOrchestrationStore) RetireSignatureProfile(ctx context.Context, id string) (*domain.SignatureProfile, error) {
	var out domain.SignatureProfile
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM signature_profiles WHERE profile_id = $1 AND tenant_id = $2 FOR UPDATE`,
			id, middleware.GetTenantID(ctx)).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrSignatureProfileNotFound
			}
			return err
		}
		if domain.ProfileStatus(status) != domain.ProfileActive {
			return domain.ErrSignatureProfileNotActive
		}
		row := tx.QueryRow(ctx, `UPDATE signature_profiles SET status = 'RETIRED', retired_at = now()
			WHERE profile_id = $1 RETURNING `+profileColumns, id)
		return scanProfile(row, &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ── Envelope binding / amendment ─────────────────────────────────────────────

const envelopeColumnsFull = `
	envelope_id, tenant_id, legal_entity_id, provider, document_title, signer_email, signer_name, status,
	COALESCE(external_ref,''), created_at, updated_at, signature_profile_id, supersedes_envelope_id
`

func scanEnvelopeFull(row pgx.Row, e *domain.SignatureEnvelope, profileID, supersedes **string) error {
	return row.Scan(&e.EnvelopeID, &e.TenantID, &e.LegalEntityID, &e.Provider, &e.DocumentTitle, &e.SignerEmail, &e.SignerName,
		&e.Status, &e.ExternalRef, &e.CreatedAt, &e.UpdatedAt, profileID, supersedes)
}

// BindSignatureProfile attaches a governed signing contract to an
// envelope — must be ACTIVE, and an envelope can only be bound once.
func (p *PgOrchestrationStore) BindSignatureProfile(ctx context.Context, envelopeID, profileID string) (*domain.SignatureEnvelope, error) {
	tenantID := middleware.GetTenantID(ctx)
	var out domain.SignatureEnvelope
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		var existingProfileID *string
		if err := tx.QueryRow(ctx, `SELECT signature_profile_id FROM signature_envelopes WHERE envelope_id = $1 AND tenant_id = $2 FOR UPDATE`,
			envelopeID, tenantID).Scan(&existingProfileID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrEnvelopeNotFound
			}
			return err
		}
		if existingProfileID != nil {
			return domain.ErrEnvelopeAlreadyBoundToProfile
		}

		var profileStatus string
		if err := tx.QueryRow(ctx, `SELECT status FROM signature_profiles WHERE profile_id = $1 AND tenant_id = $2`,
			profileID, tenantID).Scan(&profileStatus); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrSignatureProfileNotFound
			}
			return err
		}
		if domain.ProfileStatus(profileStatus) != domain.ProfileActive {
			return domain.ErrSignatureProfileNotActive
		}

		row := tx.QueryRow(ctx, `UPDATE signature_envelopes SET signature_profile_id = $2, updated_at = now()
			WHERE envelope_id = $1 RETURNING `+envelopeColumnsFull, envelopeID, profileID)
		var pID, supersedes *string
		if err := scanEnvelopeFull(row, &out, &pID, &supersedes); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// AmendEnvelope voids the old envelope (only reachable from a
// non-terminal state — the trigger is the real enforcement, this is
// the friendlier pre-check) and creates a new one referencing it via
// supersedes_envelope_id, in the same transaction. A SIGNED envelope
// can never be amended — only superseded by an entirely new signing
// process, which this is not.
func (p *PgOrchestrationStore) AmendEnvelope(ctx context.Context, oldEnvelopeID string, req *domain.CreateEnvelopeRequest, principalID string) (*domain.SignatureEnvelope, error) {
	tenantID := middleware.GetTenantID(ctx)
	var out domain.SignatureEnvelope
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM signature_envelopes WHERE envelope_id = $1 AND tenant_id = $2 FOR UPDATE`,
			oldEnvelopeID, tenantID).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrEnvelopeNotFound
			}
			return err
		}
		if status == "SIGNED" || status == "VOIDED" {
			return domain.ErrEnvelopeNotAmendable
		}
		if _, err := tx.Exec(ctx, `UPDATE signature_envelopes SET status = 'VOIDED', updated_at = now() WHERE envelope_id = $1`, oldEnvelopeID); err != nil {
			return err
		}

		newID := uuid.New().String()
		now := time.Now().UTC()
		row := tx.QueryRow(ctx, `
			INSERT INTO signature_envelopes (envelope_id, tenant_id, legal_entity_id, provider, document_title,
				signer_email, signer_name, status, created_at, updated_at, supersedes_envelope_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'SENT', $8, $8, $9)
			RETURNING `+envelopeColumnsFull,
			newID, tenantID, req.LegalEntityID, req.Provider, req.DocumentTitle, req.SignerEmail, req.SignerName, now, oldEnvelopeID,
		)
		var pID, supersedes *string
		return scanEnvelopeFull(row, &out, &pID, &supersedes)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ── Provider Attempts ────────────────────────────────────────────────────────

const attemptColumns = `
	attempt_id, tenant_id, envelope_id, idempotency_key, provider, attempted_action, outcome,
	COALESCE(provider_response_ref,''), COALESCE(error_detail,''), attempted_at, resolved_at, resolved_by_principal_id
`

func scanAttempt(row pgx.Row, a *domain.ProviderAttempt) error {
	return row.Scan(&a.AttemptID, &a.TenantID, &a.EnvelopeID, &a.IdempotencyKey, &a.Provider, &a.AttemptedAction, &a.Outcome,
		&a.ProviderResponseRef, &a.ErrorDetail, &a.AttemptedAt, &a.ResolvedAt, &a.ResolvedByPrincipalID)
}

// RecordProviderAttempt is idempotent on (envelope_id, idempotency_key):
// a retried call with the same key returns the existing attempt instead
// of creating a duplicate — the ON CONFLICT DO NOTHING + fallback SELECT
// pattern means a replay never races the original insert.
func (p *PgOrchestrationStore) RecordProviderAttempt(ctx context.Context, envelopeID string, req *domain.RecordProviderAttemptRequest) (*domain.ProviderAttempt, error) {
	if !domain.ProviderAttemptAction(req.AttemptedAction).Valid() {
		return nil, domain.ErrInvalidAttemptAction
	}
	outcome := req.Outcome
	if outcome == "" {
		outcome = string(domain.AttemptPending)
	}
	if !domain.ProviderAttemptOutcome(outcome).Valid() {
		return nil, domain.ErrInvalidAttemptOutcome
	}
	tenantID := middleware.GetTenantID(ctx)
	attemptID := uuid.New().String()

	var out domain.ProviderAttempt
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		var envExists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM signature_envelopes WHERE envelope_id = $1 AND tenant_id = $2)`,
			envelopeID, tenantID).Scan(&envExists); err != nil {
			return err
		}
		if !envExists {
			return domain.ErrEnvelopeNotFound
		}

		row := tx.QueryRow(ctx, `
			INSERT INTO provider_attempts (attempt_id, tenant_id, envelope_id, idempotency_key, provider, attempted_action, outcome, provider_response_ref, error_detail)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT ON CONSTRAINT provider_attempts_idempotent DO NOTHING
			RETURNING `+attemptColumns,
			attemptID, tenantID, envelopeID, req.IdempotencyKey, req.Provider, req.AttemptedAction, outcome, req.ProviderResponseRef, req.ErrorDetail,
		)
		if err := scanAttempt(row, &out); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Conflict on the idempotency key — this is a replay;
				// return the original attempt rather than a fabricated
				// second one.
				existing := tx.QueryRow(ctx, `SELECT `+attemptColumns+` FROM provider_attempts WHERE envelope_id = $1 AND idempotency_key = $2`,
					envelopeID, req.IdempotencyKey)
				return scanAttempt(existing, &out)
			}
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ReconcileAttempt resolves an UNKNOWN attempt by checking authoritative
// provider status — the only way out of UNKNOWN; never a bare retry.
func (p *PgOrchestrationStore) ReconcileAttempt(ctx context.Context, attemptID string, req *domain.ReconcileAttemptRequest, principalID string) (*domain.ProviderAttempt, error) {
	outcome := domain.ProviderAttemptOutcome(req.Outcome)
	if outcome != domain.AttemptSucceeded && outcome != domain.AttemptFailed {
		return nil, domain.ErrReconcileOutcomeMustBeFinal
	}
	tenantID := middleware.GetTenantID(ctx)
	var out domain.ProviderAttempt
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		var current string
		if err := tx.QueryRow(ctx, `SELECT outcome FROM provider_attempts WHERE attempt_id = $1 AND tenant_id = $2 FOR UPDATE`,
			attemptID, tenantID).Scan(&current); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrProviderAttemptNotFound
			}
			return err
		}
		if domain.ProviderAttemptOutcome(current) != domain.AttemptUnknown {
			return domain.ErrAttemptNotUnknown
		}
		row := tx.QueryRow(ctx, `
			UPDATE provider_attempts SET outcome = $2, provider_response_ref = $3, resolved_at = now(), resolved_by_principal_id = $4
			WHERE attempt_id = $1 RETURNING `+attemptColumns,
			attemptID, string(outcome), req.ProviderResponseRef, principalID,
		)
		return scanAttempt(row, &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (p *PgOrchestrationStore) ListProviderAttempts(ctx context.Context, envelopeID string) ([]domain.ProviderAttempt, error) {
	tenantID := middleware.GetTenantID(ctx)
	res := make([]domain.ProviderAttempt, 0)
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+attemptColumns+` FROM provider_attempts WHERE envelope_id = $1 AND tenant_id = $2 ORDER BY attempted_at`,
			envelopeID, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a domain.ProviderAttempt
			if err := scanAttempt(rows, &a); err != nil {
				return err
			}
			res = append(res, a)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// ── Participants ─────────────────────────────────────────────────────────────

const participantColumns = `
	participant_id, tenant_id, envelope_id, email, name, role, participant_state,
	invited_at, viewed_at, signed_at, declined_at, COALESCE(decline_reason,'')
`

func scanParticipant(row pgx.Row, pt *domain.Participant) error {
	return row.Scan(&pt.ParticipantID, &pt.TenantID, &pt.EnvelopeID, &pt.Email, &pt.Name, &pt.Role, &pt.ParticipantState,
		&pt.InvitedAt, &pt.ViewedAt, &pt.SignedAt, &pt.DeclinedAt, &pt.DeclineReason)
}

func (p *PgOrchestrationStore) AddParticipant(ctx context.Context, envelopeID string, req *domain.AddParticipantRequest) (*domain.Participant, error) {
	role := req.Role
	if role == "" {
		role = string(domain.RoleSigner)
	}
	if !domain.ParticipantRole(role).Valid() {
		return nil, domain.ErrInvalidParticipantRole
	}
	tenantID := middleware.GetTenantID(ctx)
	participantID := uuid.New().String()

	var out domain.Participant
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		var envExists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM signature_envelopes WHERE envelope_id = $1 AND tenant_id = $2)`,
			envelopeID, tenantID).Scan(&envExists); err != nil {
			return err
		}
		if !envExists {
			return domain.ErrEnvelopeNotFound
		}
		row := tx.QueryRow(ctx, `
			INSERT INTO participants (participant_id, tenant_id, envelope_id, email, name, role)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING `+participantColumns,
			participantID, tenantID, envelopeID, req.Email, req.Name, role,
		)
		return scanParticipant(row, &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func loadParticipantForUpdate(ctx context.Context, tx pgx.Tx, tenantID, id string) (*domain.Participant, error) {
	var pt domain.Participant
	row := tx.QueryRow(ctx, `SELECT `+participantColumns+` FROM participants WHERE participant_id = $1 AND tenant_id = $2 FOR UPDATE`,
		id, tenantID)
	if err := scanParticipant(row, &pt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrParticipantNotFound
		}
		return nil, err
	}
	return &pt, nil
}

func (p *PgOrchestrationStore) MarkParticipantViewed(ctx context.Context, participantID string) (*domain.Participant, error) {
	tenantID := middleware.GetTenantID(ctx)
	var out domain.Participant
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		pt, err := loadParticipantForUpdate(ctx, tx, tenantID, participantID)
		if err != nil {
			return err
		}
		if pt.ParticipantState != domain.ParticipantInvited {
			return domain.ErrParticipantNotViewable
		}
		row := tx.QueryRow(ctx, `UPDATE participants SET participant_state = 'VIEWED', viewed_at = now()
			WHERE participant_id = $1 RETURNING `+participantColumns, participantID)
		return scanParticipant(row, &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (p *PgOrchestrationStore) MarkParticipantSigned(ctx context.Context, participantID string) (*domain.Participant, error) {
	tenantID := middleware.GetTenantID(ctx)
	var out domain.Participant
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		pt, err := loadParticipantForUpdate(ctx, tx, tenantID, participantID)
		if err != nil {
			return err
		}
		if pt.ParticipantState != domain.ParticipantViewed {
			return domain.ErrParticipantNotSignable
		}
		row := tx.QueryRow(ctx, `UPDATE participants SET participant_state = 'SIGNED', signed_at = now()
			WHERE participant_id = $1 RETURNING `+participantColumns, participantID)
		return scanParticipant(row, &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (p *PgOrchestrationStore) DeclineParticipant(ctx context.Context, participantID, reason string) (*domain.Participant, error) {
	tenantID := middleware.GetTenantID(ctx)
	var out domain.Participant
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		pt, err := loadParticipantForUpdate(ctx, tx, tenantID, participantID)
		if err != nil {
			return err
		}
		if pt.ParticipantState != domain.ParticipantInvited && pt.ParticipantState != domain.ParticipantViewed {
			return domain.ErrParticipantNotDeclinable
		}
		row := tx.QueryRow(ctx, `UPDATE participants SET participant_state = 'DECLINED', declined_at = now(), decline_reason = $2
			WHERE participant_id = $1 RETURNING `+participantColumns, participantID, reason)
		return scanParticipant(row, &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (p *PgOrchestrationStore) ListParticipants(ctx context.Context, envelopeID string) ([]domain.Participant, error) {
	tenantID := middleware.GetTenantID(ctx)
	res := make([]domain.Participant, 0)
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+participantColumns+` FROM participants WHERE envelope_id = $1 AND tenant_id = $2 ORDER BY invited_at`,
			envelopeID, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var pt domain.Participant
			if err := scanParticipant(rows, &pt); err != nil {
				return err
			}
			res = append(res, pt)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// ── Completion Evidence ──────────────────────────────────────────────────────

const evidenceColumns = `evidence_id, tenant_id, envelope_id, completion_certificate_ref, completed_artifact_hash, sealed_at, sealed_by_principal_id`

func scanEvidence(row pgx.Row, e *domain.CompletionEvidence) error {
	return row.Scan(&e.EvidenceID, &e.TenantID, &e.EnvelopeID, &e.CompletionCertificateRef, &e.CompletedArtifactHash, &e.SealedAt, &e.SealedByPrincipalID)
}

// SealCompletionEvidence writes the sealed, durable completion record —
// only valid once the envelope itself is SIGNED, and only once per
// envelope (the UNIQUE constraint and the trigger both forbid a second
// write; this check gives the friendlier error).
func (p *PgOrchestrationStore) SealCompletionEvidence(ctx context.Context, envelopeID string, req *domain.SealCompletionEvidenceRequest, principalID string) (*domain.CompletionEvidence, error) {
	if len(req.CompletedArtifactHash) != 64 {
		return nil, errors.New("completed_artifact_hash must be a 64-character hex SHA-256 digest")
	}
	tenantID := middleware.GetTenantID(ctx)
	evidenceID := uuid.New().String()

	var out domain.CompletionEvidence
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM signature_envelopes WHERE envelope_id = $1 AND tenant_id = $2`,
			envelopeID, tenantID).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrEnvelopeNotFound
			}
			return err
		}
		if status != "SIGNED" {
			return domain.ErrEnvelopeNotSigned
		}

		row := tx.QueryRow(ctx, `
			INSERT INTO completion_evidence (evidence_id, tenant_id, envelope_id, completion_certificate_ref, completed_artifact_hash, sealed_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING `+evidenceColumns,
			evidenceID, tenantID, envelopeID, req.CompletionCertificateRef, req.CompletedArtifactHash, principalID,
		)
		if err := scanEvidence(row, &out); err != nil {
			if isUniqueViolation(err) {
				return domain.ErrCompletionEvidenceExists
			}
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (p *PgOrchestrationStore) GetCompletionEvidence(ctx context.Context, envelopeID string) (*domain.CompletionEvidence, error) {
	tenantID := middleware.GetTenantID(ctx)
	var out domain.CompletionEvidence
	err := p.withTenant(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+evidenceColumns+` FROM completion_evidence WHERE envelope_id = $1 AND tenant_id = $2`,
			envelopeID, tenantID)
		if err := scanEvidence(row, &out); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errors.New("completion evidence not found")
			}
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}
