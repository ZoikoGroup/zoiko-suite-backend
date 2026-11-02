package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/anomaly-detection-svc/internal/domain"
)

// AI-04 AnomalySignal commands (AcknowledgeSignal, EscalateForReview,
// CloseSignal), signal reads and ReviewDisposition reads.

const (
	resourceTypeSignal      = "anomaly_signal"
	resourceTypeDisposition = "review_disposition"
)

const signalColumns = `signal_id, tenant_id, run_id, domain_name, model_provider, model_version,
	feature_set_version, source_entity_ref, severity, anomaly_score_bp, features, content_hash,
	status, version, created_at, updated_at`

func scanSignal(row pgx.Row) (*domain.GovernedAnomalySignal, error) {
	var signal domain.GovernedAnomalySignal
	var severity, status string
	var featuresJSON []byte
	err := row.Scan(
		&signal.SignalID, &signal.TenantID, &signal.RunID, &signal.DomainName, &signal.ModelProvider, &signal.ModelVersion,
		&signal.FeatureSetVersion, &signal.SourceEntityRef, &severity, &signal.AnomalyScoreBP, &featuresJSON, &signal.ContentHash,
		&status, &signal.Version, &signal.CreatedAt, &signal.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	signal.Severity = domain.Severity(severity)
	signal.Status = domain.SignalStatus(status)
	signal.AssuranceState = domain.AssuranceStateAdvisory

	signal.Features = map[string]string{}
	if len(featuresJSON) > 0 && string(featuresJSON) != "null" {
		if err := json.Unmarshal(featuresJSON, &signal.Features); err != nil {
			return nil, fmt.Errorf("decode signal features: %w", err)
		}
	}
	return &signal, nil
}

func loadSignal(ctx context.Context, tx pgx.Tx, tenantID, signalID string, forUpdate bool) (*domain.GovernedAnomalySignal, error) {
	query := `SELECT ` + signalColumns + ` FROM anomaly_signals WHERE tenant_id = $1 AND signal_id = $2`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	signal, err := scanSignal(tx.QueryRow(ctx, query, tenantID, signalID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrSignalNotFound
	}
	return signal, err
}

func loadSignalsOfRun(ctx context.Context, tx pgx.Tx, tenantID, runID string) ([]domain.GovernedAnomalySignal, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+signalColumns+`
		FROM anomaly_signals
		WHERE tenant_id = $1 AND run_id = $2
		ORDER BY created_at, signal_id`,
		tenantID, runID)
	if err != nil {
		return nil, fmt.Errorf("list signals of run: %w", err)
	}
	defer rows.Close()

	signals := []domain.GovernedAnomalySignal{}
	for rows.Next() {
		signal, err := scanSignal(rows)
		if err != nil {
			return nil, fmt.Errorf("scan anomaly signal: %w", err)
		}
		signals = append(signals, *signal)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return signals, nil
}

// GetGovernedSignal returns one signal of the tenant (404 otherwise).
func (s *PgStore) GetGovernedSignal(ctx context.Context, tenantID, signalID string) (*domain.GovernedAnomalySignal, error) {
	var signal *domain.GovernedAnomalySignal
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		loaded, err := loadSignal(ctx, tx, tenantID, signalID, false)
		signal = loaded
		return err
	})
	return signal, err
}

