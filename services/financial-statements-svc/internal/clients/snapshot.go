// Package clients holds HTTP implementations of the upstream contracts the service layer depends on.
package clients

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"time"

	"zoiko.io/financial-statements-svc/internal/domain"
)

// SnapShot is the subset of financial-close-svc's FinancialSnapshot this service reads.

type Snapshot struct {
	SnapshotID    string `json:"snapshot_id"`
	LegalEntityID string `json:"legal_entity_id"`
	Content       string `json:"content"`
	ContentHash   string `json:"content_hash"`
	Status        string `json:"status"`
}

// SnapshotReader is what the service layer needs from financial-close-svc's ACC-16(Signed Financial Snapshot) API.

type SnapshotReader interface {
	// GetSnapshot forwards the caller's own identity (not a system principal): financial-close-svc authorizes GET /v1/financial-snapshots/{id} against the caller's own view permission for that snapshot's legal entity, so a user with no right to see the underlying ledger data cannot build a statement set from it either.

	GetSnapshot(ctx context.Context, tenantID, principalID, snapshotID string) (*Snapshot, error)
}

const snapshotStatusCertified = "CERTIFIED"

// HTTPSnapshotReader is the real implementation.
type HTTPSnapshotClient struct {
	BaseURL string
	Client  *http.Client
}

// NewHTTPSnapshotClient builds the client with a short timeout.

func NewHTTPSnapshotClient(baseURL string) *HTTPSnapshotClient {
	return &HTTPSnapshotClient{
		BaseURL: baseURL,
		Client:  &http.Client{Timeout: 5 * time.Second},
	}
}

var _ SnapshotReader = (*HTTPSnapshotClient)(nil)

// GetSnapshot fetches the snapshot and fails closed unless it is CERTIFIED:
// REP-01's own mandatory control (ZS-SVC-H-001 §4.1) is that certified
// output must bind to a Signed Financial Snapshot, never a Draft or merely
// Sealed one.
func (c *HTTPSnapshotClient) GetSnapshot(ctx context.Context, tenantID, principalID, snapshotID string) (*Snapshot, error) {
	u := c.BaseURL + "/v1/financial-snapshots/" + url.PathEscape(snapshotID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, domain.Errf(domain.CodeDependencyUnavailable, "cannot build financial-close-svc request")
	}
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)
	req.Header.Set("Accept", "application/json")

	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, domain.Errf(domain.CodeDependencyUnavailable, "financial-close-svc unreachable; failing closed")
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, domain.Errf(domain.CodeSourceUnverified, "source snapshot %q not found", snapshotID)
	case resp.StatusCode == http.StatusForbidden:
		return nil, domain.Errf(domain.CodeForbidden, "not authorized to view source snapshot %q", snapshotID)
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		return nil, domain.Errf(domain.CodeDependencyUnavailable, "financial-close-svc answered %d; failing closed", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, domain.Errf(domain.CodeSourceUnverified, "financial-close-svc answered %d for snapshot %q", resp.StatusCode, snapshotID)
	}

	var snap Snapshot
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&snap); err != nil {
		return nil, domain.Errf(domain.CodeSourceUnverified, "snapshot %q response is not valid JSON", snapshotID)
	}
	if snap.Status != snapshotStatusCertified {
		return nil, domain.Errf(domain.CodeSourceUnverified, "snapshot %q is %q, not CERTIFIED", snapshotID, snap.Status)
	}
	if snap.ContentHash == "" {
		return nil, domain.Errf(domain.CodeSourceUnverified, "snapshot %q is CERTIFIED but carries no content hash", snapshotID)
	}
	return &snap, nil
}
