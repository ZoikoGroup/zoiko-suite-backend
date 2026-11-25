package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/data-quality-svc/internal/domain"
	"zoiko.io/data-quality-svc/internal/outbox"
)

// DQC-01 contract registry (ZS-SVC-Z-001 section 5). See domain/contract.go for the
// model and the defaults taken where the specification leaves decisions open.

const contractCols = `c.contract_id, c.tenant_id, c.asset_id, a.asset_key, a.asset_kind, c.version_number, c.status,
	c.body, c.content_sha256, c.created_by, c.created_at, c.submitted_by, c.submitted_at, c.published_by, c.published_at,
	c.deprecated_at, c.retired_at, c.lifecycle_note, c.compatibility_evidence, c.compared_to_version`

func scanContract(row pgx.Row) (*domain.ContractVersion, error) {
	var v domain.ContractVersion
	var body []byte
	var status string
	if err := row.Scan(&v.ContractID, &v.TenantID, &v.AssetID, &v.AssetKey, &v.AssetKind, &v.VersionNumber, &status,
		&body, &v.ContentSHA256, &v.CreatedBy, &v.CreatedAt, &v.SubmittedBy, &v.SubmittedAt, &v.PublishedBy, &v.PublishedAt,
		&v.DeprecatedAt, &v.RetiredAt, &v.LifecycleNote, &v.Compatibility, &v.ComparedToVer); err != nil {
		return nil, err
	}
	v.Status = domain.ContractStatus(status)
	if string(v.Compatibility) == "null" {
		v.Compatibility = nil
	}
	if err := json.Unmarshal(body, &v.Body); err != nil {
		return nil, fmt.Errorf("decode contract body: %w", err)
	}
	return &v, nil
}

func loadContract(ctx context.Context, tx pgx.Tx, assetID string, version int, forUpdate bool) (*domain.ContractVersion, error) {
	q := `SELECT ` + contractCols + ` FROM dqc_contract_versions c JOIN dqc_assets a ON a.asset_id = c.asset_id
		WHERE c.asset_id = $1 AND c.version_number = $2`
	if forUpdate {
		q += ` FOR UPDATE OF c`
	}
	v, err := scanContract(tx.QueryRow(ctx, q, assetID, version))
	if errors.Is(err, pgx.ErrNoRows) {
		// Distinguish an unknown asset from an unknown version: DQC-001 vs DQC-002.
		var n int
		if e := tx.QueryRow(ctx, `SELECT count(*) FROM dqc_assets WHERE asset_id = $1`, assetID).Scan(&n); e == nil && n == 0 {
			return nil, domain.ErrContractNotFound
		}
		return nil, domain.ErrContractVersionUnknown
	}
	return v, err
}

func (s *PgStore) emitContract(ctx context.Context, tx pgx.Tx, tenantID, eventType string, v *domain.ContractVersion, extra map[string]any) error {
	payload := map[string]any{
		"contract_id": v.ContractID, "asset_id": v.AssetID, "asset_key": v.AssetKey, "version_number": v.VersionNumber,
		"status": v.Status, "content_sha256": v.ContentSHA256,
	}
	for k, x := range extra {
		payload[k] = x
	}
	return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "dqc_contract_version", AggregateID: v.ContractID,
		EventType: eventType, TenantID: &tenantID, Payload: payload})
}

// CreateContract creates a DRAFT version, creating the asset the first time its key is seen.
func (s *PgStore) CreateContract(ctx context.Context, tenantID string, req domain.CreateContractRequest, actor string, claim domain.IdempotencyClaim) (*domain.ContractVersion, error) {
	if err := domain.ValidateAssetIdentity(req.AssetKey, req.AssetKind); err != nil {
		return nil, err
	}
	body := req.Body
	body.Normalize()
	if err := body.Validate(); err != nil {
		return nil, err
	}
	var out *domain.ContractVersion
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		assetID := newID(domain.PrefixAsset)
		tag, err := tx.Exec(ctx, `INSERT INTO dqc_assets (asset_id, tenant_id, asset_key, asset_kind, created_by)
			VALUES ($1, $2, $3, $4, $5) ON CONFLICT (tenant_id, asset_key) DO NOTHING`,
			assetID, tenantID, req.AssetKey, req.AssetKind, actor)
		if err != nil {
			return fmt.Errorf("find-or-create asset: %w", err)
		}
		if tag.RowsAffected() == 0 {
			var kind string
			if err := tx.QueryRow(ctx, `SELECT asset_id, asset_kind FROM dqc_assets WHERE tenant_id = $1 AND asset_key = $2`,
				tenantID, req.AssetKey).Scan(&assetID, &kind); err != nil {
				return fmt.Errorf("look up asset: %w", err)
			}
			if kind != req.AssetKind {
				return domain.ErrContractAssetKindClash
			}
		}
		contractID := newID(domain.PrefixContract)
		claim.ResourceID = contractID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		var next int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version_number), 0) + 1 FROM dqc_contract_versions WHERE asset_id = $1`, assetID).Scan(&next); err != nil {
			return fmt.Errorf("next version: %w", err)
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO dqc_contract_versions (contract_id, tenant_id, asset_id, version_number, status, body, content_sha256, created_by)
			VALUES ($1, $2, $3, $4, 'DRAFT', $5::jsonb, $6, $7)`,
			contractID, tenantID, assetID, next, raw, body.ContentHash(), actor); err != nil {
			var pg *pgconn.PgError
			if errors.As(err, &pg) && pg.ConstraintName == "dqc_one_open_draft" {
				return domain.ErrContractDraftOpen
			}
			return fmt.Errorf("insert contract version: %w", err)
		}
		v, err := loadContract(ctx, tx, assetID, next, false)
		if err != nil {
			return err
		}
		out = v
		return s.emitContract(ctx, tx, tenantID, "dqc.contract.created", v, nil)
	})
	return out, err
}

