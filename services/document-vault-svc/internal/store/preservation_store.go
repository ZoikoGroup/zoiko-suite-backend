package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"zoiko.io/document-vault-svc/internal/domain"
)

// ── Renditions ───────────────────────────────────────────────────────────────

const renditionColumns = `
	rendition_id, tenant_id, source_document_version_id, parent_rendition_id, rendition_class,
	transformation_profile, checksum_sha256, storage_key, size_bytes, content_type,
	created_by_principal_id, created_at
`

func scanRendition(row pgx.Row, rd *domain.Rendition) error {
	return row.Scan(&rd.RenditionID, &rd.TenantID, &rd.SourceDocumentVersionID, &rd.ParentRenditionID, &rd.RenditionClass,
		&rd.TransformationProfile, &rd.ChecksumSHA256, &rd.StorageKey, &rd.SizeBytes, &rd.ContentType,
		&rd.CreatedByPrincipalID, &rd.CreatedAt)
}

// CreateRendition inserts a new derivative content object and, in the
// same transaction, seals its fixity_manifest with integrity_state
// VERIFIED — the hash was just computed from the bytes that were
// written, so it is trustworthy at this instant (DRC-I20: every
// rendition gets a fixity manifest).
func (s *PgStore) CreateRendition(ctx context.Context, p domain.CreateRenditionParams) (*domain.Rendition, error) {
	if !domain.RenditionClass(p.RenditionClass).Valid() {
		return nil, domain.ErrInvalidRenditionClass
	}
	if len(p.ChecksumSHA256) != 64 {
		return nil, fmt.Errorf("checksum_sha256 must be a 64-character hex SHA-256 digest")
	}
	if p.StorageKey == "" || p.ContentType == "" || p.TransformationProfile == "" {
		return nil, fmt.Errorf("storage_key, content_type and transformation_profile are required")
	}

	var out domain.Rendition
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		var sourceExists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(
			SELECT 1 FROM document_versions dv JOIN documents d ON d.document_id = dv.document_id
			WHERE dv.document_version_id = $1 AND d.tenant_id::text = $2)`,
			p.SourceDocumentVersionID, tenantID).Scan(&sourceExists); err != nil {
			return fmt.Errorf("document store unavailable: %w", err)
		}
		if !sourceExists {
			return domain.ErrSourceDocumentVersionNotFound
		}

		if p.ParentRenditionID != nil {
			var parentExists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM renditions WHERE rendition_id = $1 AND tenant_id::text = $2)`,
				*p.ParentRenditionID, tenantID).Scan(&parentExists); err != nil {
				return fmt.Errorf("document store unavailable: %w", err)
			}
			if !parentExists {
				return domain.ErrParentRenditionNotFound
			}
		}

		row := tx.QueryRow(ctx, `
			INSERT INTO renditions (tenant_id, source_document_version_id, parent_rendition_id, rendition_class,
				transformation_profile, checksum_sha256, storage_key, size_bytes, content_type, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			RETURNING `+renditionColumns,
			tenantID, p.SourceDocumentVersionID, p.ParentRenditionID, p.RenditionClass,
			p.TransformationProfile, p.ChecksumSHA256, p.StorageKey, p.SizeBytes, p.ContentType, p.CreatedByPrincipalID,
		)
		if err := scanRendition(row, &out); err != nil {
			return fmt.Errorf("document store unavailable: %w", mapPgError(err))
		}

		if _, err := insertFixityManifest(ctx, tx, tenantID, nil, &out.RenditionID, p.ChecksumSHA256, p.CreatedByPrincipalID); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *PgStore) GetRendition(ctx context.Context, renditionID string) (*domain.Rendition, error) {
	var out domain.Rendition
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		row := tx.QueryRow(ctx, `SELECT `+renditionColumns+` FROM renditions WHERE rendition_id = $1 AND tenant_id::text = $2`,
			renditionID, tenantID)
		if err := scanRendition(row, &out); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrRenditionNotFound
			}
			return fmt.Errorf("document store unavailable: %w", mapPgError(err))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ── Fixity Manifests ─────────────────────────────────────────────────────────

const fixityManifestColumns = `
	manifest_id, tenant_id, document_version_id, rendition_id, source_hash_algorithm, source_hash,
	integrity_state, generated_at, generated_by_principal_id, last_verified_at, last_verified_hash,
	repair_source_ref, repair_started_at, repaired_by_principal_id
`

