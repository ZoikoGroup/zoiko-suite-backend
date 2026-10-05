package domain

import (
	"reflect"
	"testing"
)

func TestExtractProviderMessageID(t *testing.T) {
	cases := map[string]string{
		"smtp mail.example.com:587 accepted; message-id=<3f2a-b1@example.com>": "<3f2a-b1@example.com>",
		"smtp host accepted; MESSAGE-ID=<UPPER@example.com>":                   "<UPPER@example.com>",
		"smtp host accepted; message-id=<a@b>; secondary=true":                 "<a@b>",
		"smtp host accepted; message-id=bare-token-123":                        "bare-token-123",
		"in-app; readable from the recipient's notification register":          "",
		"":                                "",
		"smtp host accepted; message-id=": "",
		"smtp host accepted; message-id=" + string(make([]byte, 300)): "",
		"smtp host accepted; message-id=<x@y>\r\nX-Injected: yes":     "<x@y>",
	}
	for in, want := range cases {
		if got := ExtractProviderMessageID(in); got != want {
			t.Errorf("ExtractProviderMessageID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProviderMessageIDVariants(t *testing.T) {
	want := []string{"<a@b>", "a@b", "<a@b>"}
	if got := ProviderMessageIDVariants("<a@b>"); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
	if got := ProviderMessageIDVariants("a@b"); !reflect.DeepEqual(got, []string{"a@b", "a@b", "<a@b>"}) {
		t.Errorf("bare id: got %v", got)
	}
	if ProviderMessageIDVariants("  ") != nil || ProviderMessageIDVariants("<>") != nil {
		t.Error("an empty id has no variants")
	}
}
