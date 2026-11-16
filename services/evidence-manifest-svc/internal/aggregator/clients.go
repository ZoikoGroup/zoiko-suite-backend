// Package aggregator holds read-only HTTP clients to the source services an
// evidence manifest pulls records from. Every client is fail-closed: an
// unreachable or erroring source service fails the manifest generation
// rather than silently producing an incomplete manifest that LOOKS complete.
//
// See internal/domain/types.go's package doc for the real discovery-surface
// constraints these clients work within (governance-decision-log-svc has a
// real list query; authorization-svc and workflow-svc are get-by-ID only).
package aggregator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"zoiko.io/evidence-manifest-svc/internal/domain"
	svcenvelope "zoiko.io/evidence-manifest-svc/internal/envelope"
	svcmiddleware "zoiko.io/evidence-manifest-svc/internal/middleware"
)

var ErrSourceUnavailable = errors.New("aggregator: source service unavailable")
var ErrSourceNotFound = errors.New("aggregator: source record not found")

// SourceRecord is a fetched source record paired with the raw JSON to snapshot.
type SourceRecord struct {
	SourceType     domain.SourceType
	SourceRecordID string
	RawJSON        []byte
}

// forwardTenant propagates the caller's verified tenant and principal identity
// to the downstream source services.
//
// Callee services (such as workflow-svc :8085 and workflow-history-svc :8097)
// strictly require both X-Tenant-Id and X-Principal-Id under the platform
// canonical input contract.
func forwardTenant(ctx context.Context, req *http.Request) {
	// Forward Tenant ID
	if tenantID := svcmiddleware.TenantFromContext(ctx); tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	} else if env, ok := svcenvelope.FromContext(ctx); ok && env.TenantID != "" {
		req.Header.Set("X-Tenant-Id", env.TenantID)
	}

	// Forward Principal ID
	if principalID := svcmiddleware.PrincipalFromContext(ctx); principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	} else if env, ok := svcenvelope.FromContext(ctx); ok && env.ActorSubjectID != "" {
		req.Header.Set("X-Principal-Id", env.ActorSubjectID)
	}

	// Forward Correlation ID
	if env, ok := svcenvelope.FromContext(ctx); ok && env.CorrelationID != "" {
		req.Header.Set("X-Correlation-ID", env.CorrelationID)
	}
}

// ── governance-decision-log-svc ──────────────────────────────────────────────

// GovernanceDecisionClient reads governance-decision-log-svc.
//
// D2-2.F07 (11 Nov 2026). The decision log runs IDENTITY_VERIFY=require and
// enforces read authorization, and answers its list as a page
// ({"records": [...], "next_cursor": ...}). This client decoded a bare array
// (so every list failed) and sent only the tenant and principal. It now:
//   - presents this service's workload certificate to the decision log's
//     :8450 listener (GOVERNANCE_DECISION_LOG_MTLS_URL), where the identity
//     gate admits it as a peer workload (IDENTITY_PEERS);
//   - vouches for the SIGNED-IN USER (X-Principal-Id forwarded, never this
//     service's own principal): a manifest snapshots whole decisions, so the
//     decision log authorizes the person the manifest is assembled for;
//   - sends the full input envelope with a read purpose (register D-04, D-06);
//   - follows next_cursor to the end of the range, failing closed rather
//     than snapshotting the first page as if it were the whole range.
type GovernanceDecisionClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

// GovernancePurpose is the purpose sent on every decision-log read.
const GovernancePurpose = "evidence_manifest_assembly"

// governancePageSize and governanceMaxPages bound one range read: a range
// larger than this fails the manifest instead of truncating it.
const (
	governancePageSize = 200
	governanceMaxPages = 100
)

func NewGovernanceDecisionClient(baseURL string, log *zap.Logger) *GovernanceDecisionClient {
	return NewGovernanceDecisionClientWithHTTPClient(baseURL, &http.Client{Timeout: 5 * time.Second}, log)
}

