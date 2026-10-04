// Package httpapi 暴露统一通知 API 与一个内置的测试控制台页面。
package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"notify-service/internal/channel"
	"notify-service/internal/notify"
	"notify-service/internal/store"
)

// Options 是构造 Server 所需的依赖。
type Options struct {
	Service *notify.Service
	Store   *store.Store
	Email   *channel.EmailNotifier
	WebDir  string
	Token   string
	Auth    *TeamusersAuth // 非空时 /api/* 走 teamusers 鉴权，Token 被忽略
	Version string
	Logger  *slog.Logger
}

// Server 持有全部 HTTP 依赖。
type Server struct {
	svc     *notify.Service
	store   *store.Store
	email   *channel.EmailNotifier
	webDir  string
	token   string
	auth    *TeamusersAuth
	version string
	log     *slog.Logger
	started time.Time
}

// NewServer 构造 HTTP 服务。控制台页面不参与编译，运行时从 WebDir 读取，
// 因此页面文件不进 git 仓库，缺失时只影响控制台，API 照常可用。
func NewServer(opt Options) *Server {
	logger := opt.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		svc:     opt.Service,
		store:   opt.Store,
		email:   opt.Email,
		webDir:  opt.WebDir,
		token:   opt.Token,
		auth:    opt.Auth,
		version: opt.Version,
		log:     logger,
		started: time.Now(),
	}
}

// Handler 返回完整的路由（含日志、CORS、鉴权、panic 恢复中间件）。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	s.RegisterRoutes(func(method, pattern string, handler http.HandlerFunc) {
		mux.HandleFunc(method+" "+pattern, handler)
	})
	// 未命中的 /api/ 路径返回 JSON 404，避免落到控制台页面。
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, notify.NotFoundf("接口 %s %s 不存在", r.Method, r.URL.Path))
	})
	mux.HandleFunc("/", s.handleConsole)

	handler := http.Handler(mux)
	handler = s.authenticate(handler)
	handler = cors(handler)
	handler = s.recoverer(handler)
	handler = s.logRequests(handler)
	return handler
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "ok",
		"version":       s.version,
		"uptimeSeconds": int(time.Since(s.started).Seconds()),
		"records":       s.store.Count(),
	})
}

func (s *Server) handleChannels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"channels":    s.svc.Channels(),
		"directory":   s.svc.Directory(),
		"routes":      s.svc.Routes(),
		"typeRouting": "调用方只给 user + type + 内容，渠道由这里的路由规则决定",
	})
}

type notifyRequest struct {
	User       string            `json:"user"`
	Channel    string            `json:"channel"`
	To         []string          `json:"to"`
	Target     *notify.Target    `json:"target"`
	Type       string            `json:"type"`
	Subject    string            `json:"subject"`
	Body       string            `json:"body"`
	BodyFormat string            `json:"bodyFormat"`
	Meta       map[string]string `json:"meta"`

	// 已移除的字段保留成指针，只为给出明确的 400，而不是被当成未知字段或静默忽略。
	HTML     *string `json:"html"`
	Markdown *string `json:"markdown"`
}

// validateBody 保证正文只有一个来源：Body（+ bodyFormat）。
func (r notifyRequest) validateBody() error {
	if r.HTML != nil {
		return notify.Invalidf("html 字段已移除：正文统一为 Markdown 单源，请改用 body + \"bodyFormat\":\"markdown\"")
	}
	if r.Markdown != nil {
		return notify.Invalidf("markdown 字段已改名为 body：请改用 body + \"bodyFormat\":\"markdown\"")
	}
	if r.BodyFormat != "" && r.BodyFormat != string(notify.BodyFormatText) && r.BodyFormat != string(notify.BodyFormatMarkdown) {
		return notify.Invalidf("bodyFormat 只能是 text 或 markdown，收到 %q", r.BodyFormat)
	}
	return nil
}

func (s *Server) handleNotify(w http.ResponseWriter, r *http.Request) {
	var req notifyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, err)
		return
	}
	if err := req.validateBody(); err != nil {
		writeError(w, err)
		return
	}
	rec, err := s.svc.Send(r.Context(), notify.Message{
		User:       req.User,
		Channel:    notify.Channel(req.Channel),
		To:         req.To,
		Target:     req.Target,
		Type:       req.Type,
		Subject:    req.Subject,
		Body:       req.Body,
		BodyFormat: notify.BodyFormat(req.BodyFormat),
		Meta:       req.Meta,
	})
	if err != nil {
		status, _ := httpStatus(err)
		writeJSON(w, status, map[string]any{"ok": false, "record": rec, "error": errorBody(err)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "record": rec})
}

func (s *Server) handleListNotifications(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	limit, _ := strconv.Atoi(query.Get("limit"))
	records := s.store.List(store.Filter{
		Channel: query.Get("channel"),
		Type:    query.Get("type"),
		UserID:  query.Get("userId"),
		Status:  query.Get("status"),
		Limit:   limit,
	})
	writeJSON(w, http.StatusOK, map[string]any{"records": records, "total": s.store.Count()})
}

