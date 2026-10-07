// Package store is the PgStore persistence layer for assistant-rag-svc
// (AI-03, ZS-SVC-N-001 §4/§13 Wave 7). Every method runs inside one
// transaction that first declares app.tenant_id for RLS, then performs
// the write. Retrieve evaluates every candidate source against the
// tenant's SourceGrant registry before anything is persisted, so an
// unauthorized source can never reach a RetrievalSet; Ask can only cite
// sources already marked authorized in a given RetrievalSet, so
// generation structurally cannot assert authority over unauthorized
// evidence. A Protected tool proposal whose justification derives from
// retrieved content can never auto-approve, regardless of how the
// proposal is phrased.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/assistant-rag-svc/internal/domain"
	"zoiko.io/assistant-rag-svc/internal/outbox"
)

// Store is the AI-03 persistence contract.
type Store interface {
	RegisterSourceGrant(ctx context.Context, tenantID string, req domain.RegisterSourceGrantRequest, actor string) (*domain.SourceGrant, error)
	RegisterToolPolicy(ctx context.Context, tenantID string, req domain.RegisterToolPolicyRequest, actor string) (*domain.ToolPolicy, error)

	StartSession(ctx context.Context, tenantID string, req domain.StartSessionRequest, actor string, claim domain.IdempotencyClaim) (*domain.AssistantSession, error)
	Retrieve(ctx context.Context, tenantID string, req domain.RetrieveRequest, actor string, claim domain.IdempotencyClaim) (*domain.RetrievalSet, error)
	Ask(ctx context.Context, tenantID string, req domain.AskRequest, actor string, claim domain.IdempotencyClaim) (*domain.PromptExecution, error)
	ProposeToolCall(ctx context.Context, tenantID string, req domain.ProposeToolCallRequest, actor string, claim domain.IdempotencyClaim) (*domain.ToolProposal, error)
	ApproveToolProposal(ctx context.Context, tenantID, proposalID string, req domain.DecideToolProposalRequest, actor string, claim domain.IdempotencyClaim) (*domain.ToolProposal, error)
	RejectToolProposal(ctx context.Context, tenantID, proposalID string, req domain.DecideToolProposalRequest, actor string, claim domain.IdempotencyClaim) (*domain.ToolProposal, error)
	ExecuteApprovedToolCall(ctx context.Context, tenantID, proposalID string, actor string, claim domain.IdempotencyClaim) (*domain.ToolProposal, error)
	EndSession(ctx context.Context, tenantID, sessionID string, status domain.SessionStatus, actor string, claim domain.IdempotencyClaim) (*domain.AssistantSession, error)

	GetSession(ctx context.Context, tenantID, sessionID string) (*domain.AssistantSession, error)
	GetRetrievedItems(ctx context.Context, tenantID, retrievalSetID string) ([]domain.RetrievedItem, error)
	GetCitations(ctx context.Context, tenantID, executionID string) ([]domain.Citation, error)
	GetEvidence(ctx context.Context, tenantID, executionID string) (*domain.AIResponseEvidence, error)
	GetToolProposal(ctx context.Context, tenantID, proposalID string) (*domain.ToolProposal, error)
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

var _ Store = (*PgStore)(nil)

func (s *PgStore) withTenant(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	if tenantID == "" {
		return errors.New("tenant_id is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return mapErr(err)
	}
	return tx.Commit(ctx)
}

func mapErr(err error) error {
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) {
		return err
	}
	return fmt.Errorf("assistant-rag-svc: %s", pgErr.Message)
}

func claimIdempotency(ctx context.Context, tx pgx.Tx, c domain.IdempotencyClaim) error {
	tag, err := tx.Exec(ctx, `
		INSERT INTO idempotency_keys (tenant_id, owner_scope, principal_id, idempotency_key, operation, request_sha256, resource_id)
		VALUES (current_setting('app.tenant_id', true), $1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, owner_scope, principal_id, idempotency_key) DO NOTHING`,
		c.OwnerScope, c.PrincipalID, c.Key, c.Operation, c.RequestSHA256, c.ResourceID)
	if err != nil {
		return fmt.Errorf("claim idempotency key: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var reqHash, resourceID string
	if err := tx.QueryRow(ctx, `
		SELECT request_sha256, resource_id FROM idempotency_keys
		WHERE tenant_id = current_setting('app.tenant_id', true) AND owner_scope = $1 AND principal_id = $2 AND idempotency_key = $3`,
		c.OwnerScope, c.PrincipalID, c.Key).Scan(&reqHash, &resourceID); err != nil {
		return fmt.Errorf("read idempotency claim: %w", err)
	}
	if reqHash != c.RequestSHA256 {
		return domain.ErrIdempotencyKeyReused
	}
	return &domain.IdempotentReplayError{ResourceID: resourceID}
}

func newID(prefix string) string {
	return prefix + uuid.NewString()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ── Governance registries ───────────────────────────────────────────────────

func (s *PgStore) RegisterSourceGrant(ctx context.Context, tenantID string, req domain.RegisterSourceGrantRequest, actor string) (*domain.SourceGrant, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.SourceGrant
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var g domain.SourceGrant
		err := tx.QueryRow(ctx, `
			INSERT INTO source_grants (tenant_id, source_ref, purpose, created_by)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (tenant_id, source_ref, purpose) DO UPDATE SET created_by = EXCLUDED.created_by
			RETURNING tenant_id, source_ref, purpose, created_at, created_by`,
			tenantID, req.SourceRef, req.Purpose, actor).
			Scan(&g.TenantID, &g.SourceRef, &g.Purpose, &g.CreatedAt, &g.CreatedBy)
		if err != nil {
			return fmt.Errorf("register source grant: %w", err)
		}
		out = &g
		return nil
	})
	return out, err
}

func (s *PgStore) RegisterToolPolicy(ctx context.Context, tenantID string, req domain.RegisterToolPolicyRequest, actor string) (*domain.ToolPolicy, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.ToolPolicy
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var p domain.ToolPolicy
		err := tx.QueryRow(ctx, `
			INSERT INTO tool_policies (tenant_id, tool_name, target_domain, protected, created_by)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (tenant_id, tool_name) DO UPDATE SET target_domain = EXCLUDED.target_domain, protected = EXCLUDED.protected
			RETURNING tenant_id, tool_name, target_domain, protected, created_at, created_by`,
			tenantID, req.ToolName, req.TargetDomain, req.Protected, actor).
			Scan(&p.TenantID, &p.ToolName, &p.TargetDomain, &p.Protected, &p.CreatedAt, &p.CreatedBy)
		if err != nil {
			return fmt.Errorf("register tool policy: %w", err)
		}
		out = &p
		return nil
	})
	return out, err
}

