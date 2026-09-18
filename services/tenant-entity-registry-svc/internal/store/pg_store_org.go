package store

// ORG-02 (Tenant) and ORG-03 (Legal Entity) store methods added to close the
// named-command, as-of and evidence gaps in the Organization / Legal Entity
// specification §4.2, §4.3, §8 and §9.2.
//
// Two things distinguish everything in this file from the methods in
// pg_store.go:
//
//  1. Every write is GUARDED by record_version. The pattern is a single
//     UPDATE ... WHERE record_version = $expected, and zero rows affected is
//     reported as a conflict rather than retried or ignored. That makes
//     read-then-write a compare-and-swap instead of a race, which is what §4.2
//     and §4.3 mean by expected_version.
//
//  2. Every write that produces a domain event enqueues that event into
//     event_outbox INSIDE THE SAME TRANSACTION. Either the fact and its event
//     both land or neither does. The alternative — the `go s.events.Publish`
//     that the rest of this service still uses — has a window in which the
//     registry holds a legal-entity change the estate never hears about.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/outbox"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

// SetOutbox attaches the transactional event enqueuer.
//
// Optional rather than a constructor parameter so the existing New() call sites
// (including every test that builds a bare store) keep compiling. When it is
// nil the guarded writes below still work and simply publish nothing — which
// is the pre-outbox behaviour, not a silent data-loss path, because the
// caller's own fire-and-forget publisher is still in place.
func (s *PgStore) SetOutbox(enq outbox.Enqueuer) { s.outbox = enq }

// enqueue writes ev into the outbox inside tx, if an enqueuer is attached.
func (s *PgStore) enqueue(ctx context.Context, tx pgx.Tx, ev *outbox.Record) error {
	if s.outbox == nil || ev == nil {
		return nil
	}
	return s.outbox.EnqueueTx(ctx, tx, *ev)
}

// ---------------------------------------------------------------------------
// ORG-02 — tenant named commands
// ---------------------------------------------------------------------------

