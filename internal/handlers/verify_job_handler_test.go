// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"strings"
	"testing"
	"time"

	"github.com/goposta/posta/internal/models"
)

func TestNormalizeJobEmailsDedupesKeepsInvalid(t *testing.T) {
	got := normalizeJobEmails([]string{" A@x.com", "a@x.com", "bad", "", "b@y.com", "bad"})
	want := []string{"a@x.com", "bad", "b@y.com"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestJobViewProgress(t *testing.T) {
	j := &models.EmailVerifyJob{UUID: "u", Status: models.EmailVerifyJobRunning, Total: 3, Processed: 1, CreatedAt: time.Now()}
	v := toJobView(j, map[string]int64{"valid": 1, "pending": 2})
	if v.Progress != 33 || v.Counts["valid"] != 1 {
		t.Fatalf("view = %+v", v)
	}
	if _, has := v.Counts["pending"]; has {
		t.Fatal("pending must not appear in counts")
	}
	empty := toJobView(&models.EmailVerifyJob{Total: 0}, nil)
	if empty.Progress != 100 {
		t.Fatalf("empty job progress = %d", empty.Progress)
	}
}

func TestCSVSafeEscapesFormulas(t *testing.T) {
	cases := map[string]string{
		"=cmd|'/Ccalc'!A0@x.com": "'=cmd|'/Ccalc'!A0@x.com",
		"a@x.com":                "a@x.com",
		"-1":                     "'-1",
		"+1@x.com":               "'+1@x.com",
		"@SUM(A1)":               "'@SUM(A1)",
		"\tx@y.com":              "'\tx@y.com",
		"\rx@y.com":              "'\rx@y.com",
		"":                       "",
		"a=b@x.com":              "a=b@x.com",
	}
	for in, want := range cases {
		if got := csvSafe(in); got != want {
			t.Errorf("csvSafe(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeJobResultsPageParams(t *testing.T) {
	cases := []struct{ page, size, wantPage, wantSize, wantOffset int }{
		{0, 0, 0, 100, 0},
		{-1, -5, 0, 100, 0},
		{2, 50, 2, 50, 100},
		{1, 1000, 1, 1000, 1000},
		{1, 5000, 1, 1000, 1000}, // capped, not reset to the default
	}
	for _, c := range cases {
		p, s, o := normalizeJobResultsPageParams(c.page, c.size)
		if p != c.wantPage || s != c.wantSize || o != c.wantOffset {
			t.Errorf("(%d,%d) = (%d,%d,%d), want (%d,%d,%d)", c.page, c.size, p, s, o, c.wantPage, c.wantSize, c.wantOffset)
		}
	}
}

func TestIdempotencyKeyTooLong(t *testing.T) {
	if idempotencyKeyTooLong(strings.Repeat("k", 128)) {
		t.Fatal("128 characters must be accepted")
	}
	if !idempotencyKeyTooLong(strings.Repeat("k", 129)) {
		t.Fatal("129 characters must be rejected")
	}
	if idempotencyKeyTooLong(strings.Repeat("é", 128)) {
		t.Fatal("length is counted in characters, not bytes")
	}
}
