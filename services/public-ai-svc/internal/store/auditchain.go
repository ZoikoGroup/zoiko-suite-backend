package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// AuditRowHash chains one control-audit row to the one before it (Doc 06
// §21): any deleted, reordered or edited row breaks every later hash.
func AuditRowHash(prev string, e AuditEntry) string {
	fields := []string{prev, e.Action, e.Target, e.Actor, e.ActorRole, e.Approval, e.Reason, e.RequestID,
		e.BeforeHash, e.AfterHash, e.Environment, e.At.UTC().Format(time.RFC3339Nano)}
	for i, f := range fields {
		fields[i] = strconv.Itoa(len(f)) + ":" + f // length-prefixed: no field can bleed into the next
	}
	sum := sha256.Sum256([]byte(strings.Join(fields, "|")))
	return hex.EncodeToString(sum[:])
}

// VerifyAuditChain checks entries in append order (oldest first). Rows from
// before the chain existed (empty hashes) may only precede the first hashed
// row. It returns the audit id of the first broken row.
func VerifyAuditChain(entries []AuditEntry) error {
	prev, started := "", false
	for _, e := range entries {
		if e.RowHash == "" {
			if started {
				return fmt.Errorf("audit %d: unhashed row after the chain started", e.AuditID)
			}
			continue
		}
		if started && e.PrevHash != prev {
			return fmt.Errorf("audit %d: prev_hash does not match the row before it (row removed or reordered)", e.AuditID)
		}
		if AuditRowHash(e.PrevHash, e) != e.RowHash {
			return fmt.Errorf("audit %d: row_hash does not match its contents (row edited)", e.AuditID)
		}
		prev, started = e.RowHash, true
	}
	return nil
}
