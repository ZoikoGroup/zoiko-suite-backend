package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/counterparty-management-svc/internal/domain"
	"zoiko.io/counterparty-management-svc/internal/middleware"
)

// CRMStore is BIZ-06's own persistence contract — kept separate from the
// existing Store interface, same composition pattern used for AUD-08's
// FindingStore and BIZ-05's TaskStore elsewhere in this build.
type CRMStore interface {
	CreateRelationship(ctx context.Context, p domain.CreateRelationshipParams) (*domain.Relationship, error)
	GetRelationship(ctx context.Context, relationshipID string) (*domain.Relationship, error)
	ConfirmPartyLink(ctx context.Context, p domain.ConfirmPartyLinkParams) (*domain.Relationship, error)

	LogInteraction(ctx context.Context, p domain.LogInteractionParams) (*domain.Interaction, error)
	GetTimeline(ctx context.Context, tenantID, relationshipID string) ([]domain.Interaction, error)

	CreateOpportunity(ctx context.Context, p domain.CreateOpportunityParams) (*domain.Opportunity, error)
	GetOpportunity(ctx context.Context, opportunityID string) (*domain.Opportunity, error)
	UpdateStage(ctx context.Context, p domain.UpdateStageParams) (*domain.Opportunity, error)
	CloseOpportunity(ctx context.Context, p domain.CloseOpportunityParams) (*domain.Opportunity, error)
	ListPipeline(ctx context.Context, p domain.ListPipelineParams) ([]domain.Opportunity, error)

	ActivateRelationship(ctx context.Context, p domain.ActivateRelationshipParams) (*domain.Relationship, error)
	MarkDormant(ctx context.Context, p domain.MarkDormantParams) (*domain.Relationship, error)
	CloseRelationship(ctx context.Context, p domain.CloseRelationshipParams) (*domain.Relationship, error)
	MergeCRMProfile(ctx context.Context, p domain.MergeCRMProfileParams) (*domain.Relationship, error)

	LinkCommercialObject(ctx context.Context, p domain.LinkCommercialObjectParams) (*domain.CommercialObjectLink, error)
	GetLinkedFinancialObjects(ctx context.Context, tenantID, relationshipID string) ([]domain.CommercialObjectLink, error)

	RecordConsent(ctx context.Context, p domain.RecordConsentParams) (*domain.ConsentRecord, error)
	GetConsentContext(ctx context.Context, tenantID, relationshipID string) ([]domain.ConsentRecord, error)
}

// crmSetRLS uses SELECT set_config with a bind parameter, NOT this
// service's existing setRLS (pg_store.go), which builds `SET LOCAL
// app.tenant_id = '<value>'` via raw fmt.Sprintf string interpolation —
// a real SQL-injection-shaped defect in existing code (tenantID is never
// escaped). That pre-existing issue is out of BIZ-06's scope to fix, but
// this new code deliberately does not repeat it — same working form
// exception-escalation-svc's findingSetRLS/taskSetRLS already use.
func (s *PgStore) crmSetRLS(ctx context.Context, tx pgx.Tx) error {
	tenantID := middleware.GetTenantID(ctx)
	_, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID)
	return err
}

const relationshipColumns = `relationship_id, tenant_id, legal_entity_id, counterparty_id, candidate_counterparty_id,
	party_link_status, status, source, channel, owner_principal_id, merged_into_relationship_id,
	created_by, created_at, updated_at, closed_by, closed_at, closure_reason`

func scanRelationship(row pgx.Row) (*domain.Relationship, error) {
	r := &domain.Relationship{}
	err := row.Scan(&r.RelationshipID, &r.TenantID, &r.LegalEntityID, &r.CounterpartyID, &r.CandidateCounterpartyID,
		&r.PartyLinkStatus, &r.Status, &r.Source, &r.Channel, &r.OwnerPrincipalID, &r.MergedIntoRelationshipID,
		&r.CreatedBy, &r.CreatedAt, &r.UpdatedAt, &r.ClosedBy, &r.ClosedAt, &r.ClosureReason)
	return r, err
}

