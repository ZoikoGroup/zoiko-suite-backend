// Package entityregistry reads legal entities and their jurisdiction
// assignments from tenant-entity-registry-svc, the system of record this
// service's entity applicability projection is built from (Doc 03 §8.2).
//
// It is used by the J-10 backfill: the projection is fed by Kafka events, and
// an entity whose events left the topic's retention before this service read
// them has no mapping until it is backfilled from the registry itself.
package entityregistry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	idenv "zoiko.io/identity-envelope"

	svcenvelope "zoiko.io/jurisdiction-rules-svc/internal/envelope"
)

// ErrUnavailable is any failure to get an authoritative answer.
var ErrUnavailable = errors.New("tenant-entity-registry-svc unavailable")

// ErrRefused is the registry refusing the caller (401/403/404 on the tenant).
var ErrRefused = errors.New("tenant-entity-registry-svc refused the read")

// Entity is the part of a legal entity the projection needs.
type Entity struct {
	LegalEntityID         string    `json:"legal_entity_id"`
	TenantID              string    `json:"tenant_id"`
	PrimaryJurisdictionID string    `json:"primary_jurisdiction_id"`
	RecordVersion         int64     `json:"record_version"`
	CreatedAt             time.Time `json:"created_at"`
}

// Assignment is a jurisdiction assignment of a legal entity.
type Assignment struct {
	AssignmentID   string     `json:"assignment_id"`
	TenantID       string     `json:"tenant_id"`
	LegalEntityID  string     `json:"legal_entity_id"`
	JurisdictionID string     `json:"jurisdiction_id"`
	AssignmentType string     `json:"assignment_type"`
	EffectiveFrom  time.Time  `json:"effective_from"`
	EffectiveTo    *time.Time `json:"effective_to"`
	RecordVersion  int64      `json:"record_version"`
}

// Reader is what the backfill needs from the registry.
type Reader interface {
	ListEntities(ctx context.Context, tenantID string) ([]Entity, error)
	ListAssignments(ctx context.Context, legalEntityID string) ([]Assignment, error)
}

// Client reads the registry over HTTP, as the caller: the request's input
// envelope and signed identity are forwarded, so the registry applies its own
// tenant isolation and authorization to the operator who asked for the backfill.
type Client struct {
	baseURL string
	http    *http.Client
}

// New returns a client for baseURL (e.g. http://tenant-svc:8081).
func New(baseURL string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: 10 * time.Second}}
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Accept", "application/json")
	svcenvelope.ForwardTo(ctx, req)
	idenv.Forward(ctx, req)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%w: %s answered %d: %s", ErrRefused, path, resp.StatusCode, body)
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%w: %s answered %d: %s", ErrUnavailable, path, resp.StatusCode, body)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(out); err != nil {
		return fmt.Errorf("%w: malformed answer from %s: %v", ErrUnavailable, path, err)
	}
	return nil
}

// ListEntities lists a tenant's legal entities.
func (c *Client) ListEntities(ctx context.Context, tenantID string) ([]Entity, error) {
	var out []Entity
	if err := c.get(ctx, "/v1/tenants/"+url.PathEscape(tenantID)+"/entities", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListAssignments lists a legal entity's jurisdiction assignments.
func (c *Client) ListAssignments(ctx context.Context, legalEntityID string) ([]Assignment, error) {
	var out []Assignment
	if err := c.get(ctx, "/v1/entities/"+url.PathEscape(legalEntityID)+"/jurisdictions", &out); err != nil {
		return nil, err
	}
	return out, nil
}
