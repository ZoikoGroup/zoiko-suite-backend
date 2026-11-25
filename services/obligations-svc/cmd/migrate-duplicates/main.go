// Command migrate-duplicates backfills obligation-tracking-svc's and
// filing-tracker-svc's existing rows into obligations-svc, as the data half
// of the WFC-05 consolidation (Doc 04 §2.1 single-owner doctrine, tracker
// row 78). The schema/API half landed first (migration 000013): this tool
// does not touch that, it only populates it from the two services it is
// meant to let the platform eventually retire.
//
// It is read-only against the two source databases and additive-only
// against the target (every write goes through store.PgStore's
// BackfillObligation / BackfillFilingRequirement, both idempotent on a
// deterministic id, so re-running this tool is always safe). It NEVER
// deletes or modifies a source row, and it never writes anything unless
// -apply is passed — the default is a dry run that reports exactly what it
// would do.
//
// A row this tool cannot map safely (most commonly: the legal entity's
// jurisdiction cannot be resolved from tenant-entity-registry-svc) is
// skipped and reported, never guessed.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/obligations-svc/internal/domain"
	"zoiko.io/obligations-svc/internal/middleware"
	"zoiko.io/obligations-svc/internal/registry"
	"zoiko.io/obligations-svc/internal/store"
)

// migrateNamespace is a fixed, arbitrary UUID used only to derive
// deterministic target ids from a source row's own id (uuid.NewSHA1). It
// must never change: changing it would make every future run generate new
// ids for rows already migrated, breaking the idempotent-rerun guarantee
// this tool depends on.
var migrateNamespace = uuid.MustParse("5f3e9b10-6b1a-4a2e-9b0a-6d2c1f7a9e01")

func targetID(parts ...string) string {
	return uuid.NewSHA1(migrateNamespace, []byte(strings.Join(parts, ":"))).String()
}

type flags struct {
	trackingDSN string
	filingDSN   string
	targetDSN   string
	registryURL string
	tenantID    string
	apply       bool
}

func parseFlags() flags {
	var f flags
	flag.StringVar(&f.trackingDSN, "tracking-dsn", "", "obligation-tracking-svc Postgres DSN (empty = skip)")
	flag.StringVar(&f.filingDSN, "filing-dsn", "", "filing-tracker-svc Postgres DSN (empty = skip)")
	flag.StringVar(&f.targetDSN, "target-dsn", "", "obligations-svc Postgres DSN (required)")
	flag.StringVar(&f.registryURL, "registry-url", "", "tenant-entity-registry-svc base URL (required if -tracking-dsn is set)")
	flag.StringVar(&f.tenantID, "tenant-id", "", "limit to one tenant_id (empty = every tenant found in the source)")
	flag.BoolVar(&f.apply, "apply", false, "actually write (default: dry run, report only)")
	flag.Parse()
	return f
}

type report struct {
	migratedObligations int
	migratedFilings     int
	skipped             []string
}

func (r *report) skip(what string, err error) {
	r.skipped = append(r.skipped, fmt.Sprintf("%s: %v", what, err))
}

