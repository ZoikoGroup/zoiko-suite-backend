package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/outbox"
)

// ErrGroupConflict is a group name already in use in the tenant.
var ErrGroupConflict = errors.New("a group with this name already exists")

// ── Connections ─────────────────────────────────────────────────────────────

// CreateSCIMConnection stores a connection with its (hashed) bearer token.
func (s *PgStore) CreateSCIMConnection(ctx context.Context, c domain.SCIMConnection, tokenHash string, recs ...outbox.Record) error {
	return s.withRLS(ctx, c.TenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO scim_connections (connection_id, tenant_id, idp_id, token_hash, status, created_by)
			VALUES ($1,$2,$3,$4,'ACTIVE',$5)`, c.ConnectionID, c.TenantID, nullIfEmpty(c.IdPID), tokenHash, c.CreatedBy); err != nil {
			return err
		}
		return s.enqueueAll(ctx, tx, recs)
	})
}

// SCIMConnectionByToken authenticates a SCIM client; nil when the token is
// unknown or revoked.
func (s *PgStore) SCIMConnectionByToken(ctx context.Context, tokenHash string) (*domain.SCIMConnection, error) {
	var c domain.SCIMConnection
	var idp *string
	err := s.withTokenLookup(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT connection_id, tenant_id, idp_id, status, created_by, created_at, last_sync_at
			FROM scim_connections WHERE token_hash = $1 AND status = 'ACTIVE'`, tokenHash).Scan(
			&c.ConnectionID, &c.TenantID, &idp, &c.Status, &c.CreatedBy, &c.CreatedAt, &c.LastSyncAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if idp != nil {
		c.IdPID = *idp
	}
	return &c, nil
}

// RevokeSCIMConnection revokes a connection's token.
func (s *PgStore) RevokeSCIMConnection(ctx context.Context, tenantID, connectionID string) (bool, error) {
	var n int64
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE scim_connections SET status = 'REVOKED', revoked_at = NOW()
			WHERE connection_id = $1 AND tenant_id = $2 AND status = 'ACTIVE'`, connectionID, tenantID)
		n = tag.RowsAffected()
		return err
	})
	return n > 0, err
}

// TouchSCIMConnection records a sync.
func (s *PgStore) TouchSCIMConnection(ctx context.Context, tenantID, connectionID string) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE scim_connections SET last_sync_at = NOW() WHERE connection_id = $1 AND tenant_id = $2`,
			connectionID, tenantID)
		return err
	})
}

// ── Groups ──────────────────────────────────────────────────────────────────

// CreateGroup stores a group.
func (s *PgStore) CreateGroup(ctx context.Context, g domain.Group) error {
	err := s.withRLS(ctx, g.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO identity_groups (group_id, tenant_id, display_name, external_id, source)
			VALUES ($1,$2,$3,$4,$5)`, g.GroupID, g.TenantID, g.DisplayName, nullIfEmpty(g.ExternalID), g.Source)
		return err
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrGroupConflict
	}
	return err
}

// GetGroup reads a live group with its members; nil when absent.
func (s *PgStore) GetGroup(ctx context.Context, tenantID, groupID string) (*domain.Group, error) {
	var g domain.Group
	var ext *string
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT group_id, tenant_id, display_name, external_id, source, created_at, updated_at
			FROM identity_groups WHERE group_id = $1 AND tenant_id = $2 AND deleted_at IS NULL`, groupID, tenantID).Scan(
			&g.GroupID, &g.TenantID, &g.DisplayName, &ext, &g.Source, &g.CreatedAt, &g.UpdatedAt); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT principal_id FROM group_memberships WHERE group_id = $1 AND tenant_id = $2
			ORDER BY principal_id`, groupID, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				return err
			}
			g.Members = append(g.Members, p)
		}
		return rows.Err()
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if ext != nil {
		g.ExternalID = *ext
	}
	return &g, nil
}

// ListGroups pages a tenant's live groups; nameFilter matches displayName exactly.
func (s *PgStore) ListGroups(ctx context.Context, tenantID, nameFilter string, offset, limit int) ([]domain.Group, int, error) {
	where := "tenant_id = $1 AND deleted_at IS NULL"
	args := []any{tenantID}
	if nameFilter != "" {
		where += " AND LOWER(display_name) = LOWER($2)"
		args = append(args, nameFilter)
	}
	var out []domain.Group
	var total int
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM identity_groups WHERE `+where, args...).Scan(&total); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT group_id, tenant_id, display_name, COALESCE(external_id,''), source,
			created_at, updated_at FROM identity_groups WHERE %s ORDER BY created_at OFFSET %d LIMIT %d`, where, offset, limit), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var g domain.Group
			if err := rows.Scan(&g.GroupID, &g.TenantID, &g.DisplayName, &g.ExternalID, &g.Source, &g.CreatedAt, &g.UpdatedAt); err != nil {
				return err
			}
			out = append(out, g)
		}
		return rows.Err()
	})
	return out, total, err
}

// RenameGroup changes a group's display name.
func (s *PgStore) RenameGroup(ctx context.Context, tenantID, groupID, name string) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE identity_groups SET display_name = $3, updated_at = NOW()
			WHERE group_id = $1 AND tenant_id = $2 AND deleted_at IS NULL`, groupID, tenantID, name)
		return err
	})
}

