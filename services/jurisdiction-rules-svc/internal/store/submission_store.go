package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// ZS-JUR-001 Wave 5 persistence: regulatory submissions (e-invoice and filing)
// and their append-only authority/provider status reports.

// SubmissionRecord is one registered submission.
type SubmissionRecord struct {
	SubmissionID        string          `json:"submission_id"`
	Kind                string          `json:"submission_kind"`
	JurisdictionCode    string          `json:"jurisdiction_code"`
	ProfileCode         string          `json:"profile_code"`
	SubjectRef          string          `json:"subject_ref"`
	PayloadHash         string          `json:"payload_hash"`
	Status              string          `json:"status"`
	IsTerminal          bool            `json:"is_terminal"`
	LastEventOccurredAt *time.Time      `json:"last_event_occurred_at"`
	EffectiveAt         time.Time       `json:"effective_at"`
	PackVersionID       string          `json:"pack_version_id"`
	ArtifactDigest      string          `json:"artifact_digest"`
	RuleID              string          `json:"rule_id"`
	RuleContentDigest   string          `json:"rule_content_digest"`
	ProfileSnapshot     json.RawMessage `json:"profile_snapshot"`
	DependenciesUsed    json.RawMessage `json:"dependencies_used"`
	Approvals           json.RawMessage `json:"approvals"`
	SubmittedBy         string          `json:"submitted_by"`
	RequestDigest       string          `json:"request_digest"`
	CreatedAt           time.Time       `json:"created_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
}

// SubmissionEventRecord is one stored status report.
type SubmissionEventRecord struct {
	ProviderEventID string          `json:"provider_event_id"`
	ReportedStatus  string          `json:"reported_status"`
	ReceiptID       *string         `json:"receipt_id"`
	OccurredAt      time.Time       `json:"occurred_at"`
	ReceivedAt      time.Time       `json:"received_at"`
	Disposition     string          `json:"disposition"`
	StatusAfter     string          `json:"status_after"`
	Detail          json.RawMessage `json:"detail"`
	RecordedBy      string          `json:"recorded_by"`
}

// CreateSubmissionParams registers a submission under a resolved profile.
type CreateSubmissionParams struct {
	Kind, JurisdictionCode, ProfileCode, SubjectRef, PayloadHash string
	InitialStatus                                                string
	EffectiveAt                                                  time.Time
	PackVersionID, ArtifactDigest, RuleID, RuleContentDigest     string
	ProfileSnapshot, DependenciesUsed, Approvals                 json.RawMessage
	SubmittedBy, RequestDigest                                   string
	IdempotencyKey                                               *string
}

const submissionCols = `submission_id::text, submission_kind, jurisdiction_code, profile_code, subject_ref, payload_hash, status, is_terminal,
	last_event_occurred_at, effective_at, pack_version_id::text, artifact_digest, rule_id::text, rule_content_digest,
	profile_snapshot::text, dependencies_used::text, approvals::text, submitted_by, request_digest, created_at, updated_at`

func scanSubmission(r pgx.Row) (*SubmissionRecord, error) {
	var s SubmissionRecord
	var snap, deps, appr string
	if err := r.Scan(&s.SubmissionID, &s.Kind, &s.JurisdictionCode, &s.ProfileCode, &s.SubjectRef, &s.PayloadHash, &s.Status, &s.IsTerminal,
		&s.LastEventOccurredAt, &s.EffectiveAt, &s.PackVersionID, &s.ArtifactDigest, &s.RuleID, &s.RuleContentDigest, &snap, &deps, &appr,
		&s.SubmittedBy, &s.RequestDigest, &s.CreatedAt, &s.UpdatedAt); err != nil {
		return nil, err
	}
	s.ProfileSnapshot, s.DependenciesUsed, s.Approvals = json.RawMessage(snap), json.RawMessage(deps), json.RawMessage(appr)
	return &s, nil
}

// CreateSubmission registers a submission. With an idempotency key a repeat from
// the same caller with the same request returns the stored submission
// (replayed=true); the same key with a different request is ErrIdempotencyConflict.
func (s *PgStore) CreateSubmission(ctx context.Context, p CreateSubmissionParams) (*SubmissionRecord, bool, error) {
	rec, err := scanSubmission(s.pool.QueryRow(ctx, `INSERT INTO regulatory_submissions
		(submission_kind, jurisdiction_code, profile_code, subject_ref, payload_hash, status, effective_at, pack_version_id, artifact_digest,
		 rule_id, rule_content_digest, profile_snapshot, dependencies_used, approvals, submitted_by, idempotency_key, request_digest)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8::uuid,$9,$10::uuid,$11,$12::jsonb,$13::jsonb,$14::jsonb,$15,$16,$17)
		ON CONFLICT (submitted_by, idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
		RETURNING `+submissionCols,
		p.Kind, p.JurisdictionCode, p.ProfileCode, p.SubjectRef, p.PayloadHash, p.InitialStatus, p.EffectiveAt, p.PackVersionID, p.ArtifactDigest,
		p.RuleID, p.RuleContentDigest, string(p.ProfileSnapshot), string(p.DependenciesUsed), string(p.Approvals), p.SubmittedBy, p.IdempotencyKey, p.RequestDigest))
	if err == nil {
		return rec, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) || p.IdempotencyKey == nil {
		return nil, false, s.registryFail("CreateSubmission", err, nil)
	}
	existing, err := scanSubmission(s.pool.QueryRow(ctx, `SELECT `+submissionCols+` FROM regulatory_submissions
		WHERE submitted_by=$1 AND idempotency_key=$2`, p.SubmittedBy, *p.IdempotencyKey))
	if err != nil {
		return nil, false, s.registryFail("CreateSubmission lookup", err, nil)
	}
	if existing.RequestDigest != p.RequestDigest {
		return nil, false, domain.ErrIdempotencyConflict
	}
	return existing, true, nil
}

const eventSelect = `SELECT provider_event_id, reported_status, receipt_id, occurred_at, received_at, disposition, status_after, detail::text, recorded_by
	FROM regulatory_submission_events`

func scanEvent(r pgx.Row) (*SubmissionEventRecord, error) {
	var e SubmissionEventRecord
	var detail string
	if err := r.Scan(&e.ProviderEventID, &e.ReportedStatus, &e.ReceiptID, &e.OccurredAt, &e.ReceivedAt, &e.Disposition, &e.StatusAfter, &detail, &e.RecordedBy); err != nil {
		return nil, err
	}
	e.Detail = json.RawMessage(detail)
	return &e, nil
}

func (s *PgStore) listSubmissionEvents(ctx context.Context, id string) ([]*SubmissionEventRecord, error) {
	rows, err := s.pool.Query(ctx, eventSelect+` WHERE submission_id=$1::uuid ORDER BY received_at, event_row_id`, id)
	if err != nil {
		return nil, s.registryFail("listSubmissionEvents", err, nil)
	}
	defer rows.Close()
	out := []*SubmissionEventRecord{}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, s.registryFail("listSubmissionEvents scan", err, nil)
		}
		out = append(out, e)
	}
	return out, s.registryFail("listSubmissionEvents rows", rows.Err(), nil)
}

// GetSubmission reads a submission with its status reports in arrival order.
func (s *PgStore) GetSubmission(ctx context.Context, id string) (*SubmissionRecord, []*SubmissionEventRecord, error) {
	rec, err := scanSubmission(s.pool.QueryRow(ctx, `SELECT `+submissionCols+` FROM regulatory_submissions WHERE submission_id::text=$1`, id))
	if err != nil {
		return nil, nil, s.registryFail("GetSubmission", err, domain.ErrSubmissionNotFound)
	}
	events, err := s.listSubmissionEvents(ctx, id)
	return rec, events, err
}

// SubmissionEventParams is one status report from an authority or provider.
type SubmissionEventParams struct {
	ProviderEventID string
	Status          string
	ReceiptID       string
	OccurredAt      time.Time
	Detail          json.RawMessage
	RecordedBy      string
}

// SubmissionEventResult is the outcome of recording a report.
type SubmissionEventResult struct {
	Submission *SubmissionRecord
	Event      *SubmissionEventRecord
	// Replayed is true when this provider event id was already recorded: nothing changed (JUR-NEG-10).
	Replayed bool
}

// RecordSubmissionEvent stores a status report and, when the profile's lifecycle
// allows it, advances the submission. It runs under a row lock so concurrent or
// duplicate callbacks resolve to exactly one authoritative outcome. A duplicate
// provider event id returns the stored result unchanged. Reports that are stale,
// late after a final outcome, illegal, or missing a required receipt are stored
// with their disposition and never change the status (JUR-NEG-09, 10, 11).
func (s *PgStore) RecordSubmissionEvent(ctx context.Context, id string, p SubmissionEventParams) (*SubmissionEventResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, s.registryFail("RecordSubmissionEvent begin", err, nil)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	sub, err := scanSubmission(tx.QueryRow(ctx, `SELECT `+submissionCols+` FROM regulatory_submissions WHERE submission_id::text=$1 FOR UPDATE`, id))
	if err != nil {
		return nil, s.registryFail("RecordSubmissionEvent lock", err, domain.ErrSubmissionNotFound)
	}
	existing, err := scanEvent(tx.QueryRow(ctx, eventSelect+` WHERE submission_id=$1::uuid AND provider_event_id=$2`, id, p.ProviderEventID))
	if err == nil {
		return &SubmissionEventResult{Submission: sub, Event: existing, Replayed: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, s.registryFail("RecordSubmissionEvent lookup", err, nil)
	}

	var snap domain.SubmissionProfile
	if err := json.Unmarshal(sub.ProfileSnapshot, &snap); err != nil {
		return nil, s.registryFail("RecordSubmissionEvent snapshot", err, nil)
	}
	disp := snap.EvaluateEvent(sub.Status, sub.LastEventOccurredAt, domain.SubmissionEvent{ProviderEventID: p.ProviderEventID, Status: p.Status, ReceiptID: p.ReceiptID, OccurredAt: p.OccurredAt})
	statusAfter := sub.Status
	if disp == domain.DispApplied {
		statusAfter = p.Status
	}
	detail := p.Detail
	if len(detail) == 0 {
		detail = json.RawMessage(`{}`)
	}
	ev, err := scanEvent(tx.QueryRow(ctx, `INSERT INTO regulatory_submission_events
		(submission_id, provider_event_id, reported_status, receipt_id, occurred_at, disposition, status_after, detail, recorded_by)
		VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8::jsonb,$9)
		RETURNING provider_event_id, reported_status, receipt_id, occurred_at, received_at, disposition, status_after, detail::text, recorded_by`,
		id, p.ProviderEventID, p.Status, nullIfEmpty(p.ReceiptID), p.OccurredAt, disp, statusAfter, string(detail), p.RecordedBy))
	if err != nil {
		return nil, s.registryFail("RecordSubmissionEvent insert", err, nil)
	}
	if disp == domain.DispApplied {
		sub, err = scanSubmission(tx.QueryRow(ctx, `UPDATE regulatory_submissions SET status=$2, is_terminal=$3, last_event_occurred_at=$4, updated_at=NOW()
			WHERE submission_id::text=$1 RETURNING `+submissionCols, id, p.Status, snap.IsTerminal(p.Status), p.OccurredAt))
		if err != nil {
			return nil, s.registryFail("RecordSubmissionEvent update", err, nil)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, s.registryFail("RecordSubmissionEvent commit", err, nil)
	}
	return &SubmissionEventResult{Submission: sub, Event: ev}, nil
}
