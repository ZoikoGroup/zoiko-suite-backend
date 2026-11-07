package store_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"zoiko.io/data-ingestion-svc/internal/domain"
)

// FIN-CTRL-033 population against a real Postgres, through the UNPRIVILEGED role so row-level security is in force.

type batchSeed struct {
	declaredRows, declaredTotal, computedTotal any // nil = NULL
	rows, valid, invalid                       int
	imported, quarantined, failed, skipped     int
	status                                     string
}

func (f *fixture) seedBatch(tenant, entity string, s batchSeed) string {
	f.t.Helper()
	if _, err := f.admin.Exec(f.ctx, `
		INSERT INTO import_profiles (profile_key, version, import_type, status, material, spec, spec_hash, created_by)
		VALUES ('test.tieout', 1, 'test', 'DRAFT', false, '{}'::jsonb, repeat('a', 64), 'author')
		ON CONFLICT DO NOTHING`); err != nil {
		f.t.Fatalf("seed profile: %v", err)
	}
	id := "imb_" + uuid.NewString()
	if s.status == "" {
		s.status = "COMPLETED"
	}
	if _, err := f.admin.Exec(f.ctx, `
		INSERT INTO import_batches (batch_id, tenant_id, legal_entity_id, profile_key, profile_version, material, source_system,
		    declared_content_type, declared_sha256, declared_rows, declared_control_total, status,
		    content_sha256, chain_hash, row_count, valid_rows, invalid_rows, computed_control_total,
		    preview_fingerprint, approved_fingerprint, approved_by,
		    imported_items, quarantined_items, failed_items, skipped_items, created_by, file_name)
		VALUES ($1::text, $2, $3, 'test.tieout', 1, false, 'erp', 'text/csv', encode(sha256(convert_to($1::text, 'UTF8')), 'hex'), $4, $5, $6,
		    repeat('c', 64), repeat('d', 64), $7, $8, $9, $10, 'fp', 'fp', 'approver', $11, $12, $13, $14, 'maker', 'secret-name.csv')`,
		id, tenant, entity, s.declaredRows, s.declaredTotal, s.status, s.rows, s.valid, s.invalid, s.computedTotal,
		s.imported, s.quarantined, s.failed, s.skipped); err != nil {
		f.t.Fatalf("seed batch: %v", err)
	}
	return id
}