// ListSignals returns one page of signals filtered by severity, source,
// model and status, newest first.
func (s *PgStore) ListSignals(ctx context.Context, tenantID string, filter domain.SignalFilter, page domain.PageRequest) ([]domain.GovernedAnomalySignal, string, error) {
	var signals []domain.GovernedAnomalySignal
	cursorTime, cursorID := cursorArgs(page)

	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+signalColumns+`
			FROM anomaly_signals
			WHERE tenant_id = $1
			  AND ($2 = '' OR severity = $2)
			  AND ($3 = '' OR source_entity_ref = $3)
			  AND ($4 = '' OR domain_name = $4)
			  AND ($5 = '' OR model_provider = $5)
			  AND ($6 = '' OR model_version = $6)
			  AND ($7 = '' OR status = $7)
			  AND ($8::timestamptz IS NULL OR (created_at, signal_id) < ($8::timestamptz, $9::text))
			ORDER BY created_at DESC, signal_id DESC
			LIMIT $10`,
			tenantID, string(filter.Severity), filter.SourceEntityRef, filter.DomainName,
			filter.ModelProvider, filter.ModelVersion, string(filter.Status),
			cursorTime, cursorID, pageLimitPlusOne(page))
		if err != nil {
			return fmt.Errorf("list anomaly signals: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			signal, err := scanSignal(rows)
			if err != nil {
				return fmt.Errorf("scan anomaly signal: %w", err)
			}
			signals = append(signals, *signal)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, "", err
	}

	pageRows, nextCursor := splitPage(signals, page, func(signal domain.GovernedAnomalySignal) string {
		return domain.EncodeCursor(signal.CreatedAt, signal.SignalID)
	})
	return pageRows, nextCursor, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Signal commands
// ─────────────────────────────────────────────────────────────────────────────

// signalTransition describes one signal command for transitionSignal.
type signalTransition struct {
	// targetStatus is where the command moves the signal.
	targetStatus domain.SignalStatus
	// notAllowed is returned when the current status cannot reach targetStatus.
	notAllowed *domain.Error
	// enforceSegregationOfDuties: the actor must differ from the run
	// submitter and from the model publisher (CloseSignal only).
	enforceSegregationOfDuties bool
	// disposition, when set, is written as the signal's ReviewDisposition
	// and announced with AI.AnomalyDispositioned.
	disposition *dispositionDraft
}

type dispositionDraft struct {
	reasonCode   string
	notes        string
	evidenceRefs []string
}

// AcknowledgeSignal moves Detected → ReviewPending: a reviewer has picked
// the signal up. No event (the spec names none for this step).
func (s *PgStore) AcknowledgeSignal(ctx context.Context, cmd domain.CommandContext, signalID string, req domain.AcknowledgeSignalRequest) (domain.CommandResult[domain.GovernedAnomalySignal], error) {
	return s.transitionSignal(ctx, cmd, signalID, signalTransition{
		targetStatus: domain.SignalReviewPending,
		notAllowed:   domain.ErrSignalNotDetected,
	})
}

// EscalateForReview moves ReviewPending → Escalated (terminal for this
// service; the escalation is handed to a human/governed process) and writes
// the disposition with reason_code ESCALATED_FOR_REVIEW.
func (s *PgStore) EscalateForReview(ctx context.Context, cmd domain.CommandContext, signalID string, req domain.EscalateForReviewRequest) (domain.CommandResult[domain.GovernedAnomalySignal], error) {
	return s.transitionSignal(ctx, cmd, signalID, signalTransition{
		targetStatus: domain.SignalEscalated,
		notAllowed:   domain.ErrSignalNotReviewable,
		disposition: &dispositionDraft{
			reasonCode:   domain.ReasonCodeEscalated,
			notes:        req.Reason,
			evidenceRefs: req.EvidenceRefs,
		},
	})
}

// CloseSignal moves ReviewPending → Confirmed | Dismissed and writes the
// disposition. It writes nothing outside this service: a Confirmed outcome,
// however severe, freezes no payment and writes off nothing.
func (s *PgStore) CloseSignal(ctx context.Context, cmd domain.CommandContext, signalID string, req domain.CloseSignalRequest) (domain.CommandResult[domain.GovernedAnomalySignal], error) {
	return s.transitionSignal(ctx, cmd, signalID, signalTransition{
		targetStatus:               req.Outcome,
		notAllowed:                 domain.ErrSignalNotReviewable,
		enforceSegregationOfDuties: true,
		disposition: &dispositionDraft{
			reasonCode:   req.ReasonCode,
			notes:        req.Notes,
			evidenceRefs: req.EvidenceRefs,
		},
	})
}

// transitionSignal is the shared five-step body of every signal command.
func (s *PgStore) transitionSignal(ctx context.Context, cmd domain.CommandContext, signalID string, transition signalTransition) (domain.CommandResult[domain.GovernedAnomalySignal], error) {
	var result domain.CommandResult[domain.GovernedAnomalySignal]

	err := s.inTenantTx(ctx, cmd.TenantID, func(tx pgx.Tx) error {
		// Step 1: lock the signal (404 if not this tenant's).
		current, err := loadSignal(ctx, tx, cmd.TenantID, signalID, true)
		if err != nil {
			return err
		}

		// Step 2: an exact replay returns the signal as it is now.
		claim, err := claimIdempotencyKey(ctx, tx, cmd, resourceTypeSignal, signalID)
		if err != nil {
			return err
		}
		if claim.IsReplay {
			result = domain.CommandResult[domain.GovernedAnomalySignal]{Resource: current, Replayed: true}
			return nil
		}

		// Step 3: If-Match must name the current version.
		if err := checkExpectedVersion(cmd.ExpectedVersion, current.Version); err != nil {
			return err
		}

		// Step 4a: state machine.
		if !domain.CanTransitionSignal(current.Status, transition.targetStatus) {
			return transition.notAllowed.WithDetail(fmt.Sprintf(
				"anomaly signal is %s and cannot move to %s", current.Status, transition.targetStatus))
		}

		// Step 4b: segregation of duties.
		var run *domain.DetectionRun
		run, err = loadRun(ctx, tx, cmd.TenantID, current.RunID)
		if err != nil {
			return err
		}
		if transition.enforceSegregationOfDuties {
			if cmd.PrincipalID == run.CreatedBy {
				return domain.ErrSegregationOfDuties.WithDetail(
					"segregation of duties: the principal who submitted the detection run cannot close its signals")
			}
			if cmd.PrincipalID == run.ModelPublishedBy {
				return domain.ErrSegregationOfDuties.WithDetail(
					"segregation of duties: the publisher of the anomaly model cannot close signals it produced")
			}
		}

		// Step 5: write status, disposition and event.
		updated, err := scanSignal(tx.QueryRow(ctx, `
			UPDATE anomaly_signals
			SET status = $3, version = version + 1, updated_at = NOW()
			WHERE tenant_id = $1 AND signal_id = $2
			RETURNING `+signalColumns,
			cmd.TenantID, signalID, string(transition.targetStatus)))
		if err != nil {
			return fmt.Errorf("update anomaly signal status: %w", err)
		}

		if transition.disposition != nil {
			disposition, err := insertDisposition(ctx, tx, cmd, updated, *transition.disposition)
			if err != nil {
				return err
			}
			err = enqueueEvent(ctx, tx, cmd, outboxEvent{
				EventType:     domain.EventAnomalyDispositioned,
				AggregateType: resourceTypeSignal,
				AggregateID:   updated.SignalID,
				Payload:       dispositionPayload(run, current, updated, disposition),
			})
			if err != nil {
				return err
			}
		}

		result = domain.CommandResult[domain.GovernedAnomalySignal]{Resource: updated}
		return nil
	})
	return result, err
}

// ─────────────────────────────────────────────────────────────────────────────
// Review dispositions
// ─────────────────────────────────────────────────────────────────────────────

const dispositionColumns = `disposition_id, tenant_id, signal_id, outcome, reason_code, notes, evidence_refs, actor, decided_at`

func scanDisposition(row pgx.Row) (*domain.ReviewDisposition, error) {
	var disposition domain.ReviewDisposition
	var outcome string
	var evidenceJSON []byte
	err := row.Scan(
		&disposition.DispositionID, &disposition.TenantID, &disposition.SignalID, &outcome,
		&disposition.ReasonCode, &disposition.Notes, &evidenceJSON, &disposition.Actor, &disposition.DecidedAt,
	)
	if err != nil {
		return nil, err
	}
	disposition.Outcome = domain.SignalStatus(outcome)
	disposition.EvidenceRefs = []string{}
	if len(evidenceJSON) > 0 {
		if err := json.Unmarshal(evidenceJSON, &disposition.EvidenceRefs); err != nil {
			return nil, fmt.Errorf("decode evidence refs: %w", err)
		}
	}
	return &disposition, nil
}

func insertDisposition(ctx context.Context, tx pgx.Tx, cmd domain.CommandContext, signal *domain.GovernedAnomalySignal, draft dispositionDraft) (*domain.ReviewDisposition, error) {
	evidenceRefs := draft.evidenceRefs
	if evidenceRefs == nil {
		evidenceRefs = []string{}
	}
	evidenceJSON, err := json.Marshal(evidenceRefs)
	if err != nil {
		return nil, fmt.Errorf("marshal evidence refs: %w", err)
	}

	disposition, err := scanDisposition(tx.QueryRow(ctx, `
		INSERT INTO review_dispositions
			(disposition_id, tenant_id, signal_id, outcome, reason_code, notes, evidence_refs, actor)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING `+dispositionColumns,
		newID(domain.PrefixDisposition), cmd.TenantID, signal.SignalID, string(signal.Status),
		draft.reasonCode, draft.notes, evidenceJSON, cmd.PrincipalID))
	if err != nil {
		return nil, fmt.Errorf("record review disposition: %w", err)
	}
	return disposition, nil
}

// dispositionPayload is the AI.AnomalyDispositioned event body. It states
// plainly that no protected action was taken.
func dispositionPayload(run *domain.DetectionRun, before, after *domain.GovernedAnomalySignal, disposition *domain.ReviewDisposition) map[string]any {
	return map[string]any{
		"surface":                "governed",
		"signal_id":              after.SignalID,
		"run_id":                 run.RunID,
		"model_id":               run.ModelID,
		"model_version":          run.ModelVersion,
		"feature_set_version":    run.FeatureSetVersion,
		"disposition_id":         disposition.DispositionID,
		"previous_status":        before.Status,
		"outcome":                disposition.Outcome,
		"reason_code":            disposition.ReasonCode,
		"evidence_refs":          disposition.EvidenceRefs,
		"decided_by":             disposition.Actor,
		"advisory":               true,
		"protected_action_taken": false,
	}
}

// GetDisposition returns the disposition of a signal: 404
// anomaly_signal_not_found when the signal is unknown, 404
// disposition_not_found when it has not been dispositioned yet.
func (s *PgStore) GetDisposition(ctx context.Context, tenantID, signalID string) (*domain.ReviewDisposition, error) {
	var disposition *domain.ReviewDisposition
	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := loadSignal(ctx, tx, tenantID, signalID, false); err != nil {
			return err
		}
		loaded, err := scanDisposition(tx.QueryRow(ctx,
			`SELECT `+dispositionColumns+` FROM review_dispositions WHERE tenant_id = $1 AND signal_id = $2`,
			tenantID, signalID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrDispositionNotFound
		}
		disposition = loaded
		return err
	})
	return disposition, err
}

