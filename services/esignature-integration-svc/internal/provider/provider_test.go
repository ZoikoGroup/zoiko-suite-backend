package provider

import (
	"context"
	"errors"
	"testing"
)

type fake struct{ name string }

func (f fake) Name() string { return f.name }
func (f fake) Send(context.Context, SendRequest) (SendResult, error) {
	return SendResult{ExternalRef: "x"}, nil
}

func TestBuild_DefaultIsFailClosed(t *testing.T) {
	r, err := Build("")
	if err != nil {
		t.Fatal(err)
	}
	if r.Configured() {
		t.Fatal("empty ESIGN_PROVIDERS must configure no provider")
	}
	if _, err := r.Resolve("DOCUSIGN"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
	if _, err := r.Resolve(""); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured for default provider, got %v", err)
	}
}

func TestBuild_UnimplementedProviderIsAConfigError(t *testing.T) {
	for _, n := range []string{"DOCUSIGN", "ADOBE_SIGN"} {
		if _, err := Build(n); err == nil {
			t.Fatalf("%s has no adapter in this build; Build must refuse it rather than pretend", n)
		}
	}
}

func TestRegistry_Resolve(t *testing.T) {
	r := NewRegistry(fake{"docusign"})
	if a, err := r.Resolve(""); err != nil || a.Name() != "docusign" {
		t.Fatalf("default resolve: %v %v", a, err)
	}
	if _, err := r.Resolve("DocuSign"); err != nil {
		t.Fatalf("case-insensitive resolve: %v", err)
	}
	if _, err := r.Resolve("ADOBE_SIGN"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("unconfigured name: want ErrNotConfigured, got %v", err)
	}
}
