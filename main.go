// notify-service 是一个通用的多通道通知服务。
//
// 它不认识任何业务流程：调用方（任意子系统）只给三样东西——
// user（用户 id）、type（通知类型）、body（正文）；
// 发到哪个渠道、哪个地址，由本服务通过用户目录解析 + 路由规则自己决定。
//
// 所有配置都来自环境变量（可用同目录的 .env 文件，已在 .gitignore 中排除）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"notify-service/internal/channel"
	"notify-service/internal/directory"
	"notify-service/internal/httpapi"
	"notify-service/internal/notify"
	"notify-service/internal/store"
)

// version 由发布流程通过 -ldflags "-X main.version=$TAG" 注入；直接构建时为下面这个默认值。
var version = "0.3.0"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "启动失败:", err)
		os.Exit(1)
	}
}

func run() error {
	// .env 要在 flag.Parse 之前加载，这样 .env 里的 NOTIFY_ADDR 等也能当默认值。
	// 已经存在的真实环境变量优先，不会被 .env 覆盖。
	if err := loadDotEnv(dotEnvPath()); err != nil {
		fmt.Fprintln(os.Stderr, "读取 .env 失败:", err)
		os.Exit(1)
	}

	var (
		brand       = flag.String("brand", envStr("NOTIFY_BRAND", channel.DefaultBrand), "服务/邮件品牌名，用于邮件外壳页眉与发件人显示名")
		addr        = flag.String("addr", envStr("NOTIFY_ADDR", "127.0.0.1:8090"), "HTTP 监听地址")
		databaseURL = flag.String("database-url", envStr("NOTIFY_DATABASE_URL", ""), "PostgreSQL 连接串（必填），如 postgres://user:pass@host:5432/db?sslmode=disable")
		recordLimit = flag.Int("record-limit", envInt("NOTIFY_RECORD_LIMIT", 0), "发送记录保留条数：0=全部保留，>0=只保留最新 N 条")
		webDir      = flag.String("web", envStr("NOTIFY_WEB_DIR", "web"), "测试页面目录（内含 index.html），留空则关闭")
		token       = flag.String("token", envStr("NOTIFY_TOKEN", ""), "API Token，留空表示不鉴权")
		logLevel    = flag.String("log-level", envStr("NOTIFY_LOG_LEVEL", "info"), "日志级别 debug/info/warn/error")
	)
	flag.Parse()

	logger := newLogger(*logLevel)

	dsn := strings.TrimSpace(*databaseURL)
	if dsn == "" {
		logger.Error("必须配置 NOTIFY_DATABASE_URL（PostgreSQL 连接串）")
		return errors.New("必须配置 NOTIFY_DATABASE_URL（PostgreSQL 连接串）")
	}

	pageDir, pageReady := "", false
	if strings.TrimSpace(*webDir) != "" {
		absWeb, err := filepath.Abs(*webDir)
		if err != nil {
			return err
		}
		pageDir = absWeb
		_, err = os.Stat(filepath.Join(pageDir, "index.html"))
		pageReady = err == nil
	}

	// 存储：PostgreSQL 是唯一持久化存储，启动时建表（幂等）。
	openCtx, cancelOpen := context.WithTimeout(context.Background(), 10*time.Second)
	st, err := store.Open(openCtx, dsn, *recordLimit, logger)
	cancelOpen()
	if err != nil {
		return err
	}
	defer st.Close()

	// 邮件通道：配置全部来自环境变量；未配置时若要本地演示，需显式开 NOTIFY_DEV_OUTBOX=1，
	// 模拟邮件会写进 outbox_messages 表。
	var emailOutbox notify.OutboxWriter
	if envBool("NOTIFY_DEV_OUTBOX", false) {
		emailOutbox = st
	}
	emailNotifier := channel.NewEmailNotifier(environmentSMTP(*brand), emailOutbox, logger)

	// 短信通道：未接入真实上游时明确不可用，除非显式开 NOTIFY_SMS_SIMULATE=1 用模拟上游。
	var smsProvider channel.SMSProvider
	if envBool("NOTIFY_SMS_SIMULATE", false) {
		smsProvider = channel.NewConsoleSMSProvider(st, logger)
	}
	smsNotifier := channel.NewSMSNotifier(smsProvider)

	// 用户目录：优先用户服务 HTTP，其次 PostgreSQL users 表（与记录共用同一连接池）。
	resolver, err := directory.New(
		envStr("NOTIFY_USER_SERVICE_URL", ""),
		envStr("NOTIFY_USER_SERVICE_PATH", directory.DefaultUserServicePath),
		envStr("NOTIFY_USER_SERVICE_TOKEN", ""),
		time.Duration(envInt("NOTIFY_USER_SERVICE_TIMEOUT", 5))*time.Second,
		st.Pool(),
	)
	if err != nil {
		return err
	}

	routes, err := notify.ParseRoutes(envStr("NOTIFY_ROUTES", ""))
	if err != nil {
		return fmt.Errorf("NOTIFY_ROUTES 无效: %w", err)
	}

	svc := notify.NewService(st, resolver, routes, logger)
	if err := svc.Register(emailNotifier); err != nil {
		return err
	}
	if err := svc.Register(smsNotifier); err != nil {
		return err
	}

	// teamusers 鉴权：配置了 NOTIFY_TEAMUSERS_URL 就由 teamusers 接管 /api/* 鉴权，静态 Token 退场。
	teamusersURL := strings.TrimSpace(envStr("NOTIFY_TEAMUSERS_URL", ""))
	if teamusersURL != "" {
		normalized, err := normalizeTeamusersURL(teamusersURL)
		if err != nil {
			return err
		}
		teamusersURL = normalized
	}
	teamusersAudience := envStr("NOTIFY_TEAMUSERS_AUDIENCE", "teamusers")
	teamusersPermSend := envStr("NOTIFY_TEAMUSERS_PERMISSION_SEND", httpapi.DefaultPermissionSend)
	teamusersPermRead := envStr("NOTIFY_TEAMUSERS_PERMISSION_READ", httpapi.DefaultPermissionRead)
	var teamusersAuth *httpapi.TeamusersAuth
	teamusersCredMode := ""
	if teamusersURL != "" {
		serviceToken := strings.TrimSpace(envStr("NOTIFY_TEAMUSERS_SERVICE_TOKEN", ""))
		clientID := strings.TrimSpace(envStr("NOTIFY_TEAMUSERS_CLIENT_ID", ""))
		clientSecret := strings.TrimSpace(envStr("NOTIFY_TEAMUSERS_CLIENT_SECRET", ""))
		switch {
		case (clientID == "") != (clientSecret == ""):
			return fmt.Errorf("NOTIFY_TEAMUSERS_CLIENT_ID 与 NOTIFY_TEAMUSERS_CLIENT_SECRET 必须同时配置")
		case clientID == "" && serviceToken == "":
			return fmt.Errorf("NOTIFY_TEAMUSERS_URL 已设置，但缺少服务凭证：请配置 NOTIFY_TEAMUSERS_CLIENT_ID/NOTIFY_TEAMUSERS_CLIENT_SECRET（自动换取令牌），或 NOTIFY_TEAMUSERS_SERVICE_TOKEN（静态令牌）")
		}
		teamusersAuth = httpapi.NewTeamusersAuth(httpapi.TeamusersOptions{
			BaseURL:        teamusersURL,
			Audience:       teamusersAudience,
			ServiceToken:   serviceToken,
			ClientID:       clientID,
			ClientSecret:   clientSecret,
			Timeout:        time.Duration(envInt("NOTIFY_TEAMUSERS_TIMEOUT", 5)) * time.Second,
			PermissionSend: teamusersPermSend,
			PermissionRead: teamusersPermRead,
		})
		teamusersCredMode = "static-token"
		if clientID != "" {
			teamusersCredMode = "client-credentials"
		}
		if err := teamusersAuth.Prime(); err != nil {
			return fmt.Errorf("换取 teamusers 服务令牌失败: %w", err)
		}
		defer teamusersAuth.Close()
	}

	api := httpapi.NewServer(httpapi.Options{
		Service: svc,
		Store:   st,
		Email:   emailNotifier,
		WebDir:  pageDir,
		Token:   *token,
		Auth:    teamusersAuth,
		Version: version,
		Logger:  logger,
	})

	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("监听 %s 失败: %w", *addr, err)
	}

	httpSrv := &http.Server{
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		WriteTimeout:      60 * time.Second,
	}

	logger.Info("服务已启动",
		"version", version, "brand", *brand, "addr", *addr,
		"database", dbInfo(dsn), "recordLimit", *recordLimit)
	logger.Info("路由规则（type → 渠道顺序）", "routes", routes.String())
	if resolver != nil {
		logger.Info("用户目录", "resolver", resolver.Describe())
	} else {
		logger.Warn("未配置用户目录：按 user 发通知会返回 503；可设 NOTIFY_USER_SERVICE_URL 或准备 PostgreSQL users 表")
	}
	for _, st := range svc.Channels() {
		logger.Info("通道状态", "channel", st.Channel, "provider", st.Provider, "mode", st.Mode, "ready", st.Ready)
	}
	switch {
	case pageReady:
		logger.Info("测试页面已就绪", "dir", pageDir)
	case pageDir == "":
		logger.Info("测试页面已关闭，仅提供 API")
	default:
		logger.Info("未找到测试页面，API 不受影响", "expected", filepath.Join(pageDir, "index.html"))
	}
	switch {
	case teamusersAuth != nil:
		if *token != "" {
			logger.Warn("NOTIFY_TOKEN 被忽略：NOTIFY_TEAMUSERS_URL 已设置，/api/* 改由 teamusers JWT 鉴权")
		}
		logger.Info("已开启 teamusers 鉴权",
			"url", teamusersURL,
			"credential", teamusersCredMode,
			"audience", teamusersAudience,
			"sendPermission", teamusersPermSend,
			"readPermission", teamusersPermRead)
	case *token != "":
		logger.Info("已开启 API 鉴权：请求需带 Authorization: Bearer <token>")
	}
	logger.Info(`调用示例：POST /api/v1/notify {"user":"u1001","type":"alert","bodyFormat":"markdown","subject":"部署完成","body":"## 部署完成\n\n- 服务 **v1.2.3** 已上线"}`)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		logger.Info("收到退出信号，正在关闭服务")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("关闭失败: %w", err)
	}
	<-serveErr
	return nil
}

