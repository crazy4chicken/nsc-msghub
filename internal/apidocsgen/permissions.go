package apidocsgen

import "notify-service/internal/httpapi"

// DocPermission 返回路由对应的权限 key，供 OpenAPI 的 x-teamusers-permission 扩展使用。
// msghub 只有 any 粒度，team 级 key 恒为空；未命中任何权限类的路由（/healthz）返回空串。
func DocPermission(method, path string) (anyKey, teamKey string) {
	send, read := httpapi.PermissionClass(method, path)
	switch {
	case send:
		return httpapi.DefaultPermissionSend, ""
	case read:
		return httpapi.DefaultPermissionRead, ""
	default:
		return "", ""
	}
}