func main() {
	f := parseFlags()
	if f.targetDSN == "" {
		fatal("-target-dsn is required")
	}
	if f.trackingDSN != "" && f.registryURL == "" {
		fatal("-registry-url is required when -tracking-dsn is set (needed to resolve jurisdiction_id)")
	}

	log, _ := zap.NewProduction()
	defer func() { _ = log.Sync() }()
	ctx := context.Background()

	targetPool, err := pgxpool.New(ctx, f.targetDSN)
	if err != nil {
		fatal("connect target: %v", err)
	}
	defer targetPool.Close()
	s := store.New(targetPool, log)

	rep := &report{}

	if f.trackingDSN != "" {
		pool, err := pgxpool.New(ctx, f.trackingDSN)
		if err != nil {
			fatal("connect tracking source: %v", err)
		}
		defer pool.Close()
		reg := registry.New(f.registryURL, log)
		migrateTracking(ctx, pool, reg, s, f, rep)
	}

	if f.filingDSN != "" {
		pool, err := pgxpool.New(ctx, f.filingDSN)
		if err != nil {
			fatal("connect filing source: %v", err)
		}
		defer pool.Close()
		migrateFiling(ctx, pool, s, f, rep)
	}

	mode := "DRY RUN — nothing written"
	if f.apply {
		mode = "APPLIED"
	}
	fmt.Printf("\n=== migrate-duplicates: %s ===\n", mode)
	fmt.Printf("obligations migrated: %d\n", rep.migratedObligations)
	fmt.Printf("filing requirements migrated: %d\n", rep.migratedFilings)
	fmt.Printf("skipped (needs manual review): %d\n", len(rep.skipped))
	for _, s := range rep.skipped {
		fmt.Println("  - " + s)
	}
}

func fatal(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, "fatal: "+format+"\n", args...)
	os.Exit(1)
}

// ── obligation-tracking-svc ──────────────────────────────────────────────────

type trackingRow struct {
	obligationID, tenantID, legalEntityID string
	sourceType, sourceID                  string
	title                                 string
	description                           *string
	obligationType, riskLevel, status     string
	dueDate                               time.Time
	assignedTo                            *string
	fulfilledAt                           *time.Time
	fulfilledBy, fulfillmentNote          *string
	effectiveFrom                         time.Time
	effectiveTo                           *time.Time
	createdBy                             string
	createdAt, updatedAt                  time.Time
	extractedByAI                         bool
	validatedAt                           *time.Time
	validatedBy                           *string
	sourceClauseID, triggerDescription    *string
	calculationMethod                     *string
	scheduledAt                           *time.Time
	scheduledBy                           *string
	dueMarkedAt                           *time.Time
	dueMarkedBy                           *string
	inProgressAt                          *time.Time
	inProgressBy                          *string
	satisfiedAt                           *time.Time
	satisfiedBy                           *string
	waivedAt                              *time.Time
	waivedBy                              *string
	waiverAuthorityReference              *string
	breachedAt                            *time.Time
	breachedBy                            *string
	breachNote                            *string
	disputedAt                            *time.Time
	disputedBy                            *string
	disputeReason                         *string
	supersededBy                          *string
}

const trackingSelect = `
	SELECT obligation_id, tenant_id, legal_entity_id, source_type, source_id, title, description,
	       obligation_type, risk_level, status, due_date, assigned_to,
	       fulfilled_at, fulfilled_by, fulfillment_note, effective_from, effective_to, created_by,
	       created_at, updated_at,
	       extracted_by_ai, validated_at, validated_by,
	       source_clause_id, trigger_description, calculation_method,
	       scheduled_at, scheduled_by, due_marked_at, due_marked_by, in_progress_at, in_progress_by,
	       satisfied_at, satisfied_by, waived_at, waived_by, waiver_authority_reference,
	       breached_at, breached_by, breach_note, disputed_at, disputed_by, dispute_reason, superseded_by
	FROM obligations`

// trackingStatus maps obligation-tracking-svc's status vocabulary onto
// obligations-svc's EXTENDED statuses. Every value is either identical by
// name or SATISFIED -> FULFILLED; anything else is refused rather than
// guessed, so a future status this tool was never told about cannot migrate
// silently as a wrong one.
var trackingStatus = map[string]string{
	"CANDIDATE":   domain.ExtendedStatusCandidate,
	"PLANNED":     domain.ExtendedStatusPlanned,
	"ACTIVE":      domain.ExtendedStatusActive,
	"DUE":         domain.ExtendedStatusDue,
	"IN_PROGRESS": domain.ExtendedStatusInProgress,
	"SATISFIED":   domain.ExtendedStatusFulfilled,
	"WAIVED":      domain.ExtendedStatusWaived,
	"BREACHED":    domain.ExtendedStatusBreached,
	"DISPUTED":    domain.ExtendedStatusDisputed,
	"SUPERSEDED":  domain.ExtendedStatusSuperseded,
}

