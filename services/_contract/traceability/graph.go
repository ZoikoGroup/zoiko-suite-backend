package traceability

import (
	"errors"
	"fmt"
	"time"

	"zoiko.io/contract/types"
)

// EvidenceEvent represents an immutable, attributable audit/control evidence record (GOV-EV).
// In accordance with ZS-DATA-001 Section 4 (Plane P2) & Section 23 (Stage 8):
// Evidence is append-only, tamper-evident, and separate from operational logs.
type EvidenceEvent struct {
	EvidenceEventID     types.UUID `json:"evidence_event_id"`
	TenantID            types.UUID `json:"tenant_id"`
	LegalEntityID       types.UUID `json:"legal_entity_id"`
	SubjectObjectType   string     `json:"subject_object_type"`
	SubjectObjectID     types.UUID `json:"subject_object_id"`
	EventType           string     `json:"event_type"` // e.g. "APPROVAL_GRANTED", "FILING_SUBMITTED"
	ActorPrincipalID    string     `json:"actor_principal_id"`
	ActorRole           string     `json:"actor_role"`
	PolicyVersionID     string     `json:"policy_version_id,omitempty"`
	PayloadHash         string     `json:"payload_hash"` // SHA-256
	AuthorityReceiptRef string     `json:"authority_receipt_ref,omitempty"`
	OccurredAt          time.Time  `json:"occurred_at"`
}

// LineageNode represents a node in the 8-stage Source-to-Report graph.
type LineageNode struct {
	Stage         types.TraceStage `json:"stage"`
	ObjectType    string           `json:"object_type"`
	ObjectID      types.UUID       `json:"object_id"`
	ObjectVersion int              `json:"object_version"`
}

// LineageGraph manages the attributable provenance edges connecting business records.
type LineageGraph struct {
	edges []types.LineageEdge
}

// NewLineageGraph creates an empty lineage graph.
func NewLineageGraph() *LineageGraph {
	return &LineageGraph{edges: make([]types.LineageEdge, 0)}
}

// AddEdge inserts a validated lineage edge into the graph.
func (g *LineageGraph) AddEdge(edge types.LineageEdge) error {
	if err := edge.Validate(); err != nil {
		return fmt.Errorf("invalid lineage edge: %w", err)
	}
	g.edges = append(g.edges, edge)
	return nil
}

// TraceBackward performs a backward search from a destination object (e.g. a ReportRun or BalanceSnapshot)
// traversing the lineage graph back to the original source document (Auditability Test ZS-DATA-001 §23).
func (g *LineageGraph) TraceBackward(targetObjectID types.UUID) ([]LineageNode, error) {
	if targetObjectID.IsNil() {
		return nil, errors.New("cannot trace backward from nil object ID")
	}

	var path []LineageNode
	visited := make(map[types.UUID]bool)
	queue := []types.UUID{targetObjectID}

	for len(queue) > 0 {
		currID := queue[0]
		queue = queue[1:]

		if visited[currID] {
			continue
		}
		visited[currID] = true

		// Find edges where ToObjectID == currID
		foundIncoming := false
		for _, e := range g.edges {
			if e.ToObjectID == currID {
				foundIncoming = true
				path = append(path, LineageNode{
					Stage:         e.FromStage,
					ObjectType:    e.FromObjectType,
					ObjectID:      e.FromObjectID,
					ObjectVersion: e.FromObjectVersion,
				})
				queue = append(queue, e.FromObjectID)
			}
		}

		if !foundIncoming && len(path) == 0 {
			// If target node itself has no parents yet, record it
			for _, e := range g.edges {
				if e.FromObjectID == currID {
					path = append(path, LineageNode{
						Stage:         e.FromStage,
						ObjectType:    e.FromObjectType,
						ObjectID:      e.FromObjectID,
						ObjectVersion: e.FromObjectVersion,
					})
					break
				}
			}
		}
	}

	return path, nil
}
