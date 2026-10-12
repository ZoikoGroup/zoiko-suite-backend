package ar05

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// Submission is the AR-05 aggregate: one logical e-invoice submission for a (document, profile).
type Submission struct {
	SubmissionID         string    `json:"submission_id"`
	TenantID             string    `json:"tenant_id"`
	LegalEntityID        string    `json:"legal_entity_id"`
	DocumentType         string    `json:"document_type"`
	DocumentID           string    `json:"document_id"`
	DocumentNumber       string    `json:"document_number"`
	ContentHash          string    `json:"content_hash"`
	JurisdictionCode     string    `json:"jurisdiction_code"`
	ProfileCode          string    `json:"profile_code"`
	ProfileVersion       string    `json:"profile_version"`
	Network              string    `json:"network"`
	Syntax               string    `json:"syntax"`
	Mode                 string    `json:"transmission_mode"`
	PackVersionID        string    `json:"pack_version_id,omitempty"`
	PackArtifactDigest   string    `json:"pack_artifact_digest,omitempty"`
	RuleID               string    `json:"rule_id,omitempty"`
	RuleContentDigest    string    `json:"rule_content_digest,omitempty"`
	ResolutionDecisionID string    `json:"resolution_decision_id,omitempty"`
	EndpointID           string    `json:"endpoint_id,omitempty"`
	ParticipantID        string    `json:"participant_id,omitempty"`
	Purpose              string    `json:"purpose,omitempty"`
	Provider             string    `json:"provider,omitempty"`
	State                State     `json:"state"`
	ValidationStatus     string    `json:"validation_status"`
	ArtifactHash         string    `json:"artifact_hash"`
	ArtifactRef          string    `json:"artifact_ref"`
	ParentSubmissionID   string    `json:"parent_submission_id,omitempty"`
	RegistrySubmissionID string    `json:"registry_submission_id,omitempty"`
	AttemptCount         int       `json:"attempt_count"`
	LastProviderRef      string    `json:"last_provider_ref,omitempty"`
	PreparedBy           string    `json:"prepared_by"`
	Version              int       `json:"version"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`

	profileSnapshot []byte
}

