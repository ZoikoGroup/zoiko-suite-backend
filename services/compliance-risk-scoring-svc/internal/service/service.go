// Package service holds the business workflows behind each route. Handlers
// parse HTTP and call exactly one method here; this layer validates, checks
// authorization, runs the transaction (state change + idempotency record +
// outbox event together) and returns either a result or a *domain.Error.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"zoiko.io/compliance-risk-scoring-svc/internal/authz"
	"zoiko.io/compliance-risk-scoring-svc/internal/domain"
	"zoiko.io/compliance-risk-scoring-svc/internal/events"
	"zoiko.io/compliance-risk-scoring-svc/internal/store"
)

// Authorization actions sent to authorization-svc. Reads are not
// authorized per call (tenant isolation + RLS scope them), matching the
// platform's other read paths.
const (
	ActionRiskScoreCreate          = "RISK_SCORE_CREATE"
	ActionRiskScoreReview          = "RISK_SCORE_REVIEW"
	ActionRiskAssessmentArchive    = "RISK_ASSESSMENT_ARCHIVE"
	ActionRiskThresholdRuleCreate  = "RISK_THRESHOLD_RULE_CREATE"
	ActionRiskThresholdRuleUpdate  = "RISK_THRESHOLD_RULE_UPDATE"
	ActionRiskThresholdRuleApprove = "RISK_THRESHOLD_RULE_APPROVE"
	ActionRiskThresholdRuleRetire  = "RISK_THRESHOLD_RULE_RETIRE"
)

// Authorizer is the authorization-svc client (fail closed).
type Authorizer interface {
	CheckAllowed(ctx context.Context, tenantID, principalID, legalEntityID, actionType string) error
}

// Service implements every route's workflow.
type Service struct {
	store  *store.Store
	authz  Authorizer
	logger *zap.Logger
	now    func() time.Time
}

// New builds a Service.
func New(s *store.Store, a Authorizer, logger *zap.Logger) *Service {
	return &Service{store: s, authz: a, logger: logger, now: time.Now}
}

// WithClock replaces the clock (tests).
func (s *Service) WithClock(now func() time.Time) *Service {
	s.now = now
	return s
}

// currentTime is "now" in UTC at the database's microsecond precision, so a
// value returned in a response is identical to the value read back later.
func (s *Service) currentTime() time.Time {
	return s.now().UTC().Truncate(time.Microsecond)
}

// CommandContext is everything about the caller of a write, taken from the
// request envelope headers by the handler.
type CommandContext struct {
	TenantID         string
	PrincipalID      string
	LegalEntityScope string // X-Legal-Entity-Id; may be empty
	RequestID        string
	CorrelationID    string
	IdempotencyKey   string
	RequestHash      string // sha256 over principal, method, path and body
	IfMatchVersion   *int   // parsed If-Match; nil when the header is absent or "*"
	IfMatchPresent   bool
}

// CommandResult is the response of a write: either freshly produced or
// replayed from the idempotency record.
type CommandResult struct {
	StatusCode int
	Body       []byte
	ETag       string
	Replayed   bool
}

// QueryContext is the caller of a read.
type QueryContext struct {
	TenantID string
}

// ETagForVersion renders a row version as a strong ETag.
func ETagForVersion(version int) string {
	return `"` + strconv.Itoa(version) + `"`
}

// workResult is what a write's transactional body produces.
type workResult struct {
	statusCode int
	response   any
	version    int // row version for the ETag; 0 = no ETag
}

// executeIdempotently runs work in one tenant transaction guarded by the
// Idempotency-Key:
//
//  1. If a record exists for the key and it was produced by the same
//     operation and the same request (hash), its stored response is replayed
//     and work does not run.
//  2. If the key was used for a different operation or request body -> 422
//     idempotency_key_reused.
//  3. Otherwise work runs; its response is stored with the key in the same
//     transaction, so "state changed" and "key recorded" commit together.
//
// Failed work stores nothing: a retry after a 4xx re-executes.
func (s *Service) executeIdempotently(ctx context.Context, cmd CommandContext, operation string,
	work func(q *store.TenantQueries, now time.Time) (*workResult, error)) (*CommandResult, error) {

	if cmd.IdempotencyKey == "" {
		return nil, domain.BadRequest("idempotency_key_required", "Idempotency-Key header is required for this write")
	}

	var result *CommandResult
	err := s.store.InTenantTx(ctx, cmd.TenantID, func(q *store.TenantQueries) error {
		existing, findErr := q.FindIdempotencyRecord(ctx, cmd.IdempotencyKey)
		if findErr == nil {
			if existing.Operation != operation || existing.RequestHash != cmd.RequestHash {
				return domain.Unprocessable("idempotency_key_reused",
					"Idempotency-Key was already used for a different request; use a new key")
			}
			result = &CommandResult{
				StatusCode: existing.ResponseStatus,
				Body:       existing.ResponseBody,
				ETag:       existing.ResponseETag,
				Replayed:   true,
			}
			return nil
		}
		if !errors.Is(findErr, store.ErrNotFound) {
			return findErr
		}

		now := s.currentTime()
		produced, workErr := work(q, now)
		if workErr != nil {
			return workErr
		}

		body, marshalErr := json.Marshal(produced.response)
		if marshalErr != nil {
			return marshalErr
		}
		etag := ""
		if produced.version > 0 {
			etag = ETagForVersion(produced.version)
		}

		saveErr := q.SaveIdempotencyRecord(ctx, store.IdempotencyRecord{
			Key:            cmd.IdempotencyKey,
			Operation:      operation,
			RequestHash:    cmd.RequestHash,
			ResponseStatus: produced.statusCode,
			ResponseBody:   body,
			ResponseETag:   etag,
			CreatedAt:      now,
		})
		if saveErr != nil {
			return saveErr
		}

		result = &CommandResult{StatusCode: produced.statusCode, Body: body, ETag: etag}
		return nil
	})
	if err != nil {
		return nil, translateStoreError(err)
	}
	return result, nil
}

