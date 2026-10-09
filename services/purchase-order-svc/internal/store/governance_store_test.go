package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/purchase-order-svc/internal/domain"
	"zoiko.io/purchase-order-svc/internal/outbox"
	"zoiko.io/purchase-order-svc/internal/store"
)

// fixture is one tenant with a store.
type fixture struct {
	t      *testing.T
	s      *store.PgStore
	owner  *pgxpool.Pool
	tenant string
	entity string
	ctx    context.Context
}

func newFixture(t *testing.T) *fixture {
	s, owner := openStore(t)
	tenant := uuid.New().String()
	return &fixture{t: t, s: s, owner: owner, tenant: tenant, entity: uuid.New().String(), ctx: tctx(tenant)}
}

func twoLines() []domain.LineInput {
	return []domain.LineInput{
		{ItemRef: "SKU-1", Description: "bolts", Quantity: 10, UnitPrice: 5, UOM: "EA"},
		{ItemRef: "SKU-2", Description: "nuts", Quantity: 4, UnitPrice: 2.5, UOM: "EA"},
	}
}

// draft creates a DRAFT with two lines (total 60) prepared by "maker".
func (f *fixture) draft() *domain.OrderDetail {
	f.t.Helper()
	d, created, err := f.s.CreateDraft(f.ctx, store.CreateDraftInput{
		TenantID: f.tenant, LegalEntityID: f.entity, SupplierRef: "SUP-1", CurrencyCode: "USD",
		CorrelationID: "corr-" + uuid.NewString(), PreparedBy: "maker", Lines: twoLines(),
	})
	if err != nil || !created {
		f.t.Fatalf("CreateDraft: created=%v err=%v", created, err)
	}
	return d
}

func (f *fixture) cmd(id, actor string, c store.Command, mutate ...func(*store.TransitionInput)) (*domain.OrderDetail, error) {
	in := store.TransitionInput{TenantID: f.tenant, OrderID: id, Actor: actor, Command: c, Reason: "because"}
	for _, m := range mutate {
		m(&in)
	}
	return f.s.Transition(f.ctx, in)
}

// issued takes a fresh order DRAFT -> PENDING -> APPROVED -> ISSUED.
func (f *fixture) issued() *domain.OrderDetail {
	f.t.Helper()
	d := f.draft()
	for _, step := range []struct {
		actor string
		c     store.Command
	}{{"maker", store.CmdSubmit}, {"checker", store.CmdApprove}, {"buyer", store.CmdIssue}} {
		var err error
		if d, err = f.cmd(d.PurchaseOrderID, step.actor, step.c); err != nil {
			f.t.Fatalf("%s: %v", step.c, err)
		}
	}
	return d
}

func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.owner.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		f.t.Fatalf("count: %v", err)
	}
	return n
}

// ── lifecycle & maker-checker ────────────────────────────────────────────────

func TestGovernance_FullLifecycle_VersionsHistoryAndOutbox(t *testing.T) {
	f := newFixture(t)
	d := f.draft()
	if d.Status != domain.OrderStatusDraft || d.Version != 1 || d.Revision != 1 || d.TotalAmount != 60 || len(d.Lines) != 2 {
		t.Fatalf("unexpected draft: status=%s v=%d r=%d total=%v lines=%d", d.Status, d.Version, d.Revision, d.TotalAmount, len(d.Lines))
	}
	if d.IssuedAt != nil || d.IssuedByPrincipalID != "" {
		t.Fatal("a draft carries issue evidence")
	}

	d, _ = f.cmd(d.PurchaseOrderID, "maker", store.CmdSubmit)
	d, _ = f.cmd(d.PurchaseOrderID, "checker", store.CmdApprove)
	if d.Status != domain.OrderStatusApproved || d.ApprovalBasis != domain.ApprovalBasisWorkflow || d.ApprovedByPrincipalID == nil || *d.ApprovedByPrincipalID != "checker" {
		t.Fatalf("approval evidence missing: %+v", d.PurchaseOrder)
	}
	d, _ = f.cmd(d.PurchaseOrderID, "buyer", store.CmdIssue)
	if d.Status != domain.OrderStatusIssued || d.IssuedAt == nil || d.IssuedByPrincipalID != "buyer" {
		t.Fatalf("issue evidence missing: %+v", d.PurchaseOrder)
	}
	if d.Version != 4 {
		t.Errorf("version = %d, want 4 after three commands on a version-1 draft", d.Version)
	}

	events, err := f.s.ListEvents(f.ctx, d.PurchaseOrderID)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range events {
		got = append(got, e.EventType)
	}
	want := []string{store.EventCreated, store.EventSubmitted, store.EventApproved, store.EventIssued}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("history = %v, want %v", got, want)
	}
	// Spec events plus the legacy alias for Issued, all in the outbox.
	if n := f.count(`SELECT count(*) FROM outbox_events WHERE aggregate_id = $1`, d.PurchaseOrderID); n != 5 {
		t.Errorf("outbox rows = %d, want 5", n)
	}
	if n := f.count(`SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'purchase.order.issued'`, d.PurchaseOrderID); n != 1 {
		t.Errorf("legacy purchase.order.issued alias rows = %d, want 1", n)
	}
}