func migrateTracking(ctx context.Context, pool *pgxpool.Pool, reg *registry.Client, s *store.PgStore, f flags, rep *report) {
	q, args := trackingSelect, []any{}
	if f.tenantID != "" {
		q += " WHERE tenant_id = $1"
		args = append(args, f.tenantID)
	}
	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		fatal("query obligation-tracking-svc: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var r trackingRow
		if err := rows.Scan(&r.obligationID, &r.tenantID, &r.legalEntityID, &r.sourceType, &r.sourceID, &r.title, &r.description,
			&r.obligationType, &r.riskLevel, &r.status, &r.dueDate, &r.assignedTo,
			&r.fulfilledAt, &r.fulfilledBy, &r.fulfillmentNote, &r.effectiveFrom, &r.effectiveTo, &r.createdBy,
			&r.createdAt, &r.updatedAt,
			&r.extractedByAI, &r.validatedAt, &r.validatedBy,
			&r.sourceClauseID, &r.triggerDescription, &r.calculationMethod,
			&r.scheduledAt, &r.scheduledBy, &r.dueMarkedAt, &r.dueMarkedBy, &r.inProgressAt, &r.inProgressBy,
			&r.satisfiedAt, &r.satisfiedBy, &r.waivedAt, &r.waivedBy, &r.waiverAuthorityReference,
			&r.breachedAt, &r.breachedBy, &r.breachNote, &r.disputedAt, &r.disputedBy, &r.disputeReason, &r.supersededBy,
		); err != nil {
			fatal("scan obligation-tracking-svc row: %v", err)
		}

		what := fmt.Sprintf("obligation-tracking-svc obligation %s (tenant %s)", r.obligationID, r.tenantID)

		targetStatus, known := trackingStatus[r.status]
		if !known {
			rep.skip(what, fmt.Errorf("unmapped status %q", r.status))
			continue
		}

		jurisdictionID, err := reg.PrimaryJurisdiction(ctx, r.tenantID, r.legalEntityID)
		if err != nil {
			rep.skip(what, fmt.Errorf("jurisdiction lookup: %w", err))
			continue
		}

		responsibleFunction := "UNASSIGNED (migrated from obligation-tracking-svc, no assignee on source row)"
		if r.assignedTo != nil && strings.TrimSpace(*r.assignedTo) != "" {
			responsibleFunction = *r.assignedTo
		}
		severity := strings.ToUpper(r.riskLevel)
		if severity == "" {
			severity = "MEDIUM"
		}

		params := domain.BackfillObligationParams{
			Obligation: domain.Obligation{
				ObligationID:         targetID("obligation-tracking-svc", r.obligationID),
				LegalEntityID:        r.legalEntityID,
				JurisdictionID:       jurisdictionID,
				ObligationSourceType: r.sourceType,
				ObligationSourceID:   r.sourceID,
				ObligationCode:       truncate("OTS-"+r.obligationID, 128),
				ObligationType:       r.obligationType,
				ObligationStatus:     targetStatus,
				DueDate:              r.dueDate,
				SeverityLevel:        severity,
				ResponsibleFunction:  responsibleFunction,
				SourceReference:      fmt.Sprintf("obligation-tracking-svc:%s:%s", r.sourceType, r.sourceID),
				CreatedAt:            r.createdAt,
				CreatedByPrincipalID: r.createdBy,
				UpdatedAt:            r.updatedAt,
				LifecycleModel:       domain.LifecycleModelExtended,
				ClockClass:           domain.ClockClassContractual,
				ExtractedByAI:        r.extractedByAI,
				ValidatedAt:          r.validatedAt,
				ValidatedBy:          r.validatedBy,
				SourceClauseID:       r.sourceClauseID,
				TriggerDescription:   r.triggerDescription,
				CalculationMethod:    r.calculationMethod,
				ScheduledAt:          r.scheduledAt,
				ScheduledBy:          r.scheduledBy,
				DueMarkedAt:          r.dueMarkedAt,
				DueMarkedBy:          r.dueMarkedBy,
				InProgressAt:         r.inProgressAt,
				InProgressBy:         r.inProgressBy,
				FulfilledAt:          r.satisfiedAt,
				FulfilledBy:          r.satisfiedBy,
				// fulfillment_note is the closest the source has to
				// fulfillment evidence — preserved rather than dropped.
				FulfillmentEvidenceRef:   r.fulfillmentNote,
				WaivedAt:                 r.waivedAt,
				WaivedBy:                 r.waivedBy,
				WaiverAuthorityReference: r.waiverAuthorityReference,
				BreachedAt:               r.breachedAt,
				BreachedBy:               r.breachedBy,
				BreachNote:               r.breachNote,
				DisputedAt:               r.disputedAt,
				DisputedBy:               r.disputedBy,
				DisputeReason:            r.disputeReason,
				// SupersededByObligationID is deliberately left nil: the
				// successor row may not be migrated yet in this same run,
				// and the target column is a real FK — getting this wrong
				// would either fail the insert or point at a row that does
				// not exist yet. Resolving the supersession graph is left
				// as an explicit follow-up, never guessed here.
				EffectiveFrom: &r.effectiveFrom,
				EffectiveTo:   r.effectiveTo,
			},
			CorrelationID: "migrate-duplicates",
		}

		if !f.apply {
			fmt.Printf("[dry-run] would migrate %s -> obligation_id=%s status=%s\n", what, params.ObligationID, targetStatus)
			rep.migratedObligations++
			continue
		}
		_, created, err := s.BackfillObligation(middleware.WithTenant(ctx, r.tenantID), params)
		if err != nil {
			rep.skip(what, err)
			continue
		}
		if created {
			rep.migratedObligations++
		}
	}
	if err := rows.Err(); err != nil {
		fatal("reading obligation-tracking-svc rows: %v", err)
	}
}

