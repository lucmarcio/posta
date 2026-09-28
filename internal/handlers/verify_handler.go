// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"errors"
	"fmt"

	"github.com/goposta/posta/internal/services/verifier"
	"github.com/jkaninda/okapi"
)

// MaxVerifyBatch caps the synchronous batch endpoint; larger lists go through
// verification jobs.
const MaxVerifyBatch = 100

// VerifyHandler exposes the email-verification endpoint.
type VerifyHandler struct {
	svc *verifier.Service
}

func NewVerifyHandler(svc *verifier.Service) *VerifyHandler {
	return &VerifyHandler{svc: svc}
}

// VerifyAddressRequest is the body for POST /emails/verify. The email is
// validated with format:"email", so a syntactically malformed address is
// rejected with a 400 before the handler runs; the verifier still re-checks
// syntax as a safety net.
type VerifyAddressRequest struct {
	Fresh bool `query:"fresh" doc:"Bypass the cache and re-check the address"`
	Body  struct {
		Email string `json:"email" required:"true" format:"email" doc:"Email address to verify"`
	} `json:"body"`
}

// Verify checks whether an email address is valid/deliverable.
func (h *VerifyHandler) Verify(c *okapi.Context, req *VerifyAddressRequest) error {
	if h.svc == nil || !h.svc.Enabled() {
		return c.AbortNotFound("email verification is disabled")
	}

	res, err := h.svc.Verify(c.Request().Context(), getScope(c), req.Body.Email, req.Fresh)
	if err != nil {
		if errors.Is(err, verifier.ErrRateLimited) {
			return c.AbortTooManyRequests("email verification rate limit exceeded")
		}
		return c.AbortInternalServerError("failed to verify email")
	}
	return ok(c, res)
}

// VerifyBatchRequest is the body for POST /emails/verify/batch. Malformed
// entries are reported as invalid results, not rejected outright.
type VerifyBatchRequest struct {
	Fresh bool `query:"fresh" doc:"Bypass the cache and re-check every address"`
	Body  struct {
		Emails []string `json:"emails" required:"true" doc:"Up to 100 addresses; malformed entries are reported as invalid, not rejected"`
	} `json:"body"`
}

// VerifyBatchResponse is the response for POST /emails/verify/batch.
type VerifyBatchResponse struct {
	Items   []*verifier.Result      `json:"items"`
	Summary map[verifier.Status]int `json:"summary"`
}

// VerifyBatch verifies up to MaxVerifyBatch addresses synchronously, in input order.
func (h *VerifyHandler) VerifyBatch(c *okapi.Context, req *VerifyBatchRequest) error {
	if h.svc == nil || !h.svc.Enabled() {
		return c.AbortNotFound("email verification is disabled")
	}
	n := len(req.Body.Emails)
	if n == 0 {
		return c.AbortBadRequest("emails must contain at least one address")
	}
	if n > MaxVerifyBatch {
		return c.AbortRequestEntityTooLarge(fmt.Sprintf("at most %d emails per request; use /emails/verify/jobs for larger lists", MaxVerifyBatch))
	}
	items, err := h.svc.VerifyMany(c.Request().Context(), getScope(c), req.Body.Emails, req.Fresh)
	if err != nil {
		if errors.Is(err, verifier.ErrRateLimited) {
			return c.AbortTooManyRequests("email verification rate limit exceeded")
		}
		return c.AbortInternalServerError("failed to verify emails")
	}
	summary := make(map[verifier.Status]int)
	for _, it := range items {
		summary[it.Status]++
	}
	return ok(c, VerifyBatchResponse{Items: items, Summary: summary})
}
