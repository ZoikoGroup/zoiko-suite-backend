// Package searchclient provides a reusable, tenant-scoped OpenSearch client
// for the ZoikoSuite platform.
//
// # Architecture context (01-backend.md §10.4)
//
// OpenSearch backs document search, audit retrieval, obligations lookup, and
// contract clause retrieval. Every index operation and every search MUST be
// scoped to a tenant — a search without TenantID is architecturally prohibited
// and is rejected at the API boundary (ErrTenantIDRequired).
//
// # Index naming convention
//
// Indices are domain-scoped, not tenant-scoped. Every document carries
// tenant_id and legal_entity_id as indexed fields so a tenant filter is
// always applied at query time:
//
//	zoiko-obligations  — obligations-svc records
//	zoiko-audit        — audit event store records
//	zoiko-documents    — Document Vault records (Phase 2)
//
// A per-tenant index model was explicitly rejected: it creates unbounded index
// proliferation and makes cross-entity queries impossible. See README.md §Index
// Naming for the full rationale and the GTRM/residency follow-up note.
//
// # Tamper / residency follow-up (Phase 2.x)
//
// For RESTRICTED-tier data (data classification §10.3), the search index
// may need to be pinned to a region-specific OpenSearch cluster to satisfy
// GTRM routing requirements. This is documented in README.md and flagged as
// a Phase 2.x follow-up requiring cross-team coordination. This package uses
// a single cluster address for now.
package searchclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	opensearch "github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// IndexName is a strongly-typed, domain-scoped index name.
// Use the package-level constants — never construct raw strings.
type IndexName string

const (
	// IndexObligations indexes obligations-svc records.
	IndexObligations IndexName = "zoiko-obligations"
	// IndexAuditEvents indexes audit-event-store-svc records.
	IndexAuditEvents IndexName = "zoiko-audit"
	// IndexDocuments indexes Document Vault records (Phase 2).
	IndexDocuments IndexName = "zoiko-documents"
)

// Config holds the connection parameters for an OpenSearch cluster.
type Config struct {
	// Addresses is the list of OpenSearch node URLs.
	// Example: []string{"http://opensearch:9200"}
	Addresses []string

	// Username and Password are empty in dev (security plugin disabled).
	// They are required in staging/prod with TLS + auth enabled.
	Username string
	Password string
}

// Document is the unit of indexing.
//
// Every document MUST carry TenantID and LegalEntityID so tenant-scoped
// queries can always be applied. Omitting either is a caller error.
type Document struct {
	// ID becomes the OpenSearch _id, enabling idempotent upserts.
	// Use the domain record's primary key (e.g. obligation_id).
	ID string

	// TenantID is mandatory. Every indexed document must be tenant-bound.
	TenantID string `json:"tenant_id"`

	// LegalEntityID is mandatory per doctrine §3.2.
	LegalEntityID string `json:"legal_entity_id"`

	// Body holds the domain-specific fields to index. Keys should be
	// consistent within each index to enable reliable field-level queries.
	Body map[string]any
}

// SearchQuery holds parameters for a tenant-scoped keyword query.
type SearchQuery struct {
	// TenantID is required. Search without tenant scope is prohibited.
	TenantID string

	// Keywords is the full-text match string applied across all text fields.
	// Empty string returns all documents for the tenant (use with care).
	Keywords string

	// Size is the maximum number of results to return. Defaults to 20.
	Size int
}

// SearchResult is a single matched document returned from a search.
type SearchResult struct {
	// ID is the OpenSearch _id, matching the Document.ID used at index time.
	ID string

	// Score is the relevance score assigned by OpenSearch.
	Score float64

	// Body is the raw source document as indexed.
	Body map[string]any
}

// ErrTenantIDRequired is returned by Search when TenantID is empty.
// A search without a tenant filter violates the platform's isolation model.
var ErrTenantIDRequired = errors.New("searchclient: TenantID is required — a search without tenant scope is prohibited")

// Client is the narrow interface consumers depend on.
// Construct with New().
type Client interface {
	// EnsureIndex creates the index if it does not already exist (idempotent).
	// Safe to call on every startup.
	EnsureIndex(ctx context.Context, index IndexName) error

	// Index upserts a document by ID. Idempotent: re-indexing the same ID
	// overwrites the previous document.
	Index(ctx context.Context, index IndexName, doc Document) error

	// Search executes a tenant-scoped keyword search.
	// Returns ErrTenantIDRequired if query.TenantID is empty.
	Search(ctx context.Context, index IndexName, q SearchQuery) ([]SearchResult, error)
}

// client is the concrete implementation of Client.
type client struct {
	os *opensearch.Client
}

// New constructs a Client from cfg. Returns an error if the configuration
// is invalid or the underlying OpenSearch client cannot be created.
func New(cfg Config) (Client, error) {
	if len(cfg.Addresses) == 0 {
		return nil, errors.New("searchclient: at least one address is required")
	}
	osCfg := opensearch.Config{
		Addresses: cfg.Addresses,
	}
	if cfg.Username != "" {
		osCfg.Username = cfg.Username
		osCfg.Password = cfg.Password
	}
	c, err := opensearch.NewClient(osCfg)
	if err != nil {
		return nil, fmt.Errorf("searchclient: failed to create opensearch client: %w", err)
	}
	return &client{os: c}, nil
}