// ExecuteTenantCommand applies one named lifecycle command atomically.
//
// Three writes in one transaction: the guarded tenant UPDATE, the
// tenant_lifecycle_history row that is §4.2's required durable evidence, and
// the outbox event. Splitting them would allow a tenant to be suspended with
// no record of who suspended it or why.
//
// Returns registry.ErrConflict when zero rows match — which conflates "someone
// else moved it first" with "it was not in an allowed state". The caller
// distinguishes them by re-reading, and the distinction is not worth a second
// round trip inside the transaction: both mean "your command did not apply and
// you should look at the current state".
func (s *PgStore) ExecuteTenantCommand(ctx context.Context, p registry.TenantCommandParams, ev *outbox.Record) (*registry.TenantCommandResult, error) {
	s.log.Debug("store.ExecuteTenantCommand",
		zap.String("tenant_id", p.TenantID),
		zap.String("command", string(p.Command)),
	)
	tid := tenantFromCtxOrFallback(ctx, p.TenantID)

	allowed := make([]string, 0, len(p.AllowedFrom))
	for _, st := range p.AllowedFrom {
		allowed = append(allowed, string(st))
	}

	// Status follows lifecycle for the states where the two must agree.
	// OFFBOARDING and TERMINATED both map to ARCHIVED: the tenant is no longer
	// transacting in either, and TenantStatus has no separate terminal value.
	var newStatus domain.TenantStatus
	switch p.TargetState {
	case domain.TenantLifecycleActive:
		newStatus = domain.TenantStatusActive
	case domain.TenantLifecycleSuspended:
		newStatus = domain.TenantStatusSuspended
	case domain.TenantLifecycleOffboarding, domain.TenantLifecycleTerminated:
		newStatus = domain.TenantStatusArchived
	}

	var res registry.TenantCommandResult
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		now := time.Now().UTC()

		// UPDATE ... FROM (SELECT ... FOR UPDATE) rather than a subquery in
		// RETURNING: a RETURNING subquery is not guaranteed to observe the
		// pre-update row, so the evidence record could name the state the
		// tenant was moved TO as the state it came FROM. The FOR UPDATE join
		// reads the prior row and locks it in the same statement.
		var fromState string
		err := tx.QueryRow(ctx, `
			UPDATE tenants t
			   SET lifecycle_state         = $1,
			       status                  = COALESCE($2::varchar, t.status),
			       record_version          = t.record_version + 1,
			       updated_at              = $3,
			       updated_by_principal_id = $4
			  FROM (
			      SELECT tenant_id, lifecycle_state AS prev_state
			        FROM tenants
			       WHERE tenant_id = $5 AND tenant_id = $6
			       FOR UPDATE
			  ) o
			 WHERE t.tenant_id       = o.tenant_id
			   AND t.record_version  = $7
			   AND t.lifecycle_state = ANY($8)
			RETURNING o.prev_state, t.record_version, t.status`,
			string(p.TargetState), nullableStatus(newStatus), now, p.ActorID,
			p.TenantID, tid, p.ExpectedVersion, allowed,
		).Scan(&fromState, &res.NewVersion, &res.Status)
		if errors.Is(err, pgx.ErrNoRows) {
			return registry.ErrConflict
		}
		if err != nil {
			return fmt.Errorf("tenant command update: %w", err)
		}
		res.FromState = domain.TenantLifecycleState(fromState)
		res.ToState = p.TargetState

		if _, err := tx.Exec(ctx, `
			INSERT INTO tenant_lifecycle_history (
				lifecycle_event_id, tenant_id, from_state, to_state,
				command_name, reason, actor_principal_id,
				approved_by_principal_id, correlation_id, occurred_at
			) VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			p.TenantID, fromState, string(p.TargetState),
			string(p.Command), p.Reason, p.ActorID,
			nullableString(p.ApprovedBy), nullableString(p.CorrelationID), now,
		); err != nil {
			return fmt.Errorf("lifecycle history insert: %w", err)
		}

		return s.enqueue(ctx, tx, ev)
	})
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// ChangeDefaultLocale applies the ORG-02 ChangeDefaultLocale command.
//
// Not a lifecycle transition and deliberately not routed through one: it
// changes baseline tenancy configuration and leaves lifecycle_state untouched.
// It is still version-guarded and still written to the lifecycle history,
// because §4.2 lists it among the named commands and an unrecorded
// configuration change is one nobody can attribute later.
func (s *PgStore) ChangeDefaultLocale(ctx context.Context, tenantID, locale, timezone, reason, actorID, correlationID string, expectedVersion int64, ev *outbox.Record) (*domain.Tenant, error) {
	s.log.Debug("store.ChangeDefaultLocale", zap.String("tenant_id", tenantID))
	tid := tenantFromCtxOrFallback(ctx, tenantID)

	var t domain.Tenant
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		err := tx.QueryRow(ctx, `
			UPDATE tenants
			   SET primary_locale         = COALESCE($1::varchar, primary_locale),
			       primary_timezone       = COALESCE($2::varchar, primary_timezone),
			       record_version         = record_version + 1,
			       updated_at             = $3,
			       updated_by_principal_id = $4
			 WHERE tenant_id      = $5
			   AND tenant_id      = $6
			   AND record_version = $7
			RETURNING tenant_id, tenant_code, legal_name, trading_name, status,
			          default_currency_code, primary_timezone, primary_locale,
			          default_data_residency_policy_id, lifecycle_state,
			          record_version, created_at, updated_at,
			          created_by_principal_id, updated_by_principal_id`,
			nullableString(locale), nullableString(timezone), now, actorID,
			tenantID, tid, expectedVersion,
		).Scan(
			&t.TenantID, &t.TenantCode, &t.LegalName, &t.TradingName, &t.Status,
			&t.DefaultCurrencyCode, &t.PrimaryTimezone, &t.PrimaryLocale,
			&t.DefaultDataResidencyPolicyID, &t.LifecycleState,
			&t.RecordVersion, &t.CreatedAt, &t.UpdatedAt,
			&t.CreatedByPrincipalID, &t.UpdatedByPrincipalID,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return registry.ErrConflict
		}
		if err != nil {
			return fmt.Errorf("change default locale: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO tenant_lifecycle_history (
				lifecycle_event_id, tenant_id, from_state, to_state,
				command_name, reason, actor_principal_id, correlation_id, occurred_at
			) VALUES (gen_random_uuid(), $1, $2, $2, $3, $4, $5, $6, $7)`,
			tenantID, string(t.LifecycleState),
			string(domain.TenantCommandChangeDefaultLocale), reason, actorID,
			nullableString(correlationID), now,
		); err != nil {
			return fmt.Errorf("lifecycle history insert: %w", err)
		}

		return s.enqueue(ctx, tx, ev)
	})
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// ListTenantLifecycleHistory is the ORG-02 read surface of the same name.
func (s *PgStore) ListTenantLifecycleHistory(ctx context.Context, tenantID string) ([]*domain.TenantLifecycleEvent, error) {
	tid := tenantFromCtxOrFallback(ctx, tenantID)

	var out []*domain.TenantLifecycleEvent
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT lifecycle_event_id, tenant_id, from_state, to_state,
			       command_name, reason, actor_principal_id,
			       approved_by_principal_id, correlation_id, occurred_at
			  FROM tenant_lifecycle_history
			 WHERE tenant_id = $1
			 ORDER BY occurred_at DESC, lifecycle_event_id DESC`, tid)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var e domain.TenantLifecycleEvent
			var from *string
			if err := rows.Scan(
				&e.LifecycleEventID, &e.TenantID, &from, &e.ToState,
				&e.CommandName, &e.Reason, &e.ActorPrincipalID,
				&e.ApprovedByPrincipalID, &e.CorrelationID, &e.OccurredAt,
			); err != nil {
				return err
			}
			if from != nil {
				st := domain.TenantLifecycleState(*from)
				e.FromState = &st
			}
			out = append(out, &e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []*domain.TenantLifecycleEvent{}
	}
	return out, nil
}

// GetTenantDefaults is the ORG-02 GetTenantDefaults read surface.
func (s *PgStore) GetTenantDefaults(ctx context.Context, tenantID string) (*domain.TenantDefaults, error) {
	tid := tenantFromCtxOrFallback(ctx, tenantID)

	var d domain.TenantDefaults
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT tenant_id, default_currency_code, primary_timezone, primary_locale,
			       default_data_residency_policy_id, record_version
			  FROM tenants WHERE tenant_id = $1 AND tenant_id = $2`,
			tenantID, tid,
		).Scan(&d.TenantID, &d.DefaultCurrencyCode, &d.PrimaryTimezone,
			&d.PrimaryLocale, &d.DefaultDataResidencyPolicyID, &d.RecordVersion)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// ---------------------------------------------------------------------------