// Profile decodes the profile snapshot stored at prepare time (a later pack release never changes an in-flight submission).
func (s *Submission) Profile() (*Profile, error) {
	var p Profile
	if err := json.Unmarshal(s.profileSnapshot, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

const subCols = `submission_id::text, tenant_id::text, legal_entity_id::text, document_type, document_id::text, document_number, content_hash,
	jurisdiction_code, profile_code, profile_version, network, syntax, transmission_mode, pack_version_id, pack_artifact_digest, rule_id,
	rule_content_digest, resolution_decision_id, endpoint_id, participant_id, purpose, COALESCE(provider,''), state, validation_status,
	artifact_hash, artifact_ref, COALESCE(parent_submission_id::text,''), COALESCE(registry_submission_id,''), attempt_count,
	COALESCE(last_provider_ref,''), prepared_by, version, created_at, updated_at, profile_snapshot`

func scanSubmission(row pgx.Row) (*Submission, error) {
	var s Submission
	var state string
	if err := row.Scan(&s.SubmissionID, &s.TenantID, &s.LegalEntityID, &s.DocumentType, &s.DocumentID, &s.DocumentNumber, &s.ContentHash,
		&s.JurisdictionCode, &s.ProfileCode, &s.ProfileVersion, &s.Network, &s.Syntax, &s.Mode, &s.PackVersionID, &s.PackArtifactDigest, &s.RuleID,
		&s.RuleContentDigest, &s.ResolutionDecisionID, &s.EndpointID, &s.ParticipantID, &s.Purpose, &s.Provider, &state, &s.ValidationStatus,
		&s.ArtifactHash, &s.ArtifactRef, &s.ParentSubmissionID, &s.RegistrySubmissionID, &s.AttemptCount,
		&s.LastProviderRef, &s.PreparedBy, &s.Version, &s.CreatedAt, &s.UpdatedAt, &s.profileSnapshot); err != nil {
		return nil, err
	}
	s.State = State(state)
	return &s, nil
}

func getSubmission(ctx context.Context, tx pgx.Tx, tenantID, id string, forUpdate bool) (*Submission, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, kit.NotFound("submission not found")
	}
	q := `SELECT ` + subCols + ` FROM ar05_einvoice_submissions WHERE tenant_id = $1 AND submission_id = $2`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	s, err := scanSubmission(tx.QueryRow(ctx, q, tenantID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, kit.NotFound("submission not found")
	}
	return s, kit.MapPgErr(err)
}

// activeFor finds the unfinished (not REJECTED/CANCELLED) submission of a (document, profile), if any.
func activeFor(ctx context.Context, tx pgx.Tx, tenantID, docType, docID, profileCode string) (*Submission, error) {
	s, err := scanSubmission(tx.QueryRow(ctx, `SELECT `+subCols+` FROM ar05_einvoice_submissions
		WHERE tenant_id = $1 AND document_type = $2 AND document_id = $3 AND profile_code = $4 AND state NOT IN ('REJECTED','CANCELLED')`,
		tenantID, docType, docID, profileCode))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return s, kit.MapPgErr(err)
}

// latestRejected is the newest REJECTED submission of the (document, profile): the parent of a chained resubmission.
func latestRejected(ctx context.Context, tx pgx.Tx, tenantID, docType, docID, profileCode string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT submission_id::text FROM ar05_einvoice_submissions
		WHERE tenant_id = $1 AND document_type = $2 AND document_id = $3 AND profile_code = $4 AND state = 'REJECTED'
		ORDER BY created_at DESC LIMIT 1`, tenantID, docType, docID, profileCode).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, kit.MapPgErr(err)
}

func insertSubmission(ctx context.Context, tx pgx.Tx, s *Submission, snapshot []byte, artifact []byte, contentType string) error {
	_, err := tx.Exec(ctx, `INSERT INTO ar05_einvoice_submissions (submission_id, tenant_id, legal_entity_id, document_type, document_id, document_number,
			content_hash, jurisdiction_code, profile_code, profile_version, network, syntax, transmission_mode, profile_snapshot, pack_version_id,
			pack_artifact_digest, rule_id, rule_content_digest, resolution_decision_id, endpoint_id, participant_id, purpose, state, validation_status,
			artifact_hash, artifact_ref, parent_submission_id, prepared_by, version, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,'PREPARED','NOT_RUN',$23,$24,NULLIF($25,'')::uuid,$26,1,$27,$27)`,
		s.SubmissionID, s.TenantID, s.LegalEntityID, s.DocumentType, s.DocumentID, s.DocumentNumber, s.ContentHash, s.JurisdictionCode, s.ProfileCode,
		s.ProfileVersion, s.Network, s.Syntax, s.Mode, snapshot, s.PackVersionID, s.PackArtifactDigest, s.RuleID, s.RuleContentDigest,
		s.ResolutionDecisionID, s.EndpointID, s.ParticipantID, s.Purpose, s.ArtifactHash, s.ArtifactRef, s.ParentSubmissionID, s.PreparedBy, s.CreatedAt.UTC())
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO ar05_artifacts (artifact_id, tenant_id, legal_entity_id, submission_id, syntax, content_type, content, sha256, byte_len, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, uuid.NewString(), s.TenantID, s.LegalEntityID, s.SubmissionID, s.Syntax, contentType, artifact,
		s.ArtifactHash, len(artifact), s.CreatedAt.UTC())
	return err
}

func loadArtifact(ctx context.Context, tx pgx.Tx, tenantID, submissionID string) ([]byte, string, error) {
	var b []byte
	var ct string
	err := tx.QueryRow(ctx, `SELECT content, content_type FROM ar05_artifacts WHERE tenant_id = $1 AND submission_id = $2`, tenantID, submissionID).Scan(&b, &ct)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", kit.NotFound("artifact not found")
	}
	return b, ct, kit.MapPgErr(err)
}

// subUpdate is the set of mutable submission columns a command may change. Identity columns are frozen by trigger.
type subUpdate struct {
	State            State
	Provider         *string
	RegistryID       *string
	AttemptsDelta    int
	LastProviderRef  *string
	ValidationStatus *string
	StillRequired    bool // CANCELLED only: whether the document still needs an e-invoice (selects REQUIRED vs NOT_REQUIRED on the dimension)
	At               time.Time
}

// updateSubmission applies upd to cur under optimistic concurrency (the row is already FOR UPDATE-locked) and returns the
// new row. The version bumps exactly once per call.
func updateSubmission(ctx context.Context, tx pgx.Tx, cur *Submission, upd subUpdate) (*Submission, error) {
	out := *cur
	err := tx.QueryRow(ctx, `UPDATE ar05_einvoice_submissions SET state = $3, provider = COALESCE($4, provider), registry_submission_id = COALESCE($5, registry_submission_id),
			attempt_count = attempt_count + $6, last_provider_ref = COALESCE($7, last_provider_ref), validation_status = COALESCE($8, validation_status),
			version = version + 1, updated_at = $9
		WHERE tenant_id = $1 AND submission_id = $2 AND version = $10 RETURNING version, updated_at, attempt_count`,
		cur.TenantID, cur.SubmissionID, string(upd.State), upd.Provider, upd.RegistryID, upd.AttemptsDelta, upd.LastProviderRef, upd.ValidationStatus,
		upd.At.UTC(), cur.Version).Scan(&out.Version, &out.UpdatedAt, &out.AttemptCount)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, kit.ErrStaleVersion
		}
		return nil, kit.MapPgErr(err)
	}
	out.State = upd.State
	if upd.Provider != nil {
		out.Provider = *upd.Provider
	}
	if upd.RegistryID != nil {
		out.RegistrySubmissionID = *upd.RegistryID
	}
	if upd.LastProviderRef != nil {
		out.LastProviderRef = *upd.LastProviderRef
	}
	if upd.ValidationStatus != nil {
		out.ValidationStatus = *upd.ValidationStatus
	}
	return &out, nil
}

// hist is one submission-history row (append-only evidence of a command).
type hist struct {
	At       time.Time
	Command  string
	Before   State
	Reason   string
	Evidence string
	Detail   map[string]any
}

func writeHistory(ctx context.Context, tx pgx.Tx, c kit.Caller, s *Submission, h hist) error {
	var detail []byte
	if h.Detail != nil {
		detail, _ = json.Marshal(h.Detail)
	}
	_, err := tx.Exec(ctx, `INSERT INTO ar05_submission_history (history_id, tenant_id, legal_entity_id, submission_id, submission_version, command, state_before,
			state_after, actor_id, reason_code, evidence_id, correlation_id, detail, recorded_at)
		VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,$9,NULLIF($10,''),NULLIF($11,''),$12,$13,$14)`,
		uuid.NewString(), s.TenantID, s.LegalEntityID, s.SubmissionID, s.Version, h.Command, string(h.Before), string(s.State), c.PrincipalID,
		h.Reason, h.Evidence, c.CorrelationID, detail, h.At.UTC())
	return kit.MapPgErr(err)
}

// ── attempts and their results ───────────────────────────────────────────────

// Attempt is one transmission try. Its outcome is the newest ar05_attempt_results row (OutInFlight when there is none).
type Attempt struct {
	ExternalAttemptID string    `json:"external_attempt_id"`
	SubmissionID      string    `json:"submission_id"`
	LegalEntityID     string    `json:"legal_entity_id"`
	AttemptNo         int       `json:"attempt_no"`
	Provider          string    `json:"provider"`
	ArtifactHash      string    `json:"artifact_hash"`
	RequestDigest     string    `json:"request_digest"`
	ParentAttemptID   string    `json:"parent_attempt_id,omitempty"`
	ReconciliationID  string    `json:"reconciliation_id,omitempty"`
	StartedBy         string    `json:"started_by"`
	StartedAt         time.Time `json:"started_at"`
	Outcome           string    `json:"outcome"`
	ProviderRef       string    `json:"provider_ref,omitempty"`
	Results           []Result  `json:"results,omitempty"`
}

// Result is one append-only outcome record of an attempt.
type Result struct {
	Seq            int            `json:"seq"`
	Outcome        string         `json:"outcome"`
	Source         string         `json:"source"`
	ProviderRef    string         `json:"provider_ref,omitempty"`
	ResponseDigest string         `json:"response_digest,omitempty"`
	Detail         map[string]any `json:"detail,omitempty"`
	RecordedBy     string         `json:"recorded_by"`
	RecordedAt     time.Time      `json:"recorded_at"`
}

const attemptCols = `a.external_attempt_id::text, a.submission_id::text, a.legal_entity_id::text, a.attempt_no, a.provider, a.artifact_hash, a.request_digest,
	COALESCE(a.parent_attempt_id::text,''), COALESCE(a.reconciliation_id::text,''), a.started_by, a.started_at,
	COALESCE((SELECT r.outcome FROM ar05_attempt_results r WHERE r.external_attempt_id = a.external_attempt_id ORDER BY r.seq DESC LIMIT 1), 'IN_FLIGHT'),
	COALESCE((SELECT r.provider_ref FROM ar05_attempt_results r WHERE r.external_attempt_id = a.external_attempt_id AND r.provider_ref IS NOT NULL ORDER BY r.seq DESC LIMIT 1), '')`

func scanAttempt(row pgx.Row) (*Attempt, error) {
	var a Attempt
	if err := row.Scan(&a.ExternalAttemptID, &a.SubmissionID, &a.LegalEntityID, &a.AttemptNo, &a.Provider, &a.ArtifactHash, &a.RequestDigest, &a.ParentAttemptID,
		&a.ReconciliationID, &a.StartedBy, &a.StartedAt, &a.Outcome, &a.ProviderRef); err != nil {
		return nil, err
	}
	return &a, nil
}

func getAttempt(ctx context.Context, tx pgx.Tx, tenantID, externalID string) (*Attempt, error) {
	if _, err := uuid.Parse(externalID); err != nil {
		return nil, kit.NotFound("attempt not found")
	}
	a, err := scanAttempt(tx.QueryRow(ctx, `SELECT `+attemptCols+` FROM ar05_submission_attempts a WHERE a.tenant_id = $1 AND a.external_attempt_id = $2`, tenantID, externalID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, kit.NotFound("attempt not found")
	}
	return a, kit.MapPgErr(err)
}

func latestAttempt(ctx context.Context, tx pgx.Tx, tenantID, submissionID string) (*Attempt, error) {
	a, err := scanAttempt(tx.QueryRow(ctx, `SELECT `+attemptCols+` FROM ar05_submission_attempts a WHERE a.tenant_id = $1 AND a.submission_id = $2
		ORDER BY a.attempt_no DESC LIMIT 1`, tenantID, submissionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return a, kit.MapPgErr(err)
}

func listAttempts(ctx context.Context, tx pgx.Tx, tenantID, submissionID string) ([]Attempt, error) {
	rows, err := tx.Query(ctx, `SELECT `+attemptCols+` FROM ar05_submission_attempts a WHERE a.tenant_id = $1 AND a.submission_id = $2 ORDER BY a.attempt_no`, tenantID, submissionID)
	if err != nil {
		return nil, kit.MapPgErr(err)
	}
	defer rows.Close()
	out := []Attempt{}
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

func listResults(ctx context.Context, tx pgx.Tx, tenantID, externalID string) ([]Result, error) {
	rows, err := tx.Query(ctx, `SELECT seq, outcome, source, COALESCE(provider_ref,''), COALESCE(response_digest,''), detail, recorded_by, recorded_at
		FROM ar05_attempt_results WHERE tenant_id = $1 AND external_attempt_id = $2 ORDER BY seq`, tenantID, externalID)
	if err != nil {
		return nil, kit.MapPgErr(err)
	}
	defer rows.Close()
	out := []Result{}
	for rows.Next() {
		var r Result
		var detail []byte
		if err := rows.Scan(&r.Seq, &r.Outcome, &r.Source, &r.ProviderRef, &r.ResponseDigest, &detail, &r.RecordedBy, &r.RecordedAt); err != nil {
			return nil, err
		}
		if len(detail) > 0 {
			_ = json.Unmarshal(detail, &r.Detail)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type attemptStart struct {
	ExternalID, Provider, ArtifactHash, RequestDigest, ParentID, ReconciliationID string
	At                                                                            time.Time
}

// insertAttempt records the attempt (and its tenant routing row) BEFORE the transport call, so the durable
// external_attempt_id exists whatever happens next.
func insertAttempt(ctx context.Context, tx pgx.Tx, c kit.Caller, s *Submission, a attemptStart) (*Attempt, error) {
	var n int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(attempt_no),0) + 1 FROM ar05_submission_attempts WHERE tenant_id = $1 AND submission_id = $2`,
		s.TenantID, s.SubmissionID).Scan(&n); err != nil {
		return nil, kit.MapPgErr(err)
	}
	if n > maxAttempts {
		return nil, kit.Conflict(kit.CodeInvalidTransition, "the maximum number of transmission attempts for this submission has been reached")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ar05_submission_attempts (external_attempt_id, tenant_id, legal_entity_id, submission_id, attempt_no, provider, artifact_hash,
			request_digest, parent_attempt_id, reconciliation_id, started_by, started_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,'')::uuid,NULLIF($10,'')::uuid,$11,$12)`,
		a.ExternalID, s.TenantID, s.LegalEntityID, s.SubmissionID, n, a.Provider, a.ArtifactHash, a.RequestDigest, a.ParentID, a.ReconciliationID, c.PrincipalID, a.At.UTC()); err != nil {
		return nil, kit.MapPgErr(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ar05_attempt_index (external_attempt_id, tenant_id, legal_entity_id, submission_id, provider) VALUES ($1,$2,$3,$4,$5)`,
		a.ExternalID, s.TenantID, s.LegalEntityID, s.SubmissionID, a.Provider); err != nil {
		return nil, kit.MapPgErr(err)
	}
	return &Attempt{ExternalAttemptID: a.ExternalID, SubmissionID: s.SubmissionID, LegalEntityID: s.LegalEntityID, AttemptNo: n, Provider: a.Provider, ArtifactHash: a.ArtifactHash,
		RequestDigest: a.RequestDigest, ParentAttemptID: a.ParentID, ReconciliationID: a.ReconciliationID, StartedBy: c.PrincipalID, StartedAt: a.At, Outcome: OutInFlight}, nil
}

