package contenttype_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/document-vault-svc/internal/contenttype"
)

var (
	pdf  = []byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n1 0 obj\n<<>>\nendobj\n")
	png  = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00")
	zip  = []byte("PK\x03\x04\x14\x00\x00\x00\x08\x00")
	text = []byte("quarterly figures, nothing binary here\n")
	exe  = []byte("MZ\x90\x00\x03\x00\x00\x00\x04\x00\xff\xff\x00\x00\xb8\x00")
)

func TestValidate_AcceptsContentThatAgreesWithTheDeclaredType(t *testing.T) {
	for _, tc := range []struct {
		name, declared string
		content        []byte
		wantDetected   string
	}{
		{"pdf", "application/pdf", pdf, "application/pdf"},
		{"pdf with parameter", "application/pdf; version=1.7", pdf, "application/pdf"},
		{"png", "image/png", png, "image/png"},
		{"plain text", "text/plain", text, "text/plain"},
		{"csv is text", "text/csv", text, "text/plain"},
		{"json is text", "application/json", []byte(`{"a":1}`), "text/plain"},
		{"docx is a zip", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", zip, "application/zip"},
		{"odt is a zip", "application/vnd.oasis.opendocument.text", zip, "application/zip"},
		{"xml spelled application/xml", "application/xml", []byte("<?xml version=\"1.0\"?><a/>"), "text/xml"},
		{"case-insensitive declared", "APPLICATION/PDF", pdf, "application/pdf"},
		{"opaque binary, non-sniffable declared type", "application/x-custom", []byte{0x00, 0x01, 0x02, 0xfe}, "application/octet-stream"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := contenttype.Validate(tc.declared, tc.content)
			require.NoError(t, err)
			assert.Equal(t, tc.wantDetected, got)
		})
	}
}

func TestValidate_RejectsAMismatchBetweenDeclaredTypeAndBytes(t *testing.T) {
	for _, tc := range []struct {
		name, declared string
		content        []byte
	}{
		{"png declared as pdf", "application/pdf", png},
		{"pdf declared as png", "image/png", pdf},
		{"pdf bytes declared as text", "text/plain", pdf},
		{"zip declared as pdf", "application/pdf", zip},
		{"text declared as pdf", "application/pdf", text},
		{"text declared as png", "image/png", text},
		{"text declared as docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", text},
		{"opaque binary declared as pdf", "application/pdf", exe},
		{"opaque binary declared as zip", "application/zip", exe},
		{"png declared as docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", png},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := contenttype.Validate(tc.declared, tc.content)
			require.ErrorIs(t, err, contenttype.ErrMismatch)
		})
	}
}

func TestValidate_RejectsAnInvalidDeclaredType(t *testing.T) {
	for _, declared := range []string{"", "   ", "not a media type", "text/", "/plain", ";charset=utf-8"} {
		_, err := contenttype.Validate(declared, text)
		require.ErrorIsf(t, err, contenttype.ErrInvalidDeclared, "declared %q", declared)
	}
}

func TestValidate_ReportsWhatWasDetected_EvenOnMismatch(t *testing.T) {
	det, err := contenttype.Validate("application/pdf", png)
	require.Error(t, err)
	assert.Equal(t, "image/png", det)
}

// Only the first 512 bytes are inspected, so a large body is not copied or
// scanned in full for detection.
func TestValidate_LargeContentIsClassifiedFromItsHead(t *testing.T) {
	big := append(append([]byte{}, pdf...), make([]byte, 5<<20)...)
	det, err := contenttype.Validate("application/pdf", big)
	require.NoError(t, err)
	assert.Equal(t, "application/pdf", det)
}

// The documented limit of the policy: bytes the sniffer cannot classify are
// accepted under any non-sniffable declared type.
func TestValidate_DocumentedLimit_UnclassifiableBytesAreAcceptedUnderANonSniffableType(t *testing.T) {
	_, err := contenttype.Validate("application/x-msdownload", exe)
	require.NoError(t, err, "no block-list exists in the specification; see the package doc")
}
