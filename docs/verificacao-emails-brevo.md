# Verificação de e-mails e ciclo de bounce com a Brevo

Notas de operação do fork local do Posta (`/devathenas/docker/posta`), escritas em 2026-09-28,
sobre a branch `feat/email-verify`: fechamento do ciclo de bounce com a Brevo (webhook nativo +
importação de bloqueados), o novo validador (verificação em lote/job, sonda SMTP opcional) e as
mudanças de comportamento na supressão/bounce por tenant. Para a referência de request/response
da API pública, veja `docs/docs/email-sending/email-verification.md`.

## 1. Ao fazer deploy desta branch: limpar o cache do validador no Redis

O validador antigo cacheava erro temporário de DNS como `"invalid"` por até 24h (chave
`verify:mx:*`) e o resultado de endereço por até 7 dias (chave `verify:addr:*`, TTL de
`POSTA_EMAIL_VERIFY_CACHE_TTL_HOURS`, padrão 168h). Entradas gravadas pelo código antigo continuam
servindo esse veredito errado até expirar, mesmo depois do deploy da correção. Antes de considerar
o deploy concluído, apague as duas famílias de chave:

```bash
docker exec athenas_redis redis-cli --scan --pattern 'verify:mx:*' | xargs -r docker exec -i athenas_redis redis-cli del
docker exec athenas_redis redis-cli --scan --pattern 'verify:addr:*' | xargs -r docker exec -i athenas_redis redis-cli del
```

No ambiente local do Athenas o Redis é o container `athenas_redis` (ver
`docs/setup-local-athenas.md`), por isso os comandos rodam dentro dele via `docker exec` em vez de
um `redis-cli` no host. Isso não afeta o cache de catch-all (`verify:catchall:*`, só existe quando
a sonda SMTP está ligada) — pode ficar, ele já nasceu com o código novo.

## 2. Mudança de comportamento: histórico de bounce agora é por workspace

Antes, um endereço que tinha hard-bounced em **qualquer** tenant era marcado como `invalid` para
todo mundo que tentasse verificá-lo. Agora a consulta de bounce que alimenta a verificação é
escopada ao workspace de quem chama (`repositories.ApplyScope`): um bounce registrado no workspace
A não aparece mais como `previously_bounced` numa verificação feita pelo workspace B. Isso é
esperado — não é regressão — mas muda o resultado de endereços que antes só passavam por serem
"limpos" no tenant local.

## 3. Webhook genérico `/webhooks/bounce` também passou a ser escopado

`POST /api/v1/webhooks/bounce` (autenticado por API key) agora grava o bounce e a supressão **só
no workspace da chave que chamou** o webhook. Antes, `FindAllByEmail` marcava o subscriber
correspondente em **todos os tenants** que tivessem aquele e-mail cadastrado — ou seja, o bounce de
um cliente vazava para a lista de outro. Se você tem alguma integração externa que dependia do
comportamento antigo (marcar o e-mail em todo tenant), ela vai parar de fazer isso; ajuste a
chamada para usar a chave de API do workspace correto, ou prefira o webhook nativo da Brevo (item 5)
quando a origem for a própria Brevo.

## 4. Correlação: `X-Mailin-custom: posta-id=<uuid>`

Todo e-mail enviado pelo Posta agora carrega o header `X-Mailin-custom: posta-id=<uuid do registro
em emails>`. A Brevo devolve esse valor de volta no payload do webhook (campo
`X-Mailin-custom`), o que permite ligar o evento de bounce ao e-mail específico que foi enviado —
sem isso, o ingestor ainda funciona por endereço, mas não consegue marcar `bounced_at` na
mensagem da campanha nem confirmar que o e-mail pertence ao escopo de quem chamou o webhook.

## 5. Configurar o webhook na Brevo (via API)

```
POST /api/v1/webhooks/brevo
```

Aceita um evento único ou, no modo `batched` da Brevo, um array. Sempre responde `200` quando o
JSON é válido — inclusive para eventos ignorados — para a Brevo não reenviar. Autenticação: API key
do Posta no header `Authorization`.

Mapeamento de eventos:

| Evento Brevo | Ação no Posta |
|---|---|
| `hard_bounce`, `invalid_email`, `blocked` | bounce + supressão `bounce` |
| `spam` | reclamação + supressão `complaint` |
| `unsubscribed`, `unsubscribe` | supressão `hard` |
| `soft_bounce` | bounce soft, sem supressão |
| outros | ignorado |