// DeleteGroup soft-deletes a group and returns its former members.
func (s *PgStore) DeleteGroup(ctx context.Context, tenantID, groupID string) ([]string, error) {
	var members []string
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `DELETE FROM group_memberships WHERE group_id = $1 AND tenant_id = $2 RETURNING principal_id`,
			groupID, tenantID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				rows.Close()
				return err
			}
			members = append(members, p)
		}
		rows.Close()
		_, err = tx.Exec(ctx, `UPDATE identity_groups SET deleted_at = NOW() WHERE group_id = $1 AND tenant_id = $2`, groupID, tenantID)
		return err
	})
	return members, err
}

// SetGroupMembers applies member additions and removals and reports which
// actually changed.
func (s *PgStore) SetGroupMembers(ctx context.Context, tenantID, groupID string, add, remove []string, replace bool) (added, removed []string, err error) {
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if replace {
			rows, err := tx.Query(ctx, `SELECT principal_id FROM group_memberships WHERE group_id = $1 AND tenant_id = $2`, groupID, tenantID)
			if err != nil {
				return err
			}
			want := map[string]bool{}
			for _, a := range add {
				want[a] = true
			}
			for rows.Next() {
				var p string
				if err := rows.Scan(&p); err != nil {
					rows.Close()
					return err
				}
				if !want[p] {
					remove = append(remove, p)
				}
			}
			rows.Close()
		}
		for _, p := range remove {
			tag, err := tx.Exec(ctx, `DELETE FROM group_memberships WHERE group_id = $1 AND principal_id = $2 AND tenant_id = $3`,
				groupID, p, tenantID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() > 0 {
				removed = append(removed, p)
			}
		}
		for _, p := range add {
			tag, err := tx.Exec(ctx, `INSERT INTO group_memberships (group_id, principal_id, tenant_id)
				SELECT $1, $2, $3 WHERE EXISTS (SELECT 1 FROM principals WHERE principal_id = $2 AND tenant_id = $3)
				ON CONFLICT DO NOTHING`, groupID, p, tenantID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() > 0 {
				added = append(added, p)
			}
		}
		_, err := tx.Exec(ctx, `UPDATE identity_groups SET updated_at = NOW() WHERE group_id = $1 AND tenant_id = $2`, groupID, tenantID)
		return err
	})
	return added, removed, err
}

// GroupsOfPrincipal lists the groups a principal belongs to.
func (s *PgStore) GroupsOfPrincipal(ctx context.Context, tenantID, principalID string) ([]string, error) {
	var out []string
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT group_id FROM group_memberships WHERE principal_id = $1 AND tenant_id = $2`, principalID, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var g string
			if err := rows.Scan(&g); err != nil {
				return err
			}
			out = append(out, g)
		}
		return rows.Err()
	})
	return out, err
}

// ── Group-to-role mappings ──────────────────────────────────────────────────

const mappingCols = `mapping_id, tenant_id, group_id, role_id, COALESCE(legal_entity_id,''), status, version, reason,
	proposed_by, proposed_at, COALESCE(decided_by,''), decided_at`

func scanMapping(row pgx.Row) (*domain.GroupRoleMapping, error) {
	var m domain.GroupRoleMapping
	err := row.Scan(&m.MappingID, &m.TenantID, &m.GroupID, &m.RoleID, &m.LegalEntityID, &m.Status, &m.Version, &m.Reason,
		&m.ProposedBy, &m.ProposedAt, &m.DecidedBy, &m.DecidedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &m, err
}

// ProposeMapping stores a PROPOSED mapping.
func (s *PgStore) ProposeMapping(ctx context.Context, m domain.GroupRoleMapping, recs ...outbox.Record) error {
	err := s.withRLS(ctx, m.TenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO group_role_mappings (mapping_id, tenant_id, group_id, role_id, legal_entity_id,
			    status, version, reason, proposed_by) VALUES ($1,$2,$3,$4,$5,'PROPOSED',1,$6,$7)`,
			m.MappingID, m.TenantID, m.GroupID, m.RoleID, nullIfEmpty(m.LegalEntityID), m.Reason, m.ProposedBy); err != nil {
			return err
		}
		return s.enqueueAll(ctx, tx, recs)
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return errors.New("an equivalent mapping is already proposed or active")
	}
	return err
}

