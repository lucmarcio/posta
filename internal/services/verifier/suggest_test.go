// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package verifier

import "testing"

func TestDisposableListLoadedAndParentMatch(t *testing.T) {
	if len(disposableDomains) < 1000 {
		t.Fatalf("embedded list too small: %d", len(disposableDomains))
	}
	for _, d := range []string{"mailinator.com", "sub.mailinator.com", "YOPMAIL.COM"} {
		if !isDisposable(d) {
			t.Errorf("%s should be disposable", d)
		}
	}
	for _, d := range []string{"gmail.com", "uol.com.br", "com", ""} {
		if isDisposable(d) {
			t.Errorf("%s should not be disposable", d)
		}
	}
}

func TestSuggestDomain(t *testing.T) {
	cases := map[string]string{
		"gmial.com":   "gmail.com",
		"gmail.co":    "gmail.com",
		"hotmial.com": "hotmail.com",
		"yaho.com.br": "yahoo.com.br",
		"outlok.com":  "outlook.com",
		"gmail.com":   "",
		"uol.com.br":  "",
		"example.org": "",
		"acme.com.br": "",
	}
	for in, want := range cases {
		if got := suggestDomain(in); got != want {
			t.Errorf("suggestDomain(%q) = %q, want %q", in, got, want)
		}
	}
}
