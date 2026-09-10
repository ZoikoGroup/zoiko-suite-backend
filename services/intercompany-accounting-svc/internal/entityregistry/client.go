// Package entityregistry is ACC-11's own closure of its "Entity loses
// group relationship mid-period" negative path — a real client against
// tenant-entity-registry-svc's own EntityHierarchy authority, rather than
// leaving the check permanently unenforced for lack of anywhere to ask.
package entityregistry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"go.uber.org/zap"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
)

// Hierarchy mirrors tenant-entity-registry-svc's own domain.EntityHierarchy
// wire shape — only the fields this client needs. EffectiveTo nil means
// the relationship is currently open; a non-nil value is the date it was
// (or will be) closed.
type Hierarchy struct {
	ParentLegalEntityID string     `json:"parent_legal_entity_id"`
	ChildLegalEntityID  string     `json:"child_legal_entity_id"`
	EffectiveFrom       time.Time  `json:"effective_from"`
	EffectiveTo         *time.Time `json:"effective_to"`
}

type Client struct {
	baseURL string
	http    *http.Client
	log     *zap.Logger
}

func NewClient(baseURL string, log *zap.Logger) *Client {
	return &Client{baseURL: baseURL, log: log, http: &http.Client{Timeout: 5 * time.Second}}
}

// ListHierarchies returns every EntityHierarchy row naming legalEntityID as
// EITHER parent or child — open and closed alike, tenant-entity-registry-
// svc's own GET /entities/{id}/hierarchies applies no effective_to filter,
// so "currently open" vs "closed mid-period" is this caller's own decision
// to make, not something the registry decides on its behalf.
func (c *Client) ListHierarchies(ctx context.Context, tenantID, legalEntityID string) ([]Hierarchy, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/entities/"+url.PathEscape(legalEntityID)+"/hierarchies", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Tenant-Id", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Error("failed to query tenant-entity-registry-svc", zap.Error(err))
		return nil, domain.ErrEntityRegistryUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, domain.ErrEntityRegistryUnavailable
	}
	var list []Hierarchy
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, err
	}
	return list, nil
}

// SharesOpenGroupRelationship answers ACC-11's own negative path directly:
// does entityA still belong to the same group as entityB, as of asOf?
// Checked two ways, both against currently-open (EffectiveTo nil or after
// asOf) hierarchy rows: a direct parent/child link between the two, or a
// shared immediate parent (co-siblings under the same parent).
//
// Deliberately NOT a full arbitrary-depth group-tree walk — the spec names
// no such algorithm, and this two-hop check catches the common real case
// (two entities directly related, or both direct children of the same
// parent) without this service reimplementing tenant-entity-registry-svc's
// own hierarchy semantics. A documented scope boundary, not a silent one.
func SharesOpenGroupRelationship(aHierarchies, bHierarchies []Hierarchy, entityA, entityB string, asOf time.Time) bool {
	openParents := func(hs []Hierarchy, entity string) map[string]bool {
		parents := map[string]bool{}
		for _, h := range hs {
			if h.ChildLegalEntityID != entity {
				continue
			}
			if h.EffectiveTo != nil && !h.EffectiveTo.After(asOf) {
				continue // closed on or before asOf
			}
			if h.EffectiveFrom.After(asOf) {
				continue // not yet effective
			}
			parents[h.ParentLegalEntityID] = true
		}
		return parents
	}
	isOpen := func(hs []Hierarchy, parent, child string) bool {
		for _, h := range hs {
			if h.ParentLegalEntityID != parent || h.ChildLegalEntityID != child {
				continue
			}
			if h.EffectiveTo != nil && !h.EffectiveTo.After(asOf) {
				continue
			}
			if h.EffectiveFrom.After(asOf) {
				continue
			}
			return true
		}
		return false
	}

	// Direct parent/child, either direction.
	if isOpen(aHierarchies, entityA, entityB) || isOpen(bHierarchies, entityB, entityA) {
		return true
	}
	if isOpen(aHierarchies, entityB, entityA) || isOpen(bHierarchies, entityA, entityB) {
		return true
	}

	// Shared immediate parent.
	aParents := openParents(aHierarchies, entityA)
	bParents := openParents(bHierarchies, entityB)
	for p := range aParents {
		if bParents[p] {
			return true
		}
	}
	return false
}
