package domain

import (
	"crypto/sha256"
	"encoding/hex"
)

// ManifestChecksum is the manifest integrity checksum: a FLAT SHA-256 over the
// records, in the order given, of source_type || source_record_id ||
// record_snapshot for each. It is not a Merkle tree — the specification asks
// for a SHA-256 checksum, not a tree — so there is no per-record proof.
//
// The records MUST be in the canonical read order (fetched_at, then
// manifest_record_id; see PgStore.ListRecords). Generation and verification
// both hash the records as read back from the database, so the two cannot
// disagree about order or about JSONB normalisation of the snapshot.
func ManifestChecksum(records []ManifestRecord) string {
	h := sha256.New()
	for _, r := range records {
		h.Write([]byte(r.SourceType))
		h.Write([]byte(r.SourceRecordID))
		h.Write(r.RecordSnapshot)
	}
	return hex.EncodeToString(h.Sum(nil))
}
