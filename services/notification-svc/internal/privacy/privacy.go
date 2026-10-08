// Package privacy asks the platform's privacy decision service (PRV,
// privacy-decision-svc) whether a communication may use a recipient's personal data,
// and turns the answer into a delivery verdict (ZS-SVC-Y-001 NCD-02 section 5.3,
// INV-09, INV-30, NP-17, NP-18).
//
// The doctrine is the standard's own: privacy permission is a state dimension separate
// from the user's preferences and from the sender's wish. "Must be PERMIT/RESTRICT with
// satisfied restrictions before personal-data use", "PRV permission INDETERMINATE: fail
// closed", and "no operational fail-open mode may silently convert BLOCK or
// INDETERMINATE into permission". Everything in this package therefore errs towards
// NOT sending: an unreachable service, a timeout, an unreadable answer, an answer this
// service does not recognise, a restriction it cannot enforce, and an intent that names
// no privacy binding are all refusals.
package privacy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
)

// Results the decision service returns, plus two this service adds for the cases in which
// no decision exists.
const (
	ResultPermit         = "PERMIT"
	ResultRestrict       = "RESTRICT"
	ResultBlock          = "BLOCK"
	ResultReviewRequired = "REVIEW_REQUIRED"
	ResultIndeterminate  = "INDETERMINATE"

	// ResultUnavailable: no decision could be obtained (outage, timeout, unreadable answer).
	ResultUnavailable = "UNAVAILABLE"
	// ResultNotBound: the intent version names no privacy activity and purpose, so there is
	// nothing to ask. Refused: a person-directed message is never sent on an assumption.
	ResultNotBound = "NOT_BOUND"
)

// ErrUnavailable means no usable decision was obtained.
var ErrUnavailable = errors.New("privacy decision service unavailable")

// Request is one question to the decision service.
type Request struct {
	TenantID      string
	PrincipalID   string // the principal on whose behalf the message is sent
	CorrelationID string
	SubjectRef    string // the recipient
	ActivityID    string
	PurposeID     string
}

// Constraint is a restriction attached to a RESTRICT answer.
type Constraint struct {
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
}

// Decision is the part of the answer this service uses.
type Decision struct {
	DecisionID  string       `json:"decision_id"`
	Result      string       `json:"result"`
	ReasonCodes []string     `json:"reason_codes"`
	Constraints []Constraint `json:"constraints"`
}

// Client calls privacy-decision-svc.
type Client struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

// NewClient returns a client with a bounded timeout: a send must not hang on a slow
// authority, and a timeout is a refusal (retryable), never a permission.
func NewClient(baseURL string, timeout time.Duration, log *zap.Logger) *Client {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: timeout}, log: log}
}

type wireRequest struct {
	SubjectRef           string         `json:"subject_ref"`
	ProcessingActivityID string         `json:"processing_activity_id"`
	PurposeID            string         `json:"purpose_id"`
	ProposedOperation    string         `json:"proposed_operation"`
	SubjectContext       map[string]any `json:"subject_context,omitempty"`
	DataContext          map[string]any `json:"data_context,omitempty"`
}

var knownResults = map[string]bool{
	ResultPermit: true, ResultRestrict: true, ResultBlock: true, ResultReviewRequired: true, ResultIndeterminate: true,
}

