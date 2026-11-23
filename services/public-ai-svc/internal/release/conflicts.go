package release

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"

	"zoiko.io/public-ai-svc/internal/claims"
	"zoiko.io/public-ai-svc/internal/knowledge"
	"zoiko.io/public-ai-svc/internal/store"
)

// Conflict detection types (Doc 03 §16).
const (
	// DetectClaimConflict: two ACTIVE claims for one key disagree (KB-005).
	DetectClaimConflict = "CLAIM_CONFLICT"
	// DetectAuthorityMismatch: approved public copy states a different value
	// than the signed P1 claim (KB-003).
	DetectAuthorityMismatch = "AUTHORITY_MISMATCH"
	// DetectEvidenceMissing: an ACTIVE claim cites no source version or
	// citation key present in the active KB release (KB-018, KB-030).
	DetectEvidenceMissing = "EVIDENCE_NOT_IN_RELEASE"
)

// defaultOwners routes a case to the accountable team when the claim names
// no owner (Doc 03 §10 RACI).
var defaultOwners = map[string]string{
	claims.Price: "commercial-ops", claims.Trial: "commercial-ops", claims.PlanAvailability: "commercial-ops",
	claims.MarketAvailability: "regional-product", claims.Certification: "security-trust",
	claims.Residency: "privacy", claims.IntegrationStatus: "integration-owner",
}

// OwnerFor names the accountable owner for a claim type.
func OwnerFor(claimType, owner string) string {
	if owner != "" {
		return owner
	}
	if o, ok := defaultOwners[claimType]; ok {
		return o
	}
	return "knowledge-engineering"
}

// ConflictID is deterministic, so the refresh scan and a visitor turn that
// hit the same problem write one case, not two.
func ConflictID(kind, key string, ids []string) string {
	sum := sha256.Sum256([]byte(kind + "|" + key + "|" + strings.Join(ids, ",")))
	return "kb-conf-" + hex.EncodeToString(sum[:8])
}

// ScanConflicts cross-checks the active claim registry against itself and
// against the active KB release and returns one owner case per problem.
// IDs are deterministic, so a rescan never duplicates a case.
func ScanConflicts(kb *knowledge.Manifest, cm claims.Manifest, now time.Time) []store.Conflict {
	var out []store.Conflict
	add := func(kind, severity, key, claimType, owner string, ids []string, blocked bool) {
		out = append(out, store.Conflict{ConflictID: ConflictID(kind, key, ids), SubjectKey: key, ClaimIDs: ids, DetectionType: kind,
			Severity: severity, ResolutionState: "OPEN", DetectedAt: now, BlockedClaim: blocked, AssignedOwner: OwnerFor(claimType, owner)})
	}
	for _, c := range cm.Conflicts() {
		add(DetectClaimConflict, "HIGH", c.SubjectKey, strings.SplitN(c.SubjectKey, "|", 2)[0], c.OwnerID, c.ClaimIDs, true)
	}
	disputed := PriceDisputes(kb, cm)
	plans := make([]string, 0, len(disputed))
	for p := range disputed {
		plans = append(plans, p)
	}
	sort.Strings(plans)
	for _, p := range plans {
		var ids []string
		owner := ""
		for _, c := range cm.Claims {
			if c.ClaimType == claims.Price && c.SubjectID == p && c.Status == claims.StatusActive {
				ids = append(ids, c.ClaimID)
				if owner == "" {
					owner = c.OwnerID
				}
			}
		}
		sort.Strings(ids)
		add(DetectAuthorityMismatch, "HIGH", "PRICE|"+p, claims.Price, owner, ids, true)
	}
	if kb == nil {
		return out
	}
	versions, keys := map[string]bool{}, map[string]bool{}
	for _, s := range kb.Sources {
		versions[s.SourceVersionID], keys[s.URLKey] = true, true
	}
	for _, r := range kb.Routes {
		keys[r.RouteKey] = true
	}
	for _, c := range cm.Claims {
		if c.Status != claims.StatusActive {
			continue
		}
		found := false
		for _, v := range c.EvidenceSourceVersionIDs {
			found = found || versions[v]
		}
		if !found && !keys[c.CitationURLKey] {
			// Recorded for triage, not blocked: the evidence may sit in a
			// registry outside this KB release.
			add(DetectEvidenceMissing, "MEDIUM", c.ClaimType+"|"+c.ClaimID, c.ClaimType, c.OwnerID, []string{c.ClaimID}, false)
		}
	}
	return out
}
