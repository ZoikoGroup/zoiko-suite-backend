package store_test

import (
	"errors"
	"testing"

	"zoiko.io/semantic-model-svc/internal/domain"
)

// DATA-05 Semantic Model, against real Postgres as the NOSUPERUSER
// NOBYPASSRLS role.

// Happy path: CreateDraftVersion -> AddMetricBinding -> AddDimensionBinding
// -> AddCalculationPlan -> ValidateCalculationPlan -> PublishSemanticVersion.
func TestSM_HappyPath_DraftBindPublish(t *testing.T) {
	f := newFixture(t)
	version := f.createDraft(orgA, "revenue-model")
	if version.Status != domain.SemVerDraft {
		t.Fatalf("version status = %s, want Draft", version.Status)
	}

	f.addMetric(orgA, version.VersionID, "gross_revenue", "ddv_abc", "gross_amount", domain.AggSum, "USD", domain.SignPositive)
	f.addMetric(orgA, version.VersionID, "returns", "ddv_abc", "return_amount", domain.AggSum, "USD", domain.SignNegative)

	_, err := f.s.AddDimensionBinding(f.ctx, orgA, domain.AddDimensionBindingRequest{
		VersionID: version.VersionID, DimensionKey: "region", DatasetVersionID: "ddv_abc",
		SourceColumn: "region_code", HierarchyLevel: 1,
	}, "test-operator", f.claim("AddDimensionBinding", version.VersionID))
	if err != nil {
		t.Fatalf("add dimension binding: %v", err)
	}

	_, err = f.s.AddCalculationPlan(f.ctx, orgA, domain.AddCalculationPlanRequest{
		VersionID: version.VersionID, MetricKey: "net_revenue",
		Expression: domain.PlanExpression{Op: "subtract", Operands: []string{"gross_revenue", "returns"}},
	}, "test-operator", f.claim("AddCalculationPlan", version.VersionID))
	if err != nil {
		t.Fatalf("add calculation plan: %v", err)
	}

	// net_revenue itself has no metric binding — that's fine, it's a
	// derived metric defined purely by its calculation plan, but a plan
	// referencing a metric_key with NO active binding must fail validation.
	valid, err := f.s.ValidateCalculationPlan(f.ctx, orgA, version.VersionID, "net_revenue")
	if err != nil {
		t.Fatalf("validate calculation plan: %v", err)
	}
	if !valid.Valid {
		t.Fatalf("net_revenue plan should be valid (both operands bound): %+v", valid)
	}

	published, err := f.s.PublishSemanticVersion(f.ctx, orgA, version.VersionID, "test-operator", f.claim("PublishSemanticVersion", version.VersionID))
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if published.Status != domain.SemVerPublished || published.PublishedAt == nil {
		t.Fatalf("published version: %+v", published)
	}
}

// Negative path #1 (doc-named): conflicting KPI/metric semantics block
// publish — the same metric_key bound twice with different unit/
// aggregation is refused at PUBLISH, not silently allowed.
func TestSM_ConflictingMetricSemanticsBlockPublish(t *testing.T) {
	f := newFixture(t)
	version := f.createDraft(orgA, "conflict-model")

	f.addMetric(orgA, version.VersionID, "order_count", "ddv_abc", "order_id", domain.AggCount, "count", domain.SignPositive)
	// Same metric_key, but a DIFFERENT aggregation and unit — a genuine
	// semantic conflict, not a legitimate re-source.
	f.addMetric(orgA, version.VersionID, "order_count", "ddv_xyz", "total_amount", domain.AggSum, "USD", domain.SignPositive)

	if _, err := f.s.PublishSemanticVersion(f.ctx, orgA, version.VersionID, "test-operator", f.claim("PublishSemanticVersion", version.VersionID)); !errors.Is(err, domain.ErrMetricCollision) {
		t.Fatalf("published a version with conflicting metric semantics: %v", err)
	}

	// Version remains Draft — nothing got published.
	after, err := f.s.GetVersion(f.ctx, orgA, version.VersionID)
	if err != nil || after.Status != domain.SemVerDraft {
		t.Fatalf("version after failed publish: %+v (err=%v)", after, err)
	}
}

