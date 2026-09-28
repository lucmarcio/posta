// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
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
