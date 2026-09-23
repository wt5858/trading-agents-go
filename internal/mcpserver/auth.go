// Package mcpserver 是 MCP 协议的传输层，与 internal/server 平级。
//
// 它只做三件事：把 Bearer 令牌换成身份、把身份挂进 context、把注册好的
// 工具集装配成一个 http.Handler。**它不认识任何业务上下文**——
// 工具本身住在各上下文自己的 application/mcp_tools/ 里，
// 和那些上下文的 http_handlers 并列。
//
// 这条边界是刻意的。本包曾经直接装着 analysis、watchlist、screening
// 三个上下文的工具定义，后果是三个上下文的聚合同时流进一个不属于任何
// 上下文的包——而 internal/server 从来没有这个问题，它只有路由和中间件。
//
// ===========================================================================
// 它与 REST 层是两个平级的入口，不是一层套一层
// ===========================================================================
//
// 两边调的是同一批领域服务方法，走的是同一套权限判定，区别只在于
// 身份是怎么从传输层取出来的：REST 那边从 *gin.Context，这边从 context.Context。
// 因此本包不复用 internal/server 的任何东西，也不经过 gin 的路由与中间件——
// 复用那一侧意味着把 MCP 的可用性绑在 gin 的中间件顺序上。
package mcpserver

import (
	"context"
	"net/http"
	"strings"

	identity_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/identity/domain_services"
)

// claimsKey 是 Claims 在 context 里的键。
//
// 用未导出的空结构体做键而不是字符串：context 的键空间是全局共享的，
// 字符串键会和任何一个碰巧用了同名字符串的库悄悄互相覆盖，
// 而这类冲突不会报错，只会让鉴权莫名其妙地取到别人的值。
type claimsKey struct{}

// withClaims 把登录身份挂进 context。
func withClaims(ctx context.Context, claims *identity_services.Claims) context.Context {
	return context.WithValue(ctx, claimsKey{}, claims)
}

// ClaimsOf 取出调用者身份，未登录时返回 nil。
func ClaimsOf(ctx context.Context) *identity_services.Claims {
	claims, _ := ctx.Value(claimsKey{}).(*identity_services.Claims)
	return claims
}

// ===========================================================================
// 为什么 context 里只存 Claims，不存 Operator
// ===========================================================================
//
// 因为存不下：每个上下文都用自己的 Operator 值对象表达「谁在调用」，
// 字段一样但类型不同（见 di/providers/http_provider.go 顶部那段说明）。
// 往 context 里塞一个「通用 Operator」等于在传输层重新发明一个跨上下文
// 共享的身份类型，而那正是各上下文各自定义 Operator 所要避免的事。
//
// 于是这里与 REST 层保持同一个结构：传输层只负责认人（存 Claims），
// 转成哪个上下文的 Operator 由各上下文声明 OperatorResolver、
// 由组装根注入实现。本包因此一个业务上下文都不认识。

// ===========================================================================
// 鉴权中间件
// ===========================================================================

// authMiddleware 包住 MCP 处理器，把 Bearer 令牌换成 context 里的 Claims。
//
// 没有令牌或令牌无效一律 401 并就地结束，绝不放行到工具层：
// MCP 的工具全部是按用户隔离的（自选股、分析任务、筛选模板都带 user_id 过滤），
// 一个没有身份的请求走到工具里，拿到的 Operator 的 UserID 是 0——
// 而领域服务对 UserID=0 的处理是 requireLogin 报错，也就是说「能跑到那里」
// 本身没有安全问题，但错误会以 MCP 工具错误的形式返回，客户端会当成
// 「这个工具坏了」而不是「你没登录」。在传输层就拒掉，语义清楚得多。
func authMiddleware(next http.Handler, auth *identity_services.AuthService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerTokenOf(r)
		if token == "" {
			writeUnauthorized(w, "缺少 Authorization: Bearer <token>")
			return
		}
		claims, err := auth.Authenticate(r.Context(), token)
		if err != nil {
			// 不回显 err 的具体内容：它能区分「签名不对」「已过期」「会话已注销」，
			// 而这个区别对攻击者比对正常用户更有用。
			writeUnauthorized(w, "令牌无效或已过期")
			return
		}
		next.ServeHTTP(w, r.WithContext(withClaims(r.Context(), claims)))
	})
}

// bearerTokenOf 从请求头里取出 Bearer 令牌，取不到返回空串。
func bearerTokenOf(r *http.Request) string {
	raw := strings.TrimSpace(r.Header.Get("Authorization"))
	if raw == "" {
		return ""
	}
	// 大小写不敏感地匹配 scheme：RFC 7235 规定 scheme 不区分大小写，
	// 而各家 MCP 客户端写成 "bearer" 的不在少数。
	const prefix = "bearer "
	if len(raw) <= len(prefix) || !strings.EqualFold(raw[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(raw[len(prefix):])
}

func writeUnauthorized(w http.ResponseWriter, detail string) {
	// WWW-Authenticate 是 401 的规范要求，也是 MCP 客户端据以发起
	// 重新认证流程的信号。
	w.Header().Set("WWW-Authenticate", `Bearer realm="trading-agents"`)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"unauthorized","detail":"` + detail + `"}`))
}
