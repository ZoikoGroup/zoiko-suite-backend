package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/reconciliation-engine-svc/internal/domain"
	"zoiko.io/reconciliation-engine-svc/internal/outbox"
)

// ── RevokeCertification ─────────────────────────────────────────────────────

const revocationColumns = `revocation_id, tenant_id, run_id, certification_id, revoked_manifest_sha256,
	certification_transition_id, certified_by, certified_at, reason, evidence_refs, authority_snapshot, revoked_by, revoked_at`

func scanRevocation(row pgx.Row) (*domain.CertificationRevocation, error) {
	var v domain.CertificationRevocation
	var authority []byte
	if err := row.Scan(&v.RevocationID, &v.TenantID, &v.RunID, &v.CertificationID, &v.RevokedManifestSHA256,
		&v.CertificationTransitionID, &v.CertifiedBy, &v.CertifiedAt, &v.Reason, &v.EvidenceRefs, &authority,
		&v.RevokedBy, &v.RevokedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(authority, &v.Authority); err != nil {
		return nil, fmt.Errorf("decode revocation authority: %w", err)
	}
	return &v, nil
}

// RevokeCertification withdraws a Certified run's certification (A12/B34;
// ZS-SVC-Z-001 DQC-05, RB-05; ZS-CONTROL-001 §27), atomically with its
// revocation record, its transition and its outbox event. Only the
// certification dimension moves, CERTIFIED -> REVOKED, through advanceRun
// like every other state change, so the move is validated and appended to
// reconciliation_run_transitions exactly as Certify's and Reperform's are. The
// status stays Certified and the result is untouched.
//
// Additive only ("prior certification retained"): the certifications row —
// its manifest hash and the populations, manual matches and authority A12
// bound into it — and the CERTIFICATION -> CERTIFIED transition are never
// touched (both tables are immutable by trigger); the new
// reconciliation_certification_revocations row references them.
//
// Independence: no principal recorded as certifying the run can revoke it.
//
// Prospective (RB-05): nothing already decided on the certification is undone
// here; CONTROL.CertificationRevoked tells consumers it is withdrawn from
// revoked_at on.
//
// Idempotent under the run's lock: a retry with the same Idempotency-Key and
// request replays (IdempotentReplayError) rather than being refused as already
// REVOKED; the same key with another request is ErrIdempotencyKeyReused. The
// key is claimed only once every check has passed, so a refused revocation
// never consumes it.
func (s *PgStore) RevokeCertification(ctx context.Context, tenantID, runID string, req domain.RevokeCertificationRequest,
	actor string, authority *domain.AuthoritySnapshot, claim domain.IdempotencyClaim) (*domain.CertificationRevocation, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if !authority.Traceable(actor) {
		return nil, domain.ErrRevokerAuthorityUntraceable
	}
	authorityJSON, err := json.Marshal(authority)
	if err != nil {
		return nil, fmt.Errorf("encode revocation authority: %w", err)
	}
	var out *domain.CertificationRevocation
	err = s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		run, err := loadRunForUpdate(ctx, tx, tenantID, runID)
		if err != nil {
			return err
		}
		claim.ResourceID = runID
		if err := peekIdempotency(ctx, tx, tenantID, claim); err != nil {
			return err
		}
		if err := domain.ValidateCertificationRevocation(run.Status, run.CertificationState); err != nil {
			return err
		}

		// The certification being revoked, and everyone who certified the run:
		// every CERTIFICATION -> CERTIFIED transition's actor and the sealer.
		var certID, manifest, sealedBy string
		var sealedAt time.Time
		err = tx.QueryRow(ctx, `SELECT certification_id, manifest_sha256, sealed_by, sealed_at FROM certifications
			WHERE tenant_id = $1 AND run_id = $2`, tenantID, runID).Scan(&certID, &manifest, &sealedBy, &sealedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			// A Certified run with no sealed certification would be a defect in
			// this service's own history; revoking it would revoke nothing traceable.
			return fmt.Errorf("run %s is Certified but has no sealed certification", runID)
		}
		if err != nil {
			return fmt.Errorf("load certification: %w", err)
		}
		rows, err := tx.Query(ctx, `SELECT transition_id, actor FROM reconciliation_run_transitions
			WHERE tenant_id = $1 AND run_id = $2 AND dimension = 'CERTIFICATION' AND to_state = $3
			ORDER BY transition_id`, tenantID, runID, string(domain.CertCertified))
		if err != nil {
			return fmt.Errorf("load certification transitions: %w", err)
		}
		certifiers := []string{sealedBy}
		if run.CertifiedBy != "" {
			certifiers = append(certifiers, run.CertifiedBy)
		}
		var transitionID int64
		for rows.Next() {
			var a string
			if err := rows.Scan(&transitionID, &a); err != nil {
				rows.Close()
				return err
			}
			certifiers = append(certifiers, a)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if transitionID == 0 {
			return fmt.Errorf("run %s is Certified but has no recorded certification transition", runID)
		}
		if err := domain.CheckRevokerIndependence(actor, certifiers); err != nil {
			return err
		}

		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		// The record first: the run's lifecycle trigger refuses REVOKED without it.
		v, err := scanRevocation(tx.QueryRow(ctx, `
			INSERT INTO reconciliation_certification_revocations (revocation_id, tenant_id, run_id, certification_id,
				revoked_manifest_sha256, certification_transition_id, certified_by, certified_at, reason, evidence_refs,
				authority_snapshot, revoked_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			RETURNING `+revocationColumns,
			newID(domain.PrefixRevocation), tenantID, runID, certID, manifest, transitionID, sealedBy, sealedAt,
			req.Reason, req.EvidenceRefs, authorityJSON, actor))
		if err != nil {
			return fmt.Errorf("record revocation: %w", err)
		}
		if _, err := advanceRun(ctx, tx, tenantID, run, runAdvance{To: domain.RunCertified, Result: run.ResultState,
			Cert: domain.CertRevoked, Reason: "certification revoked: " + req.Reason, Actor: actor,
			AcceptedRiskCertifiable: run.ResultState == domain.ResultVariance}); err != nil {
			return err
		}

		out = v
		// DQC-05's dqc.certification.revoked: the prior certification, the
		// reason and the effective time (revoked_at), plus the evidence.
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "certification", AggregateID: certID,
			EventType: "CONTROL.CertificationRevoked", TenantID: &tenantID, Payload: v})
	})
	return out, err
}

// runRevocation is the run's revocation, or nil when its certification has
// not been revoked.
func runRevocation(ctx context.Context, tx pgx.Tx, tenantID, runID string) (*domain.CertificationRevocation, error) {
	v, err := scanRevocation(tx.QueryRow(ctx, `SELECT `+revocationColumns+` FROM reconciliation_certification_revocations
		WHERE tenant_id = $1 AND run_id = $2`, tenantID, runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load revocation: %w", err)
	}
	return v, nil
}
