package notify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// 记录状态。
const (
	StatusSent      = "sent"      // 真实投递成功
	StatusSimulated = "simulated" // dev 模式：只写 outbox 表/打日志，未真实投递
	StatusFailed    = "failed"    // 投递失败或请求非法
)

// Record 是一次发送尝试的审计记录。校验失败的请求同样会被记录，
// 这样联调时能在记录列表里直接看到"发了什么、为什么失败"。
type Record struct {
	ID          string            `json:"id"`
	Time        time.Time         `json:"time"`
	Channel     Channel           `json:"channel"`
	Type        string            `json:"type,omitempty"`
	BodyFormat  string            `json:"bodyFormat,omitempty"`
	UserID      string            `json:"userId,omitempty"`
	UserName    string            `json:"userName,omitempty"`
	Provider    string            `json:"provider,omitempty"`
	To          []string          `json:"to"`
	Subject     string            `json:"subject,omitempty"`
	BodyPreview string            `json:"bodyPreview,omitempty"`
	Status      string            `json:"status"`
	Simulated   bool              `json:"simulated,omitempty"`
	MessageID   string            `json:"messageId,omitempty"`
	Detail      string            `json:"detail,omitempty"`
	DurationMS  int64             `json:"durationMs"`
	Meta        map[string]string `json:"meta,omitempty"`
	Error       string            `json:"error,omitempty"`
}

// Recorder 持久化发送记录。记录失败不影响发送结果，只打日志。
type Recorder interface {
	Save(ctx context.Context, rec Record) error
}

// Service 是统一通知入口：解析模板 → 校验 → 交给对应通道 → 落记录。
type Service struct {
	mu        sync.RWMutex
	notifiers map[Channel]Notifier
	resolver  Resolver
	routes    *RouteTable
	recorder  Recorder
	log       *slog.Logger
}

// NewService 创建通知服务。recorder 可为 nil；resolver/routes 为 nil 时按 user 发送不可用。
func NewService(recorder Recorder, resolver Resolver, routes *RouteTable, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		notifiers: make(map[Channel]Notifier),
		resolver:  resolver,
		routes:    routes,
		recorder:  recorder,
		log:       logger,
	}
}

// Register 注册一个通道。通道名重复会直接报错（启动期问题，应当失败退出）。
func (s *Service) Register(n Notifier) error {
	name := n.Name()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.notifiers[name]; dup {
		return Invalidf("通道 %q 已注册", name)
	}
	s.notifiers[name] = n
	return nil
}

// Channels 返回全部通道状态，按通道名排序。
func (s *Service) Channels() []Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Status, 0, len(s.notifiers))
	for _, n := range s.notifiers {
		out = append(out, n.Describe())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Channel < out[j].Channel })
	return out
}

// Directory 返回用户目录的描述，用于状态展示与排错。
func (s *Service) Directory() string {
	if s.resolver == nil {
		return ""
	}
	return s.resolver.Describe()
}

// Routes 返回类型 → 渠道的路由规则。
func (s *Service) Routes() map[string][]Channel {
	return s.routes.Rules()
}

func (s *Service) lookup(ch Channel) (Notifier, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	notifier, ok := s.notifiers[ch]
	return notifier, ok
}

