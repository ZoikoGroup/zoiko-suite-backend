package handler

import (
	"net/http"
	"strings"

	"zoiko.io/jurisdiction-rules-svc/internal/events"
)

// ZS-JUR-001 s23: POST /v1/admin/packs/{pack_ref}/versions/{version}/supersede.

type SupersedeRequest struct {
	// SupersededBy is the newer version of the same pack that takes over.
	SupersededBy string `json:"superseded_by"`
	Reason       string `json:"reason"`
	EvidenceRef  string `json:"evidence_ref"`
}

// SupersedePackVersion marks a RELEASED version SUPERSEDED by a newer RELEASED version of the same pack. A superseded version stays resolvable for
// pinned replay but is no longer eligible for new decisions, so it is refused (409 operation_blocked, every reason at once) unless the newer version
// covers the older one's period, jurisdictions, rules and obligations and is deployed wherever the older is. A repeat changes nothing.
//
//	200 superseded (or already)      400 missing superseded_by or reason      404 no such version
//	409 operation_blocked or invalid_lifecycle_state
func (h *Handler) SupersedePackVersion(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "jurisdiction_pack_version", "supersede")
	if !ok {
		return
	}
	var req SupersedeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if field := firstBlank(requiredField{"superseded_by", req.SupersededBy}, requiredField{"reason", req.Reason}); field != "" {
		writeMissingField(w, field)
		return
	}
	ref, version := pathRefVersion(r)
	res, err := h.registry.SupersedePackVersion(r.Context(), ref, version, strings.TrimSpace(req.SupersededBy), strings.TrimSpace(req.Reason), strings.TrimSpace(req.EvidenceRef), actor)
	if err != nil {
		h.writeOpsError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	if res.Changed {
		h.emitRegistry(r, events.EventPackSuperseded, ref+"@"+version, actor, map[string]any{"pack_ref": ref, "version": version, "superseded_by": req.SupersededBy,
			"from_status": res.FromStatus, "to_status": res.ToStatus, "reason": req.Reason, "evidence_ref": req.EvidenceRef})
	}
	writeJSON(w, http.StatusOK, res)
}
