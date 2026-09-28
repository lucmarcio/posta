# Validação de e-mails + ciclo de bounce Brevo — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fazer o Posta aprender com os bounces da Brevo, parar de enviar para endereços suprimidos e validar e-mails
de forma correta, individual e em massa, com sonda SMTP opcional.

**Architecture:**
- Um serviço `bounceingest` centraliza o registro de bounce, supressão e status do subscriber, usado pelo webhook
  genérico e pelo novo webhook da Brevo.
- O `verifier` ganha `VerifyMany`, que agrupa por domínio e processa com um pool de concorrência. É a base do
  endpoint síncrono e dos jobs asynq, que ficam persistidos em Postgres.
- A sonda SMTP é um `Prober` injetável, desligado por padrão.

**Tech Stack:** Go 1.27, okapi v0.11, GORM + Postgres, go-redis v9, asynq, emersion/go-smtp v0.25.

**Spec:** `docs/superpowers/specs/2026-09-28-email-verify-design.md`

## Global Constraints

- Branch `feat/email-verify`. Não há Go no host: todo comando Go roda em container (ver "Comandos").
- Cabeçalho SPDX em todo arquivo `.go` novo:
  `// SPDX-FileCopyrightText: 2026 Jonas Kaninda` + `// SPDX-License-Identifier: AGPL-3.0-or-later`.
- Identificadores e comentários de código em inglês, seguindo o estilo do arquivo vizinho. Documentação do usuário em
  `docs/` em português.
- Mudanças só aditivas em `POST /api/v1/emails/verify`: nenhum campo removido ou renomeado. `accept_all` e
  `suggestion` são adições.
- Veredito de negócio (e-mail inválido, suprimido etc.) sai com `200` e o campo `status`. Status HTTP de erro só
  para requisição malformada (`400`), limite excedido (`413`), taxa (`429`), recurso inexistente ou desligado
  (`404`), falha do upstream Brevo (`502`) e erro interno (`500`).
- Toda query de dados de tenant usa `repositories.ApplyScope`. Um tenant nunca lê ou altera dados de outro.
- Mensagens de commit em inglês, no estilo Conventional Commits do repositório (`feat(verify): …`). Linhas de
  atribuição: usar exatamente as do system-reminder vigente na sessão, sem copiar deste plano.
- Nenhuma dependência nova no `go.mod`.
- Limites: lote síncrono ≤ 100 e-mails; job ≤ 50.000; importação Brevo ≤ 50 páginas por chamada (100 por página);
  consultas `IN` em blocos de 5.000.

## Comandos

```bash
# Testes unitários (sem banco): os testes de repositório fazem t.Skip sozinhos
docker run --rm -v "$PWD":/src -w /src -v posta-gomod:/go/pkg/mod -v posta-gocache:/root/.cache/go-build \
  golang:1.27-alpine go test ./internal/...

# Race detector (precisa de cgo: imagem debian)
docker run --rm -v "$PWD":/src -w /src -v posta-gomod:/go/pkg/mod -v posta-gocache-deb:/root/.cache/go-build \
  golang:1.27 go test -race ./internal/services/verifier/...

# Banco de teste (uma vez por sessão)
docker network create posta-test 2>/dev/null; docker run -d --name posta-testdb --network posta-test \
  -e POSTGRES_USER=posta -e POSTGRES_PASSWORD=posta -e POSTGRES_DB=posta postgres:17-alpine
# Testes com banco
docker run --rm --network posta-test -e TEST_DATABASE_DSN="host=posta-testdb user=posta password=posta dbname=posta port=5432 sslmode=disable" \
  -v "$PWD":/src -w /src -v posta-gomod:/go/pkg/mod -v posta-gocache:/root/.cache/go-build \
  golang:1.27-alpine go test ./internal/storage/repositories/... ./internal/worker/...

# Build + vet + gofmt
docker run --rm -v "$PWD":/src -w /src -v posta-gomod:/go/pkg/mod -v posta-gocache:/root/.cache/go-build \
  golang:1.27-alpine sh -c 'go build ./... && go vet ./internal/... ./cmd/... && test -z "$(gofmt -l internal cmd)"'
```

Nos passos abaixo, `GOTEST <pkg>` abrevia o primeiro comando com o pacote indicado, e `GOTEST_DB <pkg>` abrevia o
comando com banco.

## Review Focus

1. **Vazamento entre tenants no webhook:** `X-Mailin-custom` com o UUID de um e-mail de outro workspace não pode
   gravar bounce nem marcar mensagem desse workspace (teste na Task 1).
2. **Payload `batched` da Brevo:** array de eventos, evento desconhecido e e-mail vazio no mesmo POST. Tudo `200`,
   com contagem de ignorados (teste na Task 2).
3. **DNS instável:** timeout de DNS não pode virar `invalid` nem ficar em cache (teste na Task 5).
4. **Lote com duplicatas e sintaxe inválida misturadas:** a ordem da resposta é igual à da entrada, e o rate limit
   não conta a sintaxe inválida (teste na Task 7).
5. **Job interrompido no meio (restart do worker):** a retomada processa só os itens `pending` e os contadores não
   dobram (teste na Task 9).

---

## Fase 1 — ciclo de bounce com a Brevo

### Task 1: serviço `bounceingest` + webhook genérico escopado — modelo: **opus** (escopo entre tenants)

**Files:**
- Create: `internal/services/bounceingest/ingestor.go`
- Create: `internal/services/bounceingest/ingestor_test.go`
- Modify: `internal/handlers/bounce_webhook_handler.go` (inteiro)
- Modify: `internal/routes/routes.go:373` (construção do handler)

**Interfaces:**
- Produces:
  - `bounceingest.Kind` (`KindHard|KindSoft|KindComplaint|KindUnsubscribe`)
  - `bounceingest.Event{Email, Kind, Reason, EmailUUID, Source string}`
  - `bounceingest.Outcome`
  - `bounceingest.ActionIgnored|ActionRecorded|ActionSuppressed`
  - `bounceingest.New(emails EmailFinder, bounces BounceCreator, suppressions SuppressionUpserter, subscribers SubscriberStore, messages MessageStore) *Ingestor`
  - `(*Ingestor).Record(scope repositories.ResourceScope, ev Event) Outcome`
  - `handlers.BounceRecorder` interface (usada também na Task 2)

- [ ] **Step 1: Write the failing test** (`internal/services/bounceingest/ingestor_test.go`)

```go
package bounceingest

import (
	"errors"
	"testing"

	"github.com/goposta/posta/internal/models"
	"github.com/goposta/posta/internal/storage/repositories"
)

type fakeEmails struct{ byUUID map[string]*models.Email }

func (f *fakeEmails) FindByUUID(uuid string) (*models.Email, error) {
	if e, ok := f.byUUID[uuid]; ok {
		return e, nil
	}
	return nil, errors.New("not found")
}

type fakeBounces struct{ created []models.Bounce }

func (f *fakeBounces) Create(b *models.Bounce) error { f.created = append(f.created, *b); return nil }

type fakeSuppressions struct{ upserted []models.Suppression }

func (f *fakeSuppressions) Upsert(s *models.Suppression) error {
	f.upserted = append(f.upserted, *s)
	return nil
}

type fakeSubscribers struct {
	byEmail map[string]*models.Subscriber
	updated []models.Subscriber
}

func (f *fakeSubscribers) FindByEmail(_ repositories.ResourceScope, email string) (*models.Subscriber, error) {
	if s, ok := f.byEmail[email]; ok {
		return s, nil
	}
	return nil, errors.New("not found")
}
func (f *fakeSubscribers) Update(s *models.Subscriber) error { f.updated = append(f.updated, *s); return nil }

type fakeMessages struct{ marked []uint }

func (f *fakeMessages) FindByEmailID(id uint) (*models.CampaignMessage, error) {
	return &models.CampaignMessage{ID: id * 10}, nil
}
func (f *fakeMessages) UpdateBouncedAt(id uint) error { f.marked = append(f.marked, id); return nil }

func u(v uint) *uint { return &v }

type rig struct {
	in   *Ingestor
	b    *fakeBounces
	s    *fakeSuppressions
	subs *fakeSubscribers
	m    *fakeMessages
}

func newRig() rig {
	emails := &fakeEmails{byUUID: map[string]*models.Email{
		"11111111-1111-1111-1111-111111111111": {ID: 7, UserID: 1, WorkspaceID: u(10)},
		"22222222-2222-2222-2222-222222222222": {ID: 8, UserID: 2, WorkspaceID: u(20)}, // other tenant
	}}
	r := rig{b: &fakeBounces{}, s: &fakeSuppressions{}, m: &fakeMessages{},
		subs: &fakeSubscribers{byEmail: map[string]*models.Subscriber{
			"a@b.com": {ID: 3, Email: "a@b.com", Status: models.SubscriberStatusSubscribed},
		}}}
	r.in = New(emails, r.b, r.s, r.subs, r.m)
	return r
}

var scope = repositories.ResourceScope{UserID: 1, WorkspaceID: u(10)}

func TestHardBounceWithMatchingEmail(t *testing.T) {
	r := newRig()
	out := r.in.Record(scope, Event{Email: "Foo <A@B.com>", Kind: KindHard, Reason: "550 user unknown",
		EmailUUID: "11111111-1111-1111-1111-111111111111", Source: "brevo"})

	if out.Email != "a@b.com" || !out.BounceRecorded || !out.Suppressed || !out.SubscriberUpdated || !out.MessageMarked {
		t.Fatalf("unexpected outcome: %+v", out)
	}
	if len(r.b.created) != 1 || r.b.created[0].EmailID != 7 || r.b.created[0].Type != models.BounceTypeHard {
		t.Fatalf("bounce row wrong: %+v", r.b.created)
	}
	if got := r.s.upserted[0]; got.Kind != models.SuppressionKindBounce || *got.WorkspaceID != 10 || got.Email != "a@b.com" {
		t.Fatalf("suppression wrong: %+v", got)
	}
	if r.subs.updated[0].Status != models.SubscriberStatusBounced {
		t.Fatalf("subscriber status = %s", r.subs.updated[0].Status)
	}
	if len(r.m.marked) != 1 || r.m.marked[0] != 70 {
		t.Fatalf("message not marked: %v", r.m.marked)
	}
}

func TestForeignTenantEmailUUIDIsIgnored(t *testing.T) {
	r := newRig()
	out := r.in.Record(scope, Event{Email: "a@b.com", Kind: KindHard,
		EmailUUID: "22222222-2222-2222-2222-222222222222", Source: "brevo"})
	if out.BounceRecorded || out.MessageMarked || len(r.b.created) != 0 || len(r.m.marked) != 0 {
		t.Fatalf("foreign email must not be touched: %+v", out)
	}
	if !out.Suppressed || *r.s.upserted[0].WorkspaceID != 10 {
		t.Fatalf("suppression must still land in caller scope: %+v", r.s.upserted)
	}
}

func TestSoftBounceDoesNotSuppress(t *testing.T) {
	r := newRig()
	out := r.in.Record(scope, Event{Email: "a@b.com", Kind: KindSoft,
		EmailUUID: "11111111-1111-1111-1111-111111111111"})
	if out.Suppressed || out.SubscriberUpdated || len(r.s.upserted) != 0 {
		t.Fatalf("soft bounce must not suppress: %+v", out)
	}
	if !out.BounceRecorded || r.b.created[0].Type != models.BounceTypeSoft {
		t.Fatalf("soft bounce must be recorded: %+v", r.b.created)
	}
}

func TestComplaintAndUnsubscribe(t *testing.T) {
	r := newRig()
	r.in.Record(scope, Event{Email: "a@b.com", Kind: KindComplaint})
	if r.s.upserted[0].Kind != models.SuppressionKindComplaint || r.subs.updated[0].Status != models.SubscriberStatusComplained {
		t.Fatalf("complaint mapping wrong: %+v %+v", r.s.upserted, r.subs.updated)
	}
	r2 := newRig()
	out := r2.in.Record(scope, Event{Email: "a@b.com", Kind: KindUnsubscribe,
		EmailUUID: "11111111-1111-1111-1111-111111111111"})
	if out.BounceRecorded || r2.s.upserted[0].Kind != models.SuppressionKindHard ||
		r2.subs.updated[0].Status != models.SubscriberStatusUnsubscribed {
		t.Fatalf("unsubscribe mapping wrong: %+v", out)
	}
}

func TestEmptyEmailIsIgnored(t *testing.T) {
	r := newRig()
	if out := r.in.Record(scope, Event{Email: "  ", Kind: KindHard}); out.Action != ActionIgnored {
		t.Fatalf("action = %q", out.Action)
	}
}
```

- [ ] **Step 2: Run and see it fail.** `GOTEST ./internal/services/bounceingest/` → FAIL (`undefined: New`).

- [ ] **Step 3: Implement** (`internal/services/bounceingest/ingestor.go`)