// Send 发送一条通知，并返回落库后的记录。发送失败时返回的记录里带着错误信息。
func (s *Service) Send(ctx context.Context, msg Message) (Record, error) {
	m := msg
	m.Normalize()

	rec := Record{
		ID:     NewID(),
		Time:   time.Now(),
		Type:   m.Type,
		UserID: m.User,
		Meta:   m.Meta,
		Status: StatusFailed,
	}

	fail := func(err error) (Record, error) {
		rec.Channel = m.Channel
		rec.To = append([]string(nil), m.To...)
		if rec.Subject == "" {
			rec.Subject = m.Subject
		}
		rec.Error = err.Error()
		rec.DurationMS = time.Since(rec.Time).Milliseconds()
		rec.BodyPreview = preview(m.Body, 120)
		s.persist(ctx, rec)
		s.log.Warn("通知发送失败",
			"id", rec.ID, "channel", rec.Channel, "type", rec.Type,
			"user", rec.UserID, "to", maskAll(rec.To), "error", err.Error())
		return rec, err
	}

	rec.Subject = m.Subject
	// 调用方没写 bodyFormat 时按纯文本处理（老调用方只传 body 的行为不变）。
	if m.BodyFormat == "" {
		m.BodyFormat = BodyFormatText
	}
	rec.BodyFormat = string(m.BodyFormat)

	// 2. 便宜校验先做，避免明显非法的请求打到用户服务。
	if err := m.PreValidate(); err != nil {
		return fail(err)
	}

	// 3. 收件人：给了 user 就由本服务解析地址并决定渠道；否则用调用方给的地址。
	if m.User != "" {
		if err := s.routeToUser(ctx, &m, &rec); err != nil {
			return fail(err)
		}
	}
	if m.Channel == "" {
		m.Channel = ChannelEmail
	}
	if err := m.Validate(); err != nil {
		return fail(err)
	}
	rec.Channel = m.Channel
	rec.To = append([]string(nil), m.To...)

	notifier, ok := s.lookup(m.Channel)
	if !ok {
		return fail(NotFoundf("通道 %q 不存在，可用通道见 GET /api/v1/channels", m.Channel))
	}

	// 4. 渲染：Markdown 单源 → email 要 HTML + 纯文本兜底，短信只要纯文本。
	delivery := Delivery{Message: m}
	if m.BodyFormat == BodyFormatMarkdown {
		delivery.HTML, delivery.Text = RenderMarkdown(m.Body)
	} else {
		delivery.Text = m.Body
		delivery.HTML = TextToHTML(m.Body)
	}

	started := time.Now()
	receipt, err := notifier.Send(ctx, delivery)
	rec.DurationMS = time.Since(started).Milliseconds()
	rec.Provider = receipt.Provider
	rec.To = append([]string(nil), m.To...)
	rec.Subject = m.Subject
	rec.BodyPreview = preview(delivery.Text, 120)
	if err != nil {
		rec.Error = err.Error()
		s.persist(ctx, rec)
		s.log.Warn("通知发送失败",
			"id", rec.ID, "channel", rec.Channel, "type", rec.Type,
			"user", rec.UserID, "to", maskAll(rec.To), "error", err.Error())
		return rec, err
	}

	rec.MessageID = receipt.MessageID
	rec.Simulated = receipt.Simulated
	rec.Detail = receipt.Detail
	if receipt.Provider != "" {
		rec.Provider = receipt.Provider
	}
	if receipt.Simulated {
		rec.Status = StatusSimulated
	} else {
		rec.Status = StatusSent
	}
	s.persist(ctx, rec)
	s.log.Info("通知已发送",
		"id", rec.ID, "channel", rec.Channel, "type", rec.Type,
		"user", rec.UserID, "provider", rec.Provider,
		"to", maskAll(rec.To), "status", rec.Status, "durationMs", rec.DurationMS)
	return rec, nil
}

// routeToUser 解析用户地址并决定渠道：调用方指定了 channel 就只认它，
// 否则按 type 的路由规则挑第一个"有地址且通道可用"的渠道。
func (s *Service) routeToUser(ctx context.Context, m *Message, rec *Record) error {
	if s.resolver == nil {
		return NotReadyf("未配置用户目录，无法按 user 发通知：请设置 NOTIFY_USER_SERVICE_URL 或准备 PostgreSQL users 表")
	}
	user, err := s.resolver.Resolve(ctx, m.User)
	if err != nil {
		return err
	}
	rec.UserName = user.Name
	if rec.UserID == "" {
		rec.UserID = user.ID
	}

	candidates := s.routes.Candidates(m.Type)
	if m.Channel != "" {
		candidates = []Channel{m.Channel}
	}

	var missing, notReady []Channel
	for _, ch := range candidates {
		addr, ok := user.Address(ch)
		if !ok {
			missing = append(missing, ch)
			continue
		}
		notifier, registered := s.lookup(ch)
		if !registered || !notifier.Describe().Ready {
			notReady = append(notReady, ch)
			continue
		}
		m.Channel = ch
		m.To = []string{addr}
		m.resolved = true
		s.log.Info("用户路由完成",
			"id", rec.ID, "user", user.ID, "name", user.Name,
			"detail", describeRoute(m.Type, candidates, ch))
		return nil
	}
	return routeError(m.User, m.Type, candidates, missing, notReady)
}

func (s *Service) persist(ctx context.Context, rec Record) {
	if s.recorder == nil {
		return
	}
	if err := s.recorder.Save(ctx, rec); err != nil {
		s.log.Error("写入发送记录失败", "id", rec.ID, "error", err.Error())
	}
}

// NewID 生成记录 ID。
func NewID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "ntf_" + hex.EncodeToString(b[:])
}

func preview(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func maskAll(items []string) []string {
	out := make([]string, len(items))
	for i, v := range items {
		out[i] = Mask(v)
	}
	return out
}

// Mask 对邮箱/手机号做脱敏，用于日志，避免明文刷进控制台。
func Mask(s string) string {
	if at := strings.Index(s, "@"); at > 1 {
		return s[:1] + "***" + s[at-1:]
	}
	if len(s) >= 11 {
		return s[:3] + "****" + s[len(s)-4:]
	}
	if len(s) > 4 {
		return s[:1] + "***" + s[len(s)-1:]
	}
	return s
}
