package handler

import (
	"context"
	"errors"
	"net/http"

	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/certreliance"
	"zoiko.io/general-ledger-svc/internal/domain"
)

// CertificationRelianceRecorder records that a journal relied on a certification (internal/certreliance). A nil recorder means
// this deployment has not wired it (tests); a journal that cites no certification never reaches it.
type CertificationRelianceRecorder interface {
	RecordReliance(ctx context.Context, tenantID, principalID, correlationID string, ref certreliance.Ref, obj certreliance.Object) error
}

// WithCertificationReliance wires the recorder that makes a posted journal a recorded consumer of the certifications its
// evidence cites. Without it, a journal that cites a certification cannot be posted (see recordCertificationReliance).
func (h *Handler) WithCertificationReliance(c CertificationRelianceRecorder) *Handler {
	h.certs = c
	return h
}

// recordCertificationReliance runs when a journal is about to be finalized. For each certification the journal's evidence
// cites ("certification:<service>:<run id>") it records the journal's reliance with the service that issued it. It fails
// CLOSED, like the evidence gate beside it:
//
//   - a malformed or unknown certification reference is refused (422), never skipped;
//   - a certification that is revoked, superseded or past its validity window is refused (422): a journal is not posted on
//     withdrawn evidence;
//   - a certification service that cannot answer, or is not configured, refuses the posting (503): posting without recording
//     would leave a journal resting on certified evidence that nobody would be told about if it were revoked.
//
// Recording happens BEFORE the journal is finalized and is idempotent, so a posting that fails afterwards leaves a reliance
// for a journal that did not post. That errs on the side of over-naming a journal in a blast radius, never under-naming it,
// and a retry records nothing new. A journal that cites no certification is untouched.
func (h *Handler) recordCertificationReliance(w http.ResponseWriter, r *http.Request, header *domain.JournalHeader, principalID string) bool {
	refs, err := certreliance.ParseRefs(header.EvidenceRefs)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_certification_ref", err.Error())
		return false
	}
	if len(refs) == 0 {
		return true
	}
	if h.certs == nil {
		h.log.Error("journal cites a certification but reliance recording is not wired — failing closed", zap.String("journal_id", header.JournalID))
		writeError(w, http.StatusServiceUnavailable, "certification_service_unavailable", "")
		return false
	}
	obj := certreliance.Object{ID: header.JournalID, LegalEntityID: header.LegalEntityID}
	for _, ref := range refs {
		err := h.certs.RecordReliance(r.Context(), header.TenantID, principalID, r.Header.Get("X-Correlation-ID"), ref, obj)
		switch {
		case err == nil:
		case errors.Is(err, certreliance.ErrNotInForce):
			writeError(w, http.StatusUnprocessableEntity, "certification_not_in_force", ref.String()+" is revoked, superseded or past its validity window")
			return false
		case errors.Is(err, certreliance.ErrUnknown):
			writeError(w, http.StatusUnprocessableEntity, "certification_unknown", ref.String()+" does not exist")
			return false
		case errors.Is(err, certreliance.ErrDenied):
			writeError(w, http.StatusForbidden, "certification_reliance_denied", "")
			return false
		default:
			h.log.Error("certification reliance could not be recorded — failing closed", zap.String("journal_id", header.JournalID),
				zap.String("certification", ref.String()), zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "certification_service_unavailable", "")
			return false
		}
	}
	return true
}
