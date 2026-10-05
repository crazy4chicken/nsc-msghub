package httpapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	iam "github.com/crazy4chicken/nsc-teamusers/sdk/go"
)

const (
	// DefaultPermissionSend 发送类接口（POST /api/v1/notify、POST /api/v1/channels/email/verify）的默认权限。
	DefaultPermissionSend = "msghub:send:any"
	// DefaultPermissionRead 查询类接口（GET 通道与通知记录）的默认权限。
	DefaultPermissionRead = "msghub:read:any"
)

// PermissionClass 把已知路由归类为发送或查询权限。两个返回值都为 false
// 表示该路由只需要认证（未命中的 /api/ 路径照旧 404）。
func PermissionClass(method, path string) (send, read bool) {
	switch {
	case method == http.MethodPost && (path == "/api/v1/notify" || path == "/api/v1/channels/email/verify"):
		return true, false
	case method == http.MethodGet && (path == "/api/v1/channels" || path == "/api/v1/notifications" || strings.HasPrefix(path, "/api/v1/notifications/")):
		return false, true
	default:
		return false, false
	}
}

// TeamusersOptions 是构造 TeamusersAuth 所需的配置，由 main 从环境变量读取。
type TeamusersOptions struct {
	BaseURL        string        // teamusers 服务基址，例如 http://127.0.0.1:8080
	Audience       string        // 期望的 JWT aud
	ServiceToken   string        // 静态服务令牌（service access token），仅在未配置 client 凭证时使用
	ClientID       string        // 服务账号 client_id，配置后自动换取并刷新服务令牌
	ClientSecret   string        // 服务账号一次性密钥，与 ClientID 成对使用
	Timeout        time.Duration // JWKS 与权限接口的 HTTP 超时
	PermissionSend string        // 发送类接口要求的权限
	PermissionRead string        // 查询类接口要求的权限
}

// TeamusersAuth 封装 teamusers 的 JWT 校验器与权限客户端。
// 配置后 /api/* 只认 teamusers 签发的 JWT，静态 Token 不再参与鉴权。
type TeamusersAuth struct {
	verifier       *iam.Verifier
	client         *iam.Client
	tokenSource    *clientCredentialsTokenSource
	permissionSend string
	permissionRead string
}

// NewTeamusersAuth 构造 teamusers 鉴权组件；校验器与权限客户端共用一个带超时的 HTTP 客户端。
// ClientID 与 ClientSecret 同时非空时走 client-credentials（自动换取并刷新令牌），否则使用静态 ServiceToken。
func NewTeamusersAuth(opt TeamusersOptions) *TeamusersAuth {
	httpClient := &http.Client{Timeout: opt.Timeout}
	verifier := iam.NewVerifier(opt.BaseURL,
		iam.WithHTTPClient(httpClient),
		iam.WithAudience(opt.Audience),
	)
	permissionOptions := []any{iam.WithHTTPClient(httpClient)}
	var tokenSource *clientCredentialsTokenSource
	if opt.ClientID != "" && opt.ClientSecret != "" {
		tokenSource = newClientCredentialsTokenSource(opt.BaseURL, opt.ClientID, opt.ClientSecret, httpClient)
		permissionOptions = append(permissionOptions, iam.WithTokenSource(tokenSource.Token))
	} else {
		permissionOptions = append(permissionOptions, iam.WithServiceToken(opt.ServiceToken))
	}
	permissions := iam.NewPermissionsClient(opt.BaseURL, permissionOptions...)
	return &TeamusersAuth{
		verifier:       verifier,
		client:         iam.NewClient(verifier, permissions),
		tokenSource:    tokenSource,
		permissionSend: opt.PermissionSend,
		permissionRead: opt.PermissionRead,
	}
}

// Prime 在启动时预取服务令牌（client-credentials 模式）；静态令牌模式下为空操作。
func (a *TeamusersAuth) Prime() error {
	if a.tokenSource == nil {
		return nil
	}
	return a.tokenSource.Prime()
}

// Verify 校验 Bearer JWT 并返回其中的身份声明。
func (a *TeamusersAuth) Verify(ctx context.Context, raw string) (iam.Claims, error) {
	return a.verifier.Verify(ctx, raw)
}

// Allow 判断 claims 是否具备 permission；拒绝时返回 SDK 给出的原因。
func (a *TeamusersAuth) Allow(ctx context.Context, claims iam.Claims, permission string) (bool, string) {
	return a.client.Allow(ctx, claims, permission, iam.Resource{})
}

// RequiredPermission 返回已知路由要求的权限；空串表示只需认证（未命中的 /api/ 路径照旧 404）。
func (a *TeamusersAuth) RequiredPermission(method, path string) string {
	send, read := PermissionClass(method, path)
	switch {
	case send:
		return a.permissionSend
	case read:
		return a.permissionRead
	default:
		return ""
	}
}

// Close 停止 JWKS 缓存的后台刷新。
func (a *TeamusersAuth) Close() error {
	return a.verifier.Close()
}

// bearerToken 从 Authorization 头解析 Bearer JWT；与 SDK 一致，Bearer 前缀大小写不敏感。
func bearerToken(header string) (string, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

// writeAuthError 输出鉴权错误信封，与业务错误的 {"error":{"kind","message"}} 同形。
func writeAuthError(w http.ResponseWriter, status int, kind, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"kind": kind, "message": message},
	})
}
