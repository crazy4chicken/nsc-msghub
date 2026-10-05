package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"notify-service/internal/notify"
	"notify-service/internal/store"
)

const (
	testServiceToken = "svc-token"
	testKeyID        = "auth-test-key"
	testAudience     = "teamusers"
)

// teamusersStub 模拟 teamusers 的 JWKS、client-credentials、权限查询与远程授权检查接口。
type teamusersStub struct {
	*httptest.Server
	privateKey ed25519.PrivateKey

	mu         sync.Mutex
	credID     string
	credSecret string
	credTTL    int64
	credCalls  int
}

// setClientCredentials 配置 stub 接受的服务账号凭证与签发的令牌有效期（秒）。
func (s *teamusersStub) setClientCredentials(id, secret string, ttlSeconds int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.credID, s.credSecret, s.credTTL = id, secret, ttlSeconds
}

// credentialCalls 返回 client-credentials 被调用的次数。
func (s *teamusersStub) credentialCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.credCalls
}

func newTeamusersStub(t *testing.T) *teamusersStub {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成 ed25519 密钥失败: %v", err)
	}
	stub := &teamusersStub{privateKey: privateKey}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "OKP",
				"crv": "Ed25519",
				"alg": "EdDSA",
				"use": "sig",
				"kid": testKeyID,
				"x":   base64.RawURLEncoding.EncodeToString(publicKey),
			}},
		})
	})
	mux.HandleFunc("POST /auth/client-credentials", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		stub.mu.Lock()
		stub.credCalls++
		expectedID, expectedSecret, ttlSeconds := stub.credID, stub.credSecret, stub.credTTL
		stub.mu.Unlock()
		if expectedID == "" || request.ClientID != expectedID || request.ClientSecret != expectedSecret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		token, err := stub.signJWT("msghub-service", "service", time.Duration(ttlSeconds)*time.Second)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  token,
			"token_type":    "Bearer",
			"expires_in":    ttlSeconds,
			"refresh_token": "stub-refresh-token",
		})
	})
	mux.HandleFunc("GET /authz/permissions/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !stub.acceptServiceBearer(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var grants []map[string]string
		switch r.PathValue("id") {
		case "granted":
			grants = append(grants, map[string]string{"key": "msghub:send:any"}, map[string]string{"key": "msghub:read:any"})
		case "denied":
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"user_id":  r.PathValue("id"),
			"perm_ver": 0,
			"grants":   grants,
		})
	})
	mux.HandleFunc("POST /authz/check", func(w http.ResponseWriter, r *http.Request) {
		if !stub.acceptServiceBearer(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"allow": false, "reason": "stub 不做远程放行"})
	})

	stub.Server = httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	return stub
}

// sign 用测试私钥签一个 kind=user 的 EdDSA JWT；claims 按 SDK 的校验要求给全。
func (s *teamusersStub) sign(t *testing.T, subject string) string {
	t.Helper()
	token, err := s.signJWT(subject, "user", 5*time.Minute)
	if err != nil {
		t.Fatalf("签发测试 JWT 失败: %v", err)
	}
	return token
}

