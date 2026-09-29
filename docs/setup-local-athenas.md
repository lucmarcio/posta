# Posta local no ambiente Athenas

Notas de operação do fork local do Posta (`/devathenas/docker/posta`), escritas em 2026-09-25 e
ampliadas em 2026-09-28. Cobre a integração com o Redis compartilhado do Athenas, dois bugs do
upstream corrigidos aqui, a configuração de DNS para verificar domínios que já têm SPF (§1–5) e a
operação do dia a dia: e-mails da plataforma, convites, acesso de admin, autenticação da API
pública e collection do Postman (§6–10).

Branch: `chore/athenas-redis` (commits `e699539`, `d14cfe7`, `a812917`; enviada para `origin` em 2026-09-28 junto com a validação de e-mails — ver `docs/verificacao-emails-brevo.md`).

## 1. Redis externo (`athenas_redis`)

**Sintoma:** `dial tcp: lookup athenas_redis on 127.0.0.11:53: no such host`.

**Causa:** o container `athenas_redis` (Bitnami Redis 7, `ALLOW_EMPTY_PASSWORD=yes`) está na
rede Docker `athnet`, e o compose do Posta cria só a rede `posta_default`. Container só
resolve o nome de outro pela DNS do Docker quando os dois estão na mesma rede.

**Solução** (`compose.yml`):

- `posta` e `worker` entram nas redes `default` (para acessar o `posta-db`) e `athnet`;
- `athnet` é declarada como `external: true` no nível raiz;
- o serviço `posta-redis` fica comentado e os `depends_on` do `athenas_redis` também.
  `depends_on` não funciona entre arquivos compose diferentes.

**Pré-requisito:** o stack do Athenas precisa estar de pé antes do `docker compose up`,
porque senão a rede `athnet` não existe.

### Worker exige `POSTA_ENV`

Sem `POSTA_ENV`, o worker assume ambiente de produção e aborta com
`worker configuration is not usable: POSTA_JWT_SECRET is the published placeholder value`.
O serviço `worker` precisa de `POSTA_ENV: "dev"`, igual ao `posta`.

## 2. Bug: Host / Name vazio nos registros DNS (upstream)

O backend serializa `DNSRecord` com `json:"host"` (`internal/services/domain/verifier.go:36-40`),
mas o frontend lia `record.name`. Com isso, o campo Host / Name aparecia vazio e o botão
"Copy all" saía com `undefined`.

**Correção (`d14cfe7`):** `name` → `host` em

- `web/src/api/types.ts` (interface `DnsRecord`)
- `web/src/views/domains/Domains.vue`
- `web/src/views/admin/DomainDetail.vue:243`

## 3. Bug: loop de restart em binário `dev` (upstream)

**Sintoma:** no segundo start, o servidor cai com
`upgrade: bootstrap fresh install: ERROR: duplicate key value violates unique constraint "upgrade_steps_pkey"`.

**Causa** (`internal/storage/migration/upgrade/upgrade.go`, `runLocked`):

1. `readVersion` não encontra `app.version` em `settings` e marca o boot como `fresh`;
2. `markAllApplied` insere todos os passos do registry em `upgrade_steps`;
3. `writeVersion` é pulado quando `IsDev(binaryVersion)`, então o `app.version` nunca é gravado;
4. no próximo boot, o banco parece novo de novo e o INSERT duplica.

Qualquer imagem compilada sem versão (`version=dev`) é afetada.

**Correção (`a812917`):** `markAllApplied` usa `clause.OnConflict{DoNothing: true}`.
O log `settings.go:18 record not found` continua aparecendo a cada boot, e é esperado.

## 4. Valores de SPF e DKIM do Posta são placeholders

`RequiredRecords` (`internal/services/domain/verifier.go:42-65`) devolve valores fixos no
código, e a documentação oficial (`docs/docs/smtp-domains/domain-verification.md`) repete os mesmos:

- SPF: `v=spf1 include:_spf.posta ~all`, sendo que `_spf.posta` não existe;
- DKIM: CNAME `posta._domainkey.<domínio>` → `posta._domainkey.posta`, que também não existe.

Não há variável de ambiente para configurar esses valores. **Não publique esses registros.**
SPF e DKIM devem vir do provedor SMTP que realmente envia (o cadastrado em *SMTP Servers*).

