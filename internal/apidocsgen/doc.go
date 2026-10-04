// Package apidocsgen 将 httpapi 的单一业务路由表重放到一个仅用于文档生成的
// chi 路由，并提供生成 OpenAPI 规范所需的操作元数据。
// 运行期服务不导入本包，因此也不会引入 chi。
package apidocsgen

import (
	"github.com/crazy4chicken/nsc-teamusers/apidocs/go"

	"notify-service/internal/notify"
)

// docError 描述一个已文档化的失败响应：code 是响应里的 error.kind / 问题代码，
// title 是人类可读的标题。
func docError(status int, code, title string) apidocs.ErrorDoc {
	return apidocs.ErrorDoc{Status: status, Code: code, Title: title}
}

var (
	docInvalidRequest  = docError(400, "invalid_request", "Invalid Request")
	docUnauthorized    = docError(401, "unauthorized", "Unauthorized")
	docForbidden       = docError(403, "forbidden", "Forbidden")
	docNotFound        = docError(404, "not_found", "Not Found")
	docDeliveryFailed  = docError(502, "delivery_failed", "Bad Gateway")
	docUpstreamFailed  = docError(502, "upstream_failed", "Bad Gateway")
	docChannelNotReady = docError(503, "channel_not_ready", "Service Unavailable")
	docInternal        = docError(500, "internal_error", "Internal Server Error")
)

// docRecordExample 是一条成功的发送记录，发送与记录接口共用。
var docRecordExample = map[string]any{
	"id":          "ntf_3f9c1a7b2d5e4c80",
	"time":        "2026-01-02T12:00:00Z",
	"channel":     "email",
	"type":        "alert",
	"bodyFormat":  "markdown",
	"userId":      "u1001",
	"userName":    "Alice Chen",
	"provider":    "smtp",
	"to":          []string{"alice@example.test"},
	"subject":     "Deployment complete",
	"bodyPreview": "Deployment complete v0.3.0 is live",
	"status":      "sent",
	"messageId":   "20260102T120000.abcdef@example.test",
	"durationMs":  42,
	"meta":        map[string]any{"env": "prod"},
}

// docChannelsResponse 是 GET /api/v1/channels 的响应镜像。
type docChannelsResponse struct {
	Channels    []notify.Status             `json:"channels"`
	Directory   string                      `json:"directory"`
	Routes      map[string][]notify.Channel `json:"routes"`
	TypeRouting string                      `json:"typeRouting"`
}

// docHealthResponse 是 GET /healthz 的响应镜像。
type docHealthResponse struct {
	Status        string `json:"status"`
	Version       string `json:"version"`
	UptimeSeconds int    `json:"uptimeSeconds"`
	Records       int    `json:"records"`
}

// docNotifyRequest 是 POST /api/v1/notify 的请求镜像，覆盖真实接受的字段。
type docNotifyRequest struct {
	User       string            `json:"user,omitempty"`
	Channel    string            `json:"channel,omitempty"`
	To         []string          `json:"to,omitempty"`
	Target     *notify.Target    `json:"target,omitempty"`
	Type       string            `json:"type,omitempty"`
	Subject    string            `json:"subject,omitempty"`
	Body       string            `json:"body,omitempty"`
	BodyFormat string            `json:"bodyFormat,omitempty"`
	Meta       map[string]string `json:"meta,omitempty"`
}

// docSendResponse 是发送类接口的响应镜像；失败时 ok 为 false 且带 error。
type docSendResponse struct {
	OK     bool           `json:"ok"`
	Record notify.Record  `json:"record"`
	Error  map[string]any `json:"error,omitempty"`
}

// docListResponse 是 GET /api/v1/notifications 的响应镜像。
type docListResponse struct {
	Records []notify.Record `json:"records"`
	Total   int             `json:"total"`
}

// docVerifyEmailRequest 是 POST /api/v1/channels/email/verify 的请求镜像。
type docVerifyEmailRequest struct {
	To []string `json:"to"`
}

