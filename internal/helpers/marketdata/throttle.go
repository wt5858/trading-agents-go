package marketdata

import (
	"context"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/time/rate"

	"github.com/wt5858/trading-agents-go/internal/helpers/httpx"
)

// ---------------------------------------------------------------------------
// 出站限速与重试
//
// 这一层专为东方财富存在，但写成通用的 RoundTripper，因为「免费公开接口靠封 IP
// 而不是配额错误码来限流」是这类数据源的共性，不是东财独有。
//
// # 为什么必须限速
//
// 有配额错误码的数据源（Tushare 返回 code!=0 加一句人话）其实很好伺候：超了就
// 退避重试，错误信息会告诉你超的是哪一项。东财不给这个待遇——超了它直接掐掉 TCP
// 连接，客户端看到的是一个空响应，没有状态码、没有响应体，和网络抖动长得一模一样。
// 实测（境外出口 IP，约 0.67 次/秒持续打）八次里有六次是空响应，而静置 45 秒后
// 头两次必成功。也就是说速率本身就是正确性的一部分，不是性能调优。
//
// # 为什么挂在 RoundTripper 上而不是在每个 Fetch 方法里 Wait
//
//   - 翻页循环在 provider 内部，调用点不止一个，逐个 Wait 迟早漏掉一个；
//   - 境外访问 82.push2 会 302 到 push2delay，http.Client 会自动重发一次，
//     那一次同样要占令牌，而只有 Transport 看得见它；
//   - 以后加新接口，只要过这个 Client 就自带限速，不依赖作者记得。
//
// sync_service.go 的 SyncConfig.FanOutLimit 注释早就写了「真正的节流必须做在
// 数据源实现里」——这里就是它说的那个位置。有了令牌桶之后 FanOutLimit 退化成
// 纯粹的内存/连接数上限，正好和它自己的注释一致，不需要改。
// ---------------------------------------------------------------------------

// throttleTransport 给单个数据源加令牌桶与重试。
type throttleTransport struct {
	base    http.RoundTripper
	limiter *rate.Limiter
	// maxAttempts 含首次尝试。1 表示不重试。
	maxAttempts int
	// backoff 是第一次重试前的基准等待，之后按 2 的幂次递增。
	backoff time.Duration
	// sleep 抽出来只为测试能跑快，生产恒为 sleepCtx。
	//
	// 它必须收 ctx 并在取消时立刻返回：退避一等就是秒级（基准 800ms，
	// 三次重试叠加抖动最长 ~3.2s），期间 ctx 被取消却察觉不到的话，
	// 有界扇出里每个在退避中的 worker 都会把停机拖慢这么久——
	// 而上面 limiter.Wait 那句注释承诺的正是「取消能及时传播」。
	sleep func(context.Context, time.Duration) error
	// jitter 返回 [0,1) 的随机数，同样只为测试可控。
	jitter func() float64
}

func (t *throttleTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var lastErr error
	for attempt := 1; attempt <= t.maxAttempts; attempt++ {
		// Wait 在 ctx 取消/超时时立刻返回，所以阻塞在这里的 worker 不会拖住
		// concurrency.Settle 的取消传播。
		if err := t.limiter.Wait(req.Context()); err != nil {
			return nil, err
		}
		resp, err := t.base.RoundTrip(req)

		// 服务端错误也要重试，而且必须在这一层重试。
		//
		// 上面那套只处理「连接被掐」，但同一个源同样会回 502/503——东财的
		// 82.push2delay 就是 nginx 顶着一组后端，后端抖一下前面照样是 502。
		// 这种失败是**转瞬即逝**的，而它的代价却和永久失败一样重：批量行情只有
		// 东财一个源（tushare 的批量端点要 2000 积分，如实声明了没有这个能力），
		// 一次 502 就是整个 quotes 同步彻底失败。
		//
		// 只重试幂等方法。POST 在这几个源上都不是查询，重发有可能产生第二次副作用，
		// 而「慢一点」远好过「悄悄做了两次」。
		if err == nil {
			if attempt == t.maxAttempts || !isRetriableStatus(resp.StatusCode) || !isIdempotent(req.Method) {
				return resp, nil
			}
			// 重试前必须把响应体读干并关掉，否则这条连接不会回到连接池，
			// 重试几次就攒出几条泄漏的连接——而它的表现是「跑久了越来越慢」。
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainLimit))
			_ = resp.Body.Close()
			lastErr = &statusError{code: resp.StatusCode}
			if !rewindBody(req) {
				break
			}
		} else {
			if !isRetriableOutbound(err) {
				return resp, err
			}
			lastErr = err
			if attempt == t.maxAttempts || !rewindBody(req) {
				break
			}
		}
		// 退避之外还要抖动：同步是有界并发跑的，几个 worker 会在同一时刻撞上
		// 同一次封禁，不抖动的话它们会整整齐齐地一起醒来再撞一次。
		wait := t.backoff << (attempt - 1)
		wait += time.Duration(t.jitter() * float64(wait))
		if err := t.sleep(req.Context(), wait); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// rewindBody 把请求体倒回开头，返回这个请求能不能安全重发。
