package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/audit-event-store-svc/internal/domain"
)

// EventQueryStore is the read interface for querying immutable audit events and verifying chain integrity.
type EventQueryStore interface {
	QueryEvents(ctx context.Context, p domain.QueryEventsParams) (*domain.QueryEventsResult, error)
	VerifyChain(ctx context.Context) (verified bool, checkedEvents int64, err error)
}

// QueryEvents queries audit_events from PostgreSQL, strictly enforcing tenant
// isolation and applying optional filters.
func (s *PgStore) QueryEvents(ctx context.Context, p domain.QueryEventsParams) (*domain.QueryEventsResult, error) {
	if p.Limit <= 0 {
		p.Limit = 50
	}
	if p.Limit > 200 {
		p.Limit = 200
	}
	if p.Offset < 0 {
		p.Offset = 0
	}

	var res domain.QueryEventsResult
	res.Events = make([]domain.AuditEventRecord, 0)

	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Set app.tenant_id so the row-level security policy (migration 000003)
		// is active on this transaction.
		if p.TenantID != "" {
			if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", p.TenantID); err != nil {
				return fmt.Errorf("set tenant context: %w", err)
			}
		}

		countQuery := `
			SELECT COUNT(*)
			FROM audit_events
			WHERE tenant_id = $1
			  AND ($2 = '' OR legal_entity_id = $2)
			  AND ($3 = '' OR principal_id = $3)
			  AND ($4 = '' OR event_type = $4)
			  AND ($5 = '' OR correlation_id = $5)
			  AND ($6::timestamptz IS NULL OR stored_at >= $6)
			  AND ($7::timestamptz IS NULL OR stored_at <= $7)`

		var total int64
		if err := tx.QueryRow(ctx, countQuery,
			p.TenantID,
			p.LegalEntityID,
			p.PrincipalID,
			p.EventType,
			p.CorrelationID,
			p.FromTime,
			p.ToTime,
		).Scan(&total); err != nil {
			return fmt.Errorf("count audit events: %w", err)
		}
		res.Total = total

		if total == 0 {
			// Nothing returned, nothing to be invalid: vacuously true.
			res.HashChainValid = true
			return nil
		}

		selectQuery := `
			SELECT event_id, event_type, tenant_id, legal_entity_id,
			       COALESCE(principal_id, ''), source_service, schema_version,
			       payload, stored_at, COALESCE(correlation_id, ''),
			       COALESCE(causation_id, ''), COALESCE(sequence_number, 0),
			       COALESCE(payload_hash, ''), COALESCE(previous_event_hash, '')
			FROM audit_events
			WHERE tenant_id = $1
			  AND ($2 = '' OR legal_entity_id = $2)
			  AND ($3 = '' OR principal_id = $3)
			  AND ($4 = '' OR event_type = $4)
			  AND ($5 = '' OR correlation_id = $5)
			  AND ($6::timestamptz IS NULL OR stored_at >= $6)
			  AND ($7::timestamptz IS NULL OR stored_at <= $7)
			ORDER BY sequence_number DESC, stored_at DESC
			LIMIT $8 OFFSET $9`

		rows, err := tx.Query(ctx, selectQuery,
			p.TenantID,
			p.LegalEntityID,
			p.PrincipalID,
			p.EventType,
			p.CorrelationID,
			p.FromTime,
			p.ToTime,
			p.Limit,
			p.Offset,
		)
		if err != nil {
			return fmt.Errorf("query audit events: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var rec domain.AuditEventRecord
			var prevHash string
			err := rows.Scan(
				&rec.EventID,
				&rec.EventType,
				&rec.TenantID,
				&rec.LegalEntityID,
				&rec.PrincipalID,
				&rec.SourceService,
				&rec.SchemaVersion,
				&rec.Payload,
				&rec.StoredAt,
				&rec.CorrelationID,
				&rec.CausationID,
				&rec.SequenceNumber,
				&rec.PayloadHash,
				&prevHash,
			)
			if err != nil {
				return fmt.Errorf("scan audit event: %w", err)
			}
			rec.ID = rec.EventID
			rec.Action = rec.EventType
			rec.Timestamp = rec.StoredAt.UTC().Format(time.RFC3339)
			rec.HashSignature = rec.PayloadHash
			rec.PreviousHash = prevHash

			if len(rec.Payload) > 0 {
				var pMap map[string]interface{}
				if json.Unmarshal(rec.Payload, &pMap) == nil {
					rec.Metadata = pMap
					if d, ok := pMap["domain"].(string); ok {
						rec.Domain = d
					}
					if r, ok := pMap["resource"].(string); ok {
						rec.Resource = r
					}
					if rid, ok := pMap["resource_id"].(string); ok {
						rec.ResourceID = rid
					}
					if s, ok := pMap["status"].(string); ok {
						rec.Status = s
					}
					if pn, ok := pMap["principal_name"].(string); ok {
						rec.PrincipalName = pn
					}
				}
			}

			res.Events = append(res.Events, rec)
		}
		return rows.Err()
	})

	if err != nil {
		return nil, fmt.Errorf("query audit events: %w", err)
	}

	// hash_chain_valid is computed, never assumed: the returned events are
	// re-checked after the tenant-scoped read (see pageChainValid).
	seqs := make([]int64, 0, len(res.Events))
	for _, ev := range res.Events {
		if ev.SequenceNumber > 0 {
			seqs = append(seqs, ev.SequenceNumber)
		}
	}
	valid, err := s.pageChainValid(ctx, seqs)
	if err != nil {
		return nil, fmt.Errorf("verify returned events: %w", err)
	}
	res.HashChainValid = valid

	return &res, nil
}