func loadToolPolicy(ctx context.Context, tx pgx.Tx, tenantID, toolName string) (*domain.ToolPolicy, error) {
	var p domain.ToolPolicy
	err := tx.QueryRow(ctx, `SELECT tenant_id, tool_name, target_domain, protected, created_at, created_by
		FROM tool_policies WHERE tenant_id = $1 AND tool_name = $2`, tenantID, toolName).
		Scan(&p.TenantID, &p.ToolName, &p.TargetDomain, &p.Protected, &p.CreatedAt, &p.CreatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrToolPolicyNotFound
	}
	return &p, err
}

// ── AssistantSession ─────────────────────────────────────────────────────────

const sessionColumns = `session_id, tenant_id, subject_id, purpose, status, created_at, created_by, ended_at, ended_by`

func scanSession(row pgx.Row) (*domain.AssistantSession, error) {
	var sess domain.AssistantSession
	var endedBy *string
	if err := row.Scan(&sess.SessionID, &sess.TenantID, &sess.SubjectID, &sess.Purpose, &sess.Status,
		&sess.CreatedAt, &sess.CreatedBy, &sess.EndedAt, &endedBy); err != nil {
		return nil, err
	}
	if endedBy != nil {
		sess.EndedBy = *endedBy
	}
	return &sess, nil
}

