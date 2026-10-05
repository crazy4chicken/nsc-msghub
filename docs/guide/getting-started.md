---
title: Getting Started
outline: 2
---

# Getting Started

`nsc-msghub` is a standalone Go notification gateway: any subsystem submits a
single notification intent (user id, type, body) and the service takes care of
everything else — resolving recipient addresses through the user directory,
routing by type to an ordered channel priority, rendering the body per channel,
delivering, and recording the outcome.

## The model

1. **Intent** — the caller sends one `POST /api/v1/notify` with a `user`, a
   `type`, and a `body` (`text` or `markdown`). Callers never touch SMTP, phone
   numbers, or any other channel detail.
2. **Directory resolution** — the recipient is resolved by user id: the
   PostgreSQL `users` table or a user-service HTTP contract (`{id}` path
   placeholder, optional bearer token, explicit 404/502 semantics). With no
   directory configured the call answers an explicit 503.
3. **Ordered routing** — each notification type maps to an ordered channel
   priority (`alert=email,sms;digest=email;default=email`). An explicit `channel`
   in the request bypasses routing; when nothing is routable the call fails with
   a specific reason instead of silently delivering elsewhere.
4. **Channel rendering and delivery** — email goes through SMTP (implicit TLS,
   STARTTLS, or plain text) with PLAIN/LOGIN auth; SMS is a provider interface
   and reports "not available" until one is wired up. Markdown gets an
   inline-styled HTML shell plus a `text/plain` fallback for email, and
   syntax-stripped text for SMS.
5. **Record** — every attempt is written to the PostgreSQL `notifications` table
   and can be queried through the API, enough to answer what was sent, to whom,
   and why it failed. `NOTIFY_RECORD_LIMIT` can cap the table at the newest N
   records (`0` keeps everything).

## Requirements

- Go 1.26 or newer (see `go.mod`)
- PostgreSQL 12 or newer, reachable through `NOTIFY_DATABASE_URL` (required)

## Quick start

Provision a database and set the DSN, then run:

```sh
cp .env.example .env      # NOTIFY_DATABASE_URL (required), then NOTIFY_SMTP_HOST / _USER / _PASS (mailbox auth code)
go build -o msghub .
./msghub
```

The schema (`notifications`, `users`, `outbox_messages`) is created idempotently
at startup; the database user needs table-create rights on the first start.

Check the process and the resolved configuration:

```sh
curl -fsS http://127.0.0.1:8090/healthz
curl -fsS http://127.0.0.1:8090/api/v1/channels
```

`/api/v1/channels` reports each channel's mode and readiness, the active user
directory, and the effective routing rules. Send a notification:

```sh
curl -fsS -X POST http://127.0.0.1:8090/api/v1/notify \
  -H 'Content-Type: application/json' \
  -d '{
    "user": "u1001",
    "type": "alert",
    "bodyFormat": "markdown",
    "subject": "Deploy finished",
    "body": "## Deploy finished\n\n- service **v1.2.3** is live\n\n> rollback steps are in the runbook"
  }'
```

The user id must exist in the configured user directory; with the default
PostgreSQL table, seed it with SQL:

```sql
INSERT INTO users (id, name, channels) VALUES
  ('u1001', 'Zhang San', '{"email": "zhangsan@example.com"}')
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, channels = EXCLUDED.channels;
```

The response is the delivery result: status, the channel that was chosen, the
resolved recipients, and timing.

## Development simulation switches

Two simulation paths are **off by default**, because "looks sent, never left the
machine" is more dangerous than an outright failure:

- `NOTIFY_DEV_OUTBOX=1` writes emails into the `outbox_messages` table instead of
  delivering them.
- `NOTIFY_SMS_SIMULATE=1` writes SMS messages into the same table.

Neither switch changes the API. Their records get status `simulated`, which
stays distinguishable from a real `sent`. Inspect the simulated messages with:

```sql
SELECT "time", channel, recipients, subject, body FROM outbox_messages ORDER BY seq DESC LIMIT 20;
```

## Next steps

- [Configuration](/guide/configuration) — every environment variable, CLI flag,
  auth mode, the user-directory contract, routing, and delivery records.
- [Deployment](/guide/deploy) — release artifact contract, systemd, svchost,
  teamusers authentication, upgrade and rollback, and smoke checks.
