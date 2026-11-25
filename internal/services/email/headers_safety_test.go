// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package email

import (
	"errors"
	"testing"

	"github.com/goposta/posta/internal/storage/repositories"
)

func TestFilterCustomHeaders_RejectsInjection(t *testing.T) {
	got := filterCustomHeaders(map[string]string{
		"Reply-To":        "ok@x.test",
		"X-Valor-CRLF":    "a\r\nBcc: alvo@outro.com",
		"X-Valor-LF":      "a\nb",
		"X Nome Invalido": "v",
		"X-Nome:Dois":     "v",
		"X-Posta-Id":      "bloqueado",
		"Subject":         "reservado",
		"DKIM-Signature":  "bloqueado",
	})

	if len(got) != 1 || got["Reply-To"] != "ok@x.test" {
		t.Fatalf("filterCustomHeaders = %v, want only Reply-To", got)
	}
}

func TestStoredHeaders(t *testing.T) {
	if got := storedHeaders(""); got != nil {
		t.Fatalf("empty = %v, want nil", got)
	}
	if got := storedHeaders("{nao-e-json"); got != nil {
		t.Fatalf("invalid json = %v, want nil", got)
	}
	got := storedHeaders(`{"Reply-To":"a@x.test","X-Id":"7"}`)
	if len(got) != 2 || got["Reply-To"] != "a@x.test" || got["X-Id"] != "7" {
		t.Fatalf("stored = %v", got)
	}
}

func TestResolveUnsubscribe_RejectsControlChars(t *testing.T) {
	s := &Service{}
	tests := []struct {
		name string
		u    *Unsubscribe
	}{
		{"CRLF na URL", &Unsubscribe{URL: "https://x.test/u\r\nBcc: a@b.com"}},
		{"LF no mailto", &Unsubscribe{Mailto: "u@x.test\nBcc: a@b.com"}},
		{"NUL na URL", &Unsubscribe{URL: "https://x.test/\x00"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.resolveUnsubscribe(repositories.ResourceScope{}, &SendRequest{Unsubscribe: tt.u})
			if !errors.Is(err, ErrUnsubscribeInvalid) {
				t.Fatalf("err = %v, want ErrUnsubscribeInvalid", err)
			}
		})
	}

	ok, err := s.resolveUnsubscribe(repositories.ResourceScope{}, &SendRequest{
		Unsubscribe: &Unsubscribe{URL: "https://x.test/u", Mailto: "u@x.test", OneClick: true},
	})
	if err != nil || ok == nil || ok.url != "https://x.test/u" || ok.mailto != "mailto:u@x.test" || !ok.oneClick {
		t.Fatalf("valid config rejected: %+v, %v", ok, err)
	}
}