// EnsureIndex creates index if it does not already exist.
func (c *client) EnsureIndex(ctx context.Context, index IndexName) error {
	resp, err := c.os.Indices.Exists(ctx, opensearchapi.IndicesExistsReq{
		Indices: []string{string(index)},
	})
	if err != nil {
		return fmt.Errorf("searchclient: EnsureIndex exists check: %w", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode == 200 {
		return nil // already exists
	}

	// Create with basic mappings that enforce tenant_id and legal_entity_id
	// as keyword fields (not analyzed) so term queries are exact-match.
	mapping := map[string]any{
		"mappings": map[string]any{
			"properties": map[string]any{
				"tenant_id":       map[string]any{"type": "keyword"},
				"legal_entity_id": map[string]any{"type": "keyword"},
			},
		},
	}
	body, err := json.Marshal(mapping)
	if err != nil {
		return fmt.Errorf("searchclient: EnsureIndex marshal mapping: %w", err)
	}

	createResp, err := c.os.Indices.Create(ctx, opensearchapi.IndicesCreateReq{
		Index: string(index),
		Body:  bytes.NewReader(body),
	})
	if err != nil {
		return fmt.Errorf("searchclient: EnsureIndex create: %w", err)
	}
	defer createResp.Body.Close()

	if createResp.IsError() {
		raw, _ := io.ReadAll(createResp.Body)
		return fmt.Errorf("searchclient: EnsureIndex create failed [%d]: %s", createResp.StatusCode, raw)
	}
	return nil
}

// Index upserts a document into the given index.
func (c *client) Index(ctx context.Context, index IndexName, doc Document) error {
	if doc.ID == "" {
		return errors.New("searchclient: Index: doc.ID must not be empty")
	}
	if doc.TenantID == "" {
		return errors.New("searchclient: Index: doc.TenantID must not be empty")
	}
	if doc.LegalEntityID == "" {
		return errors.New("searchclient: Index: doc.LegalEntityID must not be empty")
	}

	// Merge governance fields into the body.
	merged := make(map[string]any, len(doc.Body)+2)
	for k, v := range doc.Body {
		merged[k] = v
	}
	merged["tenant_id"] = doc.TenantID
	merged["legal_entity_id"] = doc.LegalEntityID

	body, err := json.Marshal(merged)
	if err != nil {
		return fmt.Errorf("searchclient: Index marshal: %w", err)
	}

	resp, err := c.os.Index(ctx, opensearchapi.IndexReq{
		Index:      string(index),
		DocumentID: doc.ID,
		Body:       bytes.NewReader(body),
		Params: opensearchapi.IndexParams{
			Refresh: "true", // immediate visibility for tests; relax in prod
		},
	})
	if err != nil {
		return fmt.Errorf("searchclient: Index request: %w", err)
	}
	defer resp.Body.Close()

	if resp.IsError() {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("searchclient: Index failed [%d]: %s", resp.StatusCode, raw)
	}
	return nil
}

// Search executes a tenant-scoped keyword search against the given index.
func (c *client) Search(ctx context.Context, index IndexName, q SearchQuery) ([]SearchResult, error) {
	if q.TenantID == "" {
		return nil, ErrTenantIDRequired
	}
	if q.Size <= 0 {
		q.Size = 20
	}

	// Build a bool query: must match tenant_id exactly (term), keyword is
	// a multi_match across all text fields (best_fields strategy).
	mustClauses := []map[string]any{
		{"term": map[string]any{"tenant_id": q.TenantID}},
	}
	if q.Keywords != "" {
		mustClauses = append(mustClauses, map[string]any{
			"multi_match": map[string]any{
				"query":  q.Keywords,
				"type":   "best_fields",
				"fields": []string{"*"},
			},
		})
	}

	query := map[string]any{
		"size": q.Size,
		"query": map[string]any{
			"bool": map[string]any{
				"must": mustClauses,
			},
		},
	}

	body, err := json.Marshal(query)
	if err != nil {
		return nil, fmt.Errorf("searchclient: Search marshal query: %w", err)
	}

	resp, err := c.os.Search(ctx, &opensearchapi.SearchReq{
		Indices: []string{string(index)},
		Body:    bytes.NewReader(body),
	})
	if err != nil {
		return nil, fmt.Errorf("searchclient: Search request: %w", err)
	}
	defer resp.Body.Close()

	if resp.IsError() {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("searchclient: Search failed [%d]: %s", resp.StatusCode, raw)
	}

	var result struct {
		Hits struct {
			Hits []struct {
				ID     string         `json:"_id"`
				Score  float64        `json:"_score"`
				Source map[string]any `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("searchclient: Search decode response: %w", err)
	}

	out := make([]SearchResult, 0, len(result.Hits.Hits))
	for _, h := range result.Hits.Hits {
		out = append(out, SearchResult{
			ID:    h.ID,
			Score: h.Score,
			Body:  h.Source,
		})
	}
	return out, nil
}
