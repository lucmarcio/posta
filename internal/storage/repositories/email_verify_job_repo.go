// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package repositories

import (
	"time"

	"github.com/google/uuid"
	"github.com/goposta/posta/internal/models"
	"gorm.io/gorm"
)

// EmailVerifyJobRepository persists bulk verification jobs and their items.
type EmailVerifyJobRepository struct {
	db *gorm.DB
}

func NewEmailVerifyJobRepository(db *gorm.DB) *EmailVerifyJobRepository {
	return &EmailVerifyJobRepository{db: db}
}

// CreateWithItems creates a queued job and one pending item per address, in a
// single transaction.
func (r *EmailVerifyJobRepository) CreateWithItems(job *models.EmailVerifyJob, emails []string) error {
	job.Total = len(emails)
	job.Processed = 0
	job.Status = models.EmailVerifyJobQueued
	return r.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(job).Error; err != nil {
			return err
		}
		if len(emails) == 0 {
			return nil
		}
		items := make([]models.EmailVerifyItem, len(emails))
		for i, e := range emails {
			items[i] = models.EmailVerifyItem{JobID: job.ID, Email: e, Status: models.EmailVerifyItemPending}
		}
		return tx.CreateInBatches(items, 1000).Error
	})
}

// FindByID loads a job without scoping; for the worker only.
func (r *EmailVerifyJobRepository) FindByID(id uint) (*models.EmailVerifyJob, error) {
	var job models.EmailVerifyJob
	if err := r.db.First(&job, id).Error; err != nil {
		return nil, err
	}
	return &job, nil
}

// FindByUUID loads a job visible from the given scope. A malformed UUID is
// reported as not found rather than as a database error.
func (r *EmailVerifyJobRepository) FindByUUID(scope ResourceScope, id string) (*models.EmailVerifyJob, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, gorm.ErrRecordNotFound
	}
	var job models.EmailVerifyJob
	if err := ApplyScope(r.db, scope).Where("uuid = ?", id).First(&job).Error; err != nil {
		return nil, err
	}
	return &job, nil
}

// FindByIdempotencyKey returns the scope's job created with the given key.
func (r *EmailVerifyJobRepository) FindByIdempotencyKey(scope ResourceScope, key string) (*models.EmailVerifyJob, error) {
	var job models.EmailVerifyJob
	if err := ApplyScope(r.db, scope).Where("idempotency_key = ?", key).First(&job).Error; err != nil {
		return nil, err
	}
	return &job, nil
}

// activeJobMaxAge bounds how long a queued or running job counts as active.
// It exceeds the task timeout (2h) times the retries plus the retry backoff, so
// a job older than this was abandoned (worker crash, lost task) and must not
// block new jobs forever.
const activeJobMaxAge = 24 * time.Hour

// CountActive counts the scope's jobs that are queued or running, ignoring
// jobs older than activeJobMaxAge.
func (r *EmailVerifyJobRepository) CountActive(scope ResourceScope) (int64, error) {
	var n int64
	err := ApplyScope(r.db.Model(&models.EmailVerifyJob{}), scope).
		Where("status IN ?", []models.EmailVerifyJobStatus{models.EmailVerifyJobQueued, models.EmailVerifyJobRunning}).
		Where("created_at > ?", time.Now().Add(-activeJobMaxAge)).
		Count(&n).Error
	return n, err
}

// PendingItems returns up to limit items not yet verified, oldest first.
func (r *EmailVerifyJobRepository) PendingItems(jobID uint, limit int) ([]models.EmailVerifyItem, error) {
	var items []models.EmailVerifyItem
	err := r.db.Where("job_id = ? AND status = ?", jobID, models.EmailVerifyItemPending).
		Order("id").Limit(limit).Find(&items).Error
	return items, err
}

