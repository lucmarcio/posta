// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/goposta/posta/internal/models"
	"github.com/goposta/posta/internal/services/verifier"
	"github.com/goposta/posta/internal/storage/repositories"
	"github.com/hibiken/asynq"
	"github.com/jkaninda/logger"
	"gorm.io/gorm"
)

// verifyJobChunk is how many pending items are verified and saved at a time.
const verifyJobChunk = 500

// VerifyJobHandler processes bulk email verification jobs.
type VerifyJobHandler struct {
	jobs         *repositories.EmailVerifyJobRepository
	verifier     *verifier.Service
	suppressions *repositories.SuppressionRepository
}

func NewVerifyJobHandler(jobs *repositories.EmailVerifyJobRepository, v *verifier.Service, sup *repositories.SuppressionRepository) *VerifyJobHandler {
	return &VerifyJobHandler{jobs: jobs, verifier: v, suppressions: sup}
}

// ProcessTask verifies a job's pending items in chunks. It is resumable: a
// retried task picks up only items still pending.
func (h *VerifyJobHandler) ProcessTask(ctx context.Context, t *asynq.Task) (err error) {
	var p VerifyJobPayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		return fmt.Errorf("verify job: bad payload: %w: %w", err, asynq.SkipRetry)
	}
	job, err := h.jobs.FindByID(p.JobID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("verify job %d not found: %w: %w", p.JobID, err, asynq.SkipRetry)
		}
		return fmt.Errorf("verify job %d: load: %w", p.JobID, err) // transient: retry
	}
	if job.Status == models.EmailVerifyJobCompleted || job.Status == models.EmailVerifyJobFailed {
		return nil
	}
	defer func() {
		if err != nil && isLastAttempt(ctx) {
			if mErr := h.jobs.MarkFailed(job.ID, err.Error()); mErr != nil {
				logger.Error("verify job: cannot mark failed", "job", job.UUID, "error", mErr)
			}
		}
	}()
	if err := h.jobs.MarkRunning(job.ID); err != nil {
		return err
	}
	scope := repositories.ResourceScope{UserID: job.UserID, WorkspaceID: job.WorkspaceID}

	for {
		items, err := h.jobs.PendingItems(job.ID, verifyJobChunk)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			break
		}
		emails := make([]string, len(items))
		for i, it := range items {
			emails[i] = it.Email
		}
		results, err := h.verifier.VerifyManyUnmetered(ctx, scope, emails, job.Fresh)
		if err != nil {
			return err
		}
		// A cancelled run fills unchecked addresses with "unknown"; saving them
		// would take them out of the pending set for good. Leave the chunk
		// pending so the retry verifies it for real.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if len(results) != len(items) {
			return fmt.Errorf("verify job %s: %d results for %d items", job.UUID, len(results), len(items))
		}
		now := time.Now().UTC()
		for i := range items {
			r := results[i]
			if r == nil {
				// Never leave an item pending, or the loop would pick it again.
				r = &verifier.Result{Email: items[i].Email, Status: verifier.StatusUnknown, Reason: "no result"}
			}
			raw, _ := json.Marshal(r)
			s := string(raw)
			items[i].Status = string(r.Status)
			items[i].Score = r.Score
			items[i].Reason = r.Reason
			items[i].Suggestion = r.Suggestion
			items[i].Result = &s
			items[i].CheckedAt = &now
		}
		if err := h.jobs.SaveResults(job.ID, items); err != nil {
			return err
		}
	}

	if job.Apply && h.suppressions != nil {
		bad, err := h.jobs.EmailsWithStatus(job.ID, string(verifier.StatusInvalid), string(verifier.StatusDisposable))
		if err != nil {
			return err
		}
		for _, e := range bad {
			if err := h.suppressions.Upsert(&models.Suppression{
				UserID: job.UserID, WorkspaceID: job.WorkspaceID, Email: e,
				Kind: models.SuppressionKindManual, Reason: "email verification job " + job.UUID,
			}); err != nil {
				logger.Warn("verify job: cannot suppress address", "job", job.UUID, "error", err)
			}
		}
	}
	logger.Info("verify job completed", "job", job.UUID, "total", job.Total)
	return h.jobs.MarkCompleted(job.ID)
}

// isLastAttempt reports whether the running task will not be retried on error.
func isLastAttempt(ctx context.Context) bool {
	retried, ok1 := asynq.GetRetryCount(ctx)
	maxRetry, ok2 := asynq.GetMaxRetry(ctx)
	return ok1 && ok2 && retried >= maxRetry
}
