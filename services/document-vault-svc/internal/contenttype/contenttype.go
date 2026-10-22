// Package contenttype checks a caller's declared MIME type against the bytes
// actually uploaded (ZS-SVC-S-001 DRC-I19: "MIME/content-type disagreement is
// validated by policy; declared MIME alone is not trusted").
//
// Detection is the standard library's net/http.DetectContentType (the WHATWG
// MIME-sniffing algorithm over the first 512 bytes). It recognises a fixed set
// of formats — PDF, PNG, JPEG, GIF, WebP, ZIP, gzip, HTML, XML, common audio
// and video, and so on — and otherwise answers text/plain or
// application/octet-stream. So the policy implemented here is deliberately
// narrow and is the part of DRC-I19 that can be decided from the bytes:
//
//  1. The declared value must be a well-formed media type.
//  2. If the bytes are positively identified as a format, the declared type must
//     be that format (or, for ZIP, an OOXML/ODF/EPUB container, which are ZIP).
//  3. If the bytes are generic (text or opaque binary), the declared type must
//     not CLAIM one of the formats the sniffer can positively identify — a file
//     declared application/pdf whose bytes are not a PDF is refused.
//  4. Anything else is accepted: the sniffer cannot tell a CSV from a JSON file
//     or one opaque binary format from another, and ZS-SVC-S-001 names no
//     allow-list or block-list of types. Whether one is wanted is an open
//     policy question, not something this package invents.
package contenttype

import (
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"
)

var (
	// ErrInvalidDeclared: the declared content type is empty or not a media type.
	ErrInvalidDeclared = errors.New("declared content_type is not a valid media type")
	// ErrMismatch: the bytes do not agree with the declared content type.
	ErrMismatch = errors.New("declared content_type does not match the uploaded content")
)

const sniffLen = 512

// sniffable are the media types http.DetectContentType can identify from
// content alone. Declaring one of these for generic bytes is a false claim.
var sniffable = map[string]bool{
	"application/pdf":               true,
	"application/postscript":        true,
	"image/png":                     true,
	"image/jpeg":                    true,
	"image/gif":                     true,
	"image/webp":                    true,
	"image/bmp":                     true,
	"application/zip":               true,
	"application/gzip":              true,
	"application/x-gzip":            true,
	"application/x-rar-compressed":  true,
	"application/wasm":              true,
	"application/ogg":               true,
	"audio/mpeg":                    true,
	"audio/wave":                    true,
	"audio/wav":                     true,
	"audio/aiff":                    true,
	"audio/basic":                   true,
	"video/mp4":                     true,
	"video/webm":                    true,
	"video/avi":                     true,
	"font/woff":                     true,
	"font/woff2":                    true,
	"font/ttf":                      true,
	"font/otf":                      true,
	"application/vnd.ms-fontobject": true,
}

// zipContainer reports whether a declared type is a ZIP-based document format.
func zipContainer(declared string) bool {
	return strings.HasPrefix(declared, "application/vnd.openxmlformats-officedocument.") ||
		strings.HasPrefix(declared, "application/vnd.oasis.opendocument.") ||
		declared == "application/epub+zip"
}

func isGeneric(detected string) bool {
	return detected == "text/plain" || detected == "application/octet-stream"
}

// xmlFamily treats the XML spellings as one family (the sniffer says text/xml).
func xmlFamily(t string) bool {
	return t == "text/xml" || t == "application/xml" || strings.HasSuffix(t, "+xml")
}

// Validate returns the media type detected from content (parameters stripped),
// or an error when the declared type is malformed or contradicts the bytes.
// The returned type is what downstream gates (the malware scanner) should be
// given instead of the caller's header.
func Validate(declared string, content []byte) (detected string, err error) {
	d, _, perr := mime.ParseMediaType(strings.TrimSpace(declared))
	if perr != nil || d == "" {
		return "", ErrInvalidDeclared
	}
	d = strings.ToLower(d)

	head := content
	if len(head) > sniffLen {
		head = head[:sniffLen]
	}
	det, _, derr := mime.ParseMediaType(http.DetectContentType(head))
	if derr != nil {
		det = "application/octet-stream"
	}

	switch {
	case !isGeneric(det):
		switch {
		case d == det:
		case det == "application/zip" && zipContainer(d):
		case det == "text/xml" && xmlFamily(d):
		case det == "application/gzip" && d == "application/x-gzip":
		case det == "audio/wave" && d == "audio/wav":
		default:
			return det, fmt.Errorf("%w: declared %q, content is %q", ErrMismatch, d, det)
		}
	default:
		if sniffable[d] || zipContainer(d) {
			return det, fmt.Errorf("%w: declared %q, content is %q", ErrMismatch, d, det)
		}
	}
	return det, nil
}