// ORG-02 — host bindings (ResolveTenantByHost, §8 NP3)
// ---------------------------------------------------------------------------

// BindTenantHost maps a hostname to a tenant.
//
// The hostname is lowercased here as well as constrained by a CHECK, so a
// caller that sends "Example.COM" gets a binding rather than a constraint
// error — case is not meaningful in a hostname and rejecting it would be
// pedantry, but storing both cases would let two bindings claim one host.
func (s *PgStore) BindTenantHost(ctx context.Context, b *domain.TenantHostBinding) error {
	tid := tenantFromCtxOrFallback(ctx, b.TenantID)
	b.Hostname = strings.ToLower(strings.TrimSpace(b.Hostname))

	return s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO tenant_host_bindings (
				host_binding_id, hostname, tenant_id, is_primary, active_flag,
				created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, TRUE, $5, $6)`,
			b.HostBindingID, b.Hostname, b.TenantID, b.IsPrimary,
			time.Now().UTC(), b.CreatedByPrincipalID,
		)
		if err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: hostname %s is already bound", registry.ErrConflict, b.Hostname)
			}
			return err
		}
		return nil
	})
}

// ResolveTenantByHost is the ORG-02 read surface of the same name.
//
// Runs WITHOUT the withRLS wrapper, and that is the point: this is the lookup
// that establishes which tenant a request belongs to, so it cannot be scoped by
// a tenant that is not known yet. tenant_host_bindings carries no tenant data
// for the same reason — see migration 000006.
//
// A hostname bound to a tenant that has since been terminated still resolves.
// The caller is an ingress layer deciding whether to admit traffic, and
// "belongs to a terminated tenant" is a different and more useful answer than
// "unknown host" — the latter invites a retry against a different hostname.
func (s *PgStore) ResolveTenantByHost(ctx context.Context, hostname string) (*domain.ResolvedTenantByHost, error) {
	h := strings.ToLower(strings.TrimSpace(hostname))
	if h == "" {
		return nil, nil
	}

	// Runs in a transaction that sets app.tenant_resolve, the ONE other named
	// capability on the tenants policy (migration 000006).
	//
	// tenant_host_bindings has no RLS, but the join to tenants does, and without
	// the capability that join returns nothing: every hostname would look
	// unbound, which is exactly the "unknown host" answer §8 NP3 says must not
	// be produced for a host that IS bound. The capability is transaction-local
	// and read-only — the policy's WITH CHECK half deliberately does not honour
	// it — and this is its only call site in the codebase.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only; rollback is the normal exit

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_resolve', 'true', true)"); err != nil {
		return nil, fmt.Errorf("set_config app.tenant_resolve: %w", err)
	}

	var r domain.ResolvedTenantByHost
	err = tx.QueryRow(ctx, `
		SELECT b.hostname, b.tenant_id, t.tenant_code, t.status, t.lifecycle_state, b.is_primary
		  FROM tenant_host_bindings b
		  JOIN tenants t ON t.tenant_id = b.tenant_id
		 WHERE b.hostname = $1 AND b.active_flag`, h,
	).Scan(&r.Hostname, &r.TenantID, &r.TenantCode, &r.Status, &r.LifecycleState, &r.IsPrimary)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// ListTenantHostBindings returns a tenant's hostnames.
func (s *PgStore) ListTenantHostBindings(ctx context.Context, tenantID string) ([]*domain.TenantHostBinding, error) {
	tid := tenantFromCtxOrFallback(ctx, tenantID)

	var out []*domain.TenantHostBinding
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT host_binding_id, hostname, tenant_id, is_primary, active_flag,
			       created_at, created_by_principal_id
			  FROM tenant_host_bindings
			 WHERE tenant_id = $1
			 ORDER BY is_primary DESC, hostname`, tid)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var b domain.TenantHostBinding
			if err := rows.Scan(&b.HostBindingID, &b.Hostname, &b.TenantID,
				&b.IsPrimary, &b.ActiveFlag, &b.CreatedAt, &b.CreatedByPrincipalID); err != nil {
				return err
			}
			out = append(out, &b)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []*domain.TenantHostBinding{}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// ORG-03 — profile versions
