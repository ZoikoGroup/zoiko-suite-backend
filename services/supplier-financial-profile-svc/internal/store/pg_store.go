// Package store implements supplier-financial-profile-svc's persistence.
//
// Every command runs in ONE transaction that (1) locks the profile row,
// (2) checks expected_version, (3) applies the change and bumps version,
// (4) appends a full-snapshot revision, (5) appends the change-event evidence
// row, (6) enqueues the domain events in the transactional outbox and
// (7) records the Idempotency-Key result — all or nothing.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/supplier-financial-profile-svc/internal/domain"
	"zoiko.io/supplier-financial-profile-svc/internal/middleware"
	"zoiko.io/supplier-financial-profile-svc/internal/outbox"
)

// isInvalidUUID reports whether err is Postgres's "invalid input syntax for
// type uuid" error (SQLSTATE 22P02), so a malformed ID is "not found" rather
// than a false "store unavailable".
func isInvalidUUID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

// isExclusionViolation reports SQLSTATE 23P01 — a new PaymentTermsPeriod's
// effective range overlapping an existing one for the same profile.
func isExclusionViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23P01"
}

// nullString bridges a nullable TEXT column onto a plain string field.
type nullString struct{ dest *string }

func (n *nullString) Scan(src interface{}) error {
	if src == nil {
		*n.dest = ""
		return nil
	}
	s, _ := src.(string)
	*n.dest = s
	return nil
}

// Store is the interface the handler depends on.
type Store interface {
	CreateProfile(ctx context.Context, tenantID string, req domain.CreateProfileRequest, principalID string, idem *domain.IdemScope) (*domain.SupplierFinancialProfile, error)
	FindProfile(ctx context.Context, profileID string) (*domain.SupplierFinancialProfile, error)
	FindProfileBySupplier(ctx context.Context, legalEntityID, supplierRef string) (*domain.SupplierFinancialProfile, error)
	ListProfiles(ctx context.Context) ([]domain.SupplierFinancialProfile, error)
	FindProfileAsOf(ctx context.Context, profileID string, at time.Time) (*domain.ProfileAsOf, error)
	ListProfileHistory(ctx context.Context, profileID string) ([]domain.ProfileRevision, error)
	LastPayeeChange(ctx context.Context, profileID string) (*domain.LastPayeeChange, error)

	// Transition applies a status command (activate, hold, release, suspend,
	// unsuspend, retire).
	Transition(ctx context.Context, profileID string, cmd domain.Command, expectedVersion *int, reason, principalID string, idem *domain.IdemScope) (*domain.SupplierFinancialProfile, error)
	AmendProfile(ctx context.Context, profileID string, req domain.AmendProfileRequest, principalID string, idem *domain.IdemScope) (*domain.AmendResult, error)

	ChangePaymentTerms(ctx context.Context, profileID string, req domain.ChangePaymentTermsRequest, principalID string, idem *domain.IdemScope) (*domain.PaymentTermsPeriod, error)
	ListPaymentTerms(ctx context.Context, profileID string) ([]domain.PaymentTermsPeriod, error)

	ProposeHighRiskChange(ctx context.Context, profileID string, req domain.ProposeHighRiskChangeRequest, principalID string, idem *domain.IdemScope) (*domain.HighRiskChangeRequest, error)
	FindChangeRequest(ctx context.Context, changeRequestID string) (*domain.HighRiskChangeRequest, error)
	DecideHighRiskChange(ctx context.Context, changeRequestID string, req domain.DecideHighRiskChangeRequest, principalID string, idem *domain.IdemScope) (*domain.HighRiskChangeRequest, *domain.SupplierFinancialProfile, error)

	ListChangeEvents(ctx context.Context, profileID string) ([]domain.ProfileChangeEvent, error)

	// LookupIdempotency returns the stored result for key in the context's tenant, or nil.
	LookupIdempotency(ctx context.Context, key string) (*domain.IdemRecord, error)
}

type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

