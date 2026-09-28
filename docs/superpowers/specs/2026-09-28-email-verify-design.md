# Validação de e-mails e ciclo de bounce com a Brevo — design

Data: 2026-09-28 · Branch: `feat/email-verify` (a partir de `chore/athenas-redis`)

## Problema

Listas validadas no EmailVerify continuam gerando hard bounces na Brevo. A análise do código mostrou quatro causas:

1. A Brevo, como relay SMTP, aceita qualquer `RCPT TO`. O bounce acontece depois, dentro da Brevo, e por isso o
   `permanentRejection` do worker (`internal/worker/handler.go:372`) nunca dispara. O Posta não fica sabendo.
2. O webhook genérico `POST /api/v1/webhooks/bounce` (`internal/handlers/bounce_webhook_handler.go`) não grava
   `bounces` nem `suppressions`, e marca como `bounced` subscribers de **todos os tenants** (`FindAllByEmail`).
   O payload também não é o formato da Brevo.
3. O disparo de campanha (`internal/worker/campaign_processor.go:116`) só respeita o status do subscriber e os
   opt-outs por lista. A tabela global `suppressions` é ignorada.
4. O validador atual (`internal/services/verifier`):
   - erro temporário de DNS vira `invalid` e fica 24h em cache como `"none"`;
   - null MX (RFC 7505, `0 .`) é tratado como MX válido (confirmado: o Go devolve `["."]` para `example.com`);
   - a lista de descartáveis tem só 30 domínios;
   - não há sonda SMTP;
   - não há validação em lote.

## Objetivos

| Fase | Entrega |
|---|---|
| 1 | Fechar o ciclo com a Brevo: webhook nativo, importação de bloqueados, supressão respeitada nas campanhas, webhook genérico gravando e escopado |
| 2 | Validador correto: DNS temporário = `unknown`, null MX = `invalid`, lista de descartáveis completa, sugestão de typo |
| 3 | Validação em massa: síncrona (≤ 100) e assíncrona por job (≤ 50.000), agrupada por domínio |
| 4 | Sonda SMTP opcional (desligada por padrão), com detecção de catch-all |

Fora do escopo: upload de CSV (o cliente envia JSON; o resultado sai em CSV), sinal de engajamento (o Posta não
guarda abertura por contato) e persistência da chave da Brevo.

## Decisões de contrato

- **Veredito de negócio sai com `200` e o campo `status`.** Um e-mail inválido dentro de um lote é resultado, não
  erro. `400`/`422` ficam para requisição malformada, `413` para lote acima do limite, `429` para limite de taxa e
  `404` para validação desligada ou job inexistente.
- `status`: `valid | invalid | risky | accept_all | disposable | unknown`. `accept_all` é novo, uma mudança
  aditiva.
- `checks.smtp`: `skipped | deliverable | undeliverable | accept_all | unknown`.
- Campo novo e opcional `suggestion` (ex.: `joao@gmail.com` para `joao@gmial.com`).
- Mudanças só aditivas em `POST /api/v1/emails/verify`.

## Fase 1 — ciclo com a Brevo

- **Correlação.** Todo e-mail enviado recebe o header `X-Mailin-custom: posta-id=<uuid>`. A Brevo devolve esse
  valor no webhook, o que liga o evento ao registro `emails`.
- **Serviço compartilhado `internal/services/bounceingest`.** `Ingestor.Record(scope, Event)`:
  - só aceita o e-mail do UUID se ele pertencer ao escopo do chamador;
  - grava `Bounce` quando o e-mail é encontrado (`email_id` é NOT NULL);
  - faz upsert em `Suppression` (`kind` = `bounce` / `complaint` / `hard`) para hard bounce, reclamação e
    descadastro;
  - marca o subscriber do escopo como `bounced` ou `complained`;
  - marca `bounced_at` na mensagem da campanha.
- **`POST /api/v1/webhooks/brevo`** (API key no header `Authorization`, configurado no campo `auth`/`headers` do
  webhook da Brevo). Aceita um objeto ou um array (modo `batched`). Mapeamento dos eventos:

  | Evento Brevo | Ação |
  |---|---|
  | `hard_bounce`, `invalid_email`, `blocked` | hard bounce + supressão `bounce` |
  | `spam` | reclamação + supressão `complaint` |
  | `unsubscribed`, `unsubscribe` | supressão `hard` |
  | `soft_bounce` | bounce soft, sem supressão |
  | demais | `ignored` |

  Responde `200` sempre que o JSON é válido, para a Brevo não reenviar.
