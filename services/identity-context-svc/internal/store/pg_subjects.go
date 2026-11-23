package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/oklog/ulid/v2"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/outbox"
)

// ORG-1 identity operations: subject identity, identity links, and merging a
// duplicate subject into its survivor. identity_merges is the alias table: a
// retired id is never reissued and resolves to the survivor for ever.

var (
	// ErrMergeNotFound: the survivor or the duplicate does not exist in the tenant.
	ErrMergeNotFound = errors.New("survivor or duplicate subject not found")
	// ErrMergeConflict (pg_merge.go) is shared: the duplicate was already
	// merged, or the survivor is disabled or is itself a retired id.
	//
	// ErrMergeStale: the duplicate's record_version is not the If-Match value.
	ErrMergeStale = errors.New("duplicate subject changed since it was read")
)

// maxAliasHops bounds the survivor chain walk. A merge refuses a survivor
// that is itself retired, so chains only form when a survivor is later
// merged into someone else; ten hops is far beyond any real history.
const maxAliasHops = 10

// mergedAwaySQL is the survivor that id $2 was merged into in tenant $1, by
// either merge route: identity_merges (POST /v1/subjects/{id}/merge) or
// principal_merges (POST /v1/identities/{id}/merge, pg_merge.go). Each route
// refuses an id the other has merged away, so at most one row answers.
const mergedAwaySQL = `SELECT survivor_id FROM identity_merges WHERE tenant_id = $1 AND retired_id = $2
	UNION ALL
	SELECT survivor_id FROM principal_merges WHERE tenant_id = $1 AND duplicate_id = $2
	LIMIT 1`

// ResolveSubjectID follows the merge aliases from id to the subject that now
// answers for it. merged reports whether id was retired.
func (s *PgStore) ResolveSubjectID(ctx context.Context, tenantID, id string) (survivor string, merged bool, err error) {
	survivor = id
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		for hop := 0; hop < maxAliasHops; hop++ {
			var next string
			err := tx.QueryRow(ctx, mergedAwaySQL, tenantID, survivor).Scan(&next)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			survivor, merged = next, true
		}
		return fmt.Errorf("merge alias chain from %s exceeds %d hops", id, maxAliasHops)
	})
	return survivor, merged, err
}