// DocOperations 是生成 OpenAPI 规范所用的全部操作元数据。
// 必须与 httpapi.Server.RegisterRoutes 的路由表一一对应：路由新增或改动时，
// 这里少了或多了任何一条，cmd/genspec 都会直接失败。
var DocOperations = []apidocs.Operation{
	{
		Method:      "GET",
		Path:        "/healthz",
		Tag:         "Health",
		Summary:     "Check liveness",
		Description: "Use as an inexpensive unauthenticated liveness probe. Load balancers and orchestrators call it without credentials; it reports the process version, uptime, and the number of retained send records.",
		Response:    docHealthResponse{},
		ResponseExample: map[string]any{
			"status":        "ok",
			"version":       "v0.3.0",
			"uptimeSeconds": 3600,
			"records":       42,
		},
		Errors: []apidocs.ErrorDoc{docInternal},
	},
	{
		Method:      "GET",
		Path:        "/api/v1/channels",
		Tag:         "Channels",
		Summary:     "List channels and routing",
		Description: "Use to inspect the configured delivery channels, the user directory, and the type-to-channel routing table before sending. Each channel reports its provider, mode (live, dev, or unconfigured), and readiness; sends that need an unready channel fail with 503 channel_not_ready.",
		Security:    "bearer",
		Response:    docChannelsResponse{},
		ResponseExample: map[string]any{
			"channels": []any{
				map[string]any{
					"channel": "email", "provider": "smtp", "ready": true, "mode": "live",
					"details": map[string]any{"from": "notify@example.test", "host": "smtp.example.test"},
				},
				map[string]any{
					"channel": "sms", "provider": "unconfigured", "ready": false, "mode": "unconfigured",
				},
			},
			"directory":   "用户服务 http://127.0.0.1:8081/users/lookup",
			"routes":      map[string]any{"alert": []string{"email", "sms"}, "default": []string{"email"}},
			"typeRouting": "调用方只给 user + type + 内容，渠道由这里的路由规则决定",
		},
		Errors: []apidocs.ErrorDoc{docUnauthorized, docForbidden, docInternal},
	},
	{
		Method:      "POST",
		Path:        "/api/v1/notify",
		Tag:         "Notifications",
		Summary:     "Send a notification",
		Description: "Use to send one notification through the email or SMS channel. Exactly one recipient form is accepted: user (the service resolves the address and picks a channel) or to/target (explicit addresses); supplying both returns 400. target also accepts a single string, an array of strings, or {\"channel\": ..., \"to\": [...]}. An explicit channel bypasses the type routing table, and bodyFormat is text or markdown (markdown is rendered to HTML for email and stripped for SMS). Requests are strict JSON: unknown fields are rejected and the body is capped at 1 MiB. The removed html and markdown fields return an explicit 400 instead of being ignored.",
		Security:    "bearer",
		Request:     docNotifyRequest{},
		RequestExample: map[string]any{
			"user":       "u1001",
			"type":       "alert",
			"subject":    "Deployment complete",
			"body":       "## Deployment complete\n\n- **v0.3.0** is live",
			"bodyFormat": "markdown",
			"meta":       map[string]any{"env": "prod"},
		},
		Response:        docSendResponse{},
		ResponseExample: map[string]any{"ok": true, "record": docRecordExample},
		Errors:          []apidocs.ErrorDoc{docInvalidRequest, docUnauthorized, docForbidden, docNotFound, docDeliveryFailed, docUpstreamFailed, docChannelNotReady, docInternal},
	},
	{
		Method:      "GET",
		Path:        "/api/v1/notifications",
		Tag:         "Notifications",
		Summary:     "List notification records",
		Description: "Use to review retained send attempts, newest first. limit sets the page size (default 100; at most the 5000-record retention cap). channel, type, status, and userId narrow the result set; total is the number of retained records, not the number of matches.",
		Security:    "bearer",
		Response:    docListResponse{},
		ResponseExample: map[string]any{
			"records": []any{docRecordExample},
			"total":   42,
		},
		Errors: []apidocs.ErrorDoc{docUnauthorized, docForbidden, docInternal},
	},
	{
		Method:          "GET",
		Path:            "/api/v1/notifications/{id}",
		Tag:             "Notifications",
		Summary:         "Get a notification record",
		Description:     "Use to fetch one retained send record by its id, including the failure detail when delivery failed. Unknown IDs return 404 not_found.",
		Security:        "bearer",
		Response:        notify.Record{},
		ResponseExample: docRecordExample,
		Errors:          []apidocs.ErrorDoc{docUnauthorized, docForbidden, docNotFound, docInternal},
	},
	{
		Method:      "POST",
		Path:        "/api/v1/channels/email/verify",
		Tag:         "Channels",
		Summary:     "Send an email verification",
		Description: "Use to prove that the email channel can deliver to the given addresses. It sends a fixed verification message through the configured SMTP server; without SMTP configuration the request fails with 503 channel_not_ready, and delivery problems surface as 502 delivery_failed or upstream_failed.",
		Security:    "bearer",
		Request:     docVerifyEmailRequest{},
		RequestExample: map[string]any{
			"to": []string{"ops@example.test"},
		},
		Response:        docSendResponse{},
		ResponseExample: map[string]any{"ok": true, "record": docRecordExample},
		Errors:          []apidocs.ErrorDoc{docInvalidRequest, docUnauthorized, docForbidden, docChannelNotReady, docDeliveryFailed, docUpstreamFailed, docInternal},
	},
}