- **Webhook genérico** passa a usar o Ingestor, restrito ao escopo da API key.
- **`POST /api/v1/workspaces/current/suppressions/import/brevo`**: recebe `{api_key, start_date?, end_date?,
  offset?, max_pages?}`, pagina `GET https://api.brevo.com/v3/smtp/blockedContacts` (100 por página, máximo de 50
  páginas por chamada) e faz upsert das supressões. Devolve `next_offset` (`null` quando termina). A chave não é
  gravada. Mapeamento de `reason.code`:

  | `reason.code` | `kind` |
  |---|---|
  | `hardBounce` | `bounce` |
  | `contactFlaggedAsSpam` | `complaint` |
  | demais | `hard` |
- **Campanhas.** `HandleCampaignStart` descarta assinantes com supressão global no escopo da campanha.

## Fase 2 — validador correto

- A consulta de DNS distingue três resultados:

  | Resultado | Quando | Veredito |
  |---|---|---|
  | `ok` | há MX ou A/AAAA | segue a validação |
  | `nomail` | NXDOMAIN, sem registros ou null MX | `invalid`, com cache |
  | `temperr` | timeout ou SERVFAIL | `unknown`, sem cache |
- A lista de descartáveis vem de `disposable-email-domains` (CC0), embutida com `go:embed` e casada também por
  domínio pai (`x.mailinator.com`).
- A sugestão de typo compara contra provedores populares (inclusive BR), com Levenshtein ≤ 2.

## Fase 3 — em massa

- `Service.VerifyMany(ctx, scope, emails, fresh)`:
  - normaliza e deduplica;
  - aplica o limite de taxa por endereço (`INCRBY`);
  - carrega supressões e bounces com 1 consulta cada;
  - agrupa por domínio, com 1 consulta de MX por domínio;
  - processa com um pool de concorrência;
  - devolve os resultados na ordem da entrada.

  `Verify` (individual) passa a delegar para `VerifyMany`.
- `POST /api/v1/emails/verify/batch`: até 100 e-mails, síncrono.
- Jobs: tabelas `email_verify_jobs` e `email_verify_items`, task asynq `verify:job` retomável, processada em blocos
  de 500.
  - `POST /api/v1/emails/verify/jobs`: `{emails[] | subscriber_list_id, fresh?, apply?}`, com header opcional
    `Idempotency-Key`. Responde `202`.
  - `GET /api/v1/emails/verify/jobs/{id}`: progresso e totais por status.
  - `GET /api/v1/emails/verify/jobs/{id}/results?status=&page=&size=`: resultados paginados.
  - `GET /api/v1/emails/verify/jobs/{id}/results.csv`: exportação em CSV.
  - `apply=true`: ao concluir, suprime (`kind=manual`) os endereços `invalid` e `disposable`.

## Fase 4 — sonda SMTP (opcional)

- `POSTA_EMAIL_VERIFY_SMTP_ENABLED=false` por padrão.
- Por domínio, uma sessão com o MX de maior prioridade: `EHLO` → `MAIL FROM` → `RCPT` de um endereço aleatório
  (detecção de catch-all) → até 20 `RCPT` reais → `QUIT`. Nunca envia `DATA`.
- Classificação das respostas:

  | Resposta | Classificação |
  |---|---|
  | `250`/`251` | `deliverable` |
  | `550`/`551`/`553` com código enhanced `5.1.x` | `undeliverable` |
  | `4xx`, timeout ou recusa | `unknown` |
  | aleatório aceito | todo o domínio vira `accept_all` |
- Semáforo por host MX, timeout configurável e cache do catch-all por domínio no Redis.
- Requisitos operacionais: saída na porta 25 e IP **diferente** do IP de envio, com rDNS.

## Política de envio (documentar)

| Status | O que fazer |
|---|---|
| `valid` | enviar |
| `accept_all`, `risky`, `unknown` | lotes pequenos, monitorando a taxa de bounce |
| `invalid`, `disposable`, suprimidos | nunca enviar |