Exemplo de payload recebido (modo `batched`) e da resposta:

```json
[
  {
    "event": "hard_bounce",
    "email": "bounced@example.com",
    "reason": "550 5.1.1 mailbox not found",
    "X-Mailin-custom": "posta-id=3d6f6b0a-6f3a-4e2e-9c3e-1f0b7a9c9e11",
    "message-id": "<202609281200.xxxxx@relay.brevo.com>"
  }
]
```

```json
{
  "success": true,
  "data": {
    "received": 1,
    "processed": 1,
    "ignored": 0,
    "items": [
      {
        "email": "bounced@example.com",
        "kind": "hard",
        "action": "suppressed",
        "bounce_recorded": true,
        "suppressed": true,
        "subscriber_updated": true,
        "message_marked": true
      }
    ]
  }
}
```

### Cadastrar o webhook na Brevo

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

Os nomes das propriedades de `headers` (`key`/`value`) seguem a referência "Create a webhook" da
documentação da API da Brevo, consultada em 2026-09-28. Se a Brevo recusar esse formato para a sua
conta/versão de API, a alternativa é o objeto `auth` do mesmo endpoint (`POST /v3/webhooks`), que
também aceita cabeçalho de autenticação — consulte a referência atual da Brevo antes de usar, pois
o formato de `auth` pode variar entre versões.

A chave `psk_SUA_CHAVE_POSTA` é uma API key do Posta com escopo que permita chamar o webhook (o
grupo de rotas de webhook do Posta exige apenas API key válida, sem escopo específico); gere uma
key dedicada para esse uso em vez de reaproveitar uma key de envio.

## 6. Primeira carga: importar bloqueados da Brevo

```
POST /api/v1/workspaces/current/suppressions/import/brevo
```

Autenticação: sessão (JWT + `X-Posta-Workspace-Id`) ou API key de workspace com permissão de
edição (`requireEdit`). Pagina `GET https://api.brevo.com/v3/smtp/blockedContacts` (100 contatos
por página, até `max_pages` páginas — padrão 20, máximo 50 por chamada) e faz upsert das
supressões no workspace atual. **A chave da Brevo (`api_key`) nunca é gravada nem logada.**

Mapeamento de `reason.code` → `kind` da supressão:

| `reason.code` | `kind` |
|---|---|
| `hardBounce` | `bounce` |
| `contactFlaggedAsSpam` | `complaint` |
| outros | `hard` |

Para a primeira carga, chame o endpoint repetidamente passando o `next_offset` devolvido, até ele
vir `null` — só então a importação terminou:

```bash
curl -X POST http://localhost:9000/api/v1/workspaces/current/suppressions/import/brevo \
  -H "Authorization: Bearer <jwt>" \
  -H "X-Posta-Workspace-Id: 1" \
  -H "Content-Type: application/json" \
  -d '{
    "api_key": "xkeysib-...",
    "max_pages": 50
  }'
```

```json
{
  "success": true,
  "data": {
    "fetched": 5000,
    "imported": 4998,
    "total": 12340,
    "next_offset": 5000
  }
}
```

Chamada seguinte, passando `"offset": 5000` (e assim por diante, até `next_offset` vir `null`):

```bash
curl -X POST http://localhost:9000/api/v1/workspaces/current/suppressions/import/brevo \
  -H "Authorization: Bearer <jwt>" \
  -H "X-Posta-Workspace-Id: 1" \
  -H "Content-Type: application/json" \
  -d '{
    "api_key": "xkeysib-...",
    "offset": 5000,
    "max_pages": 50
  }'
```

```json
{
  "success": true,
  "data": {
    "fetched": 340,
    "imported": 340,
    "total": 12340,
    "next_offset": null
  }
}
```

`start_date`/`end_date` (formato `YYYY-MM-DD`, os dois juntos ou nenhum) restringem o período dos
bloqueados retornados pela Brevo; sem eles, importa o histórico todo.

## 7. Verificação em massa (síncrona e por job)

