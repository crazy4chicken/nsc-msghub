// Package store 用 PostgreSQL 持久化发送记录、用户表与 dev 模拟 outbox：
// PostgreSQL 是唯一持久化存储，进程不再读写本地文件。
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"notify-service/internal/notify"
)

// Store 是 PostgreSQL 发送记录存储。连接池由本类型持有，进程内只应创建一个。
type Store struct {
	pool        *pgxpool.Pool
	recordLimit int
	log         *slog.Logger
}

// Filter 是记录查询条件。
type Filter struct {
	Channel string
	Type    string
	UserID  string
	Status  string
	Limit   int
}

// schema 是启动时执行的建表语句，全部幂等：已存在的对象不会被改动。
// "time" 与 "type" 是保留字，一律带引号。
const schema = `
CREATE TABLE IF NOT EXISTS notifications (
    seq BIGSERIAL PRIMARY KEY,
    id TEXT NOT NULL UNIQUE,
    "time" TIMESTAMPTZ NOT NULL,
    channel TEXT NOT NULL,
    "type" TEXT NOT NULL DEFAULT '',
    body_format TEXT NOT NULL DEFAULT '',
    user_id TEXT NOT NULL DEFAULT '',
    user_name TEXT NOT NULL DEFAULT '',
    provider TEXT NOT NULL DEFAULT '',
    recipients TEXT[] NOT NULL DEFAULT '{}',
    subject TEXT NOT NULL DEFAULT '',
    body_preview TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    simulated BOOLEAN NOT NULL DEFAULT FALSE,
    message_id TEXT NOT NULL DEFAULT '',
    detail TEXT NOT NULL DEFAULT '',
    duration_ms BIGINT NOT NULL DEFAULT 0,
    meta JSONB,
    error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS notifications_time_idx ON notifications ("time" DESC, seq DESC);
CREATE INDEX IF NOT EXISTS notifications_channel_idx ON notifications (channel);
CREATE INDEX IF NOT EXISTS notifications_user_id_idx ON notifications (user_id);
CREATE INDEX IF NOT EXISTS notifications_status_idx ON notifications (status);

CREATE TABLE IF NOT EXISTS users (
    id TEXT NOT NULL PRIMARY KEY,
    name TEXT NOT NULL DEFAULT '',
    channels JSONB NOT NULL DEFAULT '{}'::jsonb,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS outbox_messages (
    seq BIGSERIAL PRIMARY KEY,
    "time" TIMESTAMPTZ NOT NULL,
    channel TEXT NOT NULL,
    message_id TEXT NOT NULL DEFAULT '',
    recipients TEXT[] NOT NULL DEFAULT '{}',
    subject TEXT NOT NULL DEFAULT '',
    body TEXT NOT NULL DEFAULT '',
    raw TEXT NOT NULL DEFAULT ''
);
`

// recordColumns 是 notifications 表的读取列，顺序与 scanRecord 一致。
const recordColumns = `id, "time", channel, "type", body_format, user_id, user_name, provider,
	recipients, subject, body_preview, status, simulated, message_id, detail, duration_ms, meta, error`

// insertRecord 写入一条发送记录。
const insertRecord = `
INSERT INTO notifications
	(id, "time", channel, "type", body_format, user_id, user_name, provider,
	 recipients, subject, body_preview, status, simulated, message_id, detail, duration_ms, meta, error)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)`

// pruneRecords 只保留最新 $1 条记录：$1 是保留条数，删除 seq 小于第 $1 新记录的全部行。
const pruneRecords = `
DELETE FROM notifications
WHERE seq < (SELECT seq FROM notifications ORDER BY seq DESC OFFSET $1 - 1 LIMIT 1)`

// Migrate 创建（如不存在）通知记录、用户表与 outbox 表结构，启动时执行一次。
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, schema); err != nil {
		return err
	}
	return nil
}

// Open 连接 PostgreSQL、校验连通性并迁移表结构。
// recordLimit 为发送记录保留条数：0 表示全部保留，>0 表示只保留最新 N 条。
func Open(ctx context.Context, dsn string, recordLimit int, logger *slog.Logger) (*Store, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if recordLimit < 0 {
		recordLimit = 0
	}
	if strings.TrimSpace(dsn) == "" {
		return nil, fmt.Errorf("连接 PostgreSQL 失败: %w", errors.New("必须配置 NOTIFY_DATABASE_URL（PostgreSQL 连接串）"))
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("初始化表结构失败: %w", err)
	}
	return &Store{pool: pool, recordLimit: recordLimit, log: logger}, nil
}

// Pool 返回底层连接池，供进程内其它只读组件（如用户目录）复用同一份连接。
func (s *Store) Pool() *pgxpool.Pool {
	return s.pool
}

// Close 关闭连接池；等待已借出的连接归还后释放。
func (s *Store) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}

