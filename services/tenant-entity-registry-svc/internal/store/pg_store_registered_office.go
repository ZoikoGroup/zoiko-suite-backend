package store

// The registered office on the address model (ORG-08, plan ORG-6: "move the existing registered-office route onto
// this model, keeping the route").
//
// POST /v1/entities/{id}/registered-office is unchanged: it writes the ORG-03 profile version, which stays the
// legal record. In the same transaction, when the office was sent as a structured address, it is also recorded as a
// REGISTERED address and linked to the entity, ending the previous registered-office link on the new start date.
// The source is AUTHORITY_DOCUMENT because the command is evidenced and goes through ORG-03's approval; that is
// what lets a REGISTERED address exist at all (a postal address is not a registered one).
//
// Two cases stay in the profile history only, by design: an office that is not structured (it was always opaque
// JSON, and is still accepted), and a change that does not start after the current link began (a backdated
// correction would have to rewrite the link intervals). Neither is lost; the profile version holds it.

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

func (s *PgStore) mirrorRegisteredOffice(ctx context.Context, tx pgx.Tx, tid string, v *domain.LegalEntityProfileVersion) error {
	if v.RegisteredOffice == nil || v.SourceEvidenceRef == nil || *v.SourceEvidenceRef == "" {
		return nil
	}
	f, ok := domain.StructuredOffice(*v.RegisteredOffice)
	if !ok {
		return nil
	}
	eff := asOfDate(v.EffectiveFrom)
	f.SourceClass = domain.SourceAuthorityDocument
	f.EvidenceRef = *v.SourceEvidenceRef
	f.ValidFrom = fmtDate(eff)
	if f.Validate() != nil {
		return nil // not a valid structured address after all: the profile version still holds it as given
	}

	var liveID string
	var liveFrom time.Time
	err := tx.QueryRow(ctx, `SELECT link_id, valid_from FROM entity_address_links
		WHERE tenant_id = $1 AND legal_entity_id = $2 AND link_type = 'REGISTERED' AND valid_to IS NULL FOR UPDATE`, tid, v.LegalEntityID).Scan(&liveID, &liveFrom)
	hasLive := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if hasLive && !eff.After(liveFrom) {
		s.log.Info("registered office change not mirrored onto an address: it does not start after the current one")
		return nil
	}
	// A later link already in place (a future-dated change) would overlap; leave it to the profile history.
	var later int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM entity_address_links WHERE tenant_id = $1 AND legal_entity_id = $2
		AND link_type = 'REGISTERED' AND valid_from >= $3`, tid, v.LegalEntityID, eff).Scan(&later); err != nil {
		return err
	}
	if later > 0 {
		return nil
	}

	id := uuid.NewString()
	if _, err := tx.Exec(ctx, `INSERT INTO addresses (address_id, tenant_id, address_type, created_by, correlation_id)
		VALUES ($1,$2,'REGISTERED',$3,NULL)`, id, tid, v.CreatedByPrincipalID); err != nil {
		return err
	}
	if _, err := insertAddressVersion(ctx, tx, tid, id, 1, f, "", v.CreatedByPrincipalID); err != nil {
		return err
	}
	if hasLive {
		if _, err := tx.Exec(ctx, `UPDATE entity_address_links SET valid_to = $2 WHERE link_id = $1`, liveID, eff); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO entity_address_links (link_id, tenant_id, legal_entity_id, address_id, link_type, valid_from, evidence_ref, profile_version_id, linked_by)
		VALUES ($1,$2,$3,$4,'REGISTERED',$5,$6,$7,$8)`,
		uuid.NewString(), tid, v.LegalEntityID, id, eff, *v.SourceEvidenceRef, v.ProfileVersionID, v.CreatedByPrincipalID)
	return err
}
