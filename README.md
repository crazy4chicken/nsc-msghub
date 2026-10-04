# nsc-msghub

`nsc-msghub` 是一个独立的 Go 通知网关：任何子系统只需提交一条通知意图（用户 id、类型、正文），
其余都由本服务负责——通过用户目录解析收件地址、按类型路由选择渠道、针对渠道渲染正文、投递并记录结果。

## 功能特性

- 统一入口 `POST /api/v1/notify`。调用方不需要接触 SMTP、手机号或任何渠道细节。
- 按用户 id 解析收件人，支持本地 JSON 用户表或用户服务 HTTP 契约（`{id}` 路径占位、可选 Bearer
  Token、明确的 404/502 语义）。未配置用户目录时返回明确的 503。
- 按通知类型路由，支持有序渠道优先级（`alert=email,sms;digest=email;default=email`）；
  请求里显式指定 `channel` 则跳过路由。无法路由时返回具体原因，不会静默改发别处。
- 两个通道。邮件走 SMTP（隐式 TLS、STARTTLS 或明文），支持 PLAIN/LOGIN 认证与会话级超时；
  短信是 provider 接口，可接入任意上游，未接入时明确报"不可用"，不会假装发送成功。
- Markdown 优先的正文。`bodyFormat` 取 `text`（默认，仅做 HTML 转义）或 `markdown`：
  邮件得到内联样式的 HTML 外壳加 `text/plain` 兜底，短信得到去掉语法的纯文本。
  裸 HTML 会被拒绝，且内容先整体转义再解析，调用方文本无法注入标签。
- 追加写入的 JSONL 投递记录，可按渠道、类型、状态、用户 id 查询；失败同样记录原因。
- 配置只来自环境变量（存在 `.env` 时读取，真实环境变量优先）。没有配置接口，没有数据库。
- `/api/*` 鉴权两选一：静态 Bearer Token，或配置 teamusers（IAM）后改由 JWT + 权限校验接管
  （401/403 语义，`NOTIFY_TEAMUSERS_URL`）；对浏览器客户端开放 CORS；含 panic 恢复中间件与优雅退出。
- 除官方 teamusers SDK（`github.com/crazy4chicken/nsc-teamusers/sdk/go`，只在启用 IAM 鉴权时用到）
  外零第三方依赖，编译为单个自包含可执行文件。

## 本地开发

复制环境变量模板并填好发件邮箱；不填的话邮件通道不可用。

```sh
cp .env.example .env      # NOTIFY_SMTP_HOST / _USER / _PASS（邮箱授权码）
go build -o notify-service .
./notify-service
```

检查进程与解析出的配置：

```sh
curl -fsS http://127.0.0.1:8090/healthz
curl -fsS http://127.0.0.1:8090/api/v1/channels
```

`/api/v1/channels` 会报告每个通道的模式与可用性，以及当前生效的用户目录与路由规则。发一条通知：

```sh
curl -fsS -X POST http://127.0.0.1:8090/api/v1/notify \
  -H 'Content-Type: application/json' \
  -d '{
    "user": "u1001",
    "type": "alert",
    "bodyFormat": "markdown",
    "subject": "部署完成",
    "body": "## 部署完成\n\n- 服务 **v1.2.3** 已上线\n\n> 回滚命令见运维手册"
  }'
```

两条模拟路径**默认关闭**，因为"看起来发了、实际没出本机"比直接失败更危险：设
`NOTIFY_DEV_OUTBOX=1` 会把邮件写进 `data/outbox/*.eml` 而不投递，设 `NOTIFY_SMS_SIMULATE=1`
会把短信写进 `data/outbox/sms.log`。两个开关都不改变 API。

## 部署

生产环境用 systemd 直接运行或交给 svchost 托管；发布产物契约、compose 示例、鉴权与升级/回滚步骤见
[docs/guide/deploy.md](docs/guide/deploy.md)。

在线文档（英文）：<https://crazy4chicken.github.io/nsc-msghub/>

## 配置