// Negative path #5/#29: the preparer (or the submitter) cannot approve.
func TestGovernance_Approve_PreparerAndSubmitterRefused(t *testing.T) {
	f := newFixture(t)
	d := f.draft()
	d, _ = f.cmd(d.PurchaseOrderID, "submitter", store.CmdSubmit)

	for _, actor := range []string{"maker", "submitter"} {
		if _, err := f.cmd(d.PurchaseOrderID, actor, store.CmdApprove); !errors.Is(err, domain.ErrSoDConflict) {
			t.Errorf("%s approving: err = %v, want ErrSoDConflict", actor, err)
		}
	}
	got, _ := f.s.GetOrder(f.ctx, d.PurchaseOrderID)
	if got.Status != domain.OrderStatusPendingApproval {
		t.Errorf("a refused approval changed the status to %s", got.Status)
	}
	// ...and the database refuses it too if the code path were bypassed, with or
	// without a recorded basis: the in-service WORKFLOW approval is the very flow
	// maker-checker exists for.
	for _, basis := range []string{"NULL", "'WORKFLOW'"} {
		_, err := f.owner.Exec(context.Background(), `UPDATE purchase_orders SET po_status='APPROVED', approved_by_principal_id='maker', approved_at=now(), approval_basis=`+basis+` WHERE purchase_order_id=$1`, d.PurchaseOrderID)
		if err == nil {
			t.Fatalf("the database let the preparer approve their own order (approval_basis=%s)", basis)
		}
	}
}

// Negative path #1 (AP-03): a PO cannot reach ISSUED without approval.
func TestGovernance_CannotIssueWithoutApproval_AtStoreAndDatabase(t *testing.T) {
	f := newFixture(t)
	d := f.draft()
	if _, err := f.cmd(d.PurchaseOrderID, "buyer", store.CmdIssue); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("issue from DRAFT: err = %v, want ErrInvalidTransition", err)
	}
	d, _ = f.cmd(d.PurchaseOrderID, "maker", store.CmdSubmit)
	if _, err := f.cmd(d.PurchaseOrderID, "buyer", store.CmdIssue); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("issue from PENDING_APPROVAL: err = %v, want ErrInvalidTransition", err)
	}
	for _, sql := range []string{
		`UPDATE purchase_orders SET po_status='ISSUED', issued_by_principal_id='x', issued_at=now() WHERE purchase_order_id=$1`,
		`UPDATE purchase_orders SET po_status='APPROVED' WHERE purchase_order_id=$1`, // no approver recorded
	} {
		if _, err := f.owner.Exec(context.Background(), sql, d.PurchaseOrderID); err == nil {
			t.Errorf("the database let an unapproved order advance: %s", sql)
		}
	}
	// A fresh row cannot be born ISSUED without an approval basis either.
	_, err := f.owner.Exec(context.Background(), `INSERT INTO purchase_orders (purchase_order_id, tenant_id, legal_entity_id, po_number, po_status, total_amount, currency_code, correlation_id)
		VALUES ($1,$2,$3,$4,'ISSUED',1,'USD',$5)`, uuid.NewString(), f.tenant, f.entity, "PO-X-"+uuid.NewString()[:6], "c-"+uuid.NewString())
	if err == nil {
		t.Error("the database let an order be created ISSUED without an approval basis")
	}
}