type resultWrite struct {
	Outcome, Source, ProviderRef, ResponseDigest string
	Detail                                       map[string]any
	At                                           time.Time
}

func appendResult(ctx context.Context, tx pgx.Tx, c kit.Caller, s *Submission, externalID string, r resultWrite) error {
	var detail []byte
	if r.Detail != nil {
		detail, _ = json.Marshal(r.Detail)
	}
	_, err := tx.Exec(ctx, `INSERT INTO ar05_attempt_results (result_id, tenant_id, legal_entity_id, external_attempt_id, seq, outcome, source, provider_ref, response_digest, detail,
			recorded_by, recorded_at)
		VALUES ($1,$2,$3,$4,(SELECT COALESCE(MAX(seq),0)+1 FROM ar05_attempt_results WHERE external_attempt_id = $4),$5,$6,NULLIF($7,''),NULLIF($8,''),$9,$10,$11)`,
		uuid.NewString(), s.TenantID, s.LegalEntityID, externalID, r.Outcome, r.Source, r.ProviderRef, r.ResponseDigest, detail, c.PrincipalID, r.At.UTC())
	return kit.MapPgErr(err)
}

// ── validation, acknowledgements, reconciliations ────────────────────────────

// ValidationRecord is a stored validation run.
type ValidationRecord struct {
	RunNo        int          `json:"run_no"`
	Passed       bool         `json:"passed"`
	ErrorCount   int          `json:"error_count"`
	Errors       []Issue      `json:"errors"`
	ProfileCode  string       `json:"profile_code"`
	ProfileVer   string       `json:"profile_version"`
	Syntax       string       `json:"syntax"`
	Dependencies []Dependency `json:"dependencies"`
	ContentHash  string       `json:"content_hash"`
	ArtifactHash string       `json:"artifact_hash"`
	ValidatedBy  string       `json:"validated_by"`
	ValidatedAt  time.Time    `json:"validated_at"`
}

