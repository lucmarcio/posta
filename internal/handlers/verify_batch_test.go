// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goposta/posta/internal/services/verifier"
	"github.com/jkaninda/okapi"
)

func serveVerifyBatch(t *testing.T, payload string) *httptest.ResponseRecorder {
	t.Helper()
	app := okapi.New()
	h := NewVerifyHandler(verifier.NewService(nil, nil, nil, verifier.Options{Enabled: true}))
	app.Register(okapi.RouteDefinition{Method: http.MethodPost, Path: "/emails/verify/batch", Handler: okapi.H(h.VerifyBatch)})
	req := httptest.NewRequest(http.MethodPost, "/emails/verify/batch", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	app.ServeHTTP(w, req)
	return w
}

func TestVerifyBatchRejectsEmptyAndOversized(t *testing.T) {
	if w := serveVerifyBatch(t, `{"emails":[]}`); w.Code != http.StatusBadRequest {
		t.Fatalf("empty: %d", w.Code)
	}
	var b strings.Builder
	b.WriteString(`{"emails":[`)
	for i := 0; i < MaxVerifyBatch+1; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"u%d@x.invalid"`, i)
	}
	b.WriteString(`]}`)
	if w := serveVerifyBatch(t, b.String()); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized: %d", w.Code)
	}
}

func TestVerifyBatchInvalidSyntaxIsAResultNotAnError(t *testing.T) {
	w := serveVerifyBatch(t, `{"emails":["not-an-email"]}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"invalid"`) {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}
