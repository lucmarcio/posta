// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package smtprelay

import (
	"testing"

	"github.com/goposta/posta/internal/services/inbound"
)

func parse(t *testing.T, raw string) *inbound.ParsedEmail {
	t.Helper()
	p, err := inbound.ParseRawEmail([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return p
}

const campaignMessage = "From: Sob Demanda Softwares <mkt@sobdemanda.net>\r\n" +
	"To: cliente@example.com\r\n" +
	"Subject: Oferta\r\n" +
	"Message-ID: <abc@mautic>\r\n" +
	"Reply-To: Suporte <suporte@sobdemanda.net>\r\n" +
	"X-EMAIL-ID: 4521\r\n" +
	"X-Mautic-Campaign: fila\r\n" +
	"X-Posta-Internal: nope\r\n" +
	"List-Unsubscribe: <https://mkt.sobdemanda.net/email/unsubscribe/xyz>, <mailto:unsub@sobdemanda.net>\r\n" +
	"List-Unsubscribe-Post: List-Unsubscribe=One-Click\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"Ola\r\n"

func TestExtractPassthrough_Campaign(t *testing.T) {
	pt := extractPassthrough(parse(t, campaignMessage), "mkt@sobdemanda.net")

	if pt.from != `"Sob Demanda Softwares" <mkt@sobdemanda.net>` {
		t.Fatalf("from = %q", pt.from)
	}

	wantHeaders := map[string]string{
		"Reply-To":          "Suporte <suporte@sobdemanda.net>",
		"X-Email-Id":        "4521",
		"X-Mautic-Campaign": "fila",
	}
	if len(pt.headers) != len(wantHeaders) {
		t.Fatalf("headers = %v, want %v", pt.headers, wantHeaders)
	}
	for k, v := range wantHeaders {
		if pt.headers[k] != v {
			t.Fatalf("header %s = %q, want %q (all: %v)", k, pt.headers[k], v, pt.headers)
		}
	}
	for _, banned := range []string{"Message-Id", "Subject", "From", "To", "Content-Type", "List-Unsubscribe", "X-Posta-Internal"} {
		if _, ok := pt.headers[banned]; ok {
			t.Fatalf("header %s must not be passed through", banned)
		}
	}

	u := pt.unsubscribe
	if u == nil {
		t.Fatal("unsubscribe is nil")
	}
	if u.URL != "https://mkt.sobdemanda.net/email/unsubscribe/xyz" || u.Mailto != "mailto:unsub@sobdemanda.net" || !u.OneClick {
		t.Fatalf("unsubscribe = %+v", u)
	}
}

func TestExtractPassthrough_NoExtras(t *testing.T) {
	raw := "From: mkt@sobdemanda.net\r\nTo: a@b.com\r\nSubject: x\r\n\r\ncorpo\r\n"
	pt := extractPassthrough(parse(t, raw), "mkt@sobdemanda.net")

	if pt.from != "mkt@sobdemanda.net" {
		t.Fatalf("from = %q", pt.from)
	}
	if pt.headers != nil {
		t.Fatalf("headers = %v, want nil", pt.headers)
	}
	if pt.unsubscribe != nil {
		t.Fatalf("unsubscribe = %+v, want nil", pt.unsubscribe)
	}
}

func TestExtractPassthrough_FromHeaderForOtherAddressIsIgnored(t *testing.T) {
	raw := "From: Presidente <ceo@outro.com>\r\nTo: a@b.com\r\nSubject: x\r\n\r\ncorpo\r\n"
	pt := extractPassthrough(parse(t, raw), "mkt@sobdemanda.net")

	if pt.from != "mkt@sobdemanda.net" {
		t.Fatalf("from = %q, the envelope sender must win", pt.from)
	}
}

func TestExtractPassthrough_NilMessage(t *testing.T) {
	pt := extractPassthrough(nil, "mkt@sobdemanda.net")
	if pt.from != "mkt@sobdemanda.net" || pt.headers != nil || pt.unsubscribe != nil {
		t.Fatalf("unexpected passthrough: %+v", pt)
	}
}

func TestUnsubscribeFromHeaders(t *testing.T) {
	tests := []struct {
		name       string
		value      string
		post       string
		wantNil    bool
		wantURL    string
		wantMailto string
		wantOne    bool
	}{
		{name: "https com one-click", value: "<https://x.test/u>", post: "List-Unsubscribe=One-Click", wantURL: "https://x.test/u", wantOne: true},
		{name: "https sem post header", value: "<https://x.test/u>", wantURL: "https://x.test/u"},
		{name: "http nunca e one-click", value: "<http://x.test/u>", post: "List-Unsubscribe=One-Click", wantURL: "http://x.test/u"},
		{name: "so mailto", value: "<mailto:u@x.test>", wantMailto: "mailto:u@x.test"},
		{name: "mailto primeiro, url depois", value: "<mailto:u@x.test>, <https://x.test/u>", post: "List-Unsubscribe=One-Click", wantURL: "https://x.test/u", wantMailto: "mailto:u@x.test", wantOne: true},
		{name: "vazio", value: "", wantNil: true},
		{name: "sem colchetes", value: "https://x.test/u", wantNil: true},
		{name: "colchete aberto", value: "<https://x.test/u", wantNil: true},
		{name: "esquema desconhecido", value: "<ftp://x.test/u>", wantNil: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := unsubscribeFromHeaders(tt.value, tt.post)
			if tt.wantNil {
				if got != nil {
					t.Fatalf("got %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("got nil")
			}
			if got.URL != tt.wantURL || got.Mailto != tt.wantMailto || got.OneClick != tt.wantOne {
				t.Fatalf("got %+v, want url=%q mailto=%q oneclick=%v", got, tt.wantURL, tt.wantMailto, tt.wantOne)
			}
		})
	}
}
