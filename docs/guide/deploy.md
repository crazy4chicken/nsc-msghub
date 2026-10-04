---
title: Deployment
outline: 2
---

# Deployment

`nsc-msghub` is a single static executable. All configuration comes from
environment variables and there is no database; the only persistent state is the
data directory. This page covers the release artifact contract, running under
systemd, hosting with svchost, teamusers authentication, upgrade and rollback,
and self-checks.

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

The CLI flags `-brand`, `-addr`, `-data`, `-token`, `-log-level`, `-web` override
the corresponding environment variables. A `.env` file in the working directory
is also read (real environment variables win; the path can be changed with
`NOTIFY_ENV_FILE`). Under systemd / svchost, use the platform's own environment
injection instead of relying on `.env`.

### systemd

Install the binary and the data directory (owned by the run user):

```sh
install -m 0755 msghub /usr/local/bin/msghub
install -d -o nsc-msghub -g nsc-msghub /var/lib/nsc-msghub
```

Write `/etc/nsc-msghub.env` (mode 0600; it contains the SMTP auth code and the
teamusers service token — quote values that contain spaces or `#`):

```sh
NOTIFY_ADDR=0.0.0.0:8090
NOTIFY_DATA_DIR=/var/lib/nsc-msghub
NOTIFY_LOG_LEVEL=info

# notification type -> channel priority
NOTIFY_ROUTES=alert=email,sms;digest=email;default=email
NOTIFY_USERS_FILE=/var/lib/nsc-msghub/users.json

# sender mailbox
NOTIFY_SMTP_HOST=smtp.qq.com
NOTIFY_SMTP_PORT=465
NOTIFY_SMTP_TLS=implicit
NOTIFY_SMTP_USER=you@qq.com
NOTIFY_SMTP_PASS=auth-code

# teamusers auth (optional; once set, the static NOTIFY_TOKEN retires)
# NOTIFY_TEAMUSERS_URL=http://127.0.0.1:8080
# NOTIFY_TEAMUSERS_SERVICE_TOKEN=
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
WorkingDirectory=/var/lib/nsc-msghub
EnvironmentFile=/etc/nsc-msghub.env
ExecStart=/usr/local/bin/msghub
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/nsc-msghub

[Install]
WantedBy=multi-user.target
```

```sh
systemctl daemon-reload && systemctl enable --now nsc-msghub
```

`NOTIFY_DATA_DIR` must be a persistent absolute path writable by the run user.
It holds `notifications.jsonl` (delivery records), `users.json` (the local user
table), and `outbox/` (written only when `NOTIFY_DEV_OUTBOX` / `NOTIFY_SMS_SIMULATE`
are enabled). The test page is served from the directory given by
`NOTIFY_WEB_DIR` (it must contain `index.html`); production usually leaves it
empty to turn the page off.

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
      NOTIFY_DATA_DIR: /var/lib/nsc-msghub # must be persistent and writable; never point it at artifacts/ or tmp/
      NOTIFY_ROUTES: "alert=email,sms;digest=email;default=email"
      NOTIFY_USERS_FILE: /var/lib/nsc-msghub/users.json
      NOTIFY_SMTP_HOST: smtp.qq.com
      NOTIFY_SMTP_PORT: "465"
      NOTIFY_SMTP_TLS: implicit
      NOTIFY_SMTP_USER: you@qq.com
      NOTIFY_SMTP_PASS: "${HOST:MSGHUB_SMTP_PASS}" # SMTP auth code comes from the Host environment, not this file
      NOTIFY_TEAMUSERS_URL: "http://127.0.0.1:8080"
      NOTIFY_TEAMUSERS_AUDIENCE: teamusers
      NOTIFY_TEAMUSERS_SERVICE_TOKEN: "${HOST:MSGHUB_TEAMUSERS_SERVICE_TOKEN}" # service refuses to start when missing
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
- `NOTIFY_DATA_DIR` (and `NOTIFY_WEB_DIR`) must be absolute paths: the svchost
  process CWD is the service root (`<data>/svchost/global` under the `global`
  scope), upgrades only replace the bundle under `artifacts/` and `tmp/`, and the
  data directory is self-managed and must stay writable.
- `health` uses `http /healthz`; that endpoint requires no token. The default
  `type: process` cannot detect "process alive, listener not up".
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
- `NOTIFY_TEAMUSERS_SERVICE_TOKEN` is the service token msghub uses to look up
  user permissions; a configured URL without it makes the service refuse to
  start.
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
  environment file and data directory stay untouched.
- Data compatibility: `notifications.jsonl` is append-only JSONL; startup loads
  the most recent 5000 records into memory for queries, and upgrades/rollbacks
  neither migrate nor clear it. `users.json` reloads automatically after changes.
  `outbox/` is only the dump directory for simulated deliveries.
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
readiness, `directory` says whether the user directory comes from the local table
or the user service, and `routes` is the effective type → channel order. Check
here first after deploying, then send real notifications.

Without SMTP configured and without `NOTIFY_DEV_OUTBOX` / `NOTIFY_SMS_SIMULATE`,
channels explicitly report "not available" and never pretend to send
successfully. The simulation switches only write to `outbox/`; their records get
status `simulated`, which stays distinguishable from a real `sent`.