func scanFixityManifest(row pgx.Row, m *domain.FixityManifest) error {
	return row.Scan(&m.ManifestID, &m.TenantID, &m.DocumentVersionID, &m.RenditionID, &m.SourceHashAlgorithm, &m.SourceHash,
		&m.IntegrityState, &m.GeneratedAt, &m.GeneratedByPrincipalID, &m.LastVerifiedAt, &m.LastVerifiedHash,
		&m.RepairSourceRef, &m.RepairStartedAt, &m.RepairedByPrincipalID)
}

func insertFixityManifest(ctx context.Context, tx pgx.Tx, tenantID string, documentVersionID, renditionID *string, sourceHash, principalID string) (*domain.FixityManifest, error) {
	var out domain.FixityManifest
	row := tx.QueryRow(ctx, `
		INSERT INTO fixity_manifests (tenant_id, document_version_id, rendition_id, source_hash, generated_by_principal_id)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+fixityManifestColumns,
		tenantID, documentVersionID, renditionID, sourceHash, principalID,
	)
	if err := scanFixityManifest(row, &out); err != nil {
		return nil, fmt.Errorf("document store unavailable: %w", mapPgError(err))
	}
	return &out, nil
}

// CreateFixityManifestForVersion seals a fixity manifest for an
// ORIGINAL document_version — DRC-I20 applies to originals too, not
// only renditions, but the original's own ingestion (DRC-01) predates
// this wave and never created one, so this is an explicit, separate
// command rather than an automatic side effect of document upload.
func (s *PgStore) CreateFixityManifestForVersion(ctx context.Context, documentVersionID, sourceHash, principalID string) (*domain.FixityManifest, error) {
	if len(sourceHash) != 64 {
		return nil, fmt.Errorf("source_hash must be a 64-character hex SHA-256 digest")
	}
	var out *domain.FixityManifest
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(
			SELECT 1 FROM document_versions dv JOIN documents d ON d.document_id = dv.document_id
			WHERE dv.document_version_id = $1 AND d.tenant_id::text = $2)`,
			documentVersionID, tenantID).Scan(&exists); err != nil {
			return fmt.Errorf("document store unavailable: %w", err)
		}
		if !exists {
			return domain.ErrSourceDocumentVersionNotFound
		}
		m, err := insertFixityManifest(ctx, tx, tenantID, &documentVersionID, nil, sourceHash, principalID)
		if err != nil {
			if isUniqueViolationOn(err, "idx_fixity_manifests_one_per_version") {
				return fmt.Errorf("this document version already has a fixity manifest")
			}
			return err
		}
		out = m
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) GetFixityManifest(ctx context.Context, manifestID string) (*domain.FixityManifest, error) {
	var out domain.FixityManifest
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		row := tx.QueryRow(ctx, `SELECT `+fixityManifestColumns+` FROM fixity_manifests WHERE manifest_id = $1 AND tenant_id::text = $2`,
			manifestID, tenantID)
		if err := scanFixityManifest(row, &out); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrFixityManifestNotFound
			}
			return fmt.Errorf("document store unavailable: %w", mapPgError(err))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *PgStore) GetFixityManifestByRendition(ctx context.Context, renditionID string) (*domain.FixityManifest, error) {
	var out domain.FixityManifest
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		row := tx.QueryRow(ctx, `SELECT `+fixityManifestColumns+` FROM fixity_manifests WHERE rendition_id = $1 AND tenant_id::text = $2`,
			renditionID, tenantID)
		if err := scanFixityManifest(row, &out); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrFixityManifestNotFound
			}
			return fmt.Errorf("document store unavailable: %w", mapPgError(err))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func loadFixityManifestForUpdate(ctx context.Context, tx pgx.Tx, tenantID, id string) (*domain.FixityManifest, error) {
	var m domain.FixityManifest
	row := tx.QueryRow(ctx, `SELECT `+fixityManifestColumns+` FROM fixity_manifests WHERE manifest_id = $1 AND tenant_id::text = $2 FOR UPDATE`,
		id, tenantID)
	if err := scanFixityManifest(row, &m); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrFixityManifestNotFound
		}
		return nil, fmt.Errorf("document store unavailable: %w", mapPgError(err))
	}
	return &m, nil
}