func TestImportBatchTieout_RawFactsAbsenceVisibleScopeAndIsolation(t *testing.T) {
	f := newFixture(t)
	tenant, entity := "tenant-tieout-"+uuid.NewString()[:8], "le-tieout-1"
	q := func(tenantID, entityID, batch, after string) (*domain.ControlPopulationPage, error) {
		return f.s.QueryImportBatchTieout(f.ctx, tenantID, domain.ImportBatchTieoutQuery{LegalEntityID: entityID, BatchID: batch, AfterRecordID: after})
	}

	// A batch whose every row has an outcome: 9 imported + 1 skipped (the invalid row) = 10 rows.
	full := f.seedBatch(tenant, entity, batchSeed{declaredRows: 10, declaredTotal: "1234.50", computedTotal: "1234.50",
		rows: 10, valid: 9, invalid: 1, imported: 9, skipped: 1})
	p, err := q(tenant, entity, full, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Records) != 1 || p.DeclaredTotals.RowCount != 1 {
		t.Fatalf("exactly one record: %+v", p.Records)
	}
	r := p.Records[0]
	want := map[string]string{
		"batch_status": "COMPLETED", "declared_rows": "10", "row_count": "10", "valid_rows": "9", "invalid_rows": "1",
		"imported_items": "9", "quarantined_items": "0", "failed_items": "0", "skipped_items": "1",
		"declared_control_total": "1234.50", "computed_control_total": "1234.50", "profile_key": "test.tieout", "source_system": "erp",
	}
	for k, v := range want {
		if r.Attributes[k] != v {
			t.Errorf("attribute %s = %q, want %q", k, r.Attributes[k], v)
		}
	}
	if r.RecordID != full || r.Reference != full || r.Amount != "1234.50" || r.Currency != "XXX" || len(r.Date) != 10 {
		t.Fatalf("record %+v", r)
	}
	if p.DeclaredTotals.Totals["XXX"] != "1234.50" || !strings.HasPrefix(p.Watermark, "di1:n=1;md5=") {
		t.Fatalf("totals %+v watermark %q", p.DeclaredTotals, p.Watermark)
	}
	for k, v := range r.Attributes {
		if strings.Contains(v, "secret-name") {
			t.Fatalf("the file name is evidence and never exposed: %s=%q", k, v)
		}
	}

	// Absence is visible: a batch with no declared totals reports EMPTY strings, never a made-up zero, so the control's
	// ties cannot silently pass.
	bare := f.seedBatch(tenant, entity, batchSeed{rows: 3, valid: 3, imported: 3})
	p, err = q(tenant, entity, bare, "")
	if err != nil {
		t.Fatal(err)
	}
	a := p.Records[0].Attributes
	if a["declared_rows"] != "" || a["declared_control_total"] != "" || a["computed_control_total"] != "" || p.Records[0].Amount != "0" {
		t.Fatalf("absent values must be empty: %+v amount=%s", a, p.Records[0].Amount)
	}

	// A non-decimal control total keeps its text as evidence but is never offered as the amount.
	odd := f.seedBatch(tenant, entity, batchSeed{declaredRows: 1, declaredTotal: "n/a", computedTotal: "n/a", rows: 1, valid: 1, imported: 1})
	if p, err = q(tenant, entity, odd, ""); err != nil || p.Records[0].Amount != "0" || p.Records[0].Attributes["computed_control_total"] != "n/a" {
		t.Fatalf("non-decimal total: %v %+v", err, p)
	}

	// A batch that is not complete reports the facts as they are (the control, not the source, judges them).
	partial := f.seedBatch(tenant, entity, batchSeed{declaredRows: 10, declaredTotal: "100", computedTotal: "100", rows: 10, valid: 10,
		imported: 6, quarantined: 2, failed: 1, status: "COMPLETED_WITH_EXCEPTIONS"})
	if p, err = q(tenant, entity, partial, ""); err != nil || p.Records[0].Attributes["quarantined_items"] != "2" || p.Records[0].Attributes["batch_status"] != "COMPLETED_WITH_EXCEPTIONS" {
		t.Fatalf("partial batch: %v %+v", err, p)
	}

	// Scope and isolation: another tenant, another entity and an unknown batch are all "not found".
	for name, args := range map[string][3]string{
		"other tenant":  {"other-" + tenant, entity, full},
		"other entity":  {tenant, "le-other", full},
		"unknown batch": {tenant, entity, "imb_" + uuid.NewString()},
	} {
		if _, err := q(args[0], args[1], args[2], ""); !errors.Is(err, domain.ErrImportBatchNotFound) {
			t.Errorf("%s: want ErrImportBatchNotFound, got %v", name, err)
		}
	}

	// A cursor at or past the only record leaves nothing, under the same watermark.
	first, _ := q(tenant, entity, full, "")
	past, err := q(tenant, entity, full, full)
	if err != nil || len(past.Records) != 0 || past.Watermark != first.Watermark {
		t.Fatalf("cursor at the record: %v %+v", err, past)
	}

	// The watermark moves when the batch changes.
	if _, err := f.admin.Exec(f.ctx, `UPDATE import_batches SET failed_items = 1, imported_items = 8 WHERE batch_id = $1`, full); err != nil {
		t.Fatal(err)
	}
	if moved, _ := q(tenant, entity, full, ""); moved.Watermark == first.Watermark {
		t.Fatal("the watermark must change when the batch's counts change")
	}
}
