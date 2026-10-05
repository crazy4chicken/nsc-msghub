// Package directory 提供两种用户目录实现：PostgreSQL users 表、用户服务 HTTP 接口。
// 通知服务只依赖 notify.Resolver 接口，换实现不需要改动发送逻辑。
package directory

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"notify-service/internal/notify"
)

// Querier 是 PGResolver 需要的最小数据库能力，*pgxpool.Pool 天然满足。
// 单独抽出来是为了在测试里可以注入简单实现。
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// PGResolver 从 PostgreSQL 的 users 表读取用户：id -> name / channels。
// 表结构由 internal/store 在启动时迁移，这里是只读方。
type PGResolver struct {
	q Querier
}

// NewPGResolver 创建 PostgreSQL 用户表解析器，q 为 nil 时返回 nil。
func NewPGResolver(q Querier) *PGResolver {
	if q == nil {
		return nil
	}
	return &PGResolver{q: q}
}

// Describe 实现 notify.Resolver。
func (r *PGResolver) Describe() string {
	return "PostgreSQL 用户表"
}

// Resolve 实现 notify.Resolver。
func (r *PGResolver) Resolve(ctx context.Context, userID string) (notify.User, error) {
	var (
		id       string
		name     string
		channels []byte
	)
	err := r.q.QueryRow(ctx, `SELECT id, name, channels FROM users WHERE id = $1`, userID).
		Scan(&id, &name, &channels)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return notify.User{}, notify.NotFoundf("用户 %q 不在 users 表中", userID)
		}
		return notify.User{}, notify.Upstreamf("查询用户表失败: %v", err)
	}

	addr := map[string]string{}
	if len(channels) > 0 {
		if err := json.Unmarshal(channels, &addr); err != nil {
			return notify.User{}, notify.Upstreamf("解析用户 %q 的 channels 失败: %v", userID, err)
		}
		if addr == nil {
			addr = map[string]string{}
		}
	}
	return notify.User{ID: id, Name: name, Channels: addr}, nil
}
