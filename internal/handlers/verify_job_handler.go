// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/goposta/posta/internal/dto"
	"github.com/goposta/posta/internal/models"
	"github.com/goposta/posta/internal/services/verifier"
	"github.com/goposta/posta/internal/storage/repositories"
	"github.com/goposta/posta/internal/worker"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jkaninda/okapi"
	"gorm.io/gorm"
)

// MaxVerifyJobEmails caps a single verification job; larger lists must be
// split by the caller.
const MaxVerifyJobEmails = 50000

// MaxActiveVerifyJobs caps the scope's concurrently active (queued or
// running) verification jobs.
const MaxActiveVerifyJobs = 2

// VerifyJobHandler exposes the bulk email-verification job endpoints:
// create, status, paginated results and CSV export.
type VerifyJobHandler struct {
	jobRepo        *repositories.EmailVerifyJobRepository
	listRepo       *repositories.SubscriberListRepository
	subscriberRepo *repositories.SubscriberRepository
	verifierSvc    *verifier.Service
	producer       *worker.Producer
}

func NewVerifyJobHandler(
	jobRepo *repositories.EmailVerifyJobRepository,
	listRepo *repositories.SubscriberListRepository,
	subscriberRepo *repositories.SubscriberRepository,
	verifierSvc *verifier.Service,
	producer *worker.Producer,
) *VerifyJobHandler {
	return &VerifyJobHandler{
		jobRepo:        jobRepo,
		listRepo:       listRepo,
		subscriberRepo: subscriberRepo,
		verifierSvc:    verifierSvc,
		producer:       producer,
	}
}