// signJWT 用测试私钥签一个 EdDSA JWT；claims 按 SDK 的校验要求给全。
func (s *teamusersStub) signJWT(subject, kind string, ttl time.Duration) (string, error) {
	headerJSON, err := json.Marshal(map[string]any{"alg": "EdDSA", "kid": testKeyID, "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claimsJSON, err := json.Marshal(map[string]any{
		"iss":      testAudience,
		"aud":      testAudience,
		"exp":      time.Now().Add(ttl).Unix(),
		"sub":      subject,
		"kind":     kind,
		"perm_ver": 0,
	})
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	signature := ed25519.Sign(s.privateKey, []byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// acceptServiceBearer 接受静态测试令牌，或任何由本 stub 签发且 kind=service 的 JWT。
func (s *teamusersStub) acceptServiceBearer(r *http.Request) bool {
	fields := strings.Fields(r.Header.Get("Authorization"))
	if len(fields) != 2 || !strings.EqualFold(fields[0], "bearer") {
		return false
	}
	if fields[1] == testServiceToken {
		return true
	}
	parts := strings.Split(fields[1], ".")
	if len(parts) != 3 {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Kind != "service" {
		return false
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	publicKey, ok := s.privateKey.Public().(ed25519.PublicKey)
	if !ok {
		return false
	}
	return ed25519.Verify(publicKey, []byte(parts[0]+"."+parts[1]), signature)
}

// memRecords 是 httpapi.Records 接口的内存假实现，查询语义对齐 store：
// 记录按写入顺序保存，List 返回时间倒序（Limit<=0 默认 100，>5000 截到 5000）。
type memRecords struct {
	records []notify.Record
}

func (m *memRecords) Get(_ context.Context, id string) (notify.Record, bool, error) {
	for i := len(m.records) - 1; i >= 0; i-- {
		if m.records[i].ID == id {
			return m.records[i], true, nil
		}
	}
	return notify.Record{}, false, nil
}

func (m *memRecords) List(_ context.Context, f store.Filter) ([]notify.Record, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	} else if limit > 5000 {
		limit = 5000
	}
	out := make([]notify.Record, 0, limit)
	for i := len(m.records) - 1; i >= 0 && len(out) < limit; i-- {
		rec := m.records[i]
		if f.Channel != "" && string(rec.Channel) != f.Channel {
			continue
		}
		if f.Type != "" && rec.Type != f.Type {
			continue
		}
		if f.UserID != "" && rec.UserID != f.UserID {
			continue
		}
		if f.Status != "" && rec.Status != f.Status {
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

func (m *memRecords) Count(_ context.Context) (int, error) {
	return len(m.records), nil
}

// fakeResolver 是 notify.Resolver 的内存假实现：按 users map 解析用户。
type fakeResolver struct {
	users map[string]notify.User
}

func (r *fakeResolver) Describe() string {
	return "测试用户表（内存）"
}

func (r *fakeResolver) Resolve(_ context.Context, userID string) (notify.User, error) {
	user, ok := r.users[userID]
	if !ok {
		return notify.User{}, notify.NotFoundf("用户 %q 不存在", userID)
	}
	return user, nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testService 构造一个不落记录的最小 Service，供 HTTP 处理器用例使用。
func testService(t *testing.T, resolver notify.Resolver) *notify.Service {
	t.Helper()
	routes, err := notify.ParseRoutes("default=email")
	if err != nil {
		t.Fatalf("解析路由失败: %v", err)
	}
	return notify.NewService(nil, resolver, routes, discardLogger())
}

// newTestServer 构造一个可直接打请求的 Handler：记录库与用户目录用内存假实现，日志丢弃。
func newTestServer(t *testing.T, opt Options) http.Handler {
	t.Helper()
	opt.Service = testService(t, &fakeResolver{users: map[string]notify.User{
		"granted": {ID: "granted", Name: "已授权", Channels: map[string]string{"email": "granted@example.com"}},
	}})
	opt.Store = &memRecords{}
	opt.Logger = discardLogger()
	return NewServer(opt).Handler()
}

type httpResult struct {
	status  int
	payload map[string]any
}

func (r httpResult) kind(t *testing.T) string {
	t.Helper()
	body, _ := r.payload["error"].(map[string]any)
	kind, _ := body["kind"].(string)
	return kind
}

func call(t *testing.T, handler http.Handler, method, path, body string, headers map[string]string) httpResult {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var payload map[string]any
	if strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("%s %s: 解析响应失败: %v（%s）", method, path, err, rec.Body.String())
		}
	}
	return httpResult{status: rec.Code, payload: payload}
}

// tamperSignature 改写签名段的首字符（参与真实比特编码），保证验签必然失败。
func tamperSignature(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[2] == "" {
		return token + "x"
	}
	signature := parts[2]
	replacement := "A"
	if signature[0] == 'A' {
		replacement = "B"
	}
	parts[2] = replacement + signature[1:]
	return strings.Join(parts, ".")
}

// TestTeamusersAuth 覆盖启用 teamusers 后的可观察契约：
// 无/坏 JWT 401、权限不足 403、有权限时到达处理器、未知 /api/ 路径只认证、/healthz 公开。
func TestTeamusersAuth(t *testing.T) {
	stub := newTeamusersStub(t)
	handler := newTestServer(t, Options{
		Auth: NewTeamusersAuth(TeamusersOptions{
			BaseURL:        stub.URL,
			Audience:       testAudience,
			ServiceToken:   testServiceToken,
			Timeout:        5 * time.Second,
			PermissionSend: "msghub:send:any",
			PermissionRead: "msghub:read:any",
		}),
	})
	granted := stub.sign(t, "granted")
	denied := stub.sign(t, "denied")

	cases := []struct {
		name       string
		method     string
		path       string
		auth       string
		body       string
		wantStatus int
		wantKind   string
	}{
		{name: "无 JWT 返回 401", method: "GET", path: "/api/v1/notifications", wantStatus: 401, wantKind: "unauthorized"},
		{name: "坏 JWT 返回 401", method: "GET", path: "/api/v1/notifications", auth: "Bearer not-a-jwt", wantStatus: 401, wantKind: "unauthorized"},
		{name: "签名被篡改返回 401", method: "GET", path: "/api/v1/notifications", auth: "Bearer " + tamperSignature(granted), wantStatus: 401, wantKind: "unauthorized"},
		{name: "无发送权限返回 403", method: "POST", path: "/api/v1/notify", auth: "Bearer " + denied, body: `{"html":"x"}`, wantStatus: 403, wantKind: "forbidden"},
		{name: "有读权限放行到处理器", method: "GET", path: "/api/v1/notifications", auth: "Bearer " + granted, wantStatus: 200},
		{name: "有发送权限放行到处理器", method: "POST", path: "/api/v1/notify", auth: "Bearer " + granted, body: `{"html":"x"}`, wantStatus: 400, wantKind: "invalid_request"},
		{name: "未知 /api/ 路径认证后 404", method: "GET", path: "/api/v1/nope", auth: "Bearer " + granted, wantStatus: 404, wantKind: "not_found"},
		{name: "未知 /api/ 路径仍要认证", method: "GET", path: "/api/v1/nope", wantStatus: 401, wantKind: "unauthorized"},
		{name: "/healthz 保持公开", method: "GET", path: "/healthz", wantStatus: 200},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			headers := map[string]string{}
			if tc.auth != "" {
				headers["Authorization"] = tc.auth
			}
			got := call(t, handler, tc.method, tc.path, tc.body, headers)
			if got.status != tc.wantStatus {
				t.Fatalf("状态码 = %d，期望 %d，响应 %v", got.status, tc.wantStatus, got.payload)
			}
			if tc.wantKind != "" && got.kind(t) != tc.wantKind {
				t.Fatalf("kind = %q，期望 %q，响应 %v", got.kind(t), tc.wantKind, got.payload)
			}
		})
	}

	t.Run("403 消息带权限与 SDK 原因", func(t *testing.T) {
		got := call(t, handler, "POST", "/api/v1/notify", `{"html":"x"}`, map[string]string{"Authorization": "Bearer " + denied})
		body, _ := got.payload["error"].(map[string]any)
		message, _ := body["message"].(string)
		if !strings.Contains(message, "msghub:send:any") || !strings.Contains(message, "no matching grant") {
			t.Fatalf("message = %q，应包含权限与 SDK 原因", message)
		}
	})

	t.Run("有权限读接口返回空记录", func(t *testing.T) {
		got := call(t, handler, "GET", "/api/v1/notifications", "", map[string]string{"Authorization": "Bearer " + granted})
		if got.payload["records"] == nil || got.payload["total"].(float64) != 0 {
			t.Fatalf("响应 = %v，期望空记录列表", got.payload)
		}
	})
}

// clientCredentialsAuth 构造 client-credentials 模式的鉴权组件。
func clientCredentialsAuth(stub *teamusersStub, id, secret string) *TeamusersAuth {
	return NewTeamusersAuth(TeamusersOptions{
		BaseURL:        stub.URL,
		Audience:       testAudience,
		ClientID:       id,
		ClientSecret:   secret,
		Timeout:        5 * time.Second,
		PermissionSend: "msghub:send:any",
		PermissionRead: "msghub:read:any",
	})
}

// TestTeamusersClientCredentials 覆盖 client-credentials 模式：自动换取服务令牌、缓存复用、
// 临期自动刷新、凭证错误时启动失败并按 403 拒绝。
func TestTeamusersClientCredentials(t *testing.T) {
	t.Run("令牌缓存复用", func(t *testing.T) {
		stub := newTeamusersStub(t)
		stub.setClientCredentials("msghub", "s3cret", 3600)
		handler := newTestServer(t, Options{Auth: clientCredentialsAuth(stub, "msghub", "s3cret")})

		// 两个不同用户各自查一次权限；服务令牌只应换取一次。
		for _, tc := range []struct {
			subject    string
			wantStatus int
		}{{"granted", 200}, {"denied", 403}} {
			got := call(t, handler, "GET", "/api/v1/notifications", "", map[string]string{"Authorization": "Bearer " + stub.sign(t, tc.subject)})
			if got.status != tc.wantStatus {
				t.Fatalf("%s 状态码 = %d，期望 %d，响应 %v", tc.subject, got.status, tc.wantStatus, got.payload)
			}
		}
		if calls := stub.credentialCalls(); calls != 1 {
			t.Fatalf("client-credentials 调用次数 = %d，期望 1（令牌应缓存复用）", calls)
		}
	})

	t.Run("临期令牌自动刷新", func(t *testing.T) {
		stub := newTeamusersStub(t)
		stub.setClientCredentials("msghub", "s3cret", 1) // 有效期小于 30 秒提前刷新窗口
		handler := newTestServer(t, Options{Auth: clientCredentialsAuth(stub, "msghub", "s3cret")})

		for _, subject := range []string{"granted", "denied"} {
			got := call(t, handler, "GET", "/api/v1/notifications", "", map[string]string{"Authorization": "Bearer " + stub.sign(t, subject)})
			if got.status != 200 && got.status != 403 {
				t.Fatalf("%s 状态码 = %d，响应 %v", subject, got.status, got.payload)
			}
		}
		if calls := stub.credentialCalls(); calls < 2 {
			t.Fatalf("client-credentials 调用次数 = %d，期望至少 2（临期令牌应重新签发）", calls)
		}
	})

	t.Run("凭证错误启动失败并按 403 拒绝", func(t *testing.T) {
		stub := newTeamusersStub(t)
		stub.setClientCredentials("msghub", "s3cret", 3600)
		auth := clientCredentialsAuth(stub, "msghub", "wrong-secret")
		if err := auth.Prime(); err == nil {
			t.Fatal("Prime() 应因凭证错误返回错误")
		}
		handler := newTestServer(t, Options{Auth: auth})

		got := call(t, handler, "GET", "/api/v1/notifications", "", map[string]string{"Authorization": "Bearer " + stub.sign(t, "granted")})
		if got.status != 403 {
			t.Fatalf("状态码 = %d，期望 403，响应 %v", got.status, got.payload)
		}
		body, _ := got.payload["error"].(map[string]any)
		message, _ := body["message"].(string)
		if !strings.Contains(message, "authorization service unavailable") {
			t.Fatalf("message = %q，应包含 SDK 给出的原因", message)
		}
	})

	t.Run("Prime 预取令牌", func(t *testing.T) {
		stub := newTeamusersStub(t)
		stub.setClientCredentials("msghub", "s3cret", 3600)
		if err := clientCredentialsAuth(stub, "msghub", "s3cret").Prime(); err != nil {
			t.Fatalf("Prime() 返回错误: %v", err)
		}
		if calls := stub.credentialCalls(); calls != 1 {
			t.Fatalf("client-credentials 调用次数 = %d，期望 1", calls)
		}
	})
}

// TestStaticTokenAuthUnchanged 确认未配置 teamusers 时静态 Token（或完全不鉴权）的行为不变。
func TestStaticTokenAuthUnchanged(t *testing.T) {
	handler := newTestServer(t, Options{Token: "s3cret"})

	cases := []struct {
		name       string
		headers    map[string]string
		wantStatus int
		wantKind   string
	}{
		{name: "无 Token 返回 401", wantStatus: 401, wantKind: "unauthorized"},
		{name: "错误 Token 返回 401", headers: map[string]string{"Authorization": "Bearer wrong"}, wantStatus: 401, wantKind: "unauthorized"},
		{name: "正确 Bearer 放行", headers: map[string]string{"Authorization": "Bearer s3cret"}, wantStatus: 200},
		{name: "X-Notify-Token 回退仍有效", headers: map[string]string{"X-Notify-Token": "s3cret"}, wantStatus: 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := call(t, handler, "GET", "/api/v1/notifications", "", tc.headers)
			if got.status != tc.wantStatus {
				t.Fatalf("状态码 = %d，期望 %d，响应 %v", got.status, tc.wantStatus, got.payload)
			}
			if tc.wantKind != "" && got.kind(t) != tc.wantKind {
				t.Fatalf("kind = %q，期望 %q", got.kind(t), tc.wantKind)
			}
		})
	}

	t.Run("未配置鉴权时保持公开", func(t *testing.T) {
		open := newTestServer(t, Options{})
		if got := call(t, open, "GET", "/api/v1/notifications", "", nil); got.status != 200 {
			t.Fatalf("状态码 = %d，期望 200", got.status)
		}
	})
}
