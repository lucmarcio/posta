// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goposta/posta/internal/services/bounceingest"
	"github.com/goposta/posta/internal/storage/repositories"
	"github.com/jkaninda/okapi"
)

type recordedCall struct {
	scope repositories.ResourceScope
	ev    bounceingest.Event
}

type fakeRecorder struct{ calls []recordedCall }

func (f *fakeRecorder) Record(s repositories.ResourceScope, ev bounceingest.Event) bounceingest.Outcome {
	f.calls = append(f.calls, recordedCall{s, ev})
	return bounceingest.Outcome{Email: ev.Email, Kind: ev.Kind, Action: bounceingest.ActionSuppressed, Suppressed: true}
}

func serveBrevo(t *testing.T, rec *fakeRecorder, payload string) (*httptest.ResponseRecorder, BrevoWebhookResponse) {
	t.Helper()
	app := okapi.New()
	h := NewBrevoWebhookHandler(rec)
	app.Register(okapi.RouteDefinition{Method: http.MethodPost, Path: "/webhooks/brevo", Handler: h.Handle})
	req := httptest.NewRequest(http.MethodPost, "/webhooks/brevo", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	app.ServeHTTP(w, req)
	var env struct {
		Data BrevoWebhookResponse `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	return w, env.Data
}

func TestBrevoSingleHardBounce(t *testing.T) {
	rec := &fakeRecorder{}
	w, resp := serveBrevo(t, rec, `{"event":"hard_bounce","email":"a@b.com","reason":"550 5.1.1",
		"X-Mailin-custom":"posta-id=11111111-1111-1111-1111-111111111111","message-id":"<x@relay>"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if len(rec.calls) != 1 || rec.calls[0].ev.Kind != bounceingest.KindHard ||
		rec.calls[0].ev.EmailUUID != "11111111-1111-1111-1111-111111111111" || rec.calls[0].ev.Source != "brevo" {
		t.Fatalf("recorder call wrong: %+v", rec.calls)
	}
	if resp.Received != 1 || resp.Processed != 1 || resp.Ignored != 0 {
		t.Fatalf("counts wrong: %+v", resp)
	}
}

func TestBrevoBatchedMixed(t *testing.T) {
	rec := &fakeRecorder{}
	w, resp := serveBrevo(t, rec, `[
		{"event":"spam","email":"s@b.com"},
		{"event":"blocked","email":"bl@b.com"},
		{"event":"invalid_email","email":"inv@b.com"},
		{"event":"soft_bounce","email":"so@b.com"},
		{"event":"unsubscribed","email":"u@b.com"},
		{"event":"delivered","email":"d@b.com"},
		{"event":"hard_bounce","email":""}
	]`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	want := []bounceingest.Kind{bounceingest.KindComplaint, bounceingest.KindHard, bounceingest.KindHard,
		bounceingest.KindSoft, bounceingest.KindUnsubscribe}
	if len(rec.calls) != len(want) {
		t.Fatalf("calls = %d, want %d", len(rec.calls), len(want))
	}
	for i, k := range want {
		if rec.calls[i].ev.Kind != k {
			t.Errorf("call %d kind = %s, want %s", i, rec.calls[i].ev.Kind, k)
		}
	}
	if resp.Received != 7 || resp.Processed != 5 || resp.Ignored != 2 || len(resp.Items) != 7 {
		t.Fatalf("counts wrong: %+v", resp)
	}
}

func TestBrevoMalformedJSON(t *testing.T) {
	w, _ := serveBrevo(t, &fakeRecorder{}, `{not json`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", w.Code)
	}
}

func TestPostaIDFromCustom(t *testing.T) {
	cases := map[string]string{
		"posta-id=11111111-1111-1111-1111-111111111111":          "11111111-1111-1111-1111-111111111111",
		"foo=bar; posta-id=11111111-1111-1111-1111-111111111111": "11111111-1111-1111-1111-111111111111",
		"posta-id=not-a-uuid":                                    "",
		"":                                                       "",
		"some_custom_header":                                     "",
	}
	for in, want := range cases {
		if got := postaIDFromCustom(in); got != want {
			t.Errorf("postaIDFromCustom(%q) = %q, want %q", in, got, want)
		}
	}
}
