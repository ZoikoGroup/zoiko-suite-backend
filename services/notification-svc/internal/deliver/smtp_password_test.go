package deliver

import "testing"

func TestSMTPProvider_SetPasswordRotatesAndRefusesEmpty(t *testing.T) {
	p := &SMTPProvider{password: "old"}
	if err := p.SetPassword("new"); err != nil || p.currentPassword() != "new" {
		t.Fatalf("rotation: %v %q", err, p.currentPassword())
	}
	if err := p.SetPassword(""); err == nil || p.currentPassword() != "new" {
		t.Fatalf("an empty password must be refused and the current one kept: %v %q", err, p.currentPassword())
	}
}