// GetIdentityLinks returns every external identity bound to a principal: the
// INT-10 federation links and the auth-plane federated identities.
func (s *PgStore) GetIdentityLinks(ctx context.Context, tenantID, principalID string) ([]domain.IdentityLink, error) {
	links := []domain.IdentityLink{}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT link_id, tenant_id, principal_id, 'federation_link', connection_id, issuer, subject,
			       status, linked_by, created_at, last_login_at
			  FROM federation_links WHERE tenant_id = $1 AND principal_id = $2
			UNION ALL
			SELECT idp_id || ':' || external_subject, tenant_id, principal_id, 'federated_identity', idp_id, '', external_subject,
			       'ACTIVE', '', created_at, last_login_at
			  FROM federated_identities WHERE tenant_id = $1 AND principal_id = $2
			 ORDER BY 10 DESC`, tenantID, principalID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var l domain.IdentityLink
			if err := rows.Scan(&l.LinkID, &l.TenantID, &l.PrincipalID, &l.Source, &l.IdentityProvider, &l.Issuer,
				&l.ProviderSubject, &l.Status, &l.LinkedBy, &l.CreatedAt, &l.LastLoginAt); err != nil {
				return err
			}
			links = append(links, l)
		}
		return rows.Err()
	})
	return links, err
}

// MergeResult reports a completed merge.
type MergeResult struct {
	MergeID     string    `json:"merge_id"`
	SurvivorID  string    `json:"survivor_id"`
	DuplicateID string    `json:"duplicate_id"`
	LinksMoved  int       `json:"links_moved"`
	MergedAt    time.Time `json:"merged_at"`
}

// MergeSubjects retires duplicateID into survivorID in one transaction: the
// duplicate's external identities move to the survivor (a federation link
// whose issuer the survivor already holds is deprovisioned instead), the
// duplicate is disabled with the merge as its reason, the alias is recorded,
// the decision is logged and recs are enqueued. Ending the duplicate's live
// sessions is the caller's job once this commits.
func (s *PgStore) MergeSubjects(ctx context.Context, tenantID, survivorID, duplicateID, actor, reason, correlationID string,
	duplicateVersion int64, recs ...outbox.Record) (*MergeResult, error) {
	now := time.Now().UTC()
	res := &MergeResult{MergeID: ulid.Make().String(), SurvivorID: survivorID, DuplicateID: duplicateID, MergedAt: now}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT principal_id, status, record_version FROM principals
			WHERE tenant_id = $1 AND principal_id IN ($2, $3) FOR UPDATE`, tenantID, survivorID, duplicateID)
		if err != nil {
			return err
		}
		type state struct {
			status  string
			version int64
		}
		found := map[string]state{}
		for rows.Next() {
			var id string
			var st state
			if err := rows.Scan(&id, &st.status, &st.version); err != nil {
				rows.Close()
				return err
			}
			found[id] = st
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		surv, okS := found[survivorID]
		dup, okD := found[duplicateID]
		if !okS || !okD {
			return ErrMergeNotFound
		}
		if surv.status == string(domain.PrincipalStatusDisabled) {
			return fmt.Errorf("%w: the survivor is disabled", ErrMergeConflict)
		}
		// Either ledger: a duplicate merged away by POST /v1/identities/{id}/merge
		// is as retired as one merged here.
		var retired bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM identity_merges
			WHERE tenant_id = $1 AND retired_id IN ($2, $3))
			OR EXISTS (SELECT 1 FROM principal_merges
			WHERE tenant_id = $1 AND duplicate_id IN ($2, $3))`, tenantID, survivorID, duplicateID).Scan(&retired); err != nil {
			return err
		}
		if retired {
			return fmt.Errorf("%w: the survivor or the duplicate was already merged", ErrMergeConflict)
		}
		if dup.version != duplicateVersion {
			return ErrMergeStale
		}

		tag, err := tx.Exec(ctx, `UPDATE federation_links f SET principal_id = $3
			WHERE f.tenant_id = $1 AND f.principal_id = $2
			  AND NOT EXISTS (SELECT 1 FROM federation_links o
			                   WHERE o.tenant_id = $1 AND o.principal_id = $3 AND o.issuer = f.issuer)`,
			tenantID, duplicateID, survivorID)
		if err != nil {
			return fmt.Errorf("move federation links: %w", err)
		}
		res.LinksMoved = int(tag.RowsAffected())
		if _, err := tx.Exec(ctx, `UPDATE federation_links SET status = 'DEPROVISIONED', deprovisioned_at = $3
			WHERE tenant_id = $1 AND principal_id = $2 AND status <> 'DEPROVISIONED'`, tenantID, duplicateID, now); err != nil {
			return fmt.Errorf("deprovision duplicate links: %w", err)
		}
		tag, err = tx.Exec(ctx, `UPDATE federated_identities SET principal_id = $3
			WHERE tenant_id = $1 AND principal_id = $2`, tenantID, duplicateID, survivorID)
		if err != nil {
			return fmt.Errorf("move federated identities: %w", err)
		}
		res.LinksMoved += int(tag.RowsAffected())

		if _, err := tx.Exec(ctx, `UPDATE principals
			SET status = 'DISABLED', disabled_at = $3, disabled_reason = $4, updated_at = $3,
			    record_version = record_version + 1
			WHERE tenant_id = $1 AND principal_id = $2`,
			tenantID, duplicateID, now, "merged into "+survivorID); err != nil {
			return fmt.Errorf("disable duplicate: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO identity_merges
			(merge_id, tenant_id, survivor_id, retired_id, approved_by, approved_at, step_up_evidence, reason)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			res.MergeID, tenantID, survivorID, duplicateID, actor, now, "mfa:"+correlationID, reason); err != nil {
			return fmt.Errorf("record merge: %w", err)
		}
		if err := s.appendDecision(ctx, tx, duplicateID, tenantID, "SUBJECT_MERGED", "APPLIED",
			"merged_into:"+survivorID, correlationID, now); err != nil {
			return err
		}
		for _, rec := range recs {
			if err := s.outboxEnqueueTx(ctx, tx, rec); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}