// ---------------------------------------------------------------------------

const profileVersionColumns = `
	profile_version_id, tenant_id, legal_entity_id, version_number,
	legal_name, trading_name, legal_form_code, legal_form_source,
	legal_form_local_text, registration_number, registry_authority,
	registered_office, incorporation_jurisdiction_id, default_currency_code,
	effective_from, effective_to, recorded_at, superseded_at,
	change_reason, source_evidence_ref, created_by_principal_id,
	approved_by_principal_id`

func scanProfileVersion(row pgx.Row) (*domain.LegalEntityProfileVersion, error) {
	var v domain.LegalEntityProfileVersion
	var office *string
	err := row.Scan(
		&v.ProfileVersionID, &v.TenantID, &v.LegalEntityID, &v.VersionNumber,
		&v.LegalName, &v.TradingName, &v.LegalFormCode, &v.LegalFormSource,
		&v.LegalFormLocalText, &v.RegistrationNumber, &v.RegistryAuthority,
		&office, &v.IncorporationJurisdictionID, &v.DefaultCurrencyCode,
		&v.EffectiveFrom, &v.EffectiveTo, &v.RecordedAt, &v.SupersededAt,
		&v.ChangeReason, &v.SourceEvidenceRef, &v.CreatedByPrincipalID,
		&v.ApprovedByPrincipalID,
	)
	if err != nil {
		return nil, err
	}
	v.RegisteredOffice = office
	return &v, nil
}

// CreateInitialProfileVersion writes version 1 of an entity's profile.
//
// Called from the service immediately after CreateEntity. Separate rather than
// folded into CreateEntity because CreateEntity is on the pre-existing Store
// interface with a signature every test mock implements; widening it would
// break all of them for no gain.
func (s *PgStore) CreateInitialProfileVersion(ctx context.Context, v *domain.LegalEntityProfileVersion) error {
	tid := tenantFromCtxOrFallback(ctx, v.TenantID)

	return s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO legal_entity_profile_versions (
				profile_version_id, tenant_id, legal_entity_id, version_number,
				legal_name, trading_name, registration_number,
				incorporation_jurisdiction_id, default_currency_code,
				effective_from, recorded_at, change_reason, created_by_principal_id
			) VALUES ($1, $2, $3, 1, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			v.ProfileVersionID, v.TenantID, v.LegalEntityID,
			v.LegalName, v.TradingName, v.RegistrationNumber,
			v.IncorporationJurisdictionID, v.DefaultCurrencyCode,
			v.EffectiveFrom, time.Now().UTC(),
			string(domain.ProfileChangeInitial), v.CreatedByPrincipalID,
		)
		return err
	})
}

