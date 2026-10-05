package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"notify-service/internal/notify"
	"notify-service/internal/store"
)

// errRecords 是 httpapi.Records 的故障注入假实现：所有查询都返回同一个错误，
// 用来验证存储故障时 HTTP 层的降级行为（列表/详情 500 storage_error、健康检查 503）。
type errRecords struct {
	err error
}

func (e errRecords) Get(context.Context, string) (notify.Record, bool, error) {
	return notify.Record{}, false, e.err
}

func (e errRecords) List(context.Context, store.Filter) ([]notify.Record, error) {
	return nil, e.err
}

func (e errRecords) Count(context.Context) (int, error) {
	return 0, e.err
}

// newFakeHandler 用给定的内存记录库构造 Handler；这些用例不经过发送链路。
func newFakeHandler(t *testing.T, records Records) http.Handler {
	t.Helper()
	return NewServer(Options{
		Service: testService(t, &fakeResolver{}),
		Store:   records,
		Logger:  discardLogger(),
	}).Handler()
}

// seedRecords 造 4 条覆盖各筛选维度的记录，时间以 base 递增（后者更新）。
func seedRecords(base time.Time) []notify.Record {
	return []notify.Record{
		{ID: "ntf_1", Time: base.Add(1 * time.Minute), Channel: notify.ChannelEmail, Type: "alert", UserID: "u1", Status: notify.StatusSent, Subject: "第一封", To: []string{"u1@example.com"}},
		{ID: "ntf_2", Time: base.Add(2 * time.Minute), Channel: notify.ChannelSMS, Type: "alert", UserID: "u2", Status: notify.StatusFailed, Subject: "第二封", To: []string{"13800000000"}},
		{ID: "ntf_3", Time: base.Add(3 * time.Minute), Channel: notify.ChannelEmail, Type: "system", UserID: "u1", Status: notify.StatusSimulated, Subject: "第三封", To: []string{"u1@example.com"}},
		{ID: "ntf_4", Time: base.Add(4 * time.Minute), Channel: notify.ChannelEmail, Type: "alert", UserID: "u2", Status: notify.StatusSent, Subject: "第四封", To: []string{"u2@example.com"}},
	}
}