Além disso, a verificação é fraca: `Verify` marca SPF como ok se existir **qualquer**
`v=spf1` no domínio, e DKIM como ok se existir qualquer TXT ou CNAME em `posta._domainkey`.
O "✓" do SPF não confirma que o Posta está autorizado.

## 5. Verificar a posse de um domínio que já tem TXT/SPF (Route 53)

O Posta usa `net.LookupTXT(domínio)` e procura `posta-verification=<token>` entre **todos** os
valores. No Route 53, cada nome e tipo tem um único conjunto de registros com vários valores:

1. edite o TXT existente do domínio raiz (não crie outro, porque o Route 53 recusa);
2. acrescente uma linha: `"posta-verification=<token>"`;
3. mantenha o `v=spf1 ...` existente, que deve ser um só por domínio;
4. depois do TTL, clique em **Re-check** no Posta.

DMARC: qualquer `v=DMARC1` já existente em `_dmarc.<domínio>` é aceito, então não precisa substituir.

## 6. E-mails da plataforma (convite, reset de senha, alertas) exigem SMTP do sistema

**Sintoma (2026-09-28):** o convite para o workspace era criado (`POST .../invitations` → 201,
linha `pending` em `workspace_invitations`), mas o e-mail nunca chegava. Não aparecia nenhum erro no log.

**Causa:** os e-mails da própria plataforma usam o **SMTP do sistema** (`POSTA_SYSTEM_SMTP_*`), que
é separado dos SMTP servers dos workspaces. Sem host e remetente configurados
(`SystemSMTPConfig.IsConfigured`, `internal/config/config.go:168`), `notification.Service.Send`
(`internal/services/notification/service.go:125-129`) **pula o envio e retorna `nil`**, com log só em
nível Debug. Além disso, o handler do convite descarta o erro de envio
(`_ = h.notifier.Send(...)`, `internal/handlers/workspace_handler.go:525`). O resultado é que nada falha visivelmente.
Havia duas camadas de problema: o `compose.yml` não repassava nenhuma `POSTA_SYSTEM_SMTP_*` ao container, e o `.env` só
tinha o placeholder `smtp.example.com`.

**Solução (`00b2511`):**
- o `compose.yml` interpola `POSTA_SYSTEM_SMTP_*` a partir do `.env`, que está no `.gitignore`, então nenhum segredo fica no compose;
- no `.env`, o SMTP do sistema aponta para o Zoho: `smtppro.zoho.com:587`, starttls, usuário e
  remetente `mkt@sobdemanda.net`, o mesmo servidor do workspace 1.

**Como verificar:** no boot, o log deve mostrar `system notification service enabled` e
`system workspace: provisioned SMTP server`. Depois, recrie um convite e confira a caixa do destinatário.
Não existe endpoint de reenvio: para reenviar, apague o convite pendente e crie de novo.

## 7. Aceitar convite: a conta precisa existir e ter o mesmo e-mail

- O aceite exige que o usuário **logado** tenha o mesmo e-mail do convite
  (`workspace_handler.go:648`, `:712`, `:762`). Abrir o link no navegador logado como outro usuário dá
  **403 "invitation is for a different email address"**, e a tela mostra só um erro genérico.
- O cadastro público está desligado (`settings.registration_enabled = false`). Então o convidado sem
  conta não consegue se cadastrar sozinho. O caminho é o admin criar a conta (Admin → Users,
  `AdminHandler.CreateUser`, `internal/handlers/admin_handler.go:169`) e o convidado aceitar numa
  janela anônima.
- A conta criada pelo admin nasce com `email_verified_at = NULL`. Com
  `require_email_verification = true`, ações sensíveis ficam bloqueadas com "email address is not verified"
  (`internal/middlewares/middleware.go:356`). Se o e-mail já estiver comprovado, marque direto no banco:
  `update users set email_verified_at = now() where email = '...';`.
- Em 2026-09-28, o aceite pelo link do e-mail devolveu 404, mas o aceite pela lista de convites
  pendentes dentro do app (aceite por ID) funcionou. A causa do 404 não foi investigada.

## 8. Acesso de admin: seed, reset de senha e bloqueio de login