// Decide asks whether the recipient's contact details may be USED for the purpose.
// Any failure to obtain a recognisable decision is ErrUnavailable.
func (c *Client) Decide(ctx context.Context, r Request) (*Decision, error) {
	if r.TenantID == "" || r.PrincipalID == "" || r.SubjectRef == "" || r.ActivityID == "" || r.PurposeID == "" {
		return nil, fmt.Errorf("%w: incomplete request", ErrUnavailable)
	}
	body, err := json.Marshal(wireRequest{
		SubjectRef: r.SubjectRef, ProcessingActivityID: r.ActivityID, PurposeID: r.PurposeID, ProposedOperation: "USE",
		SubjectContext: map[string]any{"subject_ref": r.SubjectRef},
		DataContext:    map[string]any{"data_categories": []string{"CONTACT_DETAILS"}},
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/privacy/decisions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Tenant-Id", r.TenantID)
	req.Header.Set("X-Principal-Id", r.PrincipalID)
	if r.CorrelationID != "" {
		req.Header.Set("X-Correlation-ID", r.CorrelationID)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Warn("privacy-decision-svc unreachable", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		// 401, 403 and 5xx alike: this service has no decision, and says so.
		c.log.Warn("privacy-decision-svc returned an unexpected status", zap.Int("status", resp.StatusCode))
		return nil, fmt.Errorf("%w: unexpected status %d", ErrUnavailable, resp.StatusCode)
	}
	var d Decision
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256<<10)).Decode(&d); err != nil {
		return nil, fmt.Errorf("%w: decoding the decision: %v", ErrUnavailable, err)
	}
	if !knownResults[d.Result] {
		// An answer this service does not understand is not permission.
		return nil, fmt.Errorf("%w: unrecognised result %q", ErrUnavailable, d.Result)
	}
	if d.DecisionID == "" {
		return nil, fmt.Errorf("%w: the decision carries no id, so it cannot be recorded as evidence", ErrUnavailable)
	}
	return &d, nil
}

// Verdict is what a delivery does with a decision.
type Verdict struct {
	Allow      bool
	Retryable  bool // when refused: could asking again later change the answer?
	Result     string
	DecisionID string
	Reason     string
}

const codeBlocked = "NCD-008 PRIVACY_PERMISSION_BLOCKED"

// Judge turns a decision (or the failure to get one) into a verdict.
//
//	PERMIT          send.
//	RESTRICT        refuse: a restriction is a duty (minimise, redact, limit the recipient),
//	                and this service cannot satisfy arbitrary duties. It does not pretend to
//	                by sending anyway. The refusal names the constraint types.
//	BLOCK           refuse, terminal.
//	REVIEW_REQUIRED refuse, terminal: a person decides, and a resend follows that decision.
//	INDETERMINATE   refuse (NP-17), retryable: the usual cause is a dependency outage.
//	no decision     refuse, retryable.
func Judge(d *Decision, err error) Verdict {
	if err != nil || d == nil {
		reason := "privacy decision unavailable"
		if err != nil {
			reason = err.Error()
		}
		return Verdict{Result: ResultUnavailable, Retryable: true, Reason: codeBlocked + ": " + reason + "; delivery withheld"}
	}
	v := Verdict{Result: d.Result, DecisionID: d.DecisionID}
	reasons := strings.Join(d.ReasonCodes, ", ")
	switch d.Result {
	case ResultPermit:
		v.Allow = true
	case ResultRestrict:
		types := make([]string, 0, len(d.Constraints))
		for _, c := range d.Constraints {
			types = append(types, c.Type)
		}
		v.Reason = fmt.Sprintf("%s: the privacy decision restricts this use (%s) and notification-svc cannot enforce restrictions; delivery withheld", codeBlocked, strings.Join(types, ", "))
	case ResultBlock:
		v.Reason = fmt.Sprintf("%s: the privacy decision blocks this use (%s)", codeBlocked, reasons)
	case ResultReviewRequired:
		v.Reason = fmt.Sprintf("%s: the privacy decision requires review before this use (%s)", codeBlocked, reasons)
	case ResultIndeterminate:
		v.Retryable = true
		v.Reason = fmt.Sprintf("%s: the privacy decision is indeterminate (%s); delivery withheld, never assumed permitted", codeBlocked, reasons)
	default:
		v.Retryable = true
		v.Reason = fmt.Sprintf("%s: unrecognised privacy result %q", codeBlocked, d.Result)
	}
	return v
}

// IntentVersions reads the intent version a notification is pinned to.
type IntentVersions interface {
	GetIntentVersion(ctx context.Context, versionID string) (*domain.IntentVersion, error)
}

// Decider is the part of Client the gate uses (so tests can substitute it).
type Decider interface {
	Decide(ctx context.Context, r Request) (*Decision, error)
}

// Gate asks the privacy question for a notification that was sent under an intent.
type Gate struct {
	decider  Decider
	versions IntentVersions
	log      *zap.Logger
}

// NewGate builds a gate. Both collaborators are required: a gate that cannot ask is the
// defect this type exists to remove.
func NewGate(decider Decider, versions IntentVersions, log *zap.Logger) (*Gate, error) {
	if decider == nil || versions == nil {
		return nil, errors.New("privacy gate needs a decider and an intent version reader")
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &Gate{decider: decider, versions: versions, log: log}, nil
}

// Outcome is the gate's answer for one delivery.
type Outcome struct {
	// Applies is false when the notification used no intent, so there is no privacy
	// binding to enforce (the legacy, ungoverned send).
	Applies bool
	Verdict
}

// Check decides whether the delivery may proceed. It is called at the last point before the
// provider, on every attempt, so a consent withdrawn since the message was created is
// honoured on the retry.
func (g *Gate) Check(ctx context.Context, n domain.Notification) Outcome {
	if n.IntentVersionID == "" {
		return Outcome{}
	}
	iv, err := g.versions.GetIntentVersion(svcmiddleware.WithTenant(ctx, n.TenantID), n.IntentVersionID)
	if err != nil {
		g.log.Error("privacy gate: the intent version could not be read; delivery withheld",
			zap.String("notification_id", n.NotificationID), zap.Error(err))
		return Outcome{Applies: true, Verdict: Verdict{Result: ResultUnavailable, Retryable: true,
			Reason: codeBlocked + ": the intent version this message was sent under could not be read; delivery withheld"}}
	}
	if iv.PrivacyActivityID == nil || iv.PrivacyPurposeID == nil {
		return Outcome{Applies: true, Verdict: Verdict{Result: ResultNotBound,
			Reason: codeBlocked + ": the communication intent names no privacy processing activity and purpose; delivery withheld"}}
	}
	d, derr := g.decider.Decide(ctx, Request{TenantID: n.TenantID, PrincipalID: n.CreatedByPrincipalID, CorrelationID: n.CorrelationID,
		SubjectRef: n.RecipientPrincipalID, ActivityID: *iv.PrivacyActivityID, PurposeID: *iv.PrivacyPurposeID})
	v := Judge(d, derr)
	if !v.Allow {
		g.log.Info("privacy gate refused delivery; the provider was not called",
			zap.String("notification_id", n.NotificationID), zap.String("result", v.Result), zap.String("decision_id", v.DecisionID))
	}
	return Outcome{Applies: true, Verdict: v}
}