```go
// Package bounceingest records delivery failures reported after the fact
// (provider webhooks), so every source updates bounces, suppressions,
// subscribers and campaign messages the same way and inside the caller's scope.
package bounceingest

import (
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/goposta/posta/internal/metrics"
	"github.com/goposta/posta/internal/models"
	"github.com/goposta/posta/internal/storage/repositories"
)

type Kind string

const (
	KindHard        Kind = "hard"
	KindSoft        Kind = "soft"
	KindComplaint   Kind = "complaint"
	KindUnsubscribe Kind = "unsubscribe"
)

const (
	ActionIgnored    = "ignored"
	ActionRecorded   = "recorded"
	ActionSuppressed = "suppressed"
)

// Event is one provider-reported failure. EmailUUID is optional correlation
// to a Posta email; it is honoured only when that email belongs to the scope.
type Event struct {
	Email     string
	Kind      Kind
	Reason    string
	EmailUUID string
	Source    string
}

type Outcome struct {
	Email             string `json:"email"`
	Kind              Kind   `json:"kind"`
	Action            string `json:"action"`
	BounceRecorded    bool   `json:"bounce_recorded"`
	Suppressed        bool   `json:"suppressed"`
	SubscriberUpdated bool   `json:"subscriber_updated"`
	MessageMarked     bool   `json:"message_marked"`
}

type EmailFinder interface {
	FindByUUID(uuid string) (*models.Email, error)
}
type BounceCreator interface{ Create(*models.Bounce) error }
type SuppressionUpserter interface{ Upsert(*models.Suppression) error }
type SubscriberStore interface {
	FindByEmail(scope repositories.ResourceScope, email string) (*models.Subscriber, error)
	Update(*models.Subscriber) error
}
type MessageStore interface {
	FindByEmailID(emailID uint) (*models.CampaignMessage, error)
	UpdateBouncedAt(id uint) error
}

type Ingestor struct {
	emails       EmailFinder
	bounces      BounceCreator
	suppressions SuppressionUpserter
	subscribers  SubscriberStore
	messages     MessageStore
}

func New(emails EmailFinder, bounces BounceCreator, suppressions SuppressionUpserter, subscribers SubscriberStore, messages MessageStore) *Ingestor {
	return &Ingestor{emails: emails, bounces: bounces, suppressions: suppressions, subscribers: subscribers, messages: messages}
}

func (in *Ingestor) Record(scope repositories.ResourceScope, ev Event) Outcome {
	email := normalize(ev.Email)
	out := Outcome{Email: email, Kind: ev.Kind, Action: ActionIgnored}
	if email == "" {
		return out
	}

	em := in.ownedEmail(scope, ev.EmailUUID)

	if em != nil && ev.Kind != KindUnsubscribe {
		bt := bounceType(ev.Kind)
		if err := in.bounces.Create(&models.Bounce{
			UserID: em.UserID, WorkspaceID: em.WorkspaceID, EmailID: em.ID,
			Recipient: email, Type: bt, Reason: ev.Reason,
		}); err == nil {
			out.BounceRecorded = true
			metrics.IncrementBounce(string(bt))
		}
		if ev.Kind == KindHard || ev.Kind == KindSoft {
			if msg, err := in.messages.FindByEmailID(em.ID); err == nil && msg != nil {
				out.MessageMarked = in.messages.UpdateBouncedAt(msg.ID) == nil
			}
		}
	}

	if kind, ok := suppressionKind(ev.Kind); ok {
		if err := in.suppressions.Upsert(&models.Suppression{
			UserID: scope.UserID, WorkspaceID: scope.WorkspaceID, Email: email, Kind: kind,
			Reason: fmt.Sprintf("auto-suppressed (%s): %s", sourceOr(ev.Source), reasonOr(ev)),
		}); err == nil {
			out.Suppressed = true
			metrics.IncrementSuppression()
		}
	}

	if status, ok := subscriberStatus(ev.Kind); ok {
		if sub, err := in.subscribers.FindByEmail(scope, email); err == nil && sub != nil && sub.Status != status {
			now := time.Now()
			sub.Status = status
			sub.UpdatedAt = &now
			out.SubscriberUpdated = in.subscribers.Update(sub) == nil
		}
	}

	switch {
	case out.Suppressed:
		out.Action = ActionSuppressed
	case out.BounceRecorded || out.SubscriberUpdated || out.MessageMarked:
		out.Action = ActionRecorded
	}
	return out
}

// ownedEmail resolves the correlated email only when it belongs to scope.
func (in *Ingestor) ownedEmail(scope repositories.ResourceScope, uuid string) *models.Email {
	if uuid == "" || in.emails == nil {
		return nil
	}
	em, err := in.emails.FindByUUID(uuid)
	if err != nil || em == nil {
		return nil
	}
	if scope.WorkspaceID != nil {
		if em.WorkspaceID == nil || *em.WorkspaceID != *scope.WorkspaceID {
			return nil
		}
		return em
	}
	if em.WorkspaceID != nil || em.UserID != scope.UserID {
		return nil
	}
	return em
}

func bounceType(k Kind) models.BounceType {
	switch k {
	case KindSoft:
		return models.BounceTypeSoft
	case KindComplaint:
		return models.BounceTypeComplaint
	default:
		return models.BounceTypeHard
	}
}

func suppressionKind(k Kind) (models.SuppressionKind, bool) {
	switch k {
	case KindHard:
		return models.SuppressionKindBounce, true
	case KindComplaint:
		return models.SuppressionKindComplaint, true
	case KindUnsubscribe:
		return models.SuppressionKindHard, true
	}
	return "", false
}

func subscriberStatus(k Kind) (models.SubscriberStatus, bool) {
	switch k {
	case KindHard:
		return models.SubscriberStatusBounced, true
	case KindComplaint:
		return models.SubscriberStatusComplained, true
	case KindUnsubscribe:
		return models.SubscriberStatusUnsubscribed, true
	}
	return "", false
}

func normalize(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if addr, err := mail.ParseAddress(raw); err == nil {
		return strings.ToLower(addr.Address)
	}
	return strings.ToLower(raw)
}

func sourceOr(s string) string {
	if s == "" {
		return "webhook"
	}
	return s
}

func reasonOr(ev Event) string {
	if ev.Reason != "" {
		return ev.Reason
	}
	return string(ev.Kind)
}
```

  Confirme que `metrics.IncrementBounce(string)` e `metrics.IncrementSuppression()` existem com essas assinaturas
  (`grep -n "func Increment" internal/metrics/*.go`) e que `models.Subscriber.UpdatedAt` é `*time.Time`. Se algo
  diferir, ajuste a chamada, nunca a intenção.

- [ ] **Step 4: Run and see it pass.** `GOTEST ./internal/services/bounceingest/` → PASS.

- [ ] **Step 5: Refactor the generic webhook** (`internal/handlers/bounce_webhook_handler.go`). Troque o corpo do
  handler (mantenha `BounceNotification` e `BounceResponse` sem alteração de campos):

```go
// BounceRecorder is the subset of bounceingest.Ingestor the webhook handlers use.
type BounceRecorder interface {
	Record(scope repositories.ResourceScope, ev bounceingest.Event) bounceingest.Outcome
}

// BounceWebhookHandler processes inbound bounce notifications.
type BounceWebhookHandler struct {
	recorder BounceRecorder
}

func NewBounceWebhookHandler(recorder BounceRecorder) *BounceWebhookHandler {
	return &BounceWebhookHandler{recorder: recorder}
}

func (h *BounceWebhookHandler) HandleBounce(c *okapi.Context, req *BounceNotification) error {
	if strings.TrimSpace(req.Body.Email) == "" {
		return c.AbortBadRequest("email is required")
	}
	kind := bounceingest.KindHard
	if req.Body.Type == "soft" {
		kind = bounceingest.KindSoft
	}
	out := h.recorder.Record(getScope(c), bounceingest.Event{
		Email: req.Body.Email, Kind: kind, Reason: req.Body.Reason,
		EmailUUID: req.Body.EmailUUID, Source: "webhook",
	})

	// Keep the legacy action vocabulary: consumers already switch on it.
	action := "recorded"
	switch {
	case out.MessageMarked:
		action = "message_bounced"
	case out.SubscriberUpdated:
		action = "subscriber_bounced"
	}
	logger.Info("bounce processed", "email", out.Email, "type", kind, "action", action)
	return ok(c, BounceResponse{Processed: true, Action: action})
}
```

  Em `internal/routes/routes.go:373`:

```go
	bounceIngestor := bounceingest.New(emailRepo, bounceRepo, suppressionRepo, subscriberRepo, campaignMessageRepo)
	r.h.bounceWebhook = handlers.NewBounceWebhookHandler(bounceIngestor)
```

  Guarde `bounceIngestor` num campo ou variável acessível, porque a Task 2 reutiliza. Confirme por `grep` que
  `bounceRepo`, `suppressionRepo`, `subscriberRepo`, `campaignMessageRepo` e `emailRepo` estão no escopo nesse
  ponto de `routes.go`, e mova a linha se necessário.

- [ ] **Step 6: Build + tests.** Comando "Build + vet + gofmt" e `GOTEST ./internal/...` → PASS.

- [ ] **Step 7: Commit** — `feat(bounce): shared scoped ingestor; generic webhook now records bounces and suppressions`

---

### Task 2: webhook Brevo + header `X-Mailin-custom` — modelo: **sonnet**

**Files:**
- Create: `internal/handlers/brevo_webhook_handler.go`
- Create: `internal/handlers/brevo_webhook_handler_test.go`
- Modify: `internal/services/email/stamper.go` (`StampCampaign`, `StampTransactional`)
- Create: `internal/services/email/stamper_test.go`
- Modify: `internal/routes/tracking_routes.go` (nova rota ao lado de `bounceWebhookRoutes`)
- Modify: `internal/routes/routes.go` (campo `brevoWebhook` no struct de handlers + construção com o
  `bounceIngestor` da Task 1)

**Interfaces:**
- Consumes: `handlers.BounceRecorder`, `bounceingest.Event/Outcome/Kind*/Action*` (Task 1)
- Produces:
  - `POST /api/v1/webhooks/brevo`
  - `handlers.NewBrevoWebhookHandler(r BounceRecorder) *BrevoWebhookHandler`
  - `handlers.BrevoWebhookResponse`
  - header `X-Mailin-custom: posta-id=<uuid>`

- [ ] **Step 1: Failing tests** (`internal/handlers/brevo_webhook_handler_test.go`)

```go
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
```

  E `internal/services/email/stamper_test.go`:

```go
package email

import (
	"testing"

	"github.com/goposta/posta/internal/models"
)

func TestStampersSetBrevoCorrelationHeader(t *testing.T) {
	s := NewStamper("Posta", "test", nil)
	em := &models.Email{UUID: "11111111-1111-1111-1111-111111111111"}

	h := map[string]string{}
	s.StampTransactional(h, em)
	if h["X-Mailin-custom"] != "posta-id=11111111-1111-1111-1111-111111111111" {
		t.Fatalf("transactional header = %q", h["X-Mailin-custom"])
	}

	h = map[string]string{}
	s.StampCampaign(h, em, 1, 2, false, false)
	if h["X-Mailin-custom"] != "posta-id=11111111-1111-1111-1111-111111111111" {
		t.Fatalf("campaign header = %q", h["X-Mailin-custom"])
	}

	h = map[string]string{"X-Mailin-custom": "caller-value"}
	s.StampTransactional(h, em)
	if h["X-Mailin-custom"] != "caller-value" {
		t.Fatalf("caller-supplied header must win, got %q", h["X-Mailin-custom"])
	}
}
```

- [ ] **Step 2: Run and see it fail.** `GOTEST ./internal/handlers/ ./internal/services/email/` → FAIL.

- [ ] **Step 3: Implement the handler** (`internal/handlers/brevo_webhook_handler.go`)

```go
package handlers

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"github.com/goposta/posta/internal/services/bounceingest"
	"github.com/google/uuid"
	"github.com/jkaninda/logger"
	"github.com/jkaninda/okapi"
)

const brevoWebhookMaxBody = 5 << 20

// BrevoWebhookHandler ingests Brevo transactional/marketing webhook events.
// Brevo accepts every recipient at RCPT time when used as a relay, so these
// events are the only way Posta learns about its bounces.
type BrevoWebhookHandler struct {
	recorder BounceRecorder
}

func NewBrevoWebhookHandler(recorder BounceRecorder) *BrevoWebhookHandler {
	return &BrevoWebhookHandler{recorder: recorder}
}

type brevoEvent struct {
	Event        string `json:"event"`
	Email        string `json:"email"`
	Reason       string `json:"reason"`
	MailinCustom string `json:"X-Mailin-custom"`
	MessageID    string `json:"message-id"`
}

// BrevoWebhookResponse summarises what was done with each event.
type BrevoWebhookResponse struct {
	Received  int                    `json:"received"`
	Processed int                    `json:"processed"`
	Ignored   int                    `json:"ignored"`
	Items     []bounceingest.Outcome `json:"items"`
}

// Handle accepts a single event object or, in Brevo's batched mode, an array.
// Any well-formed payload is answered with 200 so Brevo does not redeliver.
func (h *BrevoWebhookHandler) Handle(c *okapi.Context) error {
	raw, err := io.ReadAll(io.LimitReader(c.Request().Body, brevoWebhookMaxBody))
	if err != nil {
		return c.AbortBadRequest("failed to read webhook body")
	}
	events, err := parseBrevoPayload(raw)
	if err != nil {
		return c.AbortBadRequest("invalid Brevo webhook payload")
	}

	scope := getScope(c)
	resp := BrevoWebhookResponse{Received: len(events), Items: make([]bounceingest.Outcome, 0, len(events))}
	for _, ev := range events {
		kind, known := brevoKind(ev.Event)
		if !known || strings.TrimSpace(ev.Email) == "" {
			resp.Ignored++
			resp.Items = append(resp.Items, bounceingest.Outcome{
				Email: strings.ToLower(strings.TrimSpace(ev.Email)), Action: bounceingest.ActionIgnored,
			})
			continue
		}
		out := h.recorder.Record(scope, bounceingest.Event{
			Email: ev.Email, Kind: kind, Reason: ev.Reason,
			EmailUUID: postaIDFromCustom(ev.MailinCustom), Source: "brevo",
		})
		resp.Processed++
		resp.Items = append(resp.Items, out)
	}
	logger.Info("brevo webhook processed", "received", resp.Received, "processed", resp.Processed, "ignored", resp.Ignored)
	return ok(c, resp)
}

func parseBrevoPayload(raw []byte) ([]brevoEvent, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var list []brevoEvent
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return nil, err
		}
		return list, nil
	}
	var one brevoEvent
	if err := json.Unmarshal(trimmed, &one); err != nil {
		return nil, err
	}
	return []brevoEvent{one}, nil
}

func brevoKind(event string) (bounceingest.Kind, bool) {
	switch strings.ToLower(strings.TrimSpace(event)) {
	case "hard_bounce", "hardbounce", "invalid_email", "invalid", "blocked":
		return bounceingest.KindHard, true
	case "soft_bounce", "softbounce":
		return bounceingest.KindSoft, true
	case "spam", "complaint":
		return bounceingest.KindComplaint, true
	case "unsubscribed", "unsubscribe":
		return bounceingest.KindUnsubscribe, true
	}
	return "", false
}

// postaIDFromCustom extracts the email UUID Posta stamps as
// "X-Mailin-custom: posta-id=<uuid>"; other key=value pairs are tolerated.
func postaIDFromCustom(v string) string {
	for _, part := range strings.Split(v, ";") {
		k, val, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found || strings.TrimSpace(k) != "posta-id" {
			continue
		}
		val = strings.TrimSpace(val)
		if _, err := uuid.Parse(val); err == nil {
			return val
		}
	}
	return ""
}
```

- [ ] **Step 4: Stamp the header** (`internal/services/email/stamper.go`). Adicione no fim de `StampCampaign` e de
  `StampTransactional` a chamada `s.correlate(headers, em)` e a função:

```go
// correlate stamps the Brevo custom header echoed back in its webhooks, so a
// bounce can be tied to this email. A caller-supplied value is left alone.
func (s *Stamper) correlate(headers map[string]string, em *models.Email) {
	if em == nil || em.UUID == "" {
		return
	}
	if _, exists := headers["X-Mailin-custom"]; exists {
		return
	}
	headers["X-Mailin-custom"] = "posta-id=" + em.UUID
}
```

  Verifique em `internal/worker/handler.go:270-290` se o map `headers` já contém os headers definidos pelo usuário
  **antes** da chamada ao stamper. Se os headers do usuário forem aplicados depois, a regra "o valor do chamador
  vence" já vale naturalmente. Registre no commit qual dos dois casos ocorre.

- [ ] **Step 5: Route** (`internal/routes/tracking_routes.go`, dentro de `bounceWebhookRoutes`, reaproveitando o
  mesmo middleware de API key):

