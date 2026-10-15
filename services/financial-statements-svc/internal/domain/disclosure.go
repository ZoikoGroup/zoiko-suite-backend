package domain

import (
	"strings"
	"time"
	"unicode/utf8"
)

// Disclosure limits. Generous for a note, small enough that one request
// cannot store an unbounded blob in a statutory record.
const (
	MaxDisclosureTitleRunes = 200
	MaxDisclosureBodyRunes  = 20000
)

// DisclosureInstance is one narrative note attached to a statement set
// (ZS-SVC-H-001 §4.1 data ownership; GetDisclosure). Like StatementFact it
// has no lifecycle of its own: it is content of its parent set, can only be
// added while the set is DRAFT, and is frozen with the set from then on —
// certification freezes "report content … and disclosures". There is no
// edit or delete; a correction after DRAFT is a superseding set.
type DisclosureInstance struct {
	DisclosureID   string `json:"disclosure_id"`
	TenantID       string `json:"tenant_id"`
	StatementSetID string `json:"statement_set_id"`
	// Sequence is the note's 1-based position in its set ("Note 1", "Note
	// 2"), assigned in add order under the parent set's row lock.
	Sequence           int       `json:"sequence"`
	Title              string    `json:"title"`
	Body               string    `json:"body"`
	AddedByPrincipalID string    `json:"added_by_principal_id"`
	AddedAt            time.Time `json:"added_at"`
	// IdempotencyKey makes a retried add return the original note instead
	// of creating a duplicate. Never echoed to clients.
	IdempotencyKey string `json:"-"`
}

// NormalizeDisclosure trims and validates a disclosure's text, returning
// the values to store.
func NormalizeDisclosure(title, body string) (string, string, error) {
	title, body = strings.TrimSpace(title), strings.TrimSpace(body)
	if title == "" || body == "" {
		return "", "", Errf(CodeValidationFailed, "disclosure title and body are both required")
	}
	if n := utf8.RuneCountInString(title); n > MaxDisclosureTitleRunes {
		return "", "", Errf(CodeValidationFailed, "disclosure title is %d characters; the limit is %d", n, MaxDisclosureTitleRunes)
	}
	if n := utf8.RuneCountInString(body); n > MaxDisclosureBodyRunes {
		return "", "", Errf(CodeValidationFailed, "disclosure body is %d characters; the limit is %d", n, MaxDisclosureBodyRunes)
	}
	return title, body, nil
}

// SameContent reports whether a replayed add carries the same note.
func (d DisclosureInstance) SameContent(title, body string) bool {
	return d.Title == title && d.Body == body
}