func TestGovernance_Submit_NeedsLines_StaleVersion_HoldRelease(t *testing.T) {
	f := newFixture(t)

	// Header-only draft with no total cannot be submitted.
	empty, _, err := f.s.CreateDraft(f.ctx, store.CreateDraftInput{TenantID: f.tenant, LegalEntityID: f.entity, CurrencyCode: "USD",
		CorrelationID: "corr-" + uuid.NewString(), PreparedBy: "maker"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.cmd(empty.PurchaseOrderID, "maker", store.CmdSubmit); !errors.Is(err, domain.ErrNoLines) {
		t.Errorf("submit with no lines and no total: err = %v, want ErrNoLines", err)
	}

	d := f.draft()
	stale := d.Version + 7
	if _, err := f.cmd(d.PurchaseOrderID, "maker", store.CmdSubmit, func(in *store.TransitionInput) { in.ExpectedVersion = &stale }); !errors.Is(err, domain.ErrStaleVersion) {
		t.Errorf("stale version: err = %v, want ErrStaleVersion", err)
	}

	// Hold from ISSUED returns to ISSUED; from APPROVED returns to APPROVED.
	i := f.issued()
	h, err := f.cmd(i.PurchaseOrderID, "ops", store.CmdHold)
	if err != nil || h.Status != domain.OrderStatusOnHold || h.HeldFromStatus != "ISSUED" || h.HoldReason != "because" {
		t.Fatalf("hold: %v %+v", err, h.PurchaseOrder)
	}
	r, err := f.cmd(i.PurchaseOrderID, "ops", store.CmdRelease)
	if err != nil || r.Status != domain.OrderStatusIssued || r.HeldFromStatus != "" || r.HoldReason != "" || r.HeldByPrincipalID != nil {
		t.Fatalf("release should restore ISSUED and clear the hold: %v %+v", err, r.PurchaseOrder)
	}

	a := f.draft()
	a, _ = f.cmd(a.PurchaseOrderID, "maker", store.CmdSubmit)
	a, _ = f.cmd(a.PurchaseOrderID, "checker", store.CmdApprove)
	if h, err = f.cmd(a.PurchaseOrderID, "ops", store.CmdHold); err != nil || h.HeldFromStatus != "APPROVED" {
		t.Fatalf("hold from APPROVED: %v %+v", err, h)
	}
	if r, err = f.cmd(a.PurchaseOrderID, "ops", store.CmdRelease); err != nil || r.Status != domain.OrderStatusApproved {
		t.Fatalf("release should restore APPROVED: %v %+v", err, r)
	}
	// A held order cannot be issued/closed until released.
	if _, err := f.cmd(i.PurchaseOrderID, "ops", store.CmdHold); err != nil {
		t.Fatal(err)
	}
	if _, err := f.cmd(i.PurchaseOrderID, "ops", store.CmdClose); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("close while held: err = %v, want ErrInvalidTransition", err)
	}
}

func TestGovernance_Cancel_RefusedWhenProgressExists(t *testing.T) {
	f := newFixture(t)
	d := f.issued()
	if _, err := f.s.RecordProgress(f.ctx, f.tenant, d.PurchaseOrderID, d.Lines[0].LineID, "ap04",
		domain.ProgressRequest{Kind: domain.ProgressReceived, Quantity: 1, SourceRef: "rcpt-1", DeltaSign: 1}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.cmd(d.PurchaseOrderID, "ops", store.CmdCancel); !errors.Is(err, domain.ErrHasProgress) {
		t.Errorf("cancel with a receipt: err = %v, want ErrHasProgress", err)
	}
	// Nothing received: cancellable, and terminal afterwards.
	clean := f.issued()
	c, err := f.cmd(clean.PurchaseOrderID, "ops", store.CmdCancel)
	if err != nil || c.Status != domain.OrderStatusCancelled || c.CancellationReason != "because" || c.CancelledBy == nil {
		t.Fatalf("cancel: %v %+v", err, c)
	}
	if _, err := f.cmd(clean.PurchaseOrderID, "ops", store.CmdClose); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("close after cancel: err = %v, want ErrInvalidTransition", err)
	}
}

// ── amendments & revisions ───────────────────────────────────────────────────

// Negative paths #7/#8: an issued PO is revised, never overwritten, and a
// quantity increase needs re-approval.
func TestGovernance_MaterialAmendment_SnapshotsRevisionAndRequiresReapproval(t *testing.T) {
	f := newFixture(t)
	d := f.issued()

	newLines := []domain.LineInput{
		{LineNumber: 1, ItemRef: "SKU-1", Description: "bolts", Quantity: 20, UnitPrice: 5, UOM: "EA"}, // quantity 10 -> 20
		{LineNumber: 2, ItemRef: "SKU-2", Description: "nuts", Quantity: 4, UnitPrice: 2.5, UOM: "EA"},
	}
	res, err := f.s.AmendOrder(f.ctx, f.tenant, d.PurchaseOrderID, "amender", domain.AmendOrderRequest{Lines: newLines, Reason: "more bolts"})
	if err != nil {
		t.Fatalf("AmendOrder: %v", err)
	}
	if !res.Material || !res.RequiresReapproval || !res.NewRevision {
		t.Fatalf("a quantity increase must be material, require re-approval and create a revision: %+v", res)
	}
	o := res.Order
	if o.Status != domain.OrderStatusDraft || o.Revision != 2 || o.TotalAmount != 110 {
		t.Fatalf("after amend: status=%s revision=%d total=%v, want DRAFT/2/110", o.Status, o.Revision, o.TotalAmount)
	}
	if o.ApprovedByPrincipalID != nil || o.ApprovalBasis != "" || o.SubmittedByPrincipalID != nil {
		t.Errorf("the old approval survived a material change: %+v", o.PurchaseOrder)
	}
	if o.PreparedByPrincipalID != "amender" {
		t.Errorf("the preparer of the new revision should be the amender, got %q", o.PreparedByPrincipalID)
	}
	if o.Lines[0].LineID != d.Lines[0].LineID {
		t.Error("an amended line must keep its identity (progress and receipts reference it)")
	}

	// The superseded revision is preserved immutably with the OLD content.
	revs, err := f.s.ListRevisions(f.ctx, d.PurchaseOrderID)
	if err != nil || len(revs) != 1 {
		t.Fatalf("revisions: n=%d err=%v", len(revs), err)
	}
	rev := revs[0]
	if rev.Revision != 1 || rev.StatusAtSnapshot != domain.OrderStatusIssued || rev.Snapshot.TotalAmount != 60 || rev.Snapshot.Lines[0].Quantity != 10 {
		t.Errorf("snapshot does not hold the issued revision: %+v", rev)
	}
	if rev.ApprovedByPrincipalID == nil || *rev.ApprovedByPrincipalID != "checker" {
		t.Error("the snapshot lost who approved the superseded revision")
	}

	// Cannot be issued again until re-approved.
	if _, err := f.cmd(d.PurchaseOrderID, "buyer", store.CmdIssue); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("issue straight after a material amendment: err = %v, want ErrInvalidTransition", err)
	}
	o2, _ := f.cmd(d.PurchaseOrderID, "amender", store.CmdSubmit)
	if _, err := f.cmd(d.PurchaseOrderID, "amender", store.CmdApprove); !errors.Is(err, domain.ErrSoDConflict) {
		t.Errorf("the amender approving their own revision: err = %v, want ErrSoDConflict", err)
	}
	f.cmd(o2.PurchaseOrderID, "checker2", store.CmdApprove)
	final, err := f.cmd(o2.PurchaseOrderID, "buyer", store.CmdIssue)
	if err != nil || final.Status != domain.OrderStatusIssued || final.Revision != 2 {
		t.Fatalf("re-approval and re-issue: %v %+v", err, final)
	}
	// The amendment ledger kept the legacy before/after record.
	ledger, _ := f.s.ListAmendments(f.ctx, d.PurchaseOrderID)
	if len(ledger) != 1 || ledger[0].PreviousTotalAmount != 60 || ledger[0].NewTotalAmount != 110 || ledger[0].Reason != "more bolts" {
		t.Errorf("amendment ledger: %+v", ledger)
	}
}

