package searchclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	opensearch "github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// Projection is one ProjectionDocument (§2.1) as it is written to a generation
// index: the registered domain fields, plus the governance lineage the query
// and retrieval layers filter and re-authorize on.
//
// Every field here except Fields is control-plane-owned. A source can never
// set them, because they are merged over the domain body at write time rather
// than under it — INV-02 ("every projection document carries trusted tenant
// identity") is a property of this merge order, not of the caller's goodwill.
type Projection struct {
	// DocID is the OpenSearch _id and therefore the idempotency key.
	// Derived from tenant + source_type + source_id by ProjectionDocID so two
	// tenants that legitimately share a source id cannot collide — which a
	// bare source id would allow, and which would be a cross-tenant overwrite
	// rather than a duplicate.
	DocID string

	TenantID        string
	LegalEntityID   string
	ResidencyRegion string
	SourceType      string
	SourceID        string
	SourceVersion   int64
	// RestrictionEpoch is monotonic per source_ref. A write whose epoch is
	// older than what is already indexed is refused by the caller, which is
	// how INV-18 / NP-11 / NP-48 ("older replay event arrives after deletion
	// tombstone → tombstone wins; no resurrection") is enforced.
	RestrictionEpoch int64
	SensitivityClass string
	RetrievalClass   string
	ACLRefs          []string
	IndexGeneration  string
	ContentHash      string
	Tombstoned       bool
	TombstoneReason  string
	TombstoneSource  string

	// Fields holds only fields the published contract registered. Anything
	// else is a contract violation the caller must quarantine before reaching
	// here; the strict mapping is the second line of defence, not the first.
	Fields map[string]any
}

// ProjectionDocID is the canonical _id for a projection.
//
// tenant first, deliberately. An id that began with the source id would let a
// mis-set tenant overwrite another tenant's document at the same source id; an
// id that begins with the tenant cannot, whatever the rest says.
func ProjectionDocID(tenantID, sourceType, sourceID string) string {
	return tenantID + ":" + sourceType + ":" + sourceID
}