func (s *PgStore) StartSession(ctx context.Context, tenantID string, req domain.StartSessionRequest, actor string, claim domain.IdempotencyClaim) (*domain.AssistantSession, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.AssistantSession
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		sessionID := newID(domain.PrefixSession)
		claim.ResourceID = sessionID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		sess, err := scanSession(tx.QueryRow(ctx, `
			INSERT INTO assistant_sessions (session_id, tenant_id, subject_id, purpose, created_by)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING `+sessionColumns,
			sessionID, tenantID, req.SubjectID, req.Purpose, actor))
		if err != nil {
			return fmt.Errorf("start session: %w", err)
		}
		out = sess
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "assistant_session", AggregateID: sessionID,
			EventType: "AI.SessionStarted", TenantID: &tenantID, Payload: sess})
	})
	return out, err
}

func loadSessionForUpdate(ctx context.Context, tx pgx.Tx, tenantID, sessionID string) (*domain.AssistantSession, error) {
	sess, err := scanSession(tx.QueryRow(ctx, `SELECT `+sessionColumns+` FROM assistant_sessions WHERE tenant_id = $1 AND session_id = $2 FOR UPDATE`,
		tenantID, sessionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrSessionNotFound
	}
	return sess, err
}

func (s *PgStore) GetSession(ctx context.Context, tenantID, sessionID string) (*domain.AssistantSession, error) {
	var out *domain.AssistantSession
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		sess, err := scanSession(tx.QueryRow(ctx, `SELECT `+sessionColumns+` FROM assistant_sessions WHERE tenant_id = $1 AND session_id = $2`, tenantID, sessionID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrSessionNotFound
		}
		out = sess
		return err
	})
	return out, err
}

func (s *PgStore) EndSession(ctx context.Context, tenantID, sessionID string, status domain.SessionStatus, actor string, claim domain.IdempotencyClaim) (*domain.AssistantSession, error) {
	if status != domain.SessionCompleted && status != domain.SessionBlocked && status != domain.SessionEscalated {
		return nil, fmt.Errorf("status must be Completed, Blocked or Escalated")
	}
	var out *domain.AssistantSession
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		sess, err := loadSessionForUpdate(ctx, tx, tenantID, sessionID)
		if err != nil {
			return err
		}
		claim.ResourceID = sessionID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		if sess.Status != domain.SessionActive {
			return domain.ErrSessionNotActive
		}
		updated, err := scanSession(tx.QueryRow(ctx, `
			UPDATE assistant_sessions SET status = $2, ended_at = NOW(), ended_by = $3 WHERE session_id = $1
			RETURNING `+sessionColumns, sessionID, status, actor))
		if err != nil {
			return fmt.Errorf("end session: %w", err)
		}
		out = updated
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "assistant_session", AggregateID: sessionID,
			EventType: "AI.SessionEnded", TenantID: &tenantID, Payload: updated})
	})
	return out, err
}

// ── Retrieve / RetrievalSet ──────────────────────────────────────────────────

