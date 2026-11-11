// Package recordvault declares a regulated notice's evidence package as a governed record in document-vault-svc
// (DRC-02, ZS-SVC-Y-001 s8.1, NP-58).
//
// Declaring a record converts one exact stored document version into a record, so the package is stored first
// (POST /v1/documents), its current version is read, and that version is declared (POST /v1/records). The three
// calls carry one idempotency key, so a retry after a partial failure replays instead of storing a second copy.
// Every failure is an error and the notice stays undeclared: a record reference is never invented.
package recordvault

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
	idenv "zoiko.io/identity-envelope"

	svcenvelope "zoiko.io/notification-svc/internal/envelope"
)

// ErrUnavailable means the vault did not accept the package; nothing is declared.
var ErrUnavailable = errors.New("document vault unavailable")

// ErrRejected means the vault refused the declaration on its merits (a 4xx): retrying the same request cannot help.
var ErrRejected = errors.New("document vault rejected the record declaration")

// Declaration is one evidence package to declare.
type Declaration struct {
	TenantID, PrincipalID, CorrelationID string
	LegalEntityID                        string
	Title                                string
	Package                              []byte // JSON
	RecordClass                          string
	JurisdictionScope                    string
	RetentionScheduleRef                 string
	BusinessContext                      string
	DeclarationReason                    string
	IdempotencyKey                       string
}

// Client calls document-vault-svc.
type Client struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

// NewClient returns a client with a bounded timeout.
func NewClient(baseURL string, timeout time.Duration, log *zap.Logger) *Client {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: timeout}, log: log}
}

func (c *Client) do(ctx context.Context, d Declaration, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rd)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Tenant-Id", d.TenantID)
	req.Header.Set("X-Principal-Id", d.PrincipalID)
	if d.CorrelationID != "" {
		req.Header.Set("X-Correlation-ID", d.CorrelationID)
	}
	if d.IdempotencyKey != "" {
		req.Header.Set("Idempotency-Key", d.IdempotencyKey+":"+method+path)
	}
	svcenvelope.ForwardTo(ctx, req)
	idenv.Forward(ctx, req)
	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Warn("document-vault-svc unreachable", zap.Error(err))
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusRequestTimeout && resp.StatusCode != http.StatusTooManyRequests {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%w: %s %s answered %d: %s", ErrRejected, method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("%w: %s %s answered %d", ErrUnavailable, method, path, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("%w: unreadable answer from %s: %v", ErrUnavailable, path, err)
	}
	return nil
}

// Declare stores the package and declares it a record, returning the record id.
func (c *Client) Declare(ctx context.Context, d Declaration) (string, error) {
	var doc struct {
		DocumentID     string `json:"document_id"`
		CurrentVersion int    `json:"current_version"`
	}
	if err := c.do(ctx, d, http.MethodPost, "/v1/documents", map[string]any{
		"legal_entity_id": d.LegalEntityID, "title": d.Title, "classification": "CONFIDENTIAL",
		"content_type": "application/json", "content_base64": base64.StdEncoding.EncodeToString(d.Package),
	}, &doc); err != nil {
		return "", err
	}
	if doc.DocumentID == "" {
		return "", fmt.Errorf("%w: the vault returned no document id", ErrUnavailable)
	}
	var versions []struct {
		DocumentVersionID string `json:"document_version_id"`
		Version           int    `json:"version"`
	}
	if err := c.do(ctx, d, http.MethodGet, "/v1/documents/"+doc.DocumentID+"/versions", nil, &versions); err != nil {
		return "", err
	}
	versionID := ""
	for _, v := range versions {
		if v.Version == doc.CurrentVersion {
			versionID = v.DocumentVersionID
		}
	}
	if versionID == "" {
		return "", fmt.Errorf("%w: the vault listed no version %d of document %s", ErrUnavailable, doc.CurrentVersion, doc.DocumentID)
	}
	var rec struct {
		RecordID string `json:"record_id"`
	}
	if err := c.do(ctx, d, http.MethodPost, "/v1/records", map[string]any{
		"document_id": doc.DocumentID, "document_version_id": versionID, "legal_entity_id": d.LegalEntityID,
		"record_class": d.RecordClass, "jurisdiction_scope": d.JurisdictionScope, "business_context": d.BusinessContext,
		"retention_schedule_ref": d.RetentionScheduleRef, "declaration_reason": d.DeclarationReason, "correlation_id": d.CorrelationID,
	}, &rec); err != nil {
		return "", err
	}
	if rec.RecordID == "" {
		return "", fmt.Errorf("%w: the vault returned no record id", ErrUnavailable)
	}
	return rec.RecordID, nil
}
