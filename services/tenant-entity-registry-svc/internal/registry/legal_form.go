package registry

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
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
