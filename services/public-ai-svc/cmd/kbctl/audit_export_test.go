package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"zoiko.io/public-ai-svc/internal/release"
	"zoiko.io/public-ai-svc/internal/store"
)

// ADM-T006 (Doc 09 §21): an audit export needs a purpose and an approval;
// a refused export writes no file, and an allowed one is itself audited
// before the file is written.
func TestAuditExportNeedsPurposeAndApproval(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	if err := st.AppendAudit(ctx, store.AuditEntry{Action: "kb.promoted", Target: "kb-1", Actor: "ops-a", Approval: "CHG-1", At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ok := release.Operator{Actor: "auditor-b", Approval: "CHG-2", Environment: "test"}
	for name, c := range map[string]struct {
		purpose string
		op      release.Operator
	}{
		"no purpose":  {"", ok},
		"no approval": {"quarterly control review", release.Operator{Actor: "auditor-b"}},
		"no actor":    {"quarterly control review", release.Operator{Approval: "CHG-2"}},
	} {
		path := filepath.Join(dir, name+".json")
		if _, _, err := exportAudit(ctx, st, path, c.purpose, c.op); err == nil {
			t.Errorf("%s: export allowed", name)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: a refused export left a file", name)
		}
	}
	path := filepath.Join(dir, "export.json")
	n, check, err := exportAudit(ctx, st, path, "quarterly control review", ok)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || check != "intact" {
		t.Fatalf("exported %d rows, chain %s; want 2 and intact", n, check)
	}
	var out struct {
		Purpose  string             `json:"purpose"`
		Approval string             `json:"approval"`
		Entries  []store.AuditEntry `json:"entries"`
	}
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	last := out.Entries[len(out.Entries)-1]
	if out.Purpose != "quarterly control review" || out.Approval != "CHG-2" || last.Action != "audit.exported" || last.Reason != out.Purpose {
		t.Fatalf("export not purpose-bound and self-audited: %+v", out)
	}
}
