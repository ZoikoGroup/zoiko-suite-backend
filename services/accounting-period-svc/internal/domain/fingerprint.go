package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// DecisionFingerprint binds one authorised decision: which period, which
// command from which state, and exactly which ACC-14 workflow and control
// snapshot (and, for a reopen, which scope/window) approved it. A workflow
// decision can be applied to a period only once (unique index in the schema).
func DecisionFingerprint(periodID string, cmd Command, from State, workflowRef, snapshotRef string, rw *ReopenWindow) string {
	parts := []string{periodID, string(cmd), string(from), workflowRef, snapshotRef}
	if rw != nil {
		parts = append(parts, rw.BookScope, rw.ModuleScope, strconv.FormatInt(rw.ExpiresAt.UTC().UnixMicro(), 10))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(sum[:])
}
