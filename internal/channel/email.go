// Package channel 实现具体通知通道。
//   - email：真实 SMTP 投递（465 隐式 TLS / 587 STARTTLS / 明文），配置全部来自环境变量；
//   - sms：  统一的 SMSProvider 抽象，未接入真实上游时明确报"不可用"，而不是假装成功。
package channel

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"notify-service/internal/notify"
)

// SMTP 加密模式。
const (
	TLSModeAuto     = "auto"     // 服务器支持 STARTTLS 就用（默认）
	TLSModeSTARTTLS = "starttls" // 强制 STARTTLS，服务器不支持则报错
	TLSModeImplicit = "implicit" // 隐式 TLS，一般用于 465 端口
	TLSModeNone     = "none"     // 明文，仅用于本地 MailHog 之类
)

const devFrom = "notify-dev@localhost"

// DefaultBrand 是邮件外壳页眉与默认发件人显示名的兜底值，可用 NOTIFY_BRAND 覆盖。
const DefaultBrand = "notify-service"

// emailShell 把正文片段套成完整邮件：浅灰底 + 白色卡片 + 页眉页脚（品牌可配置）。
// 邮件客户端普遍会剥掉 <style>，所以外壳样式必须内联。
func emailShell(fragment, brand, footer string) string {
	// 注意用单引号：整段 style 已经用双引号包住，里面再用双引号会把属性截断。
	const font = `font-family:-apple-system,'Segoe UI','Microsoft YaHei',sans-serif`
	return `<div style="margin:0;padding:24px 12px;background:#f4f6f9">` +
		`<div style="max-width:640px;margin:0 auto;background:#ffffff;border:1px solid #e6e9ef;border-radius:12px;overflow:hidden">` +
		`<div style="padding:14px 24px;background:#1f2430;color:#ffffff;` + font + `;font-size:15px;font-weight:600">` + escapeShellText(brand) + `</div>` +
		`<div style="padding:20px 24px;color:#222222;` + font + `;font-size:14px;line-height:1.75">` + fragment + `</div>` +
		`<div style="padding:12px 24px;background:#fafbfd;color:#8a909c;` + font + `;font-size:12px">` + escapeShellText(footer) + `</div>` +
		`</div></div>`
}