func (s *Server) handleGetNotification(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.store.Get(r.PathValue("id"))
	if !ok {
		writeError(w, notify.NotFoundf("记录 %q 不存在", r.PathValue("id")))
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (s *Server) handleVerifyEmail(w http.ResponseWriter, r *http.Request) {
	var req struct {
		To []string `json:"to"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, err)
		return
	}
	rec, err := s.svc.Send(r.Context(), s.email.VerifyMessage(req.To))
	if err != nil {
		status, _ := httpStatus(err)
		writeJSON(w, status, map[string]any{"ok": false, "record": rec, "error": errorBody(err)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "record": rec})
}

func (s *Server) handleConsole(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/", "/index.html":
		s.serveConsole(w)
	case "/favicon.ico":
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, notify.NotFoundf("路径 %s 不存在", r.URL.Path))
	}
}

// serveConsole 每次请求都从磁盘读页面：页面不参与编译，改完刷新浏览器即生效。
func (s *Server) serveConsole(w http.ResponseWriter) {
	if s.webDir == "" {
		writeError(w, notify.NotFoundf("控制台页面未启用（-web 传入空值）；API 不受影响"))
		return
	}
	path := filepath.Join(s.webDir, "index.html")
	page, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, notify.NotFoundf("控制台页面未安装：把 index.html 放到 %s 后刷新即可；API 不受影响", path))
			return
		}
		writeError(w, fmt.Errorf("读取控制台页面 %s 失败: %w", path, err))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(page)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	// 严格模式：字段名写错会立刻报错，而不是被静默忽略后发出空通知。
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return notify.Invalidf("请求体解析失败: %v", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
}

func writeError(w http.ResponseWriter, err error) {
	status, _ := httpStatus(err)
	writeJSON(w, status, map[string]any{"error": errorBody(err)})
}

func errorBody(err error) map[string]any {
	kind := "internal_error"
	var nerr *notify.Error
	if errors.As(err, &nerr) {
		kind = string(nerr.Kind)
	}
	return map[string]any{"kind": kind, "message": err.Error()}
}

func httpStatus(err error) (int, string) {
	kind := "internal_error"
	var nerr *notify.Error
	if !errors.As(err, &nerr) {
		return http.StatusInternalServerError, kind
	}
	kind = string(nerr.Kind)
	switch nerr.Kind {
	case notify.KindInvalid:
		return http.StatusBadRequest, kind
	case notify.KindNotFound:
		return http.StatusNotFound, kind
	case notify.KindNotReady:
		return http.StatusServiceUnavailable, kind
	case notify.KindDelivery, notify.KindUpstream:
		return http.StatusBadGateway, kind
	default:
		return http.StatusInternalServerError, kind
	}
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	if s.auth == nil && s.token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		if s.auth != nil {
			s.authenticateTeamusers(next, w, r)
			return
		}
		got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if got == "" {
			got = strings.TrimSpace(r.Header.Get("X-Notify-Token"))
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			writeAuthError(w, http.StatusUnauthorized, "unauthorized", "缺少或错误的 API Token（Authorization: Bearer <token>）")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authenticateTeamusers 校验 teamusers JWT，并按已知路由追加权限校验。
func (s *Server) authenticateTeamusers(next http.Handler, w http.ResponseWriter, r *http.Request) {
	raw, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		writeAuthError(w, http.StatusUnauthorized, "unauthorized", "缺少 Authorization: Bearer <JWT>")
		return
	}
	claims, err := s.auth.Verify(r.Context(), raw)
	if err != nil {
		s.log.Debug("JWT 校验失败", "err", err)
		writeAuthError(w, http.StatusUnauthorized, "unauthorized", "JWT 无效或已过期")
		return
	}
	permission := s.auth.RequiredPermission(r.Method, r.URL.Path)
	if permission == "" {
		next.ServeHTTP(w, r)
		return
	}
	if allowed, reason := s.auth.Allow(r.Context(), claims, permission); !allowed {
		writeAuthError(w, http.StatusForbidden, "forbidden", fmt.Sprintf("权限不足：需要 %s（%s）", permission, reason))
		return
	}
	next.ServeHTTP(w, r)
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Notify-Token")
		h.Set("Access-Control-Max-Age", "600")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("请求处理 panic", "method", r.Method, "path", r.URL.Path, "panic", fmt.Sprint(v), "stack", string(debug.Stack()))
				writeError(w, fmt.Errorf("服务内部错误: %v", v))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Debug("http",
			"method", r.Method, "path", r.URL.Path, "status", rec.status,
			"durationMs", time.Since(started).Milliseconds(), "remote", r.RemoteAddr)
	})
}