// NewGovernanceDecisionClientWithHTTPClient uses httpClient, the mTLS client
// presenting this service's workload certificate.
func NewGovernanceDecisionClientWithHTTPClient(baseURL string, httpClient *http.Client, log *zap.Logger) *GovernanceDecisionClient {
	return &GovernanceDecisionClient{baseURL: strings.TrimRight(baseURL, "/"), log: log, http: httpClient}
}

func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// governanceEnvelope adds what the decision log's input contract requires on
// a read, beyond the tenant, principal and correlation forwardTenant copies.
func governanceEnvelope(ctx context.Context, req *http.Request) {
	forwardTenant(ctx, req)
	h := req.Header
	env, hasEnv := svcenvelope.FromContext(ctx)
	if hasEnv && env.RequestID != "" {
		h.Set("X-Request-Id", env.RequestID)
	} else {
		h.Set("X-Request-Id", randomID())
	}
	if h.Get("X-Correlation-ID") == "" {
		h.Set("X-Correlation-ID", randomID())
	}
	if hasEnv && env.SourceChannel != "" {
		h.Set("X-Source-Channel", string(env.SourceChannel))
	} else {
		h.Set("X-Source-Channel", "system")
	}
	h.Set("X-Purpose-Context", GovernancePurpose)
}

// ListByEntityAndDateRange calls governance-decision-log-svc's REAL list
// endpoint (GET /v1/decisions?entity=...&from=...&to=...) — the only source
// in this service with genuine discovery — and follows every page.
func (c *GovernanceDecisionClient) ListByEntityAndDateRange(ctx context.Context, legalEntityID string, from, to *time.Time) ([]SourceRecord, error) {
	q := url.Values{}
	q.Set("entity", legalEntityID)
	if from != nil {
		q.Set("from", from.UTC().Format(time.RFC3339))
	}
	if to != nil {
		q.Set("to", to.UTC().Format(time.RFC3339))
	}
	q.Set("limit", strconv.Itoa(governancePageSize))

	out := []SourceRecord{}
	cursor := ""
	for page := 0; ; page++ {
		if page == governanceMaxPages {
			c.log.Error("governance decision range exceeds the page bound — failing closed rather than truncating",
				zap.Int("pages", page), zap.Int("page_size", governancePageSize))
			return nil, ErrSourceUnavailable
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/decisions?"+q.Encode(), nil)
		if err != nil {
			return nil, ErrSourceUnavailable
		}
		governanceEnvelope(ctx, req)
		resp, err := c.http.Do(req)
		if err != nil {
			c.log.Error("governance-decision-log-svc unreachable — failing closed", zap.Error(err))
			return nil, ErrSourceUnavailable
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			c.log.Error("governance-decision-log-svc unexpected status", zap.Int("status", resp.StatusCode))
			return nil, ErrSourceUnavailable
		}
		if readErr != nil {
			return nil, ErrSourceUnavailable
		}

		var pageBody struct {
			Records    *[]json.RawMessage `json:"records"`
			NextCursor string             `json:"next_cursor"`
		}
		if err := json.Unmarshal(body, &pageBody); err != nil || pageBody.Records == nil {
			return nil, fmt.Errorf("aggregator: decode governance decisions page: %w", errOrMissing(err))
		}
		// Each element is kept as its own JSON so each ManifestRecord
		// snapshot is exactly one decision, not the whole page.
		for _, raw := range *pageBody.Records {
			var d struct {
				DecisionID string `json:"decision_id"`
			}
			if err := json.Unmarshal(raw, &d); err != nil || d.DecisionID == "" {
				return nil, fmt.Errorf("aggregator: decode governance decision: %w", errOrMissing(err))
			}
			out = append(out, SourceRecord{SourceType: domain.SourceGovernanceDecision, SourceRecordID: d.DecisionID, RawJSON: raw})
		}
		if pageBody.NextCursor == "" || len(*pageBody.Records) == 0 {
			return out, nil
		}
		cursor = pageBody.NextCursor
	}
}

func errOrMissing(err error) error {
	if err != nil {
		return err
	}
	return errors.New("missing records or decision_id")
}

// GetByID fetches one governance decision by ID (used when the caller
// supplies explicit governance_decision_ids rather than a date range).
func (c *GovernanceDecisionClient) GetByID(ctx context.Context, decisionID string) (*SourceRecord, error) {
	return getByID(ctx, c.http, c.log, fmt.Sprintf("%s/v1/decisions/%s", c.baseURL, url.PathEscape(decisionID)),
		domain.SourceGovernanceDecision, decisionID, governanceEnvelope)
}

// ── authorization-svc ─────────────────────────────────────────────────────────

type AccessDecisionClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewAccessDecisionClient(baseURL string, log *zap.Logger) *AccessDecisionClient {
	return &AccessDecisionClient{baseURL: baseURL, log: log, http: &http.Client{Timeout: 5 * time.Second}}
}

// GetByID is the ONLY discovery mechanism authorization-svc exposes — no
// list/filter endpoint exists. Callers must already know the ID.
func (c *AccessDecisionClient) GetByID(ctx context.Context, accessDecisionID string) (*SourceRecord, error) {
	return getByID(ctx, c.http, c.log, fmt.Sprintf("%s/v1/access-decisions/%s", c.baseURL, accessDecisionID),
		domain.SourceAccessDecision, accessDecisionID)
}

// ── workflow-svc ───────────────────────────────────────────────────────────────

type WorkflowClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewWorkflowClient(baseURL string, log *zap.Logger) *WorkflowClient {
	return &WorkflowClient{baseURL: baseURL, log: log, http: &http.Client{Timeout: 5 * time.Second}}
}

// GetByID is the ONLY discovery mechanism workflow-svc exposes — no list
// endpoint, and even this returns the instance + stages, NOT the transition
// history (workflow-svc has no endpoint exposing workflow_transitions at all;
// a documented v1 gap, see internal/domain package doc).
func (c *WorkflowClient) GetByID(ctx context.Context, workflowInstanceID string) (*SourceRecord, error) {
	return getByID(ctx, c.http, c.log, fmt.Sprintf("%s/v1/workflows/%s", c.baseURL, workflowInstanceID),
		domain.SourceWorkflowInstance, workflowInstanceID)
}

// ── workflow-history-svc ─────────────────────────────────────────────────────

// WorkflowHistoryClient closes the gap workflow-history-svc's own package
// doc named directly: "evidence-manifest-svc currently fetches workflow
// data directly from workflow-svc by workflow_instance_id and is NOT wired
// to this cross-workflow query endpoint." This client uses the per-instance
// endpoint (GET /v1/workflows/{id}/history, see ListByInstanceID) and the
// cross-workflow, entity+date-range query (GET /v1/workflows/history, see
// ListByEntityAndDateRange) — both real endpoints on workflow-history-svc.
type WorkflowHistoryClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewWorkflowHistoryClient(baseURL string, log *zap.Logger) *WorkflowHistoryClient {
	return &WorkflowHistoryClient{baseURL: baseURL, log: log, http: &http.Client{Timeout: 5 * time.Second}}
}

// ListByInstanceID calls workflow-history-svc's real
// GET /v1/workflows/{id}/history, producing one SourceRecord per real
// transition event — the same "list produces many records" shape as
// GovernanceDecisionClient.ListByEntityAndDateRange. A 404 (no history
// recorded yet for this instance) is not an error: it means zero
// transitions, an empty list, not a failure of manifest generation.
func (c *WorkflowHistoryClient) ListByInstanceID(ctx context.Context, workflowInstanceID string) ([]SourceRecord, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/v1/workflows/%s/history", c.baseURL, workflowInstanceID), nil)
	if err != nil {
		return nil, ErrSourceUnavailable
	}
	forwardTenant(ctx, req)
	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("workflow-history-svc unreachable — failing closed", zap.Error(err))
		return nil, ErrSourceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		c.log.Error("workflow-history-svc unexpected status", zap.Int("status", resp.StatusCode))
		return nil, ErrSourceUnavailable
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, ErrSourceUnavailable
	}
	var events []struct {
		EventID string `json:"event_id"`
	}
	if err := json.Unmarshal(body, &events); err != nil {
		return nil, fmt.Errorf("aggregator: decode workflow history list: %w", err)
	}

	// Re-marshal each element individually, exactly like the governance
	// decision list, so each ManifestRecord snapshot is one transition
	// event's JSON, not the whole array.
	var raw []json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("aggregator: decode workflow history list: %w", err)
	}

	out := make([]SourceRecord, 0, len(events))
	for i, e := range events {
		out = append(out, SourceRecord{
			SourceType:     domain.SourceWorkflowHistory,
			SourceRecordID: e.EventID,
			RawJSON:        raw[i],
		})
	}
	return out, nil
}

