// Package domain defines the authoritative domain types for
// global-search-svc (DATA-06, ZS-SVC-N-001 §4). This service searches
// permitted business objects and evidence without leaking unauthorized
// existence, counts or snippets. It never owns authoritative records,
// permission decisions, or semantic KPI definitions — indexed content is
// always a derived, disposable projection, never evidence of record.
package domain

import (
	"fmt"
	"time"
)

const (
	PrefixIndex    = "dsi_"
	PrefixDocument = "did_"
	PrefixPolicy   = "dsp_"
)

type errorString string

func (e errorString) Error() string { return string(e) }

type IdempotentReplayError struct {
	ResourceID string
}

func (e *IdempotentReplayError) Error() string {
	return fmt.Sprintf("idempotent replay: resource %s already exists", e.ResourceID)
}

type IdempotencyClaim struct {
	OwnerScope    string
	PrincipalID   string
	Key           string
	Operation     string
	RequestSHA256 string
	ResourceID    string
}

const SellerScope = "seller"

// ── SearchIndex ──────────────────────────────────────────────────────────────

type SearchIndexStatus string

const (
	IndexIndexing   SearchIndexStatus = "Indexing"
	IndexAvailable  SearchIndexStatus = "Available"
	IndexRebuilding SearchIndexStatus = "Rebuilding"
)

// SearchIndex is one named, tenant-scoped index scope (e.g. "invoices").
// "Degraded" from the doc's lifecycle is deliberately NOT a stored status
// here — it's a computed, read-time signal (see Freshness/GetIndexHealth),
// the same "report honestly at read time, don't mutate state to record a
// transient condition" doctrine already used for DATA-04's freshness.
type SearchIndex struct {
	IndexID   string            `json:"index_id"`
	TenantID  string            `json:"tenant_id"`
	Scope     string            `json:"scope"`
	Status    SearchIndexStatus `json:"status"`
	CreatedAt time.Time         `json:"created_at"`
	CreatedBy string            `json:"created_by"`
}

// ── SearchPolicy ─────────────────────────────────────────────────────────────

// SearchPolicy governs one scope's bounded-staleness requirement — what
// "stale" means for ValidateSearchPolicy / GetIndexHealth to report
// honestly against.
type SearchPolicy struct {
	PolicyID            string    `json:"policy_id"`
	TenantID            string    `json:"tenant_id"`
	Scope               string    `json:"scope"`
	MaxStalenessSeconds int64     `json:"max_staleness_seconds"`
	CreatedAt           time.Time `json:"created_at"`
	CreatedBy           string    `json:"created_by"`
}

type SetSearchPolicyRequest struct {
	Scope               string `json:"scope"`
	MaxStalenessSeconds int64  `json:"max_staleness_seconds"`
}

func (r SetSearchPolicyRequest) Validate() error {
	if r.Scope == "" {
		return fmt.Errorf("scope is required")
	}
	if r.MaxStalenessSeconds <= 0 {
		return fmt.Errorf("max_staleness_seconds must be positive")
	}
	return nil
}

// ── IndexDocument ────────────────────────────────────────────────────────────

// IndexDocument is one indexed object. When Restricted is true, only
// principals listed in AllowedPrincipalIDs may ever see it — in search
// results, in counts, in autocomplete suggestions, or via ExplainResult.
// This is enforced by the SQL WHERE clause itself (authorization BEFORE
// retrieval), never by fetching everything and filtering in application
// code afterward.
type IndexDocument struct {
	DocID               string    `json:"doc_id"`
	TenantID            string    `json:"tenant_id"`
	IndexID             string    `json:"index_id"`
	ObjectType          string    `json:"object_type"`
	ObjectRef           string    `json:"object_ref"`
	ContentText         string    `json:"content_text"`
	ContentHash         string    `json:"content_hash"`
	Classification      string    `json:"classification"`
	ResidencyRegion     string    `json:"residency_region"`
	Restricted          bool      `json:"restricted"`
	AllowedPrincipalIDs []string  `json:"allowed_principal_ids,omitempty"`
	SourceUpdatedAt     time.Time `json:"source_updated_at"`
	IndexedAt           time.Time `json:"indexed_at"`
	IndexedBy           string    `json:"indexed_by"`
}

