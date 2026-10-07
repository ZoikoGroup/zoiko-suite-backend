// Package store is the PgStore persistence layer for global-search-svc
// (DATA-06, ZS-SVC-N-001 §4). Every method runs inside one transaction
// that first declares app.tenant_id for RLS, then performs the write —
// tenant scoping is enforced by the database, never an application-level
// WHERE clause the caller could get wrong. Authorization-before-retrieval
// for individual documents is enforced the same way: every read query's
// own WHERE clause filters on (NOT restricted OR principal = ANY(...)),
// never a post-fetch filter in Go.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/global-search-svc/internal/domain"
	"zoiko.io/global-search-svc/internal/outbox"
)

// Store is the DATA-06 persistence contract.
type Store interface {
	ReindexScope(ctx context.Context, tenantID, scope, actor string, claim domain.IdempotencyClaim) (*domain.SearchIndex, error)
	RebuildIndex(ctx context.Context, tenantID, scope, actor string, claim domain.IdempotencyClaim) (*domain.SearchIndex, error)
	MarkIndexAvailable(ctx context.Context, tenantID, scope, actor string, claim domain.IdempotencyClaim) (*domain.SearchIndex, error)
	IndexObject(ctx context.Context, tenantID string, req domain.IndexObjectRequest, actor string, claim domain.IdempotencyClaim) (*domain.IndexDocument, error)
	PurgeIndexObject(ctx context.Context, tenantID string, req domain.PurgeIndexObjectRequest, claim domain.IdempotencyClaim) error
	SetSearchPolicy(ctx context.Context, tenantID string, req domain.SetSearchPolicyRequest, actor string) (*domain.SearchPolicy, error)

	Search(ctx context.Context, tenantID, scope, principalID, queryText string, limit int) (*domain.SearchResponse, error)
	Autocomplete(ctx context.Context, tenantID, scope, principalID, prefix string, limit int) ([]string, error)
	Count(ctx context.Context, tenantID, scope, principalID, queryText string) (int, error)
	ExplainResult(ctx context.Context, tenantID, scope, principalID, objectRef string) (*domain.SearchResultEvidence, error)
	GetIndexHealth(ctx context.Context, tenantID, scope string, asOf time.Time) (*domain.IndexHealth, error)
	ValidateSearchPolicy(ctx context.Context, tenantID, scope string, asOf time.Time) (*domain.IndexHealth, error)
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
	if strings.Contains(pgErr.Message, "invalid search index transition") || strings.Contains(pgErr.Message, "cannot be deleted") {
		return fmt.Errorf("search index: %s", pgErr.Message)
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

// ── SearchIndex ──────────────────────────────────────────────────────────────

const indexColumns = `index_id, tenant_id, scope, status, created_at, created_by`

func scanIndex(row pgx.Row) (*domain.SearchIndex, error) {
	var idx domain.SearchIndex
	if err := row.Scan(&idx.IndexID, &idx.TenantID, &idx.Scope, &idx.Status, &idx.CreatedAt, &idx.CreatedBy); err != nil {
		return nil, err
	}
	return &idx, nil
}

func findOrCreateIndex(ctx context.Context, tx pgx.Tx, tenantID, scope, actor string) (*domain.SearchIndex, error) {
	id := newID(domain.PrefixIndex)
	got, err := scanIndex(tx.QueryRow(ctx, `
		INSERT INTO search_indexes (index_id, tenant_id, scope, created_by)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, scope) DO NOTHING
		RETURNING `+indexColumns,
		id, tenantID, scope, actor))
	if errors.Is(err, pgx.ErrNoRows) {
		return scanIndex(tx.QueryRow(ctx, `SELECT `+indexColumns+` FROM search_indexes WHERE tenant_id = $1 AND scope = $2`, tenantID, scope))
	}
	return got, err
}

func loadIndexByScope(ctx context.Context, tx pgx.Tx, tenantID, scope string) (*domain.SearchIndex, error) {
	idx, err := scanIndex(tx.QueryRow(ctx, `SELECT `+indexColumns+` FROM search_indexes WHERE tenant_id = $1 AND scope = $2 FOR UPDATE`,
		tenantID, scope))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrIndexNotFound
	}
	return idx, err
}

// ReindexScope ensures a SearchIndex exists for scope — the entry point
// for a caller about to start (or continue) submitting IndexObject
// upserts. Idempotent: calling it against an already-existing index of
// any status is a no-op that just returns the current state.
func (s *PgStore) ReindexScope(ctx context.Context, tenantID, scope, actor string, claim domain.IdempotencyClaim) (*domain.SearchIndex, error) {
	if scope == "" {
		return nil, fmt.Errorf("scope is required")
	}
	var out *domain.SearchIndex
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		claim.ResourceID = scope
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		idx, err := findOrCreateIndex(ctx, tx, tenantID, scope, actor)
		if err != nil {
			return err
		}
		out = idx
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "search_index", AggregateID: idx.IndexID,
			EventType: "DATA.SearchIndexUpdated", TenantID: &tenantID, Payload: idx})
	})
	return out, err
}