- `POSTA_ADMIN_EMAIL` e `POSTA_ADMIN_PASSWORD` só valem no **primeiro boot**: `SeedAdmin`
  (`internal/storage/postgres.go:33`) não faz nada se a tabela `users` já tiver alguém. Mudar essas
  variáveis e reiniciar **não** troca a senha.
- **Reset manual da senha** (o "esqueci a senha" não serve para `admin@example.com`, que não é uma caixa
  real): gere um hash bcrypt fora do Go, porque não há Go no host, e grave em `users.password_hash`.
  Passe o SQL pela entrada padrão, porque o `$` do hash seria expandido dentro de aspas duplas no shell:
  ```bash
  HASH=$(docker run --rm -e PW="$NOVA" python:3-alpine sh -c 'pip -q install bcrypt >/dev/null 2>&1 && python -c "import bcrypt,os;print(bcrypt.hashpw(os.environ[\"PW\"].encode(), bcrypt.gensalt(10)).decode())"')
  printf "update users set password_hash='%s' where email='admin@example.com';\n" "$HASH" \
    | docker exec -i posta-posta-db-1 sh -c 'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1'
  ```
  A tabela `users` **não** tem coluna `updated_at`.
- **429 "too many login attempts":** o limite padrão é de 10 tentativas por IP em 15 minutos, guardado no
  Redis compartilhado (`ratelimit:login:<ip>:<bucket>`, `internal/services/ratelimit/redis_limiter.go:158`,
  db0). Navegador e testes locais saem pelo gateway do Docker (`172.18.0.1`) e dividem o mesmo contador.
  Para zerar, apague **só** essa chave:
  `docker exec athenas_redis redis-cli -n 0 --scan --pattern 'ratelimit:login:*'` e depois `del <chave>`.
  Nunca faça `FLUSHDB`: o Redis é do Athenas.

## 9. API pública (`/api/v1/emails/*`, webhooks, leitura) aceita só API key

As rotas do grupo `apiAuth` usam `APIKeyAuth` (`internal/middlewares/middleware.go:184`). Elas aceitam
**apenas** `Authorization: Bearer psk_…`. O JWT do login e o cookie `posta_session` do navegador são
recusados com 401 ("missing Authorization header" ou "expected: Bearer <API_KEY>"). Isso é intencional,
não é bug. Exemplo: `POST /api/v1/emails/verify/batch` (até 100 e-mails) precisa de uma API key com escopo `send`,
criada em Settings → API Keys. As rotas do painel (`/workspaces/current/*`, `/users/me/*`, `/admin/*`)
usam o JWT.

## 10. Collection do Postman

`docs/postman/posta-api.postman_collection.json` (`4790d78`) foi gerada do `/openapi.json` com
`openapi-to-postmanv2`. Tem 278 requisições em 22 pastas. O conversor gera uma cópia da operação por tag,
então as duplicatas foram removidas, mantendo a pasta mais específica. Também está no Postman, na collection
"Posta API" do workspace *sobdemanda*. O arquivo do repositório pode estar atrás da versão do Postman:
depois do commit, o usuário separou a variável da API key à mão, só no Postman.

- **Auth → Login** grava o JWT em `{{token}}`. Um script *pre-request* da collection envia `{{workspaceId}}`
  como `X-Posta-Workspace-Id`, porque o conversor só tinha colocado esse header em 21 requisições.
- Para as rotas da §9, é preciso usar a API key em vez do JWT.
- As 14 rotas escondidas da documentação ficaram de fora: streams SSE, pixels e links de rastreio `/t/*`,
  callback OAuth e `/metrics`.
- **Atualizar:** exporte de novo o `/openapi.json`, converta, e envie com `PUT
  https://api.getpostman.com/collections/524246-d5ecb5c6-9659-489d-b140-a3b1d2c1c945` e o header
  `X-Api-Key`. A conexão OAuth do MCP do Postman não aceita a collection completa (cerca de 215 KB) numa chamada só.

## Como verificar (§1–3)

```bash
docker compose up -d --build posta worker
docker compose logs posta worker | grep -E 'redis connected|Server started|worker started'
docker compose restart posta && docker compose ps posta   # deve voltar a healthy, sem loop
```

Em `localhost:9000/domains` → *View DNS Records*, o campo Host / Name deve vir preenchido.
