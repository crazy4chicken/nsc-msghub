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
2. **Directory resolution** — the recipient is resolved by user id: a local JSON
   table or a user-service HTTP contract (`{id}` path placeholder, optional
   bearer token, explicit 404/502 semantics). With no directory configured the
   call answers an explicit 503.
3. **Ordered routing** — each notification type maps to an ordered channel
   priority (`alert=email,sms;digest=email;default=email`). An explicit `channel`
   in the request bypasses routing; when nothing is routable the call fails with
   a specific reason instead of silently delivering elsewhere.
4. **Channel rendering and delivery** — email goes through SMTP (implicit TLS,
   STARTTLS, or plain text) with PLAIN/LOGIN auth; SMS is a provider interface
   and reports "not available" until one is wired up. Markdown gets an
   inline-styled HTML shell plus a `text/plain` fallback for email, and
   syntax-stripped text for SMS.
5. **Record** — every attempt is appended to a JSONL delivery record and kept in
   memory for queries, enough to answer what was sent, to whom, and why it
   failed.

## Requirements

- Go 1.26 or newer (see `go.mod`)
- No database; the only persistent state is the data directory

## Quick start

```sh
cp .env.example .env      # then fill in NOTIFY_SMTP_HOST / _USER / _PASS (mailbox auth code)
go build -o msghub .
./msghub
```

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

The user id must exist in the configured user directory (a local
`data/users.json` is enough for a first try). The response is the delivery
result: status, the channel that was chosen, the resolved recipients, and timing.

## Development simulation switches

Two simulation paths are **off by default**, because "looks sent, never left the
machine" is more dangerous than an outright failure:

- `NOTIFY_DEV_OUTBOX=1` writes emails to `data/outbox/*.eml` instead of
  delivering them.
- `NOTIFY_SMS_SIMULATE=1` writes SMS messages to `data/outbox/sms.log`.

Neither switch changes the API. Their records get status `simulated`, which
stays distinguishable from a real `sent`.

## Next steps

- [Configuration](/guide/configuration) — every environment variable, CLI flag,
  auth mode, the user-directory contract, routing, and delivery records.
- [Deployment](/guide/deploy) — release artifact contract, systemd, svchost,
  teamusers authentication, upgrade and rollback, and smoke checks.
