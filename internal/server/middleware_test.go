package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/wt5858/trading-agents-go/internal/helpers/logger"
)

// TestRequestIDReachesRequestContext 钉住链路的第一环。
//
// RequestID 以前只调 c.Set，那个值只有拿得到 *gin.Context 的代码读得到；
// 而领域服务、仓储、GORM 回调收到的都是标准 context.Context。
// 这个测试断言的是「处理器从 c.Request.Context() 打出来的日志带着 trace_id，
// 且与响应头里的 X-Request-Id 是同一个值」——链路能不能串起来，就看这一条。
func TestRequestIDReachesRequestContext(t *testing.T) {
	gin.SetMode(gin.TestMode)

	core, logs := observer.New(zap.DebugLevel)
	obs := zap.New(core)

	engine := gin.New()
	// 先把可观测的 logger 当底座塞进去，再挂 RequestID——顺序不能反。
	// RequestID 内部的 WithTraceID 是在「当前上下文日志器」之上加字段的，
	// 底座没换就以全局为底，而测试里 Init 没跑，全局是 Nop，什么都观测不到。
	// 这同时也验证了真实的派生链路：底座 → 加 trace_id 字段 → 下游取用。
	engine.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(logger.NewContext(c.Request.Context(), obs))
		c.Next()
	})
	engine.Use(RequestID())
	engine.GET("/probe", func(c *gin.Context) {
		// 模拟下游：只拿标准 context，完全不碰 gin。
		logger.FromContext(c.Request.Context()).Info("下游日志")
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/probe", nil))

	header := w.Header().Get(headerRequestID)
	if header == "" {
		t.Fatal("响应头里没有 X-Request-Id")
	}

	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("期望下游打出 1 条日志，实际 %d 条", len(entries))
	}
	got := entries[0].ContextMap()[logger.TraceIDField]
	if got != header {
		t.Fatalf("下游日志 %s=%v，响应头 X-Request-Id=%s，两者应一致",
			logger.TraceIDField, got, header)
	}
}

// TestRequestIDHonoursInboundHeader 调用方带了 X-Request-Id 就该沿用它，
// 否则跨服务的链路会在本服务这里断成两截。
func TestRequestIDHonoursInboundHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)

	engine := gin.New()
	engine.Use(RequestID())
	var seen string
	engine.GET("/probe", func(c *gin.Context) {
		seen = logger.TraceIDFromContext(c.Request.Context())
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set(headerRequestID, "upstream-trace-42")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if seen != "upstream-trace-42" {
		t.Fatalf("期望沿用上游的 trace_id，实际拿到 %q", seen)
	}
	if h := w.Header().Get(headerRequestID); h != "upstream-trace-42" {
		t.Fatalf("响应头应回显上游 trace_id，实际 %q", h)
	}
}