// Save 实现 notify.Recorder：写入一条发送记录，必要时按 recordLimit 清理旧记录。
func (s *Store) Save(ctx context.Context, rec notify.Record) error {
	recipients := rec.To
	if recipients == nil {
		recipients = []string{}
	}
	var meta any
	if rec.Meta != nil {
		meta = rec.Meta
	}
	if _, err := s.pool.Exec(ctx, insertRecord,
		rec.ID, rec.Time, string(rec.Channel), rec.Type, rec.BodyFormat,
		rec.UserID, rec.UserName, rec.Provider, recipients, rec.Subject, rec.BodyPreview,
		rec.Status, rec.Simulated, rec.MessageID, rec.Detail, rec.DurationMS, meta, rec.Error,
	); err != nil {
		return fmt.Errorf("写入发送记录失败: %w", err)
	}
	if s.recordLimit > 0 {
		if _, err := s.pool.Exec(ctx, pruneRecords, s.recordLimit); err != nil {
			s.log.Warn("清理历史发送记录失败", "recordLimit", s.recordLimit, "error", err.Error())
		}
	}
	return nil
}

// Get 按 ID 取一条记录；不存在时返回 (zero, false, nil)。
func (s *Store) Get(ctx context.Context, id string) (notify.Record, bool, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+recordColumns+` FROM notifications WHERE id = $1`, id)
	rec, err := scanRecord(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return notify.Record{}, false, nil
		}
		return notify.Record{}, false, fmt.Errorf("查询发送记录失败: %w", err)
	}
	return rec, true, nil
}

// List 返回按时间倒序排列的记录：时间相同再按写入顺序（seq）倒序。
// limit <= 0 时取 100，最大 5000；空结果返回空切片（JSON 里是 [] 而不是 null）。
func (s *Store) List(ctx context.Context, f Filter) ([]notify.Record, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 5000 {
		limit = 5000
	}

	conds := make([]string, 0, 4)
	args := make([]any, 0, 5)
	add := func(column, value string) {
		args = append(args, value)
		conds = append(conds, fmt.Sprintf("%s = $%d", column, len(args)))
	}
	if f.Channel != "" {
		add("channel", f.Channel)
	}
	if f.Type != "" {
		add(`"type"`, f.Type)
	}
	if f.UserID != "" {
		add("user_id", f.UserID)
	}
	if f.Status != "" {
		add("status", f.Status)
	}

	query := `SELECT ` + recordColumns + ` FROM notifications`
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, limit)
	query += fmt.Sprintf(` ORDER BY "time" DESC, seq DESC LIMIT $%d`, len(args))

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询发送记录失败: %w", err)
	}
	defer rows.Close()

	out := make([]notify.Record, 0, limit)
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("查询发送记录失败: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("查询发送记录失败: %w", err)
	}
	return out, nil
}

// Count 返回表中的记录总数（不受筛选条件影响）。
func (s *Store) Count(ctx context.Context) (int, error) {
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM notifications`).Scan(&total); err != nil {
		return 0, fmt.Errorf("统计发送记录失败: %w", err)
	}
	return total, nil
}

// SaveOutbox 实现 notify.OutboxWriter：写入一条 dev 模拟投递记录。
func (s *Store) SaveOutbox(ctx context.Context, m notify.OutboxMessage) error {
	recipients := m.Recipients
	if recipients == nil {
		recipients = []string{}
	}
	if _, err := s.pool.Exec(ctx, `
INSERT INTO outbox_messages ("time", channel, message_id, recipients, subject, body, raw)
VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		m.Time, string(m.Channel), m.MessageID, recipients, m.Subject, m.Body, m.Raw,
	); err != nil {
		return fmt.Errorf("写入 outbox 失败: %w", err)
	}
	return nil
}

// scanRecord 按 recordColumns 的顺序读出一行记录。
func scanRecord(row pgx.Row) (notify.Record, error) {
	var (
		rec        notify.Record
		channel    string
		recipients []string
		meta       []byte
	)
	if err := row.Scan(
		&rec.ID, &rec.Time, &channel, &rec.Type, &rec.BodyFormat,
		&rec.UserID, &rec.UserName, &rec.Provider, &recipients, &rec.Subject, &rec.BodyPreview,
		&rec.Status, &rec.Simulated, &rec.MessageID, &rec.Detail, &rec.DurationMS, &meta, &rec.Error,
	); err != nil {
		return notify.Record{}, err
	}
	rec.Channel = notify.Channel(channel)
	if recipients == nil {
		recipients = []string{}
	}
	rec.To = recipients
	if len(meta) > 0 {
		fields := map[string]string{}
		if err := json.Unmarshal(meta, &fields); err != nil {
			return notify.Record{}, fmt.Errorf("解析 meta 失败: %w", err)
		}
		rec.Meta = fields
	}
	return rec, nil
}