| 变量 | 默认 | 用途 |
| --- | --- | --- |
| `NOTIFY_BRAND` | `notify-service` | 邮件外壳页眉与默认发件人显示名 |
| `NOTIFY_MAIL_FOOTER` | 自动生成 | 邮件外壳页脚文案 |
| `NOTIFY_ADDR` | `127.0.0.1:8090` | 监听地址；`0.0.0.0:8090` 供外部访问 |
| `NOTIFY_DATA_DIR` | `data` | 投递记录、默认用户表、outbox |
| `NOTIFY_TOKEN` | 空 | 静态 Token；设置后 `/api/*` 需要 `Authorization: Bearer <token>`。仅在未配置 `NOTIFY_TEAMUSERS_URL` 时生效 |
| `NOTIFY_TEAMUSERS_URL` | 空 | teamusers 服务地址；设置后由 teamusers 接管 `/api/*` 鉴权（JWT + 权限校验） |
| `NOTIFY_TEAMUSERS_AUDIENCE` | `teamusers` | 期望的 JWT `aud` |
| `NOTIFY_TEAMUSERS_SERVICE_TOKEN` | 必填 | 查询用户权限用的服务 Bearer Token；缺失时拒绝启动 |
| `NOTIFY_TEAMUSERS_TIMEOUT` | `5` | JWKS 与权限接口超时（秒） |
| `NOTIFY_TEAMUSERS_PERMISSION_SEND` | `msghub:send:any` | 发送类接口要求的权限 |
| `NOTIFY_TEAMUSERS_PERMISSION_READ` | `msghub:read:any` | 查询类接口要求的权限 |
| `NOTIFY_LOG_LEVEL` | `info` | 设为 `debug` 会打印每个 HTTP 请求 |
| `NOTIFY_SMTP_HOST` | 空 | 发件邮箱 SMTP 服务器 |
| `NOTIFY_SMTP_PORT` | `587` | QQ/163 等用 `465` |
| `NOTIFY_SMTP_TLS` | `auto` | `auto`、`starttls`(587)、`implicit`(465) 或 `none`(仅本地) |
| `NOTIFY_SMTP_USER` / `NOTIFY_SMTP_PASS` | 空 | 邮箱账号与它的 SMTP 授权码 |
| `NOTIFY_SMTP_FROM` | 取 `NOTIFY_SMTP_USER` | 发件地址；多数邮箱要求与账号一致 |
| `NOTIFY_SMTP_FROM_NAME` | 取 `NOTIFY_BRAND` | 发件人显示名，按 RFC 2047 编码 |
| `NOTIFY_SMTP_TIMEOUT` | `15` | 单次会话超时（秒） |
| `NOTIFY_USERS_FILE` | `<data>/users.json` | 本地用户表 |
| `NOTIFY_USER_SERVICE_URL` | 空 | 用户服务地址；设置后优先于本地用户表 |
| `NOTIFY_USER_SERVICE_PATH` | `/api/users/{id}` | 用户查询路径，必须含 `{id}` |
| `NOTIFY_USER_SERVICE_TOKEN` | 空 | 访问用户服务的 Bearer Token |
| `NOTIFY_USER_SERVICE_TIMEOUT` | `5` | 用户服务超时（秒） |
| `NOTIFY_ROUTES` | `default=email` | 类型到渠道的优先级 |
| `NOTIFY_SMS_SIMULATE` | `0` | 设为 `1` 使用本地模拟短信上游 |
| `NOTIFY_DEV_OUTBOX` | `0` | 设为 `1` 把邮件写进 outbox 而不投递 |

命令行参数覆盖常用项：`-brand`、`-addr`、`-data`、`-token`、`-log-level`。

## 鉴权

默认不鉴权（本地开发）。设 `NOTIFY_TOKEN` 后 `/api/*` 需要静态 Bearer Token（也接受
`X-Notify-Token` 头）。配置 `NOTIFY_TEAMUSERS_URL` 后改由 teamusers 接管：`/api/*` 必须带
teamusers 签发的 JWT（`Authorization: Bearer <JWT>`），缺失或校验失败返回 401 `unauthorized`；
已知接口再校验权限，权限不足返回 403 `forbidden`（消息里带所需权限与 SDK 给出的原因）：

| 权限（默认值） | 覆盖的接口 |
| --- | --- |
| `NOTIFY_TEAMUSERS_PERMISSION_SEND`（`msghub:send:any`） | `POST /api/v1/notify`、`POST /api/v1/channels/email/verify` |
| `NOTIFY_TEAMUSERS_PERMISSION_READ`（`msghub:read:any`） | `GET /api/v1/channels`、`GET /api/v1/notifications`、`GET /api/v1/notifications/{id}` |

`/healthz` 与测试页面保持公开；未命中的 `/api/` 路径只要求认证，随后照旧 404。此时
`NOTIFY_TOKEN` 被忽略（启动日志会给出警告），`NOTIFY_TEAMUSERS_SERVICE_TOKEN` 用于向 teamusers
查询用户权限，缺失时服务拒绝启动。