// RebuildIndex purges every document in scope and moves the index to
// Rebuilding — the caller then re-submits fresh IndexObject calls and
// finishes with MarkIndexAvailable. Only legal from Available: a fresh
// index that was never live doesn't need "rebuilding."
func (s *PgStore) RebuildIndex(ctx context.Context, tenantID, scope, actor string, claim domain.IdempotencyClaim) (*domain.SearchIndex, error) {
	var out *domain.SearchIndex
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		idx, err := loadIndexByScope(ctx, tx, tenantID, scope)
		if err != nil {
			return err
		}
		if idx.Status != domain.IndexAvailable {
			return fmt.Errorf("search index for scope %q is %s, must be Available to rebuild", scope, idx.Status)
		}
		claim.ResourceID = scope
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM index_documents WHERE tenant_id = $1 AND index_id = $2`, tenantID, idx.IndexID); err != nil {
			return fmt.Errorf("purge documents for rebuild: %w", err)
		}
		got, err := scanIndex(tx.QueryRow(ctx, `UPDATE search_indexes SET status = 'Rebuilding' WHERE index_id = $1 RETURNING `+indexColumns, idx.IndexID))
		if err != nil {
			return fmt.Errorf("mark index rebuilding: %w", err)
		}
		out = got
		_ = actor
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "search_index", AggregateID: got.IndexID,
			EventType: "DATA.SearchIndexUpdated", TenantID: &tenantID, Payload: got})
	})
	return out, err
}

func (s *PgStore) MarkIndexAvailable(ctx context.Context, tenantID, scope, actor string, claim domain.IdempotencyClaim) (*domain.SearchIndex, error) {
	var out *domain.SearchIndex
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		idx, err := loadIndexByScope(ctx, tx, tenantID, scope)
		if err != nil {
			return err
		}
		if idx.Status == domain.IndexAvailable {
			out = idx
			return nil
		}
		claim.ResourceID = scope
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		got, err := scanIndex(tx.QueryRow(ctx, `UPDATE search_indexes SET status = 'Available' WHERE index_id = $1 RETURNING `+indexColumns, idx.IndexID))
		if err != nil {
			return fmt.Errorf("mark index available: %w", err)
		}
		out = got
		_ = actor
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "search_index", AggregateID: got.IndexID,
			EventType: "DATA.SearchIndexUpdated", TenantID: &tenantID, Payload: got})
	})
	return out, err
}

// ── SearchPolicy ─────────────────────────────────────────────────────────────

func (s *PgStore) SetSearchPolicy(ctx context.Context, tenantID string, req domain.SetSearchPolicyRequest, actor string) (*domain.SearchPolicy, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.SearchPolicy
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		id := newID(domain.PrefixPolicy)
		var p domain.SearchPolicy
		if err := tx.QueryRow(ctx, `
			INSERT INTO search_policies (policy_id, tenant_id, scope, max_staleness_seconds, created_by)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (tenant_id, scope) DO UPDATE SET max_staleness_seconds = EXCLUDED.max_staleness_seconds
			RETURNING policy_id, tenant_id, scope, max_staleness_seconds, created_at, created_by`,
			id, tenantID, req.Scope, req.MaxStalenessSeconds, actor,
		).Scan(&p.PolicyID, &p.TenantID, &p.Scope, &p.MaxStalenessSeconds, &p.CreatedAt, &p.CreatedBy); err != nil {
			return fmt.Errorf("upsert search policy: %w", err)
		}
		out = &p
		return nil
	})
	return out, err
}

func loadPolicy(ctx context.Context, tx pgx.Tx, tenantID, scope string) (*domain.SearchPolicy, error) {
	var p domain.SearchPolicy
	err := tx.QueryRow(ctx, `SELECT policy_id, tenant_id, scope, max_staleness_seconds, created_at, created_by
		FROM search_policies WHERE tenant_id = $1 AND scope = $2`, tenantID, scope,
	).Scan(&p.PolicyID, &p.TenantID, &p.Scope, &p.MaxStalenessSeconds, &p.CreatedAt, &p.CreatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrPolicyNotFound
	}
	return &p, err
}

// ── IndexObject / PurgeIndexObject ──────────────────────────────────────────

const documentColumns = `doc_id, tenant_id, index_id, object_type, object_ref, content_text, content_hash,
	classification, residency_region, restricted, allowed_principal_ids, source_updated_at, indexed_at, indexed_by`

func scanDocument(row pgx.Row) (*domain.IndexDocument, error) {
	var d domain.IndexDocument
	if err := row.Scan(&d.DocID, &d.TenantID, &d.IndexID, &d.ObjectType, &d.ObjectRef, &d.ContentText, &d.ContentHash,
		&d.Classification, &d.ResidencyRegion, &d.Restricted, &d.AllowedPrincipalIDs, &d.SourceUpdatedAt, &d.IndexedAt,
		&d.IndexedBy); err != nil {
		return nil, err
	}
	return &d, nil
}

// IndexObject upserts one document — re-indexing the same (object_type,
// object_ref) within a scope updates it in place; this table is a
// disposable derived projection, never evidence, so in-place update is
// correct here (unlike every append-only entity elsewhere in DATA-0x).
func (s *PgStore) IndexObject(ctx context.Context, tenantID string, req domain.IndexObjectRequest, actor string, claim domain.IdempotencyClaim) (*domain.IndexDocument, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.IndexDocument
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		idx, err := findOrCreateIndex(ctx, tx, tenantID, req.Scope, actor)
		if err != nil {
			return err
		}

		docID := newID(domain.PrefixDocument)
		claim.ResourceID = req.Scope + "|" + req.ObjectType + "|" + req.ObjectRef
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		now := time.Now().UTC()
		got, err := scanDocument(tx.QueryRow(ctx, `
			INSERT INTO index_documents (doc_id, tenant_id, index_id, scope, object_type, object_ref, content_text,
				content_hash, classification, residency_region, restricted, allowed_principal_ids, source_updated_at,
				indexed_at, indexed_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
			ON CONFLICT (tenant_id, index_id, object_type, object_ref) DO UPDATE SET
				content_text = EXCLUDED.content_text, content_hash = EXCLUDED.content_hash,
				classification = EXCLUDED.classification, residency_region = EXCLUDED.residency_region,
				restricted = EXCLUDED.restricted, allowed_principal_ids = EXCLUDED.allowed_principal_ids,
				source_updated_at = EXCLUDED.source_updated_at, indexed_at = EXCLUDED.indexed_at, indexed_by = EXCLUDED.indexed_by
			RETURNING `+documentColumns,
			docID, tenantID, idx.IndexID, req.Scope, req.ObjectType, req.ObjectRef, req.ContentText, req.ContentHash,
			req.Classification, req.ResidencyRegion, req.Restricted, req.AllowedPrincipalIDs, req.SourceUpdatedAt, now, actor))
		if err != nil {
			return fmt.Errorf("upsert index document: %w", err)
		}
		out = got
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "index_document", AggregateID: got.DocID,
			EventType: "DATA.SearchIndexUpdated", TenantID: &tenantID, Payload: got})
	})
	return out, err
}

// PurgeIndexObject hard-deletes a document — a genuine physical removal,
// not a soft flag, satisfying "deletion/hold propagation" for real: a
// purged or restricted-under-disposition object must actually disappear
// from the index, not merely be marked and still queryable.
func (s *PgStore) PurgeIndexObject(ctx context.Context, tenantID string, req domain.PurgeIndexObjectRequest, claim domain.IdempotencyClaim) error {
	if err := req.Validate(); err != nil {
		return err
	}
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		claim.ResourceID = req.Scope + "|" + req.ObjectRef
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM index_documents WHERE tenant_id = $1 AND scope = $2 AND object_ref = $3`,
			tenantID, req.Scope, req.ObjectRef)
		if err != nil {
			return fmt.Errorf("purge index document: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrDocumentNotFound
		}
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "index_document", AggregateID: req.ObjectRef,
			EventType: "DATA.SearchIndexUpdated", TenantID: &tenantID,
			Payload: map[string]string{"scope": req.Scope, "object_ref": req.ObjectRef, "action": "purged"}})
	})
}