// DecideMapping approves (→ACTIVE), rejects or retires a mapping. The DB
// refuses an approver who proposed it.
func (s *PgStore) DecideMapping(ctx context.Context, tenantID, mappingID, from, to, actor string, recs ...outbox.Record) (*domain.GroupRoleMapping, error) {
	var out *domain.GroupRoleMapping
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var q string
		switch to {
		case domain.MappingActive, domain.MappingRejected:
			q = `UPDATE group_role_mappings SET status = $4, decided_by = $5, decided_at = NOW()
				WHERE mapping_id = $1 AND tenant_id = $2 AND status = $3 RETURNING ` + mappingCols
		case domain.MappingRetired:
			q = `UPDATE group_role_mappings SET status = $4, retired_by = $5, retired_at = NOW(), version = version + 1
				WHERE mapping_id = $1 AND tenant_id = $2 AND status = $3 RETURNING ` + mappingCols
		default:
			return fmt.Errorf("unsupported mapping transition to %s", to)
		}
		var err error
		out, err = scanMapping(tx.QueryRow(ctx, q, mappingID, tenantID, from, to, actor))
		if err != nil || out == nil {
			return err
		}
		return s.enqueueAll(ctx, tx, recs)
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23514" {
		return nil, errors.New("the proposer of a mapping cannot approve it (maker-checker)")
	}
	return out, err
}

// GetMapping reads a mapping; nil when absent.
func (s *PgStore) GetMapping(ctx context.Context, tenantID, mappingID string) (*domain.GroupRoleMapping, error) {
	var out *domain.GroupRoleMapping
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = scanMapping(tx.QueryRow(ctx, `SELECT `+mappingCols+` FROM group_role_mappings WHERE mapping_id = $1 AND tenant_id = $2`,
			mappingID, tenantID))
		return err
	})
	return out, err
}

// ListMappings lists a tenant's mappings, optionally for one group and/or status.
func (s *PgStore) ListMappings(ctx context.Context, tenantID, groupID, status string) ([]domain.GroupRoleMapping, error) {
	where := []string{"tenant_id = $1"}
	args := []any{tenantID}
	if groupID != "" {
		args = append(args, groupID)
		where = append(where, fmt.Sprintf("group_id = $%d", len(args)))
	}
	if status != "" {
		args = append(args, status)
		where = append(where, fmt.Sprintf("status = $%d", len(args)))
	}
	var out []domain.GroupRoleMapping
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+mappingCols+` FROM group_role_mappings WHERE `+strings.Join(where, " AND ")+
			` ORDER BY proposed_at`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			m, err := scanMapping(rows)
			if err != nil {
				return err
			}
			out = append(out, *m)
		}
		return rows.Err()
	})
	return out, err
}

