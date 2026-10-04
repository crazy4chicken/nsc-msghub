// Command genspec 从 httpapi 的单一业务路由表生成 docs/public/openapi.yaml，
// 供 VitePress 文档站的 API 参考页使用。路径都相对进程工作目录（仓库根目录）。
package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/crazy4chicken/nsc-teamusers/apidocs/go"

	"notify-service/internal/apidocsgen"
	"notify-service/internal/httpapi"
)

func main() {
	if err := generate(); err != nil {
		fmt.Fprintln(os.Stderr, "生成 OpenAPI 失败:", err)
		os.Exit(1)
	}
}

func generate() error {
	// 处理器不会被调用：collect 只 walk 路由表，因此 Service/Store/Email 留空即可，
	// 日志也丢弃。
	server := httpapi.NewServer(httpapi.Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	operations, err := apidocs.Collect(apidocsgen.Router(server), apidocsgen.DocOperations, apidocsgen.DocPermission)
	if err != nil {
		return fmt.Errorf("收集 API 操作: %w", err)
	}
	if err := writeSpec(filepath.Join("docs", "public", "openapi.yaml"), operations); err != nil {
		return fmt.Errorf("写入 OpenAPI 文档: %w", err)
	}
	return nil
}

// writeSpec 先写同目录临时文件再原子替换，避免中断时留下半截规范。
func writeSpec(path string, operations []apidocs.Operation) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("创建输出目录: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".spec.tmp-*")
	if err != nil {
		return fmt.Errorf("创建临时文件: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)

	options := apidocs.EmitOptions{
		Title:   "nsc-msghub API",
		Version: "0.3.0",
		Servers: []apidocs.Server{{
			URL:         "http://127.0.0.1:8090",
			Description: "Local development default",
		}},
		SecurityScheme: apidocs.SecurityScheme{
			Name:         "bearerAuth",
			Type:         "http",
			Scheme:       "bearer",
			BearerFormat: "API token or teamusers JWT",
		},
	}
	if err := apidocs.Emit(operations, temporary, options); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("生成文档: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("同步临时文件: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("关闭临时文件: %w", err)
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("替换文档: %w", err)
	}
	return nil
}
