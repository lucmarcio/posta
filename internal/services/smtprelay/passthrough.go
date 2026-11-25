// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package smtprelay

import (
	"net/mail"
	"net/url"
	"strings"

	"github.com/goposta/posta/internal/services/email"
	"github.com/goposta/posta/internal/services/inbound"
)

// passthrough is what the relay carries over from the submitted message in
// addition to the envelope, subject, bodies and attachments. Without it a
// campaign tool such as Mautic loses its List-Unsubscribe, Reply-To and X-*
// headers, and the sender display name, when it relays through Posta.
type passthrough struct {
	from        string
	headers     map[string]string
	unsubscribe *email.Unsubscribe
}

// extractPassthrough derives the From (with display name), the custom headers
// and the unsubscribe configuration from a parsed message.
//
// The envelope sender stays authoritative: the display name of the From header
// is used only when its address is the envelope sender, so a message cannot
// borrow another identity through the header.
//
// Everything offered here is written verbatim into the outbound message, and
// the parsed header values have been RFC 2047-decoded (an encoded word can
// decode to CR/LF). So any value with a control character is dropped, never
// repaired. Custom headers are also subject to the custom headers setting and
// to the reserved/blocked list applied by the email service. Message-ID, Date,
// MIME and DKIM headers are not offered: the outbound pipeline sets its own.
func extractPassthrough(parsed *inbound.ParsedEmail, envelopeFrom string) passthrough {
	pt := passthrough{from: envelopeFrom}
	if parsed == nil {
		return pt
	}

	pt.from = fromWithDisplayName(parsed.Headers["From"], envelopeFrom)

	for key, value := range parsed.Headers {
		lower := strings.ToLower(key)
		if lower != "reply-to" && !strings.HasPrefix(lower, "x-") {
			continue
		}
		if strings.HasPrefix(lower, "x-posta-") {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" || !validHeaderName(key) || hasControlChars(value) {
			continue
		}
		if pt.headers == nil {
			pt.headers = make(map[string]string)
		}
		pt.headers[key] = value
	}

	pt.unsubscribe = unsubscribeFromHeaders(
		parsed.Headers["List-Unsubscribe"],
		parsed.Headers["List-Unsubscribe-Post"],
	)
	return pt
}

// fromWithDisplayName returns "Name <addr>" when the From header carries a
// display name and its address matches the envelope sender; otherwise the
// envelope sender unchanged.
func fromWithDisplayName(fromHeader, envelopeFrom string) string {
	if fromHeader == "" || envelopeFrom == "" || hasControlChars(fromHeader) {
		return envelopeFrom
	}
	addr, err := mail.ParseAddress(fromHeader)
	if err != nil || addr.Name == "" || !strings.EqualFold(addr.Address, envelopeFrom) {
		return envelopeFrom
	}
	from := addr.String()
	if hasControlChars(from) {
		return envelopeFrom
	}
	return from
}

// unsubscribeFromHeaders maps List-Unsubscribe / List-Unsubscribe-Post (RFC 2369
// and RFC 8058) onto the email service's Unsubscribe config. One-click is only
// declared for an https URL, which is what the service requires; anything else
// would be rejected there and turn into a temporary SMTP failure.
func unsubscribeFromHeaders(listUnsubscribe, listUnsubscribePost string) *email.Unsubscribe {
	link, mailto := parseListUnsubscribe(listUnsubscribe)
	if link == "" && mailto == "" {
		return nil
	}
	oneClick := strings.HasPrefix(strings.ToLower(link), "https://") &&
		strings.Contains(strings.ToLower(listUnsubscribePost), "list-unsubscribe=one-click")
	return &email.Unsubscribe{URL: link, Mailto: mailto, OneClick: oneClick}
}

// parseListUnsubscribe extracts the first valid http(s) URL and the first valid
// mailto: URI from a List-Unsubscribe value such as "<https://x/u>, <mailto:u@x>".
// Items that fail validation are skipped.
func parseListUnsubscribe(value string) (link, mailto string) {
	for {
		start := strings.IndexByte(value, '<')
		if start < 0 {
			return link, mailto
		}
		end := strings.IndexByte(value[start:], '>')
		if end < 0 {
			return link, mailto
		}
		item := strings.TrimSpace(value[start+1 : start+end])
		value = value[start+end+1:]

		switch {
		case link == "" && validHTTPURL(item):
			link = item
		case mailto == "" && validMailto(item):
			mailto = item
		}
	}
}

// validHTTPURL accepts an absolute http(s) URL with a host and no whitespace or
// control characters.
func validHTTPURL(item string) bool {
	if item == "" || hasControlChars(item) || strings.ContainsAny(item, " <>\"") {
		return false
	}
	u, err := url.Parse(item)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Scheme == "https" || u.Scheme == "http"
}

// validMailto accepts "mailto:" followed by something, with no whitespace or
// control characters.
func validMailto(item string) bool {
	const prefix = "mailto:"
	if len(item) <= len(prefix) || !strings.EqualFold(item[:len(prefix)], prefix) {
		return false
	}
	return !hasControlChars(item) && !strings.ContainsAny(item, " <>\"")
}

// validHeaderName accepts RFC 7230 token characters only.
func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", r):
		default:
			return false
		}
	}
	return true
}

// hasControlChars reports whether s has a C0 control character (CR, LF, NUL,
// tab, ...) or DEL.
func hasControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