```go
	brevoGroup := r.v1.Group("/webhooks/brevo", r.mw.apiKey).WithTagInfo(okapi.GroupTag{
		Name:        "Webhooks",
		Description: "Inbound webhook endpoints that receive bounce and complaint notifications from upstream mail providers. Authenticated with an API key.",
	}).WithSecurity([]map[string][]string{{"ApiKeyAuth": {}}})
	// …append to the returned slice:
		{
			Method:      http.MethodPost,
			Path:        "",
			Handler:     r.h.brevoWebhook.Handle,
			Group:       brevoGroup,
			Summary:     "Brevo webhook (bounces, complaints, unsubscribes)",
			Description: "Receives Brevo transactional or marketing webhook events, single or batched. Hard bounces, invalid and blocked recipients are suppressed; spam complaints are suppressed as complaints; soft bounces are only recorded. Configure the webhook in Brevo with an Authorization header carrying a Posta API key.",
			Response:    &dto.Response[handlers.BrevoWebhookResponse]{},
		},
```

- [ ] **Step 6: Run.** `GOTEST ./internal/handlers/ ./internal/services/email/` e o build → PASS.

- [ ] **Step 7: Commit** — `feat(webhooks): ingest Brevo bounce/complaint events and stamp X-Mailin-custom correlation`

---

### Task 3: importar bloqueados da Brevo — modelo: **sonnet**

**Files:**
- Create: `internal/services/brevo/client.go`
- Create: `internal/services/brevo/client_test.go`
- Create: `internal/handlers/brevo_import_handler.go`
- Create: `internal/handlers/brevo_import_handler_test.go`
- Modify: `internal/routes/workspace_resource_routes.go` (rota ao lado de `pathSuppressions`, grupo `opsGroup`)
- Modify: `internal/routes/routes.go` (campo `brevoImport` + construção)

**Interfaces:**
- Produces:
  - `brevo.NewClient() *Client` (`Client{BaseURL string; HTTP *http.Client}`)
  - `(*Client).BlockedContacts(ctx, apiKey string, q BlockedQuery) (*BlockedPage, error)`
  - `brevo.ErrUnauthorized`
  - `POST /api/v1/workspaces/current/suppressions/import/brevo`

- [ ] **Step 1: Failing client test** (`internal/services/brevo/client_test.go`)

```go
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
```

- [ ] **Step 2: Run and see it fail.** `GOTEST ./internal/services/brevo/` → FAIL.

- [ ] **Step 3: Implement** (`internal/services/brevo/client.go`)

```go
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
```

- [ ] **Step 4: Failing handler test** (`internal/handlers/brevo_import_handler_test.go`). Testa a função pura
  de importação, que depende das interfaces `blockedSource` e `suppressionUpserter`:

```go
package handlers

import (
	"context"
	"fmt"
	"testing"

	"github.com/goposta/posta/internal/models"
	"github.com/goposta/posta/internal/services/brevo"
	"github.com/goposta/posta/internal/storage/repositories"
)

type fakeBlocked struct{ total int }

func (f *fakeBlocked) BlockedContacts(_ context.Context, _ string, q brevo.BlockedQuery) (*brevo.BlockedPage, error) {
	page := &brevo.BlockedPage{Count: int64(f.total)}
	for i := q.Offset; i < f.total && i < q.Offset+q.Limit; i++ {
		code := "hardBounce"
		switch i % 3 {
		case 1:
			code = "contactFlaggedAsSpam"
		case 2:
			code = "unsubscribedViaEmail"
		}
		page.Contacts = append(page.Contacts, brevo.BlockedContact{
			Email: fmt.Sprintf("user%d@x.com", i), Reason: brevo.BlockedReason{Code: code},
		})
	}
	return page, nil
}

type captureUpserts struct{ rows []models.Suppression }

func (c *captureUpserts) Upsert(s *models.Suppression) error { c.rows = append(c.rows, *s); return nil }

func TestImportBrevoBlockedPagesAndMapsKinds(t *testing.T) {
	src := &fakeBlocked{total: 250}
	sink := &captureUpserts{}
	ws := uint(10)
	scope := repositories.ResourceScope{UserID: 1, WorkspaceID: &ws}

	res, err := importBrevoBlocked(context.Background(), src, sink, scope, brevoImportParams{APIKey: "k", Offset: 0, MaxPages: 2})
	if err != nil {
		t.Fatal(err)
	}
	if res.Fetched != 200 || res.Imported != 200 || res.NextOffset == nil || *res.NextOffset != 200 || res.Total != 250 {
		t.Fatalf("first call = %+v", res)
	}
	if sink.rows[0].Kind != models.SuppressionKindBounce || sink.rows[1].Kind != models.SuppressionKindComplaint ||
		sink.rows[2].Kind != models.SuppressionKindHard || *sink.rows[0].WorkspaceID != 10 {
		t.Fatalf("kind mapping wrong: %+v", sink.rows[:3])
	}

	res, err = importBrevoBlocked(context.Background(), src, sink, scope, brevoImportParams{APIKey: "k", Offset: 200, MaxPages: 2})
	if err != nil || res.Fetched != 50 || res.NextOffset != nil {
		t.Fatalf("second call = %+v err=%v", res, err)
	}
}
```

- [ ] **Step 5: Implement the handler** (`internal/handlers/brevo_import_handler.go`)

```go
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
```

  Confirme a assinatura de `AbortBadGateway` e `AbortForbidden` na fonte do okapi
  (`/go/pkg/mod/github.com/jkaninda/okapi@v0.11.0`, via volume `posta-gomod`) e ajuste os argumentos se preciso.

- [ ] **Step 6: Route** (`opsGroup`, perto de `pathSuppressions` em `workspace_resource_routes.go`):

```go
		{
			Method:      http.MethodPost,
			Path:        pathSuppressions + "/import/brevo",
			Handler:     okapi.H(r.h.brevoImport.Import),
			Group:       opsGroup,
			Tags:        []string{tagUser},
			Summary:     "Import Brevo blocked contacts into the suppression list",
			Description: "Pages Brevo's blocked/unsubscribed transactional contacts (100 per page, up to max_pages per call) and upserts them as suppressions. Call again with next_offset until it is null. The Brevo API key is not stored.",
			Request:     &handlers.ImportBrevoRequest{},
			Response:    &dto.Response[handlers.ImportBrevoResponse]{},
			Options: []okapi.RouteOption{
				okapi.DocErrorResponse(400, &dto.ErrorResponseBody{}),
				okapi.DocErrorResponse(502, &dto.ErrorResponseBody{}),
			},
		},
```

  Em `routes.go`: `brevoImport: handlers.NewBrevoImportHandler(brevo.NewClient(), suppressionRepo)`.

- [ ] **Step 7: Run and commit.** `GOTEST ./internal/services/brevo/ ./internal/handlers/` + build → PASS.
  Commit: `feat(suppressions): import Brevo blocked contacts`

---

### Task 4: campanhas respeitam supressão global — modelo: **sonnet**

**Files:**
- Modify: `internal/storage/repositories/suppression_repo.go` (novo `SuppressedSet`)
- Create: `internal/storage/repositories/suppression_set_test.go`
- Modify: `internal/worker/campaign_processor.go` (campo + setter + `eligibleMessages`)
- Create: `internal/worker/campaign_eligibility_test.go`
- Modify: `cmd/posta/worker.go` e `cmd/posta/server.go` (chamar `SetSuppressionRepo` nos dois lugares onde
  `NewCampaignProcessor` é construído)

**Interfaces:**
- Produces: `(*SuppressionRepository).SuppressedSet(scope ResourceScope, emails []string) (map[string]struct{}, error)`,
  que considera só as supressões globais (`list_id IS NULL`), normaliza os endereços e consulta em blocos de 5.000.
  A Task 7 também usa este método. Também produz a constante `suppressionLookupChunk = 5000`.

- [ ] **Step 1: Failing tests.** Repositório (`suppression_set_test.go`, usa `testDB` e `createUser` do pacote):

```go
package repositories

import (
	"testing"

	"github.com/goposta/posta/internal/models"
)

func TestSuppressedSetGlobalOnlyAndScoped(t *testing.T) {
	db := testDB(t)
	if err := db.AutoMigrate(&models.Suppression{}); err != nil {
		t.Skipf("skipping: %v", err)
	}
	tx := db.Begin()
	defer tx.Rollback()
	uid := createUser(t, tx, "supset@example.com")
	ws, other := uint(9001), uint(9002)
	list := uint(5)
	repo := NewSuppressionRepository(tx)
	for _, s := range []models.Suppression{
		{UserID: uid, WorkspaceID: &ws, Email: "global@x.com", Kind: models.SuppressionKindBounce},
		{UserID: uid, WorkspaceID: &ws, Email: "listonly@x.com", ListID: &list, Kind: models.SuppressionKindListUnsubscribe},
		{UserID: uid, WorkspaceID: &other, Email: "otherws@x.com", Kind: models.SuppressionKindBounce},
	} {
		s := s
		if err := repo.Create(&s); err != nil {
			t.Fatal(err)
		}
	}
	set, err := repo.SuppressedSet(ResourceScope{UserID: uid, WorkspaceID: &ws},
		[]string{"GLOBAL@x.com", "listonly@x.com", "otherws@x.com", "clean@x.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := set["global@x.com"]; !ok || len(set) != 1 {
		t.Fatalf("set = %v, want only global@x.com", set)
	}
}
```

  Worker (`campaign_eligibility_test.go`, sem banco):

```go
package worker

import (
	"testing"

	"github.com/goposta/posta/internal/models"
)

func TestEligibleMessagesSkipsStatusOptOutAndGlobalSuppression(t *testing.T) {
	subs := []models.Subscriber{
		{ID: 1, Email: "ok@x.com", Status: models.SubscriberStatusSubscribed},
		{ID: 2, Email: "bounced@x.com", Status: models.SubscriberStatusBounced},
		{ID: 3, Email: "optout@x.com", Status: models.SubscriberStatusSubscribed},
		{ID: 4, Email: "Suppressed@X.com", Status: models.SubscriberStatusSubscribed},
	}
	msgs := eligibleMessages(42, subs,
		map[uint]struct{}{3: {}},
		map[string]struct{}{"suppressed@x.com": {}})
	if len(msgs) != 1 || msgs[0].SubscriberID != 1 || msgs[0].CampaignID != 42 || msgs[0].Status != models.CampaignMsgPending {
		t.Fatalf("msgs = %+v", msgs)
	}
}
```

- [ ] **Step 2: Run and see them fail.** `GOTEST ./internal/worker/` → FAIL. `GOTEST_DB ./internal/storage/repositories/`
  → FAIL.

- [ ] **Step 3: Implement `SuppressedSet`:**

```go
// suppressionLookupChunk bounds IN (...) lists well below Postgres' 65535
// bind-parameter limit.
const suppressionLookupChunk = 5000

// SuppressedSet returns which of emails carry a global suppression (list_id IS
// NULL) in scope, keyed by normalized address.
func (r *SuppressionRepository) SuppressedSet(scope ResourceScope, emails []string) (map[string]struct{}, error) {
	out := make(map[string]struct{})
	norm := make([]string, 0, len(emails))
	for _, e := range emails {
		if n := normalizeEmail(e); n != "" {
			norm = append(norm, n)
		}
	}
	for start := 0; start < len(norm); start += suppressionLookupChunk {
		end := min(start+suppressionLookupChunk, len(norm))
		var hits []string
		if err := applyListPredicate(ApplyScope(r.db.Model(&models.Suppression{}), scope), nil).
			Where("email IN ?", norm[start:end]).
			Pluck("email", &hits).Error; err != nil {
			return nil, err
		}
		for _, h := range hits {
			out[h] = struct{}{}
		}
	}
	return out, nil
}
```

- [ ] **Step 4: Campaign.** Adicione o campo `suppressionRepo *repositories.SuppressionRepository` e
  `func (p *CampaignProcessor) SetSuppressionRepo(r *repositories.SuppressionRepository) { p.suppressionRepo = r }`.
  Extraia o laço de montagem (hoje em `campaign_processor.go:122-137`) para:

```go
// eligibleMessages builds pending messages for subscribers that are
// subscribed, have not opted out of this list and carry no global suppression.
func eligibleMessages(campaignID uint, subs []models.Subscriber, listOptOut map[uint]struct{}, globalSuppressed map[string]struct{}) []models.CampaignMessage {
	messages := make([]models.CampaignMessage, 0, len(subs))
	for _, sub := range subs {
		if sub.Status != models.SubscriberStatusSubscribed {
			continue
		}
		if _, opt := listOptOut[sub.ID]; opt {
			continue
		}
		if _, blocked := globalSuppressed[strings.ToLower(strings.TrimSpace(sub.Email))]; blocked {
			continue
		}
		messages = append(messages, models.CampaignMessage{
			CampaignID:   campaignID,
			SubscriberID: sub.ID,
			Status:       models.CampaignMsgPending,
		})
	}
	return messages
}
```

  Em `HandleCampaignStart`, antes de montar as mensagens:

```go
	var globalSuppressed map[string]struct{}
	if p.suppressionRepo != nil {
		emails := make([]string, 0, len(subscribers))
		for _, sub := range subscribers {
			emails = append(emails, sub.Email)
		}
		// Fail closed: sending to a suppressed address burns sender reputation,
		// so a lookup failure retries the task instead of sending blind.
		globalSuppressed, err = p.suppressionRepo.SuppressedSet(scope, emails)
		if err != nil {
			return fmt.Errorf("failed to load global suppressions: %w", err)
		}
	}
	messages := eligibleMessages(campaign.ID, subscribers, suppressed, globalSuppressed)
```

  Verifique o nome da variável de escopo já usada em `HandleCampaignStart` (é passada para `FindByFilterRules`) e
  use a mesma. Chame `campaignProcessor.SetSuppressionRepo(repositories.NewSuppressionRepository(db))` em
  `cmd/posta/worker.go` e `cmd/posta/server.go`.

- [ ] **Step 5: Varredura dos caminhos de envio.** Rode
  `grep -rn "emailRepo.Create(\|EnqueueEmailSend(" internal --include='*.go' | grep -v _test` e confirme, para
  cada ocorrência, que o destinatário passou por filtro de supressão:
  - `email.Service` filtra em `service.go:474-480` e `:575-581`;
  - campanha filtra após esta task;
  - `retry/worker.go` reenfileira e-mails já filtrados.

  Liste o resultado no corpo do commit. Qualquer caminho sem filtro vira item para o controlador, não correção
  silenciosa.

- [ ] **Step 6: Run and commit.** `GOTEST ./internal/worker/`, `GOTEST_DB ./internal/storage/repositories/` e o
  build → PASS. Commit: `fix(campaigns): skip globally suppressed recipients at campaign start`

---

## Fase 2 — validador correto

### Task 5: DNS com 3 resultados, null MX e `decide` extensível — modelo: **sonnet**

**Files:**
- Modify: `internal/services/verifier/verifier.go`
- Modify: `internal/services/verifier/verifier_test.go`
- Create: `internal/services/verifier/dns_test.go`