// Retrieve evaluates every candidate source against the tenant's
// source_grants registry for this session's purpose BEFORE any
// RetrievalSet row exists. Unauthorized candidates are recorded for
// audit with authorized=false, but can never later be cited — this is
// the structural enforcement of "RAG cannot retrieve unauthorized
// source before generation."
func (s *PgStore) Retrieve(ctx context.Context, tenantID string, req domain.RetrieveRequest, actor string, claim domain.IdempotencyClaim) (*domain.RetrievalSet, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.RetrievalSet
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		sess, err := loadSessionForUpdate(ctx, tx, tenantID, req.SessionID)
		if err != nil {
			return err
		}

		setID := newID(domain.PrefixRetrievalSet)
		claim.ResourceID = setID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		if sess.Status != domain.SessionActive {
			return domain.ErrSessionNotActive
		}

		var set domain.RetrievalSet
		if err := tx.QueryRow(ctx, `
			INSERT INTO retrieval_sets (retrieval_set_id, tenant_id, session_id, query, created_by)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING retrieval_set_id, tenant_id, session_id, query, created_at, created_by`,
			setID, tenantID, req.SessionID, req.Query, actor).
			Scan(&set.RetrievalSetID, &set.TenantID, &set.SessionID, &set.Query, &set.CreatedAt, &set.CreatedBy); err != nil {
			return fmt.Errorf("create retrieval set: %w", err)
		}

		for _, c := range req.Candidates {
			var authorized bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(
				SELECT 1 FROM source_grants WHERE tenant_id = $1 AND source_ref = $2 AND purpose = $3)`,
				tenantID, c.SourceRef, sess.Purpose).Scan(&authorized); err != nil {
				return fmt.Errorf("check source grant: %w", err)
			}
			itemID := newID(domain.PrefixRetrievedItem)
			var excludedReason *string
			if !authorized {
				reason := "not_granted_for_purpose"
				excludedReason = &reason
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO retrieved_items (item_id, tenant_id, retrieval_set_id, source_ref, snippet, authorized, excluded_reason)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				itemID, tenantID, setID, c.SourceRef, c.Snippet, authorized, excludedReason); err != nil {
				return fmt.Errorf("insert retrieved item: %w", err)
			}
		}

		out = &set
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "retrieval_set", AggregateID: setID,
			EventType: "AI.SourcesRetrieved", TenantID: &tenantID, Payload: set})
	})
	return out, err
}

func (s *PgStore) GetRetrievedItems(ctx context.Context, tenantID, retrievalSetID string) ([]domain.RetrievedItem, error) {
	var out []domain.RetrievedItem
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT item_id, tenant_id, retrieval_set_id, source_ref, snippet, authorized, COALESCE(excluded_reason, ''), created_at
			FROM retrieved_items WHERE tenant_id = $1 AND retrieval_set_id = $2 ORDER BY created_at`, tenantID, retrievalSetID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var it domain.RetrievedItem
			if err := rows.Scan(&it.ItemID, &it.TenantID, &it.RetrievalSetID, &it.SourceRef, &it.Snippet, &it.Authorized, &it.ExcludedReason, &it.CreatedAt); err != nil {
				return err
			}
			out = append(out, it)
		}
		return rows.Err()
	})
	return out, err
}

// ── Ask / PromptExecution / Citation ────────────────────────────────────────