func insertValidation(ctx context.Context, tx pgx.Tx, c kit.Caller, s *Submission, rep ValidationReport, at time.Time) (int, error) {
	var n int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(run_no),0) + 1 FROM ar05_validation_results WHERE tenant_id = $1 AND submission_id = $2`, s.TenantID, s.SubmissionID).Scan(&n); err != nil {
		return 0, kit.MapPgErr(err)
	}
	errs, _ := json.Marshal(rep.Issues)
	deps, _ := json.Marshal(nonNilDeps(rep.Dependencies))
	_, err := tx.Exec(ctx, `INSERT INTO ar05_validation_results (result_id, tenant_id, legal_entity_id, submission_id, run_no, passed, error_count, errors, profile_code, profile_version,
			syntax, dependencies, content_hash, artifact_hash, validated_by, validated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		uuid.NewString(), s.TenantID, s.LegalEntityID, s.SubmissionID, n, rep.Passed, len(rep.Issues), errs, s.ProfileCode, s.ProfileVersion, s.Syntax, deps,
		s.ContentHash, s.ArtifactHash, c.PrincipalID, at.UTC())
	return n, kit.MapPgErr(err)
}

func nonNilDeps(d []Dependency) []Dependency {
	if d == nil {
		return []Dependency{}
	}
	return d
}

