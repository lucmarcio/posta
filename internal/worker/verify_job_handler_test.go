// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package worker

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"testing"

	"github.com/goposta/posta/internal/models"
	"github.com/goposta/posta/internal/services/verifier"
	"github.com/goposta/posta/internal/storage/repositories"
	"github.com/hibiken/asynq"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

type jobResolver struct{}

func (jobResolver) LookupMX(_ context.Context, d string) ([]*net.MX, error) {
	if d == "example.com" {
		return []*net.MX{{Host: ".", Pref: 0}}, nil // null MX
	}
	return []*net.MX{{Host: "mx." + d, Pref: 10}}, nil
}
func (jobResolver) LookupHost(context.Context, string) ([]string, error) { return nil, nil }

func verifyTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("skipping: TEST_DATABASE_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	if err := db.AutoMigrate(&models.User{}, &models.Suppression{}, &models.Email{}, &models.Bounce{},
		&models.EmailVerifyJob{}, &models.EmailVerifyItem{}); err != nil {
		t.Skipf("skipping: %v", err)
	}
	return db
}

func TestVerifyJobProcessesPendingAndApplies(t *testing.T) {
	db := verifyTestDB(t)
	tx := db.Begin()
	defer tx.Rollback()

	u := &models.User{Name: "t", Email: "verifyjob@example.com", PasswordHash: "x"}
	if err := tx.Create(u).Error; err != nil {
		t.Fatal(err)
	}
	ws := uint(8801)
	jobs := repositories.NewEmailVerifyJobRepository(tx)
	sup := repositories.NewSuppressionRepository(tx)
	v := verifier.NewService(nil, sup, repositories.NewBounceRepository(tx), verifier.Options{Enabled: true})
	v.SetResolver(jobResolver{})

	job := &models.EmailVerifyJob{UserID: u.ID, WorkspaceID: &ws, Source: "emails", Apply: true}
	if err := jobs.CreateWithItems(job, []string{"ok@good.com", "bad-syntax", "x@example.com"}); err != nil {
		t.Fatal(err)
	}
	// Simulate a previous partial run: first item already done.
	first, _ := jobs.PendingItems(job.ID, 1)
	first[0].Status = "valid"
	_ = jobs.SaveResults(job.ID, first)

	h := NewVerifyJobHandler(jobs, v, sup)
	payload, _ := json.Marshal(VerifyJobPayload{JobID: job.ID})
	if err := h.ProcessTask(context.Background(), asynq.NewTask(TypeVerifyJob, payload)); err != nil {
		t.Fatal(err)
	}

	got, _ := jobs.FindByID(job.ID)
	if got.Status != models.EmailVerifyJobCompleted || got.Processed != 3 {
		t.Fatalf("job = %+v", got)
	}
	counts, _ := jobs.StatusCounts(job.ID)
	if counts["valid"] != 1 || counts["invalid"] != 2 {
		t.Fatalf("counts = %v", counts)
	}
	scope := repositories.ResourceScope{UserID: u.ID, WorkspaceID: &ws}
	if s, _ := sup.IsSuppressed(scope, "x@example.com"); !s {
		t.Fatal("apply=true must suppress invalid addresses")
	}
}