- `POST /api/v1/emails/verify/batch`: até **100** endereços, síncrono.
- `POST /api/v1/emails/verify/jobs`: até **50.000** endereços por job, assíncrono, no máximo
  **2 jobs ativos** (`queued`/`running`) por workspace ao mesmo tempo. `apply=true` suprime
  (`kind=manual`) os endereços que saírem `invalid` ou `disposable` quando o job terminar.
- As duas rotas, mais os 4 endpoints de job (criar, status, listar resultados paginados, exportar
  CSV), estão documentadas com exemplo completo de request/response em
  `docs/docs/email-sending/email-verification.md`.
- Além do limite por chamada, `POSTA_EMAIL_VERIFY_RATE_HOURLY` (padrão 1000) limita quantas
  verificações um usuário pode disparar por hora, somando síncrono, lote e itens de job.

## 8. Sonda SMTP opcional — requisitos operacionais

Desligada por padrão (`POSTA_EMAIL_VERIFY_SMTP_ENABLED=false`). Quando ligada, a sonda abre uma
sessão SMTP real com o MX de maior prioridade do domínio (`EHLO` → `MAIL FROM` → `RCPT` de um
endereço aleatório, para detectar catch-all → `RCPT` dos endereços reais → `QUIT`) e **nunca envia
`DATA`** — nenhuma mensagem é transmitida durante a sonda.

Requisitos para rodar em produção:

- **Porta 25 de saída liberada.** Em VM de nuvem e no WSL essa porta costuma vir bloqueada pelo
  provedor/host; sem ela a sonda não conecta e o resultado degrada para `checks.smtp = "unknown"`
  silenciosamente (sem erro visível na resposta).
- **IP diferente do IP usado para enviar e-mail**, com rDNS (PTR) coerente com o HELO configurado
  em `POSTA_EMAIL_VERIFY_SMTP_HELO`. Sondar a partir do próprio IP de envio arrisca fazer esse IP
  ser sinalizado por provedores destinatários como tráfego suspeito (RCPT sem DATA em série).
- **Cuidado com volume.** Sondar muitos endereços em sequência se parece com enumeração de conta
  para alguns provedores, e pode levar ao bloqueio temporário do IP sondador. `max_pages`/lote e
  `POSTA_EMAIL_VERIFY_SMTP_PER_HOST` (padrão 2, concorrência por host MX) existem para conter isso
  — não force esses limites para cima sem necessidade.

O resultado de catch-all fica em cache por domínio no Redis (`verify:catchall:*`, TTL igual ao de
MX, `POSTA_EMAIL_VERIFY_MX_CACHE_TTL_HOURS`) — uma vez detectado catch-all, os endereços seguintes
daquele domínio saem como `accept_all` sem nova sonda, até a chave expirar.

## 9. Variáveis de ambiente novas

| Variável | Padrão | Descrição |
|---|---|---|
| `POSTA_EMAIL_VERIFY_CONCURRENCY` | `16` | Número máximo de domínios resolvidos/sondados em paralelo por um lote ou job. |
| `POSTA_EMAIL_VERIFY_SMTP_ENABLED` | `false` | Liga a sonda SMTP RCPT. |
| `POSTA_EMAIL_VERIFY_SMTP_HELO` | derivado de `POSTA_WEB_URL` | Nome anunciado em `EHLO`/`HELO`; deve bater com o rDNS do IP sondador. |
| `POSTA_EMAIL_VERIFY_SMTP_FROM` | `verify@<helo>` | Remetente de envelope usado em `MAIL FROM`. |
| `POSTA_EMAIL_VERIFY_SMTP_TIMEOUT_SECONDS` | `10` | Timeout do dial e de cada comando SMTP (a sessão toda fica limitada a ~3× esse valor). |
| `POSTA_EMAIL_VERIFY_SMTP_PER_HOST` | `2` | Sessões de sonda concorrentes permitidas por host MX. |

## Como verificar

```bash
# Cache limpo após o deploy
docker exec athenas_redis redis-cli --scan --pattern 'verify:mx:*' | wc -l    # deve dar 0
docker exec athenas_redis redis-cli --scan --pattern 'verify:addr:*' | wc -l  # deve dar 0

# Webhook da Brevo cadastrado
curl -s https://api.brevo.com/v3/webhooks -H "api-key: $BREVO_API_KEY" | grep -o '"url":"[^"]*brevo[^"]*"'

# Importação repetida até next_offset == null (ver item 6)
```
