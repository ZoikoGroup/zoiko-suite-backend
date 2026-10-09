package domain

import "time"

// ── Change classification, staleness, residency (AA-001 §3, §10.1) ───────────

// Material keys. S2/S3 are the authority-bearing safety classes: §8 gives
// their changes impact analysis, approval, effective time, propagation
// verification and rollback, so they never change by a direct write.
func IsMaterial(safetyClass string) bool {
	return safetyClass == SafetyS2 || safetyClass == SafetyS3
}

// changeClassRank orders change classes C0 < C1 < C2 < C3.
var changeClassRank = map[string]int{ChangeClassC0: 0, ChangeClassC1: 1, ChangeClassC2: 2, ChangeClassC3: 3}

// MinChangeClass is the least change class that may carry a part touching a
// key of the given safety class: S2 needs C2, S3 needs C3, anything else C0.
func MinChangeClass(safetyClass string) string {
	switch safetyClass {
	case SafetyS3:
		return ChangeClassC3
	case SafetyS2:
		return ChangeClassC2
	default:
		return ChangeClassC0
	}
}

// ChangeClassCovers reports whether a change of class have may carry a part
// that needs class need.
func ChangeClassCovers(have, need string) bool {
	h, ok := changeClassRank[have]
	return ok && h >= changeClassRank[need]
}

// SnapshotRefreshMargin is how long before its freshness deadline the sweep
// re-mints an environment's snapshot. Snapshots are otherwise minted only on
// writes, so an environment nobody writes to for a day would go stale and —
// under INV-13 — stop serving its protected keys. Half the 24h placeholder
// deadline (OD-04).
const SnapshotRefreshMargin = 12 * time.Hour

// Resolution reasons added for INV-13, INV-26 and INV-28.
const (
	// The served snapshot is past its freshness deadline and the key is
	// material: it is withheld rather than served stale (INV-13).
	ReasonStaleSnapshot = "STALE_SNAPSHOT"
	// The key's declared residency does not include the caller's
	// gateway-verified jurisdiction (INV-26).
	ReasonResidency = "RESIDENCY"
)

// Refusals for the controls in this file.
var (
	ErrChangeNotYetEffective = newCodedError("change_not_yet_effective",
		"the change's planned effective time has not arrived")
	ErrChangeClassInsufficient = newCodedError("change_class_insufficient",
		"a part touches a key whose safety class needs a higher change class")
	ErrMaterialKeyRequiresChange = newCodedError("material_key_requires_change",
		"an S2/S3 key changes only through an approved change set, never a direct write")
	ErrApprovalReferenceRequired = newCodedError("approval_reference_required",
		"publishing an S2/S3 definition requires an approval reference")
	ErrFlagNotRetired = newCodedError("flag_not_retired",
		"only a RETIRED flag key can be marked removed")
	// AA-001 §8.1: C2 needs WFC approval and C3 "segregated approval"; the
	// proposer approving their own change is neither (S3-1 / R-4).
	ErrChangeSelfApproval = newCodedError("change_self_approval",
		"the proposer of a C2/C3 change cannot approve it; segregated approval needs another principal")
	// §8 lifecycle: only a change still awaiting a decision can be approved
	// or rejected. A VERIFIED change sent back to APPROVED became
	// activatable again (S3-2 / R-5).
	ErrChangeNotApprovable = newCodedError("change_not_approvable",
		"only a PROPOSED or VALIDATED change can be approved or rejected")
)

// AllowsRegion reports whether a declaration admits delivery to region. An
// empty allowlist is unrestricted; a restricted key is never delivered to a
// caller whose jurisdiction is unknown.
func AllowsRegion(def *ConfigDefinition, region string) bool {
	if def == nil || len(def.AllowedRegions) == 0 {
		return true
	}
	for _, r := range def.AllowedRegions {
		if r == region {
			return true
		}
	}
	return false
}