// ListDispositions returns one page of dispositions, newest first.
func (s *PgStore) ListDispositions(ctx context.Context, tenantID string, filter domain.DispositionFilter, page domain.PageRequest) ([]domain.ReviewDisposition, string, error) {
	var dispositions []domain.ReviewDisposition
	cursorTime, cursorID := cursorArgs(page)

	err := s.inTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+dispositionColumns+`
			FROM review_dispositions
			WHERE tenant_id = $1
			  AND ($2 = '' OR outcome = $2)
			  AND ($3::timestamptz IS NULL OR (decided_at, disposition_id) < ($3::timestamptz, $4::text))
			ORDER BY decided_at DESC, disposition_id DESC
			LIMIT $5`,
			tenantID, string(filter.Outcome), cursorTime, cursorID, pageLimitPlusOne(page))
		if err != nil {
			return fmt.Errorf("list review dispositions: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			disposition, err := scanDisposition(rows)
			if err != nil {
				return fmt.Errorf("scan review disposition: %w", err)
			}
			dispositions = append(dispositions, *disposition)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, "", err
	}

	pageRows, nextCursor := splitPage(dispositions, page, func(disposition domain.ReviewDisposition) string {
		return domain.EncodeCursor(disposition.DecidedAt, disposition.DispositionID)
	})
	return pageRows, nextCursor, nil
}