// AmendLegalProfile creates the next effective-dated profile version.
//
// Four writes, one transaction:
//
//  1. close the version currently in force (effective_to = new effective_from)
//  2. insert the new version, carrying forward every field the caller left nil
//  3. update the denormalized columns on legal_entities, version-guarded
//  4. enqueue the event
//
// Step 3 is what keeps GetEntity fast and unchanged for the 95% of callers who
// want current identity and not history. It is a projection of step 2, never
// the authority: a disagreement between them is a bug in this method, and the
// as-of read deliberately answers from the versions table alone so it cannot
// inherit one.
//
// Backdating is allowed and is the case worth thinking about. A version
// effective BEFORE the one currently in force does not become current, so
// step 3 must not run for it — otherwise learning about an old filing would
// rewrite the entity's present-day name. inForce reports which case this is.
func (s *PgStore) AmendLegalProfile(
	ctx context.Context,
	legalEntityID string,
	next *domain.LegalEntityProfileVersion,
	expectedVersion int64,
	ev *outbox.Record,
) (*domain.LegalEntityProfileVersion, error) {
	tid := tenantFromCtxOrFallback(ctx, next.TenantID)

	var out *domain.LegalEntityProfileVersion
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		now := time.Now().UTC()

		// The version in force at the new version's effective instant. This is
		// the one being superseded, which for a backdated amendment is NOT
		// necessarily the latest.
		var prevID string
		var prevNum int
		var prevEffTo *time.Time
		err := tx.QueryRow(ctx, `
			SELECT profile_version_id, version_number, effective_to
			  FROM legal_entity_profile_versions
			 WHERE legal_entity_id = $1 AND tenant_id = $2
			   AND effective_from <= $3
			   AND (effective_to IS NULL OR effective_to > $3)
			 ORDER BY effective_from DESC, version_number DESC
			 LIMIT 1`, legalEntityID, tid, next.EffectiveFrom).
			Scan(&prevID, &prevNum, &prevEffTo)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read version in force: %w", err)
		}
		hasPrev := !errors.Is(err, pgx.ErrNoRows)

		// The highest version number ever issued for this entity — the new
		// version's number. Distinct from prevNum: a backdated amendment
		// supersedes version 2 but is itself version 5.
		var maxNum int
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(MAX(version_number), 0)
			  FROM legal_entity_profile_versions
			 WHERE legal_entity_id = $1 AND tenant_id = $2`,
			legalEntityID, tid).Scan(&maxNum); err != nil {
			return fmt.Errorf("read max version: %w", err)
		}
		next.VersionNumber = maxNum + 1

		if hasPrev {
			// Close the superseded version at the instant the new one starts.
			// superseded_at is RECORD time and effective_to is BUSINESS time;
			// both are set because they answer different questions, and a
			// reader reproducing a historical decision needs to know the row
			// was still open when that decision was made.
			if _, err := tx.Exec(ctx, `
				UPDATE legal_entity_profile_versions
				   SET effective_to = $1, superseded_at = $2
				 WHERE profile_version_id = $3 AND tenant_id = $4`,
				next.EffectiveFrom, now, prevID, tid); err != nil {
				return fmt.Errorf("close prior version: %w", err)
			}
			// A backdated insert lands between two existing versions, so the
			// new row must end where the one it displaced used to end.
			if prevEffTo != nil {
				next.EffectiveTo = prevEffTo
			}
		}

		row := tx.QueryRow(ctx, `
			INSERT INTO legal_entity_profile_versions (
				profile_version_id, tenant_id, legal_entity_id, version_number,
				legal_name, trading_name, legal_form_code, legal_form_source,
				legal_form_local_text, registration_number, registry_authority,
				registered_office, incorporation_jurisdiction_id, default_currency_code,
				effective_from, effective_to, recorded_at,
				change_reason, source_evidence_ref, created_by_principal_id,
				approved_by_principal_id
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)
			RETURNING `+profileVersionColumns,
			next.ProfileVersionID, next.TenantID, next.LegalEntityID, next.VersionNumber,
			next.LegalName, next.TradingName, next.LegalFormCode, next.LegalFormSource,
			next.LegalFormLocalText, next.RegistrationNumber, next.RegistryAuthority,
			next.RegisteredOffice, next.IncorporationJurisdictionID, next.DefaultCurrencyCode,
			next.EffectiveFrom, next.EffectiveTo, now,
			string(next.ChangeReason), next.SourceEvidenceRef, next.CreatedByPrincipalID,
			next.ApprovedByPrincipalID,
		)
		created, err := scanProfileVersion(row)
		if err != nil {
			return fmt.Errorf("insert profile version: %w", err)
		}
		out = created

		// Is the new version the one in force NOW? Only then does the
		// denormalized projection on legal_entities move.
		inForce := !next.EffectiveFrom.After(now) &&
			(next.EffectiveTo == nil || next.EffectiveTo.After(now))

		if inForce {
			ct, err := tx.Exec(ctx, `
				UPDATE legal_entities
				   SET legal_name           = $1,
				       trading_name         = $2,
				       registration_number  = $3,
				       default_currency_code = COALESCE($4::varchar, default_currency_code),
				       record_version       = record_version + 1,
				       updated_at           = $5,
				       updated_by_principal_id = $6
				 WHERE legal_entity_id = $7 AND tenant_id = $8 AND record_version = $9`,
				created.LegalName, created.TradingName, created.RegistrationNumber,
				created.DefaultCurrencyCode, now, created.CreatedByPrincipalID,
				legalEntityID, tid, expectedVersion)
			if err != nil {
				return fmt.Errorf("project onto legal_entities: %w", err)
			}
			if ct.RowsAffected() == 0 {
				return registry.ErrConflict
			}
		}

		return s.enqueue(ctx, tx, ev)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListEntityProfileVersions is the ORG-03 ListEntityVersions read surface.
// Newest first, which is the order a reader auditing a change wants.
func (s *PgStore) ListEntityProfileVersions(ctx context.Context, legalEntityID string) ([]*domain.LegalEntityProfileVersion, error) {
	tid := domain.TenantFromContext(ctx)

	var out []*domain.LegalEntityProfileVersion
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+profileVersionColumns+`
			  FROM legal_entity_profile_versions
			 WHERE legal_entity_id = $1 AND tenant_id = $2
			 ORDER BY version_number DESC`, legalEntityID, tid)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			v, err := scanProfileVersion(rows)
			if err != nil {
				return err
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []*domain.LegalEntityProfileVersion{}
	}
	return out, nil
}

// GetEntityProfileAsOf is the ORG-03 GetLegalEntityAsOf read surface —
// "as-of entity reconstruction exact" (§4.3) and §8 NP6.
//
// asOf is BUSINESS time: the question is "what was this entity's legal name on
// the date that invoice was issued", not "what did we believe on that date".
// The two differ for a backdated correction, and business time is the one a
// financial reconstruction needs — a report restated for a period must use the
// facts that were true in that period, including ones learned later.
//
// The half-open interval [effective_from, effective_to) is deliberate: a
// version effective from midnight and one superseding it at the same instant
// must not both match, and the boundary belongs to the later one.
func (s *PgStore) GetEntityProfileAsOf(ctx context.Context, legalEntityID string, asOf time.Time) (*domain.EntityAsOf, error) {
	tid := domain.TenantFromContext(ctx)

	var out domain.EntityAsOf
	out.AsOf = asOf

	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			SELECT legal_entity_id, tenant_id, entity_code, entity_type, entity_status
			  FROM legal_entities WHERE legal_entity_id = $1 AND tenant_id = $2`,
			legalEntityID, tid,
		).Scan(&out.LegalEntityID, &out.TenantID, &out.EntityCode,
			&out.EntityType, &out.EntityStatus); err != nil {
			return err
		}

		row := tx.QueryRow(ctx, `
			SELECT `+profileVersionColumns+`
			  FROM legal_entity_profile_versions
			 WHERE legal_entity_id = $1 AND tenant_id = $2
			   AND effective_from <= $3
			   AND (effective_to IS NULL OR effective_to > $3)
			 ORDER BY effective_from DESC, version_number DESC
			 LIMIT 1`, legalEntityID, tid, asOf)
		v, err := scanProfileVersion(row)
		if errors.Is(err, pgx.ErrNoRows) {
			// The entity exists but had no profile at that instant — asking
			// for a date before it was incorporated. Profile stays nil rather
			// than the whole read 404ing, because "this entity did not yet
			// exist then" is a true and useful answer.
			return nil
		}
		if err != nil {
			return err
		}
		out.Profile = v
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// FindEntitiesByRegistryNumber is the ORG-03 FindByRegistryNumber read surface.
//
// Returns a LIST, not one entity, and §4.3 says why: "Natural key candidate
// registry+jurisdiction is dedup signal not universal identifier". Returning a
// single entity would present a dedup hint as an identity lookup, which is the
// assumption that produces silent merges.
func (s *PgStore) FindEntitiesByRegistryNumber(ctx context.Context, registrationNumber, jurisdictionID string) ([]*domain.LegalEntity, error) {
	tid := domain.TenantFromContext(ctx)

	var out []*domain.LegalEntity
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		q := `
			SELECT legal_entity_id, tenant_id, entity_code, legal_name, trading_name,
			       registration_number, tax_identity_bundle_id, entity_type,
			       incorporation_date, default_currency_code, fiscal_calendar_id,
			       parent_legal_entity_id, entity_status, primary_jurisdiction_id,
			       data_residency_policy_id, record_version, created_at, updated_at,
			       created_by_principal_id, updated_by_principal_id
			  FROM legal_entities
			 WHERE tenant_id = $1 AND registration_number = $2`
		args := []any{tid, registrationNumber}
		if jurisdictionID != "" {
			q += ` AND primary_jurisdiction_id = $3`
			args = append(args, jurisdictionID)
		}
		q += ` ORDER BY created_at`

		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e domain.LegalEntity
			if err := rows.Scan(
				&e.LegalEntityID, &e.TenantID, &e.EntityCode, &e.LegalName, &e.TradingName,
				&e.RegistrationNumber, &e.TaxIdentityBundleID, &e.EntityType,
				&e.IncorporationDate, &e.DefaultCurrencyCode, &e.FiscalCalendarID,
				&e.ParentLegalEntityID, &e.EntityStatus, &e.PrimaryJurisdictionID,
				&e.DataResidencyPolicyID, &e.RecordVersion, &e.CreatedAt, &e.UpdatedAt,
				&e.CreatedByPrincipalID, &e.UpdatedByPrincipalID,
			); err != nil {
				return err
			}
			out = append(out, &e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []*domain.LegalEntity{}
	}
	return out, nil
}

