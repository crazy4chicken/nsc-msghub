package directory

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"notify-service/internal/notify"
)

// DefaultUserServicePath 是默认的用户查询路径，{id} 会被替换成用户 id。
const DefaultUserServicePath = "/api/users/{id}"

// HTTPResolver 通过用户服务的 HTTP 接口解析用户，是"跟用户服务通信拿地址"的实现。
//
// 约定（用户服务返回 200 + JSON）：
//
//	{"id":"u1001","name":"张三","channels":{"email":"zhangsan@example.com","sms":"13800000000"}}
//
// 也兼容把地址直接放在顶层：{"email":"...","sms":"...","phone":"..."}。
// 用户不存在请返回 404；其它非 2xx 会按上游故障处理（502 upstream_failed）。
type HTTPResolver struct {
	baseURL string
	path    string
	token   string
	client  *http.Client
}

// NewHTTPResolver 创建用户服务解析器；baseURL 为空时返回 nil。
func NewHTTPResolver(baseURL, path, token string, timeout time.Duration) (*HTTPResolver, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, nil
	}
	if _, err := url.Parse(baseURL); err != nil {
		return nil, notify.Invalidf("NOTIFY_USER_SERVICE_URL 不是合法地址: %v", err)
	}
	if strings.TrimSpace(path) == "" {
		path = DefaultUserServicePath
	}
	if !strings.Contains(path, "{id}") {
		return nil, notify.Invalidf("用户服务路径 %q 需要包含 {id} 占位符，例如 %s", path, DefaultUserServicePath)
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &HTTPResolver{
		baseURL: baseURL,
		path:    path,
		token:   strings.TrimSpace(token),
		client:  &http.Client{Timeout: timeout},
	}, nil
}

// Describe 实现 notify.Resolver。
func (r *HTTPResolver) Describe() string {
	return "用户服务 " + r.baseURL + r.path
}

// Resolve 实现 notify.Resolver。
func (r *HTTPResolver) Resolve(ctx context.Context, userID string) (notify.User, error) {
	endpoint := r.baseURL + strings.ReplaceAll(r.path, "{id}", url.PathEscape(userID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return notify.User{}, notify.Upstreamf("构造用户服务请求失败: %v", err)
	}
	req.Header.Set("Accept", "application/json")
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return notify.User{}, notify.Upstreamf("请求用户服务失败（%s）: %v", endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return notify.User{}, notify.Upstreamf("读取用户服务响应失败: %v", err)
	}

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return notify.User{}, notify.NotFoundf("用户 %q 在用户服务里不存在（%s 返回 404）", userID, endpoint)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return notify.User{}, notify.Upstreamf("用户服务返回 %d: %s", resp.StatusCode, snippet(body))
	}

	var payload userPayload
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	if err := decoder.Decode(&payload); err != nil {
		return notify.User{}, notify.Upstreamf("用户服务返回的不是合法 JSON（%s）: %v", endpoint, err)
	}
	if payload.ID != "" && payload.ID != userID {
		return notify.User{}, notify.Upstreamf("用户服务返回的 id %q 与请求的 %q 不一致", payload.ID, userID)
	}
	return payload.toUser(userID), nil
}

type userPayload struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Channels map[string]string `json:"channels"`
	Email    string            `json:"email"`
	SMS      string            `json:"sms"`
	Phone    string            `json:"phone"`
}

func (p userPayload) toUser(userID string) notify.User {
	user := notify.User{
		ID:       userID,
		Name:     strings.TrimSpace(p.Name),
		Channels: map[string]string{},
	}
	for name, addr := range p.Channels {
		if addr = strings.TrimSpace(addr); addr != "" {
			user.Channels[strings.ToLower(strings.TrimSpace(name))] = addr
		}
	}
	// 顶层写法当补充：channels 里没写才采用。
	for name, addr := range map[string]string{
		"email": p.Email,
		"sms":   firstNonEmpty(p.SMS, p.Phone),
	} {
		if addr = strings.TrimSpace(addr); addr == "" {
			continue
		}
		if _, exists := user.Channels[name]; !exists {
			user.Channels[name] = addr
		}
	}
	return user
}

func snippet(body []byte) string {
	text := strings.Join(strings.Fields(string(body)), " ")
	if len(text) > 200 {
		return text[:200] + "…"
	}
	if text == "" {
		return "(空响应体)"
	}
	return text
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// New 按配置选择解析器：优先用户服务，其次 PostgreSQL 用户表，都没有时返回 nil。
func New(baseURL, path, token string, timeout time.Duration, q Querier) (notify.Resolver, error) {
	remote, err := NewHTTPResolver(baseURL, path, token, timeout)
	if err != nil {
		return nil, err
	}
	if remote != nil {
		return remote, nil
	}
	if local := NewPGResolver(q); local != nil {
		return local, nil
	}
	return nil, nil
}
