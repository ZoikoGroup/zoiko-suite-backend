// Package entity provides a read-only client against tenant-entity-registry-svc,
// used to reconcile a caller-supplied legal_entity_id with the caller's verified
// tenant before anything is written against it.
//
// THE GAP THIS CLOSES. Every write on this service authorizes against a
// legal_entity_id taken from the request body, and nothing checked that the
// entity belonged to the caller's tenant. Fixing the tenant scope (see
// requireTenant) closed the register and the row's tenant_id, but left this:
// authority is granted per entity, so a principal holding a grant on an entity in
// ANOTHER tenant could raise an invoice attributed to that entity while the row
// itself was filed under their own tenant — a receivable whose two halves name
// different tenants. This is the gap left open in obligations-svc and named in the
// notes as worth grepping for in every entity-scoped service; this is one of them.
//
// It fails CLOSED. If the registry cannot be reached, no invoice is issued. That
// is the same posture this service already takes on authorization-svc and
// general-ledger-svc, and for the same reason: writing a receivable whose
// attribution could not be checked is worse than not writing one.
package entity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

var (
	// ErrNotFound means the registry was reached and knows no such legal entity.
	ErrNotFound = errors.New("legal entity not found")

	// ErrForeignTenant means the entity exists but belongs to another tenant.
	// Distinct from ErrNotFound on purpose: the caller is not entitled to learn
	// which of the two it is, so the HANDLER collapses them — but this service's
	// own logs should record which happened, because one is a typo and the other
	// is a cross-tenant attempt.
	ErrForeignTenant = errors.New("legal entity belongs to another tenant")

	// ErrNotActive means the entity is real and in-tenant but not in a status that
	// may take on new receivables.
	ErrNotActive = errors.New("legal entity is not active")

	// ErrUnavailable means the registry could not be reached or answered
	// unexpectedly. Callers MUST fail closed.
	ErrUnavailable = errors.New("tenant-entity-registry-svc unavailable")
)

// LegalEntity is the subset of the registry's entity needed here.
type LegalEntity struct {
	LegalEntityID string `json:"legal_entity_id"`
	TenantID      string `json:"tenant_id"`
	EntityStatus  string `json:"entity_status"`
}

// Client is the narrow interface the handler depends on.
type Client interface {
	// VerifyInTenant confirms legalEntityID exists, belongs to tenantID, and is
	// in a status that may take on new receivables.
	VerifyInTenant(ctx context.Context, tenantID, legalEntityID string) error
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
}

func NewHTTPClient(baseURL string) *HTTPClient {
	return &HTTPClient{
		baseURL: baseURL,
		http:    &http.Client{Timeout: 3 * time.Second, Transport: newRetryTransport()},
	}
}

// activeStatuses are the entity statuses that may take on a new receivable.
//
// tenant-entity-registry-svc's vocabulary is ACTIVE, DORMANT, SUSPENDED and
// DISSOLVED (internal/domain/enums.go). Only ACTIVE is here: a dissolved or
// suspended entity plainly must not raise new invoices, and a DORMANT one is by
// definition not trading — raising a receivable in its name is the kind of thing
// a governance platform exists to prevent rather than permit by omission.
//
// Deliberately an allow-list, not a deny-list of the dead ones: a status added to
// the registry later should stop invoicing until somebody decides it should not.
// A deny-list would silently admit it.
var activeStatuses = map[string]bool{
	"ACTIVE": true,
}

func (c *HTTPClient) VerifyInTenant(ctx context.Context, tenantID, legalEntityID string) error {
	// Path segment, path-escaped — never interpolated into a query string.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/v1/entities/%s", c.baseURL, url.PathEscape(legalEntityID)), nil)
	if err != nil {
		return fmt.Errorf("%w: build request: %v", ErrUnavailable, err)
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("%w: status %d: %s", ErrUnavailable, resp.StatusCode, body)
	}

	var e LegalEntity
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		return fmt.Errorf("%w: decode: %v", ErrUnavailable, err)
	}

	// The comparison this whole package exists for. Note the registry's own
	// GET /v1/entities/{id} is NOT tenant-scoped — it serves any entity to anyone
	// who asks — so sending X-Tenant-Id above does not filter anything and the
	// check has to happen HERE. That is a gap in tenant-entity-registry-svc, not
	// one this service can fix, and it is precisely why this client compares the
	// tenant itself rather than trusting a 404.
	if e.TenantID != tenantID {
		return ErrForeignTenant
	}
	if !activeStatuses[e.EntityStatus] {
		return fmt.Errorf("%w: status is %s", ErrNotActive, e.EntityStatus)
	}
	return nil
}