// VerifyChain checks the whole hash chain, across every tenant, in sequence
// order: the genesis row, sequence contiguity, each previous_event_hash link,
// and — for every row written under a hash scheme — that the stored payload
// still hashes to its stored payload_hash. It runs under app.platform_scope and
// returns only a verdict and a count, never row content.
func (s *PgStore) VerifyChain(ctx context.Context) (bool, int64, error) {
	var verified bool
	var count int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// readChainRange sets app.platform_scope so RLS permits reading the
		// global chain.
		chain, err := readChainRange(ctx, tx, 1, math.MaxInt64)
		if err != nil {
			return fmt.Errorf("read full chain: %w", err)
		}

		count = int64(len(chain))
		if count == 0 {
			verified = true
			return nil
		}

		// Genesis event check
		if chain[0].seq != 1 || chain[0].previous != nil {
			verified = false
			return nil
		}

		// Payload, sequence and link checks
		if failedSeq := verifyLinks(chain); failedSeq != nil {
			verified = false
			count = *failedSeq
			return nil
		}

		verified = true
		return nil
	})
	if err != nil {
		return false, 0, err
	}
	return verified, count, nil
}

// pageChainValid reports whether the events whose sequence numbers are seqs are
// individually intact: each one's stored payload still hashes to its
// payload_hash (when its scheme allows re-deriving it) and each one's
// previous_event_hash equals its predecessor's payload_hash (or it is the
// genesis row). It is the meaning of hash_chain_valid on GET /v1/events: a
// verdict about the events returned, not about the whole chain (that is POST
// /v1/events/verify). The predecessor is read under app.platform_scope because
// it may belong to another tenant, but only a boolean leaves this function —
// no other tenant's data is exposed. An empty page is vacuously valid.
func (s *PgStore) pageChainValid(ctx context.Context, seqs []int64) (bool, error) {
	if len(seqs) == 0 {
		return true, nil
	}
	valid := true
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.platform_scope', 'true', true)"); err != nil {
			return fmt.Errorf("set platform scope: %w", err)
		}
		return tx.QueryRow(ctx, `
			SELECT COALESCE(bool_and(ok), true) FROM (
			  SELECT (
			    (e.payload_hash_scheme IS DISTINCT FROM '`+PayloadHashSchemeJSONBTextV1+`'
			       OR e.payload_hash = encode(sha256(convert_to(e.payload::text, 'UTF8')), 'hex'))
			    AND (
			      (e.sequence_number = 1 AND e.previous_event_hash IS NULL)
			      OR EXISTS (SELECT 1 FROM audit_events p
			                  WHERE p.sequence_number = e.sequence_number - 1
			                    AND p.payload_hash = e.previous_event_hash)
			    )
			  ) AS ok
			  FROM audit_events e
			  WHERE e.sequence_number = ANY($1)
			) t`, seqs).Scan(&valid)
	})
	if err != nil {
		return false, err
	}
	return valid, nil
}