**Interfaces:**
- Produces:
  - `verifier.Resolver` interface (`LookupMX`, `LookupHost` com as assinaturas de `*net.Resolver`)
  - `(*Service).SetResolver(Resolver)`
  - `type SMTPVerdict string` com constantes `SMTPSkipped|SMTPDeliverable|SMTPUndeliverable|SMTPAcceptAll|SMTPUnknown`
  - `StatusAcceptAll Status = "accept_all"`
  - `type dnsOutcome int` (`dnsOK|dnsNoMail|dnsTempErr`)
  - `type verdictInput struct{ SyntaxOK, Disposable, Role bool; DNS dnsOutcome; NullMX bool; SMTP SMTPVerdict }`
  - `decide(in verdictInput) (Status, int, string)`
  - `(*Service).mxHosts(ctx, domain) ([]string, dnsOutcome, bool /*nullMX*/)`
  - `shouldCache(r *Result) bool`
  - `(*Service).compute(ctx, email) *Result`, mantido

- [ ] **Step 1: Failing tests** (`dns_test.go`):

```go
package verifier

import (
	"context"
	"net"
	"testing"
)

type fakeResolver struct {
	mx      map[string][]*net.MX
	mxErr   map[string]error
	host    map[string][]string
	hostErr map[string]error
}

func (f *fakeResolver) LookupMX(_ context.Context, d string) ([]*net.MX, error) {
	return f.mx[d], f.mxErr[d]
}
func (f *fakeResolver) LookupHost(_ context.Context, d string) ([]string, error) {
	return f.host[d], f.hostErr[d]
}

var errNotFound = &net.DNSError{Err: "no such host", Name: "x", IsNotFound: true}
var errTimeout = &net.DNSError{Err: "i/o timeout", Name: "x", IsTimeout: true, IsTemporary: true}

func newTestService(r Resolver) *Service {
	s := NewService(nil, nil, nil, Options{Enabled: true})
	s.SetResolver(r)
	return s
}

func TestNullMXIsInvalid(t *testing.T) {
	s := newTestService(&fakeResolver{mx: map[string][]*net.MX{"example.com": {{Host: ".", Pref: 0}}}})
	r := s.compute(context.Background(), "a@example.com")
	if r.Status != StatusInvalid || r.Checks.MX || r.Reason != "domain does not accept mail (null MX)" {
		t.Fatalf("got %+v", r)
	}
}

func TestDNSTimeoutIsUnknownAndNotCached(t *testing.T) {
	s := newTestService(&fakeResolver{mxErr: map[string]error{"slow.com": errTimeout}})
	r := s.compute(context.Background(), "a@slow.com")
	if r.Status != StatusUnknown {
		t.Fatalf("status = %s, want unknown", r.Status)
	}
	if shouldCache(r) {
		t.Fatal("unknown results must not be cached")
	}
}

func TestNXDomainIsInvalid(t *testing.T) {
	s := newTestService(&fakeResolver{
		mxErr:   map[string]error{"nope.invalid": errNotFound},
		hostErr: map[string]error{"nope.invalid": errNotFound},
	})
	if r := s.compute(context.Background(), "a@nope.invalid"); r.Status != StatusInvalid {
		t.Fatalf("status = %s", r.Status)
	}
}

func TestARecordFallbackIsValid(t *testing.T) {
	s := newTestService(&fakeResolver{
		mxErr: map[string]error{"a-only.com": errNotFound},
		host:  map[string][]string{"a-only.com": {"192.0.2.1"}},
	})
	if r := s.compute(context.Background(), "a@a-only.com"); r.Status != StatusValid || !r.Checks.MX {
		t.Fatalf("got %+v", r)
	}
}

func TestHostTimeoutAfterMXNotFoundIsUnknown(t *testing.T) {
	s := newTestService(&fakeResolver{
		mxErr:   map[string]error{"flaky.com": errNotFound},
		hostErr: map[string]error{"flaky.com": errTimeout},
	})
	if r := s.compute(context.Background(), "a@flaky.com"); r.Status != StatusUnknown {
		t.Fatalf("status = %s", r.Status)
	}
}
```

  Substitua `TestDecide` em `verifier_test.go` por uma tabela sobre `verdictInput`:

```go
func TestDecide(t *testing.T) {
	ok := verdictInput{SyntaxOK: true, DNS: dnsOK, SMTP: SMTPSkipped}
	with := func(f func(*verdictInput)) verdictInput { v := ok; f(&v); return v }
	cases := []struct {
		name       string
		in         verdictInput
		wantStatus Status
		wantScore  int
	}{
		{"bad syntax", verdictInput{}, StatusInvalid, 0},
		{"disposable beats everything", with(func(v *verdictInput) { v.Disposable = true; v.Role = true }), StatusDisposable, 10},
		{"no mail", with(func(v *verdictInput) { v.DNS = dnsNoMail }), StatusInvalid, 0},
		{"null mx", with(func(v *verdictInput) { v.DNS = dnsNoMail; v.NullMX = true }), StatusInvalid, 0},
		{"dns temp error", with(func(v *verdictInput) { v.DNS = dnsTempErr }), StatusUnknown, 40},
		{"smtp undeliverable", with(func(v *verdictInput) { v.SMTP = SMTPUndeliverable }), StatusInvalid, 0},
		{"role beats deliverable", with(func(v *verdictInput) { v.Role = true; v.SMTP = SMTPDeliverable }), StatusRisky, 60},
		{"accept all", with(func(v *verdictInput) { v.SMTP = SMTPAcceptAll }), StatusAcceptAll, 50},
		{"smtp deliverable", with(func(v *verdictInput) { v.SMTP = SMTPDeliverable }), StatusValid, 95},
		{"smtp unknown keeps dns verdict", with(func(v *verdictInput) { v.SMTP = SMTPUnknown }), StatusValid, 90},
		{"role without smtp", with(func(v *verdictInput) { v.Role = true }), StatusRisky, 60},
		{"clean", ok, StatusValid, 90},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, score, _ := decide(c.in)
			if s != c.wantStatus || score != c.wantScore {
				t.Fatalf("decide() = (%s, %d), want (%s, %d)", s, score, c.wantStatus, c.wantScore)
			}
		})
	}
}
```

- [ ] **Step 2: Run and see it fail.** `GOTEST ./internal/services/verifier/` → FAIL.

- [ ] **Step 3: Implement** in `verifier.go`:

```go
// Resolver is the DNS surface the verifier needs; *net.Resolver satisfies it.
type Resolver interface {
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// SetResolver swaps the DNS resolver (tests, custom resolvers).
func (s *Service) SetResolver(r Resolver) { s.resolver = r }

type SMTPVerdict string

const (
	SMTPSkipped       SMTPVerdict = "skipped"
	SMTPDeliverable   SMTPVerdict = "deliverable"
	SMTPUndeliverable SMTPVerdict = "undeliverable"
	SMTPAcceptAll     SMTPVerdict = "accept_all"
	SMTPUnknown       SMTPVerdict = "unknown"
)

type dnsOutcome int

const (
	dnsOK      dnsOutcome = iota // domain can receive mail
	dnsNoMail                    // NXDOMAIN, no MX/A, or null MX: conclusively cannot
	dnsTempErr                   // timeout/SERVFAIL: undetermined, never cached
)

type verdictInput struct {
	SyntaxOK, Disposable, Role bool
	DNS                        dnsOutcome
	NullMX                     bool
	SMTP                       SMTPVerdict
}

// decide encodes the verdict precedence as a pure function so it is easy to test.
func decide(in verdictInput) (Status, int, string) {
	switch {
	case !in.SyntaxOK:
		return StatusInvalid, 0, "invalid syntax"
	case in.Disposable:
		return StatusDisposable, 10, "disposable email provider"
	case in.DNS == dnsTempErr:
		return StatusUnknown, 40, "DNS lookup failed temporarily"
	case in.DNS == dnsNoMail && in.NullMX:
		return StatusInvalid, 0, "domain does not accept mail (null MX)"
	case in.DNS == dnsNoMail:
		return StatusInvalid, 0, "domain has no mail exchanger (MX/A) records"
	case in.SMTP == SMTPUndeliverable:
		return StatusInvalid, 0, "mailbox does not exist"
	case in.Role:
		return StatusRisky, 60, "role-based address"
	case in.SMTP == SMTPAcceptAll:
		return StatusAcceptAll, 50, "domain accepts all addresses (catch-all)"
	case in.SMTP == SMTPDeliverable:
		return StatusValid, 95, ""
	default:
		return StatusValid, 90, ""
	}
}

// shouldCache keeps undetermined results out of the address cache.
func shouldCache(r *Result) bool { return r.Status != StatusUnknown }
```

  Outras mudanças no mesmo arquivo:
  - `Service.resolver` passa a ter o tipo `Resolver`; `NewService` continua atribuindo `net.DefaultResolver`.
  - `StatusAcceptAll Status = "accept_all"` e a tag `enum:"valid,invalid,risky,accept_all,disposable,unknown"` em
    `Result.Status`.
  - `Checks.SMTP` passa a ser `SMTPVerdict`, com o mesmo JSON. `baseResult` usa `SMTPSkipped`.
  - `setCache` retorna cedo quando `!shouldCache(r)`.
  - Troque `lookupMX` e `mxHosts` por:

```go
const nullMXSentinel = "nullmx"

// mxHosts returns the domain's mail hosts and the DNS outcome, caching only
// conclusive answers ("none"/"nullmx" sentinels for negatives).
func (s *Service) mxHosts(ctx context.Context, domain string) ([]string, dnsOutcome, bool) {
	key := mxKey(domain)
	if s.client != nil {
		if v, err := s.client.Get(ctx, key).Result(); err == nil {
			switch v {
			case "", "none":
				return nil, dnsNoMail, false
			case nullMXSentinel:
				return nil, dnsNoMail, true
			default:
				return strings.Split(v, ","), dnsOK, false
			}
		}
	}

	hosts, outcome, nullMX := s.lookupMX(ctx, domain)

	if s.client != nil && outcome != dnsTempErr {
		val := strings.Join(hosts, ",")
		if outcome == dnsNoMail {
			val = "none"
			if nullMX {
				val = nullMXSentinel
			}
		}
		s.client.Set(ctx, key, val, s.opts.MXTTL)
	}
	return hosts, outcome, nullMX
}

func (s *Service) lookupMX(ctx context.Context, domain string) ([]string, dnsOutcome, bool) {
	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	resolver := s.resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}

	mxs, err := resolver.LookupMX(lookupCtx, domain)
	switch {
	case err == nil && len(mxs) > 0:
		// RFC 7505: a single "." exchanger means the domain accepts no mail.
		if len(mxs) == 1 && strings.TrimSuffix(mxs[0].Host, ".") == "" {
			return nil, dnsNoMail, true
		}
		hosts := make([]string, 0, len(mxs))
		for _, mx := range mxs {
			hosts = append(hosts, mx.Host)
		}
		return hosts, dnsOK, false
	case err != nil && !isNotFound(err):
		return nil, dnsTempErr, false
	}

	// No MX: a domain with an A/AAAA record still receives mail (RFC 5321 §5.1).
	addrs, err := resolver.LookupHost(lookupCtx, domain)
	switch {
	case err == nil && len(addrs) > 0:
		return []string{domain}, dnsOK, false
	case err != nil && !isNotFound(err):
		return nil, dnsTempErr, false
	}
	return nil, dnsNoMail, false
}

func isNotFound(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound
}
```

  `compute` passa a chamar `_, outcome, nullMX := s.mxHosts(ctx, domain)` (só quando o domínio não é
  descartável) e
  `decide(verdictInput{SyntaxOK: true, Disposable: disposable, Role: role, DNS: outcome, NullMX: nullMX, SMTP: SMTPSkipped})`,
  com `r.Checks.MX = !disposable && outcome == dnsOK`. Quando o domínio é descartável, o `DNS` fica `dnsOK` e não
  há consulta.

- [ ] **Step 4: Run.** `GOTEST ./internal/services/verifier/` → PASS (incluindo os testes antigos de
  cache/JSON).

- [ ] **Step 5: Commit** — `fix(verify): treat DNS failures as unknown, reject null MX, never cache undetermined results`

---

### Task 6: lista de descartáveis completa + sugestão de typo — modelo: **sonnet**

**Files:**
- Create: `internal/services/verifier/data/disposable_domains.txt` (baixado)
- Modify: `internal/services/verifier/disposable.go`
- Create: `internal/services/verifier/suggest.go`
- Create: `internal/services/verifier/suggest_test.go`
- Modify: `internal/services/verifier/verifier.go` (campo `Suggestion`, preenchido em `compute`)
- Modify: `NOTICE` (atribuição)

**Interfaces:**
- Produces:
  - `Result.Suggestion string \`json:"suggestion,omitempty"\``
  - `isDisposable(domain string) bool`, agora casando também domínios pai
  - `suggestDomain(domain string) string`

- [ ] **Step 1: Baixar a lista (CC0)**

```bash
mkdir -p internal/services/verifier/data
curl -fsSL https://raw.githubusercontent.com/disposable-email-domains/disposable-email-domains/main/disposable_email_blocklist.conf \
  -o internal/services/verifier/data/disposable_domains.txt
wc -l internal/services/verifier/data/disposable_domains.txt   # esperado: milhares de linhas
```

  Anexe ao `NOTICE`: `internal/services/verifier/data/disposable_domains.txt — disposable-email-domains
  (https://github.com/disposable-email-domains/disposable-email-domains), CC0 1.0.`

- [ ] **Step 2: Failing tests** (`suggest_test.go`):

```go
package verifier

import "testing"

func TestDisposableListLoadedAndParentMatch(t *testing.T) {
	if len(disposableDomains) < 1000 {
		t.Fatalf("embedded list too small: %d", len(disposableDomains))
	}
	for _, d := range []string{"mailinator.com", "sub.mailinator.com", "YOPMAIL.COM"} {
		if !isDisposable(d) {
			t.Errorf("%s should be disposable", d)
		}
	}
	for _, d := range []string{"gmail.com", "uol.com.br", "com", ""} {
		if isDisposable(d) {
			t.Errorf("%s should not be disposable", d)
		}
	}
}

func TestSuggestDomain(t *testing.T) {
	cases := map[string]string{
		"gmial.com":   "gmail.com",
		"gmail.co":    "gmail.com",
		"hotmial.com": "hotmail.com",
		"yaho.com.br": "yahoo.com.br",
		"outlok.com":  "outlook.com",
		"gmail.com":   "",
		"uol.com.br":  "",
		"example.org": "",
		"acme.com.br": "",
	}
	for in, want := range cases {
		if got := suggestDomain(in); got != want {
			t.Errorf("suggestDomain(%q) = %q, want %q", in, got, want)
		}
	}
}
```

- [ ] **Step 3: Run and see it fail.** `GOTEST ./internal/services/verifier/` → FAIL.

- [ ] **Step 4: Implement.** Em `disposable.go`, troque o map literal por um conjunto carregado do arquivo
  embutido. O map atual continua como `extraDisposable` (mesmo conteúdo) e é mesclado ao conjunto:

```go
import (
	_ "embed"
	"strings"
)

//go:embed data/disposable_domains.txt
var disposableList string

// disposableDomains merges the embedded community blocklist with a few
// hand-picked extras. Built once at package init.
var disposableDomains = func() map[string]bool {
	set := make(map[string]bool, 8192)
	for _, line := range strings.Split(disposableList, "\n") {
		d := strings.ToLower(strings.TrimSpace(line))
		if d == "" || strings.HasPrefix(d, "#") {
			continue
		}
		set[d] = true
	}
	for d := range extraDisposable {
		set[d] = true
	}
	return set
}()

// isDisposable reports whether the domain, or any parent domain above the TLD,
// is a known throwaway provider (x.mailinator.com matches mailinator.com).
func isDisposable(domain string) bool {
	d := strings.ToLower(strings.TrimSpace(domain))
	for strings.Contains(d, ".") {
		if disposableDomains[d] {
			return true
		}
		d = d[strings.IndexByte(d, '.')+1:]
	}
	return false
}
```

  `suggest.go`:

```go
package verifier

import "strings"

// popularDomains are consumer mailbox providers common in Posta's audience
// (global + Brazil). Order breaks ties.
var popularDomains = []string{
	"gmail.com", "googlemail.com", "hotmail.com", "hotmail.com.br", "outlook.com", "outlook.com.br",
	"live.com", "msn.com", "yahoo.com", "yahoo.com.br", "icloud.com", "me.com", "aol.com",
	"proton.me", "protonmail.com", "uol.com.br", "bol.com.br", "terra.com.br", "ig.com.br",
	"globo.com", "globomail.com", "r7.com", "zipmail.com.br",
}

var popularSet = func() map[string]bool {
	m := make(map[string]bool, len(popularDomains))
	for _, d := range popularDomains {
		m[d] = true
	}
	return m
}()

// suggestDomain proposes a likely intended provider for a mistyped domain
// (Levenshtein distance 1–2 from a popular provider). Advisory only.
func suggestDomain(domain string) string {
	d := strings.ToLower(strings.TrimSpace(domain))
	if len(d) < 6 || popularSet[d] {
		return ""
	}
	best, bestDist := "", 3
	for _, p := range popularDomains {
		if dist := levenshtein(d, p); dist < bestDist {
			best, bestDist = p, dist
		}
	}
	return best
}

func levenshtein(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
```

  Em `verifier.go`: adicione `Suggestion string \`json:"suggestion,omitempty"\`` em `Result`, logo após `Reason`.
  Em `compute`, logo após separar `local, domain`, faça
  `if sug := suggestDomain(domain); sug != "" { r.Suggestion = local + "@" + sug }`.

  Se algum caso de `TestSuggestDomain` falhar por empate ou distância (ex.: `acme.com.br` fica a ≤ 2 de algum
  provedor), **não altere o teste para passar**: reporte ao controlador o par e a distância encontrados.

- [ ] **Step 5: Run and commit.** `GOTEST ./internal/services/verifier/` → PASS.
  Commit: `feat(verify): full disposable-domain list with parent matching; suggest fixes for mistyped providers`

---

## Fase 3 — validação em massa

### Task 7: `VerifyMany` (agrupado por domínio, pool, overlay em lote) — modelo: **opus** (concorrência)

**Files:**
- Modify: `internal/storage/repositories/bounce_repo.go` (novo `HardBouncedSet`)
- Create: `internal/storage/repositories/bounce_set_test.go`
- Modify: `internal/services/verifier/verifier.go`
- Create: `internal/services/verifier/many_test.go`
- Modify: `internal/config/config.go` (`EmailVerifyConcurrency`, env `POSTA_EMAIL_VERIFY_CONCURRENCY`, default 16)
- Modify: `internal/routes/routes.go:200` (passar `Concurrency`)

**Interfaces:**
- Consumes:
  - `SuppressionRepository.SuppressedSet`, `suppressionLookupChunk` (Task 4)
  - `Resolver`, `SetResolver`, `mxHosts(ctx, domain) ([]string, dnsOutcome, bool)`, `decide`, `shouldCache`
    (Task 5)
  - `suggestDomain` (Task 6)
- Produces:
  - `(*BounceRepository).HardBouncedSet(scope ResourceScope, emails []string) (map[string]struct{}, error)`
  - `Options.Concurrency int`
  - `(*Service).VerifyMany(ctx, scope, emails []string, fresh bool) ([]*Result, error)`, que cobra o rate limit
  - `(*Service).VerifyManyUnmetered(ctx, scope, emails []string, fresh bool) ([]*Result, error)`, sem cobrança,
    para jobs
  - `(*Service).computeDomain(ctx, domain string, addrs []string, fresh bool) map[string]*Result`
  - `(*Service).buildResult(email string, disposable bool, outcome dnsOutcome, nullMX bool, smtp SMTPVerdict) *Result`
  - `Verify` passa a delegar para `VerifyMany`

- [ ] **Step 1: Failing tests** (`many_test.go`):

```go
package verifier

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"

	"github.com/goposta/posta/internal/storage/repositories"
)

type setLookup struct{ set map[string]struct{} }

func (s setLookup) SuppressedSet(_ repositories.ResourceScope, _ []string) (map[string]struct{}, error) {
	return s.set, nil
}
func (s setLookup) HardBouncedSet(_ repositories.ResourceScope, _ []string) (map[string]struct{}, error) {
	return s.set, nil
}

type countingResolver struct {
	mu    sync.Mutex
	calls map[string]int
}

func (c *countingResolver) LookupMX(_ context.Context, d string) ([]*net.MX, error) {
	c.mu.Lock()
	c.calls[d]++
	c.mu.Unlock()
	return []*net.MX{{Host: "mx." + d, Pref: 10}}, nil
}
func (c *countingResolver) LookupHost(_ context.Context, d string) ([]string, error) {
	return []string{"192.0.2.1"}, nil
}

func TestVerifyManyOrderDedupAndSyntax(t *testing.T) {
	s := NewService(nil, nil, nil, Options{Enabled: true, Concurrency: 4})
	s.SetResolver(&countingResolver{calls: map[string]int{}})
	in := []string{"b@x.com", "not-an-email", "A@X.com", "b@x.com", "c@y.com"}
	res, err := s.VerifyMany(context.Background(), repositories.ResourceScope{UserID: 1}, in, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != len(in) {
		t.Fatalf("len = %d", len(res))
	}
	want := []string{"b@x.com", "not-an-email", "a@x.com", "b@x.com", "c@y.com"}
	for i, w := range want {
		if res[i].Email != w {
			t.Errorf("res[%d].Email = %q, want %q", i, res[i].Email, w)
		}
	}
	if res[1].Status != StatusInvalid || res[0].Status != StatusValid {
		t.Fatalf("statuses: %s %s", res[0].Status, res[1].Status)
	}
	if res[0] == res[3] {
		t.Fatal("duplicate inputs must get distinct result copies")
	}
}

func TestVerifyManyOneMXLookupPerDomain(t *testing.T) {
	r := &countingResolver{calls: map[string]int{}}
	s := NewService(nil, nil, nil, Options{Enabled: true, Concurrency: 8})
	s.SetResolver(r)
	var in []string
	for i := 0; i < 60; i++ {
		in = append(in, fmt.Sprintf("u%d@d%d.com", i, i%3))
	}
	if _, err := s.VerifyMany(context.Background(), repositories.ResourceScope{UserID: 1}, in, false); err != nil {
		t.Fatal(err)
	}
	for d, n := range r.calls {
		if n != 1 {
			t.Errorf("domain %s looked up %d times", d, n)
		}
	}
	if len(r.calls) != 3 {
		t.Fatalf("domains looked up = %d", len(r.calls))
	}
}

func TestVerifyManyOverlay(t *testing.T) {
	s := NewService(nil, nil, nil, Options{Enabled: true})
	s.SetResolver(&countingResolver{calls: map[string]int{}})
	s.suppressions = setLookup{set: map[string]struct{}{"sup@x.com": {}}}
	s.bounces = setLookup{set: map[string]struct{}{"bnc@x.com": {}}}
	res, err := s.VerifyMany(context.Background(), repositories.ResourceScope{UserID: 1},
		[]string{"sup@x.com", "bnc@x.com", "ok@x.com"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res[0].Suppressed || res[0].Status != StatusInvalid {
		t.Errorf("suppressed: %+v", res[0])
	}
	if !res[1].PreviouslyBounced || res[1].Status != StatusInvalid {
		t.Errorf("bounced: %+v", res[1])
	}
	if res[2].Status != StatusValid || res[2].Suppressed || res[2].PreviouslyBounced {
		t.Errorf("clean: %+v", res[2])
	}
}

func TestVerifyDelegatesToMany(t *testing.T) {
	s := NewService(nil, nil, nil, Options{Enabled: true})
	s.SetResolver(&countingResolver{calls: map[string]int{}})
	r, err := s.Verify(context.Background(), repositories.ResourceScope{UserID: 1}, " Foo@X.com ", false)
	if err != nil || r.Email != "foo@x.com" || r.Status != StatusValid {
		t.Fatalf("r=%+v err=%v", r, err)
	}
}
```

  Teste de repositório (`bounce_set_test.go`, mesmo formato do `suppression_set_test.go` da Task 4): cria bounces
  `hard` e `soft` no workspace 9001 e um `hard` no 9002. `HardBouncedSet` com escopo 9001 deve retornar só o
  `hard` de 9001. Como `Bounce.EmailID` é NOT NULL e tem FK para `emails`, faça `AutoMigrate(&models.Email{},
  &models.Bounce{})` e crie um `models.Email` mínimo (UserID, Sender, Subject) antes. Se a FK exigir outros
  campos, preencha-os e anote no commit.

- [ ] **Step 2: Run and see it fail.** `GOTEST ./internal/services/verifier/` → FAIL.

- [ ] **Step 3: Implement.**

  `bounce_repo.go`:

```go
// HardBouncedSet returns which of emails hard-bounced in scope, keyed by
// normalized address. Queried in chunks to stay under the bind-param limit.
func (r *BounceRepository) HardBouncedSet(scope ResourceScope, emails []string) (map[string]struct{}, error) {
	out := make(map[string]struct{})
	norm := make([]string, 0, len(emails))
	for _, e := range emails {
		if n := normalizeEmail(e); n != "" {
			norm = append(norm, n)
		}
	}
	for start := 0; start < len(norm); start += suppressionLookupChunk {
		end := min(start+suppressionLookupChunk, len(norm))
		var hits []string
		if err := ApplyScope(r.db.Model(&models.Bounce{}), scope).
			Where("type = ?", models.BounceTypeHard).
			Where("LOWER(recipient) IN ?", norm[start:end]).
			Distinct().Pluck("LOWER(recipient)", &hits).Error; err != nil {
			return nil, err
		}
		for _, h := range hits {
			out[h] = struct{}{}
		}
	}
	return out, nil
}
```

  `verifier.go`:
  - Troque os campos `suppressionRepo`/`bounceRepo` por interfaces
    (`suppressions suppressionLookup`, `bounces bounceLookup`):

```go
type suppressionLookup interface {
	SuppressedSet(scope repositories.ResourceScope, emails []string) (map[string]struct{}, error)
}
type bounceLookup interface {
	HardBouncedSet(scope repositories.ResourceScope, emails []string) (map[string]struct{}, error)
}
```

  - Em `NewService`, atribua só quando o ponteiro não for nil, para evitar a armadilha da interface não-nil
    contendo ponteiro nil:

```go
	if suppressionRepo != nil {
		s.suppressions = suppressionRepo
	}
	if bounceRepo != nil {
		s.bounces = bounceRepo
	}
```

  - `Options.Concurrency int`. Se for `<= 0`, use 16.
  - Rate limit por quantidade (substitui o `checkRate` atual):

```go
func (s *Service) checkRate(ctx context.Context, userID uint, n int) error {
	if s.client == nil || n <= 0 {
		return nil
	}
	key := fmt.Sprintf("verify:rl:hour:%d:%s", userID, time.Now().Format("2006010215"))
	total, err := s.client.IncrBy(ctx, key, int64(n)).Result()
	if err != nil {
		return nil
	}
	if total == int64(n) {
		s.client.Expire(ctx, key, time.Hour)
	}
	if total > int64(s.opts.RateHourly) {
		return ErrRateLimited
	}
	return nil
}
```

  - Núcleo:

```go
// Verify checks one address; it is VerifyMany with a single input.
func (s *Service) Verify(ctx context.Context, scope repositories.ResourceScope, rawEmail string, fresh bool) (*Result, error) {
	res, err := s.VerifyMany(ctx, scope, []string{rawEmail}, fresh)
	if err != nil {
		return nil, err
	}
	return res[0], nil
}

// VerifyMany verifies addresses in input order, charging the hourly rate
// limit once per distinct syntactically valid address.
func (s *Service) VerifyMany(ctx context.Context, scope repositories.ResourceScope, emails []string, fresh bool) ([]*Result, error) {
	return s.verifyMany(ctx, scope, emails, fresh, true)
}

// VerifyManyUnmetered is VerifyMany without the hourly rate limit, for
// background jobs whose size was already bounded at submission.
func (s *Service) VerifyManyUnmetered(ctx context.Context, scope repositories.ResourceScope, emails []string, fresh bool) ([]*Result, error) {
	return s.verifyMany(ctx, scope, emails, fresh, false)
}

func (s *Service) verifyMany(ctx context.Context, scope repositories.ResourceScope, emails []string, fresh, metered bool) ([]*Result, error) {
	results := make([]*Result, len(emails))
	positions := make(map[string][]int) // normalized address -> input indexes
	var distinct []string

	for i, raw := range emails {
		email, ok := normalizeAddress(raw)
		if !ok {
			r := baseResult(strings.ToLower(strings.TrimSpace(raw)))
			r.Status, r.Score, r.Reason = decide(verdictInput{})
			results[i] = r
			continue
		}
		if _, seen := positions[email]; !seen {
			distinct = append(distinct, email)
		}
		positions[email] = append(positions[email], i)
	}
	if len(distinct) == 0 {
		return results, nil
	}

	if metered && s.opts.RateHourly > 0 {
		if err := s.checkRate(ctx, scope.UserID, len(distinct)); err != nil {
			return nil, err
		}
	}

	suppressed, bounced := s.overlay(scope, distinct)

	var toCompute []string
	for _, e := range distinct {
		_, sup := suppressed[e]
		_, bnc := bounced[e]
		if !sup && !bnc {
			toCompute = append(toCompute, e)
		}
	}
	intrinsic := s.computeMany(ctx, toCompute, fresh)

	for _, e := range distinct {
		_, sup := suppressed[e]
		_, bnc := bounced[e]
		var base *Result
		if sup || bnc {
			base = baseResult(e)
			base.Checks.Syntax = true
			base.Status, base.Score = StatusInvalid, 0
			base.Suppressed, base.PreviouslyBounced = sup, bnc
			if sup {
				base.Reason = "address is on the suppression list"
			} else {
				base.Reason = "address previously hard-bounced"
			}
		} else {
			base = intrinsic[e]
		}
		for _, idx := range positions[e] {
			cp := *base
			results[idx] = &cp
		}
	}
	return results, nil
}

func normalizeAddress(raw string) (string, bool) {
	addr, err := mail.ParseAddress(strings.TrimSpace(raw))
	if err != nil || !strings.Contains(addr.Address, "@") {
		return "", false
	}
	return strings.ToLower(addr.Address), true
}

// overlay loads the tenant's suppression and hard-bounce history in one query
// each. Lookup errors fail open, as the single-address path always has.
func (s *Service) overlay(scope repositories.ResourceScope, emails []string) (map[string]struct{}, map[string]struct{}) {
	sup, bnc := map[string]struct{}{}, map[string]struct{}{}
	if s.suppressions != nil {
		if m, err := s.suppressions.SuppressedSet(scope, emails); err == nil {
			sup = m
		}
	}
	if s.bounces != nil {
		if m, err := s.bounces.HardBouncedSet(scope, emails); err == nil {
			bnc = m
		}
	}
	return sup, bnc
}

// computeMany resolves intrinsic results, grouping by domain so each domain's
// DNS is looked up once, with at most Concurrency domains in flight.
func (s *Service) computeMany(ctx context.Context, emails []string, fresh bool) map[string]*Result {
	out := make(map[string]*Result, len(emails))
	if len(emails) == 0 {
		return out
	}
	byDomain := make(map[string][]string)
	var domains []string
	for _, e := range emails {
		d := e[strings.LastIndex(e, "@")+1:]
		if _, ok := byDomain[d]; !ok {
			domains = append(domains, d)
		}
		byDomain[d] = append(byDomain[d], e)
	}

	workers := s.opts.Concurrency
	if workers <= 0 {
		workers = 16
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)
	for _, d := range domains {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(domain string, addrs []string) {
			defer wg.Done()
			defer func() { <-sem }()
			local := s.computeDomain(ctx, domain, addrs, fresh)
			mu.Lock()
			for k, v := range local {
				out[k] = v
			}
			mu.Unlock()
		}(d, byDomain[d])
	}
	wg.Wait()

	// Anything skipped by cancellation is reported as undetermined.
	for _, e := range emails {
		if out[e] == nil {
			r := baseResult(e)
			r.Checks.Syntax = true
			r.Status, r.Score, r.Reason = decide(verdictInput{SyntaxOK: true, DNS: dnsTempErr})
			out[e] = r
		}
	}
	return out
}

// computeDomain serves cached addresses and computes the rest with a single
// DNS resolution for the domain.
func (s *Service) computeDomain(ctx context.Context, domain string, addrs []string, fresh bool) map[string]*Result {
	out := make(map[string]*Result, len(addrs))
	var pending []string
	for _, e := range addrs {
		if !fresh {
			if cached := s.getCache(ctx, e); cached != nil {
				cached.Cached = true
				out[e] = cached
				continue
			}
		}
		pending = append(pending, e)
	}
	if len(pending) == 0 {
		return out
	}

	disposable := isDisposable(domain)
	outcome, nullMX := dnsOK, false
	if !disposable {
		_, outcome, nullMX = s.mxHosts(ctx, domain)
	}
	for _, e := range pending {
		out[e] = s.buildResult(e, disposable, outcome, nullMX, SMTPSkipped)
		s.setCache(ctx, e, out[e])
	}
	return out
}

// buildResult assembles the intrinsic result for one syntactically valid address.
func (s *Service) buildResult(email string, disposable bool, outcome dnsOutcome, nullMX bool, smtp SMTPVerdict) *Result {
	at := strings.LastIndex(email, "@")
	local, domain := email[:at], email[at+1:]
	r := baseResult(email)
	r.Checks.Syntax = true
	r.Checks.Disposable = disposable
	r.Checks.RoleAccount = isRoleAccount(local)
	r.Checks.MX = !disposable && outcome == dnsOK
	r.Checks.SMTP = smtp
	r.MailboxVerified = smtp == SMTPDeliverable
	if sug := suggestDomain(domain); sug != "" {
		r.Suggestion = local + "@" + sug
	}
	r.Status, r.Score, r.Reason = decide(verdictInput{
		SyntaxOK: true, Disposable: disposable, Role: r.Checks.RoleAccount,
		DNS: outcome, NullMX: nullMX, SMTP: smtp,
	})
	return r
}
```

  Faça `compute(ctx, email)` (usado pelos testes da Task 5) delegar para
  `s.computeDomain(ctx, email[strings.LastIndex(email, "@")+1:], []string{email}, true)[email]`. Remova
  `tenantOverlay` e o antigo `checkRate(ctx, userID)` depois de migrar as chamadas. Se
  `CountHardBouncesByRecipient` ficar sem uso, mantenha: é API de repositório.

- [ ] **Step 4: Run with race.** `GOTEST ./internal/services/verifier/`, o comando "Race detector" e
  `GOTEST_DB ./internal/storage/repositories/` → PASS.

- [ ] **Step 5: Commit** — `feat(verify): VerifyMany with per-domain DNS, bounded concurrency and batched tenant overlay`

---

### Task 8: `POST /api/v1/emails/verify/batch` (síncrono ≤ 100) — modelo: **sonnet**

**Files:**
- Modify: `internal/handlers/verify_handler.go`
- Create: `internal/handlers/verify_batch_test.go`
- Modify: `internal/routes/auth_routes.go` (rota logo após `/emails/verify`)

**Interfaces:**
- Consumes: `(*verifier.Service).VerifyMany` (Task 7)
- Produces: `VerifyBatchRequest`, `VerifyBatchResponse{Items []*verifier.Result; Summary map[verifier.Status]int}`,
  `const MaxVerifyBatch = 100`

- [ ] **Step 1: Failing test** (`verify_batch_test.go`), que exercita a validação via okapi real:

```go
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
```

  Se o okapi rejeitar `{"emails":[]}` com 400 antes do handler (por `required:"true"`), o teste continua válido:
  o que importa é o `400`.

- [ ] **Step 2: Run and see it fail.** `GOTEST ./internal/handlers/` → FAIL.

- [ ] **Step 3: Implement** (`verify_handler.go`):

```go
// MaxVerifyBatch caps the synchronous batch endpoint; larger lists go through
// verification jobs.
const MaxVerifyBatch = 100

type VerifyBatchRequest struct {
	Fresh bool `query:"fresh" doc:"Bypass the cache and re-check every address"`
	Body  struct {
		Emails []string `json:"emails" required:"true" doc:"Up to 100 addresses; malformed entries are reported as invalid, not rejected"`
	} `json:"body"`
}

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
```

  A rota vai em `auth_routes.go`, depois de `/emails/verify`, no mesmo grupo `apiAuth`, com `Request:
  &handlers.VerifyBatchRequest{}`, `Response: &dto.Response[handlers.VerifyBatchResponse]{}` e as mesmas
  `DocErrorResponse` (401, 404, 429) mais 400 e 413. Confira a assinatura de `AbortRequestEntityTooLarge` na fonte
  do okapi.

- [ ] **Step 4: Run and commit.** `GOTEST ./internal/handlers/` + build → PASS.
  Commit: `feat(verify): synchronous batch endpoint (up to 100 addresses)`

---

### Task 9: jobs assíncronos — modelo, repositório, task asynq — modelo: **opus** (transações/retomada/worker)

**Files:**
- Create: `internal/models/email_verify_job.go`
- Modify: `internal/storage/migration/migration.go` (adicionar os dois modelos à lista de `AutoMigrate`)
- Modify: `internal/storage/migration/constraints.go` (índice único parcial de idempotência)
- Create: `internal/storage/repositories/email_verify_job_repo.go`
- Create: `internal/storage/repositories/email_verify_job_repo_test.go`
- Modify: `internal/worker/tasks.go` (`TypeVerifyJob = "verify:job"`, payload e construtor)
- Modify: `internal/worker/producer.go` (`EnqueueVerifyJob`)
- Create: `internal/worker/verify_job_handler.go`
- Create: `internal/worker/verify_job_handler_test.go`
- Create: `internal/services/verifier/config.go` (`FromConfig`)
- Modify: `cmd/posta/worker.go`, `cmd/posta/server.go` (registrar `TypeVerifyJob` nos dois mux; construir o
  verifier com um `*redis.Client` via `storage.NewRedis(cfg.Redis.RedisOptions())`)
- Modify: `internal/routes/routes.go:200` (usar `verifier.FromConfig`)

**Interfaces:**
- Consumes: `VerifyManyUnmetered`, `Result`, `Status*`, `SetResolver` (Tasks 5–7); `SuppressionRepository.Upsert`,
  `IsSuppressed`
- Produces:
  - `models.EmailVerifyJob`, `models.EmailVerifyItem`, `models.EmailVerifyJobStatus`
    (`queued|running|completed|failed`), `models.EmailVerifyItemPending`
  - `repositories.NewEmailVerifyJobRepository(db)` com:
    - `CreateWithItems(job *models.EmailVerifyJob, emails []string) error`
    - `FindByID(id uint) (*models.EmailVerifyJob, error)`
    - `FindByUUID(scope ResourceScope, uuid string) (*models.EmailVerifyJob, error)`
    - `FindByIdempotencyKey(scope ResourceScope, key string) (*models.EmailVerifyJob, error)`
    - `CountActive(scope ResourceScope) (int64, error)`
    - `PendingItems(jobID uint, limit int) ([]models.EmailVerifyItem, error)`
    - `SaveResults(jobID uint, items []models.EmailVerifyItem) error`
    - `MarkRunning(jobID uint) error`
    - `MarkCompleted(jobID uint) error`
    - `MarkFailed(jobID uint, msg string) error`
    - `ListItems(jobID uint, status string, limit, offset int) ([]models.EmailVerifyItem, int64, error)`
    - `EachItem(jobID uint, fn func(models.EmailVerifyItem) error) error`
    - `EmailsWithStatus(jobID uint, statuses ...string) ([]string, error)`
    - `StatusCounts(jobID uint) (map[string]int64, error)`
  - `worker.TypeVerifyJob`, `worker.VerifyJobPayload{JobID uint}`
  - `(*worker.Producer).EnqueueVerifyJob(jobID uint) error`
  - `worker.NewVerifyJobHandler(repo *repositories.EmailVerifyJobRepository, v *verifier.Service, sup *repositories.SuppressionRepository) *VerifyJobHandler`
  - `verifier.FromConfig(cfg *config.Config, rc *redis.Client, sup *repositories.SuppressionRepository, bnc *repositories.BounceRepository) *Service`

- [ ] **Step 1: Model** (`internal/models/email_verify_job.go`):

```go
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
```

  Em `constraints.go`, depois dos índices existentes:

```go
	// Idempotency: one job per (workspace, Idempotency-Key). NULL keys are free.
	db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_verify_job_idempotency
		ON email_verify_jobs (workspace_id, idempotency_key) WHERE idempotency_key IS NOT NULL`)
```

- [ ] **Step 2: Failing repository test** (`email_verify_job_repo_test.go`, com banco):

```go
package repositories

import (
	"testing"

	"github.com/goposta/posta/internal/models"
)

func verifyJobDB(t *testing.T) *EmailVerifyJobRepository {
	t.Helper()
	db := testDB(t)
	if err := db.AutoMigrate(&models.EmailVerifyJob{}, &models.EmailVerifyItem{}); err != nil {
		t.Skipf("skipping: %v", err)
	}
	tx := db.Begin()
	t.Cleanup(func() { tx.Rollback() })
	return NewEmailVerifyJobRepository(tx)
}

func TestVerifyJobLifecycle(t *testing.T) {
	repo := verifyJobDB(t)
	ws := uint(7001)
	scope := ResourceScope{UserID: 1, WorkspaceID: &ws}
	job := &models.EmailVerifyJob{UserID: 1, WorkspaceID: &ws, Source: "emails"}
	if err := repo.CreateWithItems(job, []string{"a@x.com", "b@x.com", "c@x.com"}); err != nil {
		t.Fatal(err)
	}
	if job.Total != 3 || job.UUID == "" {
		t.Fatalf("job = %+v", job)
	}
	got, err := repo.FindByUUID(scope, job.UUID)
	if err != nil || got.ID != job.ID {
		t.Fatalf("FindByUUID: %v", err)
	}
	other := uint(7002)
	if _, err := repo.FindByUUID(ResourceScope{UserID: 1, WorkspaceID: &other}, job.UUID); err == nil {
		t.Fatal("job must not be visible from another workspace")
	}

	pending, _ := repo.PendingItems(job.ID, 2)
	if len(pending) != 2 {
		t.Fatalf("pending = %d", len(pending))
	}
	for i := range pending {
		pending[i].Status = "valid"
		pending[i].Score = 90
	}
	if err := repo.SaveResults(job.ID, pending); err != nil {
		t.Fatal(err)
	}
	// Saving the same chunk again (retried task) must not double count.
	if err := repo.SaveResults(job.ID, pending); err != nil {
		t.Fatal(err)
	}
	pending, _ = repo.PendingItems(job.ID, 10)
	if len(pending) != 1 || pending[0].Email != "c@x.com" {
		t.Fatalf("remaining = %+v", pending)
	}
	got, _ = repo.FindByID(job.ID)
	if got.Processed != 2 {
		t.Fatalf("processed = %d", got.Processed)
	}
	items, total, _ := repo.ListItems(job.ID, "valid", 10, 0)
	if total != 2 || len(items) != 2 {
		t.Fatalf("list valid = %d/%d", len(items), total)
	}
	counts, _ := repo.StatusCounts(job.ID)
	if counts["valid"] != 2 || counts[models.EmailVerifyItemPending] != 1 {
		t.Fatalf("counts = %v", counts)
	}
}
```

- [ ] **Step 3: Implement the repository.** Pontos obrigatórios:
  - `CreateWithItems`: em transação, preenche `job.Total = len(emails)`, `job.Status = queued`, cria o job e os
    itens com `CreateInBatches(items, 1000)`.
  - `FindByUUID` e `FindByIdempotencyKey` usam `ApplyScope`.
  - `CountActive`: `status IN (queued, running)` no escopo.
  - `PendingItems`: `job_id = ? AND status = 'pending' ORDER BY id LIMIT ?`.
  - `SaveResults`: em transação, atualiza cada item **condicionado a `status = 'pending'`** e soma ao
    `processed` do job só o número de linhas efetivamente alteradas. Isso impede contagem dupla quando uma task
    retomada reprocessa um bloco já salvo:

```go
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
```

  - `MarkRunning`: `status = running` e `started_at = COALESCE(started_at, now())`.
  - `MarkCompleted`: `status = completed` e `completed_at = now()`.
  - `MarkFailed`: `status = failed` e `error = msg`.
  - `ListItems`: filtro opcional por `status`, `ORDER BY id`, total por `Count`.
  - `EachItem`: `FindInBatches(…, 1000, …)` ordenado por id.
  - `EmailsWithStatus`: `Pluck("email")`.
  - `StatusCounts`: `SELECT status, COUNT(*) … WHERE job_id = ? GROUP BY status`.

- [ ] **Step 4: Failing worker test** (`verify_job_handler_test.go`, com banco). Ele não precisa de rede: o
  verifier recebe um resolver falso via `SetResolver`.

```go
package worker

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"testing"

	"github.com/goposta/posta/internal/models"
	"github.com/goposta/posta/internal/services/verifier"
	"github.com/goposta/posta/internal/storage/repositories"
	"github.com/hibiken/asynq"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