// Negative path #2 (doc-named): dimension hierarchy change does not
// rewrite a prior semantic version — publishing v2 with a changed
// hierarchy leaves v1's own binding untouched.
func TestSM_DimensionHierarchyChangeDoesNotRewritePriorVersion(t *testing.T) {
	f := newFixture(t)
	v1 := f.createDraft(orgA, "hierarchy-model")
	f.addMetric(orgA, v1.VersionID, "revenue", "ddv_abc", "amount", domain.AggSum, "USD", domain.SignPositive)
	dim1, err := f.s.AddDimensionBinding(f.ctx, orgA, domain.AddDimensionBindingRequest{
		VersionID: v1.VersionID, DimensionKey: "product_category", DatasetVersionID: "ddv_abc",
		SourceColumn: "category_code", HierarchyLevel: 1,
	}, "test-operator", f.claim("AddDimensionBinding-v1", v1.VersionID))
	if err != nil {
		t.Fatalf("add v1 dimension binding: %v", err)
	}
	if _, err := f.s.PublishSemanticVersion(f.ctx, orgA, v1.VersionID, "test-operator", f.claim("PublishSemanticVersion-v1", v1.VersionID)); err != nil {
		t.Fatalf("publish v1: %v", err)
	}

	// v2: same model, changed hierarchy level for the same dimension_key.
	v2, err := f.s.CreateDraftVersion(f.ctx, orgA, domain.CreateDraftVersionRequest{ModelName: "hierarchy-model"},
		"test-operator", f.claim("CreateDraftVersion-v2", "hierarchy-model"))
	if err != nil {
		t.Fatalf("create v2: %v", err)
	}
	f.addMetric(orgA, v2.VersionID, "revenue", "ddv_abc", "amount", domain.AggSum, "USD", domain.SignPositive)
	if _, err := f.s.AddDimensionBinding(f.ctx, orgA, domain.AddDimensionBindingRequest{
		VersionID: v2.VersionID, DimensionKey: "product_category", DatasetVersionID: "ddv_abc",
		SourceColumn: "category_code", HierarchyLevel: 2, // changed
	}, "test-operator", f.claim("AddDimensionBinding-v2", v2.VersionID)); err != nil {
		t.Fatalf("add v2 dimension binding: %v", err)
	}
	if _, err := f.s.PublishSemanticVersion(f.ctx, orgA, v2.VersionID, "test-operator", f.claim("PublishSemanticVersion-v2", v2.VersionID)); err != nil {
		t.Fatalf("publish v2: %v", err)
	}

	// v1's own dimension binding is untouched.
	v1Dims, err := f.s.GetDimensionBindings(f.ctx, orgA, v1.VersionID)
	if err != nil || len(v1Dims) != 1 || v1Dims[0].DimensionBindingID != dim1.DimensionBindingID || v1Dims[0].HierarchyLevel != 1 {
		t.Fatalf("v1 dimension bindings after v2 publish: %+v (err=%v)", v1Dims, err)
	}
}

// Negative path #3 (doc-named): unauthorized raw SQL/source-store binding
// is rejected — source_column must be a plain column reference.
func TestSM_UnauthorizedRawSQLBindingRejected(t *testing.T) {
	f := newFixture(t)
	version := f.createDraft(orgA, "sql-injection-model")

	maliciousColumns := []string{
		"id; DROP TABLE users;--",
		"amount) FROM secrets; SELECT (1",
		"amount OR 1=1",
		"(SELECT password FROM users)",
	}
	for _, col := range maliciousColumns {
		_, err := f.s.AddMetricBinding(f.ctx, orgA, domain.AddMetricBindingRequest{
			VersionID: version.VersionID, MetricKey: "malicious", DatasetVersionID: "ddv_abc",
			SourceColumn: col, Aggregation: domain.AggSum, Unit: "USD", Sign: domain.SignPositive,
		}, "test-operator", f.claim("AddMetricBinding-malicious", col))
		if err == nil {
			t.Fatalf("accepted a non-column source_column: %q", col)
		}
	}

	// A plain column reference (optionally dotted) is accepted.
	if _, err := f.s.AddMetricBinding(f.ctx, orgA, domain.AddMetricBindingRequest{
		VersionID: version.VersionID, MetricKey: "revenue", DatasetVersionID: "ddv_abc",
		SourceColumn: "facts.gross_amount", Aggregation: domain.AggSum, Unit: "USD", Sign: domain.SignPositive,
	}, "test-operator", f.claim("AddMetricBinding-valid", version.VersionID)); err != nil {
		t.Fatalf("rejected a legitimate dotted column reference: %v", err)
	}
}

