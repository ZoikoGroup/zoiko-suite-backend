package domain

import "strings"

// ExtractProviderMessageID returns the identifier a provider callback will use to
// refer to a submission, taken from the receipt the transport returned.
//
// WHY THIS EXISTS. A bounce, complaint or delivery callback names a message by
// its provider message id (for SMTP, the Message-ID header). The receipt an
// accepting transport returns is a sentence that CONTAINS that id:
//
//	smtp mail.example.com:587 accepted; message-id=<3f2a...@example.com>
//
// Storing the whole sentence as the id (as the ledger path did) means a callback
// carrying "<3f2a...@example.com>" never equals the stored value, so it cannot be
// matched to its attempt and its bounce or complaint never becomes a suppression
// (ZS-SVC-Y-001 NP-24, NP-27). The id is therefore extracted once, here, and
// stored in its own column.
//
// A receipt with no "message-id=" token yields "": the caller must then record no
// id rather than a sentence. (IN_APP receipts, for one, carry none.)
func ExtractProviderMessageID(receipt string) string {
	const marker = "message-id="
	i := strings.Index(strings.ToLower(receipt), marker)
	if i < 0 {
		return ""
	}
	rest := strings.TrimLeft(receipt[i+len(marker):], " ")
	end := len(rest)
	for j, r := range rest {
		if r == ' ' || r == ';' || r == ',' || r == '\r' || r == '\n' || r == '\t' {
			end = j
			break
		}
	}
	id := strings.TrimSpace(rest[:end])
	if len(id) > 255 {
		return ""
	}
	return id
}

// ProviderMessageIDVariants returns the spellings under which a callback may carry
// an id: with and without the angle brackets of an RFC 5322 Message-ID. Providers
// differ on whether they keep them.
func ProviderMessageIDVariants(id string) []string {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}
	bare := strings.TrimSuffix(strings.TrimPrefix(id, "<"), ">")
	if bare == "" {
		return nil
	}
	return []string{id, bare, "<" + bare + ">"}
}
