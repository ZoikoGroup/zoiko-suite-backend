package lineage

import (
	"time"

	"zoiko.io/contract/types"
)

// NodeType represents the semantic role of a lineage node (§8, §28).
type NodeType string

const (
	NodeTypeEntity      NodeType = "ENTITY"       // Business object or database table/record
	NodeTypeActivity    NodeType = "ACTIVITY"     // Transformation pipeline, batch calculation, job
	NodeTypeAgent       NodeType = "AGENT"        // Service, workflow operator, automated worker (DG-023)
	NodeTypeReportField NodeType = "REPORT_FIELD" // Granular financial/statutory report line/field (DG-027)
)

// LineageNode models an entity, activity, agent, or report field in the provenance graph (§28).
type LineageNode struct {
	NodeID    types.UUID `json:"node_id"`
	TenantID  types.UUID `json:"tenant_id"`
	NodeType  NodeType   `json:"node_type"`
	NodeCode  string     `json:"node_code"`
	NodeName  string     `json:"node_name"`
	DomainID  types.UUID `json:"domain_id"`
	Metadata  string     `json:"metadata,omitempty"` // JSON attributes
	CreatedAt time.Time  `json:"created_at"`
}

// ProvenanceAssertion records the authoritative source, activity, rule version, and agent for a business fact (§8, §28, DG-021..DG-023).
type ProvenanceAssertion struct {
	AssertionID                types.UUID `json:"assertion_id"`
	TenantID                   types.UUID `json:"tenant_id"`
	SubjectEntityType          string     `json:"subject_entity_type"`
	SubjectEntityID            types.UUID `json:"subject_entity_id"`
	SourceActivityID           types.UUID `json:"source_activity_id"`
	AgentPrincipalID           string     `json:"agent_principal_id"`           // Service or user responsible (DG-023)
	TransformationRuleVersion  string     `json:"transformation_rule_version"` // Version of formula/code applied (DG-022, NP-08)
	SourceHashes               string     `json:"source_hashes"`               // JSON map of source object IDs to digests
	SystemRecordedAt           time.Time  `json:"system_recorded_at"`
	ValidFrom                  time.Time  `json:"valid_from"`
	ValidTo                    *time.Time `json:"valid_to,omitempty"`
}

// Edge represents a directed provenance relationship between two lineage nodes.
type Edge struct {
	EdgeID             types.UUID `json:"edge_id"`
	TenantID           types.UUID `json:"tenant_id"`
	SourceNodeID       types.UUID `json:"source_node_id"`
	TargetNodeID       types.UUID `json:"target_node_id"`
	RelationshipType   string     `json:"relationship_type"`   // e.g. "DERIVED_FROM", "AGGREGATED_INTO", "ATTRIBUTED_TO"
	TransformationRule string     `json:"transformation_rule"` // e.g. "MAP_TAX_LINE_V1"
	RuleVersion        string     `json:"rule_version"`        // Version-pinned transformation (DG-022)
	ValidFrom          time.Time  `json:"valid_from"`
	ValidTo            *time.Time `json:"valid_to,omitempty"`
	SystemRecordedAt   time.Time  `json:"system_recorded_at"`
}