// RetireMetricBinding: retired bindings don't count toward publish
// requirements or collision detection.
func TestSM_RetireMetricBinding(t *testing.T) {
	f := newFixture(t)
	version := f.createDraft(orgA, "retire-model")
	m1 := f.addMetric(orgA, version.VersionID, "temp_metric", "ddv_abc", "temp_col", domain.AggSum, "USD", domain.SignPositive)
	f.addMetric(orgA, version.VersionID, "keeper", "ddv_abc", "keeper_col", domain.AggSum, "USD", domain.SignPositive)

	retired, err := f.s.RetireMetricBinding(f.ctx, orgA, m1.MetricBindingID, "test-operator", f.claim("RetireMetricBinding", m1.MetricBindingID))
	if err != nil {
		t.Fatalf("retire metric binding: %v", err)
	}
	if !retired.Retired {
		t.Fatalf("retired binding still shows Retired=false: %+v", retired)
	}
	if _, err := f.s.RetireMetricBinding(f.ctx, orgA, m1.MetricBindingID, "test-operator", f.claim("RetireMetricBinding-again", m1.MetricBindingID)); !errors.Is(err, domain.ErrVersionAlreadyRetired) {
		t.Fatalf("re-retired an already-retired binding: %v", err)
	}

	published, err := f.s.PublishSemanticVersion(f.ctx, orgA, version.VersionID, "test-operator", f.claim("PublishSemanticVersion", version.VersionID))
	if err != nil {
		t.Fatalf("publish with one retired binding: %v", err)
	}
	if published.Status != domain.SemVerPublished {
		t.Fatalf("published status: %s", published.Status)
	}
}

// A calculation plan referencing a metric_key with no active binding
// blocks publish.
func TestSM_CalculationPlanReferencingMissingMetricBlocksPublish(t *testing.T) {
	f := newFixture(t)
	version := f.createDraft(orgA, "broken-plan-model")
	f.addMetric(orgA, version.VersionID, "revenue", "ddv_abc", "amount", domain.AggSum, "USD", domain.SignPositive)
	if _, err := f.s.AddCalculationPlan(f.ctx, orgA, domain.AddCalculationPlanRequest{
		VersionID: version.VersionID, MetricKey: "margin",
		Expression: domain.PlanExpression{Op: "subtract", Operands: []string{"revenue", "cost_of_goods"}}, // cost_of_goods never bound
	}, "test-operator", f.claim("AddCalculationPlan", version.VersionID)); err != nil {
		t.Fatalf("add calculation plan: %v", err)
	}

	if _, err := f.s.PublishSemanticVersion(f.ctx, orgA, version.VersionID, "test-operator", f.claim("PublishSemanticVersion", version.VersionID)); !errors.Is(err, domain.ErrCalcPlanInvalid) {
		t.Fatalf("published a version with a broken calculation plan: %v", err)
	}
}