// Ask records one model execution and its citations. Every citation
// must reference a source_ref that exists AND is authorized within the
// given retrieval set — a citation to an unauthorized or absent source
// is refused outright, so an answer can never assert authority over
// evidence it was never permitted to retrieve.
func (s *PgStore) Ask(ctx context.Context, tenantID string, req domain.AskRequest, actor string, claim domain.IdempotencyClaim) (*domain.PromptExecution, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.PromptExecution
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		sess, err := loadSessionForUpdate(ctx, tx, tenantID, req.SessionID)
		if err != nil {
			return err
		}

		execID := newID(domain.PrefixExecution)
		claim.ResourceID = execID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		if sess.Status != domain.SessionActive {
			return domain.ErrSessionNotActive
		}

		var setExists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM retrieval_sets WHERE tenant_id = $1 AND retrieval_set_id = $2 AND session_id = $3)`,
			tenantID, req.RetrievalSetID, req.SessionID).Scan(&setExists); err != nil {
			return fmt.Errorf("check retrieval set: %w", err)
		}
		if !setExists {
			return domain.ErrRetrievalSetNotFound
		}

		for _, c := range req.Citations {
			var authorized bool
			if err := tx.QueryRow(ctx, `SELECT COALESCE(
				(SELECT authorized FROM retrieved_items WHERE tenant_id = $1 AND retrieval_set_id = $2 AND source_ref = $3), FALSE)`,
				tenantID, req.RetrievalSetID, c.SourceRef).Scan(&authorized); err != nil {
				return fmt.Errorf("check citation authorization: %w", err)
			}
			if !authorized {
				return domain.ErrCitationSourceNotAuthorized
			}
		}

		contentHash := sha256Hex([]byte(req.AnswerText))
		var exec domain.PromptExecution
		if err := tx.QueryRow(ctx, `
			INSERT INTO prompt_executions (execution_id, tenant_id, session_id, retrieval_set_id, model_provider, model_version,
				prompt_version, query, answer_text, content_hash)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			RETURNING execution_id, tenant_id, session_id, retrieval_set_id, model_provider, model_version, prompt_version, query, answer_text, content_hash, created_at`,
			execID, tenantID, req.SessionID, req.RetrievalSetID, req.ModelProvider, req.ModelVersion, req.PromptVersion, sess.Purpose, req.AnswerText, contentHash).
			Scan(&exec.ExecutionID, &exec.TenantID, &exec.SessionID, &exec.RetrievalSetID, &exec.ModelProvider, &exec.ModelVersion,
				&exec.PromptVersion, &exec.Query, &exec.AnswerText, &exec.ContentHash, &exec.CreatedAt); err != nil {
			return fmt.Errorf("create prompt execution: %w", err)
		}

		for _, c := range req.Citations {
			citID := newID(domain.PrefixCitation)
			if _, err := tx.Exec(ctx, `
				INSERT INTO citations (citation_id, tenant_id, execution_id, source_ref, snippet)
				VALUES ($1, $2, $3, $4, $5)`,
				citID, tenantID, execID, c.SourceRef, c.Snippet); err != nil {
				return fmt.Errorf("insert citation: %w", err)
			}
		}

		evID := newID(domain.PrefixEvidence)
		if _, err := tx.Exec(ctx, `
			INSERT INTO ai_response_evidence (evidence_id, tenant_id, execution_id, outcome, citation_count)
			VALUES ($1, $2, $3, 'Answered', $4)`,
			evID, tenantID, execID, len(req.Citations)); err != nil {
			return fmt.Errorf("record response evidence: %w", err)
		}

		out = &exec
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "prompt_execution", AggregateID: execID,
			EventType: "AI.AnswerGenerated", TenantID: &tenantID, Payload: exec})
	})
	return out, err
}

func (s *PgStore) GetCitations(ctx context.Context, tenantID, executionID string) ([]domain.Citation, error) {
	var out []domain.Citation
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT citation_id, tenant_id, execution_id, source_ref, snippet, created_at
			FROM citations WHERE tenant_id = $1 AND execution_id = $2 ORDER BY created_at`, tenantID, executionID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c domain.Citation
			if err := rows.Scan(&c.CitationID, &c.TenantID, &c.ExecutionID, &c.SourceRef, &c.Snippet, &c.CreatedAt); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

func (s *PgStore) GetEvidence(ctx context.Context, tenantID, executionID string) (*domain.AIResponseEvidence, error) {
	var out *domain.AIResponseEvidence
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var ev domain.AIResponseEvidence
		err := tx.QueryRow(ctx, `SELECT evidence_id, tenant_id, execution_id, outcome, citation_count, created_at
			FROM ai_response_evidence WHERE tenant_id = $1 AND execution_id = $2`, tenantID, executionID).
			Scan(&ev.EvidenceID, &ev.TenantID, &ev.ExecutionID, &ev.Outcome, &ev.CitationCount, &ev.CreatedAt)
		if err != nil {
			return err
		}
		out = &ev
		return nil
	})
	return out, err
}

// ── ToolProposal ─────────────────────────────────────────────────────────────

