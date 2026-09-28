// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"github.com/google/uuid"
	"github.com/goposta/posta/internal/services/bounceingest"
	"github.com/jkaninda/logger"
	"github.com/jkaninda/okapi"
)

const brevoWebhookMaxBody = 5 << 20

// BrevoWebhookHandler ingests Brevo transactional/marketing webhook events.
// Brevo accepts every recipient at RCPT time when used as a relay, so these
// events are the only way Posta learns about its bounces.
type BrevoWebhookHandler struct {
	recorder BounceRecorder
}

func NewBrevoWebhookHandler(recorder BounceRecorder) *BrevoWebhookHandler {
	return &BrevoWebhookHandler{recorder: recorder}
}

type brevoEvent struct {
	Event        string `json:"event"`
	Email        string `json:"email"`
	Reason       string `json:"reason"`
	MailinCustom string `json:"X-Mailin-custom"`
	MessageID    string `json:"message-id"`
}

// BrevoWebhookResponse summarises what was done with each event.
type BrevoWebhookResponse struct {
	Received  int                    `json:"received"`
	Processed int                    `json:"processed"`
	Ignored   int                    `json:"ignored"`
	Items     []bounceingest.Outcome `json:"items"`
}

// Handle accepts a single event object or, in Brevo's batched mode, an array.
// Any well-formed payload is answered with 200 so Brevo does not redeliver.
func (h *BrevoWebhookHandler) Handle(c *okapi.Context) error {
	raw, err := io.ReadAll(io.LimitReader(c.Request().Body, brevoWebhookMaxBody))
	if err != nil {
		return c.AbortBadRequest("failed to read webhook body")
	}
	events, err := parseBrevoPayload(raw)
	if err != nil {
		return c.AbortBadRequest("invalid Brevo webhook payload")
	}

	scope := getScope(c)
	resp := BrevoWebhookResponse{Received: len(events), Items: make([]bounceingest.Outcome, 0, len(events))}
	for _, ev := range events {
		kind, known := brevoKind(ev.Event)
		if !known || strings.TrimSpace(ev.Email) == "" {
			resp.Ignored++
			resp.Items = append(resp.Items, bounceingest.Outcome{
				Email: strings.ToLower(strings.TrimSpace(ev.Email)), Action: bounceingest.ActionIgnored,
			})
			continue
		}
		out := h.recorder.Record(scope, bounceingest.Event{
			Email: ev.Email, Kind: kind, Reason: ev.Reason,
			EmailUUID: postaIDFromCustom(ev.MailinCustom), Source: "brevo",
		})
		resp.Processed++
		resp.Items = append(resp.Items, out)
	}
	logger.Info("brevo webhook processed", "received", resp.Received, "processed", resp.Processed, "ignored", resp.Ignored)
	return ok(c, resp)
}

func parseBrevoPayload(raw []byte) ([]brevoEvent, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var list []brevoEvent
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return nil, err
		}
		return list, nil
	}
	var one brevoEvent
	if err := json.Unmarshal(trimmed, &one); err != nil {
		return nil, err
	}
	return []brevoEvent{one}, nil
}

func brevoKind(event string) (bounceingest.Kind, bool) {
	switch strings.ToLower(strings.TrimSpace(event)) {
	case "hard_bounce", "hardbounce", "invalid_email", "invalid", "blocked":
		return bounceingest.KindHard, true
	case "soft_bounce", "softbounce":
		return bounceingest.KindSoft, true
	case "spam", "complaint":
		return bounceingest.KindComplaint, true
	case "unsubscribed", "unsubscribe":
		return bounceingest.KindUnsubscribe, true
	}
	return "", false
}

// postaIDFromCustom extracts the email UUID Posta stamps as
// "X-Mailin-custom: posta-id=<uuid>"; other key=value pairs are tolerated.
func postaIDFromCustom(v string) string {
	for _, part := range strings.Split(v, ";") {
		k, val, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found || strings.TrimSpace(k) != "posta-id" {
			continue
		}
		val = strings.TrimSpace(val)
		if _, err := uuid.Parse(val); err == nil {
			return val
		}
	}
	return ""
}
