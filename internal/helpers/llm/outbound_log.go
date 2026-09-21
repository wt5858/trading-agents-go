package llm

import (
	"context"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/wt5858/trading-agents-go/internal/helpers/httpx"
	"github.com/wt5858/trading-agents-go/internal/helpers/logger"
)

// ---------------------------------------------------------------------------
// 出站调用日志
//
// 打点挂在 http.RoundTripper 上，而不是散在每个调用点，理由有三：
//   - RoundTrip 手里有 req.Context()，trace_id 自动跟着链路走，不必逐层传参；
//   - 以后新增一家 provider，只要它的 http.Client 过了 ensureClient 就自带日志，
//     不存在「新写的客户端忘了打点」这种迟早会发生的疏漏；
//   - doJSON 的重试是在这层之上做的，每次真实发包各出一条，
//     线上能直接看出某次补全到底重试了几轮、卡在哪一轮。
//
// 脱敏规则本身在 helpers/httpx，这里只负责调用。marketdata 包里有一份形态相近的
// RoundTripper，两边共用那套规则——原先各自抄了一份，直到错误信息也要用同一套规则时
// 才发现：日志脱敏改漏一边最多少打一条码，而规则一旦分叉，分叉出去的那份就是
// 将来泄密的那份。httpx 只做字符串处理、不认识任何 provider，所以谁都能 import 它，
// 不会造出「行情数据源依赖大模型客户端」这种方向。
//
// 安全约束（这层经手的每个请求都带着厂商密钥，写错一行就是一次泄密）：
//   - header 绝不落盘。密钥就在 Authorization / x-api-key / x-goog-api-key 里。
//   - 请求体与响应体绝不落盘。
//   - URL 只取 host + path，query 一律丢弃——把密钥放在 query string 里是
//     相当常见的写法（部分厂商、几乎所有自建网关），而这层无从判断
//     某个参数是不是密钥，只能整段不要。
//   - path 还要再过一遍 httpx.RedactPath：自建网关有 /v1/<key>/chat/completions
//     这种形态，baseURL 是配置给的，这层管不住它怎么写。
//
// 想在日志里认出「这是哪一次调用」，走 withOutboundTarget 显式传一个安全标签，
// 不要指望从 URL 里捞。
// ---------------------------------------------------------------------------

type outboundTargetKey struct{}

// withOutboundTarget 给出站请求附一个业务标签，以 target 字段出现在日志里。
//
// 需要它是因为 URL 本身经常认不出是哪次调用：三家的 path 都是固定的
// chat/completions / messages，真正有区分度的模型名在请求体里，而请求体不能打。
// 标签只允许填领域概念（模型名、接口名、股票代码），绝不能填请求参数原文。
func withOutboundTarget(ctx context.Context, target string) context.Context {
	if target == "" {
		return ctx
	}
	return context.WithValue(ctx, outboundTargetKey{}, target)
}

func outboundTargetFrom(ctx context.Context) string {
	s, _ := ctx.Value(outboundTargetKey{}).(string)
	return s
}

// loggingTransport 包在真实 Transport 外面，只观测不改写：
// 请求原样透传，响应与错误原样返回，重试/降级的判断完全不受影响。
type loggingTransport struct {
	base     http.RoundTripper
	provider string
}

func (t *loggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.base.RoundTrip(req)
	latency := time.Since(start)

	ctx := req.Context()
	// FromContext 拿到的日志器已经烤进了 trace_id（没有 trace_id 的 ctx —— 比如
	// 启动探活、定时同步 —— 就退回全局日志器，字段缺席，不报错），
	// 所以这里不要再手工补一次 trace_id。
	fields := []zap.Field{
		zap.String("provider", t.provider),
		zap.String("method", req.Method),
		zap.String("host", req.URL.Host),
		zap.String("path", httpx.RedactPath(req.URL.Path)),
		zap.Duration("latency", latency),
	}
	if target := outboundTargetFrom(ctx); target != "" {
		fields = append(fields, zap.String("target", target))
	}

	log := logger.FromContext(ctx)
	switch {
	case err != nil:
		// 传输层故障（DNS、连不上、超时）没有状态码。这里的 err 直接来自
		// Transport，还没被 http.Client 包成带完整 URL 的 *url.Error，
		// 所以打出来不会把 query 里可能存在的密钥带上。
		log.Error("外部调用失败", append(fields, zap.Error(err))...)
	case resp.StatusCode >= http.StatusInternalServerError:
		log.Error("外部调用返回服务端错误", append(fields, zap.Int("status", resp.StatusCode))...)
	case resp.StatusCode >= http.StatusBadRequest:
		// 4xx 基本都是限流(429)、密钥失效(401)或参数错，属于要人介入的事，
		// 必须在默认级别就能看见，不能藏在 debug 里。
		log.Warn("外部调用返回错误状态", append(fields, zap.Int("status", resp.StatusCode))...)
	default:
		log.Info("外部调用完成", append(fields, zap.Int("status", resp.StatusCode))...)
	}
	return resp, err
}