// ListByEntityAndDateRange calls workflow-history-svc's real
// GET /v1/workflows/history?legal_entity_id=...&from=...&to=... (the
// "cross-workflow query endpoint" named in this client's own doc comment),
// the same "list produces many records" shape as
// GovernanceDecisionClient.ListByEntityAndDateRange. Both from and to are
// required by the callee (it 400s on a missing or non-RFC3339 value, and on
// to <= from), matching the non-pointer signature this method is called
// with in handler.go — unlike the governance decisions list, whose from/to
// are genuinely optional there.
func (c *WorkflowHistoryClient) ListByEntityAndDateRange(ctx context.Context, legalEntityID string, from, to time.Time) ([]SourceRecord, error) {
	q := url.Values{}
	q.Set("legal_entity_id", legalEntityID)
	q.Set("from", from.UTC().Format(time.RFC3339))
	q.Set("to", to.UTC().Format(time.RFC3339))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/workflows/history?"+q.Encode(), nil)
	if err != nil {
		return nil, ErrSourceUnavailable
	}
	forwardTenant(ctx, req)
	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("workflow-history-svc unreachable — failing closed", zap.Error(err))
		return nil, ErrSourceUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		c.log.Error("workflow-history-svc unexpected status", zap.Int("status", resp.StatusCode))
		return nil, ErrSourceUnavailable
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, ErrSourceUnavailable
	}
	var events []struct {
		EventID string `json:"event_id"`
	}
	if err := json.Unmarshal(body, &events); err != nil {
		return nil, fmt.Errorf("aggregator: decode cross-workflow history list: %w", err)
	}

	// Re-marshal each element individually, same reason as ListByInstanceID.
	var raw []json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("aggregator: decode cross-workflow history list: %w", err)
	}

	out := make([]SourceRecord, 0, len(events))
	for i, e := range events {
		out = append(out, SourceRecord{
			SourceType:     domain.SourceWorkflowHistory,
			SourceRecordID: e.EventID,
			RawJSON:        raw[i],
		})
	}
	return out, nil
}

// ── shared helper ────────────────────────────────────────────────────────────

func getByID(ctx context.Context, client *http.Client, log *zap.Logger, url string, sourceType domain.SourceType, id string, decorate ...func(context.Context, *http.Request)) (*SourceRecord, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, ErrSourceUnavailable
	}
	forwardTenant(ctx, req)
	for _, d := range decorate {
		d(ctx, req)
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Error("source service unreachable — failing closed", zap.String("url", url), zap.Error(err))
		return nil, ErrSourceUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrSourceNotFound
	}
	if resp.StatusCode != http.StatusOK {
		log.Error("source service unexpected status", zap.String("url", url), zap.Int("status", resp.StatusCode))
		return nil, ErrSourceUnavailable
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, ErrSourceUnavailable
	}
	return &SourceRecord{SourceType: sourceType, SourceRecordID: id, RawJSON: body}, nil
}