// escapeShellText 转义来自配置的文案，避免品牌名里的字符破坏外壳结构。
func escapeShellText(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

// EmailConfig 是发件邮箱的全部配置，字段与 NOTIFY_SMTP_* 环境变量一一对应。
type EmailConfig struct {
	Host        string
	Port        int
	Username    string
	Password    string
	From        string
	FromName    string
	TLS         string
	TimeoutSecs int

	// Brand 显示在邮件外壳页眉，同时也是发件人显示名的兜底值。
	Brand string
	// Footer 是邮件外壳页脚的提示语，留空则按 Brand 生成。
	Footer string
}

// WithDefaults 补齐缺省值，方便 main 里只写关心的几项。
func (c EmailConfig) WithDefaults() EmailConfig {
	c.Host = strings.TrimSpace(c.Host)
	c.Username = strings.TrimSpace(c.Username)
	c.From = strings.TrimSpace(c.From)
	c.FromName = strings.TrimSpace(c.FromName)
	c.TLS = strings.ToLower(strings.TrimSpace(c.TLS))
	if c.Port == 0 {
		c.Port = 587
	}
	if c.TLS == "" {
		c.TLS = TLSModeAuto
	}
	if c.From == "" {
		c.From = c.Username
	}
	if c.TimeoutSecs <= 0 {
		c.TimeoutSecs = 15
	}
	c.Brand = strings.TrimSpace(c.Brand)
	if c.Brand == "" {
		c.Brand = DefaultBrand
	}
	c.Footer = strings.TrimSpace(c.Footer)
	if c.Footer == "" {
		c.Footer = "本邮件由 " + c.Brand + " 自动发送，请勿直接回复。"
	}
	if c.FromName == "" {
		c.FromName = c.Brand
	}
	return c
}

// Validate 校验配置是否可用。
func (c EmailConfig) Validate() error {
	if c.Host == "" {
		return notify.Invalidf("未配置发件邮箱：请设置 NOTIFY_SMTP_HOST")
	}
	if c.Port < 1 || c.Port > 65535 {
		return notify.Invalidf("NOTIFY_SMTP_PORT 非法：%d", c.Port)
	}
	switch c.TLS {
	case TLSModeAuto, TLSModeSTARTTLS, TLSModeImplicit, TLSModeNone:
	default:
		return notify.Invalidf("NOTIFY_SMTP_TLS 只能是 auto / starttls / implicit / none，收到 %q", c.TLS)
	}
	if c.From == "" {
		return notify.Invalidf("未配置发件人：请设置 NOTIFY_SMTP_FROM，或让它等于 NOTIFY_SMTP_USER")
	}
	if strings.ContainsAny(c.From, "\r\n") || !strings.Contains(c.From, "@") {
		return notify.Invalidf("NOTIFY_SMTP_FROM %q 不是合法邮箱地址", c.From)
	}
	if strings.ContainsAny(c.FromName, "\r\n") {
		return notify.Invalidf("NOTIFY_SMTP_FROM_NAME 不能包含回车/换行")
	}
	if c.Username != "" && c.Password == "" && c.TLS != TLSModeNone {
		return notify.Invalidf("配置了 NOTIFY_SMTP_USER 却没有 NOTIFY_SMTP_PASS（QQ/163 等需要授权码）")
	}
	return nil
}

// EmailNotifier 是邮件通道。配置来自环境变量，进程启动后不再变化；
// 未配置 SMTP 时，若显式开启 outbox（NOTIFY_DEV_OUTBOX=1），邮件会写入 outbox_messages 表而不投递。
type EmailNotifier struct {
	cfg     EmailConfig
	outbox  notify.OutboxWriter
	ready   bool
	devMode bool
	reason  string
	log     *slog.Logger
}

// NewEmailNotifier 创建邮件通道。outbox 非空表示允许 dev 兜底模式。
func NewEmailNotifier(cfg EmailConfig, outbox notify.OutboxWriter, logger *slog.Logger) *EmailNotifier {
	if logger == nil {
		logger = slog.Default()
	}
	cfg = cfg.WithDefaults()
	n := &EmailNotifier{cfg: cfg, outbox: outbox, log: logger}
	if strings.TrimSpace(cfg.Host) == "" {
		n.reason = "未配置发件邮箱（NOTIFY_SMTP_HOST 为空）"
		if outbox != nil {
			n.devMode, n.ready = true, true
			n.reason = "未配置发件邮箱，邮件只写入 outbox_messages 表（NOTIFY_DEV_OUTBOX=1）"
		}
		return n
	}
	if err := cfg.Validate(); err != nil {
		n.reason = err.Error()
		return n
	}
	n.ready = true
	return n
}

// Name 实现 notify.Notifier。
func (n *EmailNotifier) Name() notify.Channel { return notify.ChannelEmail }

// Describe 实现 notify.Notifier。
func (n *EmailNotifier) Describe() notify.Status {
	status := notify.Status{Channel: notify.ChannelEmail, Provider: "smtp"}
	switch {
	case n.devMode:
		status.Ready = true
		status.Mode = "dev"
		status.Provider = "smtp-dev-outbox"
		status.Details = map[string]string{"outbox": "outbox_messages", "hint": n.reason}
	case n.ready:
		status.Ready = true
		status.Mode = "live"
		status.Details = map[string]string{
			"host":        n.cfg.Host,
			"port":        strconv.Itoa(n.cfg.Port),
			"from":        n.cfg.From,
			"tls":         n.cfg.TLS,
			"username":    n.cfg.Username,
			"passwordSet": strconv.FormatBool(n.cfg.Password != ""),
		}
	default:
		status.Mode = "unconfigured"
		status.Details = map[string]string{"hint": n.reason}
	}
	return status
}

// Send 实现 notify.Notifier：把渲染好的正文片段套上邮件外壳，
// 同时产出 text/plain 兜底（buildMIME 会组成 multipart/alternative）。
func (n *EmailNotifier) Send(ctx context.Context, d notify.Delivery) (notify.Receipt, error) {
	if !n.ready {
		return notify.Receipt{}, notify.NotReadyf("邮件通道不可用：%s", n.reason)
	}
	msg := d.Message
	to, err := ValidateEmailRecipients(msg.To)
	if err != nil {
		return notify.Receipt{}, err
	}
	subject := strings.TrimSpace(msg.Subject)
	if subject == "" {
		subject = "(无主题)"
	}
	textBody := d.Text
	htmlBody := ""
	if strings.TrimSpace(d.HTML) != "" {
		htmlBody = emailShell(d.HTML, n.cfg.Brand, n.cfg.Footer)
	}

	if n.devMode {
		raw, messageID := buildMIME(devConfig(n.cfg.Brand), to, subject, textBody, htmlBody)
		if err := n.outbox.SaveOutbox(ctx, notify.OutboxMessage{
			Time:       time.Now(),
			Channel:    notify.ChannelEmail,
			MessageID:  messageID,
			Recipients: to,
			Subject:    subject,
			Body:       textBody,
			Raw:        string(raw),
		}); err != nil {
			return notify.Receipt{}, notify.NotReadyf("写入 email outbox 失败: %v", err)
		}
		n.log.Warn("dev 模式：邮件未投递，已写入 outbox_messages 表", "messageId", messageID, "subject", subject)
		return notify.Receipt{
			Channel:   notify.ChannelEmail,
			Provider:  "smtp-dev-outbox",
			MessageID: messageID,
			Accepted:  to,
			Simulated: true,
			Detail:    "dev 模式：邮件写入 outbox_messages 表",
		}, nil
	}

	raw, messageID := buildMIME(n.cfg, to, subject, textBody, htmlBody)
	if err := smtpSend(ctx, n.cfg, to, raw); err != nil {
		return notify.Receipt{}, notify.Deliveryf("SMTP 投递失败（%s:%d）: %v", n.cfg.Host, n.cfg.Port, err)
	}
	return notify.Receipt{
		Channel:   notify.ChannelEmail,
		Provider:  "smtp",
		MessageID: messageID,
		Accepted:  to,
		Detail:    fmt.Sprintf("已投递至 %s:%d", n.cfg.Host, n.cfg.Port),
	}, nil
}

// VerifyMessage 构造一封"发件邮箱自检"邮件，走统一发送链路以便留下记录。
func (n *EmailNotifier) VerifyMessage(to []string) notify.Message {
	where := "未配置发件邮箱（dev outbox 模式）"
	if n.ready && !n.devMode {
		where = fmt.Sprintf("%s:%d（tls=%s，from=%s）", n.cfg.Host, n.cfg.Port, n.cfg.TLS, n.cfg.From)
	}
	body := "这是一封来自 " + n.cfg.Brand + " 的测试邮件。" +
		"\n\n当前发件邮箱：" + where +
		"\n\n收到本邮件说明邮件通道可用。" +
		"\n\n发送时间：" + time.Now().Format(time.RFC3339)
	return notify.Message{
		Channel: notify.ChannelEmail,
		To:      to,
		Type:    "self-test",
		Subject: "【" + n.cfg.Brand + "】发件邮箱自检",
		Body:    body,
		Meta:    map[string]string{"kind": "verify"},
	}
}

func devConfig(brand string) EmailConfig {
	return EmailConfig{Host: "localhost", From: devFrom, FromName: brand + "(dev)", TLS: TLSModeNone}
}

// ValidateEmailRecipients 校验收件人格式；只接受裸地址（a@b.c），不接受带显示名的写法。
func ValidateEmailRecipients(to []string) ([]string, error) {
	if len(to) == 0 {
		return nil, notify.Invalidf("邮件收件人不能为空")
	}
	for _, rcpt := range to {
		if strings.ContainsAny(rcpt, "\r\n ,;<>") {
			return nil, notify.Invalidf("收件人 %q 含非法字符", rcpt)
		}
		at := strings.Index(rcpt, "@")
		if at <= 0 || at == len(rcpt)-1 || !strings.Contains(rcpt[at+1:], ".") {
			return nil, notify.Invalidf("收件人 %q 不是合法邮箱地址", rcpt)
		}
	}
	return to, nil
}

// buildMIME 组装一封 UTF-8 邮件（RFC 5322）：
// 只有纯文本时是单段 text/plain；带 HTML 时是 multipart/alternative，正文均用 base64，避免中文乱码。
func buildMIME(cfg EmailConfig, to []string, subject, body, htmlBody string) (raw []byte, messageID string) {
	body = strings.TrimSpace(body)
	htmlBody = strings.TrimSpace(htmlBody)
	if body == "" && htmlBody != "" {
		body = notify.StripTags(htmlBody)
	}

	fromHeader := cfg.From
	if cfg.FromName != "" {
		fromHeader = mime.QEncoding.Encode("utf-8", cfg.FromName) + " <" + cfg.From + ">"
	}
	messageID = newMessageID(cfg.From)

	var b bytes.Buffer
	header := func(key, value string) {
		b.WriteString(key)
		b.WriteString(": ")
		b.WriteString(value)
		b.WriteString("\r\n")
	}
	header("From", fromHeader)
	header("To", strings.Join(to, ", "))
	header("Subject", mime.QEncoding.Encode("utf-8", subject))
	header("Date", time.Now().Format(time.RFC1123Z))
	header("Message-ID", messageID)
	header("MIME-Version", "1.0")

	if htmlBody == "" {
		header("Content-Type", `text/plain; charset="utf-8"`)
		header("Content-Transfer-Encoding", "base64")
		b.WriteString("\r\n")
		writeBase64(&b, body)
		return b.Bytes(), messageID
	}

	boundary := "----notify-" + shortID()
	header("Content-Type", `multipart/alternative; boundary="`+boundary+`"`)
	b.WriteString("\r\n")
	part := func(contentType, content string) {
		b.WriteString("--" + boundary + "\r\n")
		b.WriteString("Content-Type: " + contentType + `; charset="utf-8"` + "\r\n")
		b.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
		writeBase64(&b, content)
	}
	part("text/plain", body)
	part("text/html", htmlBody)
	b.WriteString("--" + boundary + "--\r\n")
	return b.Bytes(), messageID
}

// writeBase64 按 76 字符折行写出 base64（RFC 2045 要求）。
func writeBase64(b *bytes.Buffer, content string) {
	encoded := base64.StdEncoding.EncodeToString([]byte(content))
	for len(encoded) > 76 {
		b.WriteString(encoded[:76])
		b.WriteString("\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded)
	b.WriteString("\r\n")
}

func newMessageID(from string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	domain := "notify.local"
	if at := strings.LastIndex(from, "@"); at >= 0 && at < len(from)-1 {
		domain = from[at+1:]
	}
	return "<" + hex.EncodeToString(b[:]) + "." + strconv.FormatInt(time.Now().UnixNano(), 36) + "@" + domain + ">"
}

func shortID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// smtpSend 按配置完成一次 SMTP 会话。全程受 deadline 与 ctx 约束。
func smtpSend(ctx context.Context, cfg EmailConfig, to []string, raw []byte) error {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	timeout := time.Duration(cfg.TimeoutSecs) * time.Second
	dialer := &net.Dialer{Timeout: timeout}

	var (
		conn net.Conn
		err  error
	)
	if cfg.TLS == TLSModeImplicit {
		tlsDialer := tls.Dialer{NetDialer: dialer, Config: &tls.Config{ServerName: cfg.Host}}
		conn, err = tlsDialer.DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("连接失败: %w", err)
	}
	defer conn.Close()

	deadline := time.Now().Add(timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}

	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		return fmt.Errorf("SMTP 握手失败: %w", err)
	}
	defer client.Close()

	if cfg.TLS == TLSModeAuto || cfg.TLS == TLSModeSTARTTLS {
		supported, _ := client.Extension("STARTTLS")
		switch {
		case supported:
			if err := client.StartTLS(&tls.Config{ServerName: cfg.Host}); err != nil {
				return fmt.Errorf("STARTTLS 失败: %w", err)
			}
		case cfg.TLS == TLSModeSTARTTLS:
			return errors.New("服务器未提供 STARTTLS，但配置要求强制启用")
		default:
			if cfg.Username != "" {
				return errors.New("服务器未提供 STARTTLS，拒绝在明文连接上发送账号密码；可把 NOTIFY_SMTP_TLS 改为 none 仅用于本地测试")
			}
		}
	}

	if cfg.Username != "" {
		_, mechs := client.Extension("AUTH")
		var auth smtp.Auth
		switch {
		case strings.Contains(mechs, "PLAIN"):
			auth = smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
		case strings.Contains(mechs, "LOGIN"):
			auth = loginAuth{username: cfg.Username, password: cfg.Password}
		default:
			return fmt.Errorf("服务器未提供可用的 AUTH 机制（返回 %q）", mechs)
		}
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("认证失败: %w", err)
		}
	}

	if err := client.Mail(addrOnly(cfg.From)); err != nil {
		return fmt.Errorf("MAIL FROM 被拒绝: %w", err)
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return fmt.Errorf("RCPT TO %s 被拒绝: %w", rcpt, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("DATA 被拒绝: %w", err)
	}
	if _, err := w.Write(raw); err != nil {
		w.Close()
		return fmt.Errorf("写入邮件内容失败: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("提交邮件失败: %w", err)
	}
	return client.Quit()
}

func addrOnly(s string) string {
	if i := strings.LastIndex(s, "<"); i >= 0 {
		if j := strings.Index(s[i:], ">"); j > 0 {
			return s[i+1 : i+j]
		}
	}
	return strings.TrimSpace(s)
}

// loginAuth 实现 SMTP AUTH LOGIN（标准库只带 PLAIN / CRAM-MD5）。
type loginAuth struct {
	username string
	password string
}

func (a loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	return "LOGIN", nil, nil
}

func (a loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch strings.ToLower(strings.TrimSpace(string(fromServer))) {
	case "username:", "username":
		return []byte(a.username), nil
	case "password:", "password":
		return []byte(a.password), nil
	}
	return nil, fmt.Errorf("AUTH LOGIN 收到未知质询 %q", fromServer)
}