const toolProposalColumns = `proposal_id, tenant_id, session_id, tool_name, target_domain, protected, justification,
	justification_source, status, created_at, created_by, decided_at, decided_by, decision_reason`

func scanToolProposal(row pgx.Row) (*domain.ToolProposal, error) {
	var p domain.ToolProposal
	var decidedBy, decisionReason *string
	if err := row.Scan(&p.ProposalID, &p.TenantID, &p.SessionID, &p.ToolName, &p.TargetDomain, &p.Protected, &p.Justification,
		&p.JustificationSource, &p.Status, &p.CreatedAt, &p.CreatedBy, &p.DecidedAt, &decidedBy, &decisionReason); err != nil {
		return nil, err
	}
	if decidedBy != nil {
		p.DecidedBy = *decidedBy
	}
	if decisionReason != nil {
		p.DecisionReason = *decisionReason
	}
	return &p, nil
}

// ProposeToolCall refuses any tool absent from the tool policy
// registry. target_domain/protected are frozen from that registry at
// creation. A Protected tool proposed with JustificationRetrievedContent
// can never auto-approve — it always lands Proposed, awaiting an
// explicit human decision — regardless of how the justification reads;
// this is what makes "retrieved prompt injection cannot invoke a
// protected tool" true structurally rather than by confidence scoring.
func (s *PgStore) ProposeToolCall(ctx context.Context, tenantID string, req domain.ProposeToolCallRequest, actor string, claim domain.IdempotencyClaim) (*domain.ToolProposal, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var out *domain.ToolProposal
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		sess, err := loadSessionForUpdate(ctx, tx, tenantID, req.SessionID)
		if err != nil {
			return err
		}

		proposalID := newID(domain.PrefixToolProposal)
		claim.ResourceID = proposalID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}

		if sess.Status != domain.SessionActive {
			return domain.ErrSessionNotActive
		}

		policy, err := loadToolPolicy(ctx, tx, tenantID, req.ToolName)
		if err != nil {
			return err
		}

		autoApprove := !policy.Protected || req.JustificationSource == domain.JustificationSystem
		status := domain.ToolProposed
		if autoApprove {
			status = domain.ToolApproved
		}

		var p *domain.ToolProposal
		if autoApprove {
			p, err = scanToolProposal(tx.QueryRow(ctx, `
				INSERT INTO tool_proposals (proposal_id, tenant_id, session_id, tool_name, target_domain, protected,
					justification, justification_source, status, created_by, decided_at, decided_by, decision_reason)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW(), 'system-auto-approve', 'not protected or explicitly system-directed')
				RETURNING `+toolProposalColumns,
				proposalID, tenantID, req.SessionID, req.ToolName, policy.TargetDomain, policy.Protected,
				req.Justification, req.JustificationSource, status, actor))
		} else {
			p, err = scanToolProposal(tx.QueryRow(ctx, `
				INSERT INTO tool_proposals (proposal_id, tenant_id, session_id, tool_name, target_domain, protected,
					justification, justification_source, status, created_by)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
				RETURNING `+toolProposalColumns,
				proposalID, tenantID, req.SessionID, req.ToolName, policy.TargetDomain, policy.Protected,
				req.Justification, req.JustificationSource, status, actor))
		}
		if err != nil {
			return fmt.Errorf("create tool proposal: %w", err)
		}

		out = p
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "tool_proposal", AggregateID: proposalID,
			EventType: "AI.ToolCallProposed", TenantID: &tenantID, Payload: p})
	})
	return out, err
}

