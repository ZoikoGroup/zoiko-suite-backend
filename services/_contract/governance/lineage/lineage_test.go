package lineage_test

import (
	"errors"
	"testing"
	"time"

	"zoiko.io/contract/governance/lineage"
	"zoiko.io/contract/types"
)

func TestLineageScenarios(t *testing.T) {
	tenantID := types.MustNewV7()
	domainID := types.MustNewV7()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	// Nodes: Source -> Activity -> Target Report Field
	invoiceID := types.MustNewV7()
	taxCalcID := types.MustNewV7()
	reportFieldID := types.MustNewV7()
	orphanFieldID := types.MustNewV7()

	g := lineage.NewGraph()
	g.AddNode(lineage.LineageNode{NodeID: invoiceID, TenantID: tenantID, NodeType: lineage.NodeTypeEntity, NodeCode: "SALES_INVOICE_001", DomainID: domainID, CreatedAt: t0})
	g.AddNode(lineage.LineageNode{NodeID: taxCalcID, TenantID: tenantID, NodeType: lineage.NodeTypeActivity, NodeCode: "VAT_CALCULATION_ENGINE", DomainID: domainID, CreatedAt: t0})
	g.AddNode(lineage.LineageNode{NodeID: reportFieldID, TenantID: tenantID, NodeType: lineage.NodeTypeReportField, NodeCode: "BOX_4_VAT_DUE", DomainID: domainID, CreatedAt: t0})
	g.AddNode(lineage.LineageNode{NodeID: orphanFieldID, TenantID: tenantID, NodeType: lineage.NodeTypeReportField, NodeCode: "BOX_7_TOTAL_PURCHASES", DomainID: domainID, CreatedAt: t0})

	// Edge 1: Invoice -> TaxCalc (version 1)
	g.AddEdge(lineage.Edge{
		EdgeID:             types.MustNewV7(),
		TenantID:           tenantID,
		SourceNodeID:       invoiceID,
		TargetNodeID:       taxCalcID,
		RelationshipType:   "PROCESSED_BY",
		TransformationRule: "VAT_RATE_LOOKUP",
		RuleVersion:        "1.0.0",
		ValidFrom:          t0,
		ValidTo:            &t1,
		SystemRecordedAt:   t0,
	})

	// Edge 1b: Invoice -> TaxCalc (version 2 after update at t1)
	g.AddEdge(lineage.Edge{
		EdgeID:             types.MustNewV7(),
		TenantID:           tenantID,
		SourceNodeID:       invoiceID,
		TargetNodeID:       taxCalcID,
		RelationshipType:   "PROCESSED_BY",
		TransformationRule: "VAT_RATE_LOOKUP",
		RuleVersion:        "2.0.0",
		ValidFrom:          t1,
		ValidTo:            nil,
		SystemRecordedAt:   t1,
	})

	// Edge 2: TaxCalc -> ReportField
	g.AddEdge(lineage.Edge{
		EdgeID:             types.MustNewV7(),
		TenantID:           tenantID,
		SourceNodeID:       taxCalcID,
		TargetNodeID:       reportFieldID,
		RelationshipType:   "AGGREGATED_INTO",
		TransformationRule: "SUM_VAT_DUE",
		RuleVersion:        "1.0.0",
		ValidFrom:          t0,
		ValidTo:            nil,
		SystemRecordedAt:   t0,
	})

	t.Run("Multi-hop backward trace", func(t *testing.T) {
		upstream := g.TraceBackward(reportFieldID, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
		if len(upstream) != 2 {
			t.Fatalf("expected 2 upstream nodes (TaxCalc, Invoice), got %d", len(upstream))
		}
	})

	t.Run("Multi-hop forward impact trace", func(t *testing.T) {
		downstream := g.TraceForward(invoiceID, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
		if len(downstream) != 2 {
			t.Fatalf("expected 2 downstream nodes (TaxCalc, ReportField), got %d", len(downstream))
		}
	})

	t.Run("NP-07: Lineage edge missing for report field blocks certification", func(t *testing.T) {
		err := g.VerifyReportLineageCompleteness([]types.UUID{reportFieldID, orphanFieldID}, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
		if !errors.Is(err, lineage.ErrIncompleteReportLineage) {
			t.Fatalf("expected ErrIncompleteReportLineage for orphan field, got: %v", err)
		}

		err = g.VerifyReportLineageCompleteness([]types.UUID{reportFieldID}, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatalf("expected complete report field to pass, got: %v", err)
		}
	})

	t.Run("NP-08: Transformation code changed retains exact prior version in historical lineage", func(t *testing.T) {
		// When querying historical lineage as-of March 2026 (before t1)
		marchUpstream := g.TraceBackward(reportFieldID, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
		if len(marchUpstream) != 2 {
			t.Fatalf("expected 2 upstream nodes in March 2026")
		}

		// When querying current lineage in August 2026 (after t1)
		augustUpstream := g.TraceBackward(reportFieldID, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
		if len(augustUpstream) != 2 {
			t.Fatalf("expected 2 upstream nodes in August 2026")
		}
	})
}
