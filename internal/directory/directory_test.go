package directory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"notify-service/internal/notify"
)

func asNotify(err error, target **notify.Error) bool {
	return errors.As(err, target)
}

func TestHTTPResolverShapes(t *testing.T) {
	var gotPath, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		switch strings.TrimPrefix(r.URL.Path, "/users/") {
		case "u1": // 规范形状
			writeJSON(w, 200, map[string]any{
				"id": "u1", "name": "张三",
				"channels": map[string]string{"email": "a@qq.com", "sms": "13800000000"},
			})
		case "u2": // 兼容平铺形状
			writeJSON(w, 200, map[string]any{"email": "b@qq.com", "phone": "13900000000"})
		case "missing":
			w.WriteHeader(http.StatusNotFound)
		case "broken":
			_, _ = w.Write([]byte("not json"))
		case "wrong-id":
			writeJSON(w, 200, map[string]any{"id": "other", "channels": map[string]string{"email": "x@qq.com"}})
		case "boom":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("user service exploded"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	resolver, err := NewHTTPResolver(server.URL, "/users/{id}", "secret-token", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	user, err := resolver.Resolve(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if user.Name != "张三" || user.Channels["sms"] != "13800000000" {
		t.Errorf("解析结果不对: %+v", user)
	}
	if gotPath != "/users/u1" || gotAuth != "Bearer secret-token" {
		t.Errorf("请求不对：path=%q auth=%q", gotPath, gotAuth)
	}

	flat, err := resolver.Resolve(ctx, "u2")
	if err != nil {
		t.Fatal(err)
	}
	if flat.Channels["email"] != "b@qq.com" || flat.Channels["sms"] != "13900000000" {
		t.Errorf("平铺形状解析不对: %+v", flat)
	}

	expectKind := map[string]notify.Kind{
		"missing":  notify.KindNotFound,
		"broken":   notify.KindUpstream,
		"wrong-id": notify.KindUpstream,
		"boom":     notify.KindUpstream,
	}
	for id, kind := range expectKind {
		_, err := resolver.Resolve(ctx, id)
		var nerr *notify.Error
		if err == nil || !asNotify(err, &nerr) || nerr.Kind != kind {
			t.Errorf("%s 期望 %s，实际 %v", id, kind, err)
		}
	}
}

func TestHTTPResolverConfigValidation(t *testing.T) {
	if r, err := NewHTTPResolver("", "", "", 0); err != nil || r != nil {
		t.Errorf("URL 为空应返回 nil,nil，实际 %v %v", r, err)
	}
	if _, err := NewHTTPResolver("http://x", "/users", "", 0); err == nil {
		t.Error("路径缺少 {id} 占位符应报错")
	}
}

// fakeRow 是 pgx.Row 的假实现：把预置的 id/name/channels 按扫描目标类型填进去。
// err 非 nil 时直接返回，用来模拟 pgx.ErrNoRows 或数据库故障。
type fakeRow struct {
	id       string
	name     string
	channels []byte
	err      error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	values := []any{r.id, r.name, r.channels}
	if len(dest) != len(values) {
		return fmt.Errorf("fakeRow: 期望 %d 个扫描目标，实际 %d", len(values), len(dest))
	}
	for i, target := range dest {
		switch ptr := target.(type) {
		case *string:
			s, ok := values[i].(string)
			if !ok {
				return fmt.Errorf("fakeRow: 第 %d 列不是字符串", i)
			}
			*ptr = s
		case *[]byte:
			b, ok := values[i].([]byte)
			if !ok {
				return fmt.Errorf("fakeRow: 第 %d 列不是字节切片", i)
			}
			*ptr = b
		case *json.RawMessage:
			b, _ := values[i].([]byte)
			*ptr = append((*ptr)[:0], b...)
		case *map[string]string:
			raw, _ := values[i].([]byte)
			if len(raw) == 0 {
				*ptr = map[string]string{}
				continue
			}
			var channels map[string]string
			if err := json.Unmarshal(raw, &channels); err != nil {
				return err
			}
			*ptr = channels
		default:
			return fmt.Errorf("fakeRow: 不支持的扫描目标 %T", target)
		}
	}
	return nil
}

// fakeQuerier 是 Querier 的假实现：每次查询返回同一行，并记录收到的参数。
type fakeQuerier struct {
	row  pgx.Row
	sql  string
	args []any
}

func (q *fakeQuerier) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	q.sql = sql
	q.args = append([]any(nil), args...)
	return q.row
}

func TestPGResolverResolve(t *testing.T) {
	channels, err := json.Marshal(map[string]string{"email": "a@qq.com", "sms": "13800000000"})
	if err != nil {
		t.Fatal(err)
	}
	q := &fakeQuerier{row: fakeRow{id: "u1", name: "张三", channels: channels}}
	resolver := NewPGResolver(q)

	user, err := resolver.Resolve(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if user.ID != "u1" || user.Name != "张三" {
		t.Errorf("用户基本信息不对: %+v", user)
	}
	if addr, ok := user.Address(notify.ChannelEmail); !ok || addr != "a@qq.com" {
		t.Errorf("email 地址不对: %+v", user.Channels)
	}
	if addr, ok := user.Address(notify.ChannelSMS); !ok || addr != "13800000000" {
		t.Errorf("sms 地址不对: %+v", user.Channels)
	}
	if len(q.args) != 1 || q.args[0] != "u1" {
		t.Errorf("查询参数不对: %v", q.args)
	}
}

func TestPGResolverEmptyChannels(t *testing.T) {
	cases := []struct {
		name     string
		channels []byte
	}{
		{name: "NULL", channels: nil},
		{name: "空对象", channels: []byte("{}")},
		{name: "JSON null", channels: []byte("null")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolver := NewPGResolver(&fakeQuerier{row: fakeRow{id: "u1", channels: tc.channels}})
			user, err := resolver.Resolve(context.Background(), "u1")
			if err != nil {
				t.Fatal(err)
			}
			if user.Channels == nil || len(user.Channels) != 0 {
				t.Fatalf("channels 应为空且非 nil，实际 %#v", user.Channels)
			}
		})
	}
}

func TestPGResolverErrors(t *testing.T) {
	cases := []struct {
		name string
		row  fakeRow
		want notify.Kind
	}{
		{name: "用户不存在", row: fakeRow{err: pgx.ErrNoRows}, want: notify.KindNotFound},
		{name: "扫描失败", row: fakeRow{err: errors.New("连接中断")}, want: notify.KindUpstream},
		{name: "channels 不是合法 JSON", row: fakeRow{id: "u1", channels: []byte("not-json")}, want: notify.KindUpstream},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolver := NewPGResolver(&fakeQuerier{row: tc.row})
			_, err := resolver.Resolve(context.Background(), "u1")
			var nerr *notify.Error
			if err == nil || !asNotify(err, &nerr) || nerr.Kind != tc.want {
				t.Fatalf("期望 %s，实际 %v", tc.want, err)
			}
		})
	}
}

func TestNewPGResolverNilQuerier(t *testing.T) {
	if r := NewPGResolver(nil); r != nil {
		t.Fatalf("Querier 为 nil 应返回 nil，实际 %v", r)
	}
}

// TestNewResolverFactory 覆盖 New 的选择顺序：用户服务 HTTP 优先，其次 PostgreSQL 用户表。
func TestNewResolverFactory(t *testing.T) {
	q := &fakeQuerier{row: fakeRow{id: "u1"}}

	t.Run("未配置用户服务时用 PostgreSQL 用户表", func(t *testing.T) {
		resolver, err := New("", DefaultUserServicePath, "", 0, q)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := resolver.(*PGResolver); !ok {
			t.Fatalf("期望 *PGResolver，实际 %T", resolver)
		}
	})

	t.Run("配置用户服务时优先 HTTP", func(t *testing.T) {
		resolver, err := New("http://user-service", DefaultUserServicePath, "tok", time.Second, q)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := resolver.(*HTTPResolver); !ok {
			t.Fatalf("期望 *HTTPResolver，实际 %T", resolver)
		}
	})

	t.Run("都没有时返回 nil", func(t *testing.T) {
		resolver, err := New("", DefaultUserServicePath, "", 0, nil)
		if err != nil || resolver != nil {
			t.Fatalf("期望 nil,nil，实际 %v %v", resolver, err)
		}
	})

	t.Run("用户服务路径非法时报错", func(t *testing.T) {
		if _, err := New("http://user-service", "/users", "", 0, q); err == nil {
			t.Fatal("路径缺少 {id} 占位符应报错")
		}
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
