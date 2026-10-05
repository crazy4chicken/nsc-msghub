package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// tokenRefreshSkew 是提前刷新的时间窗口，避免在令牌即将过期的边界上发起权限查询。
	tokenRefreshSkew = 30 * time.Second
	// defaultServiceTokenTTL 是 teamusers 响应缺少 expires_in 时的兜底有效期（teamusers 默认 10 分钟）。
	defaultServiceTokenTTL = 10 * time.Minute
)

// clientCredentialsTokenSource 用 teamusers 的 client-credentials 换取服务访问令牌，并在到期前自动刷新。
// SDK 会在每次权限查询/远程检查前调用 Token，所以这里必须缓存：否则每个请求都会触发一次签发
// （并同步做一次 Argon2id 校验）。
type clientCredentialsTokenSource struct {
	baseURL  string
	clientID string
	secret   string
	client   *http.Client

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

func newClientCredentialsTokenSource(baseURL, clientID, secret string, client *http.Client) *clientCredentialsTokenSource {
	return &clientCredentialsTokenSource{
		baseURL:  strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		clientID: strings.TrimSpace(clientID),
		secret:   strings.TrimSpace(secret),
		client:   client,
	}
}

// Token 返回可用的服务访问令牌：缓存未临近过期时复用，否则重新走 client-credentials 签发。
func (s *clientCredentialsTokenSource) Token() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && time.Now().Before(s.expiresAt.Add(-tokenRefreshSkew)) {
		return s.token, nil
	}
	token, ttl, err := s.issue()
	if err != nil {
		return "", err
	}
	s.token = token
	s.expiresAt = time.Now().Add(ttl)
	return token, nil
}

// Prime 在启动时预取一次令牌，让凭证或网络问题尽早暴露。
func (s *clientCredentialsTokenSource) Prime() error {
	_, err := s.Token()
	return err
}

type clientCredentialsRequest struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

type clientCredentialsResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
}

// issue 调 POST /auth/client-credentials 换取服务访问令牌。
func (s *clientCredentialsTokenSource) issue() (string, time.Duration, error) {
	payload, err := json.Marshal(clientCredentialsRequest{ClientID: s.clientID, ClientSecret: s.secret})
	if err != nil {
		return "", 0, fmt.Errorf("编码 client-credentials 请求失败: %w", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, s.baseURL+"/auth/client-credentials", bytes.NewReader(payload))
	if err != nil {
		return "", 0, fmt.Errorf("构造 client-credentials 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("请求 teamusers 签发服务令牌失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		return "", 0, fmt.Errorf("读取 teamusers 响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("teamusers 签发服务令牌失败（HTTP %d）：%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var decoded clientCredentialsResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", 0, fmt.Errorf("解析 teamusers 响应失败: %w", err)
	}
	if strings.TrimSpace(decoded.AccessToken) == "" {
		return "", 0, errors.New("teamusers 响应缺少 access_token")
	}
	ttl := time.Duration(decoded.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = defaultServiceTokenTTL
	}
	return decoded.AccessToken, ttl, nil
}
