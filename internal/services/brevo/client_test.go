// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package brevo

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBlockedContacts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/smtp/blockedContacts" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("api-key") != "k1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		q := r.URL.Query()
		if q.Get("limit") != "100" || q.Get("offset") != "200" || q.Get("sort") != "asc" ||
			q.Get("startDate") != "2026-01-01" || q.Get("endDate") != "2026-09-01" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":2,"contacts":[
			{"email":"A@x.com","senderEmail":null,"blockedAt":"2026-05-01T12:30:00Z","reason":{"code":"hardBounce","message":"Hard bounce"}},
			{"email":"b@x.com","senderEmail":"s@me.com","blockedAt":"2026-05-02T12:30:00Z","reason":{"code":"contactFlaggedAsSpam","message":"Spam"}}]}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	page, err := c.BlockedContacts(context.Background(), "k1", BlockedQuery{Limit: 100, Offset: 200, StartDate: "2026-01-01", EndDate: "2026-09-01"})
	if err != nil {
		t.Fatal(err)
	}
	if page.Count != 2 || len(page.Contacts) != 2 || page.Contacts[0].Reason.Code != "hardBounce" {
		t.Fatalf("page = %+v", page)
	}
	if _, err := c.BlockedContacts(context.Background(), "bad", BlockedQuery{Limit: 100}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
}
