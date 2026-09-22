// Package store is document-vault-svc's Postgres persistence layer.
//
// Every statement here runs inside a transaction that has installed
// `app.tenant_id`, and every statement carries an explicit `tenant_id = $n`
// predicate as well. Both, deliberately: migration 000002 puts FORCE ROW LEVEL
// SECURITY on all three tables, so the policy is the backstop, and the explicit
// predicate is what makes the intent readable at the call site. Neither is
// load-bearing alone.
//
// What this replaced is worth recording. The tenant predicate used to read
// `($2::uuid IS NULL OR tenant_id = $2::uuid)` and the tenant came from
// `tenantFromCtx`, which returned nil when the request carried no X-Tenant-Id.
// A NULL there makes the first disjunct TRUE for every row, so a caller who
// simply omitted the header read the whole platform's documents. The doc
// comment on FindDocumentByID described this as a "fallback to unscoped
// lookup" for "a not-yet-migrated caller" — an accurate description of a
// cross-tenant read on a vault holding RESTRICTED content.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/document-vault-svc/internal/domain"
	svcmiddleware "zoiko.io/document-vault-svc/internal/middleware"
	"zoiko.io/document-vault-svc/internal/outbox"
)

type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

func New(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

// tenantOf returns the caller's tenant, or ErrTenantMissing.
//
// It returns an error rather than an empty string on purpose: an empty tenant
// is not a scope, and every previous shape of this function let "no tenant"
// mean "all tenants" somewhere downstream.
func tenantOf(ctx context.Context) (string, error) {
	t := svcmiddleware.TenantFromContext(ctx)
	if t == "" {
		return "", domain.ErrTenantMissing
	}
	return t, nil
}

// mapPgError turns a malformed identifier into "not found".
//
// document_id is a uuid column, so a caller passing a non-UUID string reaches
// Postgres and returns 22P02 invalid_text_representation, which surfaced as a
// 503 — a request the caller got wrong, reported as an outage. A malformed id
// cannot name a row, which is what 404 means.
func mapPgError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22P02" {
		return domain.ErrDocumentNotFound
	}
	return err
}

// withTenant runs fn inside a transaction that has installed app.tenant_id, so
// the row-level security policies added in 000002 apply to every statement it
// makes.
//
// set_config takes the value as a PARAMETER. It is never interpolated into the
// statement — board-resolutions-svc shipped `fmt.Sprintf("SET LOCAL
// app.tenant_id = '%s'", tenantID)`, which built the statement enforcing tenant
// isolation out of a request header, under the owner role that RLS exempts.
func (s *PgStore) withTenant(ctx context.Context, fn func(tx pgx.Tx, tenantID string) error) error {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("document store unavailable: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("document store unavailable: %w", err)
	}

	if err := fn(tx, tenantID); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("document store unavailable: %w", err)
	}
	return nil
}

const documentColumns = `
	document_id, tenant_id, legal_entity_id, title, classification, retention_policy,
	residency_region_code, current_version, status, created_by_principal_id, created_at, updated_at,
	declared_version, declared_at, declared_by_principal_id, superseded_by_document_id,
	archived_at, archived_by_principal_id, archive_reason,
	disposition_requested_at, disposition_requested_by_principal_id, disposition_reason
`

func scanDocument(row pgx.Row, d *domain.Document) error {
	return row.Scan(&d.DocumentID, &d.TenantID, &d.LegalEntityID, &d.Title, &d.Classification,
		&d.RetentionPolicy, &d.ResidencyRegionCode, &d.CurrentVersion, &d.Status,
		&d.CreatedByPrincipalID, &d.CreatedAt, &d.UpdatedAt,
		&d.DeclaredVersion, &d.DeclaredAt, &d.DeclaredByPrincipalID, &d.SupersededByDocumentID,
		&d.ArchivedAt, &d.ArchivedByPrincipalID, &d.ArchiveReason,
		&d.DispositionRequestedAt, &d.DispositionRequestedByPrincipalID, &d.DispositionReason)
}

const versionColumns = `
	document_version_id, document_id, version, checksum_sha256, storage_key, size_bytes,
	content_type, created_by_principal_id, created_at
`

