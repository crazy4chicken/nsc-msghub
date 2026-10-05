package store

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"notify-service/internal/notify"
)

// testStore 打开一个干净的测试库：迁移 + 清空三张表后返回 Store 和用于直查的 pool。
// 未设置 NOTIFY_TEST_DATABASE_URL 时跳过，保证默认 go test 不依赖 PostgreSQL。
func testStore(t *testing.T, recordLimit int) (*Store, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("NOTIFY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set NOTIFY_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("迁移测试库失败: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE notifications, outbox_messages, users RESTART IDENTITY`); err != nil {
		t.Fatalf("清空测试库失败: %v", err)
	}
	st, err := Open(ctx, dsn, recordLimit, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("打开 Store 失败: %v", err)
	}
	t.Cleanup(st.Close)
	return st, pool
}

func recordAt(id string, at time.Time) notify.Record {
	return notify.Record{
		ID:      id,
		Time:    at,
		Channel: notify.ChannelEmail,
		To:      []string{"a@example.com"},
		Status:  notify.StatusSent,
	}
}

func recordIDsOf(recs []notify.Record) []string {
	ids := make([]string, 0, len(recs))
	for _, rec := range recs {
		ids = append(ids, rec.ID)
	}
	return ids
}

// TestSaveListOrderingAndFilters 覆盖 Save→List 的时间倒序与各筛选维度。
func TestSaveListOrderingAndFilters(t *testing.T) {
	st, _ := testStore(t, 0)
	ctx := context.Background()
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	seeded := []notify.Record{
		{ID: "ntf_1", Time: base.Add(1 * time.Minute), Channel: notify.ChannelEmail, Type: "alert", UserID: "u1", Status: notify.StatusSent, To: []string{"u1@example.com"}},
		{ID: "ntf_2", Time: base.Add(2 * time.Minute), Channel: notify.ChannelSMS, Type: "alert", UserID: "u2", Status: notify.StatusFailed, To: []string{"13800000000"}},
		{ID: "ntf_3", Time: base.Add(3 * time.Minute), Channel: notify.ChannelEmail, Type: "system", UserID: "u1", Status: notify.StatusSimulated, To: []string{"u1@example.com"}},
		{ID: "ntf_4", Time: base.Add(4 * time.Minute), Channel: notify.ChannelEmail, Type: "alert", UserID: "u2", Status: notify.StatusSent, To: []string{"u2@example.com"}},
	}
	for _, rec := range seeded {
		if err := st.Save(ctx, rec); err != nil {
			t.Fatalf("写入 %s 失败: %v", rec.ID, err)
		}
	}

	cases := []struct {
		name   string
		filter Filter
		want   []string
	}{
		{name: "无过滤时间倒序", filter: Filter{}, want: []string{"ntf_4", "ntf_3", "ntf_2", "ntf_1"}},
		{name: "channel 过滤", filter: Filter{Channel: "email"}, want: []string{"ntf_4", "ntf_3", "ntf_1"}},
		{name: "type 过滤", filter: Filter{Type: "alert"}, want: []string{"ntf_4", "ntf_2", "ntf_1"}},
		{name: "userId 过滤", filter: Filter{UserID: "u1"}, want: []string{"ntf_3", "ntf_1"}},
		{name: "status 过滤", filter: Filter{Status: notify.StatusFailed}, want: []string{"ntf_2"}},
		{name: "组合过滤", filter: Filter{Channel: "email", Type: "alert", UserID: "u2", Status: notify.StatusSent}, want: []string{"ntf_4"}},
		{name: "无匹配返回空切片", filter: Filter{Channel: "sms", Status: notify.StatusSent}, want: []string{}},
		{name: "limit 取最新 N 条", filter: Filter{Limit: 2}, want: []string{"ntf_4", "ntf_3"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recs, err := st.List(ctx, tc.filter)
			if err != nil {
				t.Fatalf("List 失败: %v", err)
			}
			if recs == nil {
				t.Fatal("List 应返回非 nil 切片（JSON 里的 records 要是 []）")
			}
			if ids := recordIDsOf(recs); !reflect.DeepEqual(ids, tc.want) {
				t.Fatalf("记录 = %v，期望 %v", ids, tc.want)
			}
		})
	}
}

// TestListLimitDefaultAndClamp 覆盖 Limit<=0 的默认 100 与 Limit>5000 的上限截断。
func TestListLimitDefaultAndClamp(t *testing.T) {
	st, pool := testStore(t, 0)
	ctx := context.Background()

	// 直接批量造 5101 行（时间逐秒变旧），用来观察默认值与上限两个边界。
	if _, err := pool.Exec(ctx, `
		INSERT INTO notifications (id, "time", channel, status)
		SELECT 'ntf_' || i, now() - make_interval(secs => i), 'email', 'sent'
		FROM generate_series(1, 5101) AS i`); err != nil {
		t.Fatalf("批量造数失败: %v", err)
	}

	cases := []struct {
		name      string
		limit     int
		wantCount int
		wantLast  string
	}{
		{name: "Limit 为 0 按默认 100", limit: 0, wantCount: 100, wantLast: "ntf_100"},
		{name: "Limit 为负也按默认 100", limit: -5, wantCount: 100, wantLast: "ntf_100"},
		{name: "显式 limit 原样生效", limit: 200, wantCount: 200, wantLast: "ntf_200"},
		{name: "超过 5000 截到 5000", limit: 999999, wantCount: 5000, wantLast: "ntf_5000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recs, err := st.List(ctx, Filter{Limit: tc.limit})
			if err != nil {
				t.Fatalf("List 失败: %v", err)
			}
			if len(recs) != tc.wantCount {
				t.Fatalf("返回 %d 条，期望 %d", len(recs), tc.wantCount)
			}
			if recs[0].ID != "ntf_1" || recs[len(recs)-1].ID != tc.wantLast {
				t.Fatalf("首尾 = %s..%s，期望 ntf_1..%s", recs[0].ID, recs[len(recs)-1].ID, tc.wantLast)
			}
		})
	}
}

// TestGetHitAndMiss 覆盖按 id 命中与未命中。
func TestGetHitAndMiss(t *testing.T) {
	st, _ := testStore(t, 0)
	ctx := context.Background()
	rec := recordAt("ntf_get", time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC))

	if err := st.Save(ctx, rec); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	got, ok, err := st.Get(ctx, rec.ID)
	if err != nil || !ok {
		t.Fatalf("应命中，实际 ok=%v err=%v", ok, err)
	}
	if got.ID != rec.ID || !got.Time.Equal(rec.Time) {
		t.Fatalf("记录不对: %+v", got)
	}

	if _, ok, err := st.Get(ctx, "ntf_never_exists"); err != nil {
		t.Fatalf("未命中不应报错: %v", err)
	} else if ok {
		t.Fatal("不存在的记录不应命中")
	}
}

// TestCountAllRecords 覆盖 Count 返回全量行数。
func TestCountAllRecords(t *testing.T) {
	st, _ := testStore(t, 0)
	ctx := context.Background()

	if n, err := st.Count(ctx); err != nil || n != 0 {
		t.Fatalf("空表 Count = %d, err = %v，期望 0", n, err)
	}
	base := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	for i := 1; i <= 3; i++ {
		if err := st.Save(ctx, recordAt(fmt.Sprintf("ntf_%d", i), base.Add(time.Duration(i)*time.Minute))); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}
	if n, err := st.Count(ctx); err != nil || n != 3 {
		t.Fatalf("Count = %d, err = %v，期望 3", n, err)
	}
}

// TestRecordRoundTripFidelity 覆盖记录各字段经 PostgreSQL 往返后不失真。
func TestRecordRoundTripFidelity(t *testing.T) {
	st, _ := testStore(t, 0)
	ctx := context.Background()
	base := time.Date(2026, 3, 4, 5, 6, 7, 123456000, time.UTC)

	cases := []struct {
		name string
		rec  notify.Record
	}{
		{
			name: "空收件人与 nil meta 的失败记录",
			rec:  notify.Record{
				ID:          "ntf_round_empty",
				Time:        base,
				Channel:     notify.ChannelSMS,
				Type:        "告警-alert",
				BodyFormat:  string(notify.BodyFormatMarkdown),
				UserID:      "u1001",
				UserName:    "张三",
				Provider:    "console",
				To:          []string{},
				Subject:     "【测试】主题 with emoji 🎉",
				BodyPreview: "正文预览：换行 压缩 与 Unicode ✓",
				Status:      notify.StatusFailed,
				Simulated:   false,
				MessageID:   "",
				Detail:      "dev 模式",
				DurationMS:  17,
				Meta:        nil,
				Error:       "投递失败：SMTP 拒收",
			},
		},
		{
			name: "多收件人与非 nil meta 的模拟记录",
			rec:  notify.Record{
				ID:          "ntf_round_multi",
				Time:        base.Add(time.Minute),
				Channel:     notify.ChannelEmail,
				Type:        "system",
				BodyFormat:  string(notify.BodyFormatText),
				UserID:      "u1002",
				UserName:    "李四",
				Provider:    "simulated",
				To:          []string{"li@example.com", "team@example.com"},
				Subject:     "多收件人 ✓",
				BodyPreview: "预览",
				Status:      notify.StatusSimulated,
				Simulated:   true,
				MessageID:   "msg-42",
				Detail:      "",
				DurationMS:  3,
				Meta:        map[string]string{"k1": "v1", "中文键": "值"},
				Error:       "",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := st.Save(ctx, tc.rec); err != nil {
				t.Fatalf("写入失败: %v", err)
			}
			got, ok, err := st.Get(ctx, tc.rec.ID)
			if err != nil || !ok {
				t.Fatalf("应命中，实际 ok=%v err=%v", ok, err)
			}
			assertRecordEqual(t, tc.rec, got)
		})
	}
}

func assertRecordEqual(t *testing.T, want, got notify.Record) {
	t.Helper()
	if got.ID != want.ID {
		t.Errorf("id = %q，期望 %q", got.ID, want.ID)
	}
	if !got.Time.Equal(want.Time) {
		t.Errorf("time = %s，期望 %s", got.Time, want.Time)
	}
	if got.Channel != want.Channel || got.Type != want.Type || got.BodyFormat != want.BodyFormat {
		t.Errorf("channel/type/bodyFormat = %q/%q/%q，期望 %q/%q/%q",
			got.Channel, got.Type, got.BodyFormat, want.Channel, want.Type, want.BodyFormat)
	}
	if got.UserID != want.UserID || got.UserName != want.UserName || got.Provider != want.Provider {
		t.Errorf("userId/userName/provider = %q/%q/%q，期望 %q/%q/%q",
			got.UserID, got.UserName, got.Provider, want.UserID, want.UserName, want.Provider)
	}
	if !reflect.DeepEqual(got.To, want.To) {
		t.Errorf("to = %#v，期望 %#v", got.To, want.To)
	}
	if got.Subject != want.Subject || got.BodyPreview != want.BodyPreview {
		t.Errorf("subject/bodyPreview = %q/%q，期望 %q/%q", got.Subject, got.BodyPreview, want.Subject, want.BodyPreview)
	}
	if got.Status != want.Status || got.Simulated != want.Simulated {
		t.Errorf("status/simulated = %q/%v，期望 %q/%v", got.Status, got.Simulated, want.Status, want.Simulated)
	}
	if got.MessageID != want.MessageID || got.Detail != want.Detail || got.DurationMS != want.DurationMS {
		t.Errorf("messageId/detail/durationMs = %q/%q/%d，期望 %q/%q/%d",
			got.MessageID, got.Detail, got.DurationMS, want.MessageID, want.Detail, want.DurationMS)
	}
	if !reflect.DeepEqual(got.Meta, want.Meta) {
		t.Errorf("meta = %#v，期望 %#v", got.Meta, want.Meta)
	}
	if got.Error != want.Error {
		t.Errorf("error = %q，期望 %q", got.Error, want.Error)
	}
}

// TestRecordLimitPrunesOldest 覆盖 recordLimit>0 时每次写入后只保留最新 N 条。
func TestRecordLimitPrunesOldest(t *testing.T) {
	st, _ := testStore(t, 3)
	ctx := context.Background()
	base := time.Date(2026, 4, 5, 6, 7, 8, 0, time.UTC)

	for i := 1; i <= 5; i++ {
		if err := st.Save(ctx, recordAt(fmt.Sprintf("ntf_%d", i), base.Add(time.Duration(i)*time.Minute))); err != nil {
			t.Fatalf("写入 ntf_%d 失败: %v", i, err)
		}
	}

	if n, err := st.Count(ctx); err != nil || n != 3 {
		t.Fatalf("Count = %d, err = %v，期望 3", n, err)
	}
	recs, err := st.List(ctx, Filter{})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if ids := recordIDsOf(recs); !reflect.DeepEqual(ids, []string{"ntf_5", "ntf_4", "ntf_3"}) {
		t.Fatalf("保留的记录 = %v，期望最新的 ntf_5,ntf_4,ntf_3", ids)
	}
	if _, ok, _ := st.Get(ctx, "ntf_1"); ok {
		t.Fatal("最旧记录应被裁剪")
	}
	if _, ok, _ := st.Get(ctx, "ntf_2"); ok {
		t.Fatal("第二旧记录应被裁剪")
	}
	if _, ok, _ := st.Get(ctx, "ntf_3"); !ok {
		t.Fatal("最新 3 条应保留")
	}

	// 继续写入会滚动裁剪。
	if err := st.Save(ctx, recordAt("ntf_6", base.Add(6*time.Minute))); err != nil {
		t.Fatalf("写入 ntf_6 失败: %v", err)
	}
	recs, err = st.List(ctx, Filter{})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if ids := recordIDsOf(recs); !reflect.DeepEqual(ids, []string{"ntf_6", "ntf_5", "ntf_4"}) {
		t.Fatalf("保留的记录 = %v，期望 ntf_6,ntf_5,ntf_4", ids)
	}
}

// TestSaveOutboxRoundTrip 覆盖 outbox_messages 的写入内容可原样读回。
func TestSaveOutboxRoundTrip(t *testing.T) {
	st, pool := testStore(t, 0)
	ctx := context.Background()
	at := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	msg := notify.OutboxMessage{
		Time:       at,
		Channel:    notify.ChannelEmail,
		MessageID:  "msg-eml-1",
		Recipients: []string{"a@example.com", "b@example.com"},
		Subject:    "【测试】邮件主题 ✓",
		Body:       "正文（纯文本）\n第二行",
		Raw:        "Subject: test\r\nContent-Type: text/plain\r\n\r\n原始内容",
	}
	if err := st.SaveOutbox(ctx, msg); err != nil {
		t.Fatalf("写入 outbox 失败: %v", err)
	}

	var (
		gotTime    time.Time
		channel    string
		messageID  string
		recipients []string
		subject    string
		body       string
		raw        string
	)
	err := pool.QueryRow(ctx,
		`SELECT "time", channel, message_id, recipients, subject, body, raw FROM outbox_messages ORDER BY seq DESC LIMIT 1`).
		Scan(&gotTime, &channel, &messageID, &recipients, &subject, &body, &raw)
	if err != nil {
		t.Fatalf("读回 outbox 失败: %v", err)
	}
	if !gotTime.Equal(msg.Time) || channel != string(msg.Channel) || messageID != msg.MessageID {
		t.Fatalf("time/channel/messageId = %s/%q/%q，期望 %s/%q/%q",
			gotTime, channel, messageID, msg.Time, msg.Channel, msg.MessageID)
	}
	if !reflect.DeepEqual(recipients, msg.Recipients) {
		t.Fatalf("recipients = %#v，期望 %#v", recipients, msg.Recipients)
	}
	if subject != msg.Subject || body != msg.Body || raw != msg.Raw {
		t.Fatalf("subject/body/raw 不一致: %q / %q / %q", subject, body, raw)
	}
}
