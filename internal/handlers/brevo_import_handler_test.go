// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/goposta/posta/internal/models"
	"github.com/goposta/posta/internal/services/brevo"
	"github.com/goposta/posta/internal/storage/repositories"
)

type fakeBlocked struct{ total int }

func (f *fakeBlocked) BlockedContacts(_ context.Context, _ string, q brevo.BlockedQuery) (*brevo.BlockedPage, error) {
	page := &brevo.BlockedPage{Count: int64(f.total)}
	for i := q.Offset; i < f.total && i < q.Offset+q.Limit; i++ {
		code := "hardBounce"
		switch i % 3 {
		case 1:
			code = "contactFlaggedAsSpam"
		case 2:
			code = "unsubscribedViaEmail"
		}
		page.Contacts = append(page.Contacts, brevo.BlockedContact{
			Email: fmt.Sprintf("user%d@x.com", i), Reason: brevo.BlockedReason{Code: code},
		})
	}
	return page, nil
}

type captureUpserts struct{ rows []models.Suppression }

func (c *captureUpserts) Upsert(s *models.Suppression) error { c.rows = append(c.rows, *s); return nil }

// failingUpserts fails every Upsert call, so a caller can assert that a
// failed write is neither counted as Imported nor silently ignored.
type failingUpserts struct{ calls int }

func (f *failingUpserts) Upsert(*models.Suppression) error {
	f.calls++
	return errors.New("db down")
}

func TestImportBrevoBlockedPagesAndMapsKinds(t *testing.T) {
	src := &fakeBlocked{total: 250}
	sink := &captureUpserts{}
	ws := uint(10)
	scope := repositories.ResourceScope{UserID: 1, WorkspaceID: &ws}

	res, err := importBrevoBlocked(context.Background(), src, sink, scope, brevoImportParams{APIKey: "k", Offset: 0, MaxPages: 2})
	if err != nil {
		t.Fatal(err)
	}
	if res.Fetched != 200 || res.Imported != 200 || res.NextOffset == nil || *res.NextOffset != 200 || res.Total != 250 {
		t.Fatalf("first call = %+v", res)
	}
	if sink.rows[0].Kind != models.SuppressionKindBounce || sink.rows[1].Kind != models.SuppressionKindComplaint ||
		sink.rows[2].Kind != models.SuppressionKindHard || *sink.rows[0].WorkspaceID != 10 {
		t.Fatalf("kind mapping wrong: %+v", sink.rows[:3])
	}

	res, err = importBrevoBlocked(context.Background(), src, sink, scope, brevoImportParams{APIKey: "k", Offset: 200, MaxPages: 2})
	if err != nil || res.Fetched != 50 || res.NextOffset != nil {
		t.Fatalf("second call = %+v err=%v", res, err)
	}
}

// TestImportBrevoBlockedFailedUpsertNotCountedButAttempted guards the
// review fix: a failed suppression write must not be reported as
// imported, but the import must still attempt every contact (not abort)
// and must not crash for lack of logging plumbing.
func TestImportBrevoBlockedFailedUpsertNotCountedButAttempted(t *testing.T) {
	src := &fakeBlocked{total: 5}
	sink := &failingUpserts{}
	scope := repositories.ResourceScope{UserID: 1}

	res, err := importBrevoBlocked(context.Background(), src, sink, scope, brevoImportParams{APIKey: "k", MaxPages: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Fetched != 5 || res.Imported != 0 {
		t.Fatalf("res = %+v", res)
	}
	if sink.calls != 5 {
		t.Fatalf("expected every contact to be attempted, calls = %d", sink.calls)
	}
}
