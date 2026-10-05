package main

import "testing"

// TestNormalizeTeamusersURL 覆盖 teamusers 服务地址的规范化：允许省略 scheme（按 http:// 处理）、
// 去掉结尾斜杠，不合法输入给出明确错误，而不是等到请求构造时才报 URL 解析失败。
func TestNormalizeTeamusersURL(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "完整 https 地址", raw: "https://iam.example.com", want: "https://iam.example.com"},
		{name: "省略 scheme 的 host:port", raw: "127.0.0.1:20705", want: "http://127.0.0.1:20705"},
		{name: "省略 scheme 的主机名", raw: "localhost:8080", want: "http://localhost:8080"},
		{name: "省略 scheme 且带路径", raw: "iam.example.com/svc", want: "http://iam.example.com/svc"},
		{name: "去掉结尾斜杠", raw: "http://127.0.0.1:8080/", want: "http://127.0.0.1:8080"},
		{name: "去掉首尾空白", raw: "  http://127.0.0.1:8080  ", want: "http://127.0.0.1:8080"},
		{name: "不支持的 scheme", raw: "ftp://iam.example.com", wantErr: true},
		{name: "空值", raw: "   ", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeTeamusersURL(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("normalizeTeamusersURL(%q) 未返回错误，得到 %q", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeTeamusersURL(%q) 返回错误: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("normalizeTeamusersURL(%q) = %q，期望 %q", tc.raw, got, tc.want)
			}
		})
	}
}
