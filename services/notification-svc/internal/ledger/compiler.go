package ledger

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	htmltmpl "html/template"
	"sort"
	"strings"
	texttmpl "text/template"
)

// Sentinel errors for template compilation and integrity verification.
var (
	ErrTemplateNotFound      = errors.New("template not found in registered catalog")
	ErrMissingVariables      = errors.New("missing required template variables")
	ErrIntegrityHashMismatch = errors.New("template integrity hash verification failed")
	ErrHashMismatch          = ErrIntegrityHashMismatch
	ErrNilVariablesMap       = errors.New("variables map cannot be nil")
)

// TemplateDefinition represents an approved immutable template in the registry per §9.
type TemplateDefinition struct {
	TemplateKey        string             `json:"template_key"`
	Version            string             `json:"version"`
	Locale             string             `json:"locale"`
	CommunicationClass CommunicationClass `json:"communication_class"`
	SenderStream       SenderStream       `json:"sender_stream"`
	SubjectTemplate    string             `json:"subject_template"`
	HTMLTemplate       string             `json:"html_template"`
	TextTemplate       string             `json:"text_template"`
	RequiredVariables  []string           `json:"required_variables"`
	ExpectedSHA256Hash string             `json:"expected_sha256_hash"`
}

// ComputeContentHash calculates the deterministic SHA-256 hex digest of a template definition.
func ComputeContentHash(key, version, locale, subjectTmpl, htmlTmpl, textTmpl string) string {
	h := sha256.New()
	canonicalPayload := fmt.Sprintf("%s|%s|%s|%s|%s|%s", key, version, locale, subjectTmpl, htmlTmpl, textTmpl)
	h.Write([]byte(canonicalPayload))
	return hex.EncodeToString(h.Sum(nil))
}

// CompiledTemplate holds the pre-parsed HTML and Plain-Text templates and metadata.
type CompiledTemplate struct {
	Def         TemplateDefinition
	parsedHTML  *htmltmpl.Template
	parsedText  *texttmpl.Template
	parsedSubj  *texttmpl.Template
	contentHash string
}

// Compiler manages registration, verification, and execution of email templates.
type Compiler struct {
	templates map[string]*CompiledTemplate
}

// NewCompiler constructs an empty template compiler.
func NewCompiler() *Compiler {
	return &Compiler{
		templates: make(map[string]*CompiledTemplate),
	}
}

// Register compiles a TemplateDefinition, asserting its content hash against ExpectedSHA256Hash.
// If the hash does not match, registration fails closed immediately.
func (c *Compiler) Register(def TemplateDefinition) error {
	if def.TemplateKey == "" {
		return errors.New("template_key is required")
	}
	if def.Version == "" {
		return errors.New("template version is required")
	}
	if def.Locale == "" {
		def.Locale = "en-US"
	}

	computedHash := ComputeContentHash(def.TemplateKey, def.Version, def.Locale, def.SubjectTemplate, def.HTMLTemplate, def.TextTemplate)
	if def.ExpectedSHA256Hash != "" && !strings.EqualFold(computedHash, def.ExpectedSHA256Hash) {
		return fmt.Errorf("%w: template %q version %q computed %s != expected %s",
			ErrIntegrityHashMismatch, def.TemplateKey, def.Version, computedHash, def.ExpectedSHA256Hash)
	}

	// Parse Subject (using text/template)
	subjT, err := texttmpl.New(def.TemplateKey + "_subject").Option("missingkey=error").Parse(def.SubjectTemplate)
	if err != nil {
		return fmt.Errorf("compile subject template for %q: %w", def.TemplateKey, err)
	}

	// Parse Plain Text (using text/template)
	textT, err := texttmpl.New(def.TemplateKey + "_text").Option("missingkey=error").Parse(def.TextTemplate)
	if err != nil {
		return fmt.Errorf("compile text template for %q: %w", def.TemplateKey, err)
	}

	// Parse HTML (using html/template with automatic contextual escaping)
	htmlT, err := htmltmpl.New(def.TemplateKey + "_html").Option("missingkey=error").Parse(def.HTMLTemplate)
	if err != nil {
		return fmt.Errorf("compile html template for %q: %w", def.TemplateKey, err)
	}

	c.templates[def.TemplateKey] = &CompiledTemplate{
		Def:         def,
		parsedHTML:  htmlT,
		parsedText:  textT,
		parsedSubj:  subjT,
		contentHash: computedHash,
	}

	return nil
}

// RenderResult contains the twin outputs and metadata.
type RenderResult struct {
	TemplateKey     string
	TemplateVersion string
	Locale          string
	ContentHash     string
	Subject         string
	BodyHTML        string
	BodyText        string
}

// Render evaluates the registered template against the supplied variables map.
func (c *Compiler) Render(templateKey string, vars map[string]string) (*RenderResult, error) {
	if vars == nil {
		return nil, ErrNilVariablesMap
	}

	ct, exists := c.templates[templateKey]
	if !exists {
		return nil, fmt.Errorf("%w: %q", ErrTemplateNotFound, templateKey)
	}

	// 1. Strict required variables validation
	var missing []string
	for _, req := range ct.Def.RequiredVariables {
		val, ok := vars[req]
		if !ok || strings.TrimSpace(val) == "" {
			missing = append(missing, req)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("%w: template %q missing %v", ErrMissingVariables, templateKey, missing)
	}

	// 2. Build structured data map to support dotted notation (e.g. {{.recipient.first_name}} or {{index . "recipient.first_name"}})
	// We pass both raw flat map and nested structure
	evalData := make(map[string]any, len(vars))
	for k, v := range vars {
		evalData[k] = v
		// Also support sub-maps if key contains dot: e.g. "recipient.first_name" -> evalData["recipient"]["first_name"]
		if parts := strings.Split(k, "."); len(parts) == 2 {
			if _, ok := evalData[parts[0]]; !ok {
				evalData[parts[0]] = make(map[string]any)
			}
			if subMap, ok := evalData[parts[0]].(map[string]any); ok {
				subMap[parts[1]] = v
			}
		}
	}

	// 3. Render Subject
	var subjBuf bytes.Buffer
	if err := ct.parsedSubj.Execute(&subjBuf, evalData); err != nil {
		return nil, fmt.Errorf("render subject for %q: %w", templateKey, err)
	}

	// 4. Render Plain-Text
	var textBuf bytes.Buffer
	if err := ct.parsedText.Execute(&textBuf, evalData); err != nil {
		return nil, fmt.Errorf("render text for %q: %w", templateKey, err)
	}

	// 5. Render HTML (contextually escaped)
	var htmlBuf bytes.Buffer
	if err := ct.parsedHTML.Execute(&htmlBuf, evalData); err != nil {
		return nil, fmt.Errorf("render html for %q: %w", templateKey, err)
	}

	return &RenderResult{
		TemplateKey:     ct.Def.TemplateKey,
		TemplateVersion: ct.Def.Version,
		Locale:          ct.Def.Locale,
		ContentHash:     ct.contentHash,
		Subject:         strings.TrimSpace(subjBuf.String()),
		BodyHTML:        htmlBuf.String(),
		BodyText:        textBuf.String(),
	}, nil
}

// GetDefinition returns the registered definition if present.
func (c *Compiler) GetDefinition(templateKey string) (TemplateDefinition, bool) {
	ct, ok := c.templates[templateKey]
	if !ok {
		return TemplateDefinition{}, false
	}
	return ct.Def, true
}
