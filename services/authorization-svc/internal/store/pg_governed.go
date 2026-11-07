package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
)

// Governed changes (migrations 000034-000036): the second person every
// emergency session, authority limit and SoD / ABAC rule change now waits
// for. Independence is held three times over — handler, the WHERE clauses
// here, and a database trigger or CHECK — so no single path can drop it.

const rulePublicationColumns = `publication_status, pending_change, change_requested_by, change_requested_at, change_reason, published_by, published_at, rejected_by, rejected_at`

// ── break-glass approval (AU-02) ────────────────────────────────────────────

// attachBreakGlassApprovals loads each session's approvals in the caller's
// transaction (same RLS scope).
func attachBreakGlassApprovals(ctx context.Context, tx pgx.Tx, sessions []*domain.BreakGlassSession) error {
	if len(sessions) == 0 {
		return nil
	}
	byID := make(map[string]*domain.BreakGlassSession, len(sessions))
	ids := make([]string, 0, len(sessions))
	for _, bg := range sessions {
		byID[bg.SessionID] = bg
		ids = append(ids, bg.SessionID)
	}
	rows, err := tx.Query(ctx, `
		SELECT session_id::text, approver_id, approved_at, approver_assurance, approver_authn_at, approver_identity_session
		  FROM break_glass_approvals
		 WHERE session_id = ANY($1::uuid[])
		 ORDER BY approved_at`, ids)
	if err != nil {
		if isUndefinedTable(err) {
			return nil
		}
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var sessionID string
		var a domain.BreakGlassApproval
		var assurance, idSession *string
		var authnAt *time.Time
		if err := rows.Scan(&sessionID, &a.ApproverID, &a.ApprovedAt, &assurance, &authnAt, &idSession); err != nil {
			return err
		}
		a.StepUp = stepUpEvidence(assurance, authnAt, idSession)
		if bg := byID[sessionID]; bg != nil {
			bg.Approvals = append(bg.Approvals, a)
		}
	}
	return rows.Err()
}

