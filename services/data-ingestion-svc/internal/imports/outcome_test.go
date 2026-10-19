package imports

import (
	"testing"

	"zoiko.io/eventing/integration"
)

// INT-NP-048 / INT-CTRL-036 for imports: success cannot conceal what is unreconciled or what failed.
func TestAnImportIsCertifiedOnlyWhenItReconciled(t *testing.T) {
	base := func(s Status) *Batch {
		return &Batch{Status: s, ScanVerdict: "CLEAN", MappingHash: "m", RowCount: 3, ValidRows: 3, ImportedItems: 3}
	}
	cases := []struct {
		name    string
		b       *Batch
		overall string
	}{
		{"created", base(StatusCreated), "PENDING"},
		{"previewed, not yet executed", base(StatusPreviewed), "PENDING"},
		{"importing", base(StatusImporting), "PENDING"},
		{"delivered but never reconciled is not a success", base(StatusCompleted), "PENDING"},
		{"reconciled", base(StatusReconciled), "CERTIFIED"},
		{"some items quarantined", func() *Batch {
			b := base(StatusCompletedWithExceptions)
			b.QuarantinedItems = 1
			b.ImportedItems = 2
			return b
		}(), "FAILED"},
		{"reconciliation found a difference", func() *Batch {
			b := base(StatusCompleted)
			b.Reconciliation = &Reconciliation{Matched: false, Problems: []string{"total differs"}}
			return b
		}(), "FAILED"},
		{"delivered fine, reconciliation broken: still FAILED, never CERTIFIED", func() *Batch {
			b := base(StatusReconciled)
			b.Reconciliation = &Reconciliation{Matched: false}
			return b
		}(), "FAILED"},
		{"quarantined for malware", &Batch{Status: StatusQuarantined, ScanVerdict: "INFECTED"}, "FAILED"},
		{"failed", base(StatusFailed), "FAILED"},
		{"invalid rows", func() *Batch { b := base(StatusValidated); b.InvalidRows = 1; return b }(), "FAILED"},
		{"cancelled is not certified", base(StatusCancelled), "PENDING"},
	}
	for _, tc := range cases {
		o := OutcomeOf(tc.b)
		if err := o.Validate(); err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
		if got := o.Overall(); got != tc.overall {
			t.Errorf("%s: overall %s, want %s (%+v)", tc.name, got, tc.overall, o)
		}
		if o.Certified() != (tc.overall == "CERTIFIED") {
			t.Errorf("%s: Certified() disagrees", tc.name)
		}
	}
	if OutcomeOf(&Batch{Status: StatusReconciled}).Provider != integration.StateNotApplicable {
		t.Error("an import has no external provider; it says so out loud")
	}
}
