import re

# Read the file
with open('services/obligations-svc/internal/store/pg_store.go', 'r', encoding='utf-8') as f:
    content = f.read()

# 1. Add imports
old_imports = '''import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/obligations-svc/internal/domain"
	"zoiko.io/obligations-svc/internal/middleware"
)'''

new_imports = '''import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/obligations-svc/internal/domain"
	"zoiko.io/obligations-svc/internal/middleware"
)'''

content = content.replace(old_imports, new_imports)

# 2. Update ListFilingRequirements in Store interface
old_list_filing = '''// ListFilingRequirements returns an obligation's filing requirements,
	// newest first, at most limit. Fails with domain.ErrObligationNotFound if
	// obligation_id does not exist.
	ListFilingRequirements(ctx context.Context, obligationID string, limit int) ([]*domain.FilingRequirement, error)'''

new_list_filing = '''// ListFilingRequirements returns an obligation's filing requirements,
	// newest first, matching the filter. Fails with domain.ErrObligationNotFound if
	// obligation_id does not exist.
	ListFilingRequirements(ctx context.Context, filter domain.ListFilingRequirementsFilter) ([]*domain.FilingRequirement, error)'''

content = content.replace(old_list_filing, new_list_filing)

# 3. Add Clock methods to Store interface
old_escalation = '''	// ── Escalation Records ────────────────────────────────────────────────────
	CreateEscalationRecord(ctx context.Context, params domain.CreateEscalationRecordParams) (*domain.EscalationRecord, error)
	FindEscalationRecordByID(ctx context.Context, escalationRecordID string) (*domain.EscalationRecord, error)
	ListEscalationRecords(ctx context.Context, exceptionCaseID string, limit int) ([]*domain.EscalationRecord, error)
}'''

new_escalation = '''	// ── Escalation Records ────────────────────────────────────────────────────
	CreateEscalationRecord(ctx context.Context, params domain.CreateEscalationRecordParams) (*domain.EscalationRecord, error)
	FindEscalationRecordByID(ctx context.Context, escalationRecordID string) (*domain.EscalationRecord, error)
	ListEscalationRecords(ctx context.Context, exceptionCaseID string, limit int) ([]*domain.EscalationRecord, error)

	// ── Clock Methods ──────────────────────────────────────────────────────────
	ListClocks(ctx context.Context, filter domain.ListClocksFilter) ([]*domain.Clock, error)
	TransitionClock(ctx context.Context, params domain.ClockTransitionParams) (*domain.Clock, bool, error)
}'''

content = content.replace(old_escalation, new_escalation)

# 4. Update ListFilingRequirements implementation
old_impl = '''// ListFilingRequirements returns all filing requirements for an obligation.
func (s *PgStore) ListFilingRequirements(ctx context.Context, obligationID string, limit int) ([]*domain.FilingRequirement, error) {
	if _, err := s.FindObligationByID(ctx, obligationID); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = defaultListLimit
	}

	const query = `
		SELECT ` + filingRequirementColumns + `
		FROM filing_requirements
		WHERE obligation_id = $1 AND tenant_id = $2
		ORDER BY created_at DESC, filing_requirement_id DESC
		LIMIT $3;`

	var results []*domain.FilingRequirement
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		rows, qErr := tx.Query(ctx, query, obligationID, tenantID, limit)
		if qErr != nil {
			s.log.Error("pg ListFilingRequirements failed", zap.String("obligation_id", obligationID), zap.Error(qErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, qErr)
		}
		defer rows.Close()

		for rows.Next() {
			f, scanErr := scanFilingRequirement(rows)
			if scanErr != nil {
				s.log.Error("pg ListFilingRequirements scan failed", zap.Error(scanErr))
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
			}
			results = append(results, f)
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			s.log.Error("pg ListFilingRequirements rows error", zap.Error(rowsErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, rowsErr)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}'''

new_impl = '''// ListFilingRequirements returns all filing requirements for an obligation.
func (s *PgStore) ListFilingRequirements(ctx context.Context, filter domain.ListFilingRequirementsFilter) ([]*domain.FilingRequirement, error) {
	obligationID := filter.ObligationID
	if _, err := s.FindObligationByID(ctx, obligationID); err != nil {
		return nil, err
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	offset := filter.Offset

	var queryBuilder strings.Builder
	queryBuilder.WriteString(`
		SELECT ` + filingRequirementColumns + `
		FROM filing_requirements
		WHERE obligation_id = $1 AND tenant_id = $2`)
	
	args := []any{obligationID}
	argIdx := 3
	
	if filter.AfterCreatedAt != nil {
		queryBuilder.WriteString(fmt.Sprintf(" AND created_at < $%d", argIdx))
		args = append(args, *filter.AfterCreatedAt)
		argIdx++
	}
	if filter.AfterID != "" {
		queryBuilder.WriteString(fmt.Sprintf(" AND filing_requirement_id < $%d", argIdx))
		args = append(args, filter.AfterID)
		argIdx++
	}
	
	queryBuilder.WriteString(fmt.Sprintf(" ORDER BY created_at DESC, filing_requirement_id DESC LIMIT $%d OFFSET $%d;", argIdx, argIdx+1))
	args = append(args, limit, offset)

	var results []*domain.FilingRequirement
	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		argsWithTenant := append([]any{tenantID}, args[1:]...)
		rows, qErr := tx.Query(ctx, queryBuilder.String(), argsWithTenant...)
		if qErr != nil {
			s.log.Error("pg ListFilingRequirements failed", zap.String("obligation_id", obligationID), zap.Error(qErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, qErr)
		}
		defer rows.Close()

		for rows.Next() {
			f, scanErr := scanFilingRequirement(rows)
			if scanErr != nil {
				s.log.Error("pg ListFilingRequirements scan failed", zap.Error(scanErr))
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
			}
			results = append(results, f)
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			s.log.Error("pg ListFilingRequirements rows error", zap.Error(rowsErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, rowsErr)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}'''

