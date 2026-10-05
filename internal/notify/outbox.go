package notify

import (
	"context"
	"time"
)

// OutboxMessage 是 dev 模拟投递留下的一条 outbox 记录：真实通道未配置
// （或显式开启模拟）时，邮件/短信不投递，改写进持久化存储供本地联调查看。
type OutboxMessage struct {
	Time       time.Time
	Channel    Channel
	MessageID  string
	Recipients []string
	Subject    string
	Body       string
	Raw        string
}

// OutboxWriter 持久化 dev 模拟投递的 outbox 记录。
// 写入失败会影响本次模拟投递的结果，由调用方决定错误处理。
type OutboxWriter interface {
	SaveOutbox(ctx context.Context, m OutboxMessage) error
}
