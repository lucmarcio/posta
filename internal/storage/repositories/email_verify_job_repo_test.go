// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package repositories

import (
	"testing"

	"github.com/goposta/posta/internal/models"
)

func verifyJobDB(t *testing.T) *EmailVerifyJobRepository {
	t.Helper()
	db := testDB(t)
	if err := db.AutoMigrate(&models.EmailVerifyJob{}, &models.EmailVerifyItem{}); err != nil {
		t.Skipf("skipping: %v", err)
	}
	tx := db.Begin()
	t.Cleanup(func() { tx.Rollback() })
	return NewEmailVerifyJobRepository(tx)
}

func TestVerifyJobLifecycle(t *testing.T) {
	repo := verifyJobDB(t)
	ws := uint(7001)
	scope := ResourceScope{UserID: 1, WorkspaceID: &ws}
	job := &models.EmailVerifyJob{UserID: 1, WorkspaceID: &ws, Source: "emails"}
	if err := repo.CreateWithItems(job, []string{"a@x.com", "b@x.com", "c@x.com"}); err != nil {
		t.Fatal(err)
	}
	if job.Total != 3 || job.UUID == "" {
		t.Fatalf("job = %+v", job)
	}
	got, err := repo.FindByUUID(scope, job.UUID)
	if err != nil || got.ID != job.ID {
		t.Fatalf("FindByUUID: %v", err)
	}
	other := uint(7002)
	if _, err := repo.FindByUUID(ResourceScope{UserID: 1, WorkspaceID: &other}, job.UUID); err == nil {
		t.Fatal("job must not be visible from another workspace")
	}

	pending, _ := repo.PendingItems(job.ID, 2)
	if len(pending) != 2 {
		t.Fatalf("pending = %d", len(pending))
	}
	for i := range pending {
		pending[i].Status = "valid"
		pending[i].Score = 90
	}
	if err := repo.SaveResults(job.ID, pending); err != nil {
		t.Fatal(err)
	}
	// Saving the same chunk again (retried task) must not double count.
	if err := repo.SaveResults(job.ID, pending); err != nil {
		t.Fatal(err)
	}
	pending, _ = repo.PendingItems(job.ID, 10)
	if len(pending) != 1 || pending[0].Email != "c@x.com" {
		t.Fatalf("remaining = %+v", pending)
	}
	got, _ = repo.FindByID(job.ID)
	if got.Processed != 2 {
		t.Fatalf("processed = %d", got.Processed)
	}
	items, total, _ := repo.ListItems(job.ID, "valid", 10, 0)
	if total != 2 || len(items) != 2 {
		t.Fatalf("list valid = %d/%d", len(items), total)
	}
	counts, _ := repo.StatusCounts(job.ID)
	if counts["valid"] != 2 || counts[models.EmailVerifyItemPending] != 1 {
		t.Fatalf("counts = %v", counts)
	}
}

func TestVerifyJobQueriesAndTransitions(t *testing.T) {
	repo := verifyJobDB(t)
	ws := uint(7011)
	scope := ResourceScope{UserID: 1, WorkspaceID: &ws}
	key := "idem-7011"
	job := &models.EmailVerifyJob{UserID: 1, WorkspaceID: &ws, Source: "emails", IdempotencyKey: &key}
	if err := repo.CreateWithItems(job, []string{"a@x.com", "b@x.com", "c@x.com"}); err != nil {
		t.Fatal(err)
	}
	if got, err := repo.FindByIdempotencyKey(scope, key); err != nil || got.ID != job.ID {
		t.Fatalf("FindByIdempotencyKey: %v", err)
	}
	other := uint(7012)
	if _, err := repo.FindByIdempotencyKey(ResourceScope{UserID: 1, WorkspaceID: &other}, key); err == nil {
		t.Fatal("idempotency key must not match another workspace")
	}
	if _, err := repo.FindByUUID(scope, "not-a-uuid"); err == nil {
		t.Fatal("malformed uuid must not be found")
	}
	if n, err := repo.CountActive(scope); err != nil || n != 1 {
		t.Fatalf("CountActive = %d, %v", n, err)
	}

	if err := repo.MarkRunning(job.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := repo.FindByID(job.ID)
	if got.Status != models.EmailVerifyJobRunning || got.StartedAt == nil {
		t.Fatalf("after MarkRunning: %+v", got)
	}
	started := *got.StartedAt
	if err := repo.MarkRunning(job.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = repo.FindByID(job.ID)
	if !got.StartedAt.Equal(started) {
		t.Fatalf("MarkRunning must keep the first start time: %v != %v", got.StartedAt, started)
	}

	items, _ := repo.PendingItems(job.ID, 10)
	items[0].Status, items[1].Status, items[2].Status = "invalid", "valid", "disposable"
	if err := repo.SaveResults(job.ID, items); err != nil {
		t.Fatal(err)
	}
	bad, err := repo.EmailsWithStatus(job.ID, "invalid", "disposable")
	if err != nil || len(bad) != 2 || bad[0] != "a@x.com" || bad[1] != "c@x.com" {
		t.Fatalf("EmailsWithStatus = %v, %v", bad, err)
	}
	var seen []string
	if err := repo.EachItem(job.ID, func(it models.EmailVerifyItem) error {
		seen = append(seen, it.Email)
		return nil
	}); err != nil || len(seen) != 3 || seen[0] != "a@x.com" {
		t.Fatalf("EachItem = %v, %v", seen, err)
	}
	all, total, err := repo.ListItems(job.ID, "", 2, 1)
	if err != nil || total != 3 || len(all) != 2 || all[0].Email != "b@x.com" {
		t.Fatalf("ListItems = %+v, %d, %v", all, total, err)
	}

	if err := repo.MarkCompleted(job.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = repo.FindByID(job.ID)
	if got.Status != models.EmailVerifyJobCompleted || got.CompletedAt == nil {
		t.Fatalf("after MarkCompleted: %+v", got)
	}
	if n, _ := repo.CountActive(scope); n != 0 {
		t.Fatalf("CountActive after completion = %d", n)
	}
	if err := repo.MarkFailed(job.ID, "boom"); err != nil {
		t.Fatal(err)
	}
	got, _ = repo.FindByID(job.ID)
	if got.Status != models.EmailVerifyJobFailed || got.Error != "boom" {
		t.Fatalf("after MarkFailed: %+v", got)
	}
}
