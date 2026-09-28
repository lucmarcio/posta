// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package repositories

import (
	"testing"

	"github.com/goposta/posta/internal/models"
)

func TestSuppressedSetGlobalOnlyAndScoped(t *testing.T) {
	db := testDB(t)
	if err := db.AutoMigrate(&models.Suppression{}); err != nil {
		t.Skipf("skipping: %v", err)
	}
	tx := db.Begin()
	defer tx.Rollback()
	uid := createUser(t, tx, "supset@example.com")
	ws, other := uint(9001), uint(9002)
	list := uint(5)
	repo := NewSuppressionRepository(tx)
	for _, s := range []models.Suppression{
		{UserID: uid, WorkspaceID: &ws, Email: "global@x.com", Kind: models.SuppressionKindBounce},
		{UserID: uid, WorkspaceID: &ws, Email: "listonly@x.com", ListID: &list, Kind: models.SuppressionKindListUnsubscribe},
		{UserID: uid, WorkspaceID: &other, Email: "otherws@x.com", Kind: models.SuppressionKindBounce},
	} {
		s := s
		if err := repo.Create(&s); err != nil {
			t.Fatal(err)
		}
	}
	set, err := repo.SuppressedSet(ResourceScope{UserID: uid, WorkspaceID: &ws},
		[]string{"GLOBAL@x.com", "listonly@x.com", "otherws@x.com", "clean@x.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := set["global@x.com"]; !ok || len(set) != 1 {
		t.Fatalf("set = %v, want only global@x.com", set)
	}
}
