// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package worker

import (
	"testing"

	"github.com/goposta/posta/internal/models"
)

func TestEligibleMessagesSkipsStatusOptOutAndGlobalSuppression(t *testing.T) {
	subs := []models.Subscriber{
		{ID: 1, Email: "ok@x.com", Status: models.SubscriberStatusSubscribed},
		{ID: 2, Email: "bounced@x.com", Status: models.SubscriberStatusBounced},
		{ID: 3, Email: "optout@x.com", Status: models.SubscriberStatusSubscribed},
		{ID: 4, Email: "Suppressed@X.com", Status: models.SubscriberStatusSubscribed},
	}
	msgs := eligibleMessages(42, subs,
		map[uint]struct{}{3: {}},
		map[string]struct{}{"suppressed@x.com": {}})
	if len(msgs) != 1 || msgs[0].SubscriberID != 1 || msgs[0].CampaignID != 42 || msgs[0].Status != models.CampaignMsgPending {
		t.Fatalf("msgs = %+v", msgs)
	}
}
