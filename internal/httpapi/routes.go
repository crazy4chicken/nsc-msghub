package httpapi

import "net/http"

// RegisterRoutes 注册全部业务路由。这里是服务的唯一路由表：运行期的
// Handler 和文档站的 OpenAPI 生成器（cmd/genspec）都从这里重放路由，
// 新增接口只需要改这一处。
//
// 未命中的 /api/ JSON 404 与控制台等兜底路由不属于业务接口，由 Handler 单独注册。
func (s *Server) RegisterRoutes(register func(method, pattern string, handler http.HandlerFunc)) {
	register(http.MethodGet, "/healthz", s.handleHealth)
	register(http.MethodGet, "/api/v1/channels", s.handleChannels)
	register(http.MethodPost, "/api/v1/notify", s.handleNotify)
	register(http.MethodGet, "/api/v1/notifications", s.handleListNotifications)
	register(http.MethodGet, "/api/v1/notifications/{id}", s.handleGetNotification)
	register(http.MethodPost, "/api/v1/channels/email/verify", s.handleVerifyEmail)
}
