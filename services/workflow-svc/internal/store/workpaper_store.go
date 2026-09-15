package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/workflow-svc/internal/domain"
)

const workpaperColumns = `workpaper_id, engagement_id, tenant_id, reference, purpose, status, required, has_contradiction_flag,
	prepared_by_principal_id, prepared_at, reviewed_at, lock_digest, locked_by_principal_id, locked_at, created_by_principal_id, created_at`

func scanWorkpaper(row pgx.Row) (*domain.Workpaper, error) {
	w := &domain.Workpaper{}
	err := row.Scan(&w.WorkpaperID, &w.EngagementID, &w.TenantID, &w.Reference, &w.Purpose, &w.Status, &w.Required, &w.HasContradictionFlag,
		&w.PreparedByPrincipalID, &w.PreparedAt, &w.ReviewedAt, &w.LockDigest, &w.LockedByPrincipalID, &w.LockedAt, &w.CreatedByPrincipalID, &w.CreatedAt)
	return w, err
}

func (s *PgStore) CreateWorkpaper(ctx context.Context, p domain.CreateWorkpaperParams) (*domain.Workpaper, bool, error) {
	if p.CorrelationID == "" {
		return nil, false, fmt.Errorf("%w: correlation id is required", domain.ErrStoreUnavailable)
	}
	if p.Reference == "" {
		return nil, false, fmt.Errorf("%w: reference is required", domain.ErrStoreUnavailable)
	}
	var out *domain.Workpaper
	created := false
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `INSERT INTO workpapers (workpaper_id, engagement_id, tenant_id, reference, purpose, required, created_by_principal_id, correlation_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING RETURNING `+workpaperColumns,
			uuid.NewString(), p.EngagementID, p.TenantID, p.Reference, p.Purpose, p.Required, p.CreatedByPrincipalID, p.CorrelationID)
		var err error
		out, err = scanWorkpaper(row)
		if err == nil {
			created = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		out, err = scanWorkpaper(tx.QueryRow(ctx, `SELECT `+workpaperColumns+` FROM workpapers WHERE tenant_id=$1 AND correlation_id=$2`, p.TenantID, p.CorrelationID))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: reference %q already exists for this engagement", domain.ErrWorkpaperInvalidState, p.Reference)
		}
		return err
	})
	if err != nil {
		if errors.Is(err, domain.ErrWorkpaperInvalidState) {
			return nil, false, err
		}
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, created, nil
}

func (s *PgStore) GetWorkpaper(ctx context.Context, tenantID, workpaperID string) (*domain.Workpaper, error) {
	var out *domain.Workpaper
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = scanWorkpaper(tx.QueryRow(ctx, `SELECT `+workpaperColumns+` FROM workpapers WHERE workpaper_id=$1`, workpaperID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrWorkpaperNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) ListWorkpapersByEngagement(ctx context.Context, tenantID, engagementID string) ([]*domain.Workpaper, error) {
	var out []*domain.Workpaper
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+workpaperColumns+` FROM workpapers WHERE engagement_id=$1 ORDER BY created_at`, engagementID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			w, err := scanWorkpaper(rows)
			if err != nil {
				return err
			}
			out = append(out, w)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) RecordProcedure(ctx context.Context, p domain.RecordProcedureParams) error {
	if p.Description == "" {
		return fmt.Errorf("%w: description is required", domain.ErrStoreUnavailable)
	}
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		if err := requireUnlockedWorkpaper(ctx, tx, p.WorkpaperID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO workpaper_procedures (procedure_id, workpaper_id, tenant_id, description) VALUES ($1,$2,$3,$4)`,
			uuid.NewString(), p.WorkpaperID, p.TenantID, p.Description)
		return err
	})
	if errors.Is(err, domain.ErrWorkpaperNotFound) || errors.Is(err, domain.ErrWorkpaperInvalidState) {
		return err
	}
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

func (s *PgStore) RecordResult(ctx context.Context, p domain.RecordResultParams) error {
	if p.Description == "" {
		return fmt.Errorf("%w: description is required", domain.ErrStoreUnavailable)
	}
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		if err := requireUnlockedWorkpaper(ctx, tx, p.WorkpaperID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO workpaper_results (result_id, workpaper_id, tenant_id, description) VALUES ($1,$2,$3,$4)`,
			uuid.NewString(), p.WorkpaperID, p.TenantID, p.Description)
		return err
	})
	if errors.Is(err, domain.ErrWorkpaperNotFound) || errors.Is(err, domain.ErrWorkpaperInvalidState) {
		return err
	}
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

func (s *PgStore) RecordConclusion(ctx context.Context, p domain.RecordConclusionParams) error {
	if p.Description == "" {
		return fmt.Errorf("%w: description is required", domain.ErrStoreUnavailable)
	}
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		if err := requireUnlockedWorkpaper(ctx, tx, p.WorkpaperID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO workpaper_conclusions (conclusion_id, workpaper_id, tenant_id, description) VALUES ($1,$2,$3,$4)`,
			uuid.NewString(), p.WorkpaperID, p.TenantID, p.Description)
		return err
	})
	if errors.Is(err, domain.ErrWorkpaperNotFound) || errors.Is(err, domain.ErrWorkpaperInvalidState) {
		return err
	}
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

func (s *PgStore) AddWorkpaperCrossReference(ctx context.Context, p domain.AddWorkpaperCrossReferenceParams) error {
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO workpaper_cross_references (cross_reference_id, from_workpaper_id, to_workpaper_id, tenant_id) VALUES ($1,$2,$3,$4)`,
			uuid.NewString(), p.FromWorkpaperID, p.ToWorkpaperID, p.TenantID)
		return err
	})
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

