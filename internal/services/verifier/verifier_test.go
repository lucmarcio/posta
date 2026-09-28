// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package verifier

import (
	"encoding/json"
	"testing"
)

func TestDecide(t *testing.T) {
	ok := verdictInput{SyntaxOK: true, DNS: dnsOK, SMTP: SMTPSkipped}
	with := func(f func(*verdictInput)) verdictInput { v := ok; f(&v); return v }
	cases := []struct {
		name       string
		in         verdictInput
		wantStatus Status
		wantScore  int
	}{
		{"bad syntax", verdictInput{}, StatusInvalid, 0},
		{"disposable beats everything", with(func(v *verdictInput) { v.Disposable = true; v.Role = true }), StatusDisposable, 10},
		{"no mail", with(func(v *verdictInput) { v.DNS = dnsNoMail }), StatusInvalid, 0},
		{"null mx", with(func(v *verdictInput) { v.DNS = dnsNoMail; v.NullMX = true }), StatusInvalid, 0},
		{"dns temp error", with(func(v *verdictInput) { v.DNS = dnsTempErr }), StatusUnknown, 40},
		{"smtp undeliverable", with(func(v *verdictInput) { v.SMTP = SMTPUndeliverable }), StatusInvalid, 0},
		{"role beats deliverable", with(func(v *verdictInput) { v.Role = true; v.SMTP = SMTPDeliverable }), StatusRisky, 60},
		{"accept all", with(func(v *verdictInput) { v.SMTP = SMTPAcceptAll }), StatusAcceptAll, 50},
		{"smtp deliverable", with(func(v *verdictInput) { v.SMTP = SMTPDeliverable }), StatusValid, 95},
		{"smtp unknown keeps dns verdict", with(func(v *verdictInput) { v.SMTP = SMTPUnknown }), StatusValid, 90},
		{"role without smtp", with(func(v *verdictInput) { v.Role = true }), StatusRisky, 60},
		{"clean", ok, StatusValid, 90},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, score, _ := decide(c.in)
			if s != c.wantStatus || score != c.wantScore {
				t.Fatalf("decide() = (%s, %d), want (%s, %d)", s, score, c.wantStatus, c.wantScore)
			}
		})
	}
}

func TestIsDisposable(t *testing.T) {
	if !isDisposable("Mailinator.com") {
		t.Error("expected mailinator.com to be disposable (case-insensitive)")
	}
	if isDisposable("gmail.com") {
		t.Error("gmail.com should not be disposable")
	}
}

func TestIsRoleAccount(t *testing.T) {
	for _, local := range []string{"info", "ADMIN", "no-reply", "support"} {
		if !isRoleAccount(local) {
			t.Errorf("expected %q to be a role account", local)
		}
	}
	if isRoleAccount("jonas") {
		t.Error("jonas should not be a role account")
	}
}

func TestCacheKeys(t *testing.T) {
	if got := addrKey("a@b.com"); got != "verify:addr:a@b.com" {
		t.Errorf("addrKey = %q", got)
	}
	if got := mxKey("B.com"); got != "verify:mx:b.com" {
		t.Errorf("mxKey = %q, want lowercased", got)
	}
}

func TestResultJSONRoundTrip(t *testing.T) {
	r := &Result{
		Email:  "user@example.com",
		Status: StatusValid,
		Score:  90,
		Checks: Checks{Syntax: true, MX: true, SMTP: "skipped"},
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Result
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Status != StatusValid || back.Score != 90 || !back.Checks.MX {
		t.Fatalf("round-trip mismatch: %+v", back)
	}
}
