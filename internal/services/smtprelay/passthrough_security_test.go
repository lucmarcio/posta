// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package smtprelay

import (
	"testing"

	"github.com/goposta/posta/internal/services/inbound"
)

// The parser RFC 2047-decodes header values, and an encoded word can decode to
// CR/LF, so the tests build the ParsedEmail directly with the decoded form.

func TestExtractPassthrough_DropsControlCharsInCustomHeaders(t *testing.T) {
	parsed := &inbound.ParsedEmail{Headers: map[string]string{
		"Reply-To":  "ok@sobdemanda.net\r\nBcc: alvo@outro.com",
		"X-Bom":     "valor",
		"X-Newline": "a\nb",
		"X-Nul":     "a\x00b",
		"X-Tab":     "a\tb",
		"X Espaco":  "nome invalido",
		"X-Posta-A": "bloqueado",
		"":          "sem nome",
	}}

	pt := extractPassthrough(parsed, "mkt@sobdemanda.net")

	if len(pt.headers) != 1 || pt.headers["X-Bom"] != "valor" {
		t.Fatalf("headers = %v, want only X-Bom", pt.headers)
	}
}

func TestExtractPassthrough_UnsubscribeWithInjectionIsDropped(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"CRLF na URL", "<https://x.test/u\r\nBcc: alvo@outro.com>"},
		{"LF na URL", "<https://x.test/u\nBcc: alvo@outro.com>"},
		{"CRLF no mailto", "<mailto:u@x.test\r\nBcc: alvo@outro.com>"},
		{"espaco na URL", "<https://x.test/u v>"},
		{"URL sem host", "<https://>"},
		{"URL relativa", "</unsubscribe>"},
		{"mailto vazio", "<mailto:>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pt := extractPassthrough(&inbound.ParsedEmail{Headers: map[string]string{
				"List-Unsubscribe": tt.value,
			}}, "mkt@sobdemanda.net")
			if pt.unsubscribe != nil {
				t.Fatalf("unsubscribe = %+v, want nil", pt.unsubscribe)
			}
		})
	}
}

func TestExtractPassthrough_BadItemDoesNotHideGoodOne(t *testing.T) {
	pt := extractPassthrough(&inbound.ParsedEmail{Headers: map[string]string{
		"List-Unsubscribe":      "<https://x.test/u\r\nBcc: a@b.com>, <https://ok.test/u>, <mailto:u@ok.test>",
		"List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
	}}, "mkt@sobdemanda.net")

	u := pt.unsubscribe
	if u == nil || u.URL != "https://ok.test/u" || u.Mailto != "mailto:u@ok.test" || !u.OneClick {
		t.Fatalf("unsubscribe = %+v", u)
	}
}

func TestExtractPassthrough_FromWithControlCharsFallsBackToEnvelope(t *testing.T) {
	pt := extractPassthrough(&inbound.ParsedEmail{Headers: map[string]string{
		"From": "Nome\r\nBcc: alvo@outro.com <mkt@sobdemanda.net>",
	}}, "mkt@sobdemanda.net")

	if pt.from != "mkt@sobdemanda.net" {
		t.Fatalf("from = %q, want the envelope sender", pt.from)
	}
}