func NewPgStore(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

func (s *PgStore) withTenant(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", middleware.TenantFromContext(ctx)); err != nil {
		return fmt.Errorf("set_config app.tenant_id: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// tenantPred scopes a query to the transaction's tenant. RLS enforces the same,
// but the runtime role is a superuser (which bypasses RLS), so every query
// also carries the predicate explicitly.
const tenantPred = `tenant_id::text = NULLIF(current_setting('app.tenant_id', true), '')`

// txResult is the response a command stored under its Idempotency-Key.
type txResult struct {
	Status int
	Body   any
}

// run executes a command transaction. With an idempotency scope it first
// reserves the key (a conflicting concurrent/earlier use rolls back with
// ErrIdempotencyRace so the handler can replay the stored result) and stores
// the response in the same transaction.
func (s *PgStore) run(ctx context.Context, idem *domain.IdemScope, fn func(tx pgx.Tx) (txResult, error)) error {
	return s.withTenant(ctx, func(tx pgx.Tx) error {
		tenant := middleware.TenantFromContext(ctx)
		if idem != nil {
			tag, err := tx.Exec(ctx, `
				INSERT INTO supplier_profile_idempotency (tenant_id, idempotency_key, operation, request_hash, status_code, response)
				VALUES ($1, $2, $3, $4, 0, '{}'::jsonb) ON CONFLICT DO NOTHING`,
				tenant, idem.Key, idem.Operation, idem.RequestHash)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return domain.ErrIdempotencyRace
			}
		}
		res, err := fn(tx)
		if err != nil {
			return err
		}
		if idem != nil {
			body, err := json.Marshal(res.Body)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE supplier_profile_idempotency SET status_code = $3, response = $4::jsonb
				WHERE tenant_id = $1 AND idempotency_key = $2`,
				tenant, idem.Key, res.Status, body); err != nil {
				return err
			}
		}
		return nil
	})
}

// fail maps a transaction error: domain sentinels pass through untouched,
// everything else is a store-unavailable failure.
func (s *PgStore) fail(op string, err error) error {
	if err == nil {
		return nil
	}
	if domain.IsDomain(err) {
		return err
	}
	s.log.Error("pg "+op+" failed", zap.Error(err))
	return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nz(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

func tenantOf(p *domain.SupplierFinancialProfile) string {
	if p.TenantID == nil {
		return ""
	}
	return *p.TenantID
}

// ── profile row ──────────────────────────────────────────────────────────────

const profileColumns = `
	profile_id, tenant_id, legal_entity_id, supplier_ref, status, version, payee_reference, category,
	procurement_category_refs, invoice_channel, payment_method_preference, tax_withholding_ref,
	tax_classification_refs, ap_account_policy, risk_control_flags, hold_reason,
	created_at, created_by_principal_id, updated_at`

func scanProfile(row pgx.Row) (*domain.SupplierFinancialProfile, error) {
	p := &domain.SupplierFinancialProfile{}
	err := row.Scan(&p.ProfileID, &p.TenantID, &p.LegalEntityID, &p.SupplierRef, &p.Status, &p.Version, &nullString{&p.PayeeReference},
		&nullString{&p.Category}, &p.ProcurementCategoryRefs, &nullString{&p.InvoiceChannel}, &nullString{&p.PaymentMethodPreference},
		&nullString{&p.TaxWithholdingRef}, &p.TaxClassificationRefs, &nullString{&p.APAccountPolicy}, &p.RiskControlFlags,
		&nullString{&p.HoldReason}, &p.CreatedAt, &p.CreatedByPrincipalID, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (s *PgStore) loadForUpdate(ctx context.Context, tx pgx.Tx, profileID string) (*domain.SupplierFinancialProfile, error) {
	p, err := scanProfile(tx.QueryRow(ctx, `SELECT `+profileColumns+` FROM supplier_financial_profiles WHERE profile_id = $1 AND `+tenantPred+` FOR UPDATE`, profileID))
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrProfileNotFound
	}
	return p, err
}

func checkVersion(cur *domain.SupplierFinancialProfile, expected *int) error {
	if expected != nil && *expected != cur.Version {
		return domain.ErrStaleVersion
	}
	return nil
}

// changeMeta carries who/why for one applied change.
type changeMeta struct {
	actor, approver string
	extra           map[string]any // merged into the outbox payload
}

// applyChange writes next over cur (bumping version), then the revision,
// evidence row and outbox events. cur must be the row just locked FOR UPDATE.
func (s *PgStore) applyChange(ctx context.Context, tx pgx.Tx, cur *domain.SupplierFinancialProfile, next domain.SupplierFinancialProfile, o domain.Outcome, m changeMeta) (*domain.SupplierFinancialProfile, error) {
	p, err := scanProfile(tx.QueryRow(ctx, `
		UPDATE supplier_financial_profiles SET
			status = $2, payee_reference = $3, category = $4, procurement_category_refs = $5, invoice_channel = $6,
			payment_method_preference = $7, tax_withholding_ref = $8, tax_classification_refs = $9,
			ap_account_policy = $10, risk_control_flags = $11, hold_reason = $12,
			version = version + 1, updated_at = NOW()
		WHERE profile_id = $1
		RETURNING `+profileColumns,
		cur.ProfileID, next.Status, strPtrOrNil(next.PayeeReference), strPtrOrNil(next.Category), nz(next.ProcurementCategoryRefs),
		strPtrOrNil(next.InvoiceChannel), strPtrOrNil(next.PaymentMethodPreference), strPtrOrNil(next.TaxWithholdingRef),
		nz(next.TaxClassificationRefs), strPtrOrNil(next.APAccountPolicy), nz(next.RiskControlFlags), strPtrOrNil(next.HoldReason),
	))
	if err != nil {
		return nil, err
	}
	if err := s.recordRevision(ctx, tx, p, cur, o, m); err != nil {
		return nil, err
	}
	if err := s.recordEvent(ctx, tx, p.TenantID, p.ProfileID, o.EventType, o.Prior, o.New, o.Reason, m.actor); err != nil {
		return nil, err
	}
	if err := s.enqueue(ctx, tx, p, domain.OutboxEventTypes(o.ChangeType), o, m, cur.Version); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *PgStore) recordRevision(ctx context.Context, tx pgx.Tx, p, prior *domain.SupplierFinancialProfile, o domain.Outcome, m changeMeta) error {
	snap, err := json.Marshal(p)
	if err != nil {
		return err
	}
	var priorJSON []byte
	if prior != nil {
		if priorJSON, err = json.Marshal(prior); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO supplier_profile_revisions
			(revision_id, tenant_id, profile_id, version, change_type, snapshot, prior_snapshot,
			 actor_principal_id, approver_principal_id, reason, payee_related, effective_from)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7::jsonb, $8, $9, $10, $11, $12)`,
		uuid.New().String(), tenantOf(p), p.ProfileID, p.Version, o.ChangeType, snap, priorJSON,
		m.actor, strPtrOrNil(m.approver), strPtrOrNil(o.Reason), o.PayeeRelated, p.UpdatedAt)
	return err
}

func (s *PgStore) recordEvent(ctx context.Context, tx pgx.Tx, tenantID *string, profileID, eventType, prior, new, reason, actorPrincipalID string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO profile_change_events (event_id, tenant_id, profile_id, event_type, prior_value, new_value, reason, actor_principal_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		uuid.New().String(), tenantID, profileID, eventType, strPtrOrNil(prior), strPtrOrNil(new), strPtrOrNil(reason), actorPrincipalID,
	)
	return err
}

// enqueue writes one outbox row per event type (spec name plus any legacy
// alias) in the caller's transaction.
func (s *PgStore) enqueue(ctx context.Context, tx pgx.Tx, p *domain.SupplierFinancialProfile, eventTypes []string, o domain.Outcome, m changeMeta, priorVersion int) error {
	payload := map[string]any{
		"change_type":        o.ChangeType,
		"profile":            p,
		"prior_version":      priorVersion,
		"version":            p.Version,
		"actor_principal_id": m.actor,
		"reason":             o.Reason,
	}
	if m.approver != "" {
		payload["approver_principal_id"] = m.approver
	}
	for k, v := range m.extra {
		payload[k] = v
	}
	return s.enqueueRaw(ctx, tx, p, eventTypes, m.actor, payload)
}

func (s *PgStore) enqueueRaw(ctx context.Context, tx pgx.Tx, p *domain.SupplierFinancialProfile, eventTypes []string, actor string, payload any) error {
	var tenantID *string = p.TenantID
	corr := middleware.CorrelationFromContext(ctx)
	var corrPtr *string
	if corr != "" {
		corrPtr = &corr
	}
	for _, et := range eventTypes {
		id := uuid.NewString()
		env := outbox.NewVariantBEnvelope(id, et, p.ProfileID, tenantID, &actor, corrPtr, payload)
		if err := outbox.Insert(ctx, tx, outbox.Event{
			OutboxEventID: id, AggregateType: "supplier_financial_profile", AggregateID: p.ProfileID, EventType: et,
			TenantID: tenantID, LegalEntityID: p.LegalEntityID, ActorID: &actor, CorrelationID: corrPtr, Payload: env,
		}); err != nil {
			return err
		}
	}
	return nil
}

// ── profiles ─────────────────────────────────────────────────────────────────

func (s *PgStore) CreateProfile(ctx context.Context, tenantID string, req domain.CreateProfileRequest, principalID string, idem *domain.IdemScope) (*domain.SupplierFinancialProfile, error) {
	id := uuid.New().String()
	var p *domain.SupplierFinancialProfile
	err := s.run(ctx, idem, func(tx pgx.Tx) (txResult, error) {
		var err error
		p, err = scanProfile(tx.QueryRow(ctx, `
			INSERT INTO supplier_financial_profiles
				(profile_id, tenant_id, legal_entity_id, supplier_ref, status, version, category, procurement_category_refs,
				 invoice_channel, created_at, created_by_principal_id, updated_at)
			VALUES ($1, $2, $3, $4, 'DRAFT', 1, $5, $6, $7, NOW(), $8, NOW())
			ON CONFLICT DO NOTHING
			RETURNING `+profileColumns,
			id, tenantID, req.LegalEntityID, req.SupplierRef, strPtrOrNil(req.Category), nz(req.ProcurementCategoryRefs),
			strPtrOrNil(req.InvoiceChannel), principalID,
		))
		if errors.Is(err, pgx.ErrNoRows) {
			return txResult{}, domain.ErrDuplicateProfile
		}
		if isInvalidUUID(err) {
			return txResult{}, domain.ErrInvalidTenant
		}
		if err != nil {
			return txResult{}, err
		}
		o := domain.Outcome{ChangeType: domain.ChangeCreated, EventType: domain.EventProfileCreated, New: req.SupplierRef}
		m := changeMeta{actor: principalID}
		if err := s.recordRevision(ctx, tx, p, nil, o, m); err != nil {
			return txResult{}, err
		}
		if err := s.recordEvent(ctx, tx, p.TenantID, id, o.EventType, "", req.SupplierRef, "", principalID); err != nil {
			return txResult{}, err
		}
		if err := s.enqueue(ctx, tx, p, domain.OutboxEventTypes(o.ChangeType), o, m, 0); err != nil {
			return txResult{}, err
		}
		return txResult{Status: 201, Body: p}, nil
	})
	if err != nil {
		return nil, s.fail("CreateProfile", err)
	}
	return p, nil
}

func (s *PgStore) FindProfile(ctx context.Context, profileID string) (*domain.SupplierFinancialProfile, error) {
	var p *domain.SupplierFinancialProfile
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		p, err = scanProfile(tx.QueryRow(ctx, `SELECT `+profileColumns+` FROM supplier_financial_profiles WHERE profile_id = $1 AND `+tenantPred, profileID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrProfileNotFound
	}
	if err != nil {
		return nil, s.fail("FindProfile", err)
	}
	return p, nil
}

// FindProfileBySupplier returns the live profile for (legal entity, supplier),
// else the most recent retired one.
func (s *PgStore) FindProfileBySupplier(ctx context.Context, legalEntityID, supplierRef string) (*domain.SupplierFinancialProfile, error) {
	var p *domain.SupplierFinancialProfile
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		p, err = scanProfile(tx.QueryRow(ctx, `
			SELECT `+profileColumns+` FROM supplier_financial_profiles
			WHERE legal_entity_id = $1 AND supplier_ref = $2 AND `+tenantPred+`
			ORDER BY (status = 'RETIRED'), created_at DESC LIMIT 1`, legalEntityID, supplierRef))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrProfileNotFound
	}
	if err != nil {
		return nil, s.fail("FindProfileBySupplier", err)
	}
	return p, nil
}

func (s *PgStore) ListProfiles(ctx context.Context) ([]domain.SupplierFinancialProfile, error) {
	var out []domain.SupplierFinancialProfile
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+profileColumns+` FROM supplier_financial_profiles WHERE `+tenantPred+` ORDER BY created_at DESC`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanProfile(rows)
			if err != nil {
				return err
			}
			out = append(out, *p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, s.fail("ListProfiles", err)
	}
	return out, nil
}

func (s *PgStore) Transition(ctx context.Context, profileID string, cmd domain.Command, expectedVersion *int, reason, principalID string, idem *domain.IdemScope) (*domain.SupplierFinancialProfile, error) {
	var out *domain.SupplierFinancialProfile
	err := s.run(ctx, idem, func(tx pgx.Tx) (txResult, error) {
		cur, err := s.loadForUpdate(ctx, tx, profileID)
		if err != nil {
			return txResult{}, err
		}
		if err := checkVersion(cur, expectedVersion); err != nil {
			return txResult{}, err
		}
		next, o, err := domain.ApplyCommand(*cur, cmd, reason)
		if err != nil {
			return txResult{}, err
		}
		out, err = s.applyChange(ctx, tx, cur, next, o, changeMeta{actor: principalID})
		if err != nil {
			return txResult{}, err
		}
		return txResult{Status: 200, Body: out}, nil
	})
	if err != nil {
		return nil, s.fail("Transition", err)
	}
	return out, nil
}

func (s *PgStore) AmendProfile(ctx context.Context, profileID string, req domain.AmendProfileRequest, principalID string, idem *domain.IdemScope) (*domain.AmendResult, error) {
	proposals := req.HighRiskProposals()
	if !req.LowRiskPresent() && len(proposals) == 0 {
		return nil, domain.ErrNoChanges
	}
	for _, pr := range proposals {
		if err := domain.ValidateHighRiskValue(pr.Field, pr.NewValue); err != nil {
			return nil, err
		}
	}
	var res *domain.AmendResult
	err := s.run(ctx, idem, func(tx pgx.Tx) (txResult, error) {
		cur, err := s.loadForUpdate(ctx, tx, profileID)
		if err != nil {
			return txResult{}, err
		}
		if err := checkVersion(cur, req.ExpectedVersion); err != nil {
			return txResult{}, err
		}
		if cur.Status == domain.StatusRetired {
			return txResult{}, fmt.Errorf("%w: profile is RETIRED", domain.ErrInvalidTransition)
		}
		res = &domain.AmendResult{Profile: *cur}
		if req.LowRiskPresent() {
			next, o, err := domain.ApplyAmendLowRisk(*cur, req)
			if err != nil {
				return txResult{}, err
			}
			p, err := s.applyChange(ctx, tx, cur, next, o, changeMeta{actor: principalID})
			if err != nil {
				return txResult{}, err
			}
			res.Profile = *p
		}
		for _, pr := range proposals {
			c, err := s.insertChangeRequest(ctx, tx, cur, pr, principalID)
			if err != nil {
				return txResult{}, err
			}
			res.PendingChanges = append(res.PendingChanges, *c)
		}
		return txResult{Status: res.HTTPStatus(), Body: res.Body()}, nil
	})
	if err != nil {
		return nil, s.fail("AmendProfile", err)
	}
	return res, nil
}

// ── payment terms ────────────────────────────────────────────────────────────

// ChangePaymentTerms relies on the migration's EXCLUDE constraint to reject an
// overlapping effective period atomically (a database-enforced invariant, not
// an application-level date check a race could slip past). A successful change
// also bumps the profile version and appends a revision.
func (s *PgStore) ChangePaymentTerms(ctx context.Context, profileID string, req domain.ChangePaymentTermsRequest, principalID string, idem *domain.IdemScope) (*domain.PaymentTermsPeriod, error) {
	id := uuid.New().String()
	var t domain.PaymentTermsPeriod
	err := s.run(ctx, idem, func(tx pgx.Tx) (txResult, error) {
		cur, err := s.loadForUpdate(ctx, tx, profileID)
		if err != nil {
			return txResult{}, err
		}
		if err := checkVersion(cur, req.ExpectedVersion); err != nil {
			return txResult{}, err
		}
		if cur.Status == domain.StatusRetired {
			return txResult{}, fmt.Errorf("%w: profile is RETIRED", domain.ErrInvalidTransition)
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO payment_terms_periods (payment_terms_id, tenant_id, profile_id, terms_code, effective_from, effective_to, created_at, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, NOW(), $7)
			RETURNING payment_terms_id, tenant_id, profile_id, terms_code, effective_from, effective_to, created_at, created_by_principal_id`,
			id, cur.TenantID, profileID, req.TermsCode, req.EffectiveFrom, req.EffectiveTo, principalID,
		).Scan(&t.PaymentTermsID, &t.TenantID, &t.ProfileID, &t.TermsCode, &t.EffectiveFrom, &t.EffectiveTo, &t.CreatedAt, &t.CreatedByPrincipalID); err != nil {
			if isExclusionViolation(err) {
				return txResult{}, domain.ErrOverlappingPaymentTerms
			}
			return txResult{}, err
		}
		o := domain.Outcome{ChangeType: domain.ChangePaymentTermsChanged, EventType: domain.EventPaymentTermsChanged, New: req.TermsCode}
		if _, err := s.applyChange(ctx, tx, cur, *cur, o, changeMeta{actor: principalID, extra: map[string]any{"payment_terms": t}}); err != nil {
			return txResult{}, err
		}
		return txResult{Status: 201, Body: t}, nil
	})
	if err != nil {
		return nil, s.fail("ChangePaymentTerms", err)
	}
	return &t, nil
}

func (s *PgStore) ListPaymentTerms(ctx context.Context, profileID string) ([]domain.PaymentTermsPeriod, error) {
	var out []domain.PaymentTermsPeriod
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT payment_terms_id, tenant_id, profile_id, terms_code, effective_from, effective_to, created_at, created_by_principal_id
			FROM payment_terms_periods WHERE profile_id = $1 AND `+tenantPred+` ORDER BY effective_from ASC`, profileID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t domain.PaymentTermsPeriod
			if err := rows.Scan(&t.PaymentTermsID, &t.TenantID, &t.ProfileID, &t.TermsCode, &t.EffectiveFrom, &t.EffectiveTo, &t.CreatedAt, &t.CreatedByPrincipalID); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	if isInvalidUUID(err) {
		return nil, nil
	}
	if err != nil {
		return nil, s.fail("ListPaymentTerms", err)
	}
	return out, nil
}

// ── high-risk change requests ────────────────────────────────────────────────

const changeRequestColumns = `
	change_request_id, tenant_id, profile_id, field, old_value, new_value, reason, status,
	proposed_by_principal_id, proposed_at, decided_by_principal_id, decided_at, decision_reason`

func scanChangeRequest(row pgx.Row) (*domain.HighRiskChangeRequest, error) {
	c := &domain.HighRiskChangeRequest{}
	err := row.Scan(&c.ChangeRequestID, &c.TenantID, &c.ProfileID, &c.Field, &nullString{&c.OldValue}, &c.NewValue,
		&nullString{&c.Reason}, &c.Status, &c.ProposedByPrincipalID, &c.ProposedAt, &c.DecidedByPrincipalID, &c.DecidedAt, &nullString{&c.DecisionReason})
	if err != nil {
		return nil, err
	}
	return c, nil
}

// insertChangeRequest records a pending maker-checker request for cur.
func (s *PgStore) insertChangeRequest(ctx context.Context, tx pgx.Tx, cur *domain.SupplierFinancialProfile, req domain.ProposeHighRiskChangeRequest, principalID string) (*domain.HighRiskChangeRequest, error) {
	old := domain.HighRiskFieldValue(*cur, req.Field)
	c, err := scanChangeRequest(tx.QueryRow(ctx, `
		INSERT INTO high_risk_change_requests (`+changeRequestColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'PENDING_APPROVAL', $8, NOW(), NULL, NULL, NULL)
		RETURNING `+changeRequestColumns,
		uuid.New().String(), cur.TenantID, cur.ProfileID, req.Field, strPtrOrNil(old), req.NewValue, strPtrOrNil(req.Reason), principalID,
	))
	if err != nil {
		return nil, err
	}
	if err := s.recordEvent(ctx, tx, cur.TenantID, cur.ProfileID, domain.EventHighRiskProposed, old, req.NewValue, req.Reason, principalID); err != nil {
		return nil, err
	}
	return c, nil
}

func (s *PgStore) ProposeHighRiskChange(ctx context.Context, profileID string, req domain.ProposeHighRiskChangeRequest, principalID string, idem *domain.IdemScope) (*domain.HighRiskChangeRequest, error) {
	if !req.Field.Valid() {
		return nil, fmt.Errorf("%w: unknown high-risk field", domain.ErrValidation)
	}
	if err := domain.ValidateHighRiskValue(req.Field, req.NewValue); err != nil {
		return nil, err
	}
	var c *domain.HighRiskChangeRequest
	err := s.run(ctx, idem, func(tx pgx.Tx) (txResult, error) {
		cur, err := s.loadForUpdate(ctx, tx, profileID)
		if err != nil {
			return txResult{}, err
		}
		if err := checkVersion(cur, req.ExpectedVersion); err != nil {
			return txResult{}, err
		}
		if cur.Status == domain.StatusRetired {
			return txResult{}, fmt.Errorf("%w: profile is RETIRED", domain.ErrInvalidTransition)
		}
		c, err = s.insertChangeRequest(ctx, tx, cur, req, principalID)
		if err != nil {
			return txResult{}, err
		}
		return txResult{Status: 201, Body: c}, nil
	})
	if err != nil {
		return nil, s.fail("ProposeHighRiskChange", err)
	}
	return c, nil
}

func (s *PgStore) FindChangeRequest(ctx context.Context, changeRequestID string) (*domain.HighRiskChangeRequest, error) {
	var c *domain.HighRiskChangeRequest
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		c, err = scanChangeRequest(tx.QueryRow(ctx, `SELECT `+changeRequestColumns+` FROM high_risk_change_requests WHERE change_request_id = $1 AND `+tenantPred, changeRequestID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrChangeRequestNotFound
	}
	if err != nil {
		return nil, s.fail("FindChangeRequest", err)
	}
	return c, nil
}

// DecideHighRiskChange applies or rejects a pending change atomically. The
// handler performs the authorization-svc own-object SoD check BEFORE calling;
// this method re-asserts maker != checker and that the request is still
// PENDING_APPROVAL, that the profile version is the one the approver reviewed
// and that the field still holds the value the proposal was made against.
func (s *PgStore) DecideHighRiskChange(ctx context.Context, changeRequestID string, req domain.DecideHighRiskChangeRequest, principalID string, idem *domain.IdemScope) (*domain.HighRiskChangeRequest, *domain.SupplierFinancialProfile, error) {
	var c *domain.HighRiskChangeRequest
	var p *domain.SupplierFinancialProfile
	err := s.run(ctx, idem, func(tx pgx.Tx) (txResult, error) {
		newStatus := domain.ChangeRequestRejected
		eventType := domain.EventHighRiskRejected
		if req.Approve {
			newStatus = domain.ChangeRequestApproved
			eventType = domain.EventHighRiskApplied
		}

		var err error
		c, err = scanChangeRequest(tx.QueryRow(ctx, `
			UPDATE high_risk_change_requests SET
				status = $2, decided_by_principal_id = $3, decided_at = NOW(), decision_reason = $4
			WHERE change_request_id = $1 AND status = 'PENDING_APPROVAL' AND `+tenantPred+`
			RETURNING `+changeRequestColumns,
			changeRequestID, newStatus, principalID, strPtrOrNil(req.Reason),
		))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
				return txResult{}, domain.ErrChangeRequestNotPending
			}
			return txResult{}, err
		}
		if c.ProposedByPrincipalID == principalID {
			return txResult{}, domain.ErrSoDConflict
		}

		cur, err := s.loadForUpdate(ctx, tx, c.ProfileID)
		if err != nil {
			return txResult{}, err
		}
		if err := checkVersion(cur, req.ExpectedVersion); err != nil {
			return txResult{}, err
		}

		if req.Approve {
			if cur.Status == domain.StatusRetired {
				return txResult{}, fmt.Errorf("%w: profile is RETIRED", domain.ErrInvalidTransition)
			}
			if domain.HighRiskFieldValue(*cur, c.Field) != c.OldValue {
				return txResult{}, domain.ErrStaleVersion
			}
			next := *cur
			if err := domain.ApplyHighRiskField(&next, c.Field, c.NewValue); err != nil {
				return txResult{}, err
			}
			o := domain.Outcome{
				ChangeType: domain.ChangeHighRiskApplied, EventType: eventType, Prior: c.OldValue, New: c.NewValue,
				Reason: req.Reason, PayeeRelated: c.Field.IsPayeeRelated(),
			}
			p, err = s.applyChange(ctx, tx, cur, next, o, changeMeta{
				actor: c.ProposedByPrincipalID, approver: principalID,
				extra: map[string]any{"field": c.Field, "change_request_id": c.ChangeRequestID},
			})
			if err != nil {
				return txResult{}, err
			}
			// The evidence row above is attributed to the proposer; record the
			// approver's decision explicitly too.
			if err := s.recordEvent(ctx, tx, c.TenantID, c.ProfileID, eventType, c.OldValue, c.NewValue, req.Reason, principalID); err != nil {
				return txResult{}, err
			}
		} else {
			p = cur
			if err := s.recordEvent(ctx, tx, c.TenantID, c.ProfileID, eventType, c.OldValue, c.NewValue, req.Reason, principalID); err != nil {
				return txResult{}, err
			}
		}
		if err := s.enqueueRaw(ctx, tx, p, []string{domain.LegacyHighRiskDecided}, principalID, c); err != nil {
			return txResult{}, err
		}
		return txResult{Status: 200, Body: map[string]any{"change_request": c, "profile": p}}, nil
	})
	if err != nil {
		return nil, nil, s.fail("DecideHighRiskChange", err)
	}
	return c, p, nil
}

// ── history / as-of ──────────────────────────────────────────────────────────

func (s *PgStore) ListChangeEvents(ctx context.Context, profileID string) ([]domain.ProfileChangeEvent, error) {
	var out []domain.ProfileChangeEvent
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT event_id, tenant_id, profile_id, event_type, prior_value, new_value, reason, actor_principal_id, created_at
			FROM profile_change_events WHERE profile_id = $1 AND `+tenantPred+` ORDER BY created_at ASC, event_id ASC`, profileID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e domain.ProfileChangeEvent
			if err := rows.Scan(&e.EventID, &e.TenantID, &e.ProfileID, &e.EventType, &nullString{&e.PriorValue}, &nullString{&e.NewValue}, &nullString{&e.Reason}, &e.ActorPrincipalID, &e.CreatedAt); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	if isInvalidUUID(err) {
		return nil, nil
	}
	if err != nil {
		return nil, s.fail("ListChangeEvents", err)
	}
	return out, nil
}

func (s *PgStore) ListProfileHistory(ctx context.Context, profileID string) ([]domain.ProfileRevision, error) {
	var out []domain.ProfileRevision
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT revision_id, profile_id, version, change_type, snapshot, prior_snapshot,
			       actor_principal_id, approver_principal_id, reason, payee_related, effective_from
			FROM supplier_profile_revisions WHERE profile_id = $1 AND `+tenantPred+` ORDER BY version ASC`, profileID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r domain.ProfileRevision
			var snap, prior []byte
			var approver, reason *string
			if err := rows.Scan(&r.RevisionID, &r.ProfileID, &r.Version, &r.ChangeType, &snap, &prior,
				&r.ActorPrincipalID, &approver, &reason, &r.PayeeRelated, &r.EffectiveFrom); err != nil {
				return err
			}
			if err := json.Unmarshal(snap, &r.Snapshot); err != nil {
				return err
			}
			if len(prior) > 0 {
				var ps domain.SupplierFinancialProfile
				if err := json.Unmarshal(prior, &ps); err != nil {
					return err
				}
				r.PriorSnapshot = &ps
			}
			if approver != nil {
				r.ApproverPrincipalID = *approver
			}
			if reason != nil {
				r.Reason = *reason
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if isInvalidUUID(err) {
		return nil, nil
	}
	if err != nil {
		return nil, s.fail("ListProfileHistory", err)
	}
	for i := 0; i+1 < len(out); i++ {
		t := out[i+1].EffectiveFrom
		out[i].EffectiveTo = &t
	}
	return out, nil
}

func (s *PgStore) FindProfileAsOf(ctx context.Context, profileID string, at time.Time) (*domain.ProfileAsOf, error) {
	res := &domain.ProfileAsOf{AsOf: at}
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var snap []byte
		if err := tx.QueryRow(ctx, `
			SELECT version, snapshot, effective_from FROM supplier_profile_revisions
			WHERE profile_id = $1 AND `+tenantPred+` AND effective_from <= $2
			ORDER BY version DESC LIMIT 1`, profileID, at).Scan(&res.RevisionVersion, &snap, &res.EffectiveFrom); err != nil {
			return err
		}
		if err := json.Unmarshal(snap, &res.Profile); err != nil {
			return err
		}
		var t domain.PaymentTermsPeriod
		err := tx.QueryRow(ctx, `
			SELECT payment_terms_id, tenant_id, profile_id, terms_code, effective_from, effective_to, created_at, created_by_principal_id
			FROM payment_terms_periods
			WHERE profile_id = $1 AND `+tenantPred+` AND tstzrange(effective_from, effective_to) @> $2::timestamptz
			LIMIT 1`, profileID, at).Scan(&t.PaymentTermsID, &t.TenantID, &t.ProfileID, &t.TermsCode, &t.EffectiveFrom, &t.EffectiveTo, &t.CreatedAt, &t.CreatedByPrincipalID)
		if err == nil {
			res.PaymentTerms = &t
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNoRevisionAsOf
	}
	if isInvalidUUID(err) {
		return nil, domain.ErrProfileNotFound
	}
	if err != nil {
		return nil, s.fail("FindProfileAsOf", err)
	}
	return res, nil
}

func (s *PgStore) LastPayeeChange(ctx context.Context, profileID string) (*domain.LastPayeeChange, error) {
	out := &domain.LastPayeeChange{ProfileID: profileID}
	var approver *string
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT actor_principal_id, approver_principal_id, effective_from, version, change_type
			FROM supplier_profile_revisions
			WHERE profile_id = $1 AND `+tenantPred+` AND payee_related ORDER BY version DESC LIMIT 1`, profileID,
		).Scan(&out.PrincipalID, &approver, &out.ChangedAt, &out.Version, &out.ChangeType)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNoPayeeChange
	}
	if isInvalidUUID(err) {
		return nil, domain.ErrProfileNotFound
	}
	if err != nil {
		return nil, s.fail("LastPayeeChange", err)
	}
	if approver != nil {
		out.ApproverPrincipalID = *approver
	}
	return out, nil
}

func (s *PgStore) LookupIdempotency(ctx context.Context, key string) (*domain.IdemRecord, error) {
	var rec domain.IdemRecord
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var resp []byte
		if err := tx.QueryRow(ctx, `
			SELECT operation, request_hash, status_code, response FROM supplier_profile_idempotency
			WHERE tenant_id = $1 AND idempotency_key = $2`, middleware.TenantFromContext(ctx), key,
		).Scan(&rec.Operation, &rec.RequestHash, &rec.StatusCode, &resp); err != nil {
			return err
		}
		rec.Response = resp
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, s.fail("LookupIdempotency", err)
	}
	return &rec, nil
}