// SubmitContract moves a DRAFT to REVIEW. The submitter is recorded: they, like the author,
// cannot be the one who publishes.
func (s *PgStore) SubmitContract(ctx context.Context, tenantID, assetID string, version int, actor string, claim domain.IdempotencyClaim) (*domain.ContractVersion, error) {
	var out *domain.ContractVersion
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		v, err := loadContract(ctx, tx, assetID, version, true)
		if err != nil {
			return err
		}
		claim.ResourceID = v.ContractID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		if v.Status != domain.ContractDraft {
			return fmt.Errorf("%w: %s is %s, only a DRAFT can be submitted", domain.ErrContractTransition, v.ContractID, v.Status)
		}
		if _, err := tx.Exec(ctx, `UPDATE dqc_contract_versions SET status = 'REVIEW', submitted_by = $2, submitted_at = now() WHERE contract_id = $1`,
			v.ContractID, actor); err != nil {
			return fmt.Errorf("submit: %w", err)
		}
		out, err = loadContract(ctx, tx, assetID, version, false)
		if err != nil {
			return err
		}
		return s.emitContract(ctx, tx, tenantID, "dqc.contract.submitted", out, nil)
	})
	return out, err
}

// PublishContract publishes a REVIEW version. It refuses (a) a publisher who authored or submitted
// it, and (b) a breaking change against the most recent PUBLISHED version unless the declared
// compatibility is NONE and the breaking change is explicitly acknowledged. The compatibility
// evidence is stored with the version.
func (s *PgStore) PublishContract(ctx context.Context, tenantID, assetID string, version int, actor string, req domain.PublishContractRequest, claim domain.IdempotencyClaim) (*domain.ContractVersion, error) {
	var out *domain.ContractVersion
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		v, err := loadContract(ctx, tx, assetID, version, true)
		if err != nil {
			return err
		}
		claim.ResourceID = v.ContractID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		if v.Status != domain.ContractReview {
			return fmt.Errorf("%w: %s is %s, only a version in REVIEW can be published", domain.ErrContractTransition, v.ContractID, v.Status)
		}
		if actor == v.CreatedBy || (v.SubmittedBy != nil && actor == *v.SubmittedBy) {
			return domain.ErrContractNotIndependent
		}

		var evidence []byte
		var comparedTo *int
		var base *domain.ContractVersion
		var baseNo int
		err = tx.QueryRow(ctx, `SELECT version_number FROM dqc_contract_versions
			WHERE asset_id = $1 AND status = 'PUBLISHED' AND version_number < $2 ORDER BY version_number DESC LIMIT 1`,
			assetID, version).Scan(&baseNo)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// First published version: nothing to be compatible with.
		case err != nil:
			return fmt.Errorf("find base version: %w", err)
		default:
			if base, err = loadContract(ctx, tx, assetID, baseNo, false); err != nil {
				return err
			}
			rep := domain.CheckCompatibility(base.Body, v.Body, baseNo)
			if evidence, err = json.Marshal(rep); err != nil {
				return err
			}
			comparedTo = &baseNo
			if !rep.Compatible {
				return &IncompatibleError{Report: rep}
			}
			if rep.IsBreaking() && !req.AcknowledgeBreaking { // only reachable under NONE
				return domain.ErrBreakingNotAcknowledged
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE dqc_contract_versions SET status = 'PUBLISHED', published_by = $2, published_at = now(),
			compatibility_evidence = $3::jsonb, compared_to_version = $4 WHERE contract_id = $1`,
			v.ContractID, actor, evidence, comparedTo); err != nil {
			return fmt.Errorf("publish: %w", err)
		}
		out, err = loadContract(ctx, tx, assetID, version, false)
		if err != nil {
			return err
		}
		return s.emitContract(ctx, tx, tenantID, "dqc.contract.published", out, map[string]any{"compared_to_version": comparedTo})
	})
	return out, err
}

// IncompatibleError carries the breaking-change evidence (DQC-003).
type IncompatibleError struct{ Report domain.CompatibilityReport }

func (e *IncompatibleError) Error() string { return domain.ErrSchemaIncompatible.Error() }
func (e *IncompatibleError) Unwrap() error { return domain.ErrSchemaIncompatible }

// DeprecateContract: PUBLISHED -> DEPRECATED. Existing consumers keep resolving the exact version.
func (s *PgStore) DeprecateContract(ctx context.Context, tenantID, assetID string, version int, actor, note string, claim domain.IdempotencyClaim) (*domain.ContractVersion, error) {
	return s.endOfLife(ctx, tenantID, assetID, version, actor, note, claim, "dqc.contract.deprecated",
		func(v *domain.ContractVersion) (string, error) {
			if v.Status != domain.ContractPublished {
				return "", fmt.Errorf("%w: only a PUBLISHED version can be deprecated (is %s)", domain.ErrContractTransition, v.Status)
			}
			return `UPDATE dqc_contract_versions SET status = 'DEPRECATED', deprecated_at = now(), lifecycle_note = $2 WHERE contract_id = $1`, nil
		})
}

// RetireContract: DEPRECATED -> RETIRED, or a never-published DRAFT/REVIEW -> RETIRED (withdrawn).
// A PUBLISHED version must be deprecated first. Nothing is deleted.
func (s *PgStore) RetireContract(ctx context.Context, tenantID, assetID string, version int, actor, note string, claim domain.IdempotencyClaim) (*domain.ContractVersion, error) {
	return s.endOfLife(ctx, tenantID, assetID, version, actor, note, claim, "dqc.contract.retired",
		func(v *domain.ContractVersion) (string, error) {
			if v.Status == domain.ContractPublished || v.Status == domain.ContractRetired {
				return "", fmt.Errorf("%w: a %s version cannot be retired directly", domain.ErrContractTransition, v.Status)
			}
			return `UPDATE dqc_contract_versions SET status = 'RETIRED', retired_at = now(), lifecycle_note = $2 WHERE contract_id = $1`, nil
		})
}

func (s *PgStore) endOfLife(ctx context.Context, tenantID, assetID string, version int, actor, note string, claim domain.IdempotencyClaim,
	event string, plan func(*domain.ContractVersion) (string, error)) (*domain.ContractVersion, error) {
	if note == "" {
		return nil, fmt.Errorf("%w: a reason is required", domain.ErrContractInvalid)
	}
	var out *domain.ContractVersion
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		v, err := loadContract(ctx, tx, assetID, version, true)
		if err != nil {
			return err
		}
		claim.ResourceID = v.ContractID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		stmt, err := plan(v)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, stmt, v.ContractID, note); err != nil {
			return fmt.Errorf("lifecycle update: %w", err)
		}
		out, err = loadContract(ctx, tx, assetID, version, false)
		if err != nil {
			return err
		}
		return s.emitContract(ctx, tx, tenantID, event, out, map[string]any{"reason": note, "actor": actor})
	})
	return out, err
}

// GetContractVersion reads an EXACT version. There is deliberately no "latest" read: a
// historical version never resolves through a moving pointer.
func (s *PgStore) GetContractVersion(ctx context.Context, tenantID, assetID string, version int) (*domain.ContractVersion, error) {
	var out *domain.ContractVersion
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		v, err := loadContract(ctx, tx, assetID, version, false)
		out = v
		return err
	})
	return out, err
}

// ListContractVersions lists an asset's versions (ascending). It returns every version with its
// status; it does not nominate one.
func (s *PgStore) ListContractVersions(ctx context.Context, tenantID, assetID string) ([]domain.ContractVersion, error) {
	out := []domain.ContractVersion{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM dqc_assets WHERE asset_id = $1`, assetID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return domain.ErrContractNotFound
		}
		rows, err := tx.Query(ctx, `SELECT `+contractCols+` FROM dqc_contract_versions c JOIN dqc_assets a ON a.asset_id = c.asset_id
			WHERE c.asset_id = $1 ORDER BY c.version_number`, assetID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			v, err := scanContract(rows)
			if err != nil {
				return err
			}
			out = append(out, *v)
		}
		return rows.Err()
	})
	return out, err
}

// CheckContractCompatibility is a dry run: it compares a candidate body with an EXACT earlier version
// and returns the breaking-change evidence. It writes nothing.
func (s *PgStore) CheckContractCompatibility(ctx context.Context, tenantID, assetID string, req domain.CompatibilityCheckRequest) (*domain.CompatibilityReport, error) {
	if req.AgainstVersion < 1 {
		return nil, fmt.Errorf("%w: against_version is required; a comparison never resolves the latest", domain.ErrContractInvalid)
	}
	cand := req.Candidate
	cand.Normalize()
	if err := cand.Validate(); err != nil {
		return nil, err
	}
	var rep domain.CompatibilityReport
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		base, err := loadContract(ctx, tx, assetID, req.AgainstVersion, false)
		if err != nil {
			return err
		}
		if base.Status != domain.ContractPublished && base.Status != domain.ContractDeprecated {
			return domain.ErrContractNotPublishedBase
		}
		rep = domain.CheckCompatibility(base.Body, cand, req.AgainstVersion)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &rep, nil
}
