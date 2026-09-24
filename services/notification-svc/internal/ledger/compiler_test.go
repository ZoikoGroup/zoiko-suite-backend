package ledger_test

import (
	"errors"
	"strings"
	"testing"

	"zoiko.io/notification-svc/internal/ledger"
)

func TestCompiler_SeedCatalogRegistrationAndIntegrity(t *testing.T) {
	c := ledger.NewCompiler()
	seeds := ledger.DefaultSeedDefinitions()

	if len(seeds) != 5 {
		t.Fatalf("expected 5 seed templates for Phase 1, got %d", len(seeds))
	}

	for _, s := range seeds {
		if err := c.Register(s); err != nil {
			t.Fatalf("expected seed template %q to register cleanly, got: %v", s.TemplateKey, err)
		}
	}
}

func TestCompiler_HashMismatchFailsClosed(t *testing.T) {
	c := ledger.NewCompiler()
	def := ledger.TemplateDefinition{
		TemplateKey:        "TEST-TAMPER-001",
		Version:            "1.0.0",
		Locale:             "en-US",
		CommunicationClass: ledger.ClassS0,
		SenderStream:       ledger.StreamCritical,
		SubjectTemplate:    "Security Alert",
		HTMLTemplate:       "<p>Authorized content</p>",
		TextTemplate:       "Authorized content",
		ExpectedSHA256Hash: "bad-tampered-hash-00000000000000000000000000000000000000000000000",
	}

	err := c.Register(def)
	if err == nil {
		t.Fatal("expected registration to fail with hash mismatch, but succeeded")
	}
	if !errors.Is(err, ledger.ErrIntegrityHashMismatch) {
		t.Fatalf("expected ErrIntegrityHashMismatch, got: %v", err)
	}
}

func TestCompiler_RenderSuccessfulWithParity(t *testing.T) {
	c := ledger.NewCompiler()
	for _, s := range ledger.DefaultSeedDefinitions() {
		if err := c.Register(s); err != nil {
			t.Fatalf("register failed: %v", err)
		}
	}

	vars := map[string]string{
		"recipient.first_name":           "Jane",
		"recipient.email_masked":         "j***@example.com",
		"security.link_expires_at_local": "2026-09-30 18:00 UTC",
		"links.action_url":               "https://app.zoiko.io/action/v1?token=secure123",
		"message.reference":              "MSG-REF-001",
	}

	res, err := c.Render("ZS-IA-001", vars)
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}

	if res.TemplateKey != "ZS-IA-001" {
		t.Errorf("expected template key ZS-IA-001, got %q", res.TemplateKey)
	}
	if res.Subject != "Verify your email for ZoikoSuite" {
		t.Errorf("unexpected subject: %q", res.Subject)
	}
	if !strings.Contains(res.BodyHTML, "Hi Jane,") {
		t.Errorf("HTML body missing recipient name: %s", res.BodyHTML)
	}
	if !strings.Contains(res.BodyText, "Verify Email Address: https://app.zoiko.io/action/v1?token=secure123") {
		t.Errorf("Text body missing CTA fallback link: %s", res.BodyText)
	}
	if res.ContentHash == "" {
		t.Error("expected non-empty ContentHash")
	}
}

func TestCompiler_MissingRequiredVariablesFailClosed(t *testing.T) {
	c := ledger.NewCompiler()
	for _, s := range ledger.DefaultSeedDefinitions() {
		_ = c.Register(s)
	}

	// Missing links.action_url and message.reference
	partialVars := map[string]string{
		"recipient.first_name":           "Jane",
		"recipient.email_masked":         "j***@example.com",
		"security.link_expires_at_local": "2026-09-30",
	}

	_, err := c.Render("ZS-IA-001", partialVars)
	if err == nil {
		t.Fatal("expected render to fail due to missing required variables, but succeeded")
	}
	if !errors.Is(err, ledger.ErrMissingVariables) {
		t.Fatalf("expected ErrMissingVariables, got: %v", err)
	}
	if !strings.Contains(err.Error(), "links.action_url") || !strings.Contains(err.Error(), "message.reference") {
		t.Errorf("expected error to name missing variables, got: %v", err)
	}
}

func TestCompiler_HTMLEscapingPreventsInjection(t *testing.T) {
	c := ledger.NewCompiler()
	for _, s := range ledger.DefaultSeedDefinitions() {
		_ = c.Register(s)
	}

	maliciousVars := map[string]string{
		"recipient.first_name":           "<script>alert('xss')</script>",
		"recipient.email_masked":         "j***@example.com",
		"security.link_expires_at_local": "2026-09-30",
		"links.action_url":               "https://app.zoiko.io/action",
		"message.reference":              "REF-1",
	}

	res, err := c.Render("ZS-IA-001", maliciousVars)
	if err != nil {
		t.Fatalf("unexpected render error: %v", err)
	}

	if strings.Contains(res.BodyHTML, "<script>") {
		t.Fatalf("XSS vulnerability: raw <script> tag found in rendered HTML:\n%s", res.BodyHTML)
	}
	if !strings.Contains(res.BodyHTML, "&lt;script&gt;alert(&#39;xss&#39;)&lt;/script&gt;") {
		t.Errorf("expected escaped HTML content, got:\n%s", res.BodyHTML)
	}
}

func TestCompiler_UnknownTemplateFailsClosed(t *testing.T) {
	c := ledger.NewCompiler()
	_, err := c.Render("UNKNOWN-TEMPLATE", map[string]string{"foo": "bar"})
	if !errors.Is(err, ledger.ErrTemplateNotFound) {
		t.Fatalf("expected ErrTemplateNotFound, got: %v", err)
	}
}