// FindActiveEntityByRegistry is the §8 NP5 duplicate probe: does an ACTIVE
// entity in this jurisdiction already claim this registry number?
//
// Scoped to ACTIVE deliberately. NP5's wording is "two ACTIVE entities", and a
// dissolved entity's registry number is commonly reissued or legitimately
// reused by a successor — quarantining against it would block a lawful
// re-registration.
func (s *PgStore) FindActiveEntityByRegistry(ctx context.Context, registrationNumber, jurisdictionID string) (*domain.LegalEntity, error) {
	tid := domain.TenantFromContext(ctx)
	if registrationNumber == "" || jurisdictionID == "" {
		return nil, nil
	}

	var e domain.LegalEntity
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT legal_entity_id, tenant_id, entity_code, legal_name, trading_name,
			       registration_number, tax_identity_bundle_id, entity_type,
			       incorporation_date, default_currency_code, fiscal_calendar_id,
			       parent_legal_entity_id, entity_status, primary_jurisdiction_id,
			       data_residency_policy_id, record_version, created_at, updated_at,
			       created_by_principal_id, updated_by_principal_id
			  FROM legal_entities
			 WHERE tenant_id = $1 AND registration_number = $2
			   AND primary_jurisdiction_id = $3 AND entity_status = 'ACTIVE'
			 LIMIT 1`, tid, registrationNumber, jurisdictionID,
		).Scan(
			&e.LegalEntityID, &e.TenantID, &e.EntityCode, &e.LegalName, &e.TradingName,
			&e.RegistrationNumber, &e.TaxIdentityBundleID, &e.EntityType,
			&e.IncorporationDate, &e.DefaultCurrencyCode, &e.FiscalCalendarID,
			&e.ParentLegalEntityID, &e.EntityStatus, &e.PrimaryJurisdictionID,
			&e.DataResidencyPolicyID, &e.RecordVersion, &e.CreatedAt, &e.UpdatedAt,
			&e.CreatedByPrincipalID, &e.UpdatedByPrincipalID,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// ---------------------------------------------------------------------------
// ORG-03 — registry conflict quarantine (§8 NP5)
// ---------------------------------------------------------------------------

// RecordRegistryConflict quarantines a rejected duplicate-registry claim.
func (s *PgStore) RecordRegistryConflict(ctx context.Context, c *domain.EntityRegistryConflict) error {
	tid := tenantFromCtxOrFallback(ctx, c.TenantID)

	payload, err := json.Marshal(c.AttemptedPayload)
	if err != nil {
		return fmt.Errorf("marshal attempted payload: %w", err)
	}

	return s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO entity_registry_conflicts (
				conflict_id, tenant_id, registration_number, jurisdiction_id,
				existing_legal_entity_id, attempted_payload, status,
				detected_at, detected_by_principal_id, correlation_id
			) VALUES ($1, $2, $3, $4, $5, $6, 'OPEN', $7, $8, $9)`,
			c.ConflictID, c.TenantID, c.RegistrationNumber, c.JurisdictionID,
			c.ExistingLegalEntityID, payload, time.Now().UTC(),
			c.DetectedByPrincipalID, nullableString(derefString(c.CorrelationID)),
		)
		return err
	})
}