// RecordGroupGrant upserts the access-control request made for a member.
func (s *PgStore) RecordGroupGrant(ctx context.Context, tenantID, mappingID, principalID, requestID, status, detail string) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO group_role_grants (mapping_id, principal_id, tenant_id, access_request_id, status, detail)
			VALUES ($1,$2,$3,$4,$5,$6)
			ON CONFLICT (mapping_id, principal_id) DO UPDATE SET access_request_id = COALESCE(EXCLUDED.access_request_id, group_role_grants.access_request_id),
			    status = EXCLUDED.status, detail = EXCLUDED.detail, updated_at = NOW()`,
			mappingID, principalID, tenantID, nullIfEmpty(requestID), status, nullIfEmpty(detail))
		return err
	})
}

// MembersOfGroup lists a group's members (for a newly activated mapping).
func (s *PgStore) MembersOfGroup(ctx context.Context, tenantID, groupID string) ([]string, error) {
	g, err := s.GetGroup(ctx, tenantID, groupID)
	if err != nil || g == nil {
		return nil, err
	}
	return g.Members, nil
}

// ── WebAuthn credentials ────────────────────────────────────────────────────

// InsertWebAuthnCredential stores a registered credential with its events.
func (s *PgStore) InsertWebAuthnCredential(ctx context.Context, c domain.WebAuthnCredential, recs ...outbox.Record) error {
	return s.withRLS(ctx, c.TenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO webauthn_credentials (credential_id, tenant_id, principal_id, public_key,
			    attestation_type, aaguid, sign_count, transports, backup_eligible, backup_state, user_verified, nickname, status)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'ACTIVE')`,
			c.CredentialID, c.TenantID, c.PrincipalID, c.PublicKey, c.AttestationType, nullIfEmpty(c.AAGUID),
			int64(c.SignCount), c.Transports, c.BackupEligible, c.BackupState, c.UserVerified, nullIfEmpty(c.Nickname)); err != nil {
			return err
		}
		return s.enqueueAll(ctx, tx, recs)
	})
}

// ListWebAuthnCredentials lists a principal's active credentials.
func (s *PgStore) ListWebAuthnCredentials(ctx context.Context, tenantID, principalID string) ([]domain.WebAuthnCredential, error) {
	var out []domain.WebAuthnCredential
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT credential_id, tenant_id, principal_id, public_key, attestation_type, COALESCE(aaguid,''),
			    sign_count, transports, backup_eligible, backup_state, user_verified, COALESCE(nickname,''), status, created_at, last_used_at
			FROM webauthn_credentials WHERE tenant_id = $1 AND principal_id = $2 AND status = 'ACTIVE' ORDER BY created_at`,
			tenantID, principalID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c domain.WebAuthnCredential
			var count int64
			if err := rows.Scan(&c.CredentialID, &c.TenantID, &c.PrincipalID, &c.PublicKey, &c.AttestationType, &c.AAGUID,
				&count, &c.Transports, &c.BackupEligible, &c.BackupState, &c.UserVerified, &c.Nickname, &c.Status,
				&c.CreatedAt, &c.LastUsedAt); err != nil {
				return err
			}
			c.SignCount = uint32(count)
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

// UseWebAuthnCredential records an assertion: the new sign counter (a counter
// that does not advance on an authenticator that keeps one signals a clone,
// and the caller refuses before this) and last use.
func (s *PgStore) UseWebAuthnCredential(ctx context.Context, tenantID, credentialID string, signCount uint32, backupState bool, recs ...outbox.Record) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE webauthn_credentials SET sign_count = $3, backup_state = $4, last_used_at = NOW()
			WHERE credential_id = $1 AND tenant_id = $2 AND status = 'ACTIVE'`, credentialID, tenantID, int64(signCount), backupState); err != nil {
			return err
		}
		return s.enqueueAll(ctx, tx, recs)
	})
}

// RetireWebAuthnCredential retires one credential (or all of a principal's
// when credentialID is empty).
func (s *PgStore) RetireWebAuthnCredential(ctx context.Context, tenantID, principalID, credentialID, reason string, recs ...outbox.Record) (int64, error) {
	var n int64
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE webauthn_credentials SET status = 'RETIRED', retired_at = $4, retired_reason = $5
			WHERE tenant_id = $1 AND principal_id = $2 AND ($3 = '' OR credential_id = $3) AND status = 'ACTIVE'`,
			tenantID, principalID, credentialID, time.Now().UTC(), reason)
		if err != nil {
			return err
		}
		n = tag.RowsAffected()
		if n == 0 {
			return nil
		}
		return s.enqueueAll(ctx, tx, recs)
	})
	return n, err
}

// HasActiveWebAuthn reports whether the principal holds a passkey.
func (s *PgStore) HasActiveWebAuthn(ctx context.Context, tenantID, principalID string) (bool, error) {
	var n int
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM webauthn_credentials WHERE tenant_id = $1 AND principal_id = $2 AND status = 'ACTIVE'`,
			tenantID, principalID).Scan(&n)
	})
	return n > 0, err
}