func latestValidation(ctx context.Context, tx pgx.Tx, tenantID, submissionID string) (*ValidationRecord, error) {
	var v ValidationRecord
	var errs, deps []byte
	err := tx.QueryRow(ctx, `SELECT run_no, passed, error_count, errors, profile_code, profile_version, syntax, dependencies, content_hash, artifact_hash, validated_by, validated_at
		FROM ar05_validation_results WHERE tenant_id = $1 AND submission_id = $2 ORDER BY run_no DESC LIMIT 1`, tenantID, submissionID).
		Scan(&v.RunNo, &v.Passed, &v.ErrorCount, &errs, &v.ProfileCode, &v.ProfileVer, &v.Syntax, &deps, &v.ContentHash, &v.ArtifactHash, &v.ValidatedBy, &v.ValidatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, kit.MapPgErr(err)
	}
	_ = json.Unmarshal(errs, &v.Errors)
	_ = json.Unmarshal(deps, &v.Dependencies)
	return &v, nil
}

// Ack is a stored provider acknowledgement / rejection, with its signature-verification evidence.
type Ack struct {
	AckID             string     `json:"ack_id"`
	SubmissionID      string     `json:"submission_id"`
	ExternalAttemptID string     `json:"external_attempt_id"`
	Provider          string     `json:"provider"`
	ProviderEventID   string     `json:"provider_event_id"`
	Status            string     `json:"status"`
	ReceiptID         string     `json:"receipt_id,omitempty"`
	ProviderReference string     `json:"provider_reference,omitempty"`
	ReasonCode        string     `json:"reason_code,omitempty"`
	Message           string     `json:"message,omitempty"`
	OccurredAt        time.Time  `json:"occurred_at"`
	ReceivedAt        time.Time  `json:"received_at"`
	Source            string     `json:"source"`
	SignatureVerified bool       `json:"signature_verified"`
	SignatureAlg      string     `json:"signature_alg,omitempty"`
	SignatureDigest   string     `json:"signature_digest,omitempty"`
	CallbackNonce     string     `json:"callback_nonce,omitempty"`
	CallbackTimestamp *time.Time `json:"callback_timestamp,omitempty"`
	BodyDigest        string     `json:"body_digest,omitempty"`
	Disposition       string     `json:"disposition"`
	StateAfter        string     `json:"state_after"`
	RecordedBy        string     `json:"recorded_by"`
}