// ApproveBreakGlassSession records one independent approval of a pending
// break-glass request. When the approvals reach the quorum the session goes
// ACTIVE and its clock starts. Errors: ErrBreakGlassSessionNotFound;
// ErrSessionNotPending (not pending, or the request lapsed);
// ErrCheckerNotIndependent (the subject or requester); ErrAlreadyApproved.
func (s *PgStore) ApproveBreakGlassSession(ctx context.Context, sessionID, tenantID, approver string, stepUp *domain.StepUpEvidence) (*domain.BreakGlassSession, error) {
	if tenantID == "" {
		return nil, domain.ErrTenantScopeRequired
	}
	var out *domain.BreakGlassSession
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var status, subject string
		var requester *string
		var quorum int16
		var requestExpires *time.Time
		err := tx.QueryRow(ctx, `
			SELECT status, principal_id, requested_by, approvals_required, request_expires_at
			  FROM break_glass_sessions
			 WHERE session_id = $1::uuid AND tenant_id = $2::uuid
			 FOR UPDATE`, sessionID, tenantID).Scan(&status, &subject, &requester, &quorum, &requestExpires)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrBreakGlassSessionNotFound
		}
		if err != nil {
			return err
		}
		if status != domain.BreakGlassSessionStatusPendingApproval || (requestExpires != nil && !requestExpires.After(time.Now())) {
			return domain.ErrSessionNotPending
		}
		if approver == subject || (requester != nil && approver == *requester) {
			return domain.ErrCheckerNotIndependent
		}
		assurance, authnAt, idSession := stepUpColumns(stepUp)
		tag, err := tx.Exec(ctx, `
			INSERT INTO break_glass_approvals (session_id, tenant_id, approver_id, approver_assurance, approver_authn_at, approver_identity_session)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6)
			ON CONFLICT (session_id, approver_id) DO NOTHING`, sessionID, tenantID, approver, assurance, authnAt, idSession)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrAlreadyApproved
		}
		var approvals int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM break_glass_approvals WHERE session_id = $1::uuid`, sessionID).Scan(&approvals); err != nil {
			return err
		}
		query := `SELECT ` + breakGlassSessionColumns + ` FROM break_glass_sessions WHERE session_id = $1::uuid AND tenant_id = $2::uuid`
		args := []any{sessionID, tenantID}
		if approvals >= int(quorum) {
			// Quorum met: active from now, for the duration that was asked for.
			query = `
				UPDATE break_glass_sessions
				   SET status = 'ACTIVE', approved_by = $3, approved_at = NOW(),
				       expires_at = NOW() + make_interval(secs => duration_seconds)
				 WHERE session_id = $1::uuid AND tenant_id = $2::uuid AND status = 'PENDING_APPROVAL'
				RETURNING ` + breakGlassSessionColumns
			args = append(args, approver)
		}
		bg, err := scanBreakGlassSession(tx.QueryRow(ctx, query, args...))
		if err != nil {
			return err
		}
		out = bg
		return attachBreakGlassApprovals(ctx, tx, []*domain.BreakGlassSession{bg})
	})
	if err != nil {
		return nil, governedErr(s, "ApproveBreakGlassSession", err)
	}
	return out, nil
}

// RejectBreakGlassSession ends a pending request without granting anything.
// One independent rejection is enough: approval ambiguity means no
// activation (GOV-12 failure semantics).
func (s *PgStore) RejectBreakGlassSession(ctx context.Context, sessionID, tenantID, decider string) (*domain.BreakGlassSession, error) {
	if tenantID == "" {
		return nil, domain.ErrTenantScopeRequired
	}
	var out *domain.BreakGlassSession
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var status, subject string
		var requester *string
		err := tx.QueryRow(ctx, `
			SELECT status, principal_id, requested_by FROM break_glass_sessions
			 WHERE session_id = $1::uuid AND tenant_id = $2::uuid FOR UPDATE`, sessionID, tenantID).Scan(&status, &subject, &requester)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrBreakGlassSessionNotFound
		}
		if err != nil {
			return err
		}
		if status != domain.BreakGlassSessionStatusPendingApproval {
			return domain.ErrSessionNotPending
		}
		if decider == subject || (requester != nil && decider == *requester) {
			return domain.ErrCheckerNotIndependent
		}
		bg, err := scanBreakGlassSession(tx.QueryRow(ctx, `
			UPDATE break_glass_sessions SET status = 'REJECTED', rejected_by = $3, rejected_at = NOW()
			 WHERE session_id = $1::uuid AND tenant_id = $2::uuid AND status = 'PENDING_APPROVAL'
			RETURNING `+breakGlassSessionColumns, sessionID, tenantID, decider))
		if err != nil {
			return err
		}
		out = bg
		return attachBreakGlassApprovals(ctx, tx, []*domain.BreakGlassSession{bg})
	})
	if err != nil {
		return nil, governedErr(s, "RejectBreakGlassSession", err)
	}
	return out, nil
}

// ── SoD and ABAC rule publication (AU-17) ───────────────────────────────────

// ruleTable describes the one difference between the two rule tables the
// publication flow cares about.
type ruleTable struct {
	name, idColumn, columns string
	notFound                error
}

var (
	sodRuleTable  = ruleTable{name: "sod_rules", idColumn: "sod_rule_id", columns: sodRuleColumns, notFound: domain.ErrSoDRuleNotFound}
	abacRuleTable = ruleTable{name: "abac_rules", idColumn: "abac_rule_id", columns: abacRuleColumns, notFound: domain.ErrABACRuleNotFound}
)

// ruleState is what diagnoses a refused change.
type ruleState struct {
	active        bool
	publication   string
	pendingChange *string
	requester     *string
	version       int64
}

func (t ruleTable) state(ctx context.Context, tx pgx.Tx, ruleID, tenantID string, platformWide bool) (*ruleState, error) {
	q := `SELECT active_flag, publication_status, pending_change, change_requested_by, version FROM ` + t.name +
		` WHERE ` + t.idColumn + ` = $1 AND tenant_id = NULLIF($2, '')::uuid`
	if platformWide {
		q = `SELECT active_flag, publication_status, pending_change, change_requested_by, version FROM ` + t.name +
			` WHERE ` + t.idColumn + ` = $1 AND tenant_id IS NULL AND $2 = ''`
	}
	st := &ruleState{}
	err := tx.QueryRow(ctx, q, ruleID, tenantID).Scan(&st.active, &st.publication, &st.pendingChange, &st.requester, &st.version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, t.notFound
	}
	return st, err
}

// requestChange records a PUBLISH-already-done rule's RETIRE or REACTIVATE
// request. Returns changed=false (and the rule unchanged) when the rule is
// already in the requested state — the caller's intent holds, as the old
// idempotent retire / reactivate answered.
//
// Tenant-scoped rules only, exactly as SetSoDRuleActive: a platform-wide rule
// reads as absent from a tenant scope, so no tenant can ask to switch off a
// control binding every other one.
func (t ruleTable) requestChange(ctx context.Context, tx pgx.Tx, ruleID, tenantID, change, requester, reason string, expectedVersion int64) (pgx.Row, bool, error) {
	st, err := t.state(ctx, tx, ruleID, tenantID, false)
	if err != nil {
		return nil, false, err
	}
	if expectedVersion > 0 && st.version != expectedVersion {
		return nil, false, domain.ErrVersionConflict
	}
	switch {
	case st.publication == domain.PublicationRejected:
		return nil, false, domain.ErrRuleRejected
	case st.pendingChange != nil:
		return nil, false, domain.ErrChangePending
	case change == domain.RuleChangeRetire && !st.active, change == domain.RuleChangeReactivate && st.active:
		row := tx.QueryRow(ctx, `SELECT `+t.columns+` FROM `+t.name+` WHERE `+t.idColumn+` = $1`, ruleID)
		return row, false, nil
	}
	row := tx.QueryRow(ctx, `
		UPDATE `+t.name+`
		   SET pending_change = $3, change_requested_by = $4, change_requested_at = NOW(), change_reason = NULLIF($5, '')
		 WHERE `+t.idColumn+` = $1 AND tenant_id = $2::uuid AND pending_change IS NULL
		RETURNING `+t.columns, ruleID, tenantID, change, requester, reason)
	return row, true, nil
}

// decide applies (approve) or clears (reject) a rule's pending change. tenantID
// "" with platformWide decides a platform-wide rule; the handler gates that
// behind the platform-scope grant.
func (t ruleTable) decide(ctx context.Context, tx pgx.Tx, ruleID, tenantID string, platformWide bool, decider string, approve bool) (pgx.Row, error) {
	st, err := t.state(ctx, tx, ruleID, tenantID, platformWide)
	if err != nil {
		return nil, err
	}
	if st.pendingChange == nil {
		return nil, domain.ErrNoPendingChange
	}
	if st.requester != nil && *st.requester == decider {
		return nil, domain.ErrCheckerNotIndependent
	}
	scope := `tenant_id = NULLIF($2, '')::uuid`
	if platformWide {
		scope = `tenant_id IS NULL AND $2 = ''`
	}
	set := `active_flag = (pending_change <> 'RETIRE'), publication_status = 'PUBLISHED', pending_change = NULL,
	        published_by = $3, published_at = NOW()`
	if !approve {
		set = `publication_status = CASE WHEN pending_change = 'PUBLISH' THEN 'REJECTED' ELSE publication_status END,
		       pending_change = NULL, rejected_by = $3, rejected_at = NOW()`
	}
	return tx.QueryRow(ctx, `
		UPDATE `+t.name+` SET `+set+`
		 WHERE `+t.idColumn+` = $1 AND `+scope+`
		   AND pending_change IS NOT NULL AND change_requested_by IS DISTINCT FROM $3
		RETURNING `+t.columns, ruleID, tenantID, decider), nil
}

// FindSoDRule reads one rule visible in tenantID's scope: the tenant's own or
// a platform-wide one.
func (s *PgStore) FindSoDRule(ctx context.Context, sodRuleID, tenantID string) (*domain.SoDRule, error) {
	var r *domain.SoDRule
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var scanErr error
		r, scanErr = scanSoDRule(tx.QueryRow(ctx, `SELECT `+sodRuleColumns+` FROM sod_rules
			WHERE sod_rule_id = $1 AND (tenant_id IS NULL OR tenant_id = NULLIF($2, '')::uuid)`, sodRuleID, tenantID))
		return scanErr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrSoDRuleNotFound
	}
	if err != nil {
		return nil, governedErr(s, "FindSoDRule", err)
	}
	return r, nil
}

// RequestSoDRuleChange asks for a tenant rule to be retired or reactivated.
// It changes nothing a decision reads until DecideSoDRuleChange approves it.
func (s *PgStore) RequestSoDRuleChange(ctx context.Context, sodRuleID, tenantID, change, requester, reason string, expectedVersion int64) (*domain.SoDRule, bool, error) {
	var r *domain.SoDRule
	var changed bool
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row, ch, err := sodRuleTable.requestChange(ctx, tx, sodRuleID, tenantID, change, requester, reason, expectedVersion)
		if err != nil {
			return err
		}
		changed = ch
		r, err = scanSoDRule(row)
		return err
	})
	if err != nil {
		return nil, false, governedErr(s, "RequestSoDRuleChange", err)
	}
	return r, changed, nil
}

// DecideSoDRuleChange approves or rejects a rule's pending change.
func (s *PgStore) DecideSoDRuleChange(ctx context.Context, sodRuleID, tenantID string, platformWide bool, decider string, approve bool) (*domain.SoDRule, error) {
	var r *domain.SoDRule
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := sodRuleTable.decide(ctx, tx, sodRuleID, tenantID, platformWide, decider, approve)
		if err != nil {
			return err
		}
		r, err = scanSoDRule(row)
		return err
	})
	if err != nil {
		return nil, governedErr(s, "DecideSoDRuleChange", err)
	}
	return r, nil
}

// FindABACRule reads one rule visible in tenantID's scope.
func (s *PgStore) FindABACRule(ctx context.Context, abacRuleID, tenantID string) (*domain.ABACRule, error) {
	var r *domain.ABACRule
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var scanErr error
		r, scanErr = scanABACRule(tx.QueryRow(ctx, `SELECT `+abacRuleColumns+` FROM abac_rules
			WHERE abac_rule_id = $1 AND (tenant_id IS NULL OR tenant_id = NULLIF($2, '')::uuid)`, abacRuleID, tenantID))
		return scanErr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrABACRuleNotFound
	}
	if err != nil {
		return nil, governedErr(s, "FindABACRule", err)
	}
	return r, nil
}

// RequestABACRuleChange — see RequestSoDRuleChange.
func (s *PgStore) RequestABACRuleChange(ctx context.Context, abacRuleID, tenantID, change, requester, reason string, expectedVersion int64) (*domain.ABACRule, bool, error) {
	var r *domain.ABACRule
	var changed bool
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row, ch, err := abacRuleTable.requestChange(ctx, tx, abacRuleID, tenantID, change, requester, reason, expectedVersion)
		if err != nil {
			return err
		}
		changed = ch
		r, err = scanABACRule(row)
		return err
	})
	if err != nil {
		return nil, false, governedErr(s, "RequestABACRuleChange", err)
	}
	return r, changed, nil
}

// DecideABACRuleChange — see DecideSoDRuleChange.
func (s *PgStore) DecideABACRuleChange(ctx context.Context, abacRuleID, tenantID string, platformWide bool, decider string, approve bool) (*domain.ABACRule, error) {
	var r *domain.ABACRule
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := abacRuleTable.decide(ctx, tx, abacRuleID, tenantID, platformWide, decider, approve)
		if err != nil {
			return err
		}
		r, err = scanABACRule(row)
		return err
	})
	if err != nil {
		return nil, governedErr(s, "DecideABACRuleChange", err)
	}
	return r, nil
}

// ── authority limits (AU-03) ────────────────────────────────────────────────

// RequestAuthorityLimitRetire marks an ACTIVE limit for retirement. It keeps
// limiting until a checker approves (DecideAuthorityLimit).
func (s *PgStore) RequestAuthorityLimitRetire(ctx context.Context, limitID, tenantID, requester, reason string, expectedVersion int64) (*domain.AuthorityLimit, error) {
	if tenantID == "" {
		return nil, domain.ErrTenantScopeRequired
	}
	var al *domain.AuthorityLimit
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		cur, err := scanAuthorityLimit(tx.QueryRow(ctx, `SELECT `+authorityLimitColumns+` FROM authority_limits
			WHERE authority_limit_id = $1::uuid AND tenant_id = $2::uuid FOR UPDATE`, limitID, tenantID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrAuthorityLimitNotFound
		}
		if err != nil {
			return err
		}
		if expectedVersion > 0 && cur.Version != expectedVersion {
			return domain.ErrVersionConflict
		}
		if cur.Status != domain.AuthorityLimitActive {
			return domain.ErrInvalidTransition
		}
		var openChange bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM authority_limits WHERE supersedes_id = $1::uuid AND status = 'PENDING_APPROVAL')`,
			limitID).Scan(&openChange); err != nil {
			return err
		}
		if cur.PendingAction != nil || openChange {
			return domain.ErrChangePending
		}
		al, err = scanAuthorityLimit(tx.QueryRow(ctx, `
			UPDATE authority_limits
			   SET pending_action = 'RETIRE', requested_by = $3, requested_at = NOW(), request_reason = NULLIF($4, '')
			 WHERE authority_limit_id = $1::uuid AND tenant_id = $2::uuid
			RETURNING `+authorityLimitColumns, limitID, tenantID, requester, reason))
		return err
	})
	if err != nil {
		return nil, governedErr(s, "RequestAuthorityLimitRetire", err)
	}
	return al, nil
}