content = content.replace(old_impl, new_impl)

# 5. Add ListClocks and TransitionClock implementations at end of file
old_end = '''if err != nil {
		return nil, err
	}
	return results, nil
}'''

new_end = '''if err != nil {
		return nil, err
	}
	return results, nil
}

// ListClocks returns clocks matching the given filter.
func (s *PgStore) ListClocks(ctx context.Context, filter domain.ListClocksFilter) ([]*domain.Clock, error) {
	tenantID, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	offset := filter.Offset

	var queryBuilder strings.Builder
	queryBuilder.WriteString(`
		SELECT ` + clockColumns + `
		FROM clocks
		WHERE tenant_id = $1`)
	
	args := []any{tenantID}
	argIdx := 2
	
	if filter.ObligationID != "" {
		queryBuilder.WriteString(fmt.Sprintf(" AND obligation_id = $%d", argIdx))
		args = append(args, filter.ObligationID)
		argIdx++
	}
	if filter.LegalEntityID != "" {
		queryBuilder.WriteString(fmt.Sprintf(" AND legal_entity_id = $%d", argIdx))
		args = append(args, filter.LegalEntityID)
		argIdx++
	}
	if filter.ClockClass != "" {
		queryBuilder.WriteString(fmt.Sprintf(" AND clock_class = $%d", argIdx))
		args = append(args, filter.ClockClass)
		argIdx++
	}
	if filter.ClockState != "" {
		queryBuilder.WriteString(fmt.Sprintf(" AND clock_state = $%d", argIdx))
		args = append(args, filter.ClockState)
		argIdx++
	}
	if filter.AfterCreatedAt != nil {
		queryBuilder.WriteString(fmt.Sprintf(" AND created_at < $%d", argIdx))
		args = append(args, *filter.AfterCreatedAt)
		argIdx++
	}
	if filter.AfterID != "" {
		queryBuilder.WriteString(fmt.Sprintf(" AND clock_id < $%d", argIdx))
		args = append(args, filter.AfterID)
		argIdx++
	}
	
	queryBuilder.WriteString(fmt.Sprintf(" ORDER BY created_at DESC, clock_id DESC LIMIT $%d OFFSET $%d;", argIdx, argIdx+1))
	args = append(args, limit, offset)

	var results []*domain.Clock
	err = s.withTenantTx(ctx, func(tx pgx.Tx, _ string) error {
		argsWithTenant := append([]any{}, args...)
		rows, qErr := tx.Query(ctx, queryBuilder.String(), argsWithTenant...)
		if qErr != nil {
			s.log.Error("pg ListClocks failed", zap.Error(qErr))
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, qErr)
		}
		defer rows.Close()

		for rows.Next() {
			c, scanErr := scanClock(rows)
			if scanErr != nil {
				s.log.Error("pg ListClocks scan failed", zap.Error(scanErr))
				return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
			}
			results = append(results, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// TransitionClock transitions a clock's state with optimistic locking.
// Returns the updated clock, whether a transition occurred, and any error.
func (s *PgStore) TransitionClock(ctx context.Context, params domain.ClockTransitionParams) (*domain.Clock, bool, error) {
	var updated *domain.Clock
	var transitioned bool

	err := s.withTenantTx(ctx, func(tx pgx.Tx, tenantID string) error {
		// Lock the clock row
		const lockQuery = `
			SELECT ` + clockColumns + `
			FROM clocks
			WHERE clock_id = $1 AND tenant_id = $2
			FOR UPDATE;`

		current, err := scanClock(tx.QueryRow(ctx, lockQuery, params.ClockID, tenantID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrClockNotFound
			}
			if mapped := mapPgError(err); errors.Is(mapped, domain.ErrInvalidIdentifier) {
				return domain.ErrClockNotFound
			}
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}

		if params.ExpectedVersion > 0 && current.Version != params.ExpectedVersion {
			return domain.ErrClockVersionMismatch
		}

		// Check if transition is valid
		allowedTransitions := domain.ClockStateTransitions[current.ClockState]
		valid := false
		for _, s := range allowedTransitions {
			if s == params.NewState {
				valid = true
				break
			}
		}
		if !valid {
			return domain.ErrInvalidClockTransition
		}

		// If already in the target state, return current clock without transition
		if current.ClockState == params.NewState {
			updated = current
			return nil
		}

		// Prepare update fields
		now := time.Now().UTC()
		setClauses := []string{
			"clock_state = $1",
			"version = version + 1",
			"updated_at = NOW()",
		}
		args := []any{params.NewState}
		argIdx := 2

		// Add state-specific fields
		switch params.NewState {
		case domain.ClockStateFulfilled:
			if len(params.EvidenceEvaluationIDs) == 0 {
				return domain.ErrInvalidClockTransition // fulfilled requires evidence
			}
			setClauses = append(setClauses, "satisfied_at = $"+strconv.Itoa(argIdx))
			args = append(args, now)
			argIdx++
			setClauses = append(setClauses, "satisfied_by_principal_id = $"+strconv.Itoa(argIdx))
			args = append(args, params.ActorPrincipalID)
			argIdx++
			setClauses = append(setClauses, "satisfied_evidence_ids = $"+strconv.Itoa(argIdx))
			args = append(args, params.EvidenceEvaluationIDs)
			argIdx++
		case domain.ClockStateWaived:
			if params.Reason == nil || *params.Reason == "" {
				return domain.ErrWaiverAuthorityRequired
			}
			if params.AuthorityPrincipalID == nil || *params.AuthorityPrincipalID == "" {
				return domain.ErrWaiverAuthorityRequired
			}
			setClauses = append(setClauses, "waived_at = $"+strconv.Itoa(argIdx))
			args = append(args, now)
			argIdx++
			setClauses = append(setClauses, "waived_by_principal_id = $"+strconv.Itoa(argIdx))
			args = append(args, params.ActorPrincipalID)
			argIdx++
			setClauses = append(setClauses, "waived_reason = $"+strconv.Itoa(argIdx))
			args = append(args, *params.Reason)
			argIdx++
			setClauses = append(setClauses, "waived_authority_principal_id = $"+strconv.Itoa(argIdx))
			args = append(args, *params.AuthorityPrincipalID)
			argIdx++
		case domain.ClockStateNotApplicable:
			if params.NotApplicableReason == nil || *params.NotApplicableReason == "" {
				return domain.ErrNotApplicableAuthorityRequired
			}
			if params.NotApplicableAuthorityPrincipalID == nil || *params.NotApplicableAuthorityPrincipalID == "" {
				return domain.ErrNotApplicableAuthorityRequired
			}
			setClauses = append(setClauses, "not_applicable_at = $"+strconv.Itoa(argIdx))
			args = append(args, now)
			argIdx++
			setClauses = append(setClauses, "not_applicable_by_principal_id = $"+strconv.Itoa(argIdx))
			args = append(args, params.ActorPrincipalID)
			argIdx++
			setClauses = append(setClauses, "not_applicable_reason = $"+strconv.Itoa(argIdx))
			args = append(args, *params.NotApplicableReason)
			argIdx++
			setClauses = append(setClauses, "not_applicable_authority_principal_id = $"+strconv.Itoa(argIdx))
			args = append(args, *params.NotApplicableAuthorityPrincipalID)
			argIdx++
		case domain.ClockStateSuperseded:
			setClauses = append(setClauses, "superseded_at = $"+strconv.Itoa(argIdx))
			args = append(args, now)
			argIdx++
			if params.SupersededByClockID != nil && *params.SupersededByClockID != "" {
				setClauses = append(setClauses, "superseded_by_clock_id = $"+strconv.Itoa(argIdx))
				args = append(args, *params.SupersededByClockID)
				argIdx++
			}
		}

		// Add correlation_id
		if params.CorrelationID != "" {
			setClauses = append(setClauses, "correlation_id = $"+strconv.Itoa(argIdx))
			args = append(args, params.CorrelationID)
			argIdx++
		}

		// Build and execute update query
		updateQuery := fmt.Sprintf(`
			UPDATE clocks
			SET %s
			WHERE clock_id = $%d AND tenant_id = $%d
			RETURNING `+clockColumns+";", strings.Join(setClauses, ", "), argIdx, argIdx+1)
		
		args = append(args, params.ClockID, tenantID)

		row := tx.QueryRow(ctx, updateQuery, args...)
		updated, err = scanClock(row)
		if err != nil {
			return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		transitioned = true
		return nil
	})

	if err != nil {
		return nil, false, err
	}
	return updated, transitioned, nil
}'''

content = content.replace(old_end, new_end)

# Write the file
with open('services/obligations-svc/internal/store/pg_store.go', 'w', encoding='utf-8') as f:
    f.write(content)

print("Done!")