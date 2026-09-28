// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package models

import "time"

type EmailVerifyJobStatus string

const (
	EmailVerifyJobQueued    EmailVerifyJobStatus = "queued"
	EmailVerifyJobRunning   EmailVerifyJobStatus = "running"
	EmailVerifyJobCompleted EmailVerifyJobStatus = "completed"
	EmailVerifyJobFailed    EmailVerifyJobStatus = "failed"
)

// EmailVerifyItemPending marks an item not yet verified; any other value is a
// verifier.Status.
const EmailVerifyItemPending = "pending"

// EmailVerifyJob is a bulk verification request processed by the worker.
type EmailVerifyJob struct {
	ID               uint                 `json:"-" gorm:"primaryKey"`
	UUID             string               `json:"id" gorm:"type:uuid;default:gen_random_uuid();uniqueIndex;not null"`
	UserID           uint                 `json:"-" gorm:"index;not null"`
	WorkspaceID      *uint                `json:"-" gorm:"index"`
	Status           EmailVerifyJobStatus `json:"status" gorm:"type:varchar(20);not null;default:'queued'"`
	Source           string               `json:"source" gorm:"type:varchar(20);not null"` // "emails" | "subscriber_list"
	SubscriberListID *uint                `json:"subscriber_list_id,omitempty"`
	Fresh            bool                 `json:"fresh"`
	Apply            bool                 `json:"apply"`
	IdempotencyKey   *string              `json:"-" gorm:"type:varchar(128)"`
	Total            int                  `json:"total"`
	Processed        int                  `json:"processed"`
	Error            string               `json:"error,omitempty"`
	CreatedAt        time.Time            `json:"created_at"`
	StartedAt        *time.Time           `json:"started_at,omitempty"`
	CompletedAt      *time.Time           `json:"completed_at,omitempty"`
}

// EmailVerifyItem is one address of a job and, once processed, its result.
type EmailVerifyItem struct {
	ID         uint       `json:"-" gorm:"primaryKey"`
	JobID      uint       `json:"-" gorm:"not null;index:idx_verify_items_job_status,priority:1"`
	Email      string     `json:"email" gorm:"not null"`
	Status     string     `json:"status" gorm:"type:varchar(20);not null;default:'pending';index:idx_verify_items_job_status,priority:2"`
	Score      int        `json:"score"`
	Reason     string     `json:"reason,omitempty"`
	Suggestion string     `json:"suggestion,omitempty"`
	Result     *string    `json:"-" gorm:"type:jsonb"`
	CheckedAt  *time.Time `json:"checked_at,omitempty"`
}