func scanVersion(row pgx.Row, v *domain.DocumentVersion) error {
	return row.Scan(&v.DocumentVersionID, &v.DocumentID, &v.Version, &v.ChecksumSHA256, &v.StorageKey,
		&v.SizeBytes, &v.ContentType, &v.CreatedByPrincipalID, &v.CreatedAt)
}

// CreateDocument inserts the document row and its first version in one
// transaction — a document is never left pointing at a version that doesn't
// exist.
//
// The tenant is taken from the caller's context, not from doc.TenantID. A
// document may only be written into the tenant the request is scoped to, and
// the WITH CHECK half of the policy enforces the same thing at the database.
func (s *PgStore) CreateDocument(ctx context.Context, doc *domain.Document, firstVersion *domain.DocumentVersion, correlationID string) error {
	return s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		doc.TenantID = tenantID
		err := tx.QueryRow(ctx, `
			INSERT INTO documents (tenant_id, legal_entity_id, title, classification, retention_policy,
				residency_region_code, current_version, status, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, 1, 'ACTIVE', $7)
			RETURNING document_id, created_at, updated_at
		`, tenantID, doc.LegalEntityID, doc.Title, string(doc.Classification), doc.RetentionPolicy,
			doc.ResidencyRegionCode, doc.CreatedByPrincipalID,
		).Scan(&doc.DocumentID, &doc.CreatedAt, &doc.UpdatedAt)
		if err != nil {
			return fmt.Errorf("document store unavailable: %w", mapPgError(err))
		}
		doc.CurrentVersion = 1
		doc.Status = domain.StatusActive

		firstVersion.DocumentID = doc.DocumentID
		firstVersion.Version = 1
		err = tx.QueryRow(ctx, `
			INSERT INTO document_versions (document_id, version, checksum_sha256, storage_key, size_bytes,
				content_type, created_by_principal_id)
			VALUES ($1, 1, $2, $3, $4, $5, $6)
			RETURNING document_version_id, created_at
		`, firstVersion.DocumentID, firstVersion.ChecksumSHA256, firstVersion.StorageKey, firstVersion.SizeBytes,
			firstVersion.ContentType, firstVersion.CreatedByPrincipalID,
		).Scan(&firstVersion.DocumentVersionID, &firstVersion.CreatedAt)
		if err != nil {
			return fmt.Errorf("document store unavailable: %w", mapPgError(err))
		}

		if err := s.insertDocumentOutboxEvent(ctx, tx, "document.uploaded", doc.DocumentID, tenantID,
			doc.LegalEntityID, doc.CreatedByPrincipalID, correlationID, map[string]any{
				"document_id":     doc.DocumentID,
				"tenant_id":       tenantID,
				"legal_entity_id": doc.LegalEntityID,
				"title":           doc.Title,
				"classification":  string(doc.Classification),
				"version":         firstVersion.Version,
				"checksum_sha256": firstVersion.ChecksumSHA256,
			}); err != nil {
			return err
		}
		return nil
	})
}

// insertDocumentOutboxEvent is the shared outbox-insert call site for
// every document-vault-svc event — see internal/outbox's own package doc
// for why the insert happens here, inside the same transaction as the
// business mutation, rather than as a separate publish call.
func (s *PgStore) insertDocumentOutboxEvent(ctx context.Context, tx pgx.Tx, eventType, documentID, tenantID, legalEntityID, actorID, correlationID string, payload map[string]any) error {
	if correlationID == "" {
		correlationID = documentID
	}
	env, err := outbox.NewVariantAEnvelope(eventType, correlationID, tenantID, legalEntityID, actorID, payload)
	if err != nil {
		return fmt.Errorf("build outbox envelope: %w", err)
	}
	actor := actorID
	if err := outbox.Insert(ctx, tx, outbox.Event{
		AggregateType: "DOCUMENT",
		AggregateID:   documentID,
		EventType:     eventType,
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		ActorID:       &actor,
		CorrelationID: correlationID,
		Payload:       env,
	}); err != nil {
		return fmt.Errorf("outbox insert: %w", err)
	}
	return nil
}

