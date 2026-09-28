// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"strings"

	"github.com/goposta/posta/internal/services/bounceingest"
	"github.com/goposta/posta/internal/storage/repositories"
	"github.com/jkaninda/logger"
	"github.com/jkaninda/okapi"
)

// BounceRecorder is the subset of bounceingest.Ingestor the webhook handlers use.
type BounceRecorder interface {
	Record(scope repositories.ResourceScope, ev bounceingest.Event) bounceingest.Outcome
}

// BounceWebhookHandler processes inbound bounce notifications.
type BounceWebhookHandler struct {
	recorder BounceRecorder
}

func NewBounceWebhookHandler(recorder BounceRecorder) *BounceWebhookHandler {
	return &BounceWebhookHandler{recorder: recorder}
}

type BounceNotification struct {
	Body struct {
		Email     string `json:"email" required:"true" doc:"Bounced email address"`
		Type      string `json:"type" doc:"Bounce type: hard or soft" enum:"hard,soft"`
		EmailUUID string `json:"email_id" doc:"UUID of the original email"`
		Reason    string `json:"reason" doc:"Bounce reason"`
	} `json:"body"`
}

type BounceResponse struct {
	Processed bool   `json:"processed"`
	Action    string `json:"action"`
}

func (h *BounceWebhookHandler) HandleBounce(c *okapi.Context, req *BounceNotification) error {
	if strings.TrimSpace(req.Body.Email) == "" {
		return c.AbortBadRequest("email is required")
	}
	kind := bounceingest.KindHard
	if req.Body.Type == "soft" {
		kind = bounceingest.KindSoft
	}
	out := h.recorder.Record(getScope(c), bounceingest.Event{
		Email: req.Body.Email, Kind: kind, Reason: req.Body.Reason,
		EmailUUID: req.Body.EmailUUID, Source: "webhook",
	})

	// Keep the legacy action vocabulary: consumers already switch on it.
	action := "recorded"
	switch {
	case out.MessageMarked:
		action = "message_bounced"
	case out.SubscriberUpdated:
		action = "subscriber_bounced"
	}
	logger.Info("bounce processed", "email", out.Email, "type", kind, "action", action)
	return ok(c, BounceResponse{Processed: true, Action: action})
}
