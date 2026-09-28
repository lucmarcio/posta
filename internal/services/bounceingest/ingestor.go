// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package bounceingest records delivery failures reported after the fact
// (provider webhooks), so every source updates bounces, suppressions,
// subscribers and campaign messages the same way and inside the caller's scope.
package bounceingest

import (
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/goposta/posta/internal/metrics"
	"github.com/goposta/posta/internal/models"
	"github.com/goposta/posta/internal/storage/repositories"
)

type Kind string

const (
	KindHard        Kind = "hard"
	KindSoft        Kind = "soft"
	KindComplaint   Kind = "complaint"
	KindUnsubscribe Kind = "unsubscribe"
)

const (
	ActionIgnored    = "ignored"
	ActionRecorded   = "recorded"
	ActionSuppressed = "suppressed"
)

// Event is one provider-reported failure. EmailUUID is optional correlation
// to a Posta email; it is honoured only when that email belongs to the scope.
type Event struct {
	Email     string
	Kind      Kind
	Reason    string
	EmailUUID string
	Source    string
}

type Outcome struct {
	Email             string `json:"email"`
	Kind              Kind   `json:"kind"`
	Action            string `json:"action"`
	BounceRecorded    bool   `json:"bounce_recorded"`
	Suppressed        bool   `json:"suppressed"`
	SubscriberUpdated bool   `json:"subscriber_updated"`
	MessageMarked     bool   `json:"message_marked"`
}

type EmailFinder interface {
	FindByUUID(uuid string) (*models.Email, error)
}
type BounceCreator interface{ Create(*models.Bounce) error }
type SuppressionUpserter interface {
	Upsert(*models.Suppression) error
}
type SubscriberStore interface {
	FindByEmail(scope repositories.ResourceScope, email string) (*models.Subscriber, error)
	Update(*models.Subscriber) error
}
type MessageStore interface {
	FindByEmailID(emailID uint) (*models.CampaignMessage, error)
	UpdateBouncedAt(id uint) error
}

type Ingestor struct {
	emails       EmailFinder
	bounces      BounceCreator
	suppressions SuppressionUpserter
	subscribers  SubscriberStore
	messages     MessageStore
}

func New(emails EmailFinder, bounces BounceCreator, suppressions SuppressionUpserter, subscribers SubscriberStore, messages MessageStore) *Ingestor {
	return &Ingestor{emails: emails, bounces: bounces, suppressions: suppressions, subscribers: subscribers, messages: messages}
}

func (in *Ingestor) Record(scope repositories.ResourceScope, ev Event) Outcome {
	email := normalize(ev.Email)
	out := Outcome{Email: email, Kind: ev.Kind, Action: ActionIgnored}
	if email == "" {
		return out
	}

	em := in.ownedEmail(scope, ev.EmailUUID)

	if em != nil && ev.Kind != KindUnsubscribe {
		bt := bounceType(ev.Kind)
		if err := in.bounces.Create(&models.Bounce{
			UserID: em.UserID, WorkspaceID: em.WorkspaceID, EmailID: em.ID,
			Recipient: email, Type: bt, Reason: ev.Reason,
		}); err == nil {
			out.BounceRecorded = true
			metrics.IncrementBounce(string(bt))
		}
		if ev.Kind == KindHard || ev.Kind == KindSoft {
			if msg, err := in.messages.FindByEmailID(em.ID); err == nil && msg != nil {
				out.MessageMarked = in.messages.UpdateBouncedAt(msg.ID) == nil
			}
		}
	}

	if kind, ok := suppressionKind(ev.Kind); ok {
		if err := in.suppressions.Upsert(&models.Suppression{
			UserID: scope.UserID, WorkspaceID: scope.WorkspaceID, Email: email, Kind: kind,
			Reason: fmt.Sprintf("auto-suppressed (%s): %s", sourceOr(ev.Source), reasonOr(ev)),
		}); err == nil {
			out.Suppressed = true
			metrics.IncrementSuppression()
		}
	}

	if status, ok := subscriberStatus(ev.Kind); ok {
		if sub, err := in.subscribers.FindByEmail(scope, email); err == nil && sub != nil && sub.Status != status {
			now := time.Now()
			sub.Status = status
			sub.UpdatedAt = &now
			out.SubscriberUpdated = in.subscribers.Update(sub) == nil
		}
	}

	switch {
	case out.Suppressed:
		out.Action = ActionSuppressed
	case out.BounceRecorded || out.SubscriberUpdated || out.MessageMarked:
		out.Action = ActionRecorded
	}
	return out
}

// ownedEmail resolves the correlated email only when it belongs to scope.
func (in *Ingestor) ownedEmail(scope repositories.ResourceScope, uuid string) *models.Email {
	if uuid == "" || in.emails == nil {
		return nil
	}
	em, err := in.emails.FindByUUID(uuid)
	if err != nil || em == nil {
		return nil
	}
	if scope.WorkspaceID != nil {
		if em.WorkspaceID == nil || *em.WorkspaceID != *scope.WorkspaceID {
			return nil
		}
		return em
	}
	if em.WorkspaceID != nil || em.UserID != scope.UserID {
		return nil
	}
	return em
}

func bounceType(k Kind) models.BounceType {
	switch k {
	case KindSoft:
		return models.BounceTypeSoft
	case KindComplaint:
		return models.BounceTypeComplaint
	default:
		return models.BounceTypeHard
	}
}

func suppressionKind(k Kind) (models.SuppressionKind, bool) {
	switch k {
	case KindHard:
		return models.SuppressionKindBounce, true
	case KindComplaint:
		return models.SuppressionKindComplaint, true
	case KindUnsubscribe:
		return models.SuppressionKindHard, true
	}
	return "", false
}

func subscriberStatus(k Kind) (models.SubscriberStatus, bool) {
	switch k {
	case KindHard:
		return models.SubscriberStatusBounced, true
	case KindComplaint:
		return models.SubscriberStatusComplained, true
	case KindUnsubscribe:
		return models.SubscriberStatusUnsubscribed, true
	}
	return "", false
}

func normalize(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if addr, err := mail.ParseAddress(raw); err == nil {
		return strings.ToLower(addr.Address)
	}
	return strings.ToLower(raw)
}

func sourceOr(s string) string {
	if s == "" {
		return "webhook"
	}
	return s
}

func reasonOr(ev Event) string {
	if ev.Reason != "" {
		return ev.Reason
	}
	return string(ev.Kind)
}