func TestGovernance_NonMaterialAmendment_KeepsStatusButCreatesRevision(t *testing.T) {
	f := newFixture(t)
	d := f.issued()
	res, err := f.s.AmendOrder(f.ctx, f.tenant, d.PurchaseOrderID, "amender", domain.AmendOrderRequest{
		Reason: "new dock", Lines: []domain.LineInput{
			{LineNumber: 1, ItemRef: "SKU-1", Description: "bolts (zinc)", Quantity: 10, UnitPrice: 5, UOM: "EA", DeliveryLocation: "Dock 4"},
			{LineNumber: 2, ItemRef: "SKU-2", Description: "nuts", Quantity: 4, UnitPrice: 2.5, UOM: "EA"},
		}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Material || res.RequiresReapproval || !res.NewRevision {
		t.Fatalf("descriptive changes are not material but still create a revision: %+v", res)
	}
	if res.Order.Status != domain.OrderStatusIssued || res.Order.Revision != 2 || res.Order.Lines[0].DeliveryLocation != "Dock 4" {
		t.Errorf("after non-material amend: %+v", res.Order.PurchaseOrder)
	}
	if res.Order.ApprovedByPrincipalID == nil {
		t.Error("a non-material change must keep the approval")
	}
	revs, _ := f.s.ListRevisions(f.ctx, d.PurchaseOrderID)
	if len(revs) != 1 || revs[0].Snapshot.Lines[0].Description != "bolts" {
		t.Errorf("the superseded revision should hold the ORIGINAL description: %+v", revs)
	}
}

func TestGovernance_Amendment_DraftInPlace_PendingWithdrawn_NoChangeRefused(t *testing.T) {
	f := newFixture(t)

	d := f.draft()
	res, err := f.s.AmendOrder(f.ctx, f.tenant, d.PurchaseOrderID, "maker", domain.AmendOrderRequest{
		Reason: "typo", Lines: []domain.LineInput{{LineNumber: 1, ItemRef: "SKU-1", Quantity: 11, UnitPrice: 5, UOM: "EA"}, {LineNumber: 2, ItemRef: "SKU-2", Quantity: 4, UnitPrice: 2.5, UOM: "EA"}}})
	if err != nil || res.NewRevision || res.Order.Revision != 1 || res.Order.Status != domain.OrderStatusDraft || res.Order.TotalAmount != 65 {
		t.Fatalf("a DRAFT is edited in place with no new revision: %v %+v", err, res)
	}
	if revs, _ := f.s.ListRevisions(f.ctx, d.PurchaseOrderID); len(revs) != 0 {
		t.Errorf("a DRAFT amendment created %d revisions", len(revs))
	}

	// An amendment while approval is pending withdraws that approval.
	d, _ = f.cmd(d.PurchaseOrderID, "maker", store.CmdSubmit)
	res, err = f.s.AmendOrder(f.ctx, f.tenant, d.PurchaseOrderID, "maker", domain.AmendOrderRequest{
		Reason: "late change", Lines: []domain.LineInput{{LineNumber: 1, ItemRef: "SKU-1", Quantity: 12, UnitPrice: 5, UOM: "EA"}, {LineNumber: 2, ItemRef: "SKU-2", Quantity: 4, UnitPrice: 2.5, UOM: "EA"}}})
	if err != nil || res.Order.Status != domain.OrderStatusDraft || !res.RequiresReapproval || res.Order.SubmittedByPrincipalID != nil {
		t.Fatalf("an amendment under review must send it back to DRAFT: %v %+v", err, res)
	}

	// Identical content is refused.
	same := []domain.LineInput{{LineNumber: 1, ItemRef: "SKU-1", Quantity: 12, UnitPrice: 5, UOM: "EA"}, {LineNumber: 2, ItemRef: "SKU-2", Quantity: 4, UnitPrice: 2.5, UOM: "EA"}}
	if _, err := f.s.AmendOrder(f.ctx, f.tenant, d.PurchaseOrderID, "maker", domain.AmendOrderRequest{Reason: "nothing", Lines: same}); !errors.Is(err, store.ErrNoChange) {
		t.Errorf("no-op amendment: err = %v, want ErrNoChange", err)
	}
	// Stale version.
	stale := 99
	if _, err := f.s.AmendOrder(f.ctx, f.tenant, d.PurchaseOrderID, "maker", domain.AmendOrderRequest{Reason: "x", Lines: same, ExpectedVersion: &stale}); !errors.Is(err, domain.ErrStaleVersion) {
		t.Errorf("stale amendment: err = %v, want ErrStaleVersion", err)
	}
}

// An amendment cannot undercut what was already received/invoiced, nor drop a
// line that has progress.
func TestGovernance_Amendment_BelowProgressRefused(t *testing.T) {
	f := newFixture(t)
	d := f.issued()
	if _, err := f.s.RecordProgress(f.ctx, f.tenant, d.PurchaseOrderID, d.Lines[0].LineID, "ap04",
		domain.ProgressRequest{Kind: domain.ProgressReceived, Quantity: 8, SourceRef: "rcpt-8", DeltaSign: 1}, ""); err != nil {
		t.Fatal(err)
	}
	below := []domain.LineInput{{LineNumber: 1, ItemRef: "SKU-1", Quantity: 5, UnitPrice: 5, UOM: "EA"}, {LineNumber: 2, ItemRef: "SKU-2", Quantity: 4, UnitPrice: 2.5, UOM: "EA"}}
	if _, err := f.s.AmendOrder(f.ctx, f.tenant, d.PurchaseOrderID, "amender", domain.AmendOrderRequest{Reason: "cut", Lines: below}); !errors.Is(err, domain.ErrAmendmentBelowProgress) {
		t.Errorf("cutting below received: err = %v, want ErrAmendmentBelowProgress", err)
	}
	removed := []domain.LineInput{{LineNumber: 2, ItemRef: "SKU-2", Quantity: 4, UnitPrice: 2.5, UOM: "EA"}}
	if _, err := f.s.AmendOrder(f.ctx, f.tenant, d.PurchaseOrderID, "amender", domain.AmendOrderRequest{Reason: "drop", Lines: removed}); !errors.Is(err, domain.ErrAmendmentBelowProgress) {
		t.Errorf("removing a line with progress: err = %v, want ErrAmendmentBelowProgress", err)
	}
	// The refused amendment left no trace: no revision, no state change.
	if revs, _ := f.s.ListRevisions(f.ctx, d.PurchaseOrderID); len(revs) != 0 {
		t.Errorf("a refused amendment left %d revisions behind", len(revs))
	}
	if got, _ := f.s.GetOrder(f.ctx, d.PurchaseOrderID); got.Status != domain.OrderStatusIssued || got.Revision != 1 {
		t.Errorf("a refused amendment changed the order: %s r%d", got.Status, got.Revision)
	}
	// Raising above progress is fine.
	up := []domain.LineInput{{LineNumber: 1, ItemRef: "SKU-1", Quantity: 9, UnitPrice: 5, UOM: "EA"}, {LineNumber: 2, ItemRef: "SKU-2", Quantity: 4, UnitPrice: 2.5, UOM: "EA"}}
	if _, err := f.s.AmendOrder(f.ctx, f.tenant, d.PurchaseOrderID, "amender", domain.AmendOrderRequest{Reason: "ok", Lines: up}); err != nil {
		t.Errorf("amending to a quantity above progress should work: %v", err)
	}
}

// The triggers hold even when the application code is bypassed entirely.
func TestGovernance_DatabaseRefusesInPlaceEditsAndTampering(t *testing.T) {
	f := newFixture(t)
	d := f.issued()
	id := d.PurchaseOrderID
	exec := func(sql string, args ...any) error {
		_, err := f.owner.Exec(context.Background(), sql, args...)
		return err
	}

	cases := map[string]string{
		"edit total of an issued PO in place":         `UPDATE purchase_orders SET total_amount = 1 WHERE purchase_order_id = $1`,
		"edit supplier of an issued PO in place":      `UPDATE purchase_orders SET supplier_ref = 'EVIL' WHERE purchase_order_id = $1`,
		"edit currency of an issued PO in place":      `UPDATE purchase_orders SET currency_code = 'EUR' WHERE purchase_order_id = $1`,
		"edit payment terms of an issued PO":          `UPDATE purchase_orders SET payment_terms = 'NET1' WHERE purchase_order_id = $1`,
		"back to DRAFT with no revision/snapshot":     `UPDATE purchase_orders SET po_status = 'DRAFT' WHERE purchase_order_id = $1`,
		"back to DRAFT with revision but no snapshot": `UPDATE purchase_orders SET po_status = 'DRAFT', revision = 2 WHERE purchase_order_id = $1`,
		"delete the order":                            `DELETE FROM purchase_orders WHERE purchase_order_id = $1`,
		"change the po_number":                        `UPDATE purchase_orders SET po_number = 'PO-HACK' WHERE purchase_order_id = $1`,
		"edit a line's quantity while ISSUED":         `UPDATE purchase_order_lines SET quantity = 99 WHERE purchase_order_id = $1 AND line_number = 1`,
		"edit a line's unit price while ISSUED":       `UPDATE purchase_order_lines SET unit_price = 0 WHERE purchase_order_id = $1 AND line_number = 1`,
		"delete a line while ISSUED":                  `DELETE FROM purchase_order_lines WHERE purchase_order_id = $1 AND line_number = 1`,
		"tamper with the event history":               `UPDATE purchase_order_events SET detail = 'x' WHERE purchase_order_id = $1`,
		"delete event history":                        `DELETE FROM purchase_order_events WHERE purchase_order_id = $1`,
	}
	for name, sql := range cases {
		if err := exec(sql, id); err == nil {
			t.Errorf("the database allowed: %s", name)
		}
	}
	// A line on an ISSUED PO can still take a progress counter and a description.
	if err := exec(`UPDATE purchase_order_lines SET description = 'renamed' WHERE purchase_order_id = $1 AND line_number = 1`, id); err != nil {
		t.Errorf("descriptive edits must stay possible on an issued PO: %v", err)
	}
	// Negative progress is refused by the trigger.
	if err := exec(`UPDATE purchase_order_lines SET received_quantity = -1 WHERE purchase_order_id = $1 AND line_number = 1`, id); err == nil {
		t.Error("the database allowed negative received quantity")
	}
	// Revisions are append-only.
	f.s.AmendOrder(f.ctx, f.tenant, id, "amender", domain.AmendOrderRequest{Reason: "r", Lines: []domain.LineInput{{LineNumber: 1, ItemRef: "SKU-1", Quantity: 20, UnitPrice: 5, UOM: "EA"}, {LineNumber: 2, ItemRef: "SKU-2", Quantity: 4, UnitPrice: 2.5, UOM: "EA"}}})
	if err := exec(`UPDATE purchase_order_revisions SET reason = 'x' WHERE purchase_order_id = $1`, id); err == nil {
		t.Error("the database allowed editing a revision snapshot")
	}
	if err := exec(`DELETE FROM purchase_order_revisions WHERE purchase_order_id = $1`, id); err == nil {
		t.Error("the database allowed deleting a revision snapshot")
	}
}

// ── progress ─────────────────────────────────────────────────────────────────

func TestProgress_Idempotent_ReusedRefRefused_AndOpenQuantity(t *testing.T) {
	f := newFixture(t)
	d := f.issued()
	line := d.Lines[0].LineID
	push := func(kind string, qty float64, ref string, sign int) (*domain.ProgressResult, error) {
		return f.s.RecordProgress(f.ctx, f.tenant, d.PurchaseOrderID, line, "ap04",
			domain.ProgressRequest{Kind: kind, Quantity: qty, SourceRef: ref, DeltaSign: sign}, "corr")
	}

	r, err := push(domain.ProgressReceived, 6, "rcpt-1", 1)
	if err != nil || r.Replayed || r.ReceivedQuantity != 6 {
		t.Fatalf("first push: %v %+v", err, r)
	}
	// Replay: same ref, same content -> recorded once.
	r, err = push(domain.ProgressReceived, 6, "rcpt-1", 1)
	if err != nil || !r.Replayed || r.ReceivedQuantity != 6 {
		t.Fatalf("replay must be a no-op: %v %+v", err, r)
	}
	// Same ref, different content -> refused, not merged.
	if _, err := push(domain.ProgressReceived, 7, "rcpt-1", 1); !errors.Is(err, domain.ErrProgressRefReused) {
		t.Errorf("reused ref with different quantity: err = %v, want ErrProgressRefReused", err)
	}
	// The same source_ref under the OTHER kind is a different fact.
	if _, err := push(domain.ProgressInvoiced, 6, "rcpt-1", 1); err != nil {
		t.Errorf("the same ref under INVOICED is independent: %v", err)
	}
	// Reversal.
	r, err = push(domain.ProgressReceived, 2, "rcpt-1-rev", -1)
	if err != nil || r.ReceivedQuantity != 4 {
		t.Fatalf("reversal: %v %+v", err, r)
	}

	oq, ok, err := f.s.OpenQuantity(f.ctx, d.PurchaseOrderID)
	if err != nil || !ok || len(oq) != 2 {
		t.Fatalf("OpenQuantity: ok=%v n=%d err=%v", ok, len(oq), err)
	}
	l1 := oq[0]
	if l1.OrderedQuantity != 10 || l1.ReceivedQuantity != 4 || l1.InvoicedQuantity != 6 || l1.OpenReceiptQuantity != 6 || l1.OpenInvoiceQuantity != 4 {
		t.Errorf("line 1 figures: %+v", l1)
	}
	if oq[1].ReceivedQuantity != 0 || oq[1].OpenReceiptQuantity != 4 {
		t.Errorf("line 2 figures: %+v", oq[1])
	}
	if n := f.count(`SELECT count(*) FROM purchase_order_progress WHERE purchase_order_id = $1`, d.PurchaseOrderID); n != 3 {
		t.Errorf("progress ledger rows = %d, want 3 (the replay and the refused reuse recorded nothing)", n)
	}
}

// Negative path #4 (AP-03): receipt/invoice beyond the controlled quantity.
func TestProgress_ExceedsOrder_BelowZero_AndStateRules(t *testing.T) {
	f := newFixture(t)
	d := f.issued()
	line := d.Lines[0].LineID
	push := func(qty float64, ref string, sign int) error {
		_, err := f.s.RecordProgress(f.ctx, f.tenant, d.PurchaseOrderID, line, "ap04",
			domain.ProgressRequest{Kind: domain.ProgressReceived, Quantity: qty, SourceRef: ref, DeltaSign: sign}, "")
		return err
	}
	if err := push(10, "full", 1); err != nil {
		t.Fatal(err)
	}
	if err := push(1, "one-more", 1); !errors.Is(err, domain.ErrProgressExceedsOrder) {
		t.Errorf("over-receipt: err = %v, want ErrProgressExceedsOrder", err)
	}
	if err := push(11, "big-reversal", -1); !errors.Is(err, domain.ErrProgressBelowZero) {
		t.Errorf("over-reversal: err = %v, want ErrProgressBelowZero", err)
	}
	// Refused pushes leave no ledger row, so the same ref can be used once the
	// cause is fixed (here: after a reversal frees capacity).
	if n := f.count(`SELECT count(*) FROM purchase_order_progress WHERE source_ref IN ('one-more','big-reversal')`); n != 0 {
		t.Errorf("refused pushes left %d ledger rows", n)
	}
	if err := push(3, "rev", -1); err != nil {
		t.Fatal(err)
	}
	if err := push(1, "one-more", 1); err != nil {
		t.Errorf("the ref of a refused push must be reusable: %v", err)
	}
	// Unknown line.
	if _, err := f.s.RecordProgress(f.ctx, f.tenant, d.PurchaseOrderID, uuid.NewString(), "ap04",
		domain.ProgressRequest{Kind: domain.ProgressReceived, Quantity: 1, SourceRef: "ghost", DeltaSign: 1}, ""); !errors.Is(err, domain.ErrLineNotFound) {
		t.Errorf("unknown line: err = %v, want ErrLineNotFound", err)
	}

	// A held order refuses new receipts but still accepts reversals.
	f.cmd(d.PurchaseOrderID, "ops", store.CmdHold)
	if err := push(1, "while-held", 1); !errors.Is(err, domain.ErrOrderNotIssued) {
		t.Errorf("receipt on a held order: err = %v, want ErrOrderNotIssued", err)
	}
	if err := push(1, "held-reversal", -1); err != nil {
		t.Errorf("a reversal must stay possible on a held order: %v", err)
	}
	// A DRAFT / CANCELLED order accepts nothing.
	dr := f.draft()
	if _, err := f.s.RecordProgress(f.ctx, f.tenant, dr.PurchaseOrderID, dr.Lines[0].LineID, "ap04",
		domain.ProgressRequest{Kind: domain.ProgressReceived, Quantity: 1, SourceRef: "draft-rcpt", DeltaSign: 1}, ""); !errors.Is(err, domain.ErrOrderNotIssued) {
		t.Errorf("receipt on a DRAFT: err = %v, want ErrOrderNotIssued", err)
	}
}

func TestProgress_OverTolerance_Configurable(t *testing.T) {
	f := newFixture(t)
	f.s.WithOverTolerancePercent(10)
	d := f.issued()
	push := func(qty float64, ref string) error {
		_, err := f.s.RecordProgress(f.ctx, f.tenant, d.PurchaseOrderID, d.Lines[0].LineID, "ap04",
			domain.ProgressRequest{Kind: domain.ProgressReceived, Quantity: qty, SourceRef: ref, DeltaSign: 1}, "")
		return err
	}
	if err := push(11, "within-10pct"); err != nil {
		t.Errorf("11 of 10 is within a 10%% tolerance: %v", err)
	}
	if err := push(1, "beyond"); !errors.Is(err, domain.ErrProgressExceedsOrder) {
		t.Errorf("12 of 10 exceeds a 10%% tolerance: err = %v", err)
	}
}

// Two receipts racing for the last of the capacity: exactly one may win.
func TestProgress_ConcurrentPushes_Serialize(t *testing.T) {
	f := newFixture(t)
	d := f.issued()
	line := d.Lines[0].LineID

	var wg sync.WaitGroup
	results := make([]error, 6)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = f.s.RecordProgress(f.ctx, f.tenant, d.PurchaseOrderID, line, "ap04",
				domain.ProgressRequest{Kind: domain.ProgressReceived, Quantity: 4, SourceRef: "race-" + string(rune('a'+i)), DeltaSign: 1}, "")
		}(i)
	}
	wg.Wait()

	ok, exceeded := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, domain.ErrProgressExceedsOrder):
			exceeded++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	// capacity 10, pushes of 4: at most 2 fit.
	if ok != 2 || exceeded != 4 {
		t.Errorf("ok=%d exceeded=%d, want 2 and 4 (capacity 10, pushes of 4)", ok, exceeded)
	}
	oq, _, _ := f.s.OpenQuantity(f.ctx, d.PurchaseOrderID)
	if oq[0].ReceivedQuantity != 8 {
		t.Errorf("received = %v, want 8: concurrent pushes lost or double-counted", oq[0].ReceivedQuantity)
	}
}

