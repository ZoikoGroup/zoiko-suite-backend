package domain

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"text/template"
	"text/template/parse"
	"unicode"
)

// Template subjects (ZS-SVC-Y-001 NCD-01 sections 4.1 and 4.4, INV-17, NP-08, NP-32).
//
// A subject is plain text with {{.variable}} placeholders and nothing else: no
// conditionals, no loops, no pipelines, no function calls. That keeps what a
// reviewer reads equal to what is sent, and keeps logic out of a header. The
// author also lists, in subject_variables, which variables may appear in it; a
// subject may reference nothing outside that list, and the list must be a subset
// of the version's variable_schema (a database CHECK says so too).

const maxSubjectLength = 200

// Subject errors.
const (
	ErrSubjectInvalid = errorString("template subject is invalid")
)

// SubjectProblem explains why a subject was refused; it matches ErrSubjectInvalid.
type SubjectProblem struct{ Reason string }

func (e SubjectProblem) Error() string { return "template subject is invalid: " + e.Reason }
func (e SubjectProblem) Is(target error) bool {
	return target == ErrSubjectInvalid
}

// ParseSubject checks a subject template and returns the variables it references,
// sorted. It refuses anything but text and plain {{.name}} placeholders.
func ParseSubject(subject string) ([]string, error) {
	if strings.TrimSpace(subject) == "" || len(subject) > maxSubjectLength {
		return nil, SubjectProblem{fmt.Sprintf("a subject is 1 to %d characters", maxSubjectLength)}
	}
	if strings.ContainsAny(subject, "\r\n") {
		return nil, SubjectProblem{"a subject is a single line"}
	}
	tmpl, err := template.New("subject").Option("missingkey=error").Parse(subject)
	if err != nil {
		return nil, SubjectProblem{"it does not parse: " + err.Error()}
	}
	seen := map[string]bool{}
	if tmpl.Tree == nil || tmpl.Tree.Root == nil {
		return nil, SubjectProblem{"it is empty"}
	}
	for _, node := range tmpl.Tree.Root.Nodes {
		switch n := node.(type) {
		case *parse.TextNode:
		case *parse.ActionNode:
			name, ok := plainField(n)
			if !ok {
				return nil, SubjectProblem{"only {{.variable}} placeholders are allowed, not " + n.String()}
			}
			seen[name] = true
		default:
			return nil, SubjectProblem{"only text and {{.variable}} placeholders are allowed, not " + node.String()}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// plainField reports the variable name of an action that is exactly {{.name}}.
func plainField(n *parse.ActionNode) (string, bool) {
	if n.Pipe == nil || len(n.Pipe.Decl) != 0 || len(n.Pipe.Cmds) != 1 {
		return "", false
	}
	cmd := n.Pipe.Cmds[0]
	if len(cmd.Args) != 1 {
		return "", false
	}
	field, ok := cmd.Args[0].(*parse.FieldNode)
	if !ok || len(field.Ident) != 1 {
		return "", false
	}
	return field.Ident[0], true
}

// CheckSubjectVariables verifies the author's declarations: every placeholder is in
// subjectVariables, and subjectVariables is a subset of schema.
func CheckSubjectVariables(subject string, subjectVariables, schema []string) error {
	used, err := ParseSubject(subject)
	if err != nil {
		return err
	}
	inSchema := toSet(schema)
	allowed := toSet(subjectVariables)
	for v := range allowed {
		if !inSchema[v] {
			return SubjectProblem{fmt.Sprintf("subject_variables names %q, which the version does not declare", v)}
		}
	}
	for _, v := range used {
		if !allowed[v] {
			return SubjectProblem{fmt.Sprintf("the subject uses %q, which is not listed in subject_variables (the author must declare it safe for a subject)", v)}
		}
	}
	return nil
}

// RenderSubject fills the placeholders. It refuses a value that would break the
// header (a control character or a newline) rather than stripping it, and a result
// longer than a subject may be.
func RenderSubject(subject string, vars map[string]string) (string, error) {
	if _, err := ParseSubject(subject); err != nil {
		return "", err
	}
	tmpl, err := template.New("subject").Option("missingkey=error").Parse(subject)
	if err != nil {
		return "", SubjectProblem{"it does not parse: " + err.Error()}
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, vars); err != nil {
		return "", SubjectProblem{"it could not be filled: " + err.Error()}
	}
	out := buf.String()
	for _, r := range out {
		if unicode.IsControl(r) {
			return "", SubjectProblem{"a value contains a control character or a line break"}
		}
	}
	if strings.TrimSpace(out) == "" || len(out) > maxSubjectLength {
		return "", SubjectProblem{fmt.Sprintf("the filled subject must be 1 to %d characters", maxSubjectLength)}
	}
	return out, nil
}

func toSet(list []string) map[string]bool {
	m := make(map[string]bool, len(list))
	for _, s := range list {
		m[s] = true
	}
	return m
}