// ListRegistryConflicts returns quarantined conflicts for the caller's tenant.
func (s *PgStore) ListRegistryConflicts(ctx context.Context, openOnly bool) ([]*domain.EntityRegistryConflict, error) {
	tid := domain.TenantFromContext(ctx)

	var out []*domain.EntityRegistryConflict
	err := s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		q := `
			SELECT conflict_id, tenant_id, registration_number, jurisdiction_id,
			       existing_legal_entity_id, attempted_payload, status,
			       resolution_note, resolved_by_principal_id, resolved_at,
			       detected_at, detected_by_principal_id, correlation_id
			  FROM entity_registry_conflicts
			 WHERE tenant_id = $1`
		if openOnly {
			q += ` AND status = 'OPEN'`
		}
		q += ` ORDER BY detected_at DESC`

		rows, err := tx.Query(ctx, q, tid)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c domain.EntityRegistryConflict
			var payload []byte
			if err := rows.Scan(
				&c.ConflictID, &c.TenantID, &c.RegistrationNumber, &c.JurisdictionID,
				&c.ExistingLegalEntityID, &payload, &c.Status,
				&c.ResolutionNote, &c.ResolvedByPrincipalID, &c.ResolvedAt,
				&c.DetectedAt, &c.DetectedByPrincipalID, &c.CorrelationID,
			); err != nil {
				return err
			}
			if len(payload) > 0 {
				_ = json.Unmarshal(payload, &c.AttemptedPayload)
			}
			out = append(out, &c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []*domain.EntityRegistryConflict{}
	}
	return out, nil
}

// ResolveRegistryConflict records a human's conclusion about a conflict.
//
// The guard is `status = 'OPEN'`, so resolving an already-resolved conflict
// affects zero rows and is reported as a conflict rather than overwriting the
// first resolver's conclusion.
func (s *PgStore) ResolveRegistryConflict(ctx context.Context, conflictID string, status domain.RegistryConflictStatus, note, actorID string) error {
	tid := domain.TenantFromContext(ctx)

	return s.withRLS(ctx, tid, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx, `
			UPDATE entity_registry_conflicts
			   SET status = $1, resolution_note = $2,
			       resolved_by_principal_id = $3, resolved_at = $4
			 WHERE conflict_id = $5 AND tenant_id = $6 AND status = 'OPEN'`,
			string(status), nullableString(note), actorID, time.Now().UTC(),
			conflictID, tid)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return registry.ErrConflict
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// nullableString returns nil for the empty string so an absent optional value
// is stored as SQL NULL rather than ”. The distinction matters for the
// approved_by_principal_id CHECK constraint, which compares against the actor:
// ” would compare unequal and let an unapproved command look approved.
func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullableStatus(s domain.TenantStatus) *string {
	if s == "" {
		return nil
	}
	v := string(s)
	return &v
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