type jobResolver struct{}

func (jobResolver) LookupMX(_ context.Context, d string) ([]*net.MX, error) {
	if d == "example.com" {
		return []*net.MX{{Host: ".", Pref: 0}}, nil // null MX
	}
	return []*net.MX{{Host: "mx." + d, Pref: 10}}, nil
}
func (jobResolver) LookupHost(context.Context, string) ([]string, error) { return nil, nil }

func verifyTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("skipping: TEST_DATABASE_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	if err := db.AutoMigrate(&models.User{}, &models.Suppression{}, &models.Email{}, &models.Bounce{},
		&models.EmailVerifyJob{}, &models.EmailVerifyItem{}); err != nil {
		t.Skipf("skipping: %v", err)
	}
	return db
}

func TestVerifyJobProcessesPendingAndApplies(t *testing.T) {
	db := verifyTestDB(t)
	tx := db.Begin()
	defer tx.Rollback()

	u := &models.User{Name: "t", Email: "verifyjob@example.com", PasswordHash: "x"}
	if err := tx.Create(u).Error; err != nil {
		t.Fatal(err)
	}
	ws := uint(8801)
	jobs := repositories.NewEmailVerifyJobRepository(tx)
	sup := repositories.NewSuppressionRepository(tx)
	v := verifier.NewService(nil, sup, repositories.NewBounceRepository(tx), verifier.Options{Enabled: true})
	v.SetResolver(jobResolver{})

	job := &models.EmailVerifyJob{UserID: u.ID, WorkspaceID: &ws, Source: "emails", Apply: true}
	if err := jobs.CreateWithItems(job, []string{"ok@good.com", "bad-syntax", "x@example.com"}); err != nil {
		t.Fatal(err)
	}
	// Simulate a previous partial run: first item already done.
	first, _ := jobs.PendingItems(job.ID, 1)
	first[0].Status = "valid"
	_ = jobs.SaveResults(job.ID, first)

	h := NewVerifyJobHandler(jobs, v, sup)
	payload, _ := json.Marshal(VerifyJobPayload{JobID: job.ID})
	if err := h.ProcessTask(context.Background(), asynq.NewTask(TypeVerifyJob, payload)); err != nil {
		t.Fatal(err)
	}

	got, _ := jobs.FindByID(job.ID)
	if got.Status != models.EmailVerifyJobCompleted || got.Processed != 3 {
		t.Fatalf("job = %+v", got)
	}
	counts, _ := jobs.StatusCounts(job.ID)
	if counts["valid"] != 1 || counts["invalid"] != 2 {
		t.Fatalf("counts = %v", counts)
	}
	scope := repositories.ResourceScope{UserID: u.ID, WorkspaceID: &ws}
	if s, _ := sup.IsSuppressed(scope, "x@example.com"); !s {
		t.Fatal("apply=true must suppress invalid addresses")
	}
}
```

- [ ] **Step 5: Implement the task and handler.**

  `tasks.go`:

```go
	TypeVerifyJob = "verify:job"
// …
type VerifyJobPayload struct {
	JobID uint `json:"job_id"`
}

func NewVerifyJobTask(jobID uint, opts ...asynq.Option) (*asynq.Task, error) {
	payload, err := json.Marshal(VerifyJobPayload{JobID: jobID})
	if err != nil {
		return nil, err
	}
	return asynq.NewTask(TypeVerifyJob, payload, opts...), nil
}
```

  `producer.go`, no mesmo formato de `EnqueueMessageProcess`: fila `QueueLow`, `asynq.MaxRetry(5)`,
  `asynq.Timeout(2*time.Hour)`, `asynq.TaskID(fmt.Sprintf("verify:job:%d", jobID))` e `ErrTaskIDConflict` tratado
  como sucesso.

  `verify_job_handler.go`:

```go
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/goposta/posta/internal/models"
	"github.com/goposta/posta/internal/services/verifier"
	"github.com/goposta/posta/internal/storage/repositories"
	"github.com/hibiken/asynq"
	"github.com/jkaninda/logger"
)

const verifyJobChunk = 500

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
		return fmt.Errorf("verify job %d not found: %w: %w", p.JobID, err, asynq.SkipRetry)
	}
	if job.Status == models.EmailVerifyJobCompleted || job.Status == models.EmailVerifyJobFailed {
		return nil
	}
	defer func() {
		if err != nil && isLastAttempt(ctx) {
			_ = h.jobs.MarkFailed(job.ID, err.Error())
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
		now := time.Now().UTC()
		for i := range items {
			r := results[i]
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
		if ctx.Err() != nil {
			return ctx.Err() // resume on retry
		}
	}

	if job.Apply && h.suppressions != nil {
		bad, err := h.jobs.EmailsWithStatus(job.ID, string(verifier.StatusInvalid), string(verifier.StatusDisposable))
		if err != nil {
			return err
		}
		for _, e := range bad {
			_ = h.suppressions.Upsert(&models.Suppression{
				UserID: job.UserID, WorkspaceID: job.WorkspaceID, Email: e,
				Kind: models.SuppressionKindManual, Reason: "email verification job " + job.UUID,
			})
		}
	}
	logger.Info("verify job completed", "job", job.UUID, "total", job.Total)
	return h.jobs.MarkCompleted(job.ID)
}

func isLastAttempt(ctx context.Context) bool {
	retried, ok1 := asynq.GetRetryCount(ctx)
	maxRetry, ok2 := asynq.GetMaxRetry(ctx)
	return ok1 && ok2 && retried >= maxRetry
}
```

  `apply` suprime inclusive endereços `invalid` por sintaxe (ex.: `bad-syntax`). Isso é inofensivo: nenhum envio
  chega a esse endereço de qualquer forma.

  `internal/services/verifier/config.go`:

```go
package verifier

import (
	"time"

	"github.com/goposta/posta/internal/config"
	"github.com/goposta/posta/internal/storage/repositories"
	"github.com/redis/go-redis/v9"
)

// FromConfig builds the verifier the same way for the API server and the worker.
func FromConfig(cfg *config.Config, rc *redis.Client, sup *repositories.SuppressionRepository, bnc *repositories.BounceRepository) *Service {
	return NewService(rc, sup, bnc, Options{
		Enabled:     cfg.EmailVerifyEnabled,
		AddrTTL:     time.Duration(cfg.EmailVerifyCacheTTLHours) * time.Hour,
		MXTTL:       time.Duration(cfg.EmailVerifyMXCacheTTLHours) * time.Hour,
		RateHourly:  cfg.EmailVerifyRateHourly,
		Concurrency: cfg.EmailVerifyConcurrency,
	})
}
```

  Antes de criar `config.go`, confirme que `internal/config` não importa `internal/services/verifier` nem
  `repositories` (`go list -deps ./internal/config | grep posta`). Se houver ciclo, mova `FromConfig` para
  `cmd/posta` e para `routes.go`, e anote isso no commit. Em `cmd/posta/worker.go` e `cmd/posta/server.go`,
  construa o handler e registre `mux.HandleFunc(worker.TypeVerifyJob, verifyJobHandler.ProcessTask)` nos dois.
  Em `server.go`, verifique se já existe um `*redis.Client` antes de criar outro.

- [ ] **Step 6: Run.** `GOTEST_DB ./internal/storage/repositories/ ./internal/worker/`, `GOTEST ./internal/...` e
  o build → PASS.

- [ ] **Step 7: Commit** — `feat(verify): persisted bulk verification jobs processed by a resumable asynq task`

---

### Task 10: endpoints de job — modelo: **sonnet**

**Files:**
- Create: `internal/handlers/verify_job_handler.go`
- Create: `internal/handlers/verify_job_handler_test.go`
- Modify: `internal/routes/auth_routes.go` (4 rotas no grupo `apiAuth`, depois de `/emails/verify/batch`)
- Modify: `internal/routes/routes.go` (campo `verifyJob` + construção com repo, `SubscriberListRepository`,
  `SubscriberRepository` e `producer`)

**Interfaces:**
- Consumes: repositório e producer da Task 9; `SubscriberListRepository.FindByID`, `ListMembers`;
  `SubscriberRepository.FindByFilterRules`; `verifier.Result`
- Produces:
  - `POST /api/v1/emails/verify/jobs`
  - `GET /api/v1/emails/verify/jobs/{id}`
  - `GET /api/v1/emails/verify/jobs/{id}/results`
  - `GET /api/v1/emails/verify/jobs/{id}/results.csv`

  O `{id}` é o UUID do job. Confira em `internal/routes/paths.go` a sintaxe de parâmetro sem tipo (`{id}`) e a
  tag `param:"id"` como string. Com o producer `nil` (sem Redis), `POST …/jobs` responde `503`.

Contrato:

| Endpoint | Entrada | Sucesso | Erros |
|---|---|---|---|
| `POST …/jobs` | header opcional `Idempotency-Key` (≤ 128); body `{emails?: string[], subscriber_list_id?: uint, fresh?: bool, apply?: bool}` | `202` + `JobView` | `400` (nenhum ou ambos: `emails`/`subscriber_list_id`; lista vazia), `404` (lista fora do escopo), `413` (> 50.000), `429` (≥ 2 jobs ativos no escopo) |
| `GET …/jobs/{id}` | — | `200` + `JobView` | `404` |
| `GET …/jobs/{id}/results` | `status`, `page` (0-based), `size` (default 100, máx. 1000) | `200` paginado (`paginated`) com `ItemView` | `404` |
| `GET …/jobs/{id}/results.csv` | — | `200` `text/csv`, `Content-Disposition: attachment; filename="verify-<uuid>.csv"` | `404` |

```go
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

type ItemView struct {
	Email      string           `json:"email"`
	Status     string           `json:"status"`
	Score      int              `json:"score"`
	Reason     string           `json:"reason,omitempty"`
	Suggestion string           `json:"suggestion,omitempty"`
	Result     *verifier.Result `json:"result,omitempty"`
}
```

  Colunas do CSV: `email,status,score,reason,suggestion,suppressed,previously_bounced,mx,disposable,role_account,smtp`.
  Gere com `encoding/csv` sobre `c.ResponseWriter()`, iterando `EachItem` e decodificando `Result` quando
  presente.

- [ ] **Step 1: Failing tests** (`verify_job_handler_test.go`). Testam as funções puras que o handler usa:

```go
package handlers

import (
	"testing"
	"time"

	"github.com/goposta/posta/internal/models"
)

func TestNormalizeJobEmailsDedupesKeepsInvalid(t *testing.T) {
	got := normalizeJobEmails([]string{" A@x.com", "a@x.com", "bad", "", "b@y.com", "bad"})
	want := []string{"a@x.com", "bad", "b@y.com"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestJobViewProgress(t *testing.T) {
	j := &models.EmailVerifyJob{UUID: "u", Status: models.EmailVerifyJobRunning, Total: 3, Processed: 1, CreatedAt: time.Now()}
	v := toJobView(j, map[string]int64{"valid": 1, "pending": 2})
	if v.Progress != 33 || v.Counts["valid"] != 1 {
		t.Fatalf("view = %+v", v)
	}
	if _, has := v.Counts["pending"]; has {
		t.Fatal("pending must not appear in counts")
	}
	empty := toJobView(&models.EmailVerifyJob{Total: 0}, nil)
	if empty.Progress != 100 {
		t.Fatalf("empty job progress = %d", empty.Progress)
	}
}
```

- [ ] **Step 2: Run and see it fail**, then **implement**:
  - `normalizeJobEmails`: trim; minúsculas **só** quando `mail.ParseAddress` aceita; descarta vazios; dedup
    preservando a ordem.
  - `toJobView`, com `Progress = Processed*100/Total` e 100 quando `Total == 0`.
  - Handlers `CreateJob`, `GetJob`, `ListResults`, `ResultsCSV`, conforme o contrato. Na origem
    `subscriber_list_id`, resolva a lista com `FindByID` e confirme que `list.WorkspaceID` bate com o escopo
    (senão `404`). Lista dinâmica usa `FindByFilterRules(scope, list.FilterRules, -1, 0)`; estática usa
    `ListMembers(list.ID, -1, 0)`. Copie o padrão de `HandleCampaignStart`.
  - Idempotência: se `Idempotency-Key` já existe no escopo, responda `202` com o job existente, sem criar outro.
    Se o `Create` falhar por violação do índice único (corrida), busque de novo e devolva o existente.
  - Se `EnqueueVerifyJob` falhar, `MarkFailed(job.ID, "enqueue failed")` e `500`.

- [ ] **Step 3: Run and commit.** `GOTEST ./internal/handlers/` + build → PASS.
  Commit: `feat(verify): job endpoints (create, status, paginated results, CSV export)`

---

## Fase 4 — sonda SMTP

### Task 11: `Prober` SMTP com catch-all — modelo: **opus** (rede/concorrência)

**Files:**
- Create: `internal/services/verifier/smtpprobe.go`
- Create: `internal/services/verifier/smtpprobe_test.go`

**Interfaces:**
- Consumes: `SMTPVerdict` (Task 5)
- Produces:
  - `ProbeOptions{HeloName, MailFrom string; Timeout time.Duration; PerHost, MaxRcptPerSession int; Port string}`
  - `NewProber(opts ProbeOptions) *Prober`
  - `(*Prober).ProbeDomain(ctx, domain string, mxHosts, emails []string) (map[string]SMTPVerdict, bool /*catchAll*/)`
  - `classifyRcpt(err error) SMTPVerdict`

- [ ] **Step 1: Failing tests** com servidor go-smtp local:

```go
package verifier

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
)

type probeBackend struct {
	known    map[string]bool
	catchAll bool
	greylist bool
}

func (b *probeBackend) NewSession(_ *smtp.Conn) (smtp.Session, error) { return &probeSession{b: b}, nil }

type probeSession struct{ b *probeBackend }

func (s *probeSession) Mail(string, *smtp.MailOptions) error { return nil }
func (s *probeSession) Rcpt(to string, _ *smtp.RcptOptions) error {
	switch {
	case s.b.greylist:
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 7, 1}, Message: "greylisted"}
	case s.b.catchAll || s.b.known[strings.ToLower(to)]:
		return nil
	default:
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "no such user"}
	}
}
func (s *probeSession) Data(io.Reader) error { return nil }
func (s *probeSession) Reset()               {}
func (s *probeSession) Logout() error        { return nil }

func startProbeServer(t *testing.T, be *probeBackend) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := smtp.NewServer(be)
	srv.Domain = "mx.test"
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return port
}

func testProber(port string) *Prober {
	return NewProber(ProbeOptions{HeloName: "verify.test", MailFrom: "probe@verify.test",
		Timeout: 3 * time.Second, PerHost: 2, MaxRcptPerSession: 2, Port: port})
}

