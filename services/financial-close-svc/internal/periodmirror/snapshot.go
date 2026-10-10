package periodmirror

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// ReadinessSnapshot is the period's close-readiness result as of a command.
type ReadinessSnapshot struct {
	IsReady        bool     `json:"is_ready"`
	BlockingIssues []string `json:"blocking_issues"`
}

// snapshotDoc is the canonical document hashed into control_snapshot_ref. Field
// order is the struct order, which encoding/json preserves, so the encoding is
// stable. Changing ANY field name, order or the version "v" changes every hash:
// bump "v" if the shape ever has to change.
type snapshotDoc struct {
	V                  int                `json:"v"`
	FiscalPeriodID     string             `json:"fiscal_period_id"`
	Command            string             `json:"command"`
	EvidenceDocumentID string             `json:"evidence_document_id"`
	Readiness          *ReadinessSnapshot `json:"readiness"`
}

// SnapshotRef returns control_snapshot_ref: the lowercase sha256 hex of the
// canonical JSON of
//
//	{"v":1,"fiscal_period_id":..,"command":..,"evidence_document_id":..,
//	 "readiness":{"is_ready":..,"blocking_issues":[sorted..]} | null}
//
// i.e. the period's readiness snapshot as of the command, the close evidence
// document id, the fiscal period id and the command. Blocking issues are sorted
// (and a nil slice is encoded as []) so the same readiness always hashes the
// same regardless of the order the checks ran in. readiness may be nil for a
// command that is not readiness-gated (AUTHORIZE_REOPEN); it then encodes as null.
func SnapshotRef(fiscalPeriodID, command, evidenceDocumentID string, readiness *ReadinessSnapshot) string {
	doc := snapshotDoc{V: 1, FiscalPeriodID: fiscalPeriodID, Command: command, EvidenceDocumentID: evidenceDocumentID}
	if readiness != nil {
		issues := append([]string{}, readiness.BlockingIssues...)
		sort.Strings(issues)
		doc.Readiness = &ReadinessSnapshot{IsReady: readiness.IsReady, BlockingIssues: issues}
	}
	b, _ := json.Marshal(doc) // cannot fail: only strings, bool, int
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