// DecideAuthorityLimit approves or rejects whatever is pending on limitID: a
// new limit or a change (the row is PENDING_APPROVAL), or a retirement
// (pending_action RETIRE). Approving a change supersedes the limit it replaces
// in the same transaction, so there is no instant with both or neither.
func (s *PgStore) DecideAuthorityLimit(ctx context.Context, limitID, tenantID, decider string, approve bool) (*domain.AuthorityLimit, error) {
	if tenantID == "" {
		return nil, domain.ErrTenantScopeRequired
	}
	decision := "REJECTED"
	if approve {
		decision = "APPROVED"
	}
	var al *domain.AuthorityLimit
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		cur, err := scanAuthorityLimit(tx.QueryRow(ctx, `SELECT `+authorityLimitColumns+` FROM authority_limits
			WHERE authority_limit_id = $1::uuid AND tenant_id = $2::uuid FOR UPDATE`, limitID, tenantID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrAuthorityLimitNotFound
		}
		if err != nil {
			return err
		}
		pendingRow := cur.Status == domain.AuthorityLimitPending
		pendingRetire := cur.Status == domain.AuthorityLimitActive && cur.PendingAction != nil
		if !pendingRow && !pendingRetire {
			return domain.ErrNoPendingChange
		}
		if cur.RequestedBy != nil && *cur.RequestedBy == decider {
			return domain.ErrCheckerNotIndependent
		}

		var set string
		switch {
		case pendingRow && approve:
			if cur.SupersedesID != nil {
				// The replaced limit stops at the instant this one starts. Its
				// own decision columns are left alone: they record who approved
				// IT, and rewriting them would trip the maker-checker trigger
				// whenever its maker approves the change.
				tag, err := tx.Exec(ctx, `
					UPDATE authority_limits
					   SET status = 'SUPERSEDED',
					       effective_to = CASE WHEN effective_to IS NULL OR effective_to > NOW() THEN NOW() ELSE effective_to END
					 WHERE authority_limit_id = $1::uuid AND tenant_id = $2::uuid AND status = 'ACTIVE'`, *cur.SupersedesID, tenantID)
				if err != nil {
					return err
				}
				if tag.RowsAffected() == 0 {
					// Retired (or replaced) since the change was requested.
					return domain.ErrConflict
				}
			}
			set = `status = 'ACTIVE'`
		case pendingRow:
			set = `status = 'REJECTED'`
		case approve:
			set = `status = 'RETIRED', pending_action = NULL,
			       effective_to = CASE WHEN effective_to IS NULL OR effective_to > NOW() THEN NOW() ELSE effective_to END`
		default:
			set = `pending_action = NULL`
		}
		al, err = scanAuthorityLimit(tx.QueryRow(ctx, `
			UPDATE authority_limits
			   SET `+set+`, decided_by = $3, decided_at = NOW(), last_decision = $4
			 WHERE authority_limit_id = $1::uuid AND tenant_id = $2::uuid
			RETURNING `+authorityLimitColumns, limitID, tenantID, decider, decision))
		return err
	})
	if err != nil {
		return nil, governedErr(s, "DecideAuthorityLimit", err)
	}
	return al, nil
}

// governedErr passes the domain errors these methods diagnose through
// untouched and reports anything else as the store being unavailable.
func governedErr(s *PgStore, op string, err error) error {
	for _, known := range []error{
		domain.ErrBreakGlassSessionNotFound, domain.ErrSessionNotPending, domain.ErrCheckerNotIndependent,
		domain.ErrAlreadyApproved, domain.ErrSoDRuleNotFound, domain.ErrABACRuleNotFound, domain.ErrVersionConflict,
		domain.ErrRuleRejected, domain.ErrChangePending, domain.ErrNoPendingChange, domain.ErrAuthorityLimitNotFound,
		domain.ErrInvalidTransition, domain.ErrConflict, domain.ErrTenantScopeRequired,
	} {
		if errors.Is(err, known) {
			return err
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		// The guarded UPDATE matched nothing after the state check passed: a
		// concurrent decision got there first.
		return domain.ErrNoPendingChange
	}
	s.log.Error("pg "+op+" failed", zap.Error(err))
	return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
}
