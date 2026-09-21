package marketdata

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
//   - 以后新增一个数据源，只要它的 http.Client 过了 ensureHTTPClient 就自带日志，
//     不存在「新写的 provider 忘了打点」这种迟早会发生的疏漏；
//   - Composite 会把多个源挨个试过去，每个源各出一条，
//     线上能直接看出降级链走到了第几环、卡在谁身上。
//
// 脱敏规则本身在 helpers/httpx，这里只负责调用。llm 包里有一份形态相近的
// RoundTripper，两边共用那套规则——原先各自抄了一份，直到错误信息也要用同一套规则时
// 才发现：日志脱敏改漏一边最多少打一条码，而规则一旦分叉，分叉出去的那份就是
// 将来泄密的那份。httpx 只做字符串处理、不认识任何 provider，所以谁都能 import 它。
//
// 安全约束（这层经手的每个请求都带着数据源 token，写错一行就是一次泄密）：
//   - header 绝不落盘。Finnhub 的 token 就在 X-Finnhub-Token 里。
//   - 请求体与响应体绝不落盘。Tushare 的 token 是塞在 JSON body 里的，
//     打一次 body 就等于把 token 明文写进日志。
//   - URL 只取 host + path，query 一律丢弃。Finnhub 的 token 也可以走
//     ?token= 传（本实现用的是头，但这层不该依赖调用方永远这么写），
//     而这层无从判断某个参数是不是密钥，只能整段不要。
//   - path 还要再过一遍 httpx.RedactPath，防住 /v1/<token>/... 这类路径形态。
//
// 想在日志里认出「这是哪一次调用」，走 withOutboundTarget 显式传一个安全标签，
// 不要指望从 URL 里捞。
// ---------------------------------------------------------------------------

type outboundTargetKey struct{}

// withOutboundTarget 给出站请求附一个业务标签，以 target 字段出现在日志里。
//
// Tushare 最需要它：所有接口都是 POST 同一个根路径，唯一有区分度的 api_name
// 在请求体里，而请求体里还躺着 token，绝不能打。
// 标签只允许填领域概念（接口名、股票代码），绝不能填请求参数原文。
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
// 请求原样透传，响应与错误原样返回，Composite 的降级判断完全不受影响。
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
	// 定时的全量同步任务 —— 就退回全局日志器，字段缺席，不报错），
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
		// 所以打出来不会把 query 里可能存在的 token 带上。
		log.Error("外部调用失败", append(fields, zap.Error(err))...)
	case resp.StatusCode >= http.StatusInternalServerError:
		log.Error("外部调用返回服务端错误", append(fields, zap.Int("status", resp.StatusCode))...)
	case resp.StatusCode >= http.StatusBadRequest:
		// 429 限流、403 付费档限权都落在这里，是要人介入的事，
		// 必须在默认级别就能看见，不能藏在 debug 里。
		log.Warn("外部调用返回错误状态", append(fields, zap.Int("status", resp.StatusCode))...)
	default:
		log.Info("外部调用完成", append(fields, zap.Int("status", resp.StatusCode))...)
	}
	return resp, err
}

// defaultHTTPTimeout 与两个数据源原先各自写死的值一致，仅做收敛。
const defaultHTTPTimeout = 30 * time.Second

// ensureHTTPClient 保证客户端一定带超时，并挂上出站日志 Transport。
//
// 必须复制一份而不是就地改：Tushare 与 Finnhub 在装配处拿的是**同一个**
// 注入的 *http.Client，就地设 Transport 会让两者互相盖掉 provider 名，
// 还会顺手污染调用方。复制只拷指针字段，底层连接池、代理设置一并保留。
func ensureHTTPClient(hc *http.Client, provider string) *http.Client {
	c := &http.Client{Timeout: defaultHTTPTimeout}
	if hc != nil {
		copied := *hc
		c = &copied
		if c.Timeout == 0 {
			c.Timeout = defaultHTTPTimeout
		}
	}
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c.Transport = &loggingTransport{base: base, provider: provider}
	return c
}