// ── draft idempotency & legacy ───────────────────────────────────────────────

func TestCreateDraft_Idempotent_OnCorrelationID(t *testing.T) {
	f := newFixture(t)
	in := store.CreateDraftInput{TenantID: f.tenant, LegalEntityID: f.entity, SupplierRef: "SUP-1", CurrencyCode: "USD",
		CorrelationID: "conv-" + uuid.NewString(), PreparedBy: "maker", Lines: twoLines()}
	first, created, err := f.s.CreateDraft(f.ctx, in)
	if err != nil || !created {
		t.Fatalf("first: created=%v err=%v", created, err)
	}
	in.Lines = []domain.LineInput{{ItemRef: "OTHER", Quantity: 1, UnitPrice: 1, UOM: "EA"}}
	again, created, err := f.s.CreateDraft(f.ctx, in)
	if err != nil || created || again.PurchaseOrderID != first.PurchaseOrderID || again.TotalAmount != 60 || len(again.Lines) != 2 {
		t.Fatalf("a retried conversion must resolve to the SAME order: created=%v err=%v %+v", created, err, again)
	}
	if n := f.count(`SELECT count(*) FROM outbox_events WHERE aggregate_id = $1`, first.PurchaseOrderID); n != 1 {
		t.Errorf("outbox rows = %d, want 1 (Created only; the retry published nothing)", n)
	}
}

