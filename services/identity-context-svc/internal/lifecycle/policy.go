// Package lifecycle owns the subject lifecycle (AU-09; ZS-IAM-001 §2, §22,
// §23; Doc 05 §5.3; ZS-SEC-001 §10.1).
//
// Before it, principals existed only through seed tools: nothing created a
// subject, invited a person, set or changed a password, or recovered one, and
// identity.subject.created / identity.subject.disabled were never produced.
//
//   - Administrators create subjects (PENDING_ACTIVATION) and invite them: a
//     one-time activation link, delivered by notification-svc, on which the
//     person sets their own password. No administrator ever knows it.
//   - People change their own password with the current one.
//   - Recovery is separately controlled (IAM §3): a self-service reset sends
//     a link to the verified address and never reveals whether an account
//     exists; an administrator-initiated reset is independently approved
//     before any link is issued, and the requester can never approve it.
//   - Every new password meets the policy (NIST SP 800-63B-4 §3.1.1.2):
//     length, not the user's own identifiers, not a known-breached password,
//     not one of their recent ones.
//   - A password change or reset ends every session the person holds.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ErrPolicy is a password the policy refuses; the message says which rule.
var ErrPolicy = errors.New("password does not meet the policy")

// PolicyError explains a refusal.
type PolicyError struct {
	Rule   string
	Detail string
}

func (e *PolicyError) Error() string { return e.Detail }

// Is makes errors.Is(err, ErrPolicy) true.
func (e *PolicyError) Is(target error) bool { return target == ErrPolicy }

// Policy is the password policy.
type Policy struct {
	MinLength int
	MaxLength int
	Breach    BreachChecker
}

// Check validates password for a person identified by email and name.
func (p Policy) Check(ctx context.Context, password, email, displayName string) error {
	n := utf8.RuneCountInString(password)
	max := p.MaxLength
	if max <= 0 {
		max = 128
	}
	if n < p.MinLength {
		return &PolicyError{"min_length", fmt.Sprintf("use at least %d characters", p.MinLength)}
	}
	if n > max {
		return &PolicyError{"max_length", fmt.Sprintf("use at most %d characters", max)}
	}
	if strings.TrimSpace(password) == "" {
		return &PolicyError{"blank", "the password cannot be only spaces"}
	}
	lower := strings.ToLower(password)
	local, _, _ := strings.Cut(strings.ToLower(email), "@")
	for _, ident := range append([]string{local}, strings.Fields(strings.ToLower(displayName))...) {
		if len(ident) >= 4 && strings.Contains(lower, ident) {
			return &PolicyError{"contains_identifier", "the password must not contain your name or email"}
		}
	}
	if repeatsOneChar(password) {
		return &PolicyError{"repetitive", "the password cannot be one repeated character"}
	}
	if p.Breach != nil {
		breached, err := p.Breach.Breached(ctx, password)
		if err != nil {
			return fmt.Errorf("breached-password check unavailable: %w", err)
		}
		if breached {
			return &PolicyError{"breached", "this password appears in known data breaches; choose another"}
		}
	}
	return nil
}

func repeatsOneChar(s string) bool {
	var first rune
	for i, r := range s {
		if i == 0 {
			first = r
			continue
		}
		if r != first {
			return false
		}
	}
	return true
}