// dbInfo 返回可安全打日志的数据库位置（host/database），绝不包含账号密码。
func dbInfo(dsn string) string {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return "PostgreSQL"
	}
	host := cfg.ConnConfig.Host
	if cfg.ConnConfig.Port != 0 {
		host = net.JoinHostPort(host, strconv.Itoa(int(cfg.ConnConfig.Port)))
	}
	return "PostgreSQL " + host + "/" + cfg.ConnConfig.Database
}

// environmentSMTP 从环境变量读取发件邮箱配置。
func environmentSMTP(brand string) channel.EmailConfig {
	return channel.EmailConfig{
		Host:        envStr("NOTIFY_SMTP_HOST", ""),
		Port:        envInt("NOTIFY_SMTP_PORT", 0),
		Username:    envStr("NOTIFY_SMTP_USER", ""),
		Password:    envStr("NOTIFY_SMTP_PASS", ""),
		From:        envStr("NOTIFY_SMTP_FROM", ""),
		FromName:    envStr("NOTIFY_SMTP_FROM_NAME", ""),
		TLS:         envStr("NOTIFY_SMTP_TLS", ""),
		TimeoutSecs: envInt("NOTIFY_SMTP_TIMEOUT", 0),
		Brand:       brand,
		Footer:      envStr("NOTIFY_MAIL_FOOTER", ""),
	}
}