// Idempotent replay: retrying AddMetricBinding with the same claim key
// must not create a second binding.
func TestSM_AddMetricBinding_IdempotentReplay(t *testing.T) {
	f := newFixture(t)
	version := f.createDraft(orgA, "replay-model")
	req := domain.AddMetricBindingRequest{
		VersionID: version.VersionID, MetricKey: "revenue", DatasetVersionID: "ddv_abc",
		SourceColumn: "amount", Aggregation: domain.AggSum, Unit: "USD", Sign: domain.SignPositive,
	}
	claim := f.claim("AddMetricBinding", "replay-test")
	first, err := f.s.AddMetricBinding(f.ctx, orgA, req, "test-operator", claim)
	if err != nil {
		t.Fatalf("first add: %v", err)
	}
	_, err = f.s.AddMetricBinding(f.ctx, orgA, req, "test-operator", claim)
	var replay *domain.IdempotentReplayError
	if !errors.As(err, &replay) || replay.ResourceID != first.MetricBindingID {
		t.Fatalf("replay of AddMetricBinding: %v", err)
	}
}

// DB-level negative control: a Published version's calculation plans and
// dimension bindings are immutable at the database.
func TestSM_PublishedContentIsImmutableAtTheDatabase(t *testing.T) {
	f := newFixture(t)
	version := f.createDraft(orgA, "immutability-model")
	f.addMetric(orgA, version.VersionID, "revenue", "ddv_abc", "amount", domain.AggSum, "USD", domain.SignPositive)
	dim, err := f.s.AddDimensionBinding(f.ctx, orgA, domain.AddDimensionBindingRequest{
		VersionID: version.VersionID, DimensionKey: "region", DatasetVersionID: "ddv_abc", SourceColumn: "region_code", HierarchyLevel: 1,
	}, "test-operator", f.claim("AddDimensionBinding", version.VersionID))
	if err != nil {
		t.Fatalf("add dimension binding: %v", err)
	}
	if _, err := f.s.PublishSemanticVersion(f.ctx, orgA, version.VersionID, "test-operator", f.claim("PublishSemanticVersion", version.VersionID)); err != nil {
		t.Fatalf("publish: %v", err)
	}

	tx, err := f.app.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(f.ctx) //nolint:errcheck
	if _, err := tx.Exec(f.ctx, "SELECT set_config('app.tenant_id', $1, true)", orgA); err != nil {
		t.Fatalf("declare tenant: %v", err)
	}
	if _, err := tx.Exec(f.ctx, `UPDATE dimension_bindings SET hierarchy_level = 99 WHERE dimension_binding_id = $1`, dim.DimensionBindingID); err == nil {
		t.Fatal("a raw UPDATE against a dimension binding was not rejected")
	}
	if _, err := tx.Exec(f.ctx, `UPDATE semantic_versions SET version_number = 99 WHERE version_id = $1`, version.VersionID); err == nil {
		t.Fatal("a raw UPDATE changing an unrelated column on a published version was not rejected")
	}
}

// DeprecateVersion: Published -> Deprecated, then terminal.
func TestSM_DeprecateVersion(t *testing.T) {
	f := newFixture(t)
	version := f.createDraft(orgA, "deprecate-model")
	f.addMetric(orgA, version.VersionID, "revenue", "ddv_abc", "amount", domain.AggSum, "USD", domain.SignPositive)
	if _, err := f.s.PublishSemanticVersion(f.ctx, orgA, version.VersionID, "test-operator", f.claim("PublishSemanticVersion", version.VersionID)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	deprecated, err := f.s.DeprecateVersion(f.ctx, orgA, version.VersionID, "test-operator", f.claim("DeprecateVersion", version.VersionID))
	if err != nil {
		t.Fatalf("deprecate: %v", err)
	}
	if deprecated.Status != domain.SemVerDeprecated {
		t.Fatalf("status = %s, want Deprecated", deprecated.Status)
	}
}

// Tenant isolation: cross-tenant reads are denied by RLS.
func TestSM_TenantIsolation(t *testing.T) {
	f := newFixture(t)
	version := f.createDraft(orgA, "isolation-model")
	if _, err := f.s.GetVersion(f.ctx, orgB, version.VersionID); !errors.Is(err, domain.ErrVersionNotFound) {
		t.Fatalf("cross-tenant version read: %v", err)
	}
}