const ackCols = `ack_id::text, submission_id::text, external_attempt_id::text, provider, provider_event_id, status, COALESCE(receipt_id,''), COALESCE(provider_reference,''),
	COALESCE(reason_code,''), COALESCE(message,''), occurred_at, received_at, source, signature_verified, COALESCE(signature_alg,''), COALESCE(signature_digest,''),
	COALESCE(callback_nonce,''), callback_timestamp, COALESCE(body_digest,''), disposition, state_after, recorded_by`

func scanAck(row pgx.Row) (*Ack, error) {
	var a Ack
	if err := row.Scan(&a.AckID, &a.SubmissionID, &a.ExternalAttemptID, &a.Provider, &a.ProviderEventID, &a.Status, &a.ReceiptID, &a.ProviderReference, &a.ReasonCode,
		&a.Message, &a.OccurredAt, &a.ReceivedAt, &a.Source, &a.SignatureVerified, &a.SignatureAlg, &a.SignatureDigest, &a.CallbackNonce, &a.CallbackTimestamp,
		&a.BodyDigest, &a.Disposition, &a.StateAfter, &a.RecordedBy); err != nil {
		return nil, err
	}
	return &a, nil
}

func findAck(ctx context.Context, tx pgx.Tx, tenantID, externalID, eventID string) (*Ack, error) {
	a, err := scanAck(tx.QueryRow(ctx, `SELECT `+ackCols+` FROM ar05_acknowledgements WHERE tenant_id = $1 AND external_attempt_id = $2 AND provider_event_id = $3`, tenantID, externalID, eventID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return a, kit.MapPgErr(err)
}

func insertAck(ctx context.Context, tx pgx.Tx, s *Submission, a *Ack) error {
	a.AckID = uuid.NewString()
	_, err := tx.Exec(ctx, `INSERT INTO ar05_acknowledgements (ack_id, tenant_id, legal_entity_id, submission_id, external_attempt_id, provider, provider_event_id, status,
			receipt_id, provider_reference, reason_code, message, occurred_at, received_at, source, signature_verified, signature_alg, signature_digest, callback_nonce,
			callback_timestamp, body_digest, disposition, state_after, recorded_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),NULLIF($10,''),NULLIF($11,''),NULLIF($12,''),$13,$14,$15,$16,NULLIF($17,''),NULLIF($18,''),NULLIF($19,''),$20,NULLIF($21,''),$22,$23,$24)`,
		a.AckID, s.TenantID, s.LegalEntityID, a.SubmissionID, a.ExternalAttemptID, a.Provider, a.ProviderEventID, a.Status, a.ReceiptID, a.ProviderReference, a.ReasonCode,
		a.Message, a.OccurredAt.UTC(), a.ReceivedAt.UTC(), a.Source, a.SignatureVerified, a.SignatureAlg, a.SignatureDigest, a.CallbackNonce, a.CallbackTimestamp, a.BodyDigest,
		a.Disposition, a.StateAfter, a.RecordedBy)
	return err
}

func latestAck(ctx context.Context, tx pgx.Tx, tenantID, submissionID string) (*Ack, error) {
	a, err := scanAck(tx.QueryRow(ctx, `SELECT `+ackCols+` FROM ar05_acknowledgements WHERE tenant_id = $1 AND submission_id = $2 ORDER BY received_at DESC, ack_id DESC LIMIT 1`, tenantID, submissionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return a, kit.MapPgErr(err)
}

func listAcks(ctx context.Context, tx pgx.Tx, tenantID, submissionID string) ([]Ack, error) {
	rows, err := tx.Query(ctx, `SELECT `+ackCols+` FROM ar05_acknowledgements WHERE tenant_id = $1 AND submission_id = $2 ORDER BY received_at, ack_id`, tenantID, submissionID)
	if err != nil {
		return nil, kit.MapPgErr(err)
	}
	defer rows.Close()
	out := []Ack{}
	for rows.Next() {
		a, err := scanAck(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// Reconciliation is one entry of the reconciliation history.
type Reconciliation struct {
	ReconciliationID  string         `json:"reconciliation_id"`
	SubmissionID      string         `json:"submission_id"`
	ExternalAttemptID string         `json:"external_attempt_id,omitempty"`
	Method            string         `json:"method"`
	Trigger           string         `json:"trigger"`
	ProviderState     string         `json:"provider_state"`
	StateBefore       string         `json:"state_before"`
	StateAfter        string         `json:"state_after"`
	ProviderRef       string         `json:"provider_ref,omitempty"`
	ReceiptID         string         `json:"receipt_id,omitempty"`
	ReasonCode        string         `json:"reason_code,omitempty"`
	EvidenceID        string         `json:"evidence_id,omitempty"`
	Detail            map[string]any `json:"detail,omitempty"`
	PerformedBy       string         `json:"performed_by"`
	PerformedAt       time.Time      `json:"performed_at"`
}

func insertReconciliation(ctx context.Context, tx pgx.Tx, s *Submission, r *Reconciliation) error {
	var detail []byte
	if r.Detail != nil {
		detail, _ = json.Marshal(r.Detail)
	}
	_, err := tx.Exec(ctx, `INSERT INTO ar05_reconciliations (reconciliation_id, tenant_id, legal_entity_id, submission_id, external_attempt_id, method, trigger, provider_state,
			state_before, state_after, provider_ref, receipt_id, reason_code, evidence_id, detail, performed_by, performed_at)
		VALUES ($1,$2,$3,$4,NULLIF($5,'')::uuid,$6,$7,$8,$9,$10,NULLIF($11,''),NULLIF($12,''),NULLIF($13,''),NULLIF($14,''),$15,$16,$17)`,
		r.ReconciliationID, s.TenantID, s.LegalEntityID, s.SubmissionID, r.ExternalAttemptID, r.Method, r.Trigger, r.ProviderState, r.StateBefore, r.StateAfter,
		r.ProviderRef, r.ReceiptID, r.ReasonCode, r.EvidenceID, detail, r.PerformedBy, r.PerformedAt.UTC())
	return kit.MapPgErr(err)
}

func listReconciliations(ctx context.Context, tx pgx.Tx, tenantID, submissionID string) ([]Reconciliation, error) {
	rows, err := tx.Query(ctx, `SELECT reconciliation_id::text, submission_id::text, COALESCE(external_attempt_id::text,''), method, trigger, provider_state, state_before, state_after,
			COALESCE(provider_ref,''), COALESCE(receipt_id,''), COALESCE(reason_code,''), COALESCE(evidence_id,''), detail, performed_by, performed_at
		FROM ar05_reconciliations WHERE tenant_id = $1 AND submission_id = $2 ORDER BY performed_at, reconciliation_id`, tenantID, submissionID)
	if err != nil {
		return nil, kit.MapPgErr(err)
	}
	defer rows.Close()
	out := []Reconciliation{}
	for rows.Next() {
		var r Reconciliation
		var detail []byte
		if err := rows.Scan(&r.ReconciliationID, &r.SubmissionID, &r.ExternalAttemptID, &r.Method, &r.Trigger, &r.ProviderState, &r.StateBefore, &r.StateAfter,
			&r.ProviderRef, &r.ReceiptID, &r.ReasonCode, &r.EvidenceID, &detail, &r.PerformedBy, &r.PerformedAt); err != nil {
			return nil, err
		}
		if len(detail) > 0 {
			_ = json.Unmarshal(detail, &r.Detail)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// HistoryEntry is one submission-history row.
type HistoryEntry struct {
	Version     int            `json:"submission_version"`
	Command     string         `json:"command"`
	StateBefore string         `json:"state_before,omitempty"`
	StateAfter  string         `json:"state_after"`
	ActorID     string         `json:"actor_id"`
	ReasonCode  string         `json:"reason_code,omitempty"`
	EvidenceID  string         `json:"evidence_id,omitempty"`
	Detail      map[string]any `json:"detail,omitempty"`
	RecordedAt  time.Time      `json:"recorded_at"`
}

func listHistory(ctx context.Context, tx pgx.Tx, tenantID, submissionID string) ([]HistoryEntry, error) {
	rows, err := tx.Query(ctx, `SELECT submission_version, command, COALESCE(state_before,''), state_after, actor_id, COALESCE(reason_code,''), COALESCE(evidence_id,''), detail, recorded_at
		FROM ar05_submission_history WHERE tenant_id = $1 AND submission_id = $2 ORDER BY submission_version`, tenantID, submissionID)
	if err != nil {
		return nil, kit.MapPgErr(err)
	}
	defer rows.Close()
	out := []HistoryEntry{}
	for rows.Next() {
		var h HistoryEntry
		var detail []byte
		if err := rows.Scan(&h.Version, &h.Command, &h.StateBefore, &h.StateAfter, &h.ActorID, &h.ReasonCode, &h.EvidenceID, &detail, &h.RecordedAt); err != nil {
			return nil, err
		}
		if len(detail) > 0 {
			_ = json.Unmarshal(detail, &h.Detail)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// attemptRoute is the tenant routing of a callback (read before any tenant is known).
type attemptRoute struct {
	TenantID, LegalEntityID, SubmissionID, Provider string
}

func routeAttempt(ctx context.Context, db *kit.DB, externalID string) (*attemptRoute, error) {
	if _, err := uuid.Parse(externalID); err != nil {
		return nil, nil
	}
	var r attemptRoute
	err := db.Pool.QueryRow(ctx, `SELECT tenant_id::text, legal_entity_id::text, submission_id::text, provider FROM ar05_attempt_index WHERE external_attempt_id = $1`, externalID).
		Scan(&r.TenantID, &r.LegalEntityID, &r.SubmissionID, &r.Provider)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}