// translateStoreError maps store-level errors to domain errors.
func translateStoreError(err error) error {
	if _, isDomain := domain.AsError(err); isDomain {
		return err
	}
	if errors.Is(err, store.ErrNotFound) {
		return domain.NotFound("resource not found")
	}
	var unique *store.ErrUniqueViolation
	if errors.As(err, &unique) {
		switch unique.Constraint {
		case "risk_idempotency_records_pkey":
			return domain.Conflict("idempotency_in_progress",
				"a request with this Idempotency-Key is being processed concurrently; retry shortly")
		case "uq_risk_score_one_active_per_subject":
			return domain.Conflict("concurrent_scoring",
				"another score for this subject was committed concurrently; retry")
		case "uq_risk_threshold_one_active_per_category":
			return domain.Conflict("concurrent_approval",
				"another threshold rule for this category was approved concurrently; retry")
		}
		return domain.Conflict("conflict", "the request conflicts with existing data")
	}
	return err
}

// authorize asks authorization-svc and maps the outcome. Fail closed: an
// unreachable authorization-svc is 503, never an implicit allow.
func (s *Service) authorize(ctx context.Context, cmd CommandContext, legalEntityID, action string) error {
	err := s.authz.CheckAllowed(ctx, cmd.TenantID, cmd.PrincipalID, legalEntityID, action)
	if err == nil {
		return nil
	}
	if errors.Is(err, authz.ErrAuthorizationDenied) {
		return domain.Forbidden("authorization_denied", "principal is not authorized to perform "+action)
	}
	s.logger.Error("authorization check failed closed", zap.String("action", action), zap.Error(err))
	return domain.Unavailable("authorization_unavailable", "authorization service unavailable; request refused (fail closed)")
}

// checkBodyTenant: the tenant comes only from X-Tenant-Id. A body may
// repeat it, but must then agree.
func checkBodyTenant(bodyTenantID string, cmd CommandContext) error {
	if bodyTenantID != "" && bodyTenantID != cmd.TenantID {
		return domain.Forbidden("tenant_mismatch", "tenant_id in the body does not match the X-Tenant-Id header")
	}
	return nil
}

// checkIfMatch enforces optimistic concurrency when the caller sent If-Match.
func checkIfMatch(cmd CommandContext, currentVersion int) error {
	if cmd.IfMatchPresent && cmd.IfMatchVersion != nil && *cmd.IfMatchVersion != currentVersion {
		return domain.PreconditionFailed("If-Match does not match the current version " + ETagForVersion(currentVersion))
	}
	return nil
}

// enqueueEvent writes one event into the transactional outbox.
func enqueueEvent(ctx context.Context, q *store.TenantQueries, cmd CommandContext, now time.Time,
	eventType, aggregateType, aggregateID, legalEntityID string, payload any) error {

	eventID := uuid.New().String()
	event := events.BuildEvent(events.PublishParams{
		EventType:     eventType,
		SubjectID:     aggregateID,
		TenantID:      cmd.TenantID,
		LegalEntityID: legalEntityID,
		ActorID:       cmd.PrincipalID,
		CorrelationID: cmd.CorrelationID,
		RequestID:     cmd.RequestID,
		Payload:       payload,
	}, "evt-"+eventID, now)

	envelope, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return q.EnqueueEvent(ctx, store.OutboxEvent{
		ID:            eventID,
		EventType:     eventType,
		AggregateType: aggregateType,
		AggregateID:   aggregateID,
		Envelope:      envelope,
		CreatedAt:     now,
	})
}

// todayUTC is the calendar date used for threshold effective dating.
func todayUTC(now time.Time) string {
	return now.UTC().Format(domain.DateLayout)
}
