// Package server 组装 HTTP 服务：中间件、路由与优雅停机。
package server

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"

	identity_handlers "github.com/wt5858/trading-agents-go/internal/bounded_contexts/identity/application/http_handlers"
	identity_services "github.com/wt5858/trading-agents-go/internal/bounded_contexts/identity/domain_services"
	"github.com/wt5858/trading-agents-go/internal/helpers/custom_errors"
	"github.com/wt5858/trading-agents-go/internal/helpers/logger"
	"github.com/wt5858/trading-agents-go/internal/helpers/response"
)

const headerRequestID = "X-Request-Id"

// RequestID 为每个请求分配可追踪的 ID，并透传到响应头。
// 客户端上报问题时带上这个 ID，就能直接定位到那一条日志。
//
// 它同时把这个 ID 写进 **c.Request.Context()**，而不只是 gin 自己的上下文。
//
// 这一行是整条链路的起点，也是以前断掉的地方：c.Set 存的东西只有拿得到
// *gin.Context 的代码读得到，而领域服务、仓储、GORM 的回调收到的都是标准
// context.Context——于是 SQL 日志永远不知道自己属于哪次请求，
// 排查「这条慢查询是谁打的」只能靠时间戳去猜。
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(headerRequestID)
		if id == "" {
			id = uuid.NewString()
		}
		c.Set("request_id", id)
		c.Writer.Header().Set(headerRequestID, id)

		// 换掉 Request 上挂的 context。后续处理器一律用 c.Request.Context()，
		// 所以这里换一次，整条下游就都带上了。
		c.Request = c.Request.WithContext(logger.WithTraceID(c.Request.Context(), id))

		c.Next()
	}
}

// Logger 记录访问日志。
func Logger(log *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		query := c.Request.URL.RawQuery

		c.Next()

		fields := []zap.Field{
			zap.String("method", c.Request.Method),
			zap.String("path", path),
			zap.String("query", query),
			zap.Int("status", c.Writer.Status()),
			zap.Duration("latency", time.Since(start)),
			zap.String("ip", c.ClientIP()),
			zap.String(logger.TraceIDField, c.GetString("request_id")),
		}
		if len(c.Errors) > 0 {
			log.Error("请求处理失败", append(fields, zap.String("errors", c.Errors.String()))...)
			return
		}
		if c.Writer.Status() >= http.StatusInternalServerError {
			log.Error("请求返回服务端错误", fields...)
			return
		}
		log.Info("请求完成", fields...)
	}
}

// Recovery 拦截 panic，转换成统一响应体。
//
// 刻意不复用 gin.Recovery()：它写的是裸文本，会打破「所有响应都是
// {code,message,data}」这条强制约定，前端在 panic 时反而拿到无法解析的响应。
func Recovery(log *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				log.Error("请求处理发生 panic",
					zap.Any("panic", r),
					zap.String("path", c.Request.URL.Path),
					zap.String(logger.TraceIDField, c.GetString("request_id")),
					zap.Stack("stack"))
				// panic 细节绝不外泄：它常含内部路径与数据结构。
				response.FailWith(c, response.CodeInternal, http.StatusInternalServerError, "服务内部错误")
			}
		}()
		c.Next()
	}
}

// CORS 处理跨域。
func CORS(allowedOrigins []string) gin.HandlerFunc {
	allowAll := len(allowedOrigins) == 0
	allowed := make(map[string]bool, len(allowedOrigins))
	for _, o := range allowedOrigins {
		if o == "*" {
			allowAll = true
		}
		allowed[o] = true
	}

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		switch {
		case origin == "":
			// 非浏览器请求，无需 CORS 头。
		case allowAll:
			// 回显具体 Origin 而不是写 "*"：带 Cookie 的请求在 "*" 下会被浏览器拒绝。
			c.Writer.Header().Set("Access-Control-Allow-Origin", origin)
			c.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
		case allowed[origin]:
			c.Writer.Header().Set("Access-Control-Allow-Origin", origin)
			c.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
		}
		c.Writer.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,DELETE,OPTIONS")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Origin,Content-Type,Accept,Authorization,"+headerRequestID)
		c.Writer.Header().Set("Access-Control-Expose-Headers", headerRequestID)

		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// AuthRequired 校验访问令牌并把身份写入 gin 上下文。
//
// 它调用 AuthService 而不是自己解 JWT：会话是否已被吊销这件事只有服务层知道，
// 中间件独立验签会让「登出」和「停用账号」失效。
func AuthRequired(authService *identity_services.AuthService) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := identity_handlers.BearerToken(c)
		if token == "" {
			response.Fail(c, custom_errors.Unauthorized("缺少访问令牌"))
			return
		}
		claims, err := authService.Authenticate(c.Request.Context(), token)
		if err != nil {
			response.Fail(c, err)
			return
		}
		c.Set(identity_handlers.ContextKeyClaims, claims)
		c.Next()
	}
}

// AdminRequired 叠加在 AuthRequired 之后，用于纯管理端路由。
func AdminRequired() gin.HandlerFunc {
	return func(c *gin.Context) {
		claims := identity_handlers.ClaimsOf(c)
		if claims == nil {
			response.Fail(c, custom_errors.Unauthorized("未登录"))
			return
		}
		if !claims.Role.IsAdmin() {
			response.Fail(c, custom_errors.Forbidden("需要管理员权限"))
			return
		}
		c.Next()
	}
}