// LinkWorkpaperEvidence surfaces a contradiction flag rather than
// dropping it: "contradictory evidence linked" — has_contradiction_flag
// is copied onto the workpaper itself so GetWorkpaper always shows it.
func (s *PgStore) LinkWorkpaperEvidence(ctx context.Context, p domain.LinkWorkpaperEvidenceParams) error {
	if p.EvidenceID == "" {
		return fmt.Errorf("%w: evidence_id is required", domain.ErrStoreUnavailable)
	}
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		if err := requireUnlockedWorkpaper(ctx, tx, p.WorkpaperID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO workpaper_evidence_links (link_id, workpaper_id, tenant_id, evidence_id, contradiction_flag, linked_by_principal_id)
			VALUES ($1,$2,$3,$4,$5,$6)`, uuid.NewString(), p.WorkpaperID, p.TenantID, p.EvidenceID, p.ContradictionFlag, p.LinkedByPrincipalID); err != nil {
			return err
		}
		if p.ContradictionFlag {
			if _, err := tx.Exec(ctx, `UPDATE workpapers SET has_contradiction_flag=true WHERE workpaper_id=$1`, p.WorkpaperID); err != nil {
				return err
			}
		}
		return nil
	})
	if errors.Is(err, domain.ErrWorkpaperNotFound) || errors.Is(err, domain.ErrWorkpaperInvalidState) {
		return err
	}
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

// MarkWorkpaperPrepared is DRAFT/IN_PROGRESS -> PREPARED, requiring a
// preparer to actually be attributed — "preparer/date captured."
func (s *PgStore) MarkWorkpaperPrepared(ctx context.Context, p domain.MarkWorkpaperPreparedParams) (*domain.Workpaper, bool, error) {
	if p.CorrelationID == "" {
		return nil, false, fmt.Errorf("%w: correlation id is required", domain.ErrStoreUnavailable)
	}
	var out *domain.Workpaper
	changed := false
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		row := tx.QueryRow(ctx, `UPDATE workpapers SET status=$1, prepared_by_principal_id=$2, prepared_at=$3
			WHERE workpaper_id=$4 AND status IN ('DRAFT','IN_PROGRESS') RETURNING `+workpaperColumns,
			domain.WorkpaperPrepared, p.ActorPrincipalID, now, p.WorkpaperID)
		var err error
		out, err = scanWorkpaper(row)
		if errors.Is(err, pgx.ErrNoRows) {
			var exists bool
			if chkErr := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workpapers WHERE workpaper_id=$1)`, p.WorkpaperID).Scan(&exists); chkErr != nil {
				return chkErr
			}
			if !exists {
				return domain.ErrWorkpaperNotFound
			}
			return domain.ErrWorkpaperInvalidState
		}
		if err != nil {
			return err
		}
		changed = true
		return nil
	})
	if errors.Is(err, domain.ErrWorkpaperNotFound) || errors.Is(err, domain.ErrWorkpaperInvalidState) {
		return nil, false, err
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, changed, nil
}

