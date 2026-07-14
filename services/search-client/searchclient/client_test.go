//go:build integration

// Integration tests for searchclient.
//
// Requirements:
//   - A running OpenSearch node at OPENSEARCH_URL (default http://localhost:9200)
//   - Run: go test ./searchclient/... -v -tags=integration
//
// These tests verify three contracts:
//  1. Round-trip: a doc indexed is retrievable by keyword.
//  2. Tenant isolation: a tenant-A doc must not appear in a tenant-B search.
//  3. Empty TenantID is rejected before any network call is made.
package searchclient_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/search-client/searchclient"
)

func opensearchURL() string {
	if u := os.Getenv("OPENSEARCH_URL"); u != "" {
		return u
	}
	return "http://localhost:9200"
}

func newTestClient(t *testing.T) searchclient.Client {
	t.Helper()
	c, err := searchclient.New(searchclient.Config{
		Addresses: []string{opensearchURL()},
	})
	require.NoError(t, err)
	return c
}

// uniqueIndex returns a per-test index name so tests don't bleed into each other.
func uniqueIndex(t *testing.T) searchclient.IndexName {
	t.Helper()
	return searchclient.IndexName(fmt.Sprintf("test-obligations-%d", time.Now().UnixNano()))
}

// TestPutGet_RoundTrip verifies that a document indexed is retrievable by keyword.
func TestPutGet_RoundTrip(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	idx := uniqueIndex(t)

	require.NoError(t, c.EnsureIndex(ctx, idx))

	doc := searchclient.Document{
		ID:            "ob-round-trip-001",
		TenantID:      "tenant-alpha",
		LegalEntityID: "le-001",
		Body: map[string]any{
			"obligation_code": "GST-Q4-2024",
			"obligation_type": "TAX_PAYMENT",
			"source_reference": "ATO GST Filing Rule 2024-Q4",
			"status":          "OPEN",
		},
	}
	require.NoError(t, c.Index(ctx, idx, doc))

	results, err := c.Search(ctx, idx, searchclient.SearchQuery{
		TenantID: "tenant-alpha",
		Keywords: "GST-Q4-2024",
	})
	require.NoError(t, err)
	require.Len(t, results, 1, "expected exactly one result")

	assert.Equal(t, "ob-round-trip-001", results[0].ID)
	assert.Equal(t, "tenant-alpha", results[0].Body["tenant_id"])
	assert.Equal(t, "GST-Q4-2024", results[0].Body["obligation_code"])
}

// TestSearch_TenantIsolation verifies that Tenant B's keyword cannot surface
// Tenant A's documents and vice versa.
func TestSearch_TenantIsolation(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	idx := uniqueIndex(t)

	require.NoError(t, c.EnsureIndex(ctx, idx))

	// Index two obligations for different tenants, same keywords.
	docA := searchclient.Document{
		ID:            "ob-tenant-a-001",
		TenantID:      "tenant-alpha",
		LegalEntityID: "le-a01",
		Body: map[string]any{
			"obligation_code":  "PAYROLL-2024-JAN",
			"obligation_type":  "PAYROLL",
			"source_reference": "Payroll Act Section 12",
		},
	}
	docB := searchclient.Document{
		ID:            "ob-tenant-b-001",
		TenantID:      "tenant-beta",
		LegalEntityID: "le-b01",
		Body: map[string]any{
			"obligation_code":  "PAYROLL-2024-JAN",
			"obligation_type":  "PAYROLL",
			"source_reference": "Payroll Act Section 12",
		},
	}
	require.NoError(t, c.Index(ctx, idx, docA))
	require.NoError(t, c.Index(ctx, idx, docB))

	// Tenant Alpha search must return ONLY Tenant Alpha's document.
	resultsA, err := c.Search(ctx, idx, searchclient.SearchQuery{
		TenantID: "tenant-alpha",
		Keywords: "PAYROLL",
	})
	require.NoError(t, err)
	require.Len(t, resultsA, 1, "expected only tenant-alpha's doc")
	assert.Equal(t, "ob-tenant-a-001", resultsA[0].ID)

	// Tenant Beta search must return ONLY Tenant Beta's document.
	resultsB, err := c.Search(ctx, idx, searchclient.SearchQuery{
		TenantID: "tenant-beta",
		Keywords: "PAYROLL",
	})
	require.NoError(t, err)
	require.Len(t, resultsB, 1, "expected only tenant-beta's doc")
	assert.Equal(t, "ob-tenant-b-001", resultsB[0].ID)
}

// TestSearch_EmptyTenantIDIsRejected verifies that Search returns
// ErrTenantIDRequired before making any network call when TenantID is empty.
func TestSearch_EmptyTenantIDIsRejected(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)

	_, err := c.Search(ctx, searchclient.IndexObligations, searchclient.SearchQuery{
		TenantID: "", // deliberately empty
		Keywords: "anything",
	})
	require.Error(t, err)
	assert.True(t,
		errors.Is(err, searchclient.ErrTenantIDRequired),
		"expected ErrTenantIDRequired, got: %v", err,
	)
}