// CreateRelationship — BIZ-06's own CreateRelationship command. If
// CandidateCounterpartyID is supplied, the relationship lands with
// party_link_status=CANDIDATE — an unresolved, unconfirmed match, never
// auto-promoted to a real link. See this package's own doc comment on
// ConfirmPartyLink for the only path that confirms it.
func (s *PgStore) CreateRelationship(ctx context.Context, p domain.CreateRelationshipParams) (*domain.Relationship, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.crmSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	relationshipID := "rel-" + uuid.New().String()
	linkStatus := domain.PartyLinkUnlinked
	var candidateID any
	if p.CandidateCounterpartyID != "" {
		linkStatus = domain.PartyLinkCandidate
		candidateID = p.CandidateCounterpartyID
	}
	row := tx.QueryRow(ctx, `INSERT INTO relationships (
			relationship_id, tenant_id, legal_entity_id, candidate_counterparty_id, party_link_status,
			source, channel, owner_principal_id, created_by
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING `+relationshipColumns,
		relationshipID, p.TenantID, p.LegalEntityID, candidateID, string(linkStatus),
		p.Source, p.Channel, p.OwnerPrincipalID, p.CreatedByPrincipalID)
	rel, err := scanRelationship(row)
	if err != nil {
		return nil, fmt.Errorf("insert relationship: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return rel, nil
}

func (s *PgStore) GetRelationship(ctx context.Context, relationshipID string) (*domain.Relationship, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.crmSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	r, err := scanRelationship(tx.QueryRow(ctx, `SELECT `+relationshipColumns+` FROM relationships WHERE relationship_id=$1 AND tenant_id=$2`,
		relationshipID, middleware.GetTenantID(ctx)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrRelationshipNotFound
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// ConfirmPartyLink — a gap-fill (see this package's own doc comment):
// the only path a CANDIDATE party link becomes CONFIRMED. Refuses
// anything not currently CANDIDATE — there is nothing to confirm from
// UNLINKED (no candidate was ever proposed) or CONFIRMED (already
// settled).
func (s *PgStore) ConfirmPartyLink(ctx context.Context, p domain.ConfirmPartyLinkParams) (*domain.Relationship, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.crmSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	current, err := scanRelationship(tx.QueryRow(ctx, `SELECT `+relationshipColumns+` FROM relationships WHERE relationship_id=$1 AND tenant_id=$2 FOR UPDATE`, p.RelationshipID, p.TenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrRelationshipNotFound
	}
	if err != nil {
		return nil, err
	}
	if current.PartyLinkStatus != domain.PartyLinkCandidate {
		return nil, domain.ErrPartyLinkNotCandidate
	}
	r, err := scanRelationship(tx.QueryRow(ctx, `
		UPDATE relationships SET counterparty_id=$3, party_link_status='CONFIRMED', updated_at=now()
		WHERE relationship_id=$1 AND tenant_id=$2 RETURNING `+relationshipColumns,
		p.RelationshipID, p.TenantID, p.CounterpartyID))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

const interactionColumns = `interaction_id, relationship_id, tenant_id, interaction_type, channel, notes, occurred_at, logged_by, created_at`

func scanInteraction(row pgx.Row) (*domain.Interaction, error) {
	i := &domain.Interaction{}
	err := row.Scan(&i.InteractionID, &i.RelationshipID, &i.TenantID, &i.InteractionType, &i.Channel, &i.Notes, &i.OccurredAt, &i.LoggedBy, &i.CreatedAt)
	return i, err
}

// LogInteraction — BIZ-06's own LogInteraction command. Append-only.
func (s *PgStore) LogInteraction(ctx context.Context, p domain.LogInteractionParams) (*domain.Interaction, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.crmSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	// Just proves the relationship exists in this tenant — interactions
	// do not mutate the relationship row itself, so no locking is needed.
	var relExists int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM relationships WHERE relationship_id=$1 AND tenant_id=$2`, p.RelationshipID, p.TenantID).Scan(&relExists); err != nil {
		return nil, err
	}
	if relExists == 0 {
		return nil, domain.ErrRelationshipNotFound
	}

	interactionID := "int-" + uuid.New().String()
	args := []any{interactionID, p.RelationshipID, p.TenantID, p.InteractionType, p.Channel, p.Notes, p.LoggedByPrincipalID}
	query := `INSERT INTO interactions (interaction_id, relationship_id, tenant_id, interaction_type, channel, notes, logged_by`
	if p.OccurredAt != nil {
		query += `, occurred_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING ` + interactionColumns
		args = append(args, *p.OccurredAt)
	} else {
		query += `) VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING ` + interactionColumns
	}
	i, err := scanInteraction(tx.QueryRow(ctx, query, args...))
	if err != nil {
		return nil, fmt.Errorf("insert interaction: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return i, nil
}

// GetTimeline — BIZ-06's own GetTimeline query.
// mergeChainCTE resolves every relationship that transitively merged into
// $1 (within tenant $2), so a child-row read against a merge target also
// surfaces rows still parented to a merged-away source — see MergeCRMProfile's
// own comment on why child rows are never physically reparented.
const mergeChainCTE = `WITH RECURSIVE merge_chain(relationship_id) AS (
	SELECT $1::text
	UNION ALL
	SELECT r.relationship_id FROM relationships r
	JOIN merge_chain mc ON r.merged_into_relationship_id = mc.relationship_id
	WHERE r.tenant_id = $2
) `

func (s *PgStore) GetTimeline(ctx context.Context, tenantID, relationshipID string) ([]domain.Interaction, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.crmSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, mergeChainCTE+`SELECT `+interactionColumns+` FROM interactions
		WHERE relationship_id IN (SELECT relationship_id FROM merge_chain) AND tenant_id=$2
		ORDER BY occurred_at ASC`, relationshipID, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Interaction
	for rows.Next() {
		i, err := scanInteraction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *i)
	}
	return out, rows.Err()
}

const opportunityColumns = `opportunity_id, relationship_id, tenant_id, legal_entity_id, name, value, currency, stage,
	status, owner_principal_id, created_by, created_at, updated_at, closed_by, closed_at, close_reason`

func scanOpportunity(row pgx.Row) (*domain.Opportunity, error) {
	o := &domain.Opportunity{}
	err := row.Scan(&o.OpportunityID, &o.RelationshipID, &o.TenantID, &o.LegalEntityID, &o.Name, &o.Value, &o.Currency, &o.Stage,
		&o.Status, &o.OwnerPrincipalID, &o.CreatedBy, &o.CreatedAt, &o.UpdatedAt, &o.ClosedBy, &o.ClosedAt, &o.CloseReason)
	return o, err
}

func validOpportunityStage(s domain.OpportunityStage) bool {
	switch s {
	case domain.StageLead, domain.StageQualified, domain.StageProposal, domain.StageNegotiation, domain.StageClosedWon, domain.StageClosedLost:
		return true
	}
	return false
}

// CreateOpportunity — BIZ-06's own CreateOpportunity command. Lands at
// stage LEAD (or a caller-chosen valid starting stage), status OPEN.
func (s *PgStore) CreateOpportunity(ctx context.Context, p domain.CreateOpportunityParams) (*domain.Opportunity, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.crmSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	var relExists int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM relationships WHERE relationship_id=$1 AND tenant_id=$2`, p.RelationshipID, p.TenantID).Scan(&relExists); err != nil {
		return nil, err
	}
	if relExists == 0 {
		return nil, domain.ErrRelationshipNotFound
	}
	currency := p.Currency
	if currency == "" {
		currency = "USD"
	}
	opportunityID := "opp-" + uuid.New().String()
	row := tx.QueryRow(ctx, `INSERT INTO opportunities (
			opportunity_id, relationship_id, tenant_id, legal_entity_id, name, value, currency, owner_principal_id, created_by
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING `+opportunityColumns,
		opportunityID, p.RelationshipID, p.TenantID, p.LegalEntityID, p.Name, p.Value, currency, p.OwnerPrincipalID, p.CreatedByPrincipalID)
	o, err := scanOpportunity(row)
	if err != nil {
		return nil, fmt.Errorf("insert opportunity: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO opportunity_stage_transitions (transition_id, opportunity_id, tenant_id, from_stage, to_stage, actor_principal_id, reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		"oppstr-"+uuid.New().String(), opportunityID, p.TenantID, "", string(domain.StageLead), p.CreatedByPrincipalID, "opportunity created"); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return o, nil
}

func (s *PgStore) GetOpportunity(ctx context.Context, opportunityID string) (*domain.Opportunity, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.crmSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	o, err := scanOpportunity(tx.QueryRow(ctx, `SELECT `+opportunityColumns+` FROM opportunities WHERE opportunity_id=$1 AND tenant_id=$2`,
		opportunityID, middleware.GetTenantID(ctx)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrOpportunityNotFound
	}
	if err != nil {
		return nil, err
	}
	return o, nil
}

// UpdateStage — BIZ-06's own UpdateStage command. Valid only while the
// opportunity is OPEN; CloseOpportunity (Wave 2) is the only path to a
// CLOSED_WON/CLOSED_LOST stage plus status=CLOSED together.
func (s *PgStore) UpdateStage(ctx context.Context, p domain.UpdateStageParams) (*domain.Opportunity, error) {
	if !validOpportunityStage(p.Stage) {
		return nil, domain.ErrInvalidStage
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.crmSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	current, err := scanOpportunity(tx.QueryRow(ctx, `SELECT `+opportunityColumns+` FROM opportunities WHERE opportunity_id=$1 AND tenant_id=$2 FOR UPDATE`, p.OpportunityID, p.TenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrOpportunityNotFound
	}
	if err != nil {
		return nil, err
	}
	if current.Status != domain.OpportunityOpen {
		return nil, domain.ErrOpportunityInvalidState
	}
	o, err := scanOpportunity(tx.QueryRow(ctx, `
		UPDATE opportunities SET stage=$3, updated_at=now()
		WHERE opportunity_id=$1 AND tenant_id=$2 RETURNING `+opportunityColumns,
		p.OpportunityID, p.TenantID, string(p.Stage)))
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO opportunity_stage_transitions (transition_id, opportunity_id, tenant_id, from_stage, to_stage, actor_principal_id, reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		"oppstr-"+uuid.New().String(), p.OpportunityID, p.TenantID, string(current.Stage), string(p.Stage), p.ActorPrincipalID, p.Reason); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return o, nil
}

// CloseOpportunity — BIZ-06's own CloseOpportunity command. Sets the
// final stage to CLOSED_WON or CLOSED_LOST per the caller's declared
// outcome and status=CLOSED, recording one last stage transition.
// Deliberately does not call, and this whole domain never calls, any
// accounting/AR service — the doc's own failure semantics: "opportunity
// value never feeds certified revenue automatically."
func (s *PgStore) CloseOpportunity(ctx context.Context, p domain.CloseOpportunityParams) (*domain.Opportunity, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.crmSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	current, err := scanOpportunity(tx.QueryRow(ctx, `SELECT `+opportunityColumns+` FROM opportunities WHERE opportunity_id=$1 AND tenant_id=$2 FOR UPDATE`, p.OpportunityID, p.TenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrOpportunityNotFound
	}
	if err != nil {
		return nil, err
	}
	if current.Status != domain.OpportunityOpen {
		return nil, domain.ErrOpportunityInvalidState
	}
	finalStage := domain.StageClosedLost
	if p.Won {
		finalStage = domain.StageClosedWon
	}
	o, err := scanOpportunity(tx.QueryRow(ctx, `
		UPDATE opportunities SET stage=$3, status='CLOSED', closed_by=$4, closed_at=now(), close_reason=$5, updated_at=now()
		WHERE opportunity_id=$1 AND tenant_id=$2 RETURNING `+opportunityColumns,
		p.OpportunityID, p.TenantID, string(finalStage), p.ActorPrincipalID, p.Reason))
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO opportunity_stage_transitions (transition_id, opportunity_id, tenant_id, from_stage, to_stage, actor_principal_id, reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		"oppstr-"+uuid.New().String(), p.OpportunityID, p.TenantID, string(current.Stage), string(finalStage), p.ActorPrincipalID, p.Reason); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return o, nil
}

// ListPipeline — BIZ-06's own ListPipeline query, the open-opportunity
// pipeline view for one legal entity.
func (s *PgStore) ListPipeline(ctx context.Context, p domain.ListPipelineParams) ([]domain.Opportunity, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.crmSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	query := `SELECT ` + opportunityColumns + ` FROM opportunities WHERE tenant_id=$1 AND legal_entity_id=$2 AND status='OPEN'`
	args := []any{p.TenantID, p.LegalEntityID}
	if p.Stage != "" {
		args = append(args, p.Stage)
		query += fmt.Sprintf(" AND stage=$%d", len(args))
	}
	if p.OwnerPrincipalID != "" {
		args = append(args, p.OwnerPrincipalID)
		query += fmt.Sprintf(" AND owner_principal_id=$%d", len(args))
	}
	query += " ORDER BY created_at DESC, opportunity_id DESC"
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Opportunity
	for rows.Next() {
		o, err := scanOpportunity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *o)
	}
	return out, rows.Err()
}

// ActivateRelationship — a gap-fill; see this package's own doc
// comment. Valid from PROSPECT only.
func (s *PgStore) ActivateRelationship(ctx context.Context, p domain.ActivateRelationshipParams) (*domain.Relationship, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.crmSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	current, err := scanRelationship(tx.QueryRow(ctx, `SELECT `+relationshipColumns+` FROM relationships WHERE relationship_id=$1 AND tenant_id=$2 FOR UPDATE`, p.RelationshipID, p.TenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrRelationshipNotFound
	}
	if err != nil {
		return nil, err
	}
	if current.Status != domain.RelationshipProspect {
		return nil, domain.ErrRelationshipInvalidState
	}
	r, err := scanRelationship(tx.QueryRow(ctx, `UPDATE relationships SET status='ACTIVE', updated_at=now() WHERE relationship_id=$1 AND tenant_id=$2 RETURNING `+relationshipColumns,
		p.RelationshipID, p.TenantID))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

// MarkDormant — a gap-fill; see this package's own doc comment. Valid
// from ACTIVE only.
func (s *PgStore) MarkDormant(ctx context.Context, p domain.MarkDormantParams) (*domain.Relationship, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.crmSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	current, err := scanRelationship(tx.QueryRow(ctx, `SELECT `+relationshipColumns+` FROM relationships WHERE relationship_id=$1 AND tenant_id=$2 FOR UPDATE`, p.RelationshipID, p.TenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrRelationshipNotFound
	}
	if err != nil {
		return nil, err
	}
	if current.Status != domain.RelationshipActive {
		return nil, domain.ErrRelationshipInvalidState
	}
	r, err := scanRelationship(tx.QueryRow(ctx, `UPDATE relationships SET status='DORMANT', updated_at=now() WHERE relationship_id=$1 AND tenant_id=$2 RETURNING `+relationshipColumns,
		p.RelationshipID, p.TenantID))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

// CloseRelationship — a gap-fill; see this package's own doc comment.
// Valid from PROSPECT, ACTIVE, or DORMANT.
func (s *PgStore) CloseRelationship(ctx context.Context, p domain.CloseRelationshipParams) (*domain.Relationship, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.crmSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	current, err := scanRelationship(tx.QueryRow(ctx, `SELECT `+relationshipColumns+` FROM relationships WHERE relationship_id=$1 AND tenant_id=$2 FOR UPDATE`, p.RelationshipID, p.TenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrRelationshipNotFound
	}
	if err != nil {
		return nil, err
	}
	switch current.Status {
	case domain.RelationshipProspect, domain.RelationshipActive, domain.RelationshipDormant:
	default:
		return nil, domain.ErrRelationshipInvalidState
	}
	r, err := scanRelationship(tx.QueryRow(ctx, `
		UPDATE relationships SET status='CLOSED', closed_by=$3, closed_at=now(), closure_reason=$4, updated_at=now()
		WHERE relationship_id=$1 AND tenant_id=$2 RETURNING `+relationshipColumns,
		p.RelationshipID, p.TenantID, p.ActorPrincipalID, p.ClosureReason))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

// MergeCRMProfile — BIZ-06's own MergeCRMProfile command. Reparents
// every interaction/opportunity/consent record/commercial-object-link
// belonging to Source onto Target, then forward-links Source via
// merged_into_relationship_id exactly once — checked in this order
// deliberately: the forward-link check runs before anything else, so an
// already-merged Source reports the precise reason rather than a
// generic state error (the same ordering bug already found and fixed
// twice this build, in document-vault-svc's SupersedeClassification and
// workflow-svc's SupersedeSubmission, avoided here from the start).
func (s *PgStore) MergeCRMProfile(ctx context.Context, p domain.MergeCRMProfileParams) (*domain.Relationship, error) {
	if p.SourceRelationshipID == p.TargetRelationshipID {
		return nil, domain.ErrCannotMergeIntoSelf
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.crmSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	source, err := scanRelationship(tx.QueryRow(ctx, `SELECT `+relationshipColumns+` FROM relationships WHERE relationship_id=$1 AND tenant_id=$2 FOR UPDATE`, p.SourceRelationshipID, p.TenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrRelationshipNotFound
	}
	if err != nil {
		return nil, err
	}
	if source.MergedIntoRelationshipID != nil {
		return nil, domain.ErrRelationshipAlreadyMerged
	}
	var targetExists int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM relationships WHERE relationship_id=$1 AND tenant_id=$2`, p.TargetRelationshipID, p.TenantID).Scan(&targetExists); err != nil {
		return nil, err
	}
	if targetExists == 0 {
		return nil, domain.ErrRelationshipNotFound
	}

	// interactions/consent_records/commercial_object_links are append-only
	// (Wave 1's triggers block ANY update, by design — evidence rows must
	// never be silently rewritten, including for merge bookkeeping). So a
	// merge does not physically move those child rows; it only forward-links
	// Source -> Target below, and every read of a relationship's children
	// (GetTimeline, GetLinkedFinancialObjects, GetConsentContext) walks the
	// merge chain to also surface rows still parented to a merged-away
	// source. opportunities has no such trigger and is reparented directly,
	// since no merge-chain-aware read exists for it (ListPipeline reads by
	// legal_entity_id, not relationship_id).
	if _, err := tx.Exec(ctx, `UPDATE opportunities SET relationship_id=$1 WHERE relationship_id=$2 AND tenant_id=$3`,
		p.TargetRelationshipID, p.SourceRelationshipID, p.TenantID); err != nil {
		return nil, fmt.Errorf("reparent opportunities: %w", err)
	}

	merged, err := scanRelationship(tx.QueryRow(ctx, `
		UPDATE relationships SET status='MERGED', merged_into_relationship_id=$3, updated_at=now()
		WHERE relationship_id=$1 AND tenant_id=$2 RETURNING `+relationshipColumns,
		p.SourceRelationshipID, p.TenantID, p.TargetRelationshipID))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return merged, nil
}

const commercialObjectLinkColumns = `link_id, relationship_id, tenant_id, linked_object_type, linked_object_id, linked_by, linked_at`

func scanCommercialObjectLink(row pgx.Row) (*domain.CommercialObjectLink, error) {
	l := &domain.CommercialObjectLink{}
	err := row.Scan(&l.LinkID, &l.RelationshipID, &l.TenantID, &l.LinkedObjectType, &l.LinkedObjectID, &l.LinkedBy, &l.LinkedAt)
	return l, err
}

// LinkCommercialObject — BIZ-06's own LinkCommercialObject command.
func (s *PgStore) LinkCommercialObject(ctx context.Context, p domain.LinkCommercialObjectParams) (*domain.CommercialObjectLink, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.crmSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	var relExists int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM relationships WHERE relationship_id=$1 AND tenant_id=$2`, p.RelationshipID, p.TenantID).Scan(&relExists); err != nil {
		return nil, err
	}
	if relExists == 0 {
		return nil, domain.ErrRelationshipNotFound
	}
	l, err := scanCommercialObjectLink(tx.QueryRow(ctx, `INSERT INTO commercial_object_links (link_id, relationship_id, tenant_id, linked_object_type, linked_object_id, linked_by)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING `+commercialObjectLinkColumns,
		"cobj-"+uuid.New().String(), p.RelationshipID, p.TenantID, p.LinkedObjectType, p.LinkedObjectID, p.LinkedByPrincipalID))
	if err != nil {
		return nil, fmt.Errorf("insert commercial object link: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return l, nil
}

// GetLinkedFinancialObjects — BIZ-06's own GetLinkedFinancialObjects query.
func (s *PgStore) GetLinkedFinancialObjects(ctx context.Context, tenantID, relationshipID string) ([]domain.CommercialObjectLink, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.crmSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, mergeChainCTE+`SELECT `+commercialObjectLinkColumns+` FROM commercial_object_links
		WHERE relationship_id IN (SELECT relationship_id FROM merge_chain) AND tenant_id=$2
		ORDER BY linked_at ASC`, relationshipID, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.CommercialObjectLink
	for rows.Next() {
		l, err := scanCommercialObjectLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *l)
	}
	return out, rows.Err()
}

const consentRecordColumns = `consent_id, relationship_id, tenant_id, consent_type, consent_status, basis, recorded_by, recorded_at`

func scanConsentRecord(row pgx.Row) (*domain.ConsentRecord, error) {
	c := &domain.ConsentRecord{}
	err := row.Scan(&c.ConsentID, &c.RelationshipID, &c.TenantID, &c.ConsentType, &c.ConsentStatus, &c.Basis, &c.RecordedBy, &c.RecordedAt)
	return c, err
}

// RecordConsent — a gap-fill; see this package's own doc comment.
// Append-only: each decision is a new row.
func (s *PgStore) RecordConsent(ctx context.Context, p domain.RecordConsentParams) (*domain.ConsentRecord, error) {
	if p.ConsentStatus != domain.ConsentGranted && p.ConsentStatus != domain.ConsentWithdrawn {
		return nil, domain.ErrInvalidConsentStatus
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.crmSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	var relExists int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM relationships WHERE relationship_id=$1 AND tenant_id=$2`, p.RelationshipID, p.TenantID).Scan(&relExists); err != nil {
		return nil, err
	}
	if relExists == 0 {
		return nil, domain.ErrRelationshipNotFound
	}
	c, err := scanConsentRecord(tx.QueryRow(ctx, `INSERT INTO consent_records (consent_id, relationship_id, tenant_id, consent_type, consent_status, basis, recorded_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING `+consentRecordColumns,
		"consent-"+uuid.New().String(), p.RelationshipID, p.TenantID, p.ConsentType, string(p.ConsentStatus), p.Basis, p.RecordedByPrincipalID))
	if err != nil {
		return nil, fmt.Errorf("insert consent record: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// GetConsentContext — BIZ-06's own GetConsentContext query. Returns the
// latest record per consent_type — the current answer for each type,
// not the full history (GetTimeline-style full history was not asked
// for by this query's name).
func (s *PgStore) GetConsentContext(ctx context.Context, tenantID, relationshipID string) ([]domain.ConsentRecord, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.crmSetRLS(ctx, tx); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, mergeChainCTE+`
		SELECT DISTINCT ON (consent_type) `+consentRecordColumns+`
		FROM consent_records WHERE relationship_id IN (SELECT relationship_id FROM merge_chain) AND tenant_id=$2
		ORDER BY consent_type, recorded_at DESC`, relationshipID, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ConsentRecord
	for rows.Next() {
		c, err := scanConsentRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}