// LockWorkpaper is the real enforcement of "purpose/procedure/result/
// conclusion mandatory" (the CAS predicate below) and "lock digest
// generated" (a real SHA-256 over the canonically-ordered content,
// computed server-side, never a random UUID stand-in). PREPARED or
// REVIEWED may lock — this build has no dedicated command that reaches a
// standalone REVIEWED workpaper state independently of AUD-09's own
// review/sign-off flow, so both are accepted here, the same doc-vs-command
// collapse used throughout this build.
func (s *PgStore) LockWorkpaper(ctx context.Context, p domain.LockWorkpaperParams) (*domain.Workpaper, bool, error) {
	if p.CorrelationID == "" {
		return nil, false, fmt.Errorf("%w: correlation id is required", domain.ErrStoreUnavailable)
	}
	var out *domain.Workpaper
	changed := false
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		wp, err := scanWorkpaper(tx.QueryRow(ctx, `SELECT `+workpaperColumns+` FROM workpapers WHERE workpaper_id=$1 FOR UPDATE`, p.WorkpaperID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrWorkpaperNotFound
		}
		if err != nil {
			return err
		}
		if wp.Status == domain.WorkpaperLocked {
			out = wp
			return nil
		}
		if wp.Status != domain.WorkpaperPrepared && wp.Status != domain.WorkpaperReviewed {
			return domain.ErrWorkpaperInvalidState
		}
		if wp.Purpose == "" {
			return domain.ErrWorkpaperLockRequiresContent
		}

		procedures, err := fetchDescriptions(ctx, tx, "workpaper_procedures", p.WorkpaperID)
		if err != nil {
			return err
		}
		results, err := fetchDescriptions(ctx, tx, "workpaper_results", p.WorkpaperID)
		if err != nil {
			return err
		}
		conclusions, err := fetchDescriptions(ctx, tx, "workpaper_conclusions", p.WorkpaperID)
		if err != nil {
			return err
		}
		if len(procedures) == 0 || len(results) == 0 || len(conclusions) == 0 {
			return domain.ErrWorkpaperLockRequiresContent
		}

		now := time.Now().UTC()
		digest := computeWorkpaperDigest(wp.Purpose, procedures, results, conclusions, now)
		row := tx.QueryRow(ctx, `UPDATE workpapers SET status=$1, lock_digest=$2, locked_by_principal_id=$3, locked_at=$4
			WHERE workpaper_id=$5 RETURNING `+workpaperColumns,
			domain.WorkpaperLocked, digest, p.ActorPrincipalID, now, p.WorkpaperID)
		out, err = scanWorkpaper(row)
		if err != nil {
			return err
		}
		changed = true
		return nil
	})
	if errors.Is(err, domain.ErrWorkpaperNotFound) || errors.Is(err, domain.ErrWorkpaperInvalidState) || errors.Is(err, domain.ErrWorkpaperLockRequiresContent) {
		return nil, false, err
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, changed, nil
}

func computeWorkpaperDigest(purpose string, procedures, results, conclusions []string, at time.Time) string {
	h := sha256.New()
	h.Write([]byte(purpose))
	for _, p := range procedures {
		h.Write([]byte("|proc:" + p))
	}
	for _, r := range results {
		h.Write([]byte("|res:" + r))
	}
	for _, c := range conclusions {
		h.Write([]byte("|concl:" + c))
	}
	h.Write([]byte("|at:" + at.Format(time.RFC3339Nano)))
	return hex.EncodeToString(h.Sum(nil))
}

func fetchDescriptions(ctx context.Context, tx pgx.Tx, table, workpaperID string) ([]string, error) {
	rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT description FROM %s WHERE workpaper_id=$1 ORDER BY created_at`, table), workpaperID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	sort.Strings(out) // canonical order — independent of any race in created_at ties
	return out, rows.Err()
}

// AddPostLockAddendum is the ONLY way to add content once a workpaper is
// LOCKED. reason/effect are required non-empty — "post-lock additions
// identify who/when/why/effect."
func (s *PgStore) AddPostLockAddendum(ctx context.Context, p domain.AddPostLockAddendumParams) (*domain.WorkpaperAddendum, error) {
	if strings.TrimSpace(p.Reason) == "" || strings.TrimSpace(p.Effect) == "" {
		return nil, domain.ErrWorkpaperAddendumIncomplete
	}
	var out *domain.WorkpaperAddendum
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM workpapers WHERE workpaper_id=$1`, p.WorkpaperID).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrWorkpaperNotFound
			}
			return err
		}
		if status != domain.WorkpaperLocked {
			return domain.ErrWorkpaperInvalidState
		}
		row := tx.QueryRow(ctx, `INSERT INTO workpaper_addenda (addendum_id, workpaper_id, tenant_id, added_by_principal_id, reason, effect, content)
			VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING addendum_id, workpaper_id, tenant_id, added_by_principal_id, reason, effect, content, added_at`,
			uuid.NewString(), p.WorkpaperID, p.TenantID, p.ActorPrincipalID, p.Reason, p.Effect, p.Content)
		a := &domain.WorkpaperAddendum{}
		if err := row.Scan(&a.AddendumID, &a.WorkpaperID, &a.TenantID, &a.AddedByPrincipalID, &a.Reason, &a.Effect, &a.Content, &a.AddedAt); err != nil {
			return err
		}
		out = a
		return nil
	})
	if errors.Is(err, domain.ErrWorkpaperNotFound) || errors.Is(err, domain.ErrWorkpaperInvalidState) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func requireUnlockedWorkpaper(ctx context.Context, tx pgx.Tx, workpaperID string) error {
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM workpapers WHERE workpaper_id=$1`, workpaperID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrWorkpaperNotFound
		}
		return err
	}
	if status == domain.WorkpaperLocked {
		return domain.ErrWorkpaperInvalidState
	}
	return nil
}

// GetAuditEngagementRequiredWorkpapersLocked is AUD-01's own real
// completion-gate query for the FIELDWORK stage's own workpaper
// requirement: no required workpaper may remain unlocked.
func (s *PgStore) GetAuditEngagementRequiredWorkpapersLocked(ctx context.Context, tenantID, engagementID string) (bool, error) {
	var unlockedCount int
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM workpapers WHERE engagement_id=$1 AND required=true AND status<>$2`,
			engagementID, domain.WorkpaperLocked).Scan(&unlockedCount)
	})
	if err != nil {
		return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return unlockedCount == 0, nil
}