// document renders the projection as the JSON body OpenSearch stores.
func (p Projection) document() map[string]any {
	doc := make(map[string]any, len(p.Fields)+len(GovernanceProperties()))
	for k, v := range p.Fields {
		doc[k] = v
	}

	// Governance last: a source field named tenant_id loses to the trusted
	// value rather than replacing it.
	doc["tenant_id"] = p.TenantID
	doc["legal_entity_id"] = p.LegalEntityID
	doc["source_type"] = p.SourceType
	doc["source_id"] = p.SourceID
	doc["source_version"] = p.SourceVersion
	doc["projection_version"] = p.SourceVersion
	doc["restriction_epoch"] = p.RestrictionEpoch
	doc["index_generation"] = p.IndexGeneration
	doc["indexed_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	doc["tombstoned"] = p.Tombstoned

	if p.ResidencyRegion != "" {
		doc["residency_region"] = p.ResidencyRegion
	}
	if p.SensitivityClass != "" {
		doc["sensitivity_class"] = p.SensitivityClass
	}
	if p.RetrievalClass != "" {
		doc["retrieval_class"] = p.RetrievalClass
	}
	if len(p.ACLRefs) > 0 {
		doc["acl_refs"] = p.ACLRefs
	}
	if p.ContentHash != "" {
		doc["content_hash"] = p.ContentHash
	}
	if p.TombstoneReason != "" {
		doc["tombstone_reason"] = p.TombstoneReason
	}
	if p.TombstoneSource != "" {
		doc["tombstone_source_event"] = p.TombstoneSource
	}
	return doc
}

// IndexProjection upserts one projection into a concrete generation index.
//
// Refresh is "wait_for", not "true". "true" forces a segment flush per
// document, which on a real indexing stream is the single most expensive thing
// a caller can ask OpenSearch to do; "wait_for" still guarantees the document
// is visible when this call returns — which the restriction-propagation
// verification in §8.2 depends on, since it proves invisibility by searching
// for the thing it just removed.
func (c *client) IndexProjection(ctx context.Context, physicalIndex string, p Projection) error {
	if p.DocID == "" {
		return errors.New("searchclient: IndexProjection: DocID must not be empty")
	}
	if p.TenantID == "" {
		return ErrTenantIDRequired
	}
	if p.SourceType == "" || p.SourceID == "" {
		return errors.New("searchclient: IndexProjection: SourceType and SourceID must not be empty")
	}

	body, err := json.Marshal(p.document())
	if err != nil {
		return fmt.Errorf("searchclient: IndexProjection marshal: %w", err)
	}

	resp, err := c.os.Index(ctx, opensearchapi.IndexReq{
		Index:      physicalIndex,
		DocumentID: p.DocID,
		Body:       bytes.NewReader(body),
		Params:     opensearchapi.IndexParams{Refresh: "wait_for"},
	})
	if err != nil {
		if isStrictMappingError(err.Error()) {
			return fmt.Errorf("%w: %s", ErrStrictMappingRejected, err.Error())
		}
		return fmt.Errorf("searchclient: IndexProjection request: %w", err)
	}
	if raw, isErr := drain(resp.Inspect().Response); isErr {
		if isStrictMappingError(raw) {
			return fmt.Errorf("%w: %s", ErrStrictMappingRejected, raw)
		}
		return fmt.Errorf("searchclient: IndexProjection failed: %s", raw)
	}
	return nil
}

// isStrictMappingError recognises the refusal a strict mapping produces for an
// unregistered field. Separated from a generic 400 because the two need
// opposite handling: a contract violation is quarantined and reported, a
// transient 400 is a bug worth surfacing as-is.
func isStrictMappingError(s string) bool {
	return strings.Contains(s, "strict_dynamic_mapping_exception") ||
		strings.Contains(s, "mapping set to strict")
}

// DeleteProjection removes a document from a generation index.
//
// Used only where a tombstone row would be pointless — a generation being
// rebuilt, or a source type retired. The RESTRICTION path writes a tombstone
// instead (IndexProjection with Tombstoned true), because INV-20 is explicit
// that "removing a record from search is not record deletion", and a tombstone
// is what lets a later, older replay be refused rather than silently applied.
func (c *client) DeleteProjection(ctx context.Context, physicalIndex, docID string) error {
	resp, err := c.os.Document.Delete(ctx, opensearchapi.DocumentDeleteReq{
		Index:      physicalIndex,
		DocumentID: docID,
		Params:     opensearchapi.DocumentDeleteParams{Refresh: "wait_for"},
	})
	if err != nil {
		if isMissing(err) {
			return nil
		}
		return fmt.Errorf("searchclient: DeleteProjection %s/%s: %w", physicalIndex, docID, err)
	}
	if raw, isErr := drain(resp.Inspect().Response); isErr {
		if strings.Contains(raw, "not_found") {
			return nil
		}
		return fmt.Errorf("searchclient: DeleteProjection %s/%s failed: %s", physicalIndex, docID, raw)
	}
	return nil
}

// GetProjection reads one projection back by id. Returns found=false rather
// than an error when the document is absent, because "absent" is a routine and
// meaningful answer on the restriction-verification path.
func (c *client) GetProjection(ctx context.Context, physicalIndex, docID string) (map[string]any, bool, error) {
	resp, err := c.os.Document.Get(ctx, opensearchapi.DocumentGetReq{
		Index:      physicalIndex,
		DocumentID: docID,
	})
	if err != nil {
		if isMissing(err) || strings.Contains(err.Error(), "not_found") {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("searchclient: GetProjection %s/%s: %w", physicalIndex, docID, err)
	}
	if r := resp.Inspect().Response; r != nil && r.IsError() {
		_, _ = drain(r)
		if r.StatusCode == 404 {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("searchclient: GetProjection %s/%s failed [%d]", physicalIndex, docID, r.StatusCode)
	}
	if r := resp.Inspect().Response; r != nil {
		_, _ = drain(r)
	}
	if !resp.Found {
		return nil, false, nil
	}

	var source map[string]any
	if err := json.Unmarshal(resp.Source, &source); err != nil {
		return nil, false, fmt.Errorf("searchclient: GetProjection decode source: %w", err)
	}
	return source, true, nil
}

// CountProjections counts live (non-tombstoned) documents in a generation,
// optionally narrowed by exact-match terms.
//
// This is the population measure §5.3 requires for completeness certification:
// "source eligible count vs indexed live/tombstoned count by tenant/partition".
// Tombstoned documents are excluded because a tombstone is a removal, and
// counting it as populated would let NP-18 ("index build omits one tenant
// partition") pass validation on a partition made entirely of removals.
func (c *client) CountProjections(ctx context.Context, physicalIndex string, terms map[string]string) (int64, error) {
	must := []map[string]any{{"term": map[string]any{"tombstoned": false}}}
	for field, value := range terms {
		if value == "" {
			continue
		}
		must = append(must, map[string]any{"term": map[string]any{field: value}})
	}

	body, err := json.Marshal(map[string]any{
		"size":  0,
		"query": map[string]any{"bool": map[string]any{"must": must}},
		// Without this OpenSearch stops counting at 10 000 and reports
		// {"value":10000,"relation":"gte"} — a completeness check that
		// silently plateaus is worse than none, because it reads as a pass.
		"track_total_hits": true,
	})
	if err != nil {
		return 0, fmt.Errorf("searchclient: CountProjections marshal: %w", err)
	}

	resp, err := c.os.Search(ctx, &opensearchapi.SearchReq{
		Indices: []string{physicalIndex},
		Body:    bytes.NewReader(body),
	})
	if err != nil {
		if isMissing(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("searchclient: CountProjections %s: %w", physicalIndex, err)
	}
	if raw, isErr := drain(resp.Inspect().Response); isErr {
		return 0, fmt.Errorf("searchclient: CountProjections %s failed: %s", physicalIndex, raw)
	}
	return int64(resp.Hits.Total.Value), nil
}

// drain reads and closes a response body, returning its text and whether the
// status was an error.
//
// Every call path here has to do three things — read the body for the error
// message, close it, and branch on the status — and getting any one of them
// wrong leaks a connection from the pool. Doing it in one place means the
// leak cannot be reintroduced one call site at a time.
func drain(r *opensearch.Response) (string, bool) {
	if r == nil {
		return "", false
	}
	var raw []byte
	if r.Body != nil {
		raw, _ = io.ReadAll(io.LimitReader(r.Body, 8192))
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
	}
	return string(raw), r.IsError()
}