// SaveResults stores the results of a chunk. Only items still pending are
// updated, and the job's processed counter grows by the rows actually changed,
// so a resumed task that re-saves a chunk never double counts.
func (r *EmailVerifyJobRepository) SaveResults(jobID uint, items []models.EmailVerifyItem) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		var changed int64
		for i := range items {
			it := items[i]
			res := tx.Model(&models.EmailVerifyItem{}).
				Where("id = ? AND job_id = ? AND status = ?", it.ID, jobID, models.EmailVerifyItemPending).
				Updates(map[string]any{
					"status": it.Status, "score": it.Score, "reason": it.Reason,
					"suggestion": it.Suggestion, "result": it.Result, "checked_at": it.CheckedAt,
				})
			if res.Error != nil {
				return res.Error
			}
			changed += res.RowsAffected
		}
		if changed == 0 {
			return nil
		}
		return tx.Model(&models.EmailVerifyJob{}).Where("id = ?", jobID).
			Update("processed", gorm.Expr("processed + ?", changed)).Error
	})
}

// MarkRunning flags the job running, keeping the first start time on retries.
func (r *EmailVerifyJobRepository) MarkRunning(jobID uint) error {
	return r.db.Model(&models.EmailVerifyJob{}).Where("id = ?", jobID).Updates(map[string]any{
		"status":     models.EmailVerifyJobRunning,
		"started_at": gorm.Expr("COALESCE(started_at, ?)", time.Now().UTC()),
	}).Error
}

// MarkCompleted flags the job completed.
func (r *EmailVerifyJobRepository) MarkCompleted(jobID uint) error {
	return r.db.Model(&models.EmailVerifyJob{}).Where("id = ?", jobID).Updates(map[string]any{
		"status":       models.EmailVerifyJobCompleted,
		"completed_at": time.Now().UTC(),
	}).Error
}

// MarkFailed flags the job failed with the given message.
func (r *EmailVerifyJobRepository) MarkFailed(jobID uint, msg string) error {
	return r.db.Model(&models.EmailVerifyJob{}).Where("id = ?", jobID).Updates(map[string]any{
		"status": models.EmailVerifyJobFailed,
		"error":  msg,
	}).Error
}

// ListItems pages through a job's items, optionally filtered by status.
func (r *EmailVerifyJobRepository) ListItems(jobID uint, status string, limit, offset int) ([]models.EmailVerifyItem, int64, error) {
	query := func() *gorm.DB {
		q := r.db.Model(&models.EmailVerifyItem{}).Where("job_id = ?", jobID)
		if status != "" {
			q = q.Where("status = ?", status)
		}
		return q
	}
	var total int64
	if err := query().Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var items []models.EmailVerifyItem
	if err := query().Order("id").Limit(limit).Offset(offset).Find(&items).Error; err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// EachItem calls fn for every item of the job, in id order, loading 1000 at a
// time. An error from fn stops the iteration and is returned.
func (r *EmailVerifyJobRepository) EachItem(jobID uint, fn func(models.EmailVerifyItem) error) error {
	var batch []models.EmailVerifyItem
	var fnErr error
	res := r.db.Where("job_id = ?", jobID).Order("id").
		FindInBatches(&batch, 1000, func(_ *gorm.DB, _ int) error {
			for _, it := range batch {
				if err := fn(it); err != nil {
					fnErr = err
					return err
				}
			}
			return nil
		})
	if fnErr != nil {
		return fnErr
	}
	return res.Error
}

// EmailsWithStatus returns the addresses of the job's items in any of the
// given statuses.
func (r *EmailVerifyJobRepository) EmailsWithStatus(jobID uint, statuses ...string) ([]string, error) {
	var emails []string
	if len(statuses) == 0 {
		return emails, nil
	}
	err := r.db.Model(&models.EmailVerifyItem{}).
		Where("job_id = ? AND status IN ?", jobID, statuses).
		Order("id").Pluck("email", &emails).Error
	return emails, err
}

// StatusCounts returns the number of the job's items per status.
func (r *EmailVerifyJobRepository) StatusCounts(jobID uint) (map[string]int64, error) {
	var rows []struct {
		Status string
		Count  int64
	}
	err := r.db.Model(&models.EmailVerifyItem{}).
		Select("status, COUNT(*) AS count").
		Where("job_id = ?", jobID).
		Group("status").Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int64, len(rows))
	for _, row := range rows {
		counts[row.Status] = row.Count
	}
	return counts, nil
}
