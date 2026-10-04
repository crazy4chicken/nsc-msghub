# API overview

`nsc-msghub` is an independent notification gateway. A caller submits a single notification intent — a user id, a notification type, and a body — and the service resolves the recipient, picks a channel by type, renders the body for that channel, delivers it, and records the outcome.

## Base URL

The local base URL is `http://127.0.0.1:8090` (the default `NOTIFY_ADDR`). The service does not terminate TLS; deploy TLS at the reverse-proxy boundary.

Download the machine-readable contract from [`openapi.yaml`](../openapi.yaml), or fetch it from the published documentation host:

```sh
curl -fsS https://crazy4chicken.github.io/nsc-msghub/openapi.yaml -o openapi.yaml
```

## Authentication classes

`/healthz` is public. Everything under `/api/*` shares one of two authentication modes:

| Mode | Credential | Notes |
| --- | --- | --- |
| Static API token | `Authorization: Bearer <NOTIFY_TOKEN>` (an `X-Notify-Token` header also works) | Active when `NOTIFY_TOKEN` is set and `NOTIFY_TEAMUSERS_URL` is not. |
| teamusers JWT | `Authorization: Bearer <JWT>` | Active when `NOTIFY_TEAMUSERS_URL` is set; takes over `/api/*` and ignores `NOTIFY_TOKEN`. |

With neither configured, `/api/*` is unauthenticated for local development. A missing or invalid credential returns `401` with kind `unauthorized`; a valid teamusers JWT lacking the required permission returns `403` with kind `forbidden`.

In teamusers mode each route is checked against a permission key (defaults shown):

| Permission | Routes |
| --- | --- |
| `msghub:send:any` | `POST /api/v1/notify`, `POST /api/v1/channels/email/verify` |
| `msghub:read:any` | `GET /api/v1/channels`, `GET /api/v1/notifications`, `GET /api/v1/notifications/{id}` |

Unmatched `/api/` paths still require authentication, then return `404` as usual.

## Errors

Every failure uses the same envelope:

```json
{
  "error": {
    "kind": "invalid_request",
    "message": "…"
  }
}
```

| `kind` | HTTP | Meaning |
| --- | --- | --- |
| `invalid_request` | 400 | Malformed parameters, both recipient forms supplied, or bare HTML inside a Markdown body. |
| `unauthorized` | 401 | Missing or invalid credentials — the static token, or a missing/invalid teamusers JWT. |
| `forbidden` | 403 | teamusers reported insufficient permission; the message names the required permission and the reason. |
| `not_found` | 404 | The channel or record does not exist, or the user has no recipient address on the routed channel. |
| `channel_not_ready` | 503 | The channel is not configured, the route has no usable channel, or no user directory is configured. |
| `upstream_failed` | 502 | The user service failed or returned unusable data. |
| `delivery_failed` | 502 | SMTP refused the message or could not be reached. |
| `internal_error` | 500 | Unexpected server failure. |

## Reference pages

The pages below are generated at build time from the routes and their co-located operation metadata, one page per OpenAPI tag:

- [Channels](./reference/channels) — channel status, user directory, routing rules, and the email self-check.
- [Notifications](./reference/notifications) — send a notification and query delivery records.
- [Health](./reference/health) — liveness check (`GET /healthz`, no token required).

Never hand-edit generated reference output; change the handlers and their operation metadata instead, then regenerate the spec with `go run ./cmd/genspec`.