// ── Search / read surfaces ──────────────────────────────────────────────────

// authFilter is the authorization-before-retrieval predicate every read
// query below applies in its own WHERE clause — never a post-fetch
// filter in Go. A restricted document is invisible to any principal not
// explicitly listed, in search, count, AND autocomplete alike.
const authFilter = `(NOT restricted OR $3 = ANY(allowed_principal_ids))`

func (s *PgStore) Search(ctx context.Context, tenantID, scope, principalID, queryText string, limit int) (*domain.SearchResponse, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	var out *domain.SearchResponse
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT doc_id, object_type, object_ref,
				ts_headline('english', content_text, plainto_tsquery('english', $2)) AS snippet,
				ts_rank(content_tsv, plainto_tsquery('english', $2)) AS rank
			FROM index_documents
			WHERE tenant_id = $1 AND scope = $4 AND content_tsv @@ plainto_tsquery('english', $2) AND `+authFilter+`
			ORDER BY rank DESC
			LIMIT $5`,
			tenantID, queryText, principalID, scope, limit)
		if err != nil {
			return fmt.Errorf("search: %w", err)
		}
		defer rows.Close()
		var results []domain.SearchResult
		for rows.Next() {
			var r domain.SearchResult
			if err := rows.Scan(&r.DocID, &r.ObjectType, &r.ObjectRef, &r.Snippet, &r.Rank); err != nil {
				return err
			}
			results = append(results, r)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		out = &domain.SearchResponse{Results: results, Count: len(results)}
		return nil
	})
	return out, err
}

// Autocomplete suggests matching object_ref values for a prefix — the
// same authorization filter as Search, so an unauthorized object's
// existence can never leak via a suggestion (the doc's own named
// negative test).
func (s *PgStore) Autocomplete(ctx context.Context, tenantID, scope, principalID, prefix string, limit int) ([]string, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	sanitized := sanitizePrefix(prefix)
	if sanitized == "" {
		return nil, nil
	}
	var out []string
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT DISTINCT object_ref
			FROM index_documents
			WHERE tenant_id = $1 AND scope = $4 AND content_tsv @@ to_tsquery('english', $2 || ':*') AND `+authFilter+`
			ORDER BY object_ref
			LIMIT $5`,
			tenantID, sanitized, principalID, scope, limit)
		if err != nil {
			return fmt.Errorf("autocomplete: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var ref string
			if err := rows.Scan(&ref); err != nil {
				return err
			}
			out = append(out, ref)
		}
		return rows.Err()
	})
	return out, err
}