// AddVersion appends a new immutable version row and bumps
// documents.current_version — the ONLY mutation ever applied to the documents
// row post-creation.
func (s *PgStore) AddVersion(ctx context.Context, documentID string, v *domain.DocumentVersion, correlationID string) (*domain.Document, error) {
	var out domain.Document
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		var nextVersion int
		err := tx.QueryRow(ctx, `
			UPDATE documents SET current_version = current_version + 1, updated_at = now()
			WHERE document_id = $1 AND tenant_id = $2::uuid
			RETURNING current_version
		`, documentID, tenantID).Scan(&nextVersion)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrDocumentNotFound
		}
		if err != nil {
			if mapped := mapPgError(err); errors.Is(mapped, domain.ErrDocumentNotFound) {
				return mapped
			}
			return fmt.Errorf("document store unavailable: %w", err)
		}

		v.DocumentID = documentID
		v.Version = nextVersion
		err = tx.QueryRow(ctx, `
			INSERT INTO document_versions (document_id, version, checksum_sha256, storage_key, size_bytes,
				content_type, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING document_version_id, created_at
		`, documentID, nextVersion, v.ChecksumSHA256, v.StorageKey, v.SizeBytes, v.ContentType, v.CreatedByPrincipalID,
		).Scan(&v.DocumentVersionID, &v.CreatedAt)
		if err != nil {
			return fmt.Errorf("document store unavailable: %w", mapPgError(err))
		}

		// Read the document back inside the SAME transaction. Doing it after
		// the commit — as this used to — reads through a second connection
		// that has not installed app.tenant_id, so under FORCE row-level
		// security it would find nothing and report a document that had just
		// been written successfully as missing.
		row := tx.QueryRow(ctx, `SELECT `+documentColumns+`
			FROM documents WHERE document_id = $1 AND tenant_id = $2::uuid`, documentID, tenantID)
		if err := scanDocument(row, &out); err != nil {
			return fmt.Errorf("document store unavailable: %w", err)
		}

		if err := s.insertDocumentOutboxEvent(ctx, tx, "document.version_created", documentID, tenantID,
			out.LegalEntityID, v.CreatedByPrincipalID, correlationID, map[string]any{
				"document_id":     documentID,
				"tenant_id":       tenantID,
				"legal_entity_id": out.LegalEntityID,
				"version":         v.Version,
				"checksum_sha256": v.ChecksumSHA256,
			}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// DeclareRecord marks the document's CURRENT version (as of this call) the
// authoritative declared record — BIZ-01's own central concept ("record
// declaration tracked separately"). The CAS predicate
// (status='ACTIVE' AND declared_at IS NULL) is the real guard; migration
// 000005's trigger is the second line of defense against a raw UPDATE
// bypassing it.
func (s *PgStore) DeclareRecord(ctx context.Context, p domain.DeclareRecordParams, correlationID string) (*domain.Document, error) {
	var out domain.Document
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		row := tx.QueryRow(ctx, `
			UPDATE documents
			SET declared_version = current_version, declared_at = now(), declared_by_principal_id = $3, updated_at = now()
			WHERE document_id = $1 AND tenant_id = $2::uuid AND status = 'ACTIVE' AND declared_at IS NULL
			RETURNING `+documentColumns,
			p.DocumentID, tenantID, p.DeclaredByPrincipalID,
		)
		if err := scanDocument(row, &out); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return s.classifyDeclareRecordFailure(ctx, tx, tenantID, p.DocumentID)
			}
			return fmt.Errorf("document store unavailable: %w", err)
		}

		declaredVersion := 0
		if out.DeclaredVersion != nil {
			declaredVersion = *out.DeclaredVersion
		}
		if err := s.insertDocumentOutboxEvent(ctx, tx, "document.record_declared", p.DocumentID, tenantID,
			out.LegalEntityID, p.DeclaredByPrincipalID, correlationID, map[string]any{
				"document_id":      p.DocumentID,
				"tenant_id":        tenantID,
				"legal_entity_id":  out.LegalEntityID,
				"declared_version": declaredVersion,
			}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// classifyDeclareRecordFailure runs when DeclareRecord's CAS UPDATE
// touched zero rows, to tell "document not found" apart from "found but
// not eligible" (already declared, or not ACTIVE) — same not-found-vs-
// invalid-transition split used throughout this codebase (e.g. BNK-09's
// notFoundOrInvalidTransfer).
func (s *PgStore) classifyDeclareRecordFailure(ctx context.Context, tx pgx.Tx, tenantID, documentID string) error {
	var d domain.Document
	row := tx.QueryRow(ctx, `SELECT `+documentColumns+` FROM documents WHERE document_id = $1 AND tenant_id = $2::uuid`, documentID, tenantID)
	if err := scanDocument(row, &d); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrDocumentNotFound
		}
		return fmt.Errorf("document store unavailable: %w", err)
	}
	if d.DeclaredAt != nil {
		return domain.ErrDocumentAlreadyDeclared
	}
	return domain.ErrDocumentNotActive
}

