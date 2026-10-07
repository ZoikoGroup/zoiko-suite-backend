package lineage

import (
	"errors"
	"fmt"
	"time"

	"zoiko.io/contract/types"
)

var (
	// ErrIncompleteReportLineage is returned when a report field lacks verifiable upstream lineage (DG-027, NP-07).
	ErrIncompleteReportLineage = errors.New("report field lacks mandatory source-to-report lineage edge; C3 certification blocked (per NP-07/DG-027)")
)

// Graph represents an in-memory provenance and lineage graph capable of multi-hop traversal and as-of time travel.
type Graph struct {
	nodes map[types.UUID]LineageNode
	edges []Edge
}

func NewGraph() *Graph {
	return &Graph{
		nodes: make(map[types.UUID]LineageNode),
		edges: make([]Edge, 0),
	}
}

func (g *Graph) AddNode(node LineageNode) {
	g.nodes[node.NodeID] = node
}

func (g *Graph) AddEdge(edge Edge) {
	g.edges = append(g.edges, edge)
}

// TraceBackward performs a multi-hop backward traversal from targetNodeID to all upstream root sources valid as of asOf.
func (g *Graph) TraceBackward(targetNodeID types.UUID, asOf time.Time) []types.UUID {
	visited := make(map[types.UUID]bool)
	var queue []types.UUID
	var upstream []types.UUID

	queue = append(queue, targetNodeID)
	visited[targetNodeID] = true

	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]

		for _, edge := range g.edges {
			if edge.TargetNodeID == curr && isEffective(edge.ValidFrom, edge.ValidTo, asOf) {
				if !visited[edge.SourceNodeID] {
					visited[edge.SourceNodeID] = true
					upstream = append(upstream, edge.SourceNodeID)
					queue = append(queue, edge.SourceNodeID)
				}
			}
		}
	}

	return upstream
}

// TraceForward performs a multi-hop forward impact traversal from sourceNodeID to all downstream nodes valid as of asOf.
func (g *Graph) TraceForward(sourceNodeID types.UUID, asOf time.Time) []types.UUID {
	visited := make(map[types.UUID]bool)
	var queue []types.UUID
	var downstream []types.UUID

	queue = append(queue, sourceNodeID)
	visited[sourceNodeID] = true

	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]

		for _, edge := range g.edges {
			if edge.SourceNodeID == curr && isEffective(edge.ValidFrom, edge.ValidTo, asOf) {
				if !visited[edge.TargetNodeID] {
					visited[edge.TargetNodeID] = true
					downstream = append(downstream, edge.TargetNodeID)
					queue = append(queue, edge.TargetNodeID)
				}
			}
		}
	}

	return downstream
}

// VerifyReportLineageCompleteness verifies that every required report field has an upstream lineage chain to at least one source entity (DG-027, NP-07).
func (g *Graph) VerifyReportLineageCompleteness(reportFieldNodeIDs []types.UUID, asOf time.Time) error {
	for _, fieldID := range reportFieldNodeIDs {
		upstream := g.TraceBackward(fieldID, asOf)
		if len(upstream) == 0 {
			return fmt.Errorf("%w: field node %s has no upstream lineage edges", ErrIncompleteReportLineage, fieldID)
		}
	}
	return nil
}

func isEffective(from time.Time, to *time.Time, asOf time.Time) bool {
	if asOf.Before(from) {
		return false
	}
	if to != nil && !asOf.Before(*to) {
		return false
	}
	return true
}
