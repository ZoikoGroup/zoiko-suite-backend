// Package archivestore looks up AUD-10 archives from audit-event-store-svc
// without making reporting-orchestration-svc a second owner of the hash
// chain — same narrow-client shape as workflow-svc's internal/evidence and
// internal/documentvault clients. BuildExportPackage depends on this to
// cite REAL archive content rather than fabricate it — see
// internal/domain/export.go's own package doc for why that distinction
// matters here specifically.
package archivestore

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"go.uber.org/zap"
)

// ErrArchiveServiceUnavailable is returned on any network error, non-200
// response, or an archive_id mismatch in the decoded body. Callers must
// fail closed — an unreachable audit-event-store-svc must never be
// silently treated as "archive doesn't exist" or "archive is fine."
var ErrArchiveServiceUnavailable = errors.New("audit-event-store-svc archive lookup unavailable")

type Archive struct {
	ArchiveID     string `json:"archive_id"`
	FromSequence  int64  `json:"from_sequence"`
	ToSequence    int64  `json:"to_sequence"`
	EventCount    int64  `json:"event_count"`
	ArchiveDigest string `json:"archive_digest"`
}

// Client is the narrow interface the handler/store depend on.
type Client interface {
	// GetArchive fetches archiveID's real record from audit-event-store-svc.
	// Fails closed on any network/non-200/malformed response.
	GetArchive(ctx context.Context, archiveID, actorID, correlationID string) (*Archive, error)
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{baseURL: baseURL, log: log, http: &http.Client{Timeout: 3 * time.Second}}
}

func (c *HTTPClient) GetArchive(ctx context.Context, archiveID, actorID, correlationID string) (*Archive, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/archives/"+archiveID, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Principal-Id", actorID)
	req.Header.Set("X-Correlation-ID", correlationID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("audit-event-store-svc unavailable while fetching archive", zap.String("archive_id", archiveID), zap.Error(err))
		return nil, ErrArchiveServiceUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		c.log.Error("audit-event-store-svc refused archive lookup", zap.String("archive_id", archiveID), zap.Int("status", resp.StatusCode))
		return nil, ErrArchiveServiceUnavailable
	}
	var a Archive
	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
		return nil, ErrArchiveServiceUnavailable
	}
	if a.ArchiveID != archiveID {
		return nil, ErrArchiveServiceUnavailable
	}
	return &a, nil
}