//
// 不能只判断 req.Body != nil 就放弃：body 是只能读一遍的流，第一次尝试已经把它
// 读干了，直接重发会发出一个空体请求——那比失败更糟，因为它会成功。
// 但标准库给 bytes.Reader / strings.Reader 这类可重放的 body 自动生成了 GetBody，
// 有它就能重建。两者都没有才真的不能重试。
//
// 东财这边全是 GET（Body 为 nil），这个函数是给将来复用这个 Transport 的人留的。
func rewindBody(req *http.Request) bool {
	if req.Body == nil || req.Body == http.NoBody {
		return true
	}
	if req.GetBody == nil {
		return false
	}
	body, err := req.GetBody()
	if err != nil {
		return false
	}
	req.Body = body
	return true
}

// sleepCtx 等满 d，但 ctx 一取消就立刻返回它的错误。
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// isRetriableOutbound 判断这个传输层错误值不值得重试。
//
// 判据本身搬去了 httpx——LLM 那边也要用同一套，两份迟早分叉。东财限流的表现
// （连接直接掐掉，Go 侧看到 EOF / ECONNRESET）正是那边收的那一类，所以这里
// 不需要任何补充，留这层薄壳只为保住本包内的命名。
func isRetriableOutbound(err error) bool {
	return httpx.IsRetriableOutbound(err)
}

// drainLimit 是重试前最多读多少响应体。
//
// 读它只为把连接还给连接池，不为拿内容，因此要封顶：对端在 502 页面里塞几 MB
// 填充（东财那个 502 页面就带着一段专门用来撑爆 IE/Chrome 友好错误页的 padding）
// 时，不封顶等于每次重试都白读一遍。读不完的部分由 Close 丢弃，代价只是这条连接
// 不被复用——那正是我们本来就能接受的结果。
const drainLimit = 64 << 10

// isRetriableStatus 判断哪些状态码值得重试。
//
// 只收「对端自己也承认这是它的问题」的那几个：500/502/503/504 是服务端故障，
// 429 是限流。4xx 的其余部分一律不重试——参数错了、没权限、找不到，
// 重试一百次还是同样的结果，只会把一次快速失败拖成几十秒。
//
// 408 不在列：它是对端抱怨我们发得太慢，重发同一个请求通常还是慢。
func isRetriableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// isIdempotent 判断这个方法重发是否安全。
func isIdempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

// statusError 让「重试用尽后仍是 5xx」有一个能返回的错误。
//
// 只在所有尝试都被吃掉、连一个响应都交不出去时才会被返回；正常路径上最后一次
// 尝试的响应会原样交给调用方，好让 provider 那边保持原有的报错措辞。
type statusError struct{ code int }

func (e *statusError) Error() string {
	return "上游返回 HTTP " + strconv.Itoa(e.code)
}

// 重试参数。不做成配置项：它们描述的是「东财掐掉连接之后多久才愿意再理我们」，
// 属于对端行为，不是本系统的策略旋钮。真正该调的是 rps。
const (
	throttleMaxAttempts = 3
	throttleBackoff     = 800 * time.Millisecond
)

// ensureThrottledHTTPClient 在 ensureHTTPClient 之上再包一层令牌桶与重试。
//
// 包裹顺序是「限速在外、日志在内」：这样日志里的 latency 只含真实往返，
// 不含排队等待。反过来包会让每条日志的 latency 都等于「1/rps + RTT」，
// 那个指标就再也没法用来判断东财是不是变慢了。
//
// rps <= 0 表示不限速，直接退回原客户端——测试里用得上，生产不该这么配。
func ensureThrottledHTTPClient(hc *http.Client, provider string, rps float64, burst int) *http.Client {
	c := ensureHTTPClient(hc, provider)
	if rps <= 0 {
		return c
	}
	if burst < 1 {
		burst = 1
	}
	c.Transport = &throttleTransport{
		base:        c.Transport,
		limiter:     rate.NewLimiter(rate.Limit(rps), burst),
		maxAttempts: throttleMaxAttempts,
		backoff:     throttleBackoff,
		sleep:       sleepCtx,
		//nolint:gosec // 抖动只是打散重试时刻，不需要密码学随机
		jitter: rand.Float64,
	}
	return c
}
