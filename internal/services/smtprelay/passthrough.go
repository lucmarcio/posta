// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package smtprelay

import (
	"net/mail"
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
// Custom headers are still subject to the workspace-wide custom headers setting
// and to the reserved/blocked list applied by the email service; this function
// only chooses what to offer. Message-ID, Date, MIME and DKIM headers are not
// offered: the outbound pipeline sets its own.
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
		if value == "" || strings.ContainsAny(value, "\r\n") {
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
	if fromHeader == "" || envelopeFrom == "" {
		return envelopeFrom
	}
	addr, err := mail.ParseAddress(fromHeader)
	if err != nil || addr.Name == "" || !strings.EqualFold(addr.Address, envelopeFrom) {
		return envelopeFrom
	}
	return addr.String()
}

// unsubscribeFromHeaders maps List-Unsubscribe / List-Unsubscribe-Post (RFC 2369
// and RFC 8058) onto the email service's Unsubscribe config. One-click is only
// declared for an https URL, which is what the service requires; anything else
// would be rejected there and turn into a temporary SMTP failure.
func unsubscribeFromHeaders(listUnsubscribe, listUnsubscribePost string) *email.Unsubscribe {
	url, mailto := parseListUnsubscribe(listUnsubscribe)
	if url == "" && mailto == "" {
		return nil
	}
	oneClick := strings.HasPrefix(strings.ToLower(url), "https://") &&
		strings.Contains(strings.ToLower(listUnsubscribePost), "list-unsubscribe=one-click")
	return &email.Unsubscribe{URL: url, Mailto: mailto, OneClick: oneClick}
}

// parseListUnsubscribe extracts the first http(s) URL and the first mailto: URI
// from a List-Unsubscribe value such as "<https://x/u>, <mailto:u@x>".
func parseListUnsubscribe(value string) (url, mailto string) {
	for {
		start := strings.IndexByte(value, '<')
		if start < 0 {
			return url, mailto
		}
		end := strings.IndexByte(value[start:], '>')
		if end < 0 {
			return url, mailto
		}
		item := strings.TrimSpace(value[start+1 : start+end])
		value = value[start+end+1:]

		lower := strings.ToLower(item)
		switch {
		case url == "" && (strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://")):
			url = item
		case mailto == "" && strings.HasPrefix(lower, "mailto:"):
			mailto = item
		}
	}
}
