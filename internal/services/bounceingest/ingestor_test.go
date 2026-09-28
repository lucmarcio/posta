// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package bounceingest

import (
	"errors"
	"testing"

	"github.com/goposta/posta/internal/models"
	"github.com/goposta/posta/internal/storage/repositories"
)

type fakeEmails struct{ byUUID map[string]*models.Email }

func (f *fakeEmails) FindByUUID(uuid string) (*models.Email, error) {
	if e, ok := f.byUUID[uuid]; ok {
		return e, nil
	}
	return nil, errors.New("not found")
}

type fakeBounces struct{ created []models.Bounce }

func (f *fakeBounces) Create(b *models.Bounce) error { f.created = append(f.created, *b); return nil }

type fakeSuppressions struct{ upserted []models.Suppression }

func (f *fakeSuppressions) Upsert(s *models.Suppression) error {
	f.upserted = append(f.upserted, *s)
	return nil
}

type fakeSubscribers struct {
	byEmail map[string]*models.Subscriber
	updated []models.Subscriber
}

func (f *fakeSubscribers) FindByEmail(_ repositories.ResourceScope, email string) (*models.Subscriber, error) {
	if s, ok := f.byEmail[email]; ok {
		return s, nil
	}
	return nil, errors.New("not found")
}
func (f *fakeSubscribers) Update(s *models.Subscriber) error {
	f.updated = append(f.updated, *s)
	return nil
}

type fakeMessages struct{ marked []uint }

func (f *fakeMessages) FindByEmailID(id uint) (*models.CampaignMessage, error) {
	return &models.CampaignMessage{ID: id * 10}, nil
}
func (f *fakeMessages) UpdateBouncedAt(id uint) error { f.marked = append(f.marked, id); return nil }

func u(v uint) *uint { return &v }

type rig struct {
	in   *Ingestor
	b    *fakeBounces
	s    *fakeSuppressions
	subs *fakeSubscribers
	m    *fakeMessages
}

func newRig() rig {
	emails := &fakeEmails{byUUID: map[string]*models.Email{
		"11111111-1111-1111-1111-111111111111": {ID: 7, UserID: 1, WorkspaceID: u(10)},
		"22222222-2222-2222-2222-222222222222": {ID: 8, UserID: 2, WorkspaceID: u(20)}, // other tenant
	}}
	r := rig{b: &fakeBounces{}, s: &fakeSuppressions{}, m: &fakeMessages{},
		subs: &fakeSubscribers{byEmail: map[string]*models.Subscriber{
			"a@b.com": {ID: 3, Email: "a@b.com", Status: models.SubscriberStatusSubscribed},
		}}}
	r.in = New(emails, r.b, r.s, r.subs, r.m)
	return r
}

var scope = repositories.ResourceScope{UserID: 1, WorkspaceID: u(10)}

func TestHardBounceWithMatchingEmail(t *testing.T) {
	r := newRig()
	out := r.in.Record(scope, Event{Email: "Foo <A@B.com>", Kind: KindHard, Reason: "550 user unknown",
		EmailUUID: "11111111-1111-1111-1111-111111111111", Source: "brevo"})

	if out.Email != "a@b.com" || !out.BounceRecorded || !out.Suppressed || !out.SubscriberUpdated || !out.MessageMarked {
		t.Fatalf("unexpected outcome: %+v", out)
	}
	if len(r.b.created) != 1 || r.b.created[0].EmailID != 7 || r.b.created[0].Type != models.BounceTypeHard {
		t.Fatalf("bounce row wrong: %+v", r.b.created)
	}
	if got := r.s.upserted[0]; got.Kind != models.SuppressionKindBounce || *got.WorkspaceID != 10 || got.Email != "a@b.com" {
		t.Fatalf("suppression wrong: %+v", got)
	}
	if r.subs.updated[0].Status != models.SubscriberStatusBounced {
		t.Fatalf("subscriber status = %s", r.subs.updated[0].Status)
	}
	if len(r.m.marked) != 1 || r.m.marked[0] != 70 {
		t.Fatalf("message not marked: %v", r.m.marked)
	}
}

func TestForeignTenantEmailUUIDIsIgnored(t *testing.T) {
	r := newRig()
	out := r.in.Record(scope, Event{Email: "a@b.com", Kind: KindHard,
		EmailUUID: "22222222-2222-2222-2222-222222222222", Source: "brevo"})
	if out.BounceRecorded || out.MessageMarked || len(r.b.created) != 0 || len(r.m.marked) != 0 {
		t.Fatalf("foreign email must not be touched: %+v", out)
	}
	if !out.Suppressed || *r.s.upserted[0].WorkspaceID != 10 {
		t.Fatalf("suppression must still land in caller scope: %+v", r.s.upserted)
	}
}

func TestSoftBounceDoesNotSuppress(t *testing.T) {
	r := newRig()
	out := r.in.Record(scope, Event{Email: "a@b.com", Kind: KindSoft,
		EmailUUID: "11111111-1111-1111-1111-111111111111"})
	if out.Suppressed || out.SubscriberUpdated || len(r.s.upserted) != 0 {
		t.Fatalf("soft bounce must not suppress: %+v", out)
	}
	if !out.BounceRecorded || r.b.created[0].Type != models.BounceTypeSoft {
		t.Fatalf("soft bounce must be recorded: %+v", r.b.created)
	}
}

func TestComplaintAndUnsubscribe(t *testing.T) {
	r := newRig()
	r.in.Record(scope, Event{Email: "a@b.com", Kind: KindComplaint})
	if r.s.upserted[0].Kind != models.SuppressionKindComplaint || r.subs.updated[0].Status != models.SubscriberStatusComplained {
		t.Fatalf("complaint mapping wrong: %+v %+v", r.s.upserted, r.subs.updated)
	}
	r2 := newRig()
	out := r2.in.Record(scope, Event{Email: "a@b.com", Kind: KindUnsubscribe,
		EmailUUID: "11111111-1111-1111-1111-111111111111"})
	if out.BounceRecorded || r2.s.upserted[0].Kind != models.SuppressionKindHard ||
		r2.subs.updated[0].Status != models.SubscriberStatusUnsubscribed {
		t.Fatalf("unsubscribe mapping wrong: %+v", out)
	}
}

func TestEmptyEmailIsIgnored(t *testing.T) {
	r := newRig()
	if out := r.in.Record(scope, Event{Email: "  ", Kind: KindHard}); out.Action != ActionIgnored {
		t.Fatalf("action = %q", out.Action)
	}
}
