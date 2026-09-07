package templates_test

import (
	"strings"
	"testing"

	"zoiko.io/notification-svc/internal/templates"
)

// Every catalogue template must parse and render with its required variables
// supplied. This is what catches a placeholder mistyped during the port.
func TestRender_AllTemplates(t *testing.T) {
	vars := map[string]string{
		"organization_name":  "Acme Logistics",
		"login_url":          "https://app.example.com/login",
		"first_name":         "Asha",
		"temporary_password": "Tmp-4821-Xy",
		"reason":             "Registration documents were incomplete",
	}

	for _, name := range templates.Names() {
		t.Run(name, func(t *testing.T) {
			subject, body, err := templates.Render(name, vars)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if subject == "" {
				t.Error("subject must not be empty")
			}
			if !strings.Contains(body, "<html") {
				t.Errorf("body does not look like HTML: %.80s", body)
			}
			// An unsubstituted placeholder means the port missed one.
			if strings.Contains(body, "{{") {
				t.Errorf("body still contains an unrendered placeholder: %s", body)
			}
			if strings.Contains(body, "<no value>") {
				t.Errorf("body contains a Go template no-value marker: %s", body)
			}
		})
	}
}

func TestRender_SubstitutesVariables(t *testing.T) {
	_, body, err := templates.Render(templates.Approved, map[string]string{
		"organization_name": "Acme Logistics",
		"login_url":         "https://app.example.com/login",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(body, "Acme Logistics") {
		t.Error("organization name was not substituted")
	}
	if !strings.Contains(body, "https://app.example.com/login") {
		t.Error("login url was not substituted")
	}
}

// The whole reason for moving off string replacement: operator-supplied text
// must not be able to inject markup into a recipient's inbox.
func TestRender_EscapesInjectedMarkup(t *testing.T) {
	_, body, err := templates.Render(templates.Rejected, map[string]string{
		"organization_name": "Acme",
		"reason":            `<script>alert(1)</script>`,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(body, "<script>") {
		t.Errorf("script tag reached the body unescaped: %s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("expected the reason to be escaped, got: %s", body)
	}
}

// A URL variable lands in an href, where html/template applies URL filtering.
func TestRender_FiltersDangerousURL(t *testing.T) {
	_, body, err := templates.Render(templates.Approved, map[string]string{
		"organization_name": "Acme",
		"login_url":         "javascript:alert(1)",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(body, "javascript:alert(1)") {
		t.Errorf("a javascript: URL survived into an href: %s", body)
	}
}

// rejected.html carries a conditional block ported from Handlebars.
func TestRender_RejectedReasonIsOptional(t *testing.T) {
	_, withReason, err := templates.Render(templates.Rejected, map[string]string{
		"organization_name": "Acme",
		"reason":            "Incomplete documents",
	})
	if err != nil {
		t.Fatalf("render with reason: %v", err)
	}
	if !strings.Contains(withReason, "Incomplete documents") {
		t.Error("reason was not rendered when supplied")
	}

	_, withoutReason, err := templates.Render(templates.Rejected, map[string]string{
		"organization_name": "Acme",
	})
	if err != nil {
		t.Fatalf("render without reason: %v", err)
	}
	if strings.Contains(withoutReason, "Reason:") {
		t.Errorf("the reason block should be omitted when no reason is given: %s", withoutReason)
	}
}

func TestRender_MissingRequiredVariables(t *testing.T) {
	_, _, err := templates.Render(templates.PasswordReset, map[string]string{
		"first_name": "Asha",
	})
	if err == nil {
		t.Fatal("expected an error when required variables are missing")
	}
	var missing templates.ErrMissingVariables
	if !asMissing(err, &missing) {
		t.Fatalf("expected ErrMissingVariables, got %T: %v", err, err)
	}
	if len(missing.Missing) != 2 {
		t.Errorf("expected temporary_password and login_url to be reported, got %v", missing.Missing)
	}
}

// Whitespace-only is as unusable as absent â€” it would render a blank line where
// the organization name should be.
func TestRender_BlankVariableTreatedAsMissing(t *testing.T) {
	_, _, err := templates.Render(templates.Suspended, map[string]string{
		"organization_name": "   ",
	})
	if err == nil {
		t.Fatal("expected a whitespace-only required variable to be rejected")
	}
}

func TestRender_UnknownTemplate(t *testing.T) {
	_, _, err := templates.Render("does_not_exist", nil)
	if err == nil {
		t.Fatal("expected an error for an unknown template")
	}
	var unknown templates.ErrUnknownTemplate
	if !asUnknown(err, &unknown) {
		t.Fatalf("expected ErrUnknownTemplate, got %T: %v", err, err)
	}
	// The message should help the caller find the right name.
	if !strings.Contains(err.Error(), templates.Approved) {
		t.Errorf("expected known template names in the error, got: %v", err)
	}
}

func TestNames_ListsWholeCatalogue(t *testing.T) {
	names := templates.Names()
	if len(names) != 6 {
		t.Fatalf("expected 6 ported templates, got %d: %v", len(names), names)
	}
	for _, want := range []string{
		templates.RegistrationReceived, templates.Approved, templates.Rejected,
		templates.Suspended, templates.Reactivated, templates.PasswordReset,
	} {
		found := false
		for _, got := range names {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("catalogue is missing %q", want)
		}
	}
}

func asMissing(err error, target *templates.ErrMissingVariables) bool {
	e, ok := err.(templates.ErrMissingVariables)
	if ok {
		*target = e
	}
	return ok
}

func asUnknown(err error, target *templates.ErrUnknownTemplate) bool {
	e, ok := err.(templates.ErrUnknownTemplate)
	if ok {
		*target = e
	}
	return ok
}

// Catalogue is what the console builds its form from, so a template whose
// required variables it advertises wrongly produces a form that cannot be
// submitted successfully. Every entry is therefore checked against Render
// itself rather than against a second copy of the same list.
func TestCatalogue_MatchesWhatRenderActuallyRequires(t *testing.T) {
	catalogue := templates.Catalogue()
	if len(catalogue) != len(templates.Names()) {
		t.Fatalf("Catalogue has %d entries, Names has %d", len(catalogue), len(templates.Names()))
	}

	for _, entry := range catalogue {
		if entry.Subject == "" {
			t.Errorf("%s: advertises no subject", entry.Name)
		}

		// Supplying exactly what the catalogue advertises must render.
		vars := map[string]string{}
		for _, key := range entry.Required {
			vars[key] = "x"
		}
		if _, _, err := templates.Render(entry.Name, vars); err != nil {
			t.Errorf("%s: advertised variables %v were not enough: %v", entry.Name, entry.Required, err)
		}

		// And each one must genuinely be required — an over-declared variable
		// would put a field on the form that the template does not need.
		for _, key := range entry.Required {
			short := map[string]string{}
			for _, k := range entry.Required {
				if k != key {
					short[k] = "x"
				}
			}
			if _, _, err := templates.Render(entry.Name, short); err == nil {
				t.Errorf("%s: %q is advertised as required but renders without it", entry.Name, key)
			}
		}
	}
}

// The catalogue must be sorted and must not hand out the package's own slices:
// a caller that sorts or truncates Required in place would change what every
// later caller believes the template needs.
func TestCatalogue_IsSortedAndDefensivelyCopied(t *testing.T) {
	first := templates.Catalogue()
	for i := 1; i < len(first); i++ {
		if first[i-1].Name >= first[i].Name {
			t.Fatalf("catalogue is not sorted: %q before %q", first[i-1].Name, first[i].Name)
		}
	}

	for i := range first {
		for j := range first[i].Required {
			first[i].Required[j] = "clobbered"
		}
	}

	for _, entry := range templates.Catalogue() {
		for _, key := range entry.Required {
			if key == "clobbered" {
				t.Fatalf("%s: Required is shared with package state, not copied", entry.Name)
			}
		}
	}
}