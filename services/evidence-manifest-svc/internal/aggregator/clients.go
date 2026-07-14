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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"go.uber.org/zap"

	"zoiko.io/evidence-manifest-svc/internal/domain"
)

var ErrSourceUnavailable = errors.New("aggregator: source service unavailable")
var ErrSourceNotFound = errors.New("aggregator: source record not found")

// SourceRecord is a fetched source record paired with the raw JSON to snapshot.
type SourceRecord struct {
	SourceType     domain.SourceType
	SourceRecordID string
	RawJSON        []byte
}

// ── governance-decision-log-svc ──────────────────────────────────────────────

type GovernanceDecisionClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewGovernanceDecisionClient(baseURL string, log *zap.Logger) *GovernanceDecisionClient {
	return &GovernanceDecisionClient{baseURL: baseURL, log: log, http: &http.Client{Timeout: 5 * time.Second}}
}

// ListByEntityAndDateRange calls governance-decision-log-svc's REAL list
// endpoint (GET /v1/decisions?entity=...&from=...&to=...) — the only source
// in this service with genuine discovery.
func (c *GovernanceDecisionClient) ListByEntityAndDateRange(ctx context.Context, legalEntityID string, from, to *time.Time) ([]SourceRecord, error) {
	q := url.Values{}
	q.Set("entity", legalEntityID)
	if from != nil {
		q.Set("from", from.UTC().Format(time.RFC3339))
	}
	if to != nil {
		q.Set("to", to.UTC().Format(time.RFC3339))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/decisions?"+q.Encode(), nil)
	if err != nil {
		return nil, ErrSourceUnavailable
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("governance-decision-log-svc unreachable — failing closed", zap.Error(err))
		return nil, ErrSourceUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		c.log.Error("governance-decision-log-svc unexpected status", zap.Int("status", resp.StatusCode))
		return nil, ErrSourceUnavailable
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, ErrSourceUnavailable
	}
	var decisions []struct {
		DecisionID string `json:"decision_id"`
	}
	if err := json.Unmarshal(body, &decisions); err != nil {
		return nil, fmt.Errorf("aggregator: decode governance decisions list: %w", err)
	}

	// Re-marshal each element individually so each ManifestRecord snapshot is
	// exactly one decision's JSON, not the whole array.
	var raw []json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("aggregator: decode governance decisions list: %w", err)
	}

	out := make([]SourceRecord, 0, len(decisions))
	for i, d := range decisions {
		out = append(out, SourceRecord{
			SourceType:     domain.SourceGovernanceDecision,
			SourceRecordID: d.DecisionID,
			RawJSON:        raw[i],
		})
	}
	return out, nil
}

// GetByID fetches one governance decision by ID (used when the caller
// supplies explicit governance_decision_ids rather than a date range).
func (c *GovernanceDecisionClient) GetByID(ctx context.Context, decisionID string) (*SourceRecord, error) {
	return getByID(ctx, c.http, c.log, fmt.Sprintf("%s/v1/decisions/%s", c.baseURL, decisionID),
		domain.SourceGovernanceDecision, decisionID)
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

// ── shared helper ────────────────────────────────────────────────────────────

func getByID(ctx context.Context, client *http.Client, log *zap.Logger, url string, sourceType domain.SourceType, id string) (*SourceRecord, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, ErrSourceUnavailable
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