func loadToolProposalForUpdate(ctx context.Context, tx pgx.Tx, tenantID, proposalID string) (*domain.ToolProposal, error) {
	p, err := scanToolProposal(tx.QueryRow(ctx, `SELECT `+toolProposalColumns+` FROM tool_proposals WHERE tenant_id = $1 AND proposal_id = $2 FOR UPDATE`,
		tenantID, proposalID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrToolProposalNotFound
	}
	return p, err
}

func (s *PgStore) ApproveToolProposal(ctx context.Context, tenantID, proposalID string, req domain.DecideToolProposalRequest, actor string, claim domain.IdempotencyClaim) (*domain.ToolProposal, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	return s.decideToolProposal(ctx, tenantID, proposalID, domain.ToolApproved, req.Reason, actor, claim)
}

func (s *PgStore) RejectToolProposal(ctx context.Context, tenantID, proposalID string, req domain.DecideToolProposalRequest, actor string, claim domain.IdempotencyClaim) (*domain.ToolProposal, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	return s.decideToolProposal(ctx, tenantID, proposalID, domain.ToolRejected, req.Reason, actor, claim)
}

func (s *PgStore) decideToolProposal(ctx context.Context, tenantID, proposalID string, decision domain.ToolProposalStatus, reason, actor string, claim domain.IdempotencyClaim) (*domain.ToolProposal, error) {
	var out *domain.ToolProposal
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		p, err := loadToolProposalForUpdate(ctx, tx, tenantID, proposalID)
		if err != nil {
			return err
		}
		claim.ResourceID = proposalID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		if p.Status != domain.ToolProposed {
			return domain.ErrToolProposalNotPending
		}
		updated, err := scanToolProposal(tx.QueryRow(ctx, `
			UPDATE tool_proposals SET status = $2, decided_at = NOW(), decided_by = $3, decision_reason = $4 WHERE proposal_id = $1
			RETURNING `+toolProposalColumns, proposalID, decision, actor, reason))
		if err != nil {
			return fmt.Errorf("decide tool proposal: %w", err)
		}
		out = updated
		eventType := "AI.ToolCallApproved"
		if decision == domain.ToolRejected {
			eventType = "AI.PolicyBlocked"
		}
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "tool_proposal", AggregateID: proposalID,
			EventType: eventType, TenantID: &tenantID, Payload: updated})
	})
	return out, err
}

// ExecuteApprovedToolCall re-authorizes an Approved proposal and marks
// it Executed. It does not perform the target-domain mutation itself —
// AI-03 has no business authority over other domains (see the package
// doc comment's "explicit non-ownership"); the caller invokes the
// target domain's own command, with the target domain's own
// idempotency key and authority check, immediately after this succeeds.
func (s *PgStore) ExecuteApprovedToolCall(ctx context.Context, tenantID, proposalID string, actor string, claim domain.IdempotencyClaim) (*domain.ToolProposal, error) {
	var out *domain.ToolProposal
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		p, err := loadToolProposalForUpdate(ctx, tx, tenantID, proposalID)
		if err != nil {
			return err
		}
		claim.ResourceID = proposalID
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		if p.Status != domain.ToolApproved {
			return domain.ErrToolProposalNotApproved
		}
		updated, err := scanToolProposal(tx.QueryRow(ctx, `
			UPDATE tool_proposals SET status = 'Executed' WHERE proposal_id = $1
			RETURNING `+toolProposalColumns, proposalID))
		if err != nil {
			return fmt.Errorf("execute approved tool call: %w", err)
		}
		out = updated
		return outbox.Insert(ctx, tx, outbox.Event{AggregateType: "tool_proposal", AggregateID: proposalID,
			EventType: "AI.ToolCallExecuted", TenantID: &tenantID, Payload: updated})
	})
	return out, err
}

func (s *PgStore) GetToolProposal(ctx context.Context, tenantID, proposalID string) (*domain.ToolProposal, error) {
	var out *domain.ToolProposal
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		p, err := scanToolProposal(tx.QueryRow(ctx, `SELECT `+toolProposalColumns+` FROM tool_proposals WHERE tenant_id = $1 AND proposal_id = $2`,
			tenantID, proposalID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrToolProposalNotFound
		}
		out = p
		return err
	})
	return out, err
}
