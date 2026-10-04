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
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

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
		brand    = flag.String("brand", envStr("NOTIFY_BRAND", channel.DefaultBrand), "服务/邮件品牌名，用于邮件外壳页眉与发件人显示名")
		addr     = flag.String("addr", envStr("NOTIFY_ADDR", "127.0.0.1:8090"), "HTTP 监听地址")
		dataDir  = flag.String("data", envStr("NOTIFY_DATA_DIR", "data"), "数据目录：发送记录、outbox、默认用户表")
		webDir   = flag.String("web", envStr("NOTIFY_WEB_DIR", "web"), "测试页面目录（内含 index.html），留空则关闭")
		token    = flag.String("token", envStr("NOTIFY_TOKEN", ""), "API Token，留空表示不鉴权")
		logLevel = flag.String("log-level", envStr("NOTIFY_LOG_LEVEL", "info"), "日志级别 debug/info/warn/error")
	)
	flag.Parse()

	logger := newLogger(*logLevel)
	absData, err := filepath.Abs(*dataDir)
	if err != nil {
		return err
	}
	pageDir, pageReady := "", false
	if strings.TrimSpace(*webDir) != "" {
		if pageDir, err = filepath.Abs(*webDir); err != nil {
			return err
		}
		_, err = os.Stat(filepath.Join(pageDir, "index.html"))
		pageReady = err == nil
	}

	records, skipped, err := store.Open(filepath.Join(absData, "notifications.jsonl"), 5000)
	if err != nil {
		return err
	}
	if skipped > 0 {
		logger.Warn("加载历史记录时跳过了损坏行", "skippedLines", skipped)
	}

	// 邮件通道：配置全部来自环境变量；未配置时若要本地演示，需显式开 NOTIFY_DEV_OUTBOX=1。
	outbox := ""
	if envBool("NOTIFY_DEV_OUTBOX", false) {
		outbox = filepath.Join(absData, "outbox")
	}
	emailNotifier := channel.NewEmailNotifier(environmentSMTP(*brand), outbox, logger)

	// 短信通道：未接入真实上游时明确不可用，除非显式开 NOTIFY_SMS_SIMULATE=1 用模拟上游。
	var smsProvider channel.SMSProvider
	if envBool("NOTIFY_SMS_SIMULATE", false) {
		smsProvider = channel.NewConsoleSMSProvider(filepath.Join(absData, "outbox"), logger)
	}
	smsNotifier := channel.NewSMSNotifier(smsProvider)

	// 用户目录：优先用户服务，其次本地用户表文件。
	resolver, err := directory.New(
		envStr("NOTIFY_USER_SERVICE_URL", ""),
		envStr("NOTIFY_USER_SERVICE_PATH", directory.DefaultUserServicePath),
		envStr("NOTIFY_USER_SERVICE_TOKEN", ""),
		time.Duration(envInt("NOTIFY_USER_SERVICE_TIMEOUT", 5))*time.Second,
		envStr("NOTIFY_USERS_FILE", filepath.Join(absData, "users.json")),
	)
	if err != nil {
		return err
	}

	routes, err := notify.ParseRoutes(envStr("NOTIFY_ROUTES", ""))
	if err != nil {
		return fmt.Errorf("NOTIFY_ROUTES 无效: %w", err)
	}

	svc := notify.NewService(records, resolver, routes, logger)
	if err := svc.Register(emailNotifier); err != nil {
		return err
	}
	if err := svc.Register(smsNotifier); err != nil {
		return err
	}

	// teamusers 鉴权：配置了 NOTIFY_TEAMUSERS_URL 就由 teamusers 接管 /api/* 鉴权，静态 Token 退场。
	teamusersURL := strings.TrimSpace(envStr("NOTIFY_TEAMUSERS_URL", ""))
	teamusersAudience := envStr("NOTIFY_TEAMUSERS_AUDIENCE", "teamusers")
	teamusersPermSend := envStr("NOTIFY_TEAMUSERS_PERMISSION_SEND", httpapi.DefaultPermissionSend)
	teamusersPermRead := envStr("NOTIFY_TEAMUSERS_PERMISSION_READ", httpapi.DefaultPermissionRead)
	var teamusersAuth *httpapi.TeamusersAuth
	if teamusersURL != "" {
		serviceToken := strings.TrimSpace(envStr("NOTIFY_TEAMUSERS_SERVICE_TOKEN", ""))
		if serviceToken == "" {
			return fmt.Errorf("NOTIFY_TEAMUSERS_URL 已设置，但缺少 NOTIFY_TEAMUSERS_SERVICE_TOKEN")
		}
		teamusersAuth = httpapi.NewTeamusersAuth(httpapi.TeamusersOptions{
			BaseURL:        teamusersURL,
			Audience:       teamusersAudience,
			ServiceToken:   serviceToken,
			Timeout:        time.Duration(envInt("NOTIFY_TEAMUSERS_TIMEOUT", 5)) * time.Second,
			PermissionSend: teamusersPermSend,
			PermissionRead: teamusersPermRead,
		})
		defer teamusersAuth.Close()
	}

	api := httpapi.NewServer(httpapi.Options{
		Service: svc,
		Store:   records,
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
		"version", version, "brand", *brand, "addr", *addr, "dataDir", absData)
	logger.Info("路由规则（type → 渠道顺序）", "routes", routes.String())
	if resolver != nil {
		logger.Info("用户目录", "resolver", resolver.Describe())
	} else {
		logger.Warn("未配置用户目录：按 user 发通知会返回 503；可设 NOTIFY_USER_SERVICE_URL 或 NOTIFY_USERS_FILE")
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