// QueryEvents implements EventQueryStore on FakeStore for unit tests.
func (f *FakeStore) QueryEvents(_ context.Context, p domain.QueryEventsParams) (*domain.QueryEventsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if p.Limit <= 0 {
		p.Limit = 50
	}
	if p.Limit > 200 {
		p.Limit = 200
	}
	if p.Offset < 0 {
		p.Offset = 0
	}

	var matched []domain.AuditEventRecord
	for i := len(f.order) - 1; i >= 0; i-- {
		id := f.order[i]
		e := f.events[id]

		if p.TenantID != "" && e.TenantID != p.TenantID {
			continue
		}
		if p.LegalEntityID != "" && e.LegalEntityID != p.LegalEntityID {
			continue
		}
		if p.PrincipalID != "" && e.PrincipalID != p.PrincipalID {
			continue
		}
		if p.EventType != "" && e.EventType != p.EventType {
			continue
		}
		if p.CorrelationID != "" && e.CorrelationID != p.CorrelationID {
			continue
		}

		rec := domain.AuditEventRecord{
			ID:             e.EventID,
			EventID:        e.EventID,
			EventType:      e.EventType,
			Action:         e.EventType,
			TenantID:       e.TenantID,
			LegalEntityID:  e.LegalEntityID,
			PrincipalID:    e.PrincipalID,
			SourceService:  e.SourceService,
			SchemaVersion:  e.SchemaVersion,
			Payload:        e.Payload,
			StoredAt:       time.Now().UTC(),
			Timestamp:      time.Now().UTC().Format(time.RFC3339),
			CorrelationID:  e.CorrelationID,
			CausationID:    e.CausationID,
			SequenceNumber: e.SequenceNumber,
			PayloadHash:    e.PayloadHash,
			HashSignature:  e.PayloadHash,
			PreviousHash:   e.PreviousEventHash,
		}
		if len(e.Payload) > 0 {
			var pMap map[string]interface{}
			if json.Unmarshal(e.Payload, &pMap) == nil {
				rec.Metadata = pMap
				if d, ok := pMap["domain"].(string); ok {
					rec.Domain = d
				}
				if r, ok := pMap["resource"].(string); ok {
					rec.Resource = r
				}
				if rid, ok := pMap["resource_id"].(string); ok {
					rec.ResourceID = rid
				}
				if s, ok := pMap["status"].(string); ok {
					rec.Status = s
				}
				if pn, ok := pMap["principal_name"].(string); ok {
					rec.PrincipalName = pn
				}
			}
		}

		matched = append(matched, rec)
	}

	total := int64(len(matched))
	start := p.Offset
	if start > len(matched) {
		start = len(matched)
	}
	end := start + p.Limit
	if end > len(matched) {
		end = len(matched)
	}

	page := matched[start:end]
	return &domain.QueryEventsResult{
		Events:         page,
		Total:          total,
		HashChainValid: f.pageValid(page),
	}, nil
}

// pageValid mirrors PgStore.pageChainValid: every returned event must still hash
// to its stored payload_hash and link to its predecessor in the chain. The
// caller holds f.mu.
func (f *FakeStore) pageValid(page []domain.AuditEventRecord) bool {
	idx := make(map[string]int, len(f.order))
	for i, id := range f.order {
		idx[id] = i
	}
	for _, rec := range page {
		e := f.events[rec.EventID]
		if hashPayload(e.Payload) != e.PayloadHash {
			return false
		}
		i := idx[rec.EventID]
		if i == 0 {
			if e.SequenceNumber != 1 || e.PreviousEventHash != "" {
				return false
			}
			continue
		}
		prev := f.events[f.order[i-1]]
		if e.SequenceNumber != prev.SequenceNumber+1 || e.PreviousEventHash != prev.PayloadHash {
			return false
		}
	}
	return true
}