// sanitizePrefix strips everything but alphanumerics — never builds a
// to_tsquery string from unescaped user input.
func sanitizePrefix(prefix string) string {
	var b strings.Builder
	for _, r := range prefix {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Count answers "how many results," under the SAME authorization filter
// as Search — an unauthorized principal must see the identical count
// whether or not a restricted object matching the query exists (the
// doc's own named negative test).
func (s *PgStore) Count(ctx context.Context, tenantID, scope, principalID, queryText string) (int, error) {
	var out int
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT count(*) FROM index_documents
			WHERE tenant_id = $1 AND scope = $4 AND content_tsv @@ plainto_tsquery('english', $2) AND `+authFilter,
			tenantID, queryText, principalID, scope).Scan(&out)
	})
	return out, err
}

// ExplainResult refuses as not-found — never forbidden — when the
// principal isn't authorized, so the refusal itself carries no
// information about whether the object exists at all.
func (s *PgStore) ExplainResult(ctx context.Context, tenantID, scope, principalID, objectRef string) (*domain.SearchResultEvidence, error) {
	var out *domain.SearchResultEvidence
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var e domain.SearchResultEvidence
		err := tx.QueryRow(ctx, `
			SELECT doc_id, object_type, object_ref, classification, content_hash, source_updated_at, indexed_at
			FROM index_documents
			WHERE tenant_id = $1 AND scope = $4 AND object_ref = $2 AND `+authFilter,
			tenantID, objectRef, principalID, scope,
		).Scan(&e.DocID, &e.ObjectType, &e.ObjectRef, &e.Classification, &e.ContentHash, &e.SourceUpdatedAt, &e.IndexedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrDocumentNotFound
		}
		if err != nil {
			return err
		}
		out = &e
		return nil
	})
	return out, err
}

// ── GetIndexHealth / ValidateSearchPolicy ────────────────────────────────────

// GetIndexHealth and ValidateSearchPolicy are the same computed, read-time
// report — "Degraded" is derived from freshness against the scope's
// policy, never a stored, separately-transitioned status (the doc's own
// "stale index returns freshness indication" acceptance test, satisfied
// as a live report rather than a mutation someone has to remember to
// trigger).
func (s *PgStore) GetIndexHealth(ctx context.Context, tenantID, scope string, asOf time.Time) (*domain.IndexHealth, error) {
	return s.computeHealth(ctx, tenantID, scope, asOf)
}

func (s *PgStore) ValidateSearchPolicy(ctx context.Context, tenantID, scope string, asOf time.Time) (*domain.IndexHealth, error) {
	return s.computeHealth(ctx, tenantID, scope, asOf)
}

func (s *PgStore) computeHealth(ctx context.Context, tenantID, scope string, asOf time.Time) (*domain.IndexHealth, error) {
	var out *domain.IndexHealth
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		idx, err := scanIndex(tx.QueryRow(ctx, `SELECT `+indexColumns+` FROM search_indexes WHERE tenant_id = $1 AND scope = $2`, tenantID, scope))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrIndexNotFound
		}
		if err != nil {
			return err
		}

		var docCount int64
		var oldest *time.Time
		if err := tx.QueryRow(ctx, `SELECT count(*), min(source_updated_at) FROM index_documents WHERE tenant_id = $1 AND scope = $2`,
			tenantID, scope).Scan(&docCount, &oldest); err != nil {
			return err
		}

		h := &domain.IndexHealth{Scope: scope, Status: idx.Status, DocumentCount: docCount, OldestSourceUpdatedAt: oldest}

		policy, err := loadPolicy(ctx, tx, tenantID, scope)
		if err != nil && !errors.Is(err, domain.ErrPolicyNotFound) {
			return err
		}
		if policy != nil && oldest != nil {
			h.MaxStalenessSeconds = policy.MaxStalenessSeconds
			h.StalenessSeconds = int64(asOf.Sub(*oldest).Seconds())
			h.Degraded = h.StalenessSeconds > h.MaxStalenessSeconds
		}
		out = h
		return nil
	})
	return out, err
}
