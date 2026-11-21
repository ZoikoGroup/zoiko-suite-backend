// Package documentvault stores a bank statement's original raw file in
// document-vault-svc (BNK-03's "raw source reference" requirement,
// SPEC_DEVIATIONS.md A10) and returns a retrievable reference to it. Same
// narrow-client shape as expense-claim-svc's and workflow-svc's own
// internal/documentvault packages: fails closed, never a store of its own.
package documentvault

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"
)

// ErrVaultUnavailable covers any network error, non-2xx response, or
// unreadable/incomplete answer from document-vault-svc. Callers must treat
// it as a refusal to store the raw file — never as "stored, probably".
var ErrVaultUnavailable = errors.New("document-vault-svc unavailable")

// Reference is what the vault gives back for a stored raw statement file.
type Reference struct {
	// DocumentID is document-vault-svc's own document_id — the retrievable
	// pointer to the original bytes (GET /v1/documents/{id}/content).
	DocumentID string
	// ChecksumSHA256 is read back from the vault's own verify-digest query,
	// not computed locally and echoed — it is independent proof of what the
	// vault actually stored, not merely what was sent to it.
	ChecksumSHA256 string
}

// Client is the narrow interface the handler depends on.
type Client interface {
	// StoreRawStatement uploads content to document-vault-svc under
	// legalEntityID and returns a retrievable reference, or
	// ErrVaultUnavailable. inbound is the IngestStatement request that
	// triggered this upload — its own canonical-envelope headers
	// (ZS-ARCH-SVC-001 §4: X-Request-Id, X-Correlation-ID, X-Source-Channel,
	// Idempotency-Key, X-Purpose-Context) are forwarded onto the outbound
	// create-document call, the same "forward the inbound envelope"
	// discipline every other inter-service client in this platform follows
	// (see e.g. tenant-entity-registry-svc/internal/currency's
	// svcenvelope.ForwardTo) — document-vault-svc enforces that same
	// contract on every write and refuses a request missing it.
	StoreRawStatement(ctx context.Context, inbound *http.Request, tenantID, legalEntityID, actorPrincipalID, title, contentType string, content []byte) (Reference, error)
}

type HTTPClient struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewHTTPClient(baseURL string, log *zap.Logger) *HTTPClient {
	return &HTTPClient{baseURL: baseURL, log: log, http: &http.Client{Timeout: 15 * time.Second}}
}

type createDocumentRequest struct {
	LegalEntityID   string `json:"legal_entity_id"`
	Title           string `json:"title"`
	Classification  string `json:"classification"`
	RetentionPolicy string `json:"retention_policy"`
	ContentType     string `json:"content_type"`
	ContentBase64   string `json:"content_base64"`
}

type createDocumentResponse struct {
	DocumentID string `json:"document_id"`
}

type digestVerification struct {
	DocumentID     string `json:"document_id"`
	ChecksumSHA256 string `json:"checksum_sha256"`
	Verified       bool   `json:"verified"`
}

// envelopeForwardHeaders are the inbound request's own canonical-envelope
// headers, copied verbatim onto the outbound request. Idempotency-Key is
// included deliberately: document-vault-svc's own idempotency is keyed on
// (actor, "CreateDocument", request-body-hash), a different operation name
// than anything banking-connector-svc's own idempotency uses, so reusing the
// same key cannot collide with it — and a retried IngestStatement call
// correctly retries into the same stored document rather than creating a
// second one.
var envelopeForwardHeaders = []string{
	"X-Request-Id", "X-Correlation-ID", "X-Source-Channel", "Idempotency-Key", "X-Purpose-Context",
}

// forwardEnvelopeHeaders copies inbound's own envelope headers onto out, when
// inbound is non-nil and carries them. Values in out already set (X-Tenant-Id,
// X-Principal-Id, X-Legal-Entity-Id) are left alone — this only adds the
// trace/idempotency/channel/purpose fields the caller did not already set.
func forwardEnvelopeHeaders(out *http.Request, inbound *http.Request) {
	if inbound == nil {
		return
	}
	for _, h := range envelopeForwardHeaders {
		if v := inbound.Header.Get(h); v != "" {
			out.Header.Set(h, v)
		}
	}
}

func (c *HTTPClient) StoreRawStatement(ctx context.Context, inbound *http.Request, tenantID, legalEntityID, actorPrincipalID, title, contentType string, content []byte) (Reference, error) {
	body := createDocumentRequest{
		LegalEntityID: legalEntityID,
		Title:         title,
		// CONFIDENTIAL, not PUBLIC/INTERNAL: a bank statement is financial
		// source evidence, and this is the same classification level
		// expense-claim-svc's receipts use.
		Classification:  "CONFIDENTIAL",
		RetentionPolicy: "DEFAULT",
		ContentType:     contentType,
		ContentBase64:   base64.StdEncoding.EncodeToString(content),
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Reference{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/documents", bytes.NewReader(raw))
	if err != nil {
		return Reference{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", actorPrincipalID)
	req.Header.Set("X-Legal-Entity-Id", legalEntityID)
	forwardEnvelopeHeaders(req, inbound)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("document-vault-svc unreachable while storing raw statement — failing closed", zap.Error(err))
		return Reference{}, ErrVaultUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		c.log.Error("document-vault-svc refused raw statement upload — failing closed", zap.Int("status", resp.StatusCode))
		return Reference{}, ErrVaultUnavailable
	}
	var created createDocumentResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil || created.DocumentID == "" {
		return Reference{}, ErrVaultUnavailable
	}

	digestReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/documents/"+created.DocumentID+"/verify-digest", nil)
	if err != nil {
		return Reference{}, err
	}
	digestReq.Header.Set("X-Tenant-Id", tenantID)
	digestReq.Header.Set("X-Principal-Id", actorPrincipalID)
	digestReq.Header.Set("X-Legal-Entity-Id", legalEntityID)
	forwardEnvelopeHeaders(digestReq, inbound)
	digestResp, err := c.http.Do(digestReq)
	if err != nil {
		c.log.Error("document-vault-svc unreachable while verifying raw statement digest — failing closed", zap.Error(err))
		return Reference{}, ErrVaultUnavailable
	}
	defer digestResp.Body.Close()
	if digestResp.StatusCode != http.StatusOK {
		c.log.Error("document-vault-svc refused raw statement digest verification — failing closed", zap.Int("status", digestResp.StatusCode))
		return Reference{}, ErrVaultUnavailable
	}
	var dv digestVerification
	if err := json.NewDecoder(digestResp.Body).Decode(&dv); err != nil {
		return Reference{}, ErrVaultUnavailable
	}
	if !dv.Verified || dv.ChecksumSHA256 == "" {
		return Reference{}, fmt.Errorf("document-vault-svc stored the raw statement but its own digest verification did not pass")
	}
	return Reference{DocumentID: created.DocumentID, ChecksumSHA256: dv.ChecksumSHA256}, nil
}
