package registry

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

// ORG-03 mandatory control: "Legal-form mapping may use ISO 20275/ELF code
// where available, while preserving local legal-form text/source."
//
// An ELF code is four upper-case alphanumeric characters (GLEIF ELF list,
// e.g. H0PO, 8888 for "no ELF code"). Validation against the list itself
// needs the list imported under the §11 pre-production decision on
// authoritative code-list sources; until then the format is enforced and a
// code is never accepted without the source it came from and the local text
// it maps — the two things the control says must be preserved.
var elfCode = regexp.MustCompile(`^[A-Z0-9]{4}$`)

func validateLegalForm(code, source, localText *string) error {
	if code == nil || strings.TrimSpace(*code) == "" {
		return nil
	}
	c := strings.ToUpper(strings.TrimSpace(*code))
	if !elfCode.MatchString(c) {
		return fmt.Errorf("%w: legal_form_code %q is not an ISO 20275 ELF code (four alphanumeric characters)", ErrInvalidInput, *code)
	}
	*code = c
	if source == nil || strings.TrimSpace(*source) == "" {
		return fmt.Errorf("%w: legal_form_code needs legal_form_source (e.g. GLEIF-ELF-1.6)", ErrSourceUnverified)
	}
	if localText == nil || strings.TrimSpace(*localText) == "" {
		return fmt.Errorf("%w: legal_form_code needs legal_form_local_text — the local legal-form text must be preserved", ErrInvalidInput)
	}
	return nil
}

// validateFiscalCalendarRef checks the one thing this service can: that the
// reference is a well-formed id. §4.3's "permitted calendar/currency
// references" belongs to REF-04 Fiscal Calendar, which does not exist in the
// estate yet; there is nothing to resolve it against.
func validateFiscalCalendarRef(id string) error {
	if _, err := uuid.Parse(strings.TrimSpace(id)); err != nil {
		return fmt.Errorf("%w: fiscal_calendar_id must be a UUID", ErrInvalidInput)
	}
	return nil
}

// entityProfile holds the §4.3 source inputs that CreateEntity records on
// profile version 1, validated.
type entityProfile struct {
	legalFormCode, legalFormSource, legalFormLocalText     *string
	registryAuthority, registeredOffice, sourceEvidenceRef *string
}

// entityProfileInputs validates the §4.3 required source inputs a new entity's
// first profile version carries. The legal form is held to the same ISO 20275
// control as an amendment. The registered address and supporting evidence are
// required outside local development: §4.3 lists both, and before 29 Sep 2026
// neither could be supplied at creation at all.
func (s *Service) entityProfileInputs(req domain.CreateEntityRequest) (entityProfile, error) {
	p := entityProfile{
		legalFormCode:      nullableString(strings.TrimSpace(req.LegalFormCode)),
		legalFormSource:    nullableString(strings.TrimSpace(req.LegalFormSource)),
		legalFormLocalText: nullableString(strings.TrimSpace(req.LegalFormLocalText)),
		registryAuthority:  nullableString(strings.TrimSpace(req.RegistryAuthority)),
		sourceEvidenceRef:  nullableString(strings.TrimSpace(req.SourceEvidenceRef)),
	}
	if err := validateLegalForm(p.legalFormCode, p.legalFormSource, p.legalFormLocalText); err != nil {
		return p, err
	}
	if office := bytes.TrimSpace(req.RegisteredOffice); len(office) > 0 && !bytes.Equal(office, []byte("null")) {
		o := string(office)
		p.registeredOffice = &o
	}
	if s.legacyProvisioningInputs {
		return p, nil
	}
	if p.registeredOffice == nil {
		return p, fmt.Errorf("%w: registered_office is required (ORG-03 §4.3 required source input)", ErrInvalidInput)
	}
	if p.sourceEvidenceRef == nil {
		return p, fmt.Errorf("%w: source_evidence_ref is required (ORG-03 §4.3 supporting evidence)", ErrSourceUnverified)
	}
	return p, nil
}
