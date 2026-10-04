package apidocsgen

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"notify-service/internal/httpapi"
)

// Router 把 httpapi.Server 的单一业务路由表重放到一个仅用于文档生成的 chi 路由上。
// 运行期服务不引入 chi；这里只为 cmd/genspec 的 OpenAPI 收集提供可 walk 的路由树。
func Router(s *httpapi.Server) chi.Router {
	router := chi.NewRouter()
	s.RegisterRoutes(func(method, pattern string, handler http.HandlerFunc) {
		router.MethodFunc(method, pattern, handler)
	})
	return router
}