## API

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `POST` | `/api/v1/notify` | 发送通知 |
| `GET` | `/api/v1/channels` | 通道状态、用户目录、路由规则 |
| `POST` | `/api/v1/channels/email/verify` | 发一封自检邮件，`{"to":["me@example.com"]}` |
| `GET` | `/api/v1/notifications` | 投递记录；`?limit=&channel=&type=&status=&userId=` |
| `GET` | `/api/v1/notifications/{id}` | 单条投递记录 |
| `GET` | `/healthz` | 存活检查，无需 Token |

发送接口只接受一种收件人形式：`user`（由本服务解析）或 `to` / `target`（显式地址），
同时提供会返回 400。`target` 还接受简写 `"a@example.com"`、`["a@example.com","b@example.com"]`
以及 `{"channel":"sms","to":["138…"]}`。

响应就是投递结果：

```json
{
  "ok": true,
  "record": {
    "id": "ntf_3f9c1a7b2d5e4c80",
    "channel": "email", "provider": "smtp", "status": "sent",
    "type": "alert", "bodyFormat": "markdown",
    "userId": "u1001", "userName": "张三",
    "to": ["zhangsan@example.com"], "subject": "部署完成",
    "detail": "已投递至 smtp.example.com:465", "durationMs": 812
  }
}
```

错误统一为 `{"error":{"kind":"…","message":"…"}}`：

| kind | HTTP | 含义 |
| --- | --- | --- |
| `invalid_request` | 400 | 参数非法、两种收件人形式同时出现、Markdown 正文含裸 HTML |
| `unauthorized` | 401 | 缺少或错误的凭证（静态 Token，或 teamusers JWT 缺失/校验失败） |
| `forbidden` | 403 | teamusers 权限不足（消息里带所需权限与原因） |
| `not_found` | 404 | 通道或记录不存在，或用户在该路由上没有收件地址 |
| `channel_not_ready` | 503 | 通道未配置、路由没有可用通道、未配置用户目录 |
| `upstream_failed` | 502 | 用户服务出错或返回不可用的数据 |
| `delivery_failed` | 502 | SMTP 拒收或无法连通 |

### 正文格式

| `bodyFormat` | 行为 |
| --- | --- |
| `text`（默认） | 不解析 Markdown，只做 HTML 转义，兼容既有调用方 |
| `markdown` | 解析后按渠道渲染 |

Markdown 支持通知场景够用的子集：标题、粗体、斜体、行内代码、链接、有序与无序列表、引用、
分割线、围栏代码块。链接只放行 `http`、`https`、`mailto`。Markdown 正文里出现裸 HTML 标签会
直接 400，需要原样展示就放进行内代码；已移除的 `html` 与 `markdown` 字段会返回明确的 400，
告诉调用方该改用什么。

## 用户目录

两种可互换的实现；两者都配置时用户服务优先。

本地用户表（文件变更后自动重载）是 JSON 对象或数组：

```json
{
  "users": [
    {"id": "u1001", "name": "张三", "channels": {"email": "zhangsan@example.com", "sms": "13800000000"}}
  ]
}
```

用户服务需要响应 `GET {URL}{PATH}`，返回 `200` 与用户对象：

```json
{"id": "u1001", "name": "张三", "channels": {"email": "zhangsan@example.com", "sms": "13800000000"}}
```

地址也允许平铺在顶层：`email` / `sms` / `phone`。`404` 表示用户不存在；其它非 2xx、
非法响应体、或返回的 `id` 与请求不一致，都按上游故障处理，避免接错接口后误发。

## 路由

`nsc-msghub` 把通知类型映射成有序的渠道优先级。对每个候选渠道，本服务会检查用户是否有该
渠道的地址、以及该通道是否就绪，取第一个满足的渠道。没有配规则的类型回落到 `default`。
请求里显式指定 `channel` 则完全绕过路由——并且在不可用时直接失败，而不是悄悄换渠道。

## 发送记录

每次尝试都会追加到 `data/notifications.jsonl`，内存保留最近 5000 条用于查询。记录里的
`status` 为 `sent`（真实投递）、`simulated`（只写本地、未投递）或 `failed`，并带着
`userId`、`userName`、最终选中的渠道，失败时还有错误原因——足以回答"发了什么、发给了谁、
为什么没发出去"。