// ── filing-tracker-svc ───────────────────────────────────────────────────────

type filingRow struct {
	filingID, tenantID, legalEntityID, jurisdictionID string
	filingAuthority, filingType, periodKey            string
	dueDate                                           time.Time
	status                                            string
	submissionReference                               *string
	submittedAt                                       *time.Time
	submittedBy                                       *string
	confirmationReference                             *string
	confirmedAt                                       *time.Time
	rejectionReason, notes                            string
	createdBy                                         string
	createdAt, updatedAt                              time.Time
}

const filingSelect = `
	SELECT filing_id, tenant_id, legal_entity_id, jurisdiction_id, filing_authority, filing_type, period_key,
	       due_date, status, submission_reference, submitted_at, submitted_by,
	       confirmation_reference, confirmed_at, rejection_reason, notes, created_by, created_at, updated_at
	FROM filing_requirements`

func migrateFiling(ctx context.Context, pool *pgxpool.Pool, s *store.PgStore, f flags, rep *report) {
	q, args := filingSelect, []any{}
	if f.tenantID != "" {
		q += " WHERE tenant_id = $1"
		args = append(args, f.tenantID)
	}
	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		fatal("query filing-tracker-svc: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var r filingRow
		if err := rows.Scan(&r.filingID, &r.tenantID, &r.legalEntityID, &r.jurisdictionID, &r.filingAuthority, &r.filingType, &r.periodKey,
			&r.dueDate, &r.status, &r.submissionReference, &r.submittedAt, &r.submittedBy,
			&r.confirmationReference, &r.confirmedAt, &r.rejectionReason, &r.notes, &r.createdBy, &r.createdAt, &r.updatedAt,
		); err != nil {
			fatal("scan filing-tracker-svc row: %v", err)
		}

		what := fmt.Sprintf("filing-tracker-svc filing %s (tenant %s)", r.filingID, r.tenantID)

		if _, known := domain.FilingStatuses[r.status]; !known {
			rep.skip(what, fmt.Errorf("unmapped filing_status %q", r.status))
			continue
		}

		parentID := targetID("filing-tracker-svc", "parent", r.filingID)
		parent := domain.BackfillObligationParams{
			Obligation: domain.Obligation{
				ObligationID:         parentID,
				LegalEntityID:        r.legalEntityID,
				JurisdictionID:       r.jurisdictionID,
				ObligationSourceType: "FILING_TRACKER_SVC",
				ObligationSourceID:   r.filingID,
				ObligationCode:       truncate("FTS-"+r.filingID, 128),
				ObligationType:       "FILING",
				ObligationStatus:     domain.ExtendedStatusActive,
				DueDate:              r.dueDate,
				SeverityLevel:        "MEDIUM",
				// filing-tracker-svc tracks no internal owning function —
				// its whole domain is statutory filings, so "Tax" is a
				// documented default, not a guess at an individual case.
				ResponsibleFunction:  "Tax",
				SourceReference:      fmt.Sprintf("filing-tracker-svc:%s:%s:%s", r.filingAuthority, r.filingType, r.periodKey),
				CreatedAt:            r.createdAt,
				CreatedByPrincipalID: r.createdBy,
				UpdatedAt:            r.updatedAt,
				LifecycleModel:       domain.LifecycleModelExtended,
				ClockClass:           domain.ClockClassRegulatory,
				TriggerDescription:   strPtr("filing-tracker-svc backfill: " + r.filingAuthority + " " + r.filingType),
				CalculationMethod:    strPtr("migrated verbatim from filing-tracker-svc (no calculation method recorded on the source)"),
				ScheduledAt:          &r.createdAt,
				ScheduledBy:          &r.createdBy,
			},
			CorrelationID: "migrate-duplicates",
		}

		filingParams := domain.BackfillFilingRequirementParams{
			FilingRequirement: domain.FilingRequirement{
				FilingRequirementID: targetID("filing-tracker-svc", "filing", r.filingID),
				ObligationID:        parentID,
				FilingType:          r.filingType,
				FilingAuthority:     r.filingAuthority,
				// filing-tracker-svc has no submission_channel concept —
				// recorded honestly as unknown rather than guessed.
				SubmissionChannel:     "UNKNOWN",
				FilingStatus:          r.status,
				JurisdictionID:        &r.jurisdictionID,
				PeriodKey:             &r.periodKey,
				DueDate:               &r.dueDate,
				SubmissionReference:   r.submissionReference,
				SubmittedAt:           r.submittedAt,
				SubmittedBy:           r.submittedBy,
				ConfirmationReference: r.confirmationReference,
				ConfirmedAt:           r.confirmedAt,
				RejectionReason:       strPtrOrNil(r.rejectionReason),
				Notes:                 strPtrOrNil(r.notes),
			},
			CorrelationID: "migrate-duplicates",
		}

		if !f.apply {
			fmt.Printf("[dry-run] would migrate %s -> obligation_id=%s filing_requirement_id=%s status=%s\n",
				what, parentID, filingParams.FilingRequirementID, r.status)
			rep.migratedObligations++
			rep.migratedFilings++
			continue
		}

		tctx := middleware.WithTenant(ctx, r.tenantID)
		if _, created, err := s.BackfillObligation(tctx, parent); err != nil {
			rep.skip(what+" (parent obligation)", err)
			continue
		} else if created {
			rep.migratedObligations++
		}
		if _, created, err := s.BackfillFilingRequirement(tctx, filingParams); err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				rep.skip(what, err)
			}
			continue
		} else if created {
			rep.migratedFilings++
		}
	}
	if err := rows.Err(); err != nil {
		fatal("reading filing-tracker-svc rows: %v", err)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func strPtr(s string) *string { return &s }

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
