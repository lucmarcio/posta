---
sidebar_position: 7
title: Email Verification
description: Validate an email address before sending
---

# Email Verification

Check whether an email address is valid and likely deliverable before you send to it. Verification runs a series of cheap-to-expensive checks (syntax, your suppression/bounce history, disposable/role detection, and an MX lookup) and caches results to avoid repeated lookups.

A single address can be checked synchronously (`/emails/verify`), up to 100 addresses can be checked synchronously in one call (`/emails/verify/batch`), and up to 50,000 addresses can be checked asynchronously as a job (`/emails/verify/jobs`).

:::tip
For the full request/response schema, see the interactive API Reference at `/docs` on your Posta instance.
:::

:::note
An SMTP RCPT probe is optional and **disabled by default** (`POSTA_EMAIL_VERIFY_SMTP_ENABLED=false`). While disabled, mailbox existence is never confirmed and `checks.smtp` is always `"skipped"` — a `valid` result only means the address is syntactically correct and the domain accepts mail. See [SMTP probe](#smtp-probe-optional) below for what changes when it is enabled and its operational requirements.
:::

All three endpoints require an API key with the `send` scope.

## Verify a single address

```
POST /api/v1/emails/verify
```

```bash
curl -X POST http://localhost:9000/api/v1/emails/verify \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{
    "email": "user@example.com"
  }'
```

| Field | Type | Description |
|-------|------|-------------|
| `email` | string (required) | The address to verify. Validated as an email format; a syntactically malformed address is rejected with `400` before any lookup. |

### Query parameters

| Parameter | Type | Description |
|-----------|------|-------------|
| `fresh` | bool | When `true`, bypasses the cache and re-checks the address. Example: `?fresh=true`. |

### Checks performed

Verification runs the following checks in order, returning early once the verdict is conclusive:

1. **Syntax** — the address is parsed; a malformed address is conclusively `invalid`.
2. **Suppression / bounce history** (per workspace, no network) — if the address is on your suppression list or has previously hard-bounced **for your workspace**, it is conclusively `invalid`. A hard bounce recorded in another workspace never affects this check.
3. **Disposable detection** — the domain is matched against a known set of throwaway/disposable providers. A match short-circuits to `disposable` without a DNS lookup.
4. **Role-account detection** — the local part is matched against role addresses (e.g. `info`, `admin`, `support`); a match downgrades the verdict to `risky`.
5. **MX lookup** — the domain's mail exchangers are resolved. A domain with no MX (and no A/AAAA fallback) is `invalid`. A [null MX](https://www.rfc-editor.org/rfc/rfc7505) (`0 .`) is treated the same as no MX. A temporary DNS error (timeout, SERVFAIL) is `unknown` and is **not** cached, so it is retried on the next call.
6. **SMTP RCPT probe** (only when `POSTA_EMAIL_VERIFY_SMTP_ENABLED=true`) — see [SMTP probe](#smtp-probe-optional).

A malformed address is resolved to `invalid` on syntax alone and never reaches the suppression/bounce check. For a syntactically valid address, suppressed/previously-bounced addresses short-circuit to `invalid` before the disposable, MX and SMTP checks run. For everything else, the verdict precedence is: invalid syntax → disposable → temporary DNS error (`unknown`) → no MX (`invalid`) → SMTP undeliverable (`invalid`) → role account (`risky`) → SMTP accept-all → valid.

### Cache behavior

- The **intrinsic** result (syntax, disposable, role, MX, and — when enabled — the SMTP verdict) is cached in Redis per address, and MX answers are cached per domain. The same address and domain are not re-checked on every call.
- A temporary DNS error is never cached, so it does not keep serving a stale verdict.
- The **per-workspace** flags (`suppressed`, `previously_bounced`) are always re-evaluated on each request and layered onto the (possibly cached) intrinsic result, so they reflect your current state even on a cache hit.
- `cached` is `true` in the response when the intrinsic result came from the cache.
- Pass `?fresh=true` to bypass the cache and recompute the intrinsic result.

### Response

```json
{
  "success": true,
  "data": {
    "email": "user@example.com",
    "status": "valid",
    "score": 90,
    "checks": {
      "syntax": true,
      "mx": true,
      "disposable": false,
      "role_account": false,
      "smtp": "skipped"
    },
    "reason": "",
    "mailbox_verified": false,
    "suppressed": false,
    "previously_bounced": false,
    "cached": false,
    "checked_at": "2026-05-31T12:00:00Z"
  }
}
```

A result with a likely typo also carries a `suggestion`:

```json
{
  "success": true,
  "data": {
    "email": "joao@gmial.com",
    "status": "invalid",
    "score": 0,
    "checks": {
      "syntax": true,
      "mx": false,
      "disposable": false,
      "role_account": false,
      "smtp": "skipped"
    },
    "reason": "domain has no mail exchanger (MX/A) records",
    "suggestion": "joao@gmail.com",
    "mailbox_verified": false,
    "suppressed": false,
    "previously_bounced": false,
    "cached": false,
    "checked_at": "2026-05-31T12:00:00Z"
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `email` | string | The normalized (lowercased, trimmed) address that was checked. |
| `status` | string | Overall verdict. See [Status values and sending policy](#status-values-and-sending-policy). |
| `score` | int | Confidence score (0–100). Higher is better. |
| `checks` | object | Individual check outcomes (see below). |
| `reason` | string | Human-readable explanation when the address is not plainly valid. Omitted when empty. |
| `suggestion` | string | A likely intended address for a probable typo domain (e.g. `gmial.com` → `gmail.com`), by Levenshtein distance ≤ 2 against common providers. Omitted when there is no suggestion. |
| `mailbox_verified` | bool | `true` only when the SMTP probe confirmed the specific mailbox (`checks.smtp == "deliverable"`). Always `false` while the probe is disabled. |
| `suppressed` | bool | The address is on your workspace's suppression list. |
| `previously_bounced` | bool | The address has previously hard-bounced for your workspace. |
| `cached` | bool | Whether the intrinsic result was served from cache. |
| `checked_at` | string | Timestamp (UTC) of when the result was computed. |

#### `checks` object

| Field | Type | Description |
|-------|------|-------------|
| `syntax` | bool | The address is syntactically valid. |
| `mx` | bool | The domain has resolvable MX (or A/AAAA fallback) records. |
| `disposable` | bool | The domain is a known disposable/throwaway provider. |
| `role_account` | bool | The local part is a role address (e.g. `info@`, `admin@`). |
| `smtp` | string | `skipped` (probe disabled), `deliverable`, `undeliverable`, `accept_all` (the domain accepted a random address — the whole domain is treated as catch-all), or `unknown` (the probe could not determine an answer). |

### Status values and sending policy

| Status | Meaning | Sending policy |
|--------|---------|-----------------|
| `valid` | Syntax OK and the domain accepts mail (mailbox not probed, unless the SMTP probe confirmed it). | Send. |
| `accept_all` | The domain accepts any address at SMTP time (catch-all) — deliverability of this specific mailbox is unknown. | Small batches, monitoring the bounce rate. |
| `risky` | Deliverable but discouraged, e.g. a role-based address. | Small batches, monitoring the bounce rate. |
| `unknown` | Could not be determined (e.g. a transient DNS error, or an inconclusive SMTP probe). | Small batches, monitoring the bounce rate. |
| `invalid` | Definitely undeliverable — bad syntax, no MX/null MX, SMTP RCPT rejection, or suppressed/previously bounced for your workspace. | Never send. |
| `disposable` | A throwaway/disposable email provider. | Never send. |

### Errors

| Status | When |
|--------|------|
| `400` | The `email` field is missing or not a valid email format. |
| `404` | Email verification is disabled on this instance. |
| `429` | The per-user hourly verification rate limit was exceeded. |

## Verify a batch (synchronous)

```
POST /api/v1/emails/verify/batch
```

Verifies up to 100 addresses in one request, synchronously, in input order. Malformed entries are reported as `invalid` results, not rejected. Larger lists should use a [verification job](#bulk-verification-jobs).

```bash
curl -X POST http://localhost:9000/api/v1/emails/verify/batch \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{
    "emails": ["user@example.com", "bad@mailinator.com", "info@example.com"]
  }'
```

| Field | Type | Description |
|-------|------|-------------|
| `emails` | string[] (required) | Up to 100 addresses. |

### Query parameters

| Parameter | Type | Description |
|-----------|------|-------------|
| `fresh` | bool | When `true`, bypasses the cache and re-checks every address. |

### Response

```json
{
  "success": true,
  "data": {
    "items": [
      {
        "email": "user@example.com",
        "status": "valid",
        "score": 90,
        "checks": { "syntax": true, "mx": true, "disposable": false, "role_account": false, "smtp": "skipped" },
        "mailbox_verified": false,
        "suppressed": false,
        "previously_bounced": false,
        "cached": false,
        "checked_at": "2026-05-31T12:00:00Z"
      },
      {
        "email": "bad@mailinator.com",
        "status": "disposable",
        "score": 10,
        "checks": { "syntax": true, "mx": false, "disposable": true, "role_account": false, "smtp": "skipped" },
        "reason": "disposable email provider",
        "mailbox_verified": false,
        "suppressed": false,
        "previously_bounced": false,
        "cached": false,
        "checked_at": "2026-05-31T12:00:00Z"
      },
      {
        "email": "info@example.com",
        "status": "risky",
        "score": 60,
        "checks": { "syntax": true, "mx": true, "disposable": false, "role_account": true, "smtp": "skipped" },
        "reason": "role-based address",
        "mailbox_verified": false,
        "suppressed": false,
        "previously_bounced": false,
        "cached": true,
        "checked_at": "2026-05-31T12:00:00Z"
      }
    ],
    "summary": {
      "valid": 1,
      "risky": 1,
      "disposable": 1
    }
  }
}
```

`items` preserves input order; `summary` counts results per status.

### Errors

| Status | When |
|--------|------|
| `400` | `emails` is empty. |
| `404` | Email verification is disabled on this instance. |
| `413` | More than 100 addresses were sent — use a verification job instead. |
| `429` | The per-user hourly verification rate limit was exceeded. |

## Bulk verification jobs

For lists larger than 100 addresses (up to 50,000), run an asynchronous job: create it, poll its status, then page through or export the results.

### Create a job

```
POST /api/v1/emails/verify/jobs
```

```bash
curl -X POST http://localhost:9000/api/v1/emails/verify/jobs \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: campaign-42-verify" \
  -d '{
    "emails": ["a@example.com", "b@example.com"],
    "apply": true
  }'