func dotEnvPath() string {
	if custom := strings.TrimSpace(os.Getenv("NOTIFY_ENV_FILE")); custom != "" {
		return custom
	}
	return ".env"
}

// loadDotEnv 读取 KEY=VALUE 形式的 .env；文件不存在时静默跳过。
// 已存在的环境变量优先，因此命令行/系统变量可以覆盖 .env。
func loadDotEnv(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, found := strings.Cut(line, "=")
		if !found {
			return fmt.Errorf("%s 第 %d 行缺少 '='：%s", path, i+1, line)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return fmt.Errorf("%s 第 %d 行缺少变量名", path, i+1)
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	return nil
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lv}))
}

// normalizeTeamusersURL 校验并规范化 teamusers 服务地址：允许省略 scheme（按 http:// 处理），
// 去掉结尾斜杠；其它不合法形式直接报错，避免启动后在请求构造处才报出难懂的 URL 解析错误。
func normalizeTeamusersURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("NOTIFY_TEAMUSERS_URL 不能为空")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		parsed, err = url.Parse("http://" + trimmed)
		if err != nil || parsed.Host == "" {
			return "", fmt.Errorf("NOTIFY_TEAMUSERS_URL 不是合法的服务地址: %q", raw)
		}
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("NOTIFY_TEAMUSERS_URL 只支持 http/https，收到 %q", raw)
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func envStr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return fallback
}

func envInt(key string, fallback int) int {
	raw := envStr(key, "")
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return n
}

func envBool(key string, fallback bool) bool {
	raw := strings.ToLower(envStr(key, ""))
	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return fallback
}