// RecordVerification records the outcome of checking an object's bytes
// against its manifest's source_hash. Allowed only from VERIFIED or
// REPAIRING. A claimed outcome of VERIFIED is only honored if
// ObservedHash actually equals the manifest's own source_hash — the
// store checks this itself rather than trusting the caller's claim,
// which is what makes "never rewrite metadata to fake a hash match" a
// structural guarantee rather than a documented intention.
func (s *PgStore) RecordVerification(ctx context.Context, p domain.RecordVerificationParams) (*domain.FixityManifest, error) {
	claimed := domain.FixityIntegrityState(p.ClaimedOutcome)
	if !claimed.Valid() || claimed == domain.FixityRepairing {
		return nil, domain.ErrFixityInvalidOutcome
	}
	var out domain.FixityManifest
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		m, err := loadFixityManifestForUpdate(ctx, tx, tenantID, p.ManifestID)
		if err != nil {
			return err
		}
		if m.IntegrityState != domain.FixityVerified && m.IntegrityState != domain.FixityRepairing {
			return domain.ErrFixityNotVerifiedOrRepairing
		}

		resolved := claimed
		if claimed == domain.FixityVerified && p.ObservedHash != m.SourceHash {
			// The caller claimed success but the bytes don't actually
			// match the manifest's hash — refuse the claim outright
			// rather than silently downgrading it, so a caller bug
			// can never produce a false VERIFIED.
			return domain.ErrFixityHashMismatchClaim
		}

		var observedHash *string
		if p.ObservedHash != "" {
			h := p.ObservedHash
			observedHash = &h
		}

		wasRepairing := m.IntegrityState == domain.FixityRepairing
		repairSucceeded := wasRepairing && resolved == domain.FixityVerified
		row := tx.QueryRow(ctx, `
			UPDATE fixity_manifests SET integrity_state = $2, last_verified_at = now(), last_verified_hash = $3,
				repaired_by_principal_id = CASE WHEN $4 THEN $5 ELSE repaired_by_principal_id END
			WHERE manifest_id = $1 RETURNING `+fixityManifestColumns,
			p.ManifestID, string(resolved), observedHash, repairSucceeded, p.VerifiedByPrincipalID,
		)
		return scanFixityManifest(row, &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// StartRepair moves a failed manifest (MISMATCH/MISSING/UNREADABLE)
// into REPAIRING, recording where the replacement content is being
// sourced from. The repair only actually counts once a subsequent
// RecordVerification call independently confirms the restored bytes
// hash to the manifest's original source_hash.
func (s *PgStore) StartRepair(ctx context.Context, p domain.StartRepairParams) (*domain.FixityManifest, error) {
	if p.RepairSourceRef == "" {
		return nil, fmt.Errorf("repair_source_ref is required")
	}
	var out domain.FixityManifest
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		m, err := loadFixityManifestForUpdate(ctx, tx, tenantID, p.ManifestID)
		if err != nil {
			return err
		}
		if !m.IntegrityState.IsFailure() {
			return domain.ErrFixityNotFailed
		}
		row := tx.QueryRow(ctx, `
			UPDATE fixity_manifests SET integrity_state = 'REPAIRING', repair_source_ref = $2, repair_started_at = now()
			WHERE manifest_id = $1 RETURNING `+fixityManifestColumns,
			p.ManifestID, p.RepairSourceRef,
		)
		return scanFixityManifest(row, &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ── Redaction Profiles ───────────────────────────────────────────────────────

const redactionProfileColumns = `
	redaction_id, tenant_id, rendition_id, purpose, recipient_class, fields_removed, legal_basis_ref,
	approved_by_principal_id, created_at
`

func scanRedactionProfile(row pgx.Row, rp *domain.RedactionProfile) error {
	return row.Scan(&rp.RedactionID, &rp.TenantID, &rp.RenditionID, &rp.Purpose, &rp.RecipientClass, &rp.FieldsRemoved,
		&rp.LegalBasisRef, &rp.ApprovedByPrincipalID, &rp.CreatedAt)
}

// CreateRedactionProfile records why a REDACTED_RENDITION was created.
// The rendition itself — and the unredacted source underneath it —
// are never touched (DRC-I17).
func (s *PgStore) CreateRedactionProfile(ctx context.Context, p domain.CreateRedactionProfileParams) (*domain.RedactionProfile, error) {
	if p.Purpose == "" || p.RecipientClass == "" {
		return nil, fmt.Errorf("purpose and recipient_class are required")
	}
	var out domain.RedactionProfile
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		var class string
		if err := tx.QueryRow(ctx, `SELECT rendition_class FROM renditions WHERE rendition_id = $1 AND tenant_id::text = $2`,
			p.RenditionID, tenantID).Scan(&class); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrRenditionNotFound
			}
			return fmt.Errorf("document store unavailable: %w", err)
		}
		if domain.RenditionClass(class) != domain.RenditionRedacted {
			return domain.ErrRenditionNotRedactedClass
		}

		fieldsRemoved := p.FieldsRemoved
		if fieldsRemoved == nil {
			fieldsRemoved = []string{}
		}
		row := tx.QueryRow(ctx, `
			INSERT INTO redaction_profiles (tenant_id, rendition_id, purpose, recipient_class, fields_removed, legal_basis_ref, approved_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING `+redactionProfileColumns,
			tenantID, p.RenditionID, p.Purpose, p.RecipientClass, fieldsRemoved, p.LegalBasisRef, p.ApprovedByPrincipalID,
		)
		if err := scanRedactionProfile(row, &out); err != nil {
			if isUniqueViolationOn(err, "redaction_profiles_rendition_id_key") {
				return domain.ErrRedactionProfileExists
			}
			return fmt.Errorf("document store unavailable: %w", mapPgError(err))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *PgStore) GetRedactionProfile(ctx context.Context, redactionID string) (*domain.RedactionProfile, error) {
	var out domain.RedactionProfile
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		row := tx.QueryRow(ctx, `SELECT `+redactionProfileColumns+` FROM redaction_profiles WHERE redaction_id = $1 AND tenant_id::text = $2`,
			redactionID, tenantID)
		if err := scanRedactionProfile(row, &out); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("redaction profile not found")
			}
			return fmt.Errorf("document store unavailable: %w", mapPgError(err))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ── Export Packages ──────────────────────────────────────────────────────────

const exportPackageColumns = `package_id, tenant_id, requested_by_principal_id, package_hash, created_at`

func scanExportPackage(row pgx.Row, p *domain.ExportPackage) error {
	return row.Scan(&p.PackageID, &p.TenantID, &p.RequestedByPrincipalID, &p.PackageHash, &p.CreatedAt)
}

const exportPackageItemColumns = `
	item_id, tenant_id, package_id, item_type, document_version_id, rendition_id, redaction_id,
	included, included_hash, omission_reason, created_at
`

func scanExportPackageItem(row pgx.Row, it *domain.ExportPackageItem) error {
	return row.Scan(&it.ItemID, &it.TenantID, &it.PackageID, &it.ItemType, &it.DocumentVersionID, &it.RenditionID, &it.RedactionID,
		&it.Included, &it.IncludedHash, &it.OmissionReason, &it.CreatedAt)
}

// CreateExportPackage resolves each requested item's current hash
// from the vault itself (never caller-supplied), inserts the package
// and its items in one transaction, and computes package_hash as the
// SHA-256 over the items in a deterministic order — so the package's
// own hash is a real function of exactly what it contains (DRC-I27).
func (s *PgStore) CreateExportPackage(ctx context.Context, p domain.CreateExportPackageParams) (*domain.ExportPackage, error) {
	if len(p.Items) == 0 {
		return nil, domain.ErrNoExportPackageItems
	}
	for _, it := range p.Items {
		if !domain.ExportItemType(it.ItemType).Valid() {
			return nil, domain.ErrInvalidExportItemType
		}
		if it.Omit && it.OmissionReason == "" {
			return nil, domain.ErrExportOmissionReasonRequired
		}
	}

	// resolvedItem carries everything needed to insert one
	// export_package_item, resolved BEFORE the package itself is
	// created — export_packages is immutable the instant it exists
	// (no UPDATE path at all, same as every other DRC-04 table), so
	// package_hash must be final at INSERT time, not backfilled.
	type resolvedItem struct {
		itemType       string
		refID          string
		docVerID       *string
		renditionID    *string
		redactionID    *string
		included       bool
		hash           *string
		omissionReason *string
	}

	var out domain.ExportPackage
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		var resolved []resolvedItem
		var hashLines []string

		for _, in := range p.Items {
			var hash *string
			var included = !in.Omit
			var redactionID *string
			if in.RedactionID != nil && *in.RedactionID != "" {
				redactionID = in.RedactionID
				var exists bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM redaction_profiles WHERE redaction_id = $1 AND tenant_id::text = $2)`,
					*in.RedactionID, tenantID).Scan(&exists); err != nil {
					return fmt.Errorf("document store unavailable: %w", err)
				}
				if !exists {
					return domain.ErrExportRedactionNotFound
				}
			}

			var docVerID, renditionID *string
			if included {
				var h string
				var err error
				switch domain.ExportItemType(in.ItemType) {
				case domain.ExportItemDocumentVersion:
					err = tx.QueryRow(ctx, `SELECT checksum_sha256 FROM document_versions dv JOIN documents d ON d.document_id = dv.document_id
						WHERE dv.document_version_id = $1 AND d.tenant_id::text = $2`, in.RefID, tenantID).Scan(&h)
					docVerID = &in.RefID
				case domain.ExportItemRendition:
					err = tx.QueryRow(ctx, `SELECT checksum_sha256 FROM renditions WHERE rendition_id = $1 AND tenant_id::text = $2`,
						in.RefID, tenantID).Scan(&h)
					renditionID = &in.RefID
				}
				if err != nil {
					if errors.Is(err, pgx.ErrNoRows) {
						return domain.ErrExportItemRefNotFound
					}
					return fmt.Errorf("document store unavailable: %w", err)
				}
				hash = &h
			} else {
				switch domain.ExportItemType(in.ItemType) {
				case domain.ExportItemDocumentVersion:
					docVerID = &in.RefID
				case domain.ExportItemRendition:
					renditionID = &in.RefID
				}
			}

			var omissionReason *string
			if in.Omit {
				r := in.OmissionReason
				omissionReason = &r
			}

			resolved = append(resolved, resolvedItem{
				itemType: in.ItemType, refID: in.RefID, docVerID: docVerID, renditionID: renditionID,
				redactionID: redactionID, included: included, hash: hash, omissionReason: omissionReason,
			})

			hashVal := ""
			if hash != nil {
				hashVal = *hash
			}
			hashLines = append(hashLines, fmt.Sprintf("%s|%s|%t|%s", in.ItemType, in.RefID, included, hashVal))
		}

		sort.Strings(hashLines)
		sum := sha256.Sum256([]byte(strings.Join(hashLines, "\n")))
		packageHash := hex.EncodeToString(sum[:])

		row := tx.QueryRow(ctx, `
			INSERT INTO export_packages (tenant_id, requested_by_principal_id, package_hash)
			VALUES ($1, $2, $3)
			RETURNING `+exportPackageColumns,
			tenantID, p.RequestedByPrincipalID, packageHash,
		)
		if err := scanExportPackage(row, &out); err != nil {
			return fmt.Errorf("document store unavailable: %w", mapPgError(err))
		}

		var items []domain.ExportPackageItem
		for _, ri := range resolved {
			var it domain.ExportPackageItem
			irow := tx.QueryRow(ctx, `
				INSERT INTO export_package_items (tenant_id, package_id, item_type, document_version_id, rendition_id,
					redaction_id, included, included_hash, omission_reason)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
				RETURNING `+exportPackageItemColumns,
				tenantID, out.PackageID, ri.itemType, ri.docVerID, ri.renditionID, ri.redactionID, ri.included, ri.hash, ri.omissionReason,
			)
			if err := scanExportPackageItem(irow, &it); err != nil {
				return fmt.Errorf("document store unavailable: %w", mapPgError(err))
			}
			items = append(items, it)
		}

		out.PackageHash = packageHash
		out.Items = items
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *PgStore) GetExportPackage(ctx context.Context, packageID string) (*domain.ExportPackage, error) {
	var out domain.ExportPackage
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		row := tx.QueryRow(ctx, `SELECT `+exportPackageColumns+` FROM export_packages WHERE package_id = $1 AND tenant_id::text = $2`,
			packageID, tenantID)
		if err := scanExportPackage(row, &out); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrExportPackageNotFound
			}
			return fmt.Errorf("document store unavailable: %w", mapPgError(err))
		}

		rows, err := tx.Query(ctx, `SELECT `+exportPackageItemColumns+` FROM export_package_items WHERE package_id = $1 AND tenant_id::text = $2 ORDER BY created_at`,
			packageID, tenantID)
		if err != nil {
			return fmt.Errorf("document store unavailable: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var it domain.ExportPackageItem
			if err := scanExportPackageItem(rows, &it); err != nil {
				return err
			}
			out.Items = append(out.Items, it)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