```

| Field | Type | Description |
|-------|------|-------------|
| `emails` | string[] | Addresses to verify; mutually exclusive with `subscriber_list_id`. |
| `subscriber_list_id` | int | Subscriber list to verify instead of an explicit `emails` array. |
| `fresh` | bool | Bypass the cache and re-check every address. |
| `apply` | bool | When the job completes, suppress (`kind=manual`) every address that came out `invalid` or `disposable`. |

| Header | Description |
|--------|-------------|
| `Idempotency-Key` (optional) | Replaying the same key returns the existing job instead of creating another. |

Exactly one of `emails` / `subscriber_list_id` is required. The resolved list is capped at 50,000 addresses and at most 2 jobs may be active (queued or running) per workspace at once.

### Response — `202 Accepted`

```json
{
  "success": true,
  "data": {
    "id": "b3f1c2a0-6f3a-4e2e-9c3e-1f0b7a9c9e11",
    "status": "queued",
    "total": 3200,
    "processed": 0,
    "progress": 0,
    "counts": {},
    "fresh": false,
    "apply": true,
    "created_at": "2026-09-28T12:00:00Z"
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `id` | string | Job UUID, used in the endpoints below. |
| `status` | string | `queued`, `running`, `completed`, or `failed`. |
| `total` | int | Number of addresses in the job (deduplicated, normalized). |
| `processed` | int | Number of addresses processed so far. |
| `progress` | int | `processed * 100 / total`, or `100` when `total` is `0`. |
| `counts` | object | Per-status counts of processed items (the `pending` bucket is never included). |
| `fresh`, `apply` | bool | The flags the job was created with. |
| `error` | string | Failure reason. Omitted unless `status` is `failed`. |
| `created_at`, `started_at`, `completed_at` | string | Timestamps (UTC). `started_at`/`completed_at` are omitted until reached. |

### Get job status

```
GET /api/v1/emails/verify/jobs/{id}
```

```json
{
  "success": true,
  "data": {
    "id": "b3f1c2a0-6f3a-4e2e-9c3e-1f0b7a9c9e11",
    "status": "running",
    "total": 3200,
    "processed": 1500,
    "progress": 46,
    "counts": {
      "valid": 1200,
      "invalid": 200,
      "risky": 60,
      "disposable": 40
    },
    "fresh": false,
    "apply": true,
    "created_at": "2026-09-28T12:00:00Z",
    "started_at": "2026-09-28T12:00:05Z"
  }
}
```

### List results

```
GET /api/v1/emails/verify/jobs/{id}/results?status=&page=&size=
```

| Parameter | Type | Description |
|-----------|------|-------------|
| `status` | string | Optional filter by verifier status (e.g. `?status=invalid`). |
| `page` | int | Zero-based page number. Default `0`. |
| `size` | int | Page size. Default `100`, maximum `1000`. |

```json
{
  "success": true,
  "data": [
    {
      "email": "a@example.com",
      "status": "valid",
      "score": 90,
      "result": {
        "email": "a@example.com",
        "status": "valid",
        "score": 90,
        "checks": { "syntax": true, "mx": true, "disposable": false, "role_account": false, "smtp": "skipped" },
        "mailbox_verified": false,
        "suppressed": false,
        "previously_bounced": false,
        "cached": true,
        "checked_at": "2026-09-28T12:03:11Z"
      }
    },
    {
      "email": "b@example.com",
      "status": "invalid",
      "score": 0,
      "reason": "domain has no mail exchanger (MX/A) records",
      "result": {
        "email": "b@example.com",
        "status": "invalid",
        "score": 0,
        "checks": { "syntax": true, "mx": false, "disposable": false, "role_account": false, "smtp": "skipped" },
        "reason": "domain has no mail exchanger (MX/A) records",
        "mailbox_verified": false,
        "suppressed": false,
        "previously_bounced": false,
        "cached": false,
        "checked_at": "2026-09-28T12:03:12Z"
      }
    }
  ],
  "pageable": {
    "current_page": 0,
    "size": 100,
    "total_pages": 32,
    "total_elements": 3200,
    "empty": false
  }
}
```

`result` mirrors the [single-address response](#response) and is omitted for an item that has not been processed yet (or whose stored result failed to decode).

### Export results as CSV

```
GET /api/v1/emails/verify/jobs/{id}/results.csv
```

Streams the job's items as `text/csv`, without pagination. Columns:

```
email,status,score,reason,suggestion,suppressed,previously_bounced,mx,disposable,role_account,smtp
```

```
a@example.com,valid,90,,,false,false,true,false,false,skipped
b@example.com,invalid,0,domain has no mail exchanger (MX/A) records,,false,false,false,false,false,skipped
```

### Job endpoint errors

| Status | When |
|--------|------|
| `400` | Neither or both of `emails`/`subscriber_list_id` were given, or the resolved list is empty. |
| `404` | Email verification is disabled, or the job UUID does not belong to the caller's scope. |
| `413` | The resolved list has more than 50,000 addresses. |
| `429` | 2 jobs are already active for the workspace, or the verification rate limit was exceeded. |
| `503` | Verification jobs are not available on this instance (no job producer configured). |

## SMTP probe (optional)

When `POSTA_EMAIL_VERIFY_SMTP_ENABLED=true`, addresses of domains with a conclusive MX are also probed with a real SMTP session against the domain's highest-priority MX: `EHLO` → `MAIL FROM` → `RCPT TO` of a random address (to detect a catch-all domain) → `RCPT TO` of the real address(es) → `QUIT`. **`DATA` is never sent** — no message is transmitted during a probe.

- `checks.smtp` becomes `deliverable`, `undeliverable`, `accept_all` (the domain accepted the random probe address — every address on that domain is reported `accept_all` for the MX-TTL cache window), or `unknown` (timeout or an inconclusive response).
- `mailbox_verified` is `true` only for `deliverable`.
- The catch-all verdict is cached per domain in Redis, separately from the address/MX cache.

Operational requirements — the probing host needs:

- **Outbound TCP port 25.** This is commonly blocked in cloud VMs and in WSL; the probe silently degrades to `unknown` if it cannot connect.
- **An IP address different from the one used to send mail**, with a matching reverse DNS (rDNS/PTR) entry and a HELO/EHLO name consistent with it. Probing from your sending IP risks having that IP flagged by receiving providers for RCPT-only traffic.
- Awareness that probing **in volume** looks like enumeration to some providers and can lead to temporary blocking of the probing IP. Keep concurrency modest (see `POSTA_EMAIL_VERIFY_SMTP_PER_HOST` below) and treat the probe as a spot-check, not a bulk scanner.

### Configuration

| Variable | Default | Description |
|----------|---------|--------------|
| `POSTA_EMAIL_VERIFY_ENABLED` | `true` | Enables the verification endpoints. `404` on all of them when `false`. |
| `POSTA_EMAIL_VERIFY_CACHE_TTL_HOURS` | `168` (7 days) | TTL of the per-address intrinsic-result cache in Redis. |
| `POSTA_EMAIL_VERIFY_MX_CACHE_TTL_HOURS` | `24` | TTL of the per-domain MX cache in Redis (also used for the catch-all cache when the SMTP probe is on). |
| `POSTA_EMAIL_VERIFY_RATE_HOURLY` | `1000` | Per-user hourly cap on verification calls; `0` disables rate limiting. |
| `POSTA_EMAIL_VERIFY_CONCURRENCY` | `16` | Max number of domains resolved/probed in parallel by a batch or job. |
| `POSTA_EMAIL_VERIFY_SMTP_ENABLED` | `false` | Turns on the SMTP RCPT probe. |
| `POSTA_EMAIL_VERIFY_SMTP_HELO` | derived from `POSTA_WEB_URL` | Name announced in `EHLO`/`HELO`. Should match the rDNS of the probing IP. |
| `POSTA_EMAIL_VERIFY_SMTP_FROM` | `verify@<helo>` | Envelope sender used in `MAIL FROM`. |
| `POSTA_EMAIL_VERIFY_SMTP_TIMEOUT_SECONDS` | `10` | Timeout bounding the dial and every SMTP command (a whole probe session is bounded by roughly 3× this value). |
| `POSTA_EMAIL_VERIFY_SMTP_PER_HOST` | `2` | Max concurrent probe sessions to a single MX host. |

For upgrade and Brevo-specific operational notes (cache flush after deploying, workspace-scoped bounce behavior, Brevo webhook setup, blocked-contacts import), see `docs/verificacao-emails-brevo.md` in the repository (Portuguese, not part of this published site).