// VerifyChain implements EventQueryStore on FakeStore for unit tests.
func (f *FakeStore) VerifyChain(_ context.Context) (bool, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	count := int64(len(f.order))
	if count == 0 {
		return true, 0, nil
	}

	for i := 0; i < len(f.order); i++ {
		e := f.events[f.order[i]]
		if hashPayload(e.Payload) != e.PayloadHash {
			return false, int64(i + 1), nil
		}
		if i == 0 {
			if e.SequenceNumber != 1 || e.PreviousEventHash != "" {
				return false, 0, nil
			}
		} else {
			prev := f.events[f.order[i-1]]
			if e.SequenceNumber != prev.SequenceNumber+1 {
				return false, int64(i), nil
			}
			if e.PreviousEventHash != prev.PayloadHash {
				return false, int64(i), nil
			}
		}
	}

	return true, count, nil
}

// Implement ArchiveStore on FakeStore so unit tests can use FakeStore with
// Handler. Mirroring PgStore, every method is tenant-scoped: an empty tenant
// fails closed, and another tenant's archive/verification history is reported
// as ErrArchiveNotFound.
func (f *FakeStore) CreateArchive(_ context.Context, p domain.CreateArchiveParams) (*domain.Archive, error) {
	if p.TenantID == "" {
		return nil, domain.ErrTenantRequired
	}
	if p.ToSequence < p.FromSequence {
		return nil, domain.ErrInvalidRange
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	a := domain.Archive{
		ArchiveID:            fmt.Sprintf("arch-%d", len(f.archives)+1),
		TenantID:             p.TenantID,
		FromSequence:         p.FromSequence,
		ToSequence:           p.ToSequence,
		EventCount:           p.ToSequence - p.FromSequence + 1,
		CreatedByPrincipalID: p.CreatedByPrincipalID,
	}
	f.archives[a.ArchiveID] = a
	return &a, nil
}

func (f *FakeStore) GetArchive(_ context.Context, archiveID, tenantID string) (*domain.Archive, error) {
	if tenantID == "" {
		return nil, domain.ErrTenantRequired
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.archives[archiveID]
	if !ok || a.TenantID != tenantID {
		return nil, domain.ErrArchiveNotFound
	}
	return &a, nil
}

func (f *FakeStore) VerifyArchive(_ context.Context, p domain.VerifyArchiveParams) (*domain.ArchiveVerification, error) {
	if p.TenantID == "" {
		return nil, domain.ErrTenantRequired
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.archives[p.ArchiveID]
	if !ok || a.TenantID != p.TenantID {
		return nil, domain.ErrArchiveNotFound
	}
	v := domain.ArchiveVerification{
		VerificationID:        fmt.Sprintf("ver-%d", len(f.verifications[p.ArchiveID])+1),
		ArchiveID:             p.ArchiveID,
		TenantID:              p.TenantID,
		VerifiedByPrincipalID: p.VerifiedByPrincipalID,
		Result:                domain.VerificationVerified,
	}
	f.verifications[p.ArchiveID] = append(f.verifications[p.ArchiveID], v)
	return &v, nil
}

func (f *FakeStore) ListVerifications(_ context.Context, archiveID, tenantID string) ([]domain.ArchiveVerification, error) {
	if tenantID == "" {
		return nil, domain.ErrTenantRequired
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.archives[archiveID]
	if !ok || a.TenantID != tenantID {
		return nil, domain.ErrArchiveNotFound
	}
	out := make([]domain.ArchiveVerification, 0, len(f.verifications[archiveID]))
	out = append(out, f.verifications[archiveID]...)
	return out, nil
}

// Compile-time interface checks
var _ EventQueryStore = (*PgStore)(nil)
var _ EventQueryStore = (*FakeStore)(nil)
var _ ArchiveStore = (*FakeStore)(nil)
