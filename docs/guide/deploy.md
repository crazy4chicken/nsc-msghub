---
title: Deployment
outline: 2
---

# Deployment

`nsc-msghub` is a single static executable. All configuration comes from
environment variables; PostgreSQL is the only persistent state and must be
reachable from the service. This page covers the release artifact contract,
running under systemd, hosting with svchost, teamusers authentication, upgrade
and rollback, and self-checks.

svchost field semantics and validation are defined by the official documentation:
the [Compose configuration reference](https://github.com/crazy4chicken/nekostick-svchost/blob/main/docs/compose.md)
and the [Microservice publishing guide](https://github.com/crazy4chicken/nekostick-svchost/blob/main/docs/publishing.md).

## Release artifact contract

CI builds and publishes static Linux binaries on `v*` tags (see
`.github/workflows/release.yml`):

- The build command is fixed to
  `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$TAG"`; the
  artifact uses no cgo and has no dynamic library dependencies.
- The version is injected through `-X main.version` as the full tag (including
  the `v` prefix). It appears in the startup log and in the `version` field of
  `/healthz`; a build without ldflags keeps the source default (`0.3.0`).
- The svchost release asset is named `msghub_<version>_<arch>.zip`: `<version>`
  is the tag without its `v` prefix (`v0.3.0` → `msghub_0.3.0_x64.zip`) and
  `<arch>` is `x64` or `arm64`.
- The ZIP contains exactly one entry at its root, named exactly `msghub` (the
  serviceId); the sha256 covers the ZIP file itself, not the unpacked binary.

Download the ZIP for your architecture from the
[Releases](https://github.com/crazy4chicken/nsc-msghub/releases) page; the
release notes link to this page.

## Running directly

The CLI flags `-brand`, `-addr`, `-database-url`, `-record-limit`, `-token`,
`-log-level`, `-web` override the corresponding environment variables. A `.env`
file in the working directory is also read (real environment variables win; the
path can be changed with `NOTIFY_ENV_FILE`). Under systemd / svchost, use the
platform's own environment injection instead of relying on `.env`.

### systemd

Install the binary:

```sh
install -m 0755 msghub /usr/local/bin/msghub
```

Write `/etc/nsc-msghub.env` (mode 0600; it contains the SMTP auth code, the
teamusers service credentials and the database DSN — quote values that contain
spaces or `#`):

```sh
NOTIFY_ADDR=0.0.0.0:8090
NOTIFY_DATABASE_URL=postgres://msghub:password@127.0.0.1:5432/msghub?sslmode=disable
NOTIFY_LOG_LEVEL=info

# notification type -> channel priority
NOTIFY_ROUTES=alert=email,sms;digest=email;default=email

# sender mailbox
NOTIFY_SMTP_HOST=smtp.qq.com
NOTIFY_SMTP_PORT=465
NOTIFY_SMTP_TLS=implicit
NOTIFY_SMTP_USER=you@qq.com
NOTIFY_SMTP_PASS=auth-code

# teamusers auth (optional; once set, the static NOTIFY_TOKEN retires)
# NOTIFY_TEAMUSERS_URL=http://127.0.0.1:8080
# NOTIFY_TEAMUSERS_CLIENT_ID=msghub                  # service account client_id (username)
# NOTIFY_TEAMUSERS_CLIENT_SECRET=                    # one-time secret; exchanged for a token (recommended)
# NOTIFY_TEAMUSERS_SERVICE_TOKEN=                    # alternative: static kind=service access token
```

`NOTIFY_ADDR=0.0.0.0:8090` is what opens the port to other hosts; the default
`127.0.0.1:8090` listens on loopback only.

Unit `/etc/systemd/system/nsc-msghub.service`:

```ini
[Unit]
Description=nsc-msghub notification gateway
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=nsc-msghub
Group=nsc-msghub
EnvironmentFile=/etc/nsc-msghub.env
ExecStart=/usr/local/bin/msghub
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true

[Install]
WantedBy=multi-user.target
```

```sh
systemctl daemon-reload && systemctl enable --now nsc-msghub
```

PostgreSQL is the service's only persistent state: it must be reachable at
`NOTIFY_DATABASE_URL`, and the database user needs table-create rights on the
first start (the service only runs `CREATE TABLE IF NOT EXISTS`). The test page
is served from the directory given by `NOTIFY_WEB_DIR` (it must contain
`index.html`); production usually leaves it empty to turn the page off.

## svchost deployment

Save the following as `svchost.compose.yaml`. The service key must be `msghub`:
it is both the asset name prefix and the entry file name that must exist at the
ZIP root after unpacking.

```yaml
# Field semantics: https://github.com/crazy4chicken/nekostick-svchost/blob/main/docs/compose.md
#
# strictSources is intentionally omitted (it defaults to false): a single
# source.sha256 cannot cover every architecture at once, because the per-arch
# ZIP assets have different digests. Pin sha256 only for a
# single-architecture deployment, or set strictSources: true together with a
# matching digest.
serviceScope: global

services:
  msghub:
    source:
      release: "github:crazy4chicken/nsc-msghub@v0.3.0" # SemVer ref: release tag and asset version match by SemVer identity
      # sha256: "0000000000000000000000000000000000000000000000000000000000000000" # 64 hex chars; optional single-arch pin, otherwise svchost uses the GitHub asset digest
    env:
      NOTIFY_ADDR: "0.0.0.0:8090" # use "${HOST}:${PORT}" when the Host should assign the port
      NOTIFY_DATABASE_URL: "${HOST:MSGHUB_DATABASE_URL}" # required PostgreSQL DSN; the DB must be reachable and the user needs table-create rights at first start
      NOTIFY_ROUTES: "alert=email,sms;digest=email;default=email"
      NOTIFY_SMTP_HOST: smtp.qq.com
      NOTIFY_SMTP_PORT: "465"
      NOTIFY_SMTP_TLS: implicit
      NOTIFY_SMTP_USER: you@qq.com
      NOTIFY_SMTP_PASS: "${HOST:MSGHUB_SMTP_PASS}" # SMTP auth code comes from the Host environment, not this file
      NOTIFY_TEAMUSERS_URL: "http://127.0.0.1:8080"
      NOTIFY_TEAMUSERS_AUDIENCE: teamusers
      NOTIFY_TEAMUSERS_CLIENT_ID: "${HOST:MSGHUB_TEAMUSERS_CLIENT_ID}" # service account client_id (username)
      NOTIFY_TEAMUSERS_CLIENT_SECRET: "${HOST:MSGHUB_TEAMUSERS_CLIENT_SECRET}" # one-time secret; exchanged for a token on startup
      NOTIFY_TEAMUSERS_PERMISSION_SEND: "msghub:send:any"
      NOTIFY_TEAMUSERS_PERMISSION_READ: "msghub:read:any"
    start: eager
    restart: on-failure
    health:
      type: http
      path: /healthz
      timeout: 5s
```

Notes:

- `${...}` inside `env` values are svchost launch templates, not shell:
  `${HOST:X}` passes through the Host environment variable `X`, while
  `${HOST}` / `${PORT}` take the dynamic values of the current launch. svchost
  only rewrites `env` values and `args` strings — do not write other expressions
  assuming shell semantics.
- `NOTIFY_WEB_DIR` must be an absolute path: the svchost process CWD is the
  service root (`<data>/svchost/global` under the `global` scope) and upgrades
  only replace the bundle under `artifacts/` and `tmp/`. No other path setting
  is required — all persistent state lives in PostgreSQL.
- `NOTIFY_DATABASE_URL` must point at a reachable PostgreSQL server; inject the
  DSN from the Host environment (`MSGHUB_DATABASE_URL` in the snippet). The
  service refuses to start when it is empty.
- `health` uses `http /healthz`; that endpoint requires no token. The default
  `type: process` cannot detect "process alive, listener not up". Because
  `/healthz` also reports database reachability (503 `degraded`), a PostgreSQL
  outage fails this health check.
- The service name `msghub` is deduplicated in the `serviceScope: global`
  namespace; another config declaring the same name fails the whole config. For
  multiple instances on one machine use `serviceScope: document` and keep the
  name `msghub`.
- To expose the service through the Host, add a `route` section per compose.md
  (`prefix` must start with `/`; `strip: true` removes the prefix before
  forwarding).

A ready-to-use copy is checked in at the repository root:
[`svchost.compose.yaml`](https://github.com/crazy4chicken/nsc-msghub/blob/main/svchost.compose.yaml).

## teamusers authentication

- Once `NOTIFY_TEAMUSERS_URL` is configured, `/api/*` accepts only JWTs issued by
  teamusers (still sent as `Authorization: Bearer <JWT>`): a missing or invalid
  token returns 401 `unauthorized`, and insufficient permissions return 403
  `forbidden`. The static `NOTIFY_TOKEN` is ignored at the same time (the startup
  log warns).
- Permission lookups need a teamusers service credential. Preferred:
  `NOTIFY_TEAMUSERS_CLIENT_ID` + `NOTIFY_TEAMUSERS_CLIENT_SECRET` — create a
  service account in teamusers and copy the `client_id` (username) and the
  one-time `client_secret` returned by `POST /users/{id}/credentials`. msghub
  trades them for a service access token at `POST /auth/client-credentials` on
  startup (failing fast when the credentials or the network are wrong) and
  refreshes it before expiry, so the env file holds long-lived credentials
  instead of a ten-minute token. Alternative: `NOTIFY_TEAMUSERS_SERVICE_TOKEN`
  holds a static `kind=service` access token — only practical when the teamusers
  operator raises `TEAMUSERS_ACCESS_TOKEN_TTL`. A configured URL with neither
  option makes the service refuse to start.
- `NOTIFY_TEAMUSERS_AUDIENCE` (default `teamusers`) must match the `aud` the
  issuer writes into the JWT, otherwise every request gets a 401.
- The default permission names are `msghub:send:any` (send, email self-check)
  and `msghub:read:any` (channel status, delivery-record queries); grant them on
  the teamusers side. If the issuer uses different names, align them with
  `NOTIFY_TEAMUSERS_PERMISSION_SEND` / `NOTIFY_TEAMUSERS_PERMISSION_READ`.
- msghub must be able to reach `NOTIFY_TEAMUSERS_URL` (JWKS and permission
  endpoints; timeout `NOTIFY_TEAMUSERS_TIMEOUT`, default 5 seconds).
- `/healthz` and the test page stay public and never go through this auth.

## Upgrade and rollback

- svchost: change the `source.release` ref (for example to `@v0.3.1`) and trigger
  a reconcile; svchost re-downloads and verifies the ZIP for the node's
  architecture, replaces the bundle, and restarts the process per the `restart`
  policy. To roll back, point the ref back at the old tag and reconcile.
  `vX.Y.Z` refs are the simplest; to pin a version by commit prefix the release's
  `target_commitish` and the asset version must both start with that prefix,
  otherwise the release is rejected outright (see the ref rules in the publishing
  guide).
- systemd: stop the service, replace `/usr/local/bin/msghub`, start it again; the
  environment file stays untouched and the PostgreSQL state is untouched.
- Data compatibility: the service only runs `CREATE TABLE IF NOT EXISTS` at
  startup; upgrades and rollbacks neither drop nor rewrite existing tables.
  There is no data directory anymore — PostgreSQL backups are `pg_dump`'s job.
- After a rollback, check that `version` on `/healthz` matches the target and
  confirm no old process is still around (a busy port makes startup fail
  immediately).

## Health and smoke checks

```sh
# liveness + version injection (no token required)
curl -fsS http://127.0.0.1:8090/healthz
# -> {"status":"ok","version":"v0.3.1",...}

# channel readiness, user directory and effective routes
# (needs a read credential: static token or JWT with msghub:read:any)
curl -fsS http://127.0.0.1:8090/api/v1/channels -H "Authorization: Bearer $TOKEN"

# email channel self-check (needs the send permission)
curl -fsS -X POST http://127.0.0.1:8090/api/v1/channels/email/verify \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"to":["you@qq.com"]}'
```

In the `/api/v1/channels` response, `channels` reports each channel's mode and
readiness, `directory` says whether the user directory comes from the PostgreSQL
`users` table or the user service, and `routes` is the effective type → channel
order. `/healthz` answers `503` with `{"status":"degraded",...}` when the
database cannot be queried. Check here first after deploying, then send real
notifications.

Without SMTP configured and without `NOTIFY_DEV_OUTBOX` / `NOTIFY_SMS_SIMULATE`,
channels explicitly report "not available" and never pretend to send
successfully. The simulation switches only write rows into `outbox_messages`;
their records get status `simulated`, which stays distinguishable from a real
`sent`.
