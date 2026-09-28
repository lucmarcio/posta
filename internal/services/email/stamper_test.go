// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package email

import (
	"testing"

	"github.com/goposta/posta/internal/models"
)

func TestStampersSetBrevoCorrelationHeader(t *testing.T) {
	s := NewStamper("Posta", "test", nil)
	em := &models.Email{UUID: "11111111-1111-1111-1111-111111111111"}

	h := map[string]string{}
	s.StampTransactional(h, em)
	if h["X-Mailin-custom"] != "posta-id=11111111-1111-1111-1111-111111111111" {
		t.Fatalf("transactional header = %q", h["X-Mailin-custom"])
	}

	h = map[string]string{}
	s.StampCampaign(h, em, 1, 2, false, false)
	if h["X-Mailin-custom"] != "posta-id=11111111-1111-1111-1111-111111111111" {
		t.Fatalf("campaign header = %q", h["X-Mailin-custom"])
	}

	h = map[string]string{"X-Mailin-custom": "caller-value"}
	s.StampTransactional(h, em)
	if h["X-Mailin-custom"] != "caller-value" {
		t.Fatalf("caller-supplied header must win, got %q", h["X-Mailin-custom"])
	}
}
