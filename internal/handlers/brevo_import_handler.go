// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"errors"
	"regexp"

	"github.com/goposta/posta/internal/models"
	"github.com/goposta/posta/internal/services/brevo"
	"github.com/goposta/posta/internal/storage/repositories"
	"github.com/jkaninda/okapi"
)

const (
	brevoPageSize        = 100
	brevoDefaultMaxPages = 20
	brevoMaxPages        = 50
)

var brevoDate = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

type blockedSource interface {
	BlockedContacts(ctx context.Context, apiKey string, q brevo.BlockedQuery) (*brevo.BlockedPage, error)
}

type suppressionUpserter interface {
	Upsert(*models.Suppression) error
}

// BrevoImportHandler copies Brevo's blocked/unsubscribed transactional
// contacts into the workspace suppression list. The Brevo key is used for
// this call only and never stored or logged.
type BrevoImportHandler struct {
	source blockedSource
	sink   suppressionUpserter
}

func NewBrevoImportHandler(source blockedSource, sink suppressionUpserter) *BrevoImportHandler {
	return &BrevoImportHandler{source: source, sink: sink}
}

type ImportBrevoRequest struct {
	Body struct {
		APIKey    string `json:"api_key" required:"true" doc:"Brevo API key (used for this request only, never stored)"`
		StartDate string `json:"start_date" doc:"YYYY-MM-DD; requires end_date"`
		EndDate   string `json:"end_date" doc:"YYYY-MM-DD; requires start_date"`
		Offset    int    `json:"offset" doc:"Resume from this offset (use next_offset from the previous call)"`
		MaxPages  int    `json:"max_pages" doc:"Pages of 100 contacts to import in this call (default 20, max 50)"`
	} `json:"body"`
}

type ImportBrevoResponse struct {
	Fetched    int   `json:"fetched"`
	Imported   int   `json:"imported"`
	Total      int64 `json:"total"`
	NextOffset *int  `json:"next_offset"`
}

type brevoImportParams struct {
	APIKey, StartDate, EndDate string
	Offset, MaxPages           int
}

func (h *BrevoImportHandler) Import(c *okapi.Context, req *ImportBrevoRequest) error {
	if err := requireEdit(c); err != nil {
		return c.AbortForbidden("insufficient workspace permissions", err)
	}
	b := req.Body
	if (b.StartDate == "") != (b.EndDate == "") {
		return c.AbortBadRequest("start_date and end_date must be provided together")
	}
	if b.StartDate != "" && (!brevoDate.MatchString(b.StartDate) || !brevoDate.MatchString(b.EndDate)) {
		return c.AbortBadRequest("dates must use YYYY-MM-DD")
	}
	if b.Offset < 0 {
		return c.AbortBadRequest("offset must be >= 0")
	}
	pages := b.MaxPages
	if pages <= 0 {
		pages = brevoDefaultMaxPages
	}
	if pages > brevoMaxPages {
		pages = brevoMaxPages
	}
	res, err := importBrevoBlocked(c.Request().Context(), h.source, h.sink, getScope(c), brevoImportParams{
		APIKey: b.APIKey, StartDate: b.StartDate, EndDate: b.EndDate, Offset: b.Offset, MaxPages: pages,
	})
	if errors.Is(err, brevo.ErrUnauthorized) {
		return c.AbortBadRequest("Brevo rejected the API key")
	}
	if err != nil {
		return c.AbortBadGateway("failed to read blocked contacts from Brevo", err)
	}
	return ok(c, res)
}

func importBrevoBlocked(ctx context.Context, src blockedSource, sink suppressionUpserter, scope repositories.ResourceScope, p brevoImportParams) (*ImportBrevoResponse, error) {
	res := &ImportBrevoResponse{}
	offset := p.Offset
	for i := 0; i < p.MaxPages; i++ {
		page, err := src.BlockedContacts(ctx, p.APIKey, brevo.BlockedQuery{
			StartDate: p.StartDate, EndDate: p.EndDate, Limit: brevoPageSize, Offset: offset,
		})
		if err != nil {
			return nil, err
		}
		res.Total = page.Count
		for _, bc := range page.Contacts {
			if bc.Email == "" {
				continue
			}
			if err := sink.Upsert(&models.Suppression{
				UserID: scope.UserID, WorkspaceID: scope.WorkspaceID, Email: bc.Email,
				Kind: brevoReasonKind(bc.Reason.Code), Reason: "imported from Brevo: " + bc.Reason.Code,
			}); err == nil {
				res.Imported++
			}
		}
		res.Fetched += len(page.Contacts)
		offset += len(page.Contacts)
		if len(page.Contacts) < brevoPageSize {
			return res, nil // exhausted: NextOffset stays nil
		}
	}
	res.NextOffset = &offset
	return res, nil
}

func brevoReasonKind(code string) models.SuppressionKind {
	switch code {
	case "hardBounce":
		return models.SuppressionKindBounce
	case "contactFlaggedAsSpam":
		return models.SuppressionKindComplaint
	default:
		return models.SuppressionKindHard
	}
}
