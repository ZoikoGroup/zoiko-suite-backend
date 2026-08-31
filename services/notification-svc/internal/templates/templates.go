// Package templates renders the platform's transactional email bodies.
//
// The HTML was ported from zoiko-one, where it was rendered by replacing
// {{key}} with a raw string. That is not safe here: a rejection reason is
// operator-supplied free text, and pasted straight into markup it would let
// whoever wrote it put arbitrary HTML into somebody else's inbox. These
// templates are parsed by html/template instead, which escapes each value for
// the context it lands in.
package templates

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"sort"
	"strings"
)

//go:embed assets/*.html
var assets embed.FS

// Template names, as callers refer to them.
const (
	RegistrationReceived = "registration_received"
	Approved             = "approved"
	Rejected             = "rejected"
	Suspended            = "suspended"
	Reactivated          = "reactivated"
	PasswordReset        = "password_reset"
)

// spec describes one template: its subject line, and the variables it requires.
//
// Subjects are new. zoiko-one logged bodies without ever setting one, because
// it never actually sent mail; a real message needs a subject, and these mirror
// each template's own heading.
type spec struct {
	subject  string
	required []string
}

var specs = map[string]spec{
	RegistrationReceived: {
		subject:  "Organization registration received",
		required: []string{"organization_name"},
	},
	Approved: {
		subject:  "Your organization has been approved",
		required: []string{"organization_name", "login_url"},
	},
	Rejected: {
		// reason is optional: the template omits the callout block when absent.
		subject:  "Your organization registration was not approved",
		required: []string{"organization_name"},
	},
	Suspended: {
		subject:  "Your organization has been suspended",
		required: []string{"organization_name"},
	},
	Reactivated: {
		subject:  "Your organization has been reactivated",
		required: []string{"organization_name", "login_url"},
	},
	PasswordReset: {
		subject:  "Your password has been reset",
		required: []string{"first_name", "temporary_password", "login_url"},
	},
}

// parsed holds every template, compiled once at package init. A parse failure
// is a build-time mistake baked into the binary, so it panics rather than
// failing later on a request that had nothing to do with it.
var parsed = func() map[string]*template.Template {
	out := make(map[string]*template.Template, len(specs))
	for name := range specs {
		t, err := template.New(name+".html").
			// A variable the caller forgot renders empty rather than as the
			// literal "<no value>" in somebody's inbox. Required variables are
			// checked explicitly by Render.
			Option("missingkey=zero").
			ParseFS(assets, "assets/"+name+".html")
		if err != nil {
			panic(fmt.Sprintf("notification templates: parse %s: %v", name, err))
		}
		out[name] = t
	}
	return out
}()

// ErrUnknownTemplate is returned for a name that is not in the catalogue.
type ErrUnknownTemplate struct{ Name string }

func (e ErrUnknownTemplate) Error() string {
	return fmt.Sprintf("unknown notification template %q; known templates: %s",
		e.Name, strings.Join(Names(), ", "))
}

// ErrMissingVariables is returned when a template's required variables are not
// all supplied. Rendering half a message is worse than refusing: the recipient
// would get an email with a blank organization name or an empty login link.
type ErrMissingVariables struct {
	Template string
	Missing  []string
}

func (e ErrMissingVariables) Error() string {
	return fmt.Sprintf("template %q requires %s", e.Template, strings.Join(e.Missing, ", "))
}

// Names lists the catalogue, sorted, for error messages and discovery.
func Names() []string {
	out := make([]string, 0, len(specs))
	for name := range specs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Render produces the subject and HTML body for one template.
//
// Values are escaped for the context they appear in, so an organization name
// containing markup arrives as text rather than as markup.
func Render(name string, vars map[string]string) (subject, body string, err error) {
	s, ok := specs[name]
	if !ok {
		return "", "", ErrUnknownTemplate{Name: name}
	}

	var missing []string
	for _, key := range s.required {
		if strings.TrimSpace(vars[key]) == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return "", "", ErrMissingVariables{Template: name, Missing: missing}
	}

	var buf bytes.Buffer
	if err := parsed[name].Execute(&buf, vars); err != nil {
		return "", "", fmt.Errorf("render %s: %w", name, err)
	}
	return s.subject, buf.String(), nil
}