// SupersedeDocument marks documentID as superseded by an already-existing
// document — mirrors AUD-06's SupersedeEvidence exactly: a forward link
// set once, never an in-place content change.
func (s *PgStore) SupersedeDocument(ctx context.Context, p domain.SupersedeDocumentParams, correlationID string) (*domain.Document, error) {
	if p.DocumentID == p.SupersededByDocumentID {
		return nil, domain.ErrCannotSupersedeSelf
	}
	var out domain.Document
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM documents WHERE document_id = $1 AND tenant_id = $2::uuid)`,
			p.SupersededByDocumentID, tenantID).Scan(&exists); err != nil {
			return fmt.Errorf("document store unavailable: %w", err)
		}
		if !exists {
			return domain.ErrSupersedingDocumentNotFound
		}

		row := tx.QueryRow(ctx, `
			UPDATE documents
			SET status = 'SUPERSEDED', superseded_by_document_id = $3, updated_at = now()
			WHERE document_id = $1 AND tenant_id = $2::uuid AND superseded_by_document_id IS NULL
			RETURNING `+documentColumns,
			p.DocumentID, tenantID, p.SupersededByDocumentID,
		)
		if err := scanDocument(row, &out); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return s.classifySupersedeFailure(ctx, tx, tenantID, p.DocumentID)
			}
			return fmt.Errorf("document store unavailable: %w", err)
		}

		if err := s.insertDocumentOutboxEvent(ctx, tx, "document.superseded", p.DocumentID, tenantID,
			out.LegalEntityID, p.ActorPrincipalID, correlationID, map[string]any{
				"document_id":               p.DocumentID,
				"tenant_id":                 tenantID,
				"legal_entity_id":           out.LegalEntityID,
				"superseded_by_document_id": p.SupersededByDocumentID,
			}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *PgStore) classifySupersedeFailure(ctx context.Context, tx pgx.Tx, tenantID, documentID string) error {
	var d domain.Document
	row := tx.QueryRow(ctx, `SELECT `+documentColumns+` FROM documents WHERE document_id = $1 AND tenant_id = $2::uuid`, documentID, tenantID)
	if err := scanDocument(row, &d); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrDocumentNotFound
		}
		return fmt.Errorf("document store unavailable: %w", err)
	}
	return domain.ErrDocumentAlreadySuperseded
}

// MoveToArchive marks the document ARCHIVED — ends its active life the
// same way SupersedeDocument does, just without a replacement document.
func (s *PgStore) MoveToArchive(ctx context.Context, p domain.MoveToArchiveParams, correlationID string) (*domain.Document, error) {
	var out domain.Document
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		row := tx.QueryRow(ctx, `
			UPDATE documents
			SET status = 'ARCHIVED', archived_at = now(), archived_by_principal_id = $3, archive_reason = $4, updated_at = now()
			WHERE document_id = $1 AND tenant_id = $2::uuid AND status IN ('ACTIVE', 'SUPERSEDED')
			RETURNING `+documentColumns,
			p.DocumentID, tenantID, p.ArchivedByPrincipalID, nullableString(p.Reason),
		)
		if err := scanDocument(row, &out); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return s.classifyArchiveFailure(ctx, tx, tenantID, p.DocumentID)
			}
			return fmt.Errorf("document store unavailable: %w", err)
		}

		if err := s.insertDocumentOutboxEvent(ctx, tx, "document.archived", p.DocumentID, tenantID,
			out.LegalEntityID, p.ArchivedByPrincipalID, correlationID, map[string]any{
				"document_id":     p.DocumentID,
				"tenant_id":       tenantID,
				"legal_entity_id": out.LegalEntityID,
			}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *PgStore) classifyArchiveFailure(ctx context.Context, tx pgx.Tx, tenantID, documentID string) error {
	var d domain.Document
	row := tx.QueryRow(ctx, `SELECT `+documentColumns+` FROM documents WHERE document_id = $1 AND tenant_id = $2::uuid`, documentID, tenantID)
	if err := scanDocument(row, &d); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrDocumentNotFound
		}
		return fmt.Errorf("document store unavailable: %w", err)
	}
	return domain.ErrDocumentNotArchivable
}

// RequestDisposition records a disposition request (-> PURGE_PENDING) and
// nothing more — see domain.Document.DispositionRequestedAt's own doc
// comment on why this service does not itself purge anything.
func (s *PgStore) RequestDisposition(ctx context.Context, p domain.RequestDispositionParams, correlationID string) (*domain.Document, error) {
	var out domain.Document
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		row := tx.QueryRow(ctx, `
			UPDATE documents
			SET status = 'PURGE_PENDING', disposition_requested_at = now(), disposition_requested_by_principal_id = $3, disposition_reason = $4, updated_at = now()
			WHERE document_id = $1 AND tenant_id = $2::uuid AND disposition_requested_at IS NULL
			RETURNING `+documentColumns,
			p.DocumentID, tenantID, p.RequestedByPrincipalID, nullableString(p.Reason),
		)
		if err := scanDocument(row, &out); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return s.classifyDispositionFailure(ctx, tx, tenantID, p.DocumentID)
			}
			return fmt.Errorf("document store unavailable: %w", err)
		}

		if err := s.insertDocumentOutboxEvent(ctx, tx, "document.disposition_requested", p.DocumentID, tenantID,
			out.LegalEntityID, p.RequestedByPrincipalID, correlationID, map[string]any{
				"document_id":     p.DocumentID,
				"tenant_id":       tenantID,
				"legal_entity_id": out.LegalEntityID,
			}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *PgStore) classifyDispositionFailure(ctx context.Context, tx pgx.Tx, tenantID, documentID string) error {
	var d domain.Document
	row := tx.QueryRow(ctx, `SELECT `+documentColumns+` FROM documents WHERE document_id = $1 AND tenant_id = $2::uuid`, documentID, tenantID)
	if err := scanDocument(row, &d); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrDocumentNotFound
		}
		return fmt.Errorf("document store unavailable: %w", err)
	}
	return domain.ErrDispositionAlreadyRequested
}

// nullableString turns an empty caller-supplied string into a real SQL
// NULL rather than an empty string — archive_reason/disposition_reason
// are optional free text, and "" and "not given" should not be
// indistinguishable in the column.
func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// GetAsOfDocument reconstructs which version was CURRENT as of asOf, from
// document_versions' own immutable, timestamped lineage — real data, a
// real answer. It does NOT reconstruct historical status/declaration/
// supersession state: there is no history table backing those mutable
// fields on the documents row (unlike BNK-01/BNK-08's own AsOf queries,
// which are backed by a dedicated history table) — see this service's
// own Wave 3 commit message for why that's a real, separate build, not
// silently faked here.
func (s *PgStore) GetAsOfDocument(ctx context.Context, documentID string, asOf time.Time) (*domain.Document, *domain.DocumentVersion, error) {
	doc, err := s.FindDocumentByID(ctx, documentID)
	if err != nil {
		return nil, nil, err
	}
	var v domain.DocumentVersion
	err = s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		row := tx.QueryRow(ctx, `
			SELECT `+versionColumns+` FROM document_versions
			WHERE document_id = $1 AND created_at <= $2
			ORDER BY version DESC LIMIT 1`, documentID, asOf)
		return scanVersion(row, &v)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, domain.ErrDocumentVersionNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("document store unavailable: %w", err)
	}
	return doc, &v, nil
}

// FindDocumentByID looks up a document, scoped to the caller's tenant. A
// request with no tenant is refused rather than widened.
func (s *PgStore) FindDocumentByID(ctx context.Context, documentID string) (*domain.Document, error) {
	var d domain.Document
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		row := tx.QueryRow(ctx, `SELECT `+documentColumns+`
			FROM documents WHERE document_id = $1 AND tenant_id = $2::uuid`, documentID, tenantID)
		if err := scanDocument(row, &d); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrDocumentNotFound
			}
			if mapped := mapPgError(err); errors.Is(mapped, domain.ErrDocumentNotFound) {
				return mapped
			}
			return fmt.Errorf("document store unavailable: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// ListDocuments is the tenant's register, newest first.
//
// There was no list endpoint at all — six routes, every one of which needs a
// document_id you already have. The vault could be written to and read from but
// never browsed, which is why it had no console page: there was nothing to put
// on it.
func (s *PgStore) ListDocuments(ctx context.Context, legalEntityID string, limit, offset int) ([]domain.Document, error) {
	var out []domain.Document
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		query := `SELECT ` + documentColumns + ` FROM documents WHERE tenant_id = $1::uuid`
		args := []any{tenantID}
		if legalEntityID != "" {
			args = append(args, legalEntityID)
			query += fmt.Sprintf(" AND legal_entity_id = $%d::uuid", len(args))
		}
		// document_id breaks ties: created_at alone is not a total order, so
		// without it a paged read can return one row twice and skip another.
		query += " ORDER BY created_at DESC, document_id DESC"
		args = append(args, limit)
		query += fmt.Sprintf(" LIMIT $%d", len(args))
		args = append(args, offset)
		query += fmt.Sprintf(" OFFSET $%d", len(args))

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("document store unavailable: %w", mapPgError(err))
		}
		defer rows.Close()
		for rows.Next() {
			var d domain.Document
			if err := scanDocument(rows, &d); err != nil {
				return fmt.Errorf("document store unavailable: %w", err)
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// FindVersion looks up a document version. The version row carries no
// tenant_id, so the parent document is confirmed in the same transaction.
func (s *PgStore) FindVersion(ctx context.Context, documentID string, version int) (*domain.DocumentVersion, error) {
	var v domain.DocumentVersion
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		if err := ensureDocument(ctx, tx, documentID, tenantID); err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `SELECT `+versionColumns+`
			FROM document_versions WHERE document_id = $1 AND version = $2`, documentID, version)
		if err := scanVersion(row, &v); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrDocumentVersionNotFound
			}
			return fmt.Errorf("document store unavailable: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// ListVersions scopes through the parent document the same way FindVersion does.
func (s *PgStore) ListVersions(ctx context.Context, documentID string) ([]domain.DocumentVersion, error) {
	var out []domain.DocumentVersion
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		if err := ensureDocument(ctx, tx, documentID, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+versionColumns+`
			FROM document_versions WHERE document_id = $1 ORDER BY version ASC`, documentID)
		if err != nil {
			return fmt.Errorf("document store unavailable: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var v domain.DocumentVersion
			if err := scanVersion(rows, &v); err != nil {
				return fmt.Errorf("document store unavailable: %w", err)
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RecordAccess appends one row to the access log. Called on EVERY read
// (metadata or download) — never skipped, never batched away — per the
// append-only access-history doctrine.
func (s *PgStore) RecordAccess(ctx context.Context, log *domain.DocumentAccessLog) error {
	return s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		// The parent is confirmed first so an access-log row can never be
		// written against another tenant's document. Under FORCE row-level
		// security the INSERT's WITH CHECK would refuse it anyway; this turns
		// that into a 404 rather than a constraint error surfacing as a 503.
		if err := ensureDocument(ctx, tx, log.DocumentID, tenantID); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `
			INSERT INTO document_access_log (document_id, document_version_id, accessed_by_principal_id,
				access_type, correlation_id)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING access_log_id, accessed_at
		`, log.DocumentID, log.DocumentVersionID, log.AccessedByPrincipalID, string(log.AccessType), log.CorrelationID,
		).Scan(&log.AccessLogID, &log.AccessedAt)
		if err != nil {
			return fmt.Errorf("document store unavailable: %w", mapPgError(err))
		}
		return nil
	})
}

// ListAccessLog returns the access history newest first, paged.
//
// Newest first, where this used to be oldest first: the question asked of an
// access log is almost always "who has read this recently", and an unbounded
// ascending list answers it only after scrolling past everything that came
// before.
func (s *PgStore) ListAccessLog(ctx context.Context, documentID string, limit, offset int) ([]domain.DocumentAccessLog, error) {
	var out []domain.DocumentAccessLog
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		if err := ensureDocument(ctx, tx, documentID, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT access_log_id, document_id, document_version_id, accessed_by_principal_id, access_type,
				correlation_id, accessed_at
			FROM document_access_log WHERE document_id = $1
			ORDER BY accessed_at DESC, access_log_id DESC
			LIMIT $2 OFFSET $3
		`, documentID, limit, offset)
		if err != nil {
			return fmt.Errorf("document store unavailable: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var a domain.DocumentAccessLog
			if err := rows.Scan(&a.AccessLogID, &a.DocumentID, &a.DocumentVersionID, &a.AccessedByPrincipalID,
				&a.AccessType, &a.CorrelationID, &a.AccessedAt); err != nil {
				return fmt.Errorf("document store unavailable: %w", err)
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// LinkDocument records a link to another business object — BIZ-01's
// GetLinkedObjects data source (migration 000007). Append-only; the
// document_document_links_document_id_linked_object_type_linked_object_id_key
// unique constraint (checked via 23505 here rather than a SELECT-then-INSERT
// race) makes a duplicate link a real error, not a silent second row.
func (s *PgStore) LinkDocument(ctx context.Context, p domain.LinkDocumentParams) (*domain.DocumentLink, error) {
	var out domain.DocumentLink
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		if err := ensureDocument(ctx, tx, p.DocumentID, tenantID); err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `
			INSERT INTO document_links (document_id, linked_object_type, linked_object_id, linked_by_principal_id, correlation_id)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING link_id, document_id, linked_object_type, linked_object_id, linked_by_principal_id, correlation_id, created_at
		`, p.DocumentID, p.LinkedObjectType, p.LinkedObjectID, p.LinkedByPrincipalID, nullableString(p.CorrelationID))
		if err := row.Scan(&out.LinkID, &out.DocumentID, &out.LinkedObjectType, &out.LinkedObjectID,
			&out.LinkedByPrincipalID, &out.CorrelationID, &out.CreatedAt); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return domain.ErrDuplicateLink
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

// ListDocumentLinks is GetLinkedObjects' real query — every object this
// document has ever been linked to, newest first.
func (s *PgStore) ListDocumentLinks(ctx context.Context, documentID string) ([]domain.DocumentLink, error) {
	var out []domain.DocumentLink
	err := s.withTenant(ctx, func(tx pgx.Tx, tenantID string) error {
		if err := ensureDocument(ctx, tx, documentID, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT link_id, document_id, linked_object_type, linked_object_id, linked_by_principal_id, correlation_id, created_at
			FROM document_links WHERE document_id = $1
			ORDER BY created_at DESC, link_id DESC
		`, documentID)
		if err != nil {
			return fmt.Errorf("document store unavailable: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var l domain.DocumentLink
			if err := rows.Scan(&l.LinkID, &l.DocumentID, &l.LinkedObjectType, &l.LinkedObjectID,
				&l.LinkedByPrincipalID, &l.CorrelationID, &l.CreatedAt); err != nil {
				return fmt.Errorf("document store unavailable: %w", err)
			}
			out = append(out, l)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ensureDocument confirms the document exists in this tenant, inside the
// caller's transaction. Used by every child-table read so a version, an
// access-log entry, or a download can never be served for a document the
// caller's tenant does not own.
func ensureDocument(ctx context.Context, tx pgx.Tx, documentID, tenantID string) error {
	var exists bool
	err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM documents WHERE document_id = $1 AND tenant_id = $2::uuid)`,
		documentID, tenantID).Scan(&exists)
	if err != nil {
		if mapped := mapPgError(err); errors.Is(mapped, domain.ErrDocumentNotFound) {
			return mapped
		}
		return fmt.Errorf("document store unavailable: %w", err)
	}
	if !exists {
		return domain.ErrDocumentNotFound
	}
	return nil
}
