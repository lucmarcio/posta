// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package repositories

import (
	"testing"

	"github.com/goposta/posta/internal/models"
	"github.com/lib/pq"
)

func TestHardBouncedSetScopedHardOnly(t *testing.T) {
	db := testDB(t)
	if err := db.AutoMigrate(&models.Email{}, &models.Bounce{}); err != nil {
		t.Skipf("skipping: %v", err)
	}
	tx := db.Begin()
	defer tx.Rollback()
	uid := createUser(t, tx, "bncset@example.com")
	ws, other := uint(9001), uint(9002)
	email := &models.Email{UserID: uid, Sender: "s@x.com", Recipients: pq.StringArray{"r@x.com"}, Subject: "s"}
	if err := tx.Create(email).Error; err != nil {
		t.Fatal(err)
	}
	repo := NewBounceRepository(tx)
	for _, b := range []models.Bounce{
		{UserID: uid, WorkspaceID: &ws, EmailID: email.ID, Recipient: "Hard@x.com", Type: models.BounceTypeHard},
		{UserID: uid, WorkspaceID: &ws, EmailID: email.ID, Recipient: "soft@x.com", Type: models.BounceTypeSoft},
		{UserID: uid, WorkspaceID: &other, EmailID: email.ID, Recipient: "otherws@x.com", Type: models.BounceTypeHard},
	} {
		b := b
		if err := repo.Create(&b); err != nil {
			t.Fatal(err)
		}
	}
	set, err := repo.HardBouncedSet(ResourceScope{UserID: uid, WorkspaceID: &ws},
		[]string{"hard@x.com", "soft@x.com", "otherws@x.com", "clean@x.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := set["hard@x.com"]; !ok || len(set) != 1 {
		t.Fatalf("set = %v, want only hard@x.com", set)
	}
}