// JobView is the API representation of an EmailVerifyJob.
type JobView struct {
	ID          string         `json:"id"`
	Status      string         `json:"status"`
	Total       int            `json:"total"`
	Processed   int            `json:"processed"`
	Progress    int            `json:"progress"` // 0-100
	Counts      map[string]int `json:"counts"`   // per verifier status, excludes "pending"
	Fresh       bool           `json:"fresh"`
	Apply       bool           `json:"apply"`
	Error       string         `json:"error,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	StartedAt   *time.Time     `json:"started_at,omitempty"`
	CompletedAt *time.Time     `json:"completed_at,omitempty"`
}

// ItemView is the API representation of one address of a job.
type ItemView struct {
	Email      string           `json:"email"`
	Status     string           `json:"status"`
	Score      int              `json:"score"`
	Reason     string           `json:"reason,omitempty"`
	Suggestion string           `json:"suggestion,omitempty"`
	Result     *verifier.Result `json:"result,omitempty"`
}

// toJobView converts a job and its per-status item counts into a JobView.
// Progress is Processed*100/Total, or 100 when Total is 0 (nothing to do).
// The "pending" bucket is never surfaced in Counts.
func toJobView(j *models.EmailVerifyJob, counts map[string]int64) JobView {
	progress := 100
	if j.Total > 0 {
		progress = j.Processed * 100 / j.Total
	}
	c := make(map[string]int, len(counts))
	for status, n := range counts {
		if status == models.EmailVerifyItemPending {
			continue
		}
		c[status] = int(n)
	}
	return JobView{
		ID:          j.UUID,
		Status:      string(j.Status),
		Total:       j.Total,
		Processed:   j.Processed,
		Progress:    progress,
		Counts:      c,
		Fresh:       j.Fresh,
		Apply:       j.Apply,
		Error:       j.Error,
		CreatedAt:   j.CreatedAt,
		StartedAt:   j.StartedAt,
		CompletedAt: j.CompletedAt,
	}
}

// toItemView converts a stored item into its API representation, decoding
// the serialized verifier.Result when present.
func toItemView(it models.EmailVerifyItem) ItemView {
	return ItemView{
		Email:      it.Email,
		Status:     it.Status,
		Score:      it.Score,
		Reason:     it.Reason,
		Suggestion: it.Suggestion,
		Result:     decodeItemResult(it.Result),
	}
}

// decodeItemResult unmarshals an item's stored result, returning nil when
// absent or malformed rather than failing the caller.
func decodeItemResult(raw *string) *verifier.Result {
	if raw == nil || *raw == "" {
		return nil
	}
	var res verifier.Result
	if err := json.Unmarshal([]byte(*raw), &res); err != nil {
		return nil
	}
	return &res
}

// normalizeJobEmails trims whitespace, lowercases addresses that
// mail.ParseAddress accepts (malformed entries are kept as-is, not
// discarded), drops blanks, and deduplicates while preserving input order.
func normalizeJobEmails(emails []string) []string {
	seen := make(map[string]struct{}, len(emails))
	out := make([]string, 0, len(emails))
	for _, raw := range emails {
		e := strings.TrimSpace(raw)
		if e == "" {
			continue
		}
		if _, err := mail.ParseAddress(e); err == nil {
			e = strings.ToLower(e)
		}
		if _, dup := seen[e]; dup {
			continue
		}
		seen[e] = struct{}{}
		out = append(out, e)
	}
	return out
}

// isUniqueViolation reports whether err is a Postgres unique-constraint
// violation, read from the driver error directly (gorm.ErrDuplicatedKey only
// exists when the connection is opened with TranslateError).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

// normalizeJobResultsPageParams applies the results endpoint's own bounds
// (default 100, max 1000), distinct from the generic list defaults.
func normalizeJobResultsPageParams(page, size int) (int, int, int) {
	if size <= 0 || size > 1000 {
		size = 100
	}
	if page < 0 {
		page = 0
	}
	return page, size, page * size
}

// CreateVerifyJobRequest is the body for POST /emails/verify/jobs. Exactly one
// of Emails/SubscriberListID must be set.
type CreateVerifyJobRequest struct {
	IdempotencyKey string `header:"Idempotency-Key" maxLength:"128" doc:"Optional key; replaying it returns the existing job instead of creating another"`
	Body           struct {
		Emails           []string `json:"emails,omitempty" doc:"Addresses to verify; mutually exclusive with subscriber_list_id"`
		SubscriberListID *uint    `json:"subscriber_list_id,omitempty" doc:"Subscriber list to verify; mutually exclusive with emails"`
		Fresh            bool     `json:"fresh" doc:"Bypass the cache and re-check every address"`
		Apply            bool     `json:"apply" doc:"Apply verification results to the tenant's data once the job completes"`
	} `json:"body"`
}

// CreateJob starts a bulk verification job for up to MaxVerifyJobEmails
// addresses, either given directly or resolved from a subscriber list.
func (h *VerifyJobHandler) CreateJob(c *okapi.Context, req *CreateVerifyJobRequest) error {
	if h.verifierSvc == nil || !h.verifierSvc.Enabled() {
		return c.AbortNotFound("email verification is disabled")
	}
	if h.producer == nil {
		return c.AbortServiceUnavailable("email verification jobs are not available")
	}

	scope := getScope(c)
	key := strings.TrimSpace(req.IdempotencyKey)
	if key != "" {
		existing, err := h.jobRepo.FindByIdempotencyKey(scope, key)
		if err == nil {
			return h.acceptExisting(c, existing)
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return c.AbortInternalServerError("failed to check idempotency key")
		}
	}

	hasEmails := len(req.Body.Emails) > 0
	hasList := req.Body.SubscriberListID != nil
	if hasEmails == hasList {
		return c.AbortBadRequest("exactly one of emails or subscriber_list_id is required")
	}

	var emails []string
	var source string
	var listID *uint
	if hasEmails {
		source = "emails"
		emails = normalizeJobEmails(req.Body.Emails)
	} else {
		source = "subscriber_list"
		list, err := h.listRepo.FindByID(*req.Body.SubscriberListID)
		if err != nil || !ownsResource(c, list.UserID, list.WorkspaceID) {
			return c.AbortNotFound("subscriber list not found")
		}
		listID = &list.ID

		var subs []models.Subscriber
		if list.Type == models.SubscriberListTypeDynamic && list.FilterRules != nil {
			subs, _, err = h.subscriberRepo.FindByFilterRules(scope, list.FilterRules, -1, 0)
		} else {
			subs, _, err = h.listRepo.ListMembers(list.ID, -1, 0)
		}
		if err != nil {
			return c.AbortInternalServerError("failed to resolve subscriber list")
		}
		raw := make([]string, len(subs))
		for i, s := range subs {
			raw[i] = s.Email
		}
		emails = normalizeJobEmails(raw)
	}

	if len(emails) == 0 {
		return c.AbortBadRequest("no addresses to verify")
	}
	if len(emails) > MaxVerifyJobEmails {
		return c.AbortRequestEntityTooLarge(fmt.Sprintf("at most %d emails per verification job", MaxVerifyJobEmails))
	}

	active, err := h.jobRepo.CountActive(scope)
	if err != nil {
		return c.AbortInternalServerError("failed to check active verification jobs")
	}
	if active >= MaxActiveVerifyJobs {
		return c.AbortTooManyRequests("too many active verification jobs")
	}

	job := &models.EmailVerifyJob{
		UserID:           scope.UserID,
		WorkspaceID:      scope.WorkspaceID,
		Source:           source,
		SubscriberListID: listID,
		Fresh:            req.Body.Fresh,
		Apply:            req.Body.Apply,
	}
	if key != "" {
		job.IdempotencyKey = &key
	}

	if err := h.jobRepo.CreateWithItems(job, emails); err != nil {
		if key != "" && isUniqueViolation(err) {
			if existing, ferr := h.jobRepo.FindByIdempotencyKey(scope, key); ferr == nil {
				return h.acceptExisting(c, existing)
			}
		}
		return c.AbortInternalServerError("failed to create verification job")
	}

	if err := h.producer.EnqueueVerifyJob(job.ID); err != nil {
		_ = h.jobRepo.MarkFailed(job.ID, "enqueue failed")
		return c.AbortInternalServerError("failed to enqueue verification job")
	}

	return c.JSON(http.StatusAccepted, dto.Response[JobView]{Success: true, Data: toJobView(job, nil)})
}

// acceptExisting answers an idempotent replay with the previously created job.
func (h *VerifyJobHandler) acceptExisting(c *okapi.Context, job *models.EmailVerifyJob) error {
	counts, err := h.jobRepo.StatusCounts(job.ID)
	if err != nil {
		counts = nil
	}
	return c.JSON(http.StatusAccepted, dto.Response[JobView]{Success: true, Data: toJobView(job, counts)})
}

// GetVerifyJobRequest is the path for GET /emails/verify/jobs/{id}.
type GetVerifyJobRequest struct {
	ID string `param:"id"`
}

// GetJob returns a job's current status and progress.
func (h *VerifyJobHandler) GetJob(c *okapi.Context, req *GetVerifyJobRequest) error {
	job, err := h.jobRepo.FindByUUID(getScope(c), req.ID)
	if err != nil {
		return c.AbortNotFound("job not found")
	}
	counts, err := h.jobRepo.StatusCounts(job.ID)
	if err != nil {
		counts = nil
	}
	return ok(c, toJobView(job, counts))
}

// ListVerifyJobResultsRequest is the query for GET /emails/verify/jobs/{id}/results.
type ListVerifyJobResultsRequest struct {
	ID     string `param:"id"`
	Status string `query:"status"`
	Page   int    `query:"page" default:"0"`
	Size   int    `query:"size" default:"100"`
}

// ListResults pages through a job's items, optionally filtered by status.
func (h *VerifyJobHandler) ListResults(c *okapi.Context, req *ListVerifyJobResultsRequest) error {
	job, err := h.jobRepo.FindByUUID(getScope(c), req.ID)
	if err != nil {
		return c.AbortNotFound("job not found")
	}

	page, size, offset := normalizeJobResultsPageParams(req.Page, req.Size)
	items, total, err := h.jobRepo.ListItems(job.ID, req.Status, size, offset)
	if err != nil {
		return c.AbortInternalServerError("failed to list job results")
	}

	views := make([]ItemView, len(items))
	for i, it := range items {
		views[i] = toItemView(it)
	}
	return paginated(c, views, total, page, size)
}

// VerifyJobResultsCSVRequest is the path for GET /emails/verify/jobs/{id}/results.csv.
type VerifyJobResultsCSVRequest struct {
	ID string `param:"id"`
}

// ResultsCSV streams a job's results as CSV, iterating the stored items
// without loading them all into memory at once.
func (h *VerifyJobHandler) ResultsCSV(c *okapi.Context, req *VerifyJobResultsCSVRequest) error {
	job, err := h.jobRepo.FindByUUID(getScope(c), req.ID)
	if err != nil {
		return c.AbortNotFound("job not found")
	}

	c.ResponseWriter().Header().Set("Content-Type", "text/csv")
	c.ResponseWriter().Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="verify-%s.csv"`, job.UUID))
	c.ResponseWriter().WriteHeader(http.StatusOK)

	w := csv.NewWriter(c.ResponseWriter())
	_ = w.Write([]string{
		"email", "status", "score", "reason", "suggestion",
		"suppressed", "previously_bounced", "mx", "disposable", "role_account", "smtp",
	})

	iterErr := h.jobRepo.EachItem(job.ID, func(it models.EmailVerifyItem) error {
		row := []string{
			it.Email, it.Status, strconv.Itoa(it.Score), it.Reason, it.Suggestion,
			"", "", "", "", "", "",
		}
		if res := decodeItemResult(it.Result); res != nil {
			row[5] = strconv.FormatBool(res.Suppressed)
			row[6] = strconv.FormatBool(res.PreviouslyBounced)
			row[7] = strconv.FormatBool(res.Checks.MX)
			row[8] = strconv.FormatBool(res.Checks.Disposable)
			row[9] = strconv.FormatBool(res.Checks.RoleAccount)
			row[10] = string(res.Checks.SMTP)
		}
		return w.Write(row)
	})
	w.Flush()
	if iterErr != nil {
		c.Logger().Warn("verify job CSV export failed", "job_uuid", job.UUID, "error", iterErr)
	}
	return nil
}