func TestProbeDeliverableAndUndeliverable(t *testing.T) {
	port := startProbeServer(t, &probeBackend{known: map[string]bool{"a@d.test": true, "c@d.test": true}})
	got, catchAll := testProber(port).ProbeDomain(context.Background(), "d.test", []string{"127.0.0.1"},
		[]string{"a@d.test", "b@d.test", "c@d.test"}) // 3 > MaxRcptPerSession: exercises RSET
	if catchAll {
		t.Fatal("not a catch-all")
	}
	want := map[string]SMTPVerdict{"a@d.test": SMTPDeliverable, "b@d.test": SMTPUndeliverable, "c@d.test": SMTPDeliverable}
	for e, v := range want {
		if got[e] != v {
			t.Errorf("%s = %s, want %s", e, got[e], v)
		}
	}
}

func TestProbeCatchAll(t *testing.T) {
	port := startProbeServer(t, &probeBackend{catchAll: true})
	got, catchAll := testProber(port).ProbeDomain(context.Background(), "d.test", []string{"127.0.0.1"}, []string{"x@d.test"})
	if !catchAll || got["x@d.test"] != SMTPAcceptAll {
		t.Fatalf("catchAll=%v got=%v", catchAll, got)
	}
}

func TestProbeGreylistIsUnknown(t *testing.T) {
	port := startProbeServer(t, &probeBackend{greylist: true})
	got, _ := testProber(port).ProbeDomain(context.Background(), "d.test", []string{"127.0.0.1"}, []string{"x@d.test"})
	if got["x@d.test"] != SMTPUnknown {
		t.Fatalf("got %v", got)
	}
}

func TestProbeUnreachableIsUnknown(t *testing.T) {
	got, _ := testProber("1").ProbeDomain(context.Background(), "d.test", []string{"127.0.0.1"}, []string{"x@d.test"})
	if got["x@d.test"] != SMTPUnknown {
		t.Fatalf("got %v", got)
	}
}

func TestClassifyRcpt(t *testing.T) {
	e := func(code int, a, b, c int) error {
		return &smtp.SMTPError{Code: code, EnhancedCode: smtp.EnhancedCode{a, b, c}}
	}
	cases := []struct {
		err  error
		want SMTPVerdict
	}{
		{nil, SMTPDeliverable},
		{e(550, 5, 1, 1), SMTPUndeliverable},
		{e(553, 5, 1, 3), SMTPUndeliverable},
		{e(552, 5, 2, 2), SMTPUnknown},
		{e(554, 5, 7, 1), SMTPUnknown},
		{e(450, 4, 2, 1), SMTPUnknown},
		{errors.New("broken pipe"), SMTPUnknown},
	}
	for _, c := range cases {
		if got := classifyRcpt(c.err); got != c.want {
			t.Errorf("classifyRcpt(%v) = %s, want %s", c.err, got, c.want)
		}
	}
}
```

  Confira a interface `smtp.Backend`/`smtp.Session` da v0.25 em
  `/go/pkg/mod/github.com/emersion/go-smtp@v0.25.0/backend.go` e ajuste as assinaturas do fake se preciso. O
  comportamento testado não muda.

- [ ] **Step 2: Run and see it fail.** `GOTEST ./internal/services/verifier/` → FAIL.

- [ ] **Step 3: Implement** (`smtpprobe.go`). Regras que a implementação precisa cumprir:
  - `ProbeDomain` tenta no máximo os 2 primeiros hosts de `mxHosts`, na ordem, removendo o ponto final dos nomes
    (`mx.d.test.` → `mx.d.test`). Se não conectar em nenhum, todos os endereços ficam `SMTPUnknown`.
  - Um semáforo por host (`sync.Map` de `chan struct{}` com capacidade `PerHost`) é adquirido antes de discar e
    solto ao fechar. `ctx` cancela a espera.
  - Discagem: `(&net.Dialer{Timeout: opts.Timeout}).DialContext(ctx, "tcp", net.JoinHostPort(host, port))`, depois
    `conn.SetDeadline(time.Now().Add(opts.Timeout * 3))` e `smtp.NewClient(conn)`. Em seguida `Hello(HeloName)` e
    `Mail(MailFrom, nil)`.
  - Detecção de catch-all: `Rcpt("posta-probe-<16 hex aleatórios de crypto/rand>@" + domain)`.
    - Aceito: devolve todos como `SMTPAcceptAll` e `catchAll = true`.
    - `4xx`: todos `SMTPUnknown`.
    - `5xx`: segue.
  - Para cada endereço, `Rcpt` classificado por `classifyRcpt`:
    - `nil` → `SMTPDeliverable`;
    - `*smtp.SMTPError` com `Code` 550, 551 ou 553, ou com `EnhancedCode[0] == 5 && EnhancedCode[1] == 1` →
      `SMTPUndeliverable`;
    - qualquer outro erro (4xx, 552, 554/5.7.x de política, rede) → `SMTPUnknown`.
  - A cada `MaxRcptPerSession` destinatários: `Reset()` e novo `Mail(...)`. Se o `Reset` ou o `Mail` falharem,
    os restantes ficam `SMTPUnknown`.
  - No fim, `Quit()` (ignora o erro) e `Close()`. Nunca envia `DATA`.
  - `NewProber` aplica os defaults: Port `"25"`, Timeout 10s, PerHost 2, MaxRcptPerSession 20.

- [ ] **Step 4: Run with race and commit.** Comando "Race detector" → PASS.
  Commit: `feat(verify): SMTP RCPT prober with catch-all detection and per-host limits`

---

### Task 12: ligar a sonda ao verificador + config — modelo: **sonnet**

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/services/verifier/verifier.go` (`Options`, `NewService`, `computeDomain`)
- Modify: `internal/services/verifier/config.go` (`FromConfig` repassa as opções da sonda)
- Modify: `internal/routes/auth_routes.go` (`Description` de `/emails/verify`)
- Create: `internal/services/verifier/probe_integration_test.go`

**Interfaces:**
- Consumes: `Prober`, `ProbeOptions` (Task 11); `buildResult`, `computeDomain`, `mxHosts` (Tasks 5 e 7);
  `countingResolver` (teste da Task 7)
- Produces:
  - `Options.SMTPEnabled bool`, `Options.SMTP ProbeOptions`
  - `(*Service).SetProber(p domainProber)`
  - Variáveis de ambiente:

    | Variável | Padrão |
    |---|---|
    | `POSTA_EMAIL_VERIFY_SMTP_ENABLED` | `false` |
    | `POSTA_EMAIL_VERIFY_SMTP_HELO` | host de `AppWebURL`, ou `localhost` |
    | `POSTA_EMAIL_VERIFY_SMTP_FROM` | `verify@<helo>` |
    | `POSTA_EMAIL_VERIFY_SMTP_TIMEOUT_SECONDS` | `10` |
    | `POSTA_EMAIL_VERIFY_SMTP_PER_HOST` | `2` |

- [ ] **Step 1: Failing test** (`probe_integration_test.go`):

```go
package verifier

import (
	"context"
	"testing"

	"github.com/goposta/posta/internal/storage/repositories"
)

type fakeProber struct {
	verdicts map[string]SMTPVerdict
	catchAll bool
	calls    int
}

func (f *fakeProber) ProbeDomain(_ context.Context, _ string, _ []string, emails []string) (map[string]SMTPVerdict, bool) {
	f.calls++
	out := make(map[string]SMTPVerdict, len(emails))
	for _, e := range emails {
		if f.catchAll {
			out[e] = SMTPAcceptAll
		} else {
			out[e] = f.verdicts[e]
		}
	}
	return out, f.catchAll
}

func TestProbeResultsFeedVerdict(t *testing.T) {
	s := NewService(nil, nil, nil, Options{Enabled: true, SMTPEnabled: true})
	s.SetResolver(&countingResolver{calls: map[string]int{}})
	p := &fakeProber{verdicts: map[string]SMTPVerdict{"a@x.com": SMTPDeliverable, "b@x.com": SMTPUndeliverable, "info@x.com": SMTPDeliverable}}
	s.SetProber(p)
	res, err := s.VerifyMany(context.Background(), repositories.ResourceScope{UserID: 1},
		[]string{"a@x.com", "b@x.com", "info@x.com"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Status != StatusValid || !res[0].MailboxVerified || res[0].Checks.SMTP != SMTPDeliverable {
		t.Errorf("a: %+v", res[0])
	}
	if res[1].Status != StatusInvalid || res[1].Reason != "mailbox does not exist" {
		t.Errorf("b: %+v", res[1])
	}
	if res[2].Status != StatusRisky {
		t.Errorf("role: %+v", res[2])
	}
	if p.calls != 1 {
		t.Errorf("one probe session per domain expected, got %d", p.calls)
	}
}

func TestCatchAllDomain(t *testing.T) {
	s := NewService(nil, nil, nil, Options{Enabled: true, SMTPEnabled: true})
	s.SetResolver(&countingResolver{calls: map[string]int{}})
	s.SetProber(&fakeProber{catchAll: true})
	res, _ := s.VerifyMany(context.Background(), repositories.ResourceScope{UserID: 1}, []string{"z@x.com"}, false)
	if res[0].Status != StatusAcceptAll {
		t.Fatalf("got %+v", res[0])
	}
}

func TestProbeDisabledSkips(t *testing.T) {
	s := NewService(nil, nil, nil, Options{Enabled: true})
	s.SetResolver(&countingResolver{calls: map[string]int{}})
	p := &fakeProber{}
	s.SetProber(p) // present but disabled by options
	res, _ := s.VerifyMany(context.Background(), repositories.ResourceScope{UserID: 1}, []string{"a@x.com"}, false)
	if p.calls != 0 || res[0].Checks.SMTP != SMTPSkipped {
		t.Fatalf("probe must not run when disabled: calls=%d smtp=%s", p.calls, res[0].Checks.SMTP)
	}
}
```

- [ ] **Step 2: Run and see it fail**, then **implement**:
  - Adicione `type domainProber interface { ProbeDomain(ctx context.Context, domain string, mxHosts, emails []string) (map[string]SMTPVerdict, bool) }`,
    o campo `prober domainProber` e `SetProber`. `NewService` cria `NewProber(opts.SMTP)` quando
    `opts.SMTPEnabled`.
  - Em `computeDomain`, guarde `hosts` retornado por `mxHosts`. Só quando
    `s.opts.SMTPEnabled && s.prober != nil && !disposable && outcome == dnsOK`:
    - consulte o cache `verify:catchall:<domain>` no Redis, se houver cliente;
    - valor `"1"`: todos os pendentes recebem `SMTPAcceptAll` sem sondar;
    - sem valor: chame `ProbeDomain(ctx, domain, hosts, pending)`. Se `catchAll`, grave `"1"` com TTL
      `s.opts.MXTTL`.
    - Passe o veredito de cada e-mail para `buildResult` (ausente = `SMTPUnknown`).
    - Sem sonda, o veredito continua `SMTPSkipped`.
    - `SMTPUnknown` não impede o cache do endereço: o `Status` sai do DNS, e só `StatusUnknown` bloqueia o cache.
      Mantenha esse comportamento.
  - Adicione os campos em `config.Config` (`EmailVerifySMTPEnabled`, `EmailVerifySMTPHelo`,
    `EmailVerifySMTPFrom`, `EmailVerifySMTPTimeoutSeconds`, `EmailVerifySMTPPerHost`) e a leitura com os helpers
    `goutils.Env*` no mesmo bloco de `config.go:265-268`. Use `net/url` para extrair o host de `AppWebURL`.
    `FromConfig` repassa `SMTPEnabled` e `SMTP: ProbeOptions{…}`.
  - Atualize a `Description` da rota `/emails/verify` em `auth_routes.go`: com
    `POSTA_EMAIL_VERIFY_SMTP_ENABLED=true`, o endereço também é sondado via SMTP RCPT e domínios catch-all são
    reportados como `accept_all`. Na documentação da rota, isso é mudança aditiva.

- [ ] **Step 3: Run and commit.** `GOTEST ./internal/...`, o comando "Race detector" e o build → PASS.
  Commit: `feat(verify): optional SMTP probing wired into verification (disabled by default)`

---

### Task 13: documentação — modelo: **sonnet**

**Files:**
- Modify: `docs/docs/email-sending/email-verification.md` (documentação pública, no idioma atual do arquivo)
- Create: `docs/verificacao-emails-brevo.md` (notas de operação em português, no estilo de
  `docs/setup-local-athenas.md`)

**Conteúdo obrigatório:**

1. Tabela de `status` com o significado de cada valor e a política de envio da spec
   (§"Política de envio").
2. Endpoints com exemplo real de request e response:
   - `/emails/verify`
   - `/emails/verify/batch`
   - os 4 de jobs
   - `/webhooks/brevo`
   - `/workspaces/current/suppressions/import/brevo`
3. **Configuração do webhook na Brevo**, via API:

```bash
curl -X POST https://api.brevo.com/v3/webhooks \
  -H "api-key: $BREVO_API_KEY" -H "Content-Type: application/json" \
  -d '{
    "url": "https://SEU-POSTA/api/v1/webhooks/brevo",
    "description": "Posta — bounces e reclamações",
    "type": "transactional",
    "events": ["hardBounce","softBounce","blocked","invalid","spam","unsubscribed"],
    "batched": true,
    "headers": [{"key": "Authorization", "value": "Bearer psk_SUA_CHAVE_POSTA"}]
  }'
```

   Registre que os nomes das propriedades de `headers` (`key`/`value`) seguem a referência "Create a webhook" da
   Brevo, consultada em 2026-09-28. Se a Brevo recusar o formato, a alternativa é o objeto `auth` do mesmo
   endpoint.
4. Primeira carga: rodar a importação de bloqueados até `next_offset` ser `null`.
5. Variáveis de ambiente novas: `POSTA_EMAIL_VERIFY_CONCURRENCY` e as `POSTA_EMAIL_VERIFY_SMTP_*`.
6. Requisitos da sonda SMTP: porta 25 de saída (costuma estar bloqueada em nuvem e WSL), IP diferente do IP de
   envio com rDNS e HELO coerente. A sonda nunca envia `DATA`. Sonda em volume pode levar a bloqueio pelos
   provedores.
7. Limites: 100 no síncrono, 50.000 por job, 2 jobs ativos por workspace e o rate limit por hora.

- [ ] **Step 1:** Escrever os dois arquivos.
- [ ] **Step 2:** Rodar `grep -n "emails/verify\|webhooks/brevo\|import/brevo" internal/routes/*.go` e conferir
  que cada path documentado existe exatamente assim.
- [ ] **Step 3: Commit** — `docs(verify): bulk verification, Brevo webhook and SMTP probe`

---

## Revisão final (controlador)

- Review do branch inteiro (`git diff chore/athenas-redis...feat/email-verify`) por um revisor **opus**, com esta
  lista de verificação:
  - escopo de tenant em toda query nova;
  - compatibilidade aditiva de `/emails/verify` e `/webhooks/bounce`;
  - `-race` verde;
  - nenhum `DATA` na sonda;
  - chave da Brevo nunca persistida nem logada (`grep -rn "api_key\|APIKey" internal/handlers/brevo_import_handler.go`
    e logs).
- Suíte completa com banco + build/vet/gofmt.
