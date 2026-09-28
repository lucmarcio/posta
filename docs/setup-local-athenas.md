# Posta local no ambiente Athenas

Notas de operação do fork local do Posta (`/devathenas/docker/posta`), escritas em 2026-09-25.
Cobre a integração com o Redis compartilhado do Athenas, dois bugs do upstream corrigidos
aqui e a configuração de DNS para verificar domínios que já têm SPF.

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

## Como verificar

```bash
docker compose up -d --build posta worker
docker compose logs posta worker | grep -E 'redis connected|Server started|worker started'
docker compose restart posta && docker compose ps posta   # deve voltar a healthy, sem loop
```

Em `localhost:9000/domains` → *View DNS Records*, o campo Host / Name deve vir preenchido.
