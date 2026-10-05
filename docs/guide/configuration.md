---
title: Configuration
outline: 2
---

# Configuration

Configuration comes entirely from environment variables, plus a `.env` file
when present (real environment variables win). There is no configuration API;
all persistent state lives in PostgreSQL, whose schema is created idempotently
at startup. Precedence is **CLI flag > environment variable > `.env` > built-in
default**; flags are parsed after `.env` is loaded, so `.env` values also act as
flag defaults.

The template at the repository root lists every switch with inline comments:
[`.env.example`](https://github.com/crazy4chicken/nsc-msghub/blob/main/.env.example).

## Environment variables

### Service

| Variable | Default | Purpose |
| --- | --- | --- |
| `NOTIFY_BRAND` | `notify-service` | Email shell header and fallback sender display name. |
| `NOTIFY_MAIL_FOOTER` | auto-generated | Email shell footer text. |
| `NOTIFY_ADDR` | `127.0.0.1:8090` | Listen address; `0.0.0.0:8090` exposes it to other hosts. |
| `NOTIFY_DATABASE_URL` | required | PostgreSQL DSN (e.g. `postgres://user:pass@host:5432/db?sslmode=disable`). Empty makes the service refuse to start; the schema is created at first startup with `CREATE TABLE IF NOT EXISTS` only. |
| `NOTIFY_RECORD_LIMIT` | `0` | Delivery records to keep: `0` keeps everything; when `>0` only the newest N records survive, pruned after each insert. |
| `NOTIFY_TOKEN` | empty | Static token; when set, `/api/*` requires `Authorization: Bearer <token>` (an `X-Notify-Token` header is also accepted). Only effective while `NOTIFY_TEAMUSERS_URL` is unset. |
| `NOTIFY_LOG_LEVEL` | `info` | `debug` logs every HTTP request. |
| `NOTIFY_WEB_DIR` | `web` | Test page directory (must contain `index.html`); empty disables the page. |
| `NOTIFY_ENV_FILE` | `.env` | Path of the environment file read at startup. |

### teamusers authentication

| Variable | Default | Purpose |
| --- | --- | --- |
| `NOTIFY_TEAMUSERS_URL` | empty | teamusers service address; when set it takes over `/api/*` authentication (JWT + permission checks). |
| `NOTIFY_TEAMUSERS_AUDIENCE` | `teamusers` | Expected JWT `aud`. |
| `NOTIFY_TEAMUSERS_SERVICE_TOKEN` | required | Service Bearer token used to query user permissions; the service refuses to start without it once the URL is set. |
| `NOTIFY_TEAMUSERS_TIMEOUT` | `5` | JWKS and permission endpoint timeout (seconds). |
| `NOTIFY_TEAMUSERS_PERMISSION_SEND` | `msghub:send:any` | Permission required by sending endpoints. |
| `NOTIFY_TEAMUSERS_PERMISSION_READ` | `msghub:read:any` | Permission required by query endpoints. |

### Sender mailbox

| Variable | Default | Purpose |
| --- | --- | --- |
| `NOTIFY_SMTP_HOST` | empty | SMTP server of the sender mailbox. |
| `NOTIFY_SMTP_PORT` | `587` | QQ/163 and similar use `465`. |
| `NOTIFY_SMTP_TLS` | `auto` | `auto`, `starttls` (587), `implicit` (465), or `none` (local only). |
| `NOTIFY_SMTP_USER` / `NOTIFY_SMTP_PASS` | empty | Mailbox account and its SMTP auth code. |
| `NOTIFY_SMTP_FROM` | `NOTIFY_SMTP_USER` | Sender address; most providers require it to match the account. |
| `NOTIFY_SMTP_FROM_NAME` | `NOTIFY_BRAND` | Sender display name, RFC 2047 encoded. |
| `NOTIFY_SMTP_TIMEOUT` | `15` | Per-session timeout (seconds). |

### User directory

| Variable | Default | Purpose |
| --- | --- | --- |
| `NOTIFY_USER_SERVICE_URL` | empty | User service address; takes priority over the PostgreSQL `users` table. |
| `NOTIFY_USER_SERVICE_PATH` | `/api/users/{id}` | User lookup path; must contain `{id}`. |
| `NOTIFY_USER_SERVICE_TOKEN` | empty | Bearer token for the user service. |
| `NOTIFY_USER_SERVICE_TIMEOUT` | `5` | User service timeout (seconds). |

### Routing and simulation

| Variable | Default | Purpose |
| --- | --- | --- |
| `NOTIFY_ROUTES` | `default=email` | Type-to-channel priority. |
| `NOTIFY_SMS_SIMULATE` | `0` | `1` uses the local simulated SMS upstream (rows land in the `outbox_messages` table). |
| `NOTIFY_DEV_OUTBOX` | `0` | `1` writes emails into the `outbox_messages` table instead of delivering them. |

## CLI flags

Flags override the corresponding environment variables:

| Flag | Environment variable | Purpose |
| --- | --- | --- |
| `-brand` | `NOTIFY_BRAND` | Service/email brand name. |
| `-addr` | `NOTIFY_ADDR` | HTTP listen address. |
| `-database-url` | `NOTIFY_DATABASE_URL` | PostgreSQL DSN; required. |
| `-record-limit` | `NOTIFY_RECORD_LIMIT` | Delivery records to keep (`0` = all). |
| `-web` | `NOTIFY_WEB_DIR` | Test page directory. |
| `-token` | `NOTIFY_TOKEN` | API token; empty means no authentication. |
| `-log-level` | `NOTIFY_LOG_LEVEL` | `debug` / `info` / `warn` / `error`. |

## Authentication modes

| Mode | How to enable | Behavior |
| --- | --- | --- |
| None | Default | No authentication (local development). |
| Static token | Set `NOTIFY_TOKEN` | `/api/*` requires the static Bearer token (`X-Notify-Token` also accepted). |
| teamusers | Set `NOTIFY_TEAMUSERS_URL` | `/api/*` requires a teamusers-issued JWT (`Authorization: Bearer <JWT>`). A missing or invalid token returns 401 `unauthorized`; known endpoints then check permissions and return 403 `forbidden` with the required permission and the reason from the SDK. |

Permissions required in teamusers mode:

| Permission (default) | Endpoints |
| --- | --- |
| `NOTIFY_TEAMUSERS_PERMISSION_SEND` (`msghub:send:any`) | `POST /api/v1/notify`, `POST /api/v1/channels/email/verify` |
| `NOTIFY_TEAMUSERS_PERMISSION_READ` (`msghub:read:any`) | `GET /api/v1/channels`, `GET /api/v1/notifications`, `GET /api/v1/notifications/{id}` |

`/healthz` and the test page stay public; unmatched `/api/` paths require
authentication only and then still return 404. In teamusers mode `NOTIFY_TOKEN`
is ignored (the startup log warns), and `NOTIFY_TEAMUSERS_SERVICE_TOKEN` is used
to query user permissions — the service refuses to start without it.

## User directory

Two interchangeable implementations; when both are configured the user service
wins.

The local user table lives in the PostgreSQL `users` table (`id`, `name`,
`channels` JSONB) and is maintained with SQL:

```sql
INSERT INTO users (id, name, channels) VALUES
  ('u1001', 'Zhang San', '{"email": "zhangsan@example.com", "sms": "13800000000"}')
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, channels = EXCLUDED.channels;
```

The user service must answer `GET {URL}{PATH}` with `200` and a user object:

```json
{"id": "u1001", "name": "Zhang San", "channels": {"email": "zhangsan@example.com", "sms": "13800000000"}}
```

Addresses may also be flat at the top level: `email` / `sms` / `phone` (used
only when `channels` does not define them). `404` means the user does not exist;
any other non-2xx status, a malformed body, or a returned `id` that differs from
the requested one is treated as an upstream failure — this prevents a
misconfigured endpoint from sending to the wrong address.

## Routing

`NOTIFY_ROUTES` maps notification types to an ordered channel priority:

```text
alert=email,sms;digest=email;default=email
```

For each candidate channel in order, the service checks that the user has an
address on that channel and that the channel is ready; the first channel that
satisfies both wins. Types without a rule fall back to `default`. A `channel`
specified explicitly in the request bypasses routing entirely — and fails
directly when that channel is unavailable, instead of quietly switching to
another one.

## Storage and records

PostgreSQL is the only persistent state. At startup the service creates three
tables idempotently (plain `CREATE TABLE IF NOT EXISTS`): `notifications`
(delivery records), `users` (the local user table), and `outbox_messages`
(simulated emails and SMS messages). The database must be reachable, and its
user needs table-create rights on the first start; there is no migration
tooling beyond that auto-DDL.

Every attempt is inserted into `notifications`; with `NOTIFY_RECORD_LIMIT` set
to `>0`, each insert also prunes all but the newest N records (`0`, the default,
keeps everything). Records carry `status` `sent` (real delivery), `simulated`
(row in `outbox_messages`, not delivered), or `failed`, together with `userId`,
`userName`, the channel that was finally chosen, and — on failure — the error
reason. The record list can be filtered by channel, type, status, and user id
via the query parameters of `GET /api/v1/notifications`.

`outbox_messages` is written only when `NOTIFY_DEV_OUTBOX` / `NOTIFY_SMS_SIMULATE`
are enabled; inspect simulated deliveries with:

```sql
SELECT "time", channel, recipients, subject, body FROM outbox_messages ORDER BY seq DESC LIMIT 20;
```
