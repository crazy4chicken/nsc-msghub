package channel

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"

	"notify-service/internal/notify"
)

// maxSMSTextRunes 单条短信文本上限（含签名），超出直接报 400 而不是被上游静默截断。
const maxSMSTextRunes = 500

// SMSProvider 是短信上游的统一抽象。
// 接阿里云/腾讯云时：新建一个实现该接口的类型，然后在 main.go 里替换
// NewConsoleSMSProvider 即可——通道层、HTTP API、模板都不需要改动。
type SMSProvider interface {
	// Name 返回上游名称，例如 "console"、"aliyun"、"tencent"。
	Name() string
	// Mode 返回 live（真实投递）或 dev（本地模拟）。
	Mode() string
	// Ready 表示上游凭证是否就绪。
	Ready() bool
	// Send 发送一条短信，返回上游流水号。
	Send(ctx context.Context, phone, text string) (string, error)
}

// ConsoleSMSProvider 把短信写到日志与 outbox_messages 表，用于本地演示与联调；
// 它不产生任何真实费用，也不依赖短信平台审核。
type ConsoleSMSProvider struct {
	outbox notify.OutboxWriter
	log    *slog.Logger
}

// NewConsoleSMSProvider 创建本地模拟短信上游；outbox 为 nil 时只打日志、不落库。
func NewConsoleSMSProvider(outbox notify.OutboxWriter, logger *slog.Logger) *ConsoleSMSProvider {
	if logger == nil {
		logger = slog.Default()
	}
	return &ConsoleSMSProvider{outbox: outbox, log: logger}
}

func (p *ConsoleSMSProvider) Name() string { return "console" }
func (p *ConsoleSMSProvider) Mode() string { return "dev" }
func (p *ConsoleSMSProvider) Ready() bool  { return true }

func (p *ConsoleSMSProvider) Send(ctx context.Context, phone, text string) (string, error) {
	id := "sim_" + shortID()
	p.log.Info("dev 模式：短信未真实发送", "to", notify.Mask(phone), "text", text, "id", id)
	if p.outbox == nil {
		return id, nil
	}
	if err := p.outbox.SaveOutbox(ctx, notify.OutboxMessage{
		Time:       time.Now(),
		Channel:    notify.ChannelSMS,
		MessageID:  id,
		Recipients: []string{phone},
		Body:       text,
	}); err != nil {
		return "", notify.NotReadyf("写入短信 outbox 失败: %v", err)
	}
	return id, nil
}

// SMSNotifier 把 SMSProvider 适配成统一的 notify.Notifier。
// provider 为 nil 表示还没接入真实上游：这时通道明确报"不可用"，
// 而不是静默模拟成功——整套系统的通知都可能路由到短信，假装成功等于丢消息。
type SMSNotifier struct {
	provider SMSProvider
}

// NewSMSNotifier 创建短信通道；provider 传 nil 表示尚未接入上游。
func NewSMSNotifier(provider SMSProvider) *SMSNotifier {
	return &SMSNotifier{provider: provider}
}

const smsNotWired = "短信上游尚未接入：实现 SMSProvider 并在 main.go 注册；本地联调可设 NOTIFY_SMS_SIMULATE=1 用模拟上游"

// Name 实现 notify.Notifier。
func (n *SMSNotifier) Name() notify.Channel { return notify.ChannelSMS }

// Describe 实现 notify.Notifier。
func (n *SMSNotifier) Describe() notify.Status {
	if n.provider == nil {
		return notify.Status{
			Channel:  notify.ChannelSMS,
			Provider: "none",
			Ready:    false,
			Mode:     "unconfigured",
			Details:  map[string]string{"hint": smsNotWired},
		}
	}
	status := notify.Status{
		Channel:  notify.ChannelSMS,
		Provider: n.provider.Name(),
		Ready:    n.provider.Ready(),
		Mode:     n.provider.Mode(),
	}
	if status.Mode == "dev" {
		status.Details = map[string]string{
			"hint": "当前是本地模拟上游（NOTIFY_SMS_SIMULATE=1），短信只打日志并写入 outbox_messages 表，不会真实发送",
		}
	}
	return status
}

// Send 实现 notify.Notifier：只取 Body（没有则从 HTML 提取），Subject 不参与短信内容。
func (n *SMSNotifier) Send(ctx context.Context, d notify.Delivery) (notify.Receipt, error) {
	if n.provider == nil {
		return notify.Receipt{}, notify.NotReadyf("%s", smsNotWired)
	}
	msg := d.Message
	phones, err := ValidatePhones(msg.To)
	if err != nil {
		return notify.Receipt{}, err
	}
	// 正文已由 Service 按 bodyFormat 渲染好（Markdown 已去语法），这里直接用纯文本。
	text := strings.TrimSpace(d.Text)
	if text == "" {
		text = notify.StripTags(d.HTML)
	}
	if text == "" {
		return notify.Receipt{}, notify.Invalidf("短信内容不能为空：请填 body")
	}
	if runes := []rune(text); len(runes) > maxSMSTextRunes {
		return notify.Receipt{}, notify.Invalidf("短信内容过长：%d 字，上限 %d 字", len(runes), maxSMSTextRunes)
	}

	accepted := make([]string, 0, len(phones))
	var lastID string
	for _, phone := range phones {
		id, err := n.provider.Send(ctx, phone, text)
		if err != nil {
			return notify.Receipt{}, notify.Deliveryf("短信上游 %s 发送到 %s 失败: %v", n.provider.Name(), notify.Mask(phone), err)
		}
		accepted = append(accepted, phone)
		lastID = id
	}
	return notify.Receipt{
		Channel:   notify.ChannelSMS,
		Provider:  n.provider.Name(),
		MessageID: lastID,
		Accepted:  accepted,
		Simulated: n.provider.Mode() == "dev",
		Detail:    fmt.Sprintf("已提交 %d 个号码到上游 %s", len(accepted), n.provider.Name()),
	}, nil
}

// ValidatePhones 校验手机号：允许 +86 前缀与空格分隔，长度 5~20 位数字。
func ValidatePhones(to []string) ([]string, error) {
	if len(to) == 0 {
		return nil, notify.Invalidf("短信接收号码不能为空")
	}
	out := make([]string, 0, len(to))
	for _, raw := range to {
		phone := strings.TrimSpace(raw)
		if strings.ContainsAny(phone, "\r\n,;") {
			return nil, notify.Invalidf("号码 %q 含非法字符", raw)
		}
		digits := phone
		if strings.HasPrefix(digits, "+") {
			digits = digits[1:]
		}
		if len(digits) < 5 || len(digits) > 20 {
			return nil, notify.Invalidf("号码 %q 长度非法", raw)
		}
		for _, r := range digits {
			if !unicode.IsDigit(r) {
				return nil, notify.Invalidf("号码 %q 含非数字字符", raw)
			}
		}
		out = append(out, phone)
	}
	return out, nil
}