type IndexObjectRequest struct {
	Scope               string    `json:"scope"`
	ObjectType          string    `json:"object_type"`
	ObjectRef           string    `json:"object_ref"`
	ContentText         string    `json:"content_text"`
	ContentHash         string    `json:"content_hash"`
	Classification      string    `json:"classification"`
	ResidencyRegion     string    `json:"residency_region"`
	Restricted          bool      `json:"restricted"`
	AllowedPrincipalIDs []string  `json:"allowed_principal_ids,omitempty"`
	SourceUpdatedAt     time.Time `json:"source_updated_at"`
}

func (r IndexObjectRequest) Validate() error {
	if r.Scope == "" {
		return fmt.Errorf("scope is required")
	}
	if r.ObjectType == "" {
		return fmt.Errorf("object_type is required")
	}
	if r.ObjectRef == "" {
		return fmt.Errorf("object_ref is required")
	}
	if r.ContentText == "" {
		return fmt.Errorf("content_text is required")
	}
	if r.ContentHash == "" {
		return fmt.Errorf("content_hash is required")
	}
	if r.Classification == "" {
		return fmt.Errorf("classification is required")
	}
	if r.ResidencyRegion == "" {
		return fmt.Errorf("residency_region is required")
	}
	if r.Restricted && len(r.AllowedPrincipalIDs) == 0 {
		return fmt.Errorf("allowed_principal_ids is required when restricted is true")
	}
	if r.SourceUpdatedAt.IsZero() {
		return fmt.Errorf("source_updated_at is required")
	}
	return nil
}

type PurgeIndexObjectRequest struct {
	Scope     string `json:"scope"`
	ObjectRef string `json:"object_ref"`
}

func (r PurgeIndexObjectRequest) Validate() error {
	if r.Scope == "" {
		return fmt.Errorf("scope is required")
	}
	if r.ObjectRef == "" {
		return fmt.Errorf("object_ref is required")
	}
	return nil
}

// ── Search / read surfaces ──────────────────────────────────────────────────

// SearchResult is one matched document's caller-visible projection — a
// snippet, never the full content_text, and only ever produced for
// documents the query already filtered to authorized ones.
type SearchResult struct {
	DocID      string  `json:"doc_id"`
	ObjectType string  `json:"object_type"`
	ObjectRef  string  `json:"object_ref"`
	Snippet    string  `json:"snippet"`
	Rank       float64 `json:"rank"`
}

type SearchResponse struct {
	Results []SearchResult `json:"results"`
	Count   int            `json:"count"`
}

// SearchResultEvidence is ExplainResult's answer: what a caller is
// entitled to know about WHY a document is in the index and how fresh it
// is — never returned for a document the requesting principal cannot see
// (refused as not-found, not forbidden, so the refusal itself carries no
// information about the object's existence).
type SearchResultEvidence struct {
	DocID           string    `json:"doc_id"`
	ObjectType      string    `json:"object_type"`
	ObjectRef       string    `json:"object_ref"`
	Classification  string    `json:"classification"`
	ContentHash     string    `json:"content_hash"`
	SourceUpdatedAt time.Time `json:"source_updated_at"`
	IndexedAt       time.Time `json:"indexed_at"`
}

// IndexHealth is GetIndexHealth's computed, read-time report — includes
// the "Degraded" signal the doc's lifecycle names, derived from freshness
// against the scope's SearchPolicy rather than stored as mutable state.
type IndexHealth struct {
	Scope                 string            `json:"scope"`
	Status                SearchIndexStatus `json:"status"`
	DocumentCount         int64             `json:"document_count"`
	OldestSourceUpdatedAt *time.Time        `json:"oldest_source_updated_at,omitempty"`
	StalenessSeconds      int64             `json:"staleness_seconds"`
	MaxStalenessSeconds   int64             `json:"max_staleness_seconds"`
	Degraded              bool              `json:"degraded"`
}

var (
	ErrIndexNotFound        = errorString("search index not found")
	ErrPolicyNotFound       = errorString("search policy not found")
	ErrDocumentNotFound     = errorString("index document not found")
	ErrIdempotencyKeyReused = errorString("idempotency key was already used for a different request")
)
