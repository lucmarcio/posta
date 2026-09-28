// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package brevo is a minimal client for the Brevo (ex-Sendinblue) REST API,
// limited to what Posta needs to mirror Brevo's suppression data.
package brevo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

var ErrUnauthorized = errors.New("brevo: API key rejected")

type Client struct {
	BaseURL string
	HTTP    *http.Client
}

func NewClient() *Client {
	return &Client{BaseURL: "https://api.brevo.com/v3", HTTP: &http.Client{Timeout: 30 * time.Second}}
}

type BlockedReason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type BlockedContact struct {
	Email       string        `json:"email"`
	SenderEmail *string       `json:"senderEmail"`
	BlockedAt   string        `json:"blockedAt"`
	Reason      BlockedReason `json:"reason"`
}

type BlockedPage struct {
	Count    int64            `json:"count"`
	Contacts []BlockedContact `json:"contacts"`
}

// BlockedQuery pages GET /smtp/blockedContacts. Results are requested in
// ascending creation order so offsets stay stable while new blocks arrive.
type BlockedQuery struct {
	StartDate, EndDate string // YYYY-MM-DD; both or neither
	Limit, Offset      int
}

func (c *Client) BlockedContacts(ctx context.Context, apiKey string, q BlockedQuery) (*BlockedPage, error) {
	v := url.Values{}
	v.Set("limit", strconv.Itoa(q.Limit))
	v.Set("offset", strconv.Itoa(q.Offset))
	v.Set("sort", "asc")
	if q.StartDate != "" && q.EndDate != "" {
		v.Set("startDate", q.StartDate)
		v.Set("endDate", q.EndDate)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/smtp/blockedContacts?"+v.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("api-key", apiKey)
	req.Header.Set("Accept", "application/json")

	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("brevo: request failed: %w", err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
		return nil, ErrUnauthorized
	}
	if res.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return nil, fmt.Errorf("brevo: unexpected status %d: %s", res.StatusCode, body)
	}
	var page BlockedPage
	if err := json.NewDecoder(res.Body).Decode(&page); err != nil {
		return nil, fmt.Errorf("brevo: decode response: %w", err)
	}
	return &page, nil
}