// recordIDs 从响应里按顺序取出 records 的 id，顺带证明 records 是数组而不是 null。
func recordIDs(t *testing.T, payload map[string]any) []string {
	t.Helper()
	raw, ok := payload["records"].([]any)
	if !ok {
		t.Fatalf("响应缺少 records 数组: %v", payload)
	}
	ids := make([]string, 0, len(raw))
	for _, item := range raw {
		rec, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("records 元素不是对象: %v", item)
		}
		id, _ := rec["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

// TestListNotificationsFilters 覆盖列表接口的过滤与排序：无过滤按时间倒序，
// channel/type/userId/status 各自生效且可叠加，total 始终是全量计数。
func TestListNotificationsFilters(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	handler := newFakeHandler(t, &memRecords{records: seedRecords(base)})

	cases := []struct {
		name    string
		query   string
		wantIDs []string
	}{
		{name: "无过滤按时间倒序", query: "", wantIDs: []string{"ntf_4", "ntf_3", "ntf_2", "ntf_1"}},
		{name: "channel 过滤", query: "?channel=email", wantIDs: []string{"ntf_4", "ntf_3", "ntf_1"}},
		{name: "type 过滤", query: "?type=alert", wantIDs: []string{"ntf_4", "ntf_2", "ntf_1"}},
		{name: "userId 过滤", query: "?userId=u1", wantIDs: []string{"ntf_3", "ntf_1"}},
		{name: "status 过滤", query: "?status=failed", wantIDs: []string{"ntf_2"}},
		{name: "多条件同时生效", query: "?channel=email&type=alert&userId=u2&status=sent", wantIDs: []string{"ntf_4"}},
		{name: "无匹配返回空数组", query: "?channel=sms&status=sent", wantIDs: []string{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := call(t, handler, "GET", "/api/v1/notifications"+tc.query, "", nil)
			if got.status != 200 {
				t.Fatalf("状态码 = %d，期望 200，响应 %v", got.status, got.payload)
			}
			if ids := recordIDs(t, got.payload); !reflect.DeepEqual(ids, tc.wantIDs) {
				t.Fatalf("记录 = %v，期望 %v", ids, tc.wantIDs)
			}
			if total, ok := got.payload["total"].(float64); !ok || int(total) != 4 {
				t.Fatalf("total = %v，期望 4（总量不受过滤影响）", got.payload["total"])
			}
		})
	}
}

// TestListNotificationsLimit 覆盖 limit 缺省值（100）与显式 limit 的截断。
func TestListNotificationsLimit(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	seeded := make([]notify.Record, 0, 120)
	for i := 1; i <= 120; i++ {
		seeded = append(seeded, notify.Record{
			ID:      fmt.Sprintf("ntf_%03d", i),
			Time:    base.Add(time.Duration(i) * time.Second),
			Channel: notify.ChannelEmail,
			Status:  notify.StatusSent,
			To:      []string{"a@example.com"},
		})
	}
	handler := newFakeHandler(t, &memRecords{records: seeded})

	t.Run("缺省 limit 返回最新 100 条", func(t *testing.T) {
		got := call(t, handler, "GET", "/api/v1/notifications", "", nil)
		if got.status != 200 {
			t.Fatalf("状态码 = %d，期望 200", got.status)
		}
		ids := recordIDs(t, got.payload)
		if len(ids) != 100 {
			t.Fatalf("返回 %d 条，期望 100", len(ids))
		}
		if ids[0] != "ntf_120" || ids[99] != "ntf_021" {
			t.Fatalf("默认 limit 未取最新 100 条：首 %s 尾 %s", ids[0], ids[99])
		}
		if total, ok := got.payload["total"].(float64); !ok || int(total) != 120 {
			t.Fatalf("total = %v，期望 120", got.payload["total"])
		}
	})

	t.Run("显式 limit 生效", func(t *testing.T) {
		got := call(t, handler, "GET", "/api/v1/notifications?limit=5", "", nil)
		ids := recordIDs(t, got.payload)
		want := []string{"ntf_120", "ntf_119", "ntf_118", "ntf_117", "ntf_116"}
		if !reflect.DeepEqual(ids, want) {
			t.Fatalf("记录 = %v，期望 %v", ids, want)
		}
	})
}

// TestGetNotification 覆盖按 id 取详情：命中返回落库记录，未命中返回 not_found。
func TestGetNotification(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	handler := newFakeHandler(t, &memRecords{records: seedRecords(base)})

	t.Run("命中返回完整记录", func(t *testing.T) {
		got := call(t, handler, "GET", "/api/v1/notifications/ntf_3", "", nil)
		if got.status != 200 {
			t.Fatalf("状态码 = %d，期望 200，响应 %v", got.status, got.payload)
		}
		if got.payload["id"] != "ntf_3" || got.payload["subject"] != "第三封" {
			t.Fatalf("记录内容不对: %v", got.payload)
		}
		if got.payload["channel"] != "email" || got.payload["status"] != "simulated" {
			t.Fatalf("记录字段不对: %v", got.payload)
		}
	})

	t.Run("未命中返回 404", func(t *testing.T) {
		got := call(t, handler, "GET", "/api/v1/notifications/ntf_missing", "", nil)
		if got.status != 404 || got.kind(t) != "not_found" {
			t.Fatalf("状态码 = %d，kind = %q，期望 404 not_found", got.status, got.kind(t))
		}
	})
}

// TestHealthzRecords 覆盖健康检查公开返回记录总数。
func TestHealthzRecords(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	handler := newFakeHandler(t, &memRecords{records: seedRecords(base)})

	got := call(t, handler, "GET", "/healthz", "", nil)
	if got.status != 200 {
		t.Fatalf("状态码 = %d，期望 200，响应 %v", got.status, got.payload)
	}
	if got.payload["status"] != "ok" {
		t.Fatalf("status = %v，期望 ok", got.payload["status"])
	}
	if count, ok := got.payload["records"].(float64); !ok || int(count) != 4 {
		t.Fatalf("records = %v，期望 4", got.payload["records"])
	}
}

// TestStorageFailures 覆盖存储故障的对外契约：查询接口 500 storage_error，健康检查 503 degraded。
func TestStorageFailures(t *testing.T) {
	handler := newFakeHandler(t, errRecords{err: errors.New("数据库连接已断开")})

	t.Run("列表查询失败返回 500 storage_error", func(t *testing.T) {
		got := call(t, handler, "GET", "/api/v1/notifications", "", nil)
		if got.status != 500 || got.kind(t) != "storage_error" {
			t.Fatalf("状态码 = %d，kind = %q，期望 500 storage_error（响应 %v）", got.status, got.kind(t), got.payload)
		}
	})

	t.Run("按 id 查询失败返回 500 storage_error", func(t *testing.T) {
		got := call(t, handler, "GET", "/api/v1/notifications/ntf_1", "", nil)
		if got.status != 500 || got.kind(t) != "storage_error" {
			t.Fatalf("状态码 = %d，kind = %q，期望 500 storage_error（响应 %v）", got.status, got.kind(t), got.payload)
		}
	})

	t.Run("健康检查降级为 503", func(t *testing.T) {
		got := call(t, handler, "GET", "/healthz", "", nil)
		if got.status != 503 {
			t.Fatalf("状态码 = %d，期望 503，响应 %v", got.status, got.payload)
		}
		if got.payload["status"] != "degraded" {
			t.Fatalf("status = %v，期望 degraded", got.payload["status"])
		}
	})
}

// 入口层只保证"正文只有一个来源"：body（+ bodyFormat）。
// 已移除的 html 字段、改名的 markdown 字段都要给出明确 400，而不是被静默忽略。
func TestValidateBody(t *testing.T) {
	htmlField := "<p>x</p>"
	mdField := "**x**"

	cases := []struct {
		name    string
		req     notifyRequest
		wantErr bool
	}{
		{name: "body 纯文本", req: notifyRequest{Body: "x", BodyFormat: "text"}},
		{name: "body markdown", req: notifyRequest{Body: "## x", BodyFormat: "markdown"}},
		{name: "bodyFormat 留空", req: notifyRequest{Body: "x"}},
		{name: "两个都写：body+html", req: notifyRequest{Body: "x", HTML: &htmlField}, wantErr: true},
		{name: "两个都写：body+markdown", req: notifyRequest{Body: "x", Markdown: &mdField}, wantErr: true},
		{name: "只给 html", req: notifyRequest{HTML: &htmlField}, wantErr: true},
		{name: "非法 bodyFormat", req: notifyRequest{Body: "x", BodyFormat: "html"}, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.req.validateBody()
			if tc.wantErr && err == nil {
				t.Fatal("应报错")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if err != nil {
				var nerr *notify.Error
				if !errors.As(err, &nerr) || nerr.Kind != notify.KindInvalid {
					t.Fatalf("应返回 400 类错误，实际 %v", err)
				}
			}
		})
	}
}