func TestLegacyIssue_WithLines_ProducesIssuedOrderAtVersion1(t *testing.T) {
	f := newFixture(t)
	in := legacyInput(f.tenant)
	in.LegalEntityID = f.entity
	in.Lines = twoLines()
	in.TotalAmount = 0
	d, created, err := f.s.CreateIssued(f.ctx, in)
	if err != nil || !created {
		t.Fatalf("CreateIssued: %v %v", created, err)
	}
	if d.Status != domain.OrderStatusIssued || d.Version != 1 || d.TotalAmount != 60 || len(d.Lines) != 2 {
		t.Errorf("legacy issue with lines: %+v", d.PurchaseOrder)
	}
}

// ── idempotency store & outbox relay ─────────────────────────────────────────

func TestIdempotencyKeys_Lifecycle(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx

	if _, created, err := f.s.BeginIdempotent(ctx, "approve:x", "k1", "h1"); err != nil || !created {
		t.Fatalf("first claim: created=%v err=%v", created, err)
	}
	rec, created, _ := f.s.BeginIdempotent(ctx, "approve:x", "k1", "h1")
	if created || rec.Completed {
		t.Fatalf("a second claim while running must see an unfinished record: %+v", rec)
	}
	if err := f.s.CompleteIdempotent(ctx, "approve:x", "k1", 200, []byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	rec, _, _ = f.s.BeginIdempotent(ctx, "approve:x", "k1", "h1")
	if !rec.Completed || rec.StatusCode != 200 || string(rec.Body) != `{"ok":true}` {
		t.Fatalf("completed record: %+v", rec)
	}
	// A completed record is never overwritten or released.
	f.s.CompleteIdempotent(ctx, "approve:x", "k1", 500, []byte(`x`))
	f.s.ReleaseIdempotent(ctx, "approve:x", "k1")
	if rec, _, _ = f.s.BeginIdempotent(ctx, "approve:x", "k1", "h1"); rec.StatusCode != 200 {
		t.Errorf("a completed record was disturbed: %+v", rec)
	}
	// Unfinished claims can be released, and a stale one is taken over.
	f.s.BeginIdempotent(ctx, "approve:y", "k2", "h2")
	f.s.ReleaseIdempotent(ctx, "approve:y", "k2")
	if _, created, _ := f.s.BeginIdempotent(ctx, "approve:y", "k2", "h2"); !created {
		t.Error("a released key must be claimable again")
	}
	if _, err := f.owner.Exec(context.Background(), `UPDATE idempotency_keys SET created_at = now() - interval '5 minutes' WHERE scope = 'approve:y'`); err != nil {
		t.Fatal(err)
	}
	if _, created, _ := f.s.BeginIdempotent(ctx, "approve:y", "k2", "h3"); !created {
		t.Error("a claim abandoned by a crash must be taken over after the grace period")
	}
	// Keys are per tenant.
	if _, created, _ := f.s.BeginIdempotent(tctx(uuid.NewString()), "approve:x", "k1", "h1"); !created {
		t.Error("the same key in another tenant is a different key")
	}
}

type captured struct {
	mu   sync.Mutex
	msgs map[string][]byte
}

func (c *captured) PublishOutbox(_ context.Context, id, _ string, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.msgs == nil {
		c.msgs = map[string][]byte{}
	}
	c.msgs[id] = payload
	return nil
}

// The relay reads outbox_events across ALL tenants with no tenant set. Under a
// restricted role that only works because the table has no row-level security
// (migration 000007 removes the policy 000006 put on it): with the policy, it
// would see nothing and no event would ever be published.
func TestOutboxRelay_PublishesEnvelopeAcrossTenants_UnderRestrictedRole(t *testing.T) {
	owner := openTestPool(t)
	relayPool := owner
	if appDSN := os.Getenv("TEST_APP_DATABASE_URL"); appDSN != "" {
		p, err := pgxpool.New(context.Background(), appDSN)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Close)
		relayPool = p
	}
	s := store.New(relayPool, zap.NewNop())

	var ids []string
	for i := 0; i < 2; i++ {
		tenant := uuid.NewString()
		d, _, err := s.CreateIssued(tctx(tenant), legacyInput(tenant))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, d.PurchaseOrderID)
	}

	pub := &captured{}
	outbox.NewRelay(relayPool, pub, time.Second, 100, zap.NewNop()).RelayOnce(context.Background())

	var unpublished int
	if err := owner.QueryRow(context.Background(), `SELECT count(*) FROM outbox_events WHERE published_at IS NULL`).Scan(&unpublished); err != nil {
		t.Fatal(err)
	}
	if unpublished != 0 || len(pub.msgs) != 10 {
		t.Fatalf("relay published %d of 10 events, %d still unpublished — the relay cannot see other tenants' rows", len(pub.msgs), unpublished)
	}
	// The envelope keeps the shape existing consumers read.
	var sawIssued bool
	for _, raw := range pub.msgs {
		var env struct {
			EventType     string `json:"event_type"`
			SourceService string `json:"source_service"`
			TenantID      string `json:"tenant_id"`
			LegalEntityID string `json:"legal_entity_id"`
			EmittedAt     string `json:"emitted_at"`
			Payload       struct {
				PurchaseOrderID string  `json:"purchase_order_id"`
				TotalAmount     float64 `json:"total_amount"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("envelope: %v", err)
		}
		if env.SourceService != "purchase-order-svc" || env.TenantID == "" || env.LegalEntityID == "" || env.EmittedAt == "" || env.Payload.PurchaseOrderID == "" {
			t.Errorf("envelope lost a field consumers rely on: %s", raw)
		}
		if env.EventType == "purchase.order.issued" {
			sawIssued = true
			if env.Payload.TotalAmount != 12500 {
				t.Errorf("legacy issued payload total = %v", env.Payload.TotalAmount)
			}
		}
	}
	if !sawIssued {
		t.Error("the legacy purchase.order.issued alias was not delivered")
	}
	_ = ids
}
